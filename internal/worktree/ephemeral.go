package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// EphemeralWorktree is a throwaway detached checkout of an anvil, created
// under os.MkdirTemp rather than inside the anvil itself so it collides with
// neither the worker worktrees under <anvil>/.workers/ nor Kiln's preview
// checkouts under <anvil>/.previews/, and the daemon's orphan-worktree sweep
// never sees it.
//
// It exists beside CreateDetached/RemoveDetached rather than as a second
// private implementation of them: a preview is addressed by bead ID and lives
// at a predictable path for its whole life, while this one is anonymous and
// dies with the call that made it. What the two share — the stale
// core.worktree self-heal, the post-creation .git pointer check, the
// timeout tiers and the Windows-aware teardown — is exactly what must not
// come to have two definitions, so both are built on the same helpers.
type EphemeralWorktree struct {
	// AnvilPath is the main checkout the worktree was added to.
	AnvilPath string
	// Path is the checkout itself, the directory a caller runs in.
	Path string
	// Head is the commit the checkout is detached at.
	Head string

	// tmpDir is the os.MkdirTemp parent that holds Path. It is private
	// because Remove is the only thing that may act on it: a caller
	// deleting it directly would leave the git registration behind.
	tmpDir string
}

// CreateEphemeral materializes a detached checkout of anvilPath at its
// current HEAD in a fresh temp directory. The caller must call Remove on the
// result — Create returns nothing to clean up when it fails.
func CreateEphemeral(ctx context.Context, anvilPath string) (*EphemeralWorktree, error) {
	head, err := HeadSHA(ctx, anvilPath)
	if err != nil {
		return nil, fmt.Errorf("resolving HEAD of %s: %w", anvilPath, err)
	}
	if head == "" {
		return nil, fmt.Errorf("resolving HEAD of %s: no commit", anvilPath)
	}

	// Self-heal a stale core.worktree on the main repo before running git
	// commands that would trip over it (see CleanStaleCoreWorktree). Best
	// effort, exactly as CreateDetached treats it: an anvil carrying the
	// condition would otherwise fail every ephemeral checkout until somebody
	// healed it by hand.
	if err := CleanStaleCoreWorktree(ctx, anvilPath); err != nil {
		slog.Warn("worktree: failed to clean stale core.worktree",
			"anvil", anvilPath, "error", err)
	}

	tmpDir, err := os.MkdirTemp("", "forge-ephemeral-worktree-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir for ephemeral worktree: %w", err)
	}
	// The basename is what git registers the worktree under; git resolves a
	// collision itself, so two concurrent runs against one anvil are safe.
	wtPath := filepath.Join(tmpDir, "worktree")

	var out bytes.Buffer
	// head is a resolved commit SHA, so it cannot be mistaken for an option.
	if err := gitCmdOut(ctx, anvilPath, &out, "worktree", "add", "--detach", wtPath, head); err != nil {
		_ = removeWithRetry(ctx, tmpDir)
		return nil, fmt.Errorf("git worktree add --detach (ephemeral checkout of %s at %s): %w%s",
			anvilPath, head, err, gitOutputSuffix(out.String()))
	}

	wt := &EphemeralWorktree{AnvilPath: anvilPath, Path: wtPath, Head: head, tmpDir: tmpDir}

	// Post-creation safety: verify the new worktree has a valid .git pointer
	// so git operations inside it resolve here and not to the anvil. This is
	// the one property the whole ephemeral checkout exists to establish —
	// Smith's pre-flight (ValidateWorktreeDir) refuses a directory that lacks
	// it — so a checkout without it is a failed creation, not a usable one.
	if err := verifyWorktreeGitFile(wtPath); err != nil {
		if rmErr := wt.Remove(ctx); rmErr != nil {
			slog.Warn("worktree: cleaning up unverifiable ephemeral worktree",
				"anvil", anvilPath, "worktree", wtPath, "error", rmErr)
		}
		return nil, fmt.Errorf("post-creation ephemeral worktree verification failed: %w", err)
	}

	return wt, nil
}

// Remove unregisters the checkout and deletes its temp directory. Every step
// is attempted even when an earlier one failed, and the errors are joined:
// a `git worktree remove` that declines (a directory already gone, a git that
// never registered it) must leave neither the administrative entry nor the
// temp dir behind.
//
// The order is remove, then delete, then prune, and the last two are not
// interchangeable: `git worktree prune` keeps an entry whose checkout
// directory still exists, so pruning before the delete would keep exactly the
// entry a failed `worktree remove` left behind and the anvil would accumulate
// one prunable registration per failure.
func (w *EphemeralWorktree) Remove(ctx context.Context) error {
	if w == nil {
		return nil
	}
	var errs []error

	var out bytes.Buffer
	if err := gitCmdOut(ctx, w.AnvilPath, &out, "worktree", "remove", "--force", w.Path); err != nil {
		errs = append(errs, fmt.Errorf("git worktree remove %s: %w%s", w.Path, err, gitOutputSuffix(out.String())))
	}

	// removeWithRetry rather than a bare os.RemoveAll: this directory is one
	// a just-exited session was running in, and on Windows its files stay
	// locked for a moment afterwards. It is also what Remove/RemoveDetached
	// use, so every worktree teardown in the tree waits the same way.
	if err := removeWithRetry(ctx, w.tmpDir); err != nil {
		errs = append(errs, fmt.Errorf("removing %s: %w", w.tmpDir, err))
	}

	out.Reset()
	if err := gitCmdOut(ctx, w.AnvilPath, &out, "worktree", "prune"); err != nil {
		errs = append(errs, fmt.Errorf("git worktree prune: %w%s", err, gitOutputSuffix(out.String())))
	}

	return errors.Join(errs...)
}

// gitOutputSuffix renders a git command's captured output for an error
// message, or nothing at all when the command said nothing.
func gitOutputSuffix(out string) string {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return ""
	}
	return ": " + trimmed
}
