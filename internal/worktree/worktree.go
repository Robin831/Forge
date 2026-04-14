// Package worktree manages git worktree creation and teardown for Smith workers.
//
// Each Smith operates in an isolated git worktree under .workers/<bead-id>/
// in the anvil's repository directory. The worktree is branched from origin/main
// with a forge-prefixed branch name.
package worktree

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// Worktree represents an active git worktree for a Smith worker.
type Worktree struct {
	// BeadID is the bead being worked on.
	BeadID string
	// AnvilPath is the root of the source repository.
	AnvilPath string
	// Path is the absolute path to the worktree directory.
	Path string
	// Branch is the git branch name.
	Branch string
	// BaseBranch is the branch this worktree was branched from.
	// Empty means the default (main/master).
	BaseBranch string
}

// CreateOptions controls worktree creation behaviour.
type CreateOptions struct {
	// Branch overrides the target branch name. Default: forge/<beadID>.
	Branch string
	// BaseBranch overrides the base ref to branch from. Default: origin/main
	// or origin/master (auto-detected). When set, the worktree branches from
	// origin/<BaseBranch> instead.
	BaseBranch string
	// ResetBranch, when true, resets an existing worktree branch back to the
	// base ref (origin/main or origin/<BaseBranch>) instead of reusing the
	// branch as-is. This discards all previous commits on the branch,
	// preventing cascading junk from failed pipeline runs.
	ResetBranch bool
	// Quiet suppresses git command stdout/stderr output (redirects to
	// io.Discard). Use this when creating worktrees from a TUI to avoid
	// corrupting the terminal's alt-screen with git progress output.
	Quiet bool
	// LocalHead, when true, skips the assertOnMainBranch safety check and
	// git fetch, and bases the worktree from the current local HEAD rather
	// than origin/main. Use for read-only scan worktrees where "scan the
	// working tree as-is" semantics are required and remote state should not
	// be fetched.
	LocalHead bool
}

// Manager handles creating and tearing down worktrees.
type Manager struct {
	// WorkersDir is the directory name under each anvil for worktrees.
	// Default: ".workers"
	WorkersDir string
}

// NewManager creates a Manager with default settings.
func NewManager() *Manager {
	return &Manager{WorkersDir: ".workers"}
}

// Create creates a new worktree for the given bead in the given anvil directory.
// If branch is provided, it checks out that existing branch.
// Otherwise, it creates a new branch named forge/<bead-id> from origin/main or
// origin/master (whichever exists, resolved by resolveBaseRef).
func (m *Manager) Create(ctx context.Context, anvilPath, beadID string, branch ...string) (*Worktree, error) {
	opts := CreateOptions{}
	if len(branch) > 0 {
		opts.Branch = branch[0]
	}
	return m.CreateWithOptions(ctx, anvilPath, beadID, opts)
}

