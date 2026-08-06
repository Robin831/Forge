package worktree

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Robin831/Forge/internal/executil"
)

// PreviewsDir is the directory name under each anvil that holds Kiln preview
// checkouts. It is deliberately separate from Manager.WorkersDir (".workers"):
// a preview must never collide with — or be mistaken for — a live worker
// worktree, and daemon-level cleanup treats the two directories differently.
const PreviewsDir = ".previews"

// PreviewPath returns the canonical preview worktree path for a bead in the
// given anvil: <anvilPath>/.previews/<sanitized-beadID>. CreateDetached and
// RemoveDetached both derive their target from this function so callers can
// predict where a preview lives (and clean one up) without creating it.
//
// It returns an error when beadID cannot be reduced to a safe single path
// segment — see sanitizePreviewID.
func PreviewPath(anvilPath, beadID string) (string, error) {
	id, err := sanitizePreviewID(beadID)
	if err != nil {
		return "", err
	}
	return filepath.Join(anvilPath, PreviewsDir, id), nil
}

// sanitizePreviewID reduces a bead ID to a single filesystem path segment that
// is safe to append to <anvil>/.previews/. Every character outside
// [A-Za-z0-9._-] (path separators included) folds to '-'.
//
// This is stricter than SanitizePath, which only folds '/', '\\', ' ' and ':'
// and would happily pass ".." through. A preview directory name is joined onto
// the anvil path, so a traversal segment would let a bead ID escape the anvil;
// "", "." and ".." are therefore rejected outright rather than silently
// rewritten.
func sanitizePreviewID(beadID string) (string, error) {
	if strings.TrimSpace(beadID) == "" {
		return "", fmt.Errorf("invalid bead ID %q: must not be empty", beadID)
	}
	var b strings.Builder
	b.Grow(len(beadID))
	for _, r := range beadID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	id := b.String()
	if id == "." || id == ".." {
		return "", fmt.Errorf("invalid bead ID %q: resolves to a path traversal segment", beadID)
	}
	return id, nil
}

// CreateDetached materializes a preview checkout of branchName at
// <anvilPath>/.previews/<beadID> with a DETACHED HEAD, and returns it.
//
// The detached checkout is the whole point of this helper: `git worktree add`
// refuses to check a branch out twice, so a plain branch checkout would fail
// for exactly the case previews exist to serve — a bead whose worker worktree
// under .workers/ is still alive on the same branch. Detaching at the branch
// tip sidesteps that check entirely, and it also means nothing here can move,
// reset, or delete the branch a worker is committing to.
//
// The branch is fetched from origin first (best-effort: a branch that was never
// pushed is a normal case, not an error) and the preview is created at the tip
// of origin/<branch> when that exists, falling back to the local ref. The
// preview always builds from the branch tip; uncommitted worker state is never
// visible to it.
//
// Re-creating over an existing preview directory is idempotent: the previous
// preview is torn down and replaced with a fresh checkout at the current tip.
// Callers must stop any processes running out of the old directory first.
//
// Like the worker worktrees, the preview gets node_modules linked from the
// anvil's main checkout so `npm run dev` starts without a fresh install.
// IMPORTANT: that link points AT the main checkout's node_modules — a preview
// manifest must not run `npm ci`/`npm install` inside a junctioned directory,
// or it rewrites the main checkout's dependencies out from under every other
// worker. This is the same guard Temper carries for worker worktrees.
func CreateDetached(ctx context.Context, anvilPath, beadID, branchName string) (*Worktree, error) {
	previewPath, err := PreviewPath(anvilPath, beadID)
	if err != nil {
		return nil, err
	}

	// Validate branchName before it reaches git. Branch names derive from bead
	// IDs / labels, so a name starting with "-" could be parsed as a flag.
	if err := validateBranchName(ctx, anvilPath, branchName); err != nil {
		return nil, err
	}

	// Ensure the .previews parent directory exists.
	if err := os.MkdirAll(filepath.Join(anvilPath, PreviewsDir), 0o755); err != nil {
		return nil, fmt.Errorf("creating previews directory: %w", err)
	}

	// Self-heal a stale core.worktree on the main repo before running git
	// commands that would trip over it (see CleanStaleCoreWorktree).
	if err := CleanStaleCoreWorktree(ctx, anvilPath); err != nil {
		slog.Warn("worktree: failed to clean stale core.worktree", "anvil", anvilPath, "error", err)
	}

	// Safety: refuse to operate if the main repo is on a feature branch (a
	// prior stray checkout in the parent would otherwise corrupt the anvil).
	if err := assertOnMainBranch(ctx, anvilPath); err != nil {
		return nil, fmt.Errorf("anvil branch safety check: %w", err)
	}

	// Refresh the remote-tracking ref so the preview reflects the latest push.
	// Best-effort: a local-only branch (never pushed) makes this targeted fetch
	// fail, which is expected — we fall back to the local ref below.
	if err := gitCmd(ctx, anvilPath, "fetch", "origin", "--", branchName); err != nil {
		slog.Debug("worktree: fetch of preview branch failed (may be local-only)",
			"anvil", anvilPath, "branch", branchName, "error", err)
	}

	tip, err := resolveBranchTip(ctx, anvilPath, branchName)
	if err != nil {
		return nil, fmt.Errorf("resolving tip of branch %q for preview of bead %s: %w", branchName, beadID, err)
	}

	// Idempotency: a preview directory left over from a previous run (valid or
	// half-removed) is torn down so the fresh checkout lands at the current
	// tip. RemoveDetached also prunes the registration, which is what would
	// otherwise make `git worktree add` fail with "already registered".
	if _, statErr := os.Stat(previewPath); statErr == nil {
		if err := RemoveDetached(ctx, anvilPath, beadID); err != nil {
			return nil, fmt.Errorf("removing existing preview worktree %s: %w", previewPath, err)
		}
	} else {
		// Directory absent but a registration may survive from a killed
		// process; prune so the add below cannot fail on a phantom entry.
		_ = gitCmd(ctx, anvilPath, "worktree", "prune")
	}

	// tip is a resolved commit SHA, so it cannot be mistaken for an option.
	if err := gitCmd(ctx, anvilPath, "worktree", "add", "--detach", previewPath, tip); err != nil {
		return nil, fmt.Errorf("git worktree add --detach (preview of %q at %s): %w", branchName, tip, err)
	}

	// Post-creation safety: verify the new worktree has a valid .git pointer so
	// git operations inside it resolve to the preview, not the anvil.
	if err := verifyWorktreeGitFile(previewPath); err != nil {
		_ = removeWithRetry(ctx, previewPath)
		return nil, fmt.Errorf("post-creation preview worktree verification failed: %w", err)
	}

	// Link node_modules from the main checkout (see the npm ci warning above).
	if err := linkNodeModules(anvilPath, previewPath); err != nil {
		// Non-fatal: a preview service that needs dependencies will fail its
		// health check with a clearer message than an aborted create.
		slog.Warn("worktree: failed to link node_modules into preview",
			"anvil", anvilPath, "preview", previewPath, "error", err)
	}

	return &Worktree{
		BeadID:    beadID,
		AnvilPath: anvilPath,
		Path:      previewPath,
		Branch:    branchName,
	}, nil
}

