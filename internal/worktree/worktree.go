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
	// SkipNodeModulesJunction, when true, skips linking node_modules from
	// the main checkout. Used for dependency-update beads where npm install
	// must write to a local node_modules to avoid corrupting the main
	// checkout's dependencies.
	SkipNodeModulesJunction bool
	// PreserveExisting, when true and the target worktree already exists as a
	// valid git worktree, reuses it AS-IS: the normal `git checkout --force HEAD`
	// + `git clean -fd` reset is skipped so uncommitted and untracked work is
	// preserved. Used by daemon-restart recovery to resume a bead that was
	// parked by an operator pause — the retained worktree must survive exactly as
	// it was when the pause fired so `claude --resume` continues in place. Has no
	// effect when the worktree does not yet exist (a fresh worktree is created
	// normally). Mutually exclusive with ResetBranch, which is ignored when this
	// is set.
	PreserveExisting bool
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

	// Self-heal any pre-existing stale core.worktree setting on the main repo
	// before we run any git commands that would touch it. A stale value
	// pointing to a removed worker path breaks `git status --porcelain`
	// (exit 128 "must be run in a work tree"), which in turn breaks Go's
	// VCS stamping during `go build` from this worktree.
	if err := CleanStaleCoreWorktree(ctx, anvilPath); err != nil {
		// Non-fatal: log and continue. If the unset truly fails, downstream
		// git commands will surface a clearer error.
		slog.Warn("worktree: failed to clean stale core.worktree", "anvil", anvilPath, "error", err)
	}

	// If worktree directory already exists, check whether it is a valid git
	// worktree. If so, reset it to a clean state and reuse it. If not (e.g.
	// leftover directory from a failed run), remove it so we can create fresh.
	if _, err := os.Stat(worktreePath); err == nil {
		if isValidWorktree(ctx, worktreePath) {
			// Unlink junctions/symlinks (e.g. node_modules) BEFORE any git
			// operations that might follow them into the main checkout and
			// destroy content there. On Windows, git clean -fd traverses
			// junctions as regular directories and deletes the target's files.
			unlinkReparsePoints(worktreePath)

			// PreserveExisting: reuse the retained worktree exactly as-is (no
			// reset/checkout/clean) so a paused bead's in-progress work survives
			// into the resume. Only re-link node_modules and return.
			if opts.PreserveExisting {
				if !opts.LocalHead && !opts.SkipNodeModulesJunction {
					if err := linkNodeModules(anvilPath, worktreePath); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: failed to link node_modules: %v\n", err)
					}
				}
				return &Worktree{Path: worktreePath, Branch: targetBranch}, nil
			}

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
			_ = git(worktreePath, "clean", "-fd", "-e", "node_modules")
			// Re-link node_modules for the worktree.
			if !opts.LocalHead && !opts.SkipNodeModulesJunction {
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
		remoteRef := "origin/" + targetBranch
		remoteExists := git(anvilPath, "show-ref", "--verify", "--quiet", "refs/remotes/"+remoteRef) == nil
		if err := git(anvilPath, "show-ref", "--verify", "--quiet", localRef); err == nil {
			// Local branch exists. When the remote-tracking ref also exists we
			// force the local branch to the remote tip with `-B` — bellows /
			// burnish / quench may be assembling a worktree for a branch that
			// another worker (in this same forge or another) just rebased and
			// force-pushed, leaving the local ref pointing at the pre-rebase
			// tip. A plain `worktree add ... <branch>` checks out that stale
			// ref and any commit produced on top gets rejected as
			// non-fast-forward at push time. Fall back to a plain checkout
			// (no `-B`) when no remote-tracking ref exists, so local-only
			// branches still work and we don't pass an unresolvable revision.
			if remoteExists {
				if err := git(anvilPath, "worktree", "add", "-f", "-B", targetBranch, worktreePath, remoteRef); err != nil {
					return nil, fmt.Errorf("git worktree add (reset local to %s): %w", remoteRef, err)
				}
			} else {
				if err := git(anvilPath, "worktree", "add", "-f", worktreePath, targetBranch); err != nil {
					return nil, fmt.Errorf("git worktree add (existing local, no remote): %w", err)
				}
			}
		} else {
			// Only remote branch exists; create a local tracking branch from origin/<branch>.
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
	// the anvil path IS the worktree and linking would be circular. Also skip
	// for dependency-update beads that need a fresh local node_modules.
	if !opts.LocalHead && !opts.SkipNodeModulesJunction {
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
	// Unlink junctions/symlinks (e.g. node_modules) before removal so that
	// git worktree remove and os.RemoveAll don't walk into and destroy the
	// target content in the main checkout.
	unlinkReparsePoints(wt.Path)

	// Remove the git worktree
	ProbeNodeModules("before-worktree-remove", anvilPath)
	if err := gitCmd(ctx, anvilPath, "worktree", "remove", "--force", wt.Path); err != nil {
		ProbeNodeModules("after-worktree-remove-failed", anvilPath)
		// If worktree removal fails, try manual cleanup
		ProbeNodeModules("before-removeall-fallback", anvilPath)
		_ = os.RemoveAll(wt.Path)
		ProbeNodeModules("after-removeall-fallback", anvilPath)
	} else {
		ProbeNodeModules("after-worktree-remove", anvilPath)
	}

	// Prune stale worktree references
	ProbeNodeModules("before-worktree-prune", anvilPath)
	_ = gitCmd(ctx, anvilPath, "worktree", "prune")
	ProbeNodeModules("after-worktree-prune", anvilPath)

	// Delete the local branch (best effort — might have been pushed)
	ProbeNodeModules("before-branch-delete", anvilPath)
	_ = gitCmd(ctx, anvilPath, "branch", "-D", wt.Branch)
	ProbeNodeModules("after-branch-delete", anvilPath)

	// NOTE: Do NOT delete the remote branch here. Worktree cleanup runs after
	// pipeline completion, and the remote branch is still needed by the PR that
	// was just created. Remote branch cleanup is handled by GitHub's auto-delete
	// setting or by Bellows after PR merge.

	// Belt-and-braces: ensure no stale core.worktree setting is left on the
	// main repo pointing at the path we just removed. `git worktree remove`
	// does not write core.worktree, but older Forge versions or external
	// tooling may have done so; this keeps the main repo healthy regardless.
	if err := CleanStaleCoreWorktree(ctx, anvilPath); err != nil {
		slog.Warn("worktree: failed to clean stale core.worktree after remove",
			"anvil", anvilPath, "error", err)
	}

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

// CleanStaleCoreWorktree removes a stale core.worktree setting from the main
// repository's .git/config when it points to a path that no longer exists or
// is empty. Such a stale setting breaks tooling that resolves the main repo's
// gitdir — notably Go's VCS stamping, which cd's into the main repo via the
// worktree's .git pointer and runs `git status --porcelain`. When core.worktree
// points to a missing/empty path, that command fails with exit 128
// ("fatal: this operation must be run in a work tree"), which in turn breaks
// `go build` for any worker. Forge itself does not set core.worktree on the
// main repo (per-worktree values live in .git/worktrees/<name>/config.worktree
// and are managed by `git worktree add`), but older Forge versions, manual git
// invocations, or third-party tools may have written one. This helper is the
// idempotent self-heal.
//
// The check is conservative: it only unsets when the configured path is
// missing or empty. A path that exists with content is left alone, in case it
// was set deliberately by the user. Errors are non-fatal — callers should log
// and continue.
func CleanStaleCoreWorktree(ctx context.Context, anvilPath string) error {
	value, err := readCoreWorktree(ctx, anvilPath)
	if err != nil {
		return fmt.Errorf("reading core.worktree: %w", err)
	}
	if value == "" {
		return nil
	}
	resolved := resolveWorktreePath(anvilPath, value)
	if !isStaleWorktreePath(resolved) {
		return nil
	}
	if err := unsetCoreWorktree(ctx, anvilPath); err != nil {
		return fmt.Errorf("unsetting stale core.worktree=%q: %w", value, err)
	}
	slog.Info("worktree: unset stale core.worktree on main repo",
		"anvil", anvilPath, "stale_value", value)
	return nil
}

// localGitEnv returns the process environment with Forge-set git overrides
// stripped so that git commands discover the repo from cmd.Dir rather than
// from inherited GIT_DIR / GIT_WORK_TREE values.
func localGitEnv() []string {
	skip := map[string]bool{
		"GIT_DIR":                 true,
		"GIT_WORK_TREE":           true,
		"GIT_CEILING_DIRECTORIES": true,
	}
	base := os.Environ()
	out := make([]string, 0, len(base))
	for _, e := range base {
		key, _, _ := strings.Cut(e, "=")
		if !skip[key] {
			out = append(out, e)
		}
	}
	return out
}

// anvilGitEnv returns an environment suitable for git commands that should
// operate on anvilPath. It strips any inherited GIT_DIR / GIT_WORK_TREE /
// GIT_CEILING_DIRECTORIES overrides (Forge sets these in worktree subprocesses)
// and then pins GIT_WORK_TREE to anvilPath. The pinned GIT_WORK_TREE overrides
// any core.worktree value in .git/config so git does not try to chdir to a
// possibly-missing path before the command even runs.
func anvilGitEnv(anvilPath string) []string {
	out := localGitEnv()
	out = append(out, "GIT_WORK_TREE="+anvilPath)
	return out
}

// anvilGitConfigFile returns the path to the main repo's .git/config file.
// For a standard (non-bare) repository, this is always <anvilPath>/.git/config.
// We locate it directly from the filesystem to avoid running git commands that
// would fail when core.worktree is set to a missing path.
func anvilGitConfigFile(anvilPath string) string {
	return filepath.Join(anvilPath, ".git", "config")
}

// resolveWorktreePath resolves a core.worktree config value to an absolute
// filesystem path. Git interprets relative values relative to the repo's
// gitdir (the .git directory), not the process working directory. We resolve
// the gitdir from the filesystem rather than via git to avoid failures when
// core.worktree itself is already broken.
func resolveWorktreePath(anvilPath, value string) string {
	if filepath.IsAbs(value) {
		return value
	}
	// For a standard repo the gitdir is <anvilPath>/.git. We check directly
	// rather than running `git rev-parse --git-dir` because git may refuse to
	// start when core.worktree points to a missing directory.
	gitDir := filepath.Join(anvilPath, ".git")
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		// Non-standard layout — fall back to resolving relative to anvilPath.
		return filepath.Join(anvilPath, value)
	}
	return filepath.Join(gitDir, value)
}

// readCoreWorktree returns the value of core.worktree from the main repo's
// .git/config, or "" if the key is unset. It uses `git config --file` to
// read the config file directly, bypassing git's working-tree validation so
// the function succeeds even when core.worktree already points to a missing
// path (which would cause `git config --local` to exit 128).
func readCoreWorktree(ctx context.Context, anvilPath string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", "config", "--file", anvilGitConfigFile(anvilPath), "--get", "core.worktree"))
	cmd.Dir = anvilPath
	cmd.Env = anvilGitEnv(anvilPath)
	out, err := cmd.Output()
	if err != nil {
		// Exit code 1 from `git config --get` means the key is unset; treat as no value.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// unsetCoreWorktree removes the core.worktree key from the main repo's
// .git/config. Uses `git config --file` to write the config file directly,
// bypassing git's working-tree validation. Tolerates "key not set" (exit
// code 5) — the result we want is already true.
func unsetCoreWorktree(ctx context.Context, anvilPath string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", "config", "--file", anvilGitConfigFile(anvilPath), "--unset", "core.worktree"))
	cmd.Dir = anvilPath
	cmd.Env = anvilGitEnv(anvilPath)
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 5 {
			return nil
		}
		return err
	}
	return nil
}

// isStaleWorktreePath reports whether path is missing on disk or is an empty
// directory. Both conditions break `git status --porcelain` against the main
// repo when configured as core.worktree.
func isStaleWorktreePath(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return os.IsNotExist(err)
	}
	if !info.IsDir() {
		return true
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	return len(entries) == 0
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
	info, statErr := os.Stat(gitdir)
	if statErr != nil {
		return "", statErr
	}
	if !info.IsDir() {
		return "", fmt.Errorf("gitdir %q is not a directory", gitdir)
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
	cmd.Env = localGitEnv()
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
	cmd.Env = localGitEnv()
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

// BeadIDFromBranch is the inverse of BranchName: given a canonical forge branch
// name of the form "forge/<bead-id>" it returns the bead ID and true. For any
// branch outside the "forge/" namespace (or a bare "forge/" with no bead
// segment) it returns ("", false).
//
// SanitizePath (applied by BranchName) is lossy — it folds '/', '\\', ' ' and
// ':' to '-' — so the recovered ID matches the original only for bead IDs that
// contain none of those characters. That holds for every bd-issued ID (e.g.
// "Forge-abc1", "Forge-n1g.4.1"), so the function returns the branch suffix
// verbatim. Callers use it to recover the bead ID encoded in a forge branch
// when no other source (e.g. a PR body reference) is available.
func BeadIDFromBranch(branch string) (string, bool) {
	const prefix = "forge/"
	if !strings.HasPrefix(branch, prefix) {
		return "", false
	}
	id := strings.TrimPrefix(branch, prefix)
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
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

	anvilPath := inferAnvilPath(path)
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
		if anvilPath != "" {
			ProbeNodeModules(fmt.Sprintf("before-removeall-retry-%d", i), anvilPath)
		}
		lastErr = os.RemoveAll(path)
		if anvilPath != "" {
			ProbeNodeModules(fmt.Sprintf("after-removeall-retry-%d", i), anvilPath)
		}
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

// UnlinkReparsePoints is the exported form of unlinkReparsePoints for use by
// other packages that need to safely remove worktree directories.
func UnlinkReparsePoints(path string) {
	unlinkReparsePoints(path)
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
	cmd.Env = localGitEnv()
	if err := cmd.Run(); err != nil {
		// Not inside any git repository — safe to proceed.
		return nil
	}
	// Inside a git repo without proper worktree isolation — refuse.
	return fmt.Errorf("directory %s is inside a git repository but lacks a valid worktree .git file: %w", worktreePath, gitFileErr)
}

// GitEnv returns the git environment variables that confine git operations to
// the given worktree even when the child process changes directory away from
// it. Smith runs claude inside <anvil>/.workers/<bead-id>/, which is a
// subdirectory of the anvil repo; without these variables, a stray "cd .." in
// a tool_use bash command can walk cwd into the anvil and have git apply add /
// commit / push to the parent repo's currently-checked-out branch (typically
// main). Setting GIT_DIR + GIT_WORK_TREE binds every child git invocation to
// the worktree regardless of cwd. GIT_CEILING_DIRECTORIES is added as belt-
// and-suspenders so that even if a nested shell unsets GIT_DIR, git's repo
// discovery cannot walk up past .workers/ into the parent repo.
//
// The returned slice is suitable for appending directly to a Cmd.Env. If
// worktreePath is not a valid git worktree (e.g. an os.MkdirTemp dir used by
// schematic / wicket / warden-learn) the function returns nil so that those
// non-repo runs are unaffected.
func GitEnv(worktreePath string) []string {
	gitdir, err := resolveValidatedGitDir(worktreePath)
	if err != nil {
		return nil
	}
	abs, err := filepath.Abs(worktreePath)
	if err != nil {
		return nil
	}
	env := []string{
		"GIT_DIR=" + gitdir,
		"GIT_WORK_TREE=" + abs,
	}
	// GIT_CEILING_DIRECTORIES is best-effort defense-in-depth: if a nested
	// shell unsets GIT_DIR and the cwd is somewhere inside the worktree
	// subtree, the ceiling stops git's upward walk at .workers/ before it
	// can reach the anvil's .git directory. It does NOT protect when cwd
	// itself escapes to the anvil root (git checks cwd before ascending),
	// but in that case the explicit GIT_DIR above is what wins.
	if parent := filepath.Dir(abs); parent != "" && parent != abs {
		env = append(env, "GIT_CEILING_DIRECTORIES="+parent)
	}
	return env
}

// SanitizePath converts a bead ID to a safe directory/branch name.
// E.g., "Forge-n1g.4.1" → "Forge-n1g.4.1" (dots are fine in git branches).
// Slashes and other problematic chars are replaced with dashes.
func SanitizePath(beadID string) string {
	r := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		" ", "-",
		":", "-",
	)
	return r.Replace(beadID)
}

// sanitizePath is the unexported alias kept for internal callers.
func sanitizePath(beadID string) string { return SanitizePath(beadID) }