// CreateWithOptions creates a new worktree with full control over branch and
// base ref. When opts.BaseBranch is set, the worktree branches from
// origin/<BaseBranch> instead of origin/main.
func (m *Manager) CreateWithOptions(ctx context.Context, anvilPath, beadID string, opts CreateOptions) (*Worktree, error) {
	workersDir := filepath.Join(anvilPath, m.WorkersDir)
	worktreePath := filepath.Join(workersDir, sanitizePath(beadID))

	targetBranch := opts.Branch
	if targetBranch == "" {
		targetBranch = "forge/" + sanitizePath(beadID)
	}

	// git is a local helper that suppresses stdout/stderr when opts.Quiet is set.
	// This prevents git progress output from corrupting a Bubbletea TUI alt-screen.
	out := io.Writer(os.Stderr)
	if opts.Quiet {
		out = io.Discard
	}
	git := func(dir string, args ...string) error {
		return gitCmdOut(ctx, dir, out, args...)
	}

	// Ensure .workers directory exists
	if err := os.MkdirAll(workersDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating workers directory: %w", err)
	}

	// If worktree directory already exists, check whether it is a valid git
	// worktree. If so, reset it to a clean state and reuse it. If not (e.g.
	// leftover directory from a failed run), remove it so we can create fresh.
	if _, err := os.Stat(worktreePath); err == nil {
		if isValidWorktree(ctx, worktreePath) {
			if opts.ResetBranch {
				// Hard-reset the branch back to the base ref, discarding all
				// previous commits. This prevents inheriting junk from a
				// failed pipeline run.
				if err := git(worktreePath, "fetch", "origin"); err != nil {
					return nil, fmt.Errorf("fetching origin for branch reset: %w", err)
				}
				var baseRef string
				if opts.BaseBranch != "" {
					baseRef = "origin/" + opts.BaseBranch
				} else {
					baseRef, _ = resolveBaseRef(ctx, worktreePath)
				}
				if baseRef != "" {
					if err := git(worktreePath, "reset", "--hard", baseRef); err != nil {
						return nil, fmt.Errorf("resetting branch to %s: %w", baseRef, err)
					}
				}
			}
			_ = git(worktreePath, "checkout", "--force", "HEAD")
			_ = git(worktreePath, "clean", "-fd")
			// Re-link node_modules — git clean -fd removes untracked symlinks.
			if !opts.LocalHead {
				if err := linkNodeModules(anvilPath, worktreePath); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to link node_modules: %v\n", err)
				}
			}
			return &Worktree{Path: worktreePath, Branch: targetBranch}, nil
		}
		// Not a valid worktree — remove the stale directory.
		if err := removeWithRetry(ctx, worktreePath); err != nil {
			return nil, fmt.Errorf("removing stale worktree directory %s: %w", worktreePath, err)
		}
	}

	if !opts.LocalHead {
		// Safety check: ensure the main repo is on main/master before creating a
		// worktree. If a previous smith ran git checkout in the parent directory
		// (corrupting the working environment), refuse immediately rather than
		// silently proceeding on a wrong base.
		if err := assertOnMainBranch(ctx, anvilPath); err != nil {
			return nil, fmt.Errorf("anvil branch safety check: %w", err)
		}

		// Fetch origin
		if err := git(anvilPath, "fetch", "origin"); err != nil {
			return nil, fmt.Errorf("git fetch: %w", err)
		}
	}

	if branchExists(ctx, anvilPath, targetBranch) && !opts.ResetBranch {
		// Distinguish local vs remote-only: `git worktree add <path> <branch>` requires
		// the local ref to exist. If only the remote branch exists, create a local
		// tracking branch from origin/<branch> instead.
		//
		// When ResetBranch is true we skip this path entirely and fall through to the
		// "create from base ref" path below. This ensures a clean worktree starting
		// from main, avoiding stale state from a previously merged batch branch whose
		// remote was auto-deleted (which would cause --force-with-lease to fail with
		// "(stale info)" on the subsequent push).
		localRef := "refs/heads/" + targetBranch
		if err := git(anvilPath, "show-ref", "--verify", "--quiet", localRef); err == nil {
			// Local branch exists; checkout directly.
			if err := git(anvilPath, "worktree", "add", "-f", worktreePath, targetBranch); err != nil {
				return nil, fmt.Errorf("git worktree add (existing local): %w", err)
			}
		} else {
			// Only remote branch exists; create a local tracking branch from origin/<branch>.
			remoteRef := "origin/" + targetBranch
			if err := git(anvilPath, "worktree", "add", "-f", "-b", targetBranch, worktreePath, remoteRef); err != nil {
				return nil, fmt.Errorf("git worktree add (from remote): %w", err)
			}
		}
	} else {
		// Determine base ref: use local HEAD for scan worktrees, an explicit
		// BaseBranch if provided, or auto-detect origin/main or origin/master.
		var baseRef string
		if opts.LocalHead {
			baseRef = "HEAD"
		} else if opts.BaseBranch != "" {
			baseRef = "origin/" + opts.BaseBranch
			// Verify the base branch exists on origin
			if err := git(anvilPath, "rev-parse", "--verify", baseRef); err != nil {
				return nil, fmt.Errorf("base branch %q not found on origin (ref %q): %w", opts.BaseBranch, baseRef, err)
			}
		} else {
			var err error
			baseRef, err = resolveBaseRef(ctx, anvilPath)
			if err != nil {
				return nil, fmt.Errorf("resolving base ref: %w", err)
			}
		}

		// If ResetBranch is requested and the local branch already exists (but
		// there is no matching worktree directory), delete it so that
		// "git worktree add -b" can recreate it cleanly from baseRef.
		if opts.ResetBranch {
			localRef := "refs/heads/" + targetBranch
			if err := git(anvilPath, "show-ref", "--verify", "--quiet", localRef); err == nil {
				if err := git(anvilPath, "branch", "-D", targetBranch); err != nil {
					return nil, fmt.Errorf("deleting stale local branch %s: %w", targetBranch, err)
				}
			}
		}

		// Create worktree with new branch
		if err := git(anvilPath, "worktree", "add", "-f", "-b", targetBranch, worktreePath, baseRef); err != nil {
			return nil, fmt.Errorf("git worktree add (new): %w", err)
		}
	}

	// Post-creation safety: verify the new worktree has a valid .git file
	// pointing to a real gitdir. Without this, a silent git failure could
	// leave a directory that git operations resolve to the parent repo.
	if err := verifyWorktreeGitFile(worktreePath); err != nil {
		_ = removeWithRetry(ctx, worktreePath)
		return nil, fmt.Errorf("post-creation worktree verification failed: %w", err)
	}

	// Install .beads/redirect so bd can find the beads database
	if err := installBeadsRedirect(anvilPath, worktreePath); err != nil {
		// Non-fatal: log but don't fail the worktree creation
		fmt.Fprintf(os.Stderr, "Warning: failed to install .beads/redirect: %v\n", err)
	}

	// Link node_modules from the main checkout so Smiths can run npm scripts
	// without a fresh npm ci. Skip for LocalHead (scan-only) worktrees where
	// the anvil path IS the worktree and linking would be circular.
	if !opts.LocalHead {
		if err := linkNodeModules(anvilPath, worktreePath); err != nil {
			// Non-fatal: later temper Node steps may fail if dependencies are not
			// already present or installed by another component/user.
			fmt.Fprintf(os.Stderr, "Warning: failed to link node_modules: %v\n", err)
		}
	}

	return &Worktree{
		BeadID:     beadID,
		AnvilPath:  anvilPath,
		Path:       worktreePath,
		Branch:     targetBranch,
		BaseBranch: opts.BaseBranch,
	}, nil
}