// RemoveDetached tears down the preview worktree for a bead. It is tolerant of
// a preview that was never created, was already removed, or whose directory
// survived without a git registration — in every one of those cases the
// post-condition (no preview directory, no stale registration) already holds or
// is reached, and the function returns nil.
//
// Callers must have stopped the preview's processes first; on Windows a running
// service holds file locks that make removal fail.
func RemoveDetached(ctx context.Context, anvilPath, beadID string) error {
	previewPath, err := PreviewPath(anvilPath, beadID)
	if err != nil {
		return err
	}

	// Unlink junctions/symlinks (node_modules) before any removal so neither
	// git nor os.RemoveAll walks into the main checkout and destroys content
	// there. On Windows, removal traverses junctions as regular directories.
	_ = unlinkReparsePoints(previewPath)

	// Best-effort: `git worktree remove` fails when the path is not a
	// registered worktree (already gone, or never registered). That is a
	// tolerated outcome — the RemoveAll below is the real teardown.
	if err := gitCmd(ctx, anvilPath, "worktree", "remove", "--force", previewPath); err != nil {
		slog.Debug("worktree: git worktree remove failed for preview (may already be gone)",
			"anvil", anvilPath, "preview", previewPath, "error", err)
	}

	// os.RemoveAll is a no-op for a path that does not exist, so this stays
	// tolerant of an already-deleted preview.
	if err := removeWithRetry(ctx, previewPath); err != nil {
		return fmt.Errorf("removing preview directory %s: %w", previewPath, err)
	}

	// Drop any registration left pointing at the now-absent directory.
	_ = gitCmd(ctx, anvilPath, "worktree", "prune")

	return nil
}

// resolveBranchTip returns the commit SHA at the tip of branchName, preferring
// the remote-tracking ref over the local one. Origin wins because a preview
// shows what the PR contains: when a worker's local ref is ahead of (or was
// rewritten relative to) what was pushed, the pushed tip is the honest subject
// of review. The local ref is the fallback for a branch that only ever existed
// locally.
func resolveBranchTip(ctx context.Context, anvilPath, branchName string) (string, error) {
	refs := []string{
		"refs/remotes/origin/" + branchName + "^{commit}",
		"refs/heads/" + branchName + "^{commit}",
	}
	var lastErr error
	for _, ref := range refs {
		sha, err := revParse(ctx, anvilPath, ref)
		if err == nil && sha != "" {
			return sha, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("branch not found locally or on origin: %w", lastErr)
}

// HeadSHA returns the commit a checkout currently has at HEAD. A preview
// checkout is detached at a branch tip, so its HEAD is exactly the commit the
// running services were built from — which is how a caller matches a live
// preview to a PR head.
func HeadSHA(ctx context.Context, dir string) (string, error) {
	return revParse(ctx, dir, "HEAD^{commit}")
}

// revParse resolves a single revision to its SHA in the given repository.
func revParse(ctx context.Context, dir, rev string) (string, error) {
	cmdCtx, cancel := contextWithGitTimeout(ctx)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", "rev-parse", "--verify", "--quiet", rev))
	cmd.Dir = dir
	cmd.Env = localGitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git rev-parse %s: %w: %s", rev, err, msg)
		}
		return "", fmt.Errorf("git rev-parse %s: %w", rev, err)
	}
	return strings.TrimSpace(string(out)), nil
}
