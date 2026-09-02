package warden

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/Robin831/Forge/internal/worktree"
)

// ephemeralWorktreeCleanupTimeout bounds the teardown of the throwaway
// checkout. It runs on a background context so a caller whose own context was
// cancelled still gets its worktree removed.
const ephemeralWorktreeCleanupTimeout = 60 * time.Second

// WorktreeCleanupError reports that fn ran to completion and only the
// teardown of its throwaway checkout failed.
//
// It exists because WithEphemeralWorktree's single error return answers two
// structurally different questions, and its callers act on them in opposite
// ways: a checkout that could not be created means fn never ran, while this
// one means fn ran, its results are in hand and all that outlived it is a
// directory. Reported as one kind, a temp-dir teardown reads to an operator
// as the reason a pass produced nothing — the "a run that did work must never
// be reported as one that did not" rule the Assay partial/failed/skipped split
// exists for.
type WorktreeCleanupError struct {
	// WorktreePath is the checkout that could not be torn down.
	WorktreePath string
	// Err is what the teardown reported.
	Err error
}

func (e *WorktreeCleanupError) Error() string {
	return fmt.Sprintf("cleaning up ephemeral worktree %s: %v", e.WorktreePath, e.Err)
}

func (e *WorktreeCleanupError) Unwrap() error { return e.Err }

// WithEphemeralWorktree materializes a throwaway detached checkout of
// anvilPath at its current HEAD, calls fn with that checkout's path, and
// removes it again on every exit path.
//
// It exists because of Smith's worktree pre-flight
// (worktree.ValidateWorktreeDir, enforced by smith.SpawnWithOptions): a
// directory that sits inside a git repository but carries no linked-worktree
// .git file pointer is refused outright, so that a stray `cd ..` or a git
// write from the model's own tool calls can never land on the main
// checkout's branch. An anvil path IS a main checkout by definition, so
// every AI session handed one — which is what the consolidation pass did for
// every cluster it tried to merge — failed before the provider was even
// spawned. The guard is right; what was missing was a valid worktree to hand
// it.
//
// The checkout itself is worktree.CreateEphemeral's: that package owns every
// git-worktree lifecycle in the tree, so the stale core.worktree self-heal,
// the post-creation .git pointer check, the timeout tiers and the
// Windows-aware teardown are the ones the preview and worker checkouts get
// rather than a second set that drifts from them.
//
// A path the pre-flight would accept as it stands is passed through to fn
// unchanged; see the check at the top for why that is the same question and
// not a weakening of it.
//
// One consequence worth knowing: an AI session started under fn writes its
// per-session log to <worktree>/.forge-logs, so those logs go with the
// checkout. That matches what the scheduled smelter flush has always done
// (its passes run in the batch-branch worktree), and the per-cluster errors
// themselves are returned to the caller rather than left in those files.
//
// Cleanup errors are logged and never mask fn's error: a leaked directory
// costs one temp dir, while swallowing the reason a consolidation failed
// costs the diagnosis. When fn succeeded, a cleanup failure IS the returned
// error, since nothing else would report it — but it is returned as a
// *WorktreeCleanupError, so a caller can tell "fn never ran" from "fn ran and
// its checkout outlived it" instead of reporting the second as the first.
func WithEphemeralWorktree(ctx context.Context, anvilPath string, fn func(worktreePath string) error) (err error) {
	if fn == nil {
		return errors.New("WithEphemeralWorktree: fn is nil")
	}

	// A directory the pre-flight already accepts needs no wrapping — and in
	// one of the two cases it accepts, cannot be wrapped at all: a path
	// outside any git repository has no repository to add a worktree to
	// (which is the shape every non-repo test fixture and every
	// os.MkdirTemp session directory has). The condition is asked of the
	// guard itself rather than re-derived here, so the two can never come
	// to disagree about which directories a session may run in.
	if err := worktree.ValidateWorktreeDir(anvilPath); err == nil {
		return fn(anvilPath)
	}

	wt, err := worktree.CreateEphemeral(ctx, anvilPath)
	if err != nil {
		return err
	}

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), ephemeralWorktreeCleanupTimeout)
		defer cancel()
		if cleanupErr := wt.Remove(cleanupCtx); cleanupErr != nil {
			log.Printf("[warden] cleaning up ephemeral worktree %s: %v", wt.Path, cleanupErr)
			if err == nil {
				err = &WorktreeCleanupError{WorktreePath: wt.Path, Err: cleanupErr}
			}
		}
	}()

	// A detached checkout carries the COMMITTED rules file, but the rules
	// being consolidated are read from the anvil's working tree, where the
	// smelter's own batch branch may not have merged yet. Copy the live
	// files across so a session that opens them for context reads the same
	// state the caller is operating on. Best effort: they are context for
	// the model, not the input the merge is computed from — the merged rule
	// comes back as JSON from the session and is applied by the caller to
	// the anvil's real rules file, so nothing is ever read back off disk
	// here.
	for _, name := range []string{RulesFileName, ArchiveFileName} {
		if copyErr := copyIntoWorktree(anvilPath, wt.Path, name); copyErr != nil {
			log.Printf("[warden] ephemeral worktree: copying %s: %v", name, copyErr)
		}
	}

	err = fn(wt.Path)
	return err
}

// copyIntoWorktree copies one anvil-relative file into the worktree,
// creating its parent directory. A file the anvil does not have is not an
// error — an anvil with no archive file yet is the ordinary case.
func copyIntoWorktree(anvilPath, wtPath, relName string) error {
	src := filepath.Join(anvilPath, filepath.FromSlash(relName))
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	dst := filepath.Join(wtPath, filepath.FromSlash(relName))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