// CreateEpicBranch creates or verifies an epic feature branch from main and
// pushes it to origin. This is used when an epic bead is first picked up —
// the branch is created without any code changes so child beads can branch
// from it.
func (m *Manager) CreateEpicBranch(ctx context.Context, anvilPath, branchName string) error {
	// Validate branchName before passing to git. Bead labels are user-controlled,
	// so a name starting with "-" could be interpreted as a git flag.
	if err := validateBranchName(ctx, anvilPath, branchName); err != nil {
		return err
	}

	// Fetch origin
	if err := gitCmd(ctx, anvilPath, "fetch", "origin"); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}

	// Check if the branch already exists
	if branchExists(ctx, anvilPath, branchName) {
		return nil // Already exists, nothing to do
	}

	// Determine base ref (origin/main or origin/master)
	baseRef, err := resolveBaseRef(ctx, anvilPath)
	if err != nil {
		return fmt.Errorf("resolving base ref: %w", err)
	}

	// Use -- to prevent branchName from being parsed as a git option.
	if err := gitCmd(ctx, anvilPath, "branch", "--", branchName, baseRef); err != nil {
		return fmt.Errorf("creating epic branch %s: %w", branchName, err)
	}

	// Push to origin — -- ends option parsing before the refspec.
	if err := gitCmd(ctx, anvilPath, "push", "-u", "origin", "--", branchName); err != nil {
		return fmt.Errorf("pushing epic branch %s: %w", branchName, err)
	}

	return nil
}

// validateBranchName checks that branchName is a valid git branch name and
// does not start with "-" (which git could interpret as a flag).
func validateBranchName(ctx context.Context, dir, branchName string) error {
	if strings.HasPrefix(branchName, "-") {
		return fmt.Errorf("invalid branch name %q: must not start with '-'", branchName)
	}
	if err := gitCmd(ctx, dir, "check-ref-format", "--branch", branchName); err != nil {
		return fmt.Errorf("invalid branch name %q: %w", branchName, err)
	}
	return nil
}

