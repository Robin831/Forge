package warden

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/worktree"
)

// ephemeralWorktreeCleanupTimeout bounds the git commands that tear the
// throwaway checkout down. They run on a background context so a caller
// whose own context was cancelled still gets its worktree removed.
const ephemeralWorktreeCleanupTimeout = 60 * time.Second

// ephemeralGitTimeout bounds the git commands that create the checkout.
const ephemeralGitTimeout = 5 * time.Minute

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
// The checkout is detached at HEAD and lives under os.MkdirTemp rather than
// inside the anvil, so it collides with neither the worker worktrees under
// <anvil>/.workers/ nor Kiln's preview checkouts under <anvil>/.previews/,
// and the daemon's orphan-worktree sweep never sees it.
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
// costs the diagnosis. When fn succeeded, a cleanup failure IS the run's
// error, since nothing else would report it.
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

	head, err := ephemeralHead(ctx, anvilPath)
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "forge-consolidate-*")
	if err != nil {
		return fmt.Errorf("creating temp dir for ephemeral worktree: %w", err)
	}
	// The basename is what git registers the worktree under; git resolves a
	// collision itself, so two concurrent runs against one anvil are safe.
	wtPath := filepath.Join(tmp, "worktree")

	if _, err := runEphemeralGit(ctx, anvilPath, "worktree", "add", "--detach", wtPath, head); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("creating ephemeral worktree for %s at %s: %w", anvilPath, head, err)
	}

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), ephemeralWorktreeCleanupTimeout)
		defer cancel()
		if cleanupErr := removeEphemeralWorktree(cleanupCtx, anvilPath, wtPath, tmp); cleanupErr != nil {
			log.Printf("[warden] cleaning up ephemeral worktree %s: %v", wtPath, cleanupErr)
			if err == nil {
				err = cleanupErr
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
		if copyErr := copyIntoWorktree(anvilPath, wtPath, name); copyErr != nil {
			log.Printf("[warden] ephemeral worktree: copying %s: %v", name, copyErr)
		}
	}

	err = fn(wtPath)
	return err
}

// ephemeralHead resolves the commit the throwaway checkout is created at.
// An anvil with no commits cannot produce one, and the error says so rather
// than surfacing as a bare `git worktree add` failure.
func ephemeralHead(ctx context.Context, anvilPath string) (string, error) {
	out, err := runEphemeralGit(ctx, anvilPath, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolving HEAD of %s: %w", anvilPath, err)
	}
	head := strings.TrimSpace(out)
	if head == "" {
		return "", fmt.Errorf("resolving HEAD of %s: empty output", anvilPath)
	}
	return head, nil
}

// removeEphemeralWorktree unregisters the checkout and deletes its temp dir.
// Every step is attempted even when an earlier one failed: `worktree remove`
// declining (a git that never registered it, a directory already gone) must
// not leave the administrative entry behind for `prune` to find, nor the
// temp dir on disk.
func removeEphemeralWorktree(ctx context.Context, anvilPath, wtPath, tmpDir string) error {
	var errs []error
	if _, err := runEphemeralGit(ctx, anvilPath, "worktree", "remove", "--force", wtPath); err != nil {
		errs = append(errs, fmt.Errorf("git worktree remove %s: %w", wtPath, err))
	}
	if _, err := runEphemeralGit(ctx, anvilPath, "worktree", "prune"); err != nil {
		errs = append(errs, fmt.Errorf("git worktree prune: %w", err))
	}
	if err := os.RemoveAll(tmpDir); err != nil {
		errs = append(errs, fmt.Errorf("removing %s: %w", tmpDir, err))
	}
	return errors.Join(errs...)
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

// runEphemeralGit runs one git command against the anvil. The environment is
// stripped of git's repo-location overrides so an ambient GIT_DIR cannot
// answer for another repository, and LC_ALL is pinned so the diagnostics
// that reach the log read the same on every host.
func runEphemeralGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, ephemeralGitTimeout)
	defer cancel()

	full := append([]string{"-C", dir}, args...)
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", full...))
	cmd.Env = append(executil.CleanGitEnv(), "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, msg)
	}
	return stdout.String(), nil
}
