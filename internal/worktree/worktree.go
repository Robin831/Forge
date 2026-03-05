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
// It:
//  1. Fetches origin to ensure we have latest
//  2. Creates a branch named forge/<bead-id>
//  3. Creates a git worktree at .workers/<bead-id>/
//  4. Installs a .beads/redirect file so bd works in the worktree
func (m *Manager) Create(ctx context.Context, anvilPath, beadID string) (*Worktree, error) {
	workersDir := filepath.Join(anvilPath, m.WorkersDir)
	worktreePath := filepath.Join(workersDir, sanitizePath(beadID))
	branch := "forge/" + sanitizePath(beadID)

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

	// Determine base ref (origin/main or origin/master)
	baseRef, err := resolveBaseRef(ctx, anvilPath)
	if err != nil {
		return nil, fmt.Errorf("resolving base ref: %w", err)
	}

	// Create worktree with new branch
	if err := gitCmd(ctx, anvilPath, "worktree", "add", "-b", branch, worktreePath, baseRef); err != nil {
		return nil, fmt.Errorf("git worktree add: %w", err)
	}

	// Install .beads/redirect so bd can find the beads database
	if err := installBeadsRedirect(anvilPath, worktreePath); err != nil {
		// Non-fatal: log but don't fail the worktree creation
		fmt.Fprintf(os.Stderr, "Warning: failed to install .beads/redirect: %v\n", err)
	}

	return &Worktree{
		BeadID:    beadID,
		AnvilPath: anvilPath,
		Path:      worktreePath,
		Branch:    branch,
	}, nil
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

	cmd := exec.CommandContext(cmdCtx, "git", args...)
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