// branchExists checks if a branch exists locally or on origin.
func branchExists(ctx context.Context, repoPath, branch string) bool {
	// Check local
	if err := gitCmd(ctx, repoPath, "rev-parse", "--verify", branch); err == nil {
		return true
	}
	// Check origin
	if err := gitCmd(ctx, repoPath, "rev-parse", "--verify", "origin/"+branch); err == nil {
		return true
	}
	return false
}

// Remove tears down a worktree and cleans up its branch.
func (m *Manager) Remove(ctx context.Context, anvilPath string, wt *Worktree) error {
	// Remove the git worktree
	if err := gitCmd(ctx, anvilPath, "worktree", "remove", "--force", wt.Path); err != nil {
		// If worktree removal fails, try manual cleanup
		_ = os.RemoveAll(wt.Path)
	}

	// Prune stale worktree references
	_ = gitCmd(ctx, anvilPath, "worktree", "prune")

	// Delete the local branch (best effort — might have been pushed)
	_ = gitCmd(ctx, anvilPath, "branch", "-D", wt.Branch)

	// NOTE: Do NOT delete the remote branch here. Worktree cleanup runs after
	// pipeline completion, and the remote branch is still needed by the PR that
	// was just created. Remote branch cleanup is handled by GitHub's auto-delete
	// setting or by Bellows after PR merge.

	return nil
}

// List returns the paths of all active worktrees under .workers/ for an anvil.
func (m *Manager) List(anvilPath string) ([]string, error) {
	workersDir := filepath.Join(anvilPath, m.WorkersDir)
	entries, err := os.ReadDir(workersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			paths = append(paths, filepath.Join(workersDir, e.Name()))
		}
	}
	return paths, nil
}

// installBeadsRedirect creates a .beads/redirect file in the worktree
// that points back to the main repo's .beads/ directory.
func installBeadsRedirect(anvilPath, worktreePath string) error {
	mainBeadsDir := filepath.Join(anvilPath, ".beads")
	if _, err := os.Stat(mainBeadsDir); os.IsNotExist(err) {
		return nil // No .beads in main repo, nothing to redirect
	}

	worktreeBeadsDir := filepath.Join(worktreePath, ".beads")
	if err := os.MkdirAll(worktreeBeadsDir, 0o755); err != nil {
		return err
	}

	redirectFile := filepath.Join(worktreeBeadsDir, "redirect")
	return os.WriteFile(redirectFile, []byte(mainBeadsDir+"\n"), 0o644)
}

// resolveBaseRef determines whether the repo uses origin/main or origin/master.
func resolveBaseRef(ctx context.Context, repoPath string) (string, error) {
	// Try origin/main first
	if err := gitCmd(ctx, repoPath, "rev-parse", "--verify", "origin/main"); err == nil {
		return "origin/main", nil
	}

	// Fall back to origin/master
	if err := gitCmd(ctx, repoPath, "rev-parse", "--verify", "origin/master"); err == nil {
		return "origin/master", nil
	}

	return "", fmt.Errorf("neither origin/main nor origin/master found")
}

// resolveValidatedGitDir reads and validates the .git file pointer in dir.
// Worktrees have a .git *file* (not directory) containing "gitdir: <path>".
// Returns the resolved, absolute gitdir path, or an error if the pointer is
// missing, malformed, or points to a nonexistent directory.
func resolveValidatedGitDir(dir string) (string, error) {
	gitPath := filepath.Join(dir, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil {
		return "", err
	}
	// A .git directory means this is a full repo clone, not a worktree.
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", gitPath)
	}
	content, err := os.ReadFile(gitPath)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(content))
	if !strings.HasPrefix(line, "gitdir: ") {
		return "", fmt.Errorf("%s does not contain a gitdir pointer", gitPath)
	}
	gitdir := strings.TrimPrefix(line, "gitdir: ")
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	if _, err := os.Stat(gitdir); err != nil {
		return "", err
	}
	return gitdir, nil
}

