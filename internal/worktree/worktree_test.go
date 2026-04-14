package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizePath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Forge-n1g.4.1", "Forge-n1g.4.1"},
		{"feat/fix-bug", "feat-fix-bug"},
		{"fix:typo", "fix-typo"},
		{"my work", "my-work"},
		{"a\\b", "a-b"},
	}

	for _, tt := range tests {
		got := sanitizePath(tt.input)
		if got != tt.expected {
			t.Errorf("sanitizePath(%q) = %q; want %q", tt.input, got, tt.expected)
		}
	}
}

// runGit runs a git command in dir and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// initBareRemoteCloneWithInitialCommit creates a bare remote repository and a
// clone of it with an initial commit pushed to origin/main. Returns the paths
// of the bare remote and the clone (anvil) directory.
func initBareRemoteCloneWithInitialCommit(t *testing.T) (remoteDir, cloneDir string) {
	t.Helper()

	remoteDir = t.TempDir()
	runGit(t, remoteDir, "init", "--bare", "--initial-branch=main")

	cloneDir = t.TempDir()
	runGit(t, cloneDir, "clone", remoteDir, ".")
	runGit(t, cloneDir, "config", "user.email", "test@example.com")
	runGit(t, cloneDir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(cloneDir, "README"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, cloneDir, "add", "README")
	runGit(t, cloneDir, "commit", "-m", "init")
	runGit(t, cloneDir, "push", "origin", "main")

	return remoteDir, cloneDir
}

// initTestRepo creates a minimal git repository in dir with one commit on
// the given branch. It configures a local user identity to avoid relying on
// global git config (which may be absent in CI).
func initTestRepo(t *testing.T, dir, branch string) {
	t.Helper()
	runGit(t, dir, "init", "--initial-branch="+branch)
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	// Create an initial commit so HEAD is resolvable.
	readme := filepath.Join(dir, "README")
	if err := os.WriteFile(readme, []byte("test\n"), 0o644); err != nil {
		t.Fatalf("writing README: %v", err)
	}
	runGit(t, dir, "add", "README")
	runGit(t, dir, "commit", "-m", "init")
}

func TestCurrentBranch_OnMain(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "main")

	branch, err := CurrentBranch(context.Background(), dir)
	if err != nil {
		t.Fatalf("CurrentBranch: unexpected error: %v", err)
	}
	if branch != "main" {
		t.Errorf("CurrentBranch = %q; want %q", branch, "main")
	}
}

func TestCurrentBranch_OnFeatureBranch(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "main")

	// Create and switch to a feature branch.
	cmd := exec.Command("git", "checkout", "-b", "forge/test-bead")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b: %v\n%s", err, out)
	}

	branch, err := CurrentBranch(context.Background(), dir)
	if err != nil {
		t.Fatalf("CurrentBranch: unexpected error: %v", err)
	}
	if branch != "forge/test-bead" {
		t.Errorf("CurrentBranch = %q; want %q", branch, "forge/test-bead")
	}
}

func TestAssertOnMainBranch_OnMain(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "main")

	if err := assertOnMainBranch(context.Background(), dir); err != nil {
		t.Errorf("assertOnMainBranch on main: unexpected error: %v", err)
	}
}

func TestAssertOnMainBranch_OnMaster(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "master")

	if err := assertOnMainBranch(context.Background(), dir); err != nil {
		t.Errorf("assertOnMainBranch on master: unexpected error: %v", err)
	}
}

func TestAssertOnMainBranch_OnFeatureBranch(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "main")

	// Simulate environment corruption: checkout a feature branch.
	cmd := exec.Command("git", "checkout", "-b", "forge/Forge-x1bs")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b: %v\n%s", err, out)
	}

	err := assertOnMainBranch(context.Background(), dir)
	if err == nil {
		t.Fatal("assertOnMainBranch: expected error on feature branch, got nil")
	}
	// Error message should mention the offending branch name.
	const want = "forge/Forge-x1bs"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("assertOnMainBranch error %q does not mention branch %q", err.Error(), want)
	}
}

func TestAssertOnMainBranch_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	// Not a git repo — CurrentBranch will error, which assertOnMainBranch
	// treats as non-fatal (returns nil).
	if err := assertOnMainBranch(context.Background(), dir); err != nil {
		t.Errorf("assertOnMainBranch on non-repo: expected nil (non-fatal), got %v", err)
	}
}

func TestVerifyAndRecoverMain_OnMain(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "main")

	recovered, branch, err := VerifyAndRecoverMain(context.Background(), dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recovered {
		t.Errorf("expected recovered=false on main branch")
	}
	if branch != "main" {
		t.Errorf("expected branch=main, got %q", branch)
	}
}

