// Package worktree manages git worktree creation and teardown for Smith workers.
//
// Each Smith operates in an isolated git worktree under .workers/<bead-id>/
// in the anvil's repository directory. The worktree is branched from origin/main
// with a forge-prefixed branch name.
package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

	// Ensure .workers directory exists
	if err := os.MkdirAll(workersDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating workers directory: %w", err)
	}

	// Check if worktree already exists
	if _, err := os.Stat(worktreePath); err == nil {
		return nil, fmt.Errorf("worktree already exists at %s", worktreePath)
	}

	// Fetch origin
	if err := gitCmd(ctx, anvilPath, "fetch", "origin"); err != nil {
		return nil, fmt.Errorf("git fetch: %w", err)
	}

	if branchExists(ctx, anvilPath, targetBranch) {
		// Distinguish local vs remote-only: `git worktree add <path> <branch>` requires
		// the local ref to exist. If only the remote branch exists, create a local
		// tracking branch from origin/<branch> instead.
		localRef := "refs/heads/" + targetBranch
		if err := gitCmd(ctx, anvilPath, "show-ref", "--verify", "--quiet", localRef); err == nil {
			// Local branch exists; checkout directly.
			if err := gitCmd(ctx, anvilPath, "worktree", "add", "-f", worktreePath, targetBranch); err != nil {
				return nil, fmt.Errorf("git worktree add (existing local): %w", err)
			}
		} else {
			// Only remote branch exists; create a local tracking branch from origin/<branch>.
			remoteRef := "origin/" + targetBranch
			if err := gitCmd(ctx, anvilPath, "worktree", "add", "-f", "-b", targetBranch, worktreePath, remoteRef); err != nil {
				return nil, fmt.Errorf("git worktree add (from remote): %w", err)
			}
		}
	} else {
		// Determine base ref: use explicit BaseBranch if provided, otherwise
		// auto-detect origin/main or origin/master.
		var baseRef string
		if opts.BaseBranch != "" {
			baseRef = "origin/" + opts.BaseBranch
			// Verify the base branch exists on origin
			if err := gitCmd(ctx, anvilPath, "rev-parse", "--verify", baseRef); err != nil {
				return nil, fmt.Errorf("epic base branch %q not found on origin: %w", opts.BaseBranch, err)
			}
		} else {
			var err error
			baseRef, err = resolveBaseRef(ctx, anvilPath)
			if err != nil {
				return nil, fmt.Errorf("resolving base ref: %w", err)
			}
		}

		// Create worktree with new branch
		if err := gitCmd(ctx, anvilPath, "worktree", "add", "-f", "-b", targetBranch, worktreePath, baseRef); err != nil {
			return nil, fmt.Errorf("git worktree add (new): %w", err)
		}
	}

	// Install .beads/redirect so bd can find the beads database
	if err := installBeadsRedirect(anvilPath, worktreePath); err != nil {
		// Non-fatal: log but don't fail the worktree creation
		fmt.Fprintf(os.Stderr, "Warning: failed to install .beads/redirect: %v\n", err)
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

	// Create the branch locally
	if err := gitCmd(ctx, anvilPath, "branch", branchName, baseRef); err != nil {
		return fmt.Errorf("creating epic branch %s: %w", branchName, err)
	}

	// Push to origin
	if err := gitCmd(ctx, anvilPath, "push", "-u", "origin", branchName); err != nil {
		return fmt.Errorf("pushing epic branch %s: %w", branchName, err)
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

	// Delete the branch (best effort — might have been pushed)
	_ = gitCmd(ctx, anvilPath, "branch", "-D", wt.Branch)

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

// gitCmd runs a git command in the given directory with a timeout.
func gitCmd(ctx context.Context, dir string, args ...string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", args...))
	cmd.Dir = dir
	cmd.Stdout = os.Stderr // Git output goes to stderr for debugging
	cmd.Stderr = os.Stderr

	return cmd.Run()
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