// isValidWorktree checks whether a directory is a valid git worktree.
// It verifies: (1) a .git file (not directory) exists — worktrees use a .git
// file pointing to the main repo, (2) the .git file content references a valid
// gitdir path on disk, (3) git rev-parse succeeds inside the directory.
// Without the .git file check, a directory that lost its worktree link (e.g.
// due to locked-file cleanup failure on Windows) would pass git rev-parse by
// walking up to the parent repo, causing Smith to edit the main checkout.
func isValidWorktree(ctx context.Context, dir string) bool {
	if _, err := resolveValidatedGitDir(dir); err != nil {
		return false
	}
	return gitCmd(ctx, dir, "rev-parse", "--is-inside-work-tree") == nil
}

// CurrentBranch returns the currently checked-out branch name for the
// repository at repoPath. Returns "HEAD" for detached HEAD state.
// Returns an error if git cannot determine the branch.
func CurrentBranch(ctx context.Context, repoPath string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", "rev-parse", "--abbrev-ref", "HEAD"))
	cmd.Dir = repoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return "", fmt.Errorf("git rev-parse --abbrev-ref HEAD: %s: %w", msg, err)
		}
		return "", fmt.Errorf("git rev-parse --abbrev-ref HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// isMainBranch reports whether branch is a valid default branch name (main,
// master, or detached HEAD).
func isMainBranch(branch string) bool {
	return branch == "main" || branch == "master" || branch == "HEAD"
}

// VerifyAndRecoverMain checks that the repository at repoPath is checked out to
// main, master, or a detached HEAD. If it is on a different branch, it attempts
// to recover by checking out main or master. It returns a boolean indicating whether
// recovery was attempted, the name of the original branch, and any error that occurred.
// If the current branch cannot be determined, it returns false, "", and the error.
func VerifyAndRecoverMain(ctx context.Context, repoPath string) (recovered bool, originalBranch string, err error) {
	currentBranch, err := CurrentBranch(ctx, repoPath)
	if err != nil {
		return false, "", fmt.Errorf("getting current branch: %w", err)
	}

	if isMainBranch(currentBranch) {
		return false, currentBranch, nil
	}

	// Attempt recovery with a bounded timeout, honoring caller cancellation.
	recoveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var checkoutErr error
	for _, branch := range []string{"main", "master"} {
		checkoutErr = gitCmd(recoveryCtx, repoPath, "checkout", branch)
		if checkoutErr == nil {
			return true, currentBranch, nil
		}
	}

	return true, currentBranch, fmt.Errorf("failed to checkout main or master: %w", checkoutErr)
}

// assertOnMainBranch returns an error if the repository at repoPath is not
// checked out to main, master, or a detached HEAD. This is a pre-flight guard
// before creating a worktree — if a previous smith accidentally checked out a
// feature branch in the main repo, this prevents further corruption.
func assertOnMainBranch(ctx context.Context, repoPath string) error {
	currentBranch, err := CurrentBranch(ctx, repoPath)
	if err != nil {
		// Cannot determine current branch — allow creation to proceed; the
		// subsequent git fetch will fail more informatively if something is broken.
		return nil
	}

	if isMainBranch(currentBranch) {
		return nil
	}

	return fmt.Errorf("main repo is checked out to %q instead of main/master — "+
		"refusing to create worktree to prevent environment corruption", currentBranch)
}

// gitCmdOut runs a git command in the given directory with a timeout,
// directing stdout and stderr to out.
func gitCmdOut(ctx context.Context, dir string, out io.Writer, args ...string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", args...))
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out

	return cmd.Run()
}

// gitCmd runs a git command in the given directory with a timeout.
func gitCmd(ctx context.Context, dir string, args ...string) error {
	return gitCmdOut(ctx, dir, os.Stderr, args...)
}

// BranchName returns the canonical forge branch name for a bead ID.
// This matches the branch created by CreateWithOptions when no Branch override
// is provided. Callers outside this package use this to predict the branch name
// without creating a worktree (e.g. checking for un-PR'd remote work).
func BranchName(beadID string) string {
	return "forge/" + sanitizePath(beadID)
}

// FetchBranch fetches a single named branch from origin in the given anvil
// directory, updating the local remote-tracking ref. This is the canonical
// way for daemon code to fetch a branch without going through a full worktree
// create/reset cycle.
func (m *Manager) FetchBranch(ctx context.Context, anvilPath, branchName string) error {
	return gitCmd(ctx, anvilPath, "fetch", "origin", "--", branchName)
}

// removeWithRetry attempts os.RemoveAll with exponential backoff and context
// cancellation support to handle Windows file-locking and antivirus delays.
// On non-Windows platforms it makes a single attempt.
func removeWithRetry(ctx context.Context, path string) error {
	delays := []time.Duration{0}
	if runtime.GOOS == "windows" {
		delays = []time.Duration{0, 1 * time.Second, 2 * time.Second, 4 * time.Second}
	}
	unlinkReparsePoints(path)

	var lastErr error
	for i, delay := range delays {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return fmt.Errorf("remove worktree %s canceled after %d attempts: %w", path, i, err)
			}
			return fmt.Errorf("remove worktree %s canceled: %w", path, err)
		}
		if delay > 0 {
			slog.Warn("retrying worktree directory removal",
				"path", path, "attempt", i+1, "total_attempts", len(delays), "backoff", delay)
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				if lastErr != nil {
					return fmt.Errorf("remove worktree %s canceled after %d attempts: %w", path, i, ctx.Err())
				}
				return fmt.Errorf("remove worktree %s canceled: %w", path, ctx.Err())
			}
		}
		lastErr = os.RemoveAll(path)
		if lastErr == nil {
			return nil
		}
		slog.Warn("worktree directory removal failed",
			"path", path, "attempt", i+1, "total_attempts", len(delays), "error", lastErr)
	}
	return fmt.Errorf("failed after %d attempts: %w", len(delays), lastErr)
}