func TestVerifyAndRecoverMain_OnFeatureBranch(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "main")

	// Simulate environment corruption: checkout a feature branch.
	cmd := exec.Command("git", "checkout", "-b", "forge/Forge-x1bs")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b: %v\n%s", err, out)
	}

	recovered, branch, err := VerifyAndRecoverMain(context.Background(), dir)
	if err != nil {
		t.Fatalf("unexpected error during recovery: %v", err)
	}
	if !recovered {
		t.Errorf("expected recovered=true on feature branch")
	}
	if branch != "forge/Forge-x1bs" {
		t.Errorf("expected original branch=forge/Forge-x1bs, got %q", branch)
	}

	// Verify we are back on main
	current, _ := CurrentBranch(context.Background(), dir)
	if current != "main" {
		t.Errorf("expected to be recovered to main, got %q", current)
	}
}

func TestVerifyAndRecoverMain_RecoveryFails(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "feature-only")
	// There is no main/master branch, so recovery should fail.

	recovered, branch, err := VerifyAndRecoverMain(context.Background(), dir)
	if err == nil {
		t.Fatalf("expected error when recovery fails, got nil")
	}
	if !recovered {
		t.Errorf("expected recovered=true since recovery was attempted")
	}
	if branch != "feature-only" {
		t.Errorf("expected original branch=feature-only, got %q", branch)
	}
}

// TestCreateWithOptions_ResetBranch verifies that ResetBranch=true discards
// commits made by a previous pipeline run and resets the branch back to the
// base ref (origin/main).
func TestCreateWithOptions_ResetBranch(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	// Record the base commit hash (this is origin/main).
	baseHash := gitOutput(t, anvilDir, "rev-parse", "origin/main")

	// First call: create worktree normally.
	mgr := NewManager()
	ctx := context.Background()
	wt, err := mgr.CreateWithOptions(ctx, anvilDir, "test-bead", CreateOptions{})
	if err != nil {
		t.Fatalf("first CreateWithOptions: %v", err)
	}

	// Simulate a bad Smith run: write a file, commit, and push.
	badFile := filepath.Join(wt.Path, "bad-change.txt")
	if err := os.WriteFile(badFile, []byte("junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, wt.Path, "add", "bad-change.txt")
	runGit(t, wt.Path, "commit", "-m", "bad commit from failed smith")
	runGit(t, wt.Path, "push", "origin", wt.Branch)

	badHash := gitOutput(t, wt.Path, "rev-parse", "HEAD")
	if badHash == baseHash {
		t.Fatal("bad commit should differ from base")
	}

	// Second call WITHOUT ResetBranch: should reuse with bad commit intact.
	wt2, err := mgr.CreateWithOptions(ctx, anvilDir, "test-bead", CreateOptions{})
	if err != nil {
		t.Fatalf("second CreateWithOptions (no reset): %v", err)
	}
	hashAfterReuse := gitOutput(t, wt2.Path, "rev-parse", "HEAD")
	if hashAfterReuse != badHash {
		t.Errorf("without ResetBranch: expected HEAD=%s (bad), got %s", badHash, hashAfterReuse)
	}

	// Third call WITH ResetBranch: should reset back to origin/main.
	wt3, err := mgr.CreateWithOptions(ctx, anvilDir, "test-bead", CreateOptions{
		ResetBranch: true,
	})
	if err != nil {
		t.Fatalf("third CreateWithOptions (with reset): %v", err)
	}
	hashAfterReset := gitOutput(t, wt3.Path, "rev-parse", "HEAD")
	if hashAfterReset != baseHash {
		t.Errorf("with ResetBranch: expected HEAD=%s (base), got %s", baseHash, hashAfterReset)
	}

	// Verify the bad file is gone.
	if _, err := os.Stat(filepath.Join(wt3.Path, "bad-change.txt")); !os.IsNotExist(err) {
		t.Error("with ResetBranch: bad-change.txt should not exist after reset")
	}
}

// TestRemove_DoesNotDeleteRemoteBranch is a regression test for Forge-0mmb.
// Manager.Remove must NOT delete the remote branch — it is still needed by
// the PR that was just created. Remote branch cleanup is handled by GitHub's
// auto-delete setting or Bellows after merge.
func TestRemove_DoesNotDeleteRemoteBranch(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	// Create a worktree (simulates the pipeline creating the bead branch).
	mgr := NewManager()
	ctx := context.Background()
	wt, err := mgr.CreateWithOptions(ctx, anvilDir, "test-bead", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}

	// Push the branch to origin (simulates Smith pushing the implementation).
	runGit(t, wt.Path, "push", "-u", "origin", wt.Branch)

	// Verify the remote branch exists before Remove().
	remotesBefore := gitOutput(t, anvilDir, "ls-remote", "--heads", "origin", wt.Branch)
	if remotesBefore == "" {
		t.Fatalf("expected remote branch %q to exist before Remove()", wt.Branch)
	}

	// Remove the worktree (simulates daemon cleanup after PR creation).
	if err := mgr.Remove(ctx, anvilDir, wt); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// The remote branch must still exist — the PR depends on it.
	remotesAfter := gitOutput(t, anvilDir, "ls-remote", "--heads", "origin", wt.Branch)
	if remotesAfter == "" {
		t.Errorf("Remove() deleted remote branch %q — this breaks PR creation (regression: Forge-0mmb)", wt.Branch)
	}
}

func TestIsValidWorktree_MissingGitFile(t *testing.T) {
	dir := t.TempDir()
	// Empty directory — no .git file at all.
	if isValidWorktree(context.Background(), dir) {
		t.Error("isValidWorktree should return false for directory without .git file")
	}
}

func TestIsValidWorktree_GitDirectory(t *testing.T) {
	dir := t.TempDir()
	// Create a .git directory (like a full repo clone, not a worktree).
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isValidWorktree(context.Background(), dir) {
		t.Error("isValidWorktree should return false when .git is a directory")
	}
}

func TestIsValidWorktree_BadGitFileContent(t *testing.T) {
	dir := t.TempDir()
	// Create a .git file with invalid content.
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("not a gitdir pointer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isValidWorktree(context.Background(), dir) {
		t.Error("isValidWorktree should return false when .git file has bad content")
	}
}

func TestIsValidWorktree_GitFilePointsToNonexistent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /nonexistent/path"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isValidWorktree(context.Background(), dir) {
		t.Error("isValidWorktree should return false when .git file points to nonexistent gitdir")
	}
}

