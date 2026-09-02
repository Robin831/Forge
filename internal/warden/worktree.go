package warden

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/Robin831/Forge/internal/worktree"
)

// WorktreeCleanupError reports that the throwaway checkout of a
// WithEphemeralWorktree call could not be torn down. It names the checkout
// that outlived the call, which is the one thing an operator needs to clean
// it up by hand.
//
// It is the SHAPE of WithEphemeralWorktree's second return value and never
// the discriminator between its two error channels: the type cannot answer
// that question, because nothing prevents fn's own error from carrying one —
// a nested WithEphemeralWorktree call returns exactly this type, and an
// errors.As over the run error would then read "the inner pass leaked a
// directory" as "this pass never ran". Which error is which is answered by
// which return value it arrives on.
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
// The checkout itself is worktree.CreateEphemeral's, and so is its teardown
// context (worktree.EphemeralCleanupContext); see that package for what the
// two ephemeral and preview checkouts share and why.
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
// The two errors are returned SEPARATELY because the call answers two
// structurally different questions whose answers callers act on in opposite
// ways, and one error value cannot carry both without the caller having to
// guess which it holds. runErr is fn's own error, or — when fn never ran at
// all — the reason the checkout could not be created. cleanupErr is the
// teardown's, non-nil whether or not fn succeeded, and it never masks
// anything: a leaked directory costs one temp dir, while swallowing the
// reason a consolidation failed costs the diagnosis, so ConsolidateAnvil
// reports it only when no cluster error claims the field first. Answered by
// error type rather than by position, the distinction would be wrong exactly
// where it is composed: fn is free to return a *WorktreeCleanupError of its
// own from a nested call.
func WithEphemeralWorktree(ctx context.Context, anvilPath string, fn func(worktreePath string) error) (runErr error, cleanupErr error) {
	if fn == nil {
		return errors.New("WithEphemeralWorktree: fn is nil"), nil
	}

	// A directory the pre-flight already accepts needs no wrapping — and in
	// one of the two cases it accepts, cannot be wrapped at all: a path
	// outside any git repository has no repository to add a worktree to
	// (which is the shape every non-repo test fixture and every
	// os.MkdirTemp session directory has). The condition is asked of the
	// guard itself rather than re-derived here, so the two can never come
	// to disagree about which directories a session may run in.
	if err := worktree.ValidateWorktreeDir(anvilPath); err == nil {
		return fn(anvilPath), nil
	}

	wt, err := worktree.CreateEphemeral(ctx, anvilPath)
	if err != nil {
		return err, nil
	}

	defer func() {
		// Background context, not the caller's: see
		// worktree.EphemeralCleanupTimeout for why a cancelled caller must
		// still get its worktree removed.
		ctx, cancel := worktree.EphemeralCleanupContext()
		defer cancel()
		if err := wt.Remove(ctx); err != nil {
			log.Printf("[warden] cleaning up ephemeral worktree %s: %v", wt.Path, err)
			cleanupErr = &WorktreeCleanupError{WorktreePath: wt.Path, Err: err}
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

	return fn(wt.Path), nil
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