// RemoveWithRetry is the exported form of removeWithRetry for use by daemon-level
// stale directory cleanup.
func RemoveWithRetry(ctx context.Context, path string) error {
	return removeWithRetry(ctx, path)
}

// verifyWorktreeGitFile checks that a worktree directory contains a valid .git
// file (not directory) whose gitdir target exists on disk.
func verifyWorktreeGitFile(worktreePath string) error {
	if _, err := resolveValidatedGitDir(worktreePath); err != nil {
		return fmt.Errorf("worktree %s has invalid .git file: %w", worktreePath, err)
	}
	return nil
}

// ValidateWorktreeDir checks that the given directory is safe to run Smith in.
// If the directory is not inside any git repository (e.g. an os.MkdirTemp dir
// used by schematic or wicket), the check passes — there is no parent checkout
// to accidentally inherit. If the directory is inside a git repo, it must have
// a valid worktree .git file pointer to ensure isolation from the main checkout.
func ValidateWorktreeDir(worktreePath string) error {
	_, gitFileErr := resolveValidatedGitDir(worktreePath)
	if gitFileErr == nil {
		// Has a valid .git file pointer — this is a proper worktree.
		return nil
	}
	// No valid .git file. Check whether the directory is nonetheless inside a
	// git repository (which would mean git commands resolve to the parent repo).
	// If it is not inside any repo (e.g. an os.MkdirTemp dir), there is nothing
	// to protect against and we allow the run.
	cmdCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", "rev-parse", "--git-dir"))
	cmd.Dir = worktreePath
	if err := cmd.Run(); err != nil {
		// Not inside any git repository — safe to proceed.
		return nil
	}
	// Inside a git repo without proper worktree isolation — refuse.
	return fmt.Errorf("directory %s is inside a git repository but lacks a valid worktree .git file: %w", worktreePath, gitFileErr)
}

// sanitizePath converts a bead ID to a safe directory/branch name.
// E.g., "Forge-n1g.4.1" → "Forge-n1g.4.1" (dots are fine in git branches).
// Slashes and other problematic chars are replaced.
func sanitizePath(beadID string) string {
	// Replace characters that are problematic in file paths or branch names
	r := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		" ", "-",
		":", "-",
	)
	return r.Replace(beadID)
}