func TestIsValidWorktree_RealWorktree(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	mgr := NewManager()
	wt, err := mgr.CreateWithOptions(context.Background(), anvilDir, "valid-test", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}

	if !isValidWorktree(context.Background(), wt.Path) {
		t.Error("isValidWorktree should return true for a real worktree")
	}
}

func TestValidateWorktreeDir_NonRepoTempDir(t *testing.T) {
	// A temp dir outside any git repo should be allowed — schematic/wicket use
	// os.MkdirTemp dirs for non-repo Smith runs.
	dir := t.TempDir()
	if err := ValidateWorktreeDir(dir); err != nil {
		t.Errorf("ValidateWorktreeDir should pass for a temp dir outside any repo, got: %v", err)
	}
}

func TestValidateWorktreeDir_InsideRepoMissingGitFile(t *testing.T) {
	// A subdirectory inside a git repo (but without its own .git file) must be
	// rejected — git commands would resolve to the parent repo's checkout.
	repoDir := t.TempDir()
	initTestRepo(t, repoDir, "main")

	subDir := filepath.Join(repoDir, "subdir")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorktreeDir(subDir); err == nil {
		t.Error("ValidateWorktreeDir should return error for a subdir inside a repo without a worktree .git file")
	}
}

func TestVerifyWorktreeGitFile_ValidWorktree(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	mgr := NewManager()
	wt, err := mgr.CreateWithOptions(context.Background(), anvilDir, "verify-test", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}

	if err := ValidateWorktreeDir(wt.Path); err != nil {
		t.Errorf("ValidateWorktreeDir should succeed for a real worktree, got: %v", err)
	}
}

func TestCreateWithOptions_StaleDirectoryRemoved(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	// Create a stale .workers/stale-test/ directory without a .git file.
	staleDir := filepath.Join(anvilDir, ".workers", "stale-test")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Add some junk files to simulate the bug scenario.
	if err := os.MkdirAll(filepath.Join(staleDir, ".forge-logs"), 0o755); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager()
	wt, err := mgr.CreateWithOptions(context.Background(), anvilDir, "stale-test", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateWithOptions should succeed after removing stale dir: %v", err)
	}

	// Verify the new worktree is valid.
	if err := ValidateWorktreeDir(wt.Path); err != nil {
		t.Errorf("new worktree should be valid after stale directory removal: %v", err)
	}
}

func TestUnlinkReparsePoints(t *testing.T) {
	root := t.TempDir()

	// Create regular files and directories.
	regularDir := filepath.Join(root, "src")
	if err := os.MkdirAll(regularDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(regularDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a target directory that should NOT be deleted.
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "keep.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a symlink inside root pointing to target (proxy for a junction).
	symlinkPath := filepath.Join(root, "node_modules")
	if err := os.Symlink(target, symlinkPath); err != nil {
		t.Skipf("cannot create symlink (permissions?): %v", err)
	}

	// Verify symlink exists.
	fi, err := os.Lstat(symlinkPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("expected symlink")
	}

	// Run unlinkReparsePoints.
	if err := unlinkReparsePoints(root); err != nil {
		t.Fatalf("unlinkReparsePoints: %v", err)
	}

	// Symlink should be removed.
	if _, err := os.Lstat(symlinkPath); !os.IsNotExist(err) {
		t.Error("symlink should have been removed")
	}

	// Regular files should still exist.
	if _, err := os.Stat(filepath.Join(regularDir, "main.go")); err != nil {
		t.Errorf("regular file should still exist: %v", err)
	}

	// Target directory should be untouched.
	if _, err := os.Stat(filepath.Join(target, "keep.txt")); err != nil {
		t.Errorf("target directory should be untouched: %v", err)
	}

	// os.RemoveAll should now succeed on root.
	if err := os.RemoveAll(root); err != nil {
		t.Errorf("os.RemoveAll should succeed after unlinking: %v", err)
	}
}

// gitOutput runs a git command and returns trimmed stdout.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
