package worktree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/executil"
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

func TestWorktreePath(t *testing.T) {
	m := NewManager()
	anvil := filepath.Join("home", "user", "repo")

	// A clean bead ID lands under <anvil>/.workers/<id>.
	got := m.WorktreePath(anvil, "Forge-abc1")
	want := filepath.Join(anvil, ".workers", "Forge-abc1")
	if got != want {
		t.Errorf("WorktreePath(%q, %q) = %q; want %q", anvil, "Forge-abc1", got, want)
	}

	// The path must match what CreateWithOptions derives (sanitised), so a
	// resume recreation lands exactly where the original worktree lived.
	got = m.WorktreePath(anvil, "feat/fix bug")
	want = filepath.Join(anvil, ".workers", sanitizePath("feat/fix bug"))
	if got != want {
		t.Errorf("WorktreePath sanitisation = %q; want %q", got, want)
	}

	// A custom WorkersDir is honoured.
	m2 := &Manager{WorkersDir: "wt"}
	got = m2.WorktreePath(anvil, "Forge-abc1")
	want = filepath.Join(anvil, "wt", "Forge-abc1")
	if got != want {
		t.Errorf("WorktreePath with custom WorkersDir = %q; want %q", got, want)
	}
}

func TestBeadIDFromBranch(t *testing.T) {
	tests := []struct {
		branch string
		wantID string
		wantOK bool
	}{
		{"forge/Forge-abc1", "Forge-abc1", true},
		{"forge/Forge-n1g.4.1", "Forge-n1g.4.1", true},
		{"forge/Fhi.Metadata-g1a58", "Fhi.Metadata-g1a58", true},
		{"forge/foo/bar", "", false},
		{"forge/nested/path/id", "", false},
		{"feature/random", "", false},
		{"sophie/manual-fix", "", false},
		{"main", "", false},
		{"forge/", "", false},
		{"", "", false},
	}

	for _, tt := range tests {
		gotID, gotOK := BeadIDFromBranch(tt.branch)
		if gotID != tt.wantID || gotOK != tt.wantOK {
			t.Errorf("BeadIDFromBranch(%q) = (%q, %v); want (%q, %v)",
				tt.branch, gotID, gotOK, tt.wantID, tt.wantOK)
		}
	}
}

// BeadIDFromBranch must round-trip the canonical BranchName output for any
// bead ID free of path-fold characters (the case for all bd-issued IDs).
func TestBeadIDFromBranch_RoundTripsBranchName(t *testing.T) {
	for _, id := range []string{"Forge-abc1", "Forge-n1g.4.1", "Fhi.Metadata-g1a58"} {
		got, ok := BeadIDFromBranch(BranchName(id))
		if !ok || got != id {
			t.Errorf("BeadIDFromBranch(BranchName(%q)) = (%q, %v); want (%q, true)", id, got, ok, id)
		}
	}
}

// testGitCmd builds a git command that runs in dir with the ambient git
// repo-location overrides stripped. Every git invocation in this package's
// tests must be built here: `go test` inherits the environment of whatever
// started it, and Forge's own Smith workers export GIT_DIR and GIT_WORK_TREE
// pointing at the worker's worktree. Those variables outrank both cmd.Dir and
// `git -C`, so a command built with the raw environment silently operates on
// the surrounding repository instead of the test fixture — which is how the
// whole suite failed when run from inside a worker worktree.
func testGitCmd(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = envWithoutGitVars()
	return cmd
}

// runGit runs a git command in dir and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := testGitCmd(t, dir, args...)
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
	cmd := testGitCmd(t, dir, "checkout", "-b", "forge/test-bead")
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
	cmd := testGitCmd(t, dir, "checkout", "-b", "forge/Forge-x1bs")
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
	cmd := testGitCmd(t, dir, "checkout", "-b", "forge/Forge-x1bs")
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

	// Create a directory link inside root pointing to target.
	// On Windows this should use a junction/reparse point; on other platforms, a symlink.
	symlinkPath := filepath.Join(root, "node_modules")
	if err := createDirLink(target, symlinkPath); err != nil {
		t.Skipf("cannot create directory link: %v", err)
	}

	// Verify the directory link entry exists.
	if _, err := os.Lstat(symlinkPath); err != nil {
		t.Fatal(err)
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

// TestCreateWithOptions_SkipNodeModulesJunction_FreshCreate verifies that when
// SkipNodeModulesJunction is true, node_modules is NOT linked into the worktree
// on a fresh creation even when the anvil has a node_modules directory.
func TestCreateWithOptions_SkipNodeModulesJunction_FreshCreate(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	// Place a node_modules directory in the anvil to simulate a real repo.
	anvilNM := filepath.Join(anvilDir, "node_modules")
	if err := os.Mkdir(anvilNM, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(anvilNM, "marker.txt"), []byte("anvil"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager()
	wt, err := mgr.CreateWithOptions(context.Background(), anvilDir, "skip-nm-fresh", CreateOptions{
		SkipNodeModulesJunction: true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}

	// node_modules must NOT be linked (or created) in the worktree.
	if _, err := os.Lstat(filepath.Join(wt.Path, "node_modules")); !os.IsNotExist(err) {
		t.Error("node_modules should not exist in worktree when SkipNodeModulesJunction=true (fresh create)")
	}
}

// TestCreateWithOptions_SkipNodeModulesJunction_ReuseClean verifies that when
// SkipNodeModulesJunction is true, node_modules is NOT re-linked on the
// reuse/clean path (when the worktree already exists and is valid).
func TestCreateWithOptions_SkipNodeModulesJunction_ReuseClean(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	// Place a node_modules directory in the anvil.
	anvilNM := filepath.Join(anvilDir, "node_modules")
	if err := os.Mkdir(anvilNM, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(anvilNM, "marker.txt"), []byte("anvil"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager()
	ctx := context.Background()

	// First call without SkipNodeModulesJunction — link should be created.
	wt, err := mgr.CreateWithOptions(ctx, anvilDir, "skip-nm-reuse", CreateOptions{})
	if err != nil {
		t.Fatalf("first CreateWithOptions: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(wt.Path, "node_modules")); err != nil {
		t.Skipf("node_modules link was not created (platform may not support it): %v", err)
	}

	// Second call with SkipNodeModulesJunction=true on the reuse path.
	// unlinkReparsePoints removes the symlink; it must not be re-linked.
	wt2, err := mgr.CreateWithOptions(ctx, anvilDir, "skip-nm-reuse", CreateOptions{
		SkipNodeModulesJunction: true,
	})
	if err != nil {
		t.Fatalf("second CreateWithOptions: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(wt2.Path, "node_modules")); !os.IsNotExist(err) {
		t.Error("node_modules should not exist in worktree when SkipNodeModulesJunction=true (reuse/clean path)")
	}
}

// TestRemove_PreservesSymlinkTarget verifies that Manager.Remove unlinks
// junctions/symlinks before removal so that the target content (e.g. the main
// checkout's node_modules) is not destroyed.
func TestRemove_PreservesSymlinkTarget(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	// Create a target directory simulating the main checkout's node_modules.
	targetDir := filepath.Join(anvilDir, "node_modules")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	markerFile := filepath.Join(targetDir, "eslint")
	if err := os.WriteFile(markerFile, []byte("package\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager()
	ctx := context.Background()
	wt, err := mgr.CreateWithOptions(ctx, anvilDir, "symlink-test", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}

	// Create a symlink inside the worktree pointing to the main node_modules.
	wtNodeModules := filepath.Join(wt.Path, "node_modules")
	// Remove any existing link from CreateWithOptions so we can create our own.
	_ = os.Remove(wtNodeModules)
	if err := createDirLink(targetDir, wtNodeModules); err != nil {
		t.Skipf("cannot create directory link: %v", err)
	}

	// Remove the worktree.
	if err := mgr.Remove(ctx, anvilDir, wt); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// The target directory's contents must be preserved.
	if _, err := os.Stat(markerFile); err != nil {
		t.Errorf("Remove destroyed symlink target content — main checkout's node_modules/eslint is missing: %v", err)
	}
}

// TestGitEnv_NonWorktreeReturnsNil ensures we don't inject GIT_DIR/GIT_WORK_TREE
// into a child process whose cwd is not a real worktree (e.g. an os.MkdirTemp
// dir used by schematic/wicket/warden-learn). Doing so would shape the env
// for a non-repo run, which we want to leave untouched.
func TestGitEnv_NonWorktreeReturnsNil(t *testing.T) {
	if env := GitEnv(t.TempDir()); env != nil {
		t.Errorf("GitEnv on non-worktree dir: expected nil, got %v", env)
	}
}

// TestGitEnv_RealWorktreeSetsAllVars verifies that GitEnv on a real worktree
// returns GIT_DIR, GIT_WORK_TREE, and GIT_CEILING_DIRECTORIES with values that
// confine a child git invocation to the worktree.
func TestGitEnv_RealWorktreeSetsAllVars(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	mgr := NewManager()
	wt, err := mgr.CreateWithOptions(context.Background(), anvilDir, "git-env-test", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}

	env := GitEnv(wt.Path)
	if len(env) == 0 {
		t.Fatal("GitEnv on real worktree returned empty slice")
	}

	got := map[string]string{}
	for _, e := range env {
		i := strings.IndexByte(e, '=')
		if i < 0 {
			t.Fatalf("malformed env entry %q", e)
		}
		got[e[:i]] = e[i+1:]
	}

	// GIT_WORK_TREE must equal the absolute worktree path.
	absWt, err := filepath.Abs(wt.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got["GIT_WORK_TREE"] != absWt {
		t.Errorf("GIT_WORK_TREE = %q; want %q", got["GIT_WORK_TREE"], absWt)
	}

	// GIT_DIR must be a valid directory (the worktree's gitdir under
	// <anvil>/.git/worktrees/<name>) — never the worktree's own .git file.
	gitdir := got["GIT_DIR"]
	if gitdir == "" {
		t.Fatal("GIT_DIR not set")
	}
	info, err := os.Stat(gitdir)
	if err != nil {
		t.Errorf("GIT_DIR points to missing path %q: %v", gitdir, err)
	} else if !info.IsDir() {
		t.Errorf("GIT_DIR %q is not a directory", gitdir)
	}

	// GIT_CEILING_DIRECTORIES must point to the parent of the worktree root
	// (i.e. .workers/), so that ascending repo discovery cannot reach the
	// anvil's .git directory above it.
	if got["GIT_CEILING_DIRECTORIES"] != filepath.Dir(absWt) {
		t.Errorf("GIT_CEILING_DIRECTORIES = %q; want %q",
			got["GIT_CEILING_DIRECTORIES"], filepath.Dir(absWt))
	}
}

// TestGitEnv_ConfinesGitFromOutsideWorktree is the regression test for the
// Fhi.Metadata-k41dx incident: a Smith escaped its worktree via "cd .." and
// committed 5 stray files on the parent anvil's main branch. With GitEnv
// applied to a child process, running git from a directory outside the
// worktree must still target the worktree's gitdir/work-tree, not the
// parent repo.
func TestGitEnv_ConfinesGitFromOutsideWorktree(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	mgr := NewManager()
	ctx := context.Background()
	wt, err := mgr.CreateWithOptions(ctx, anvilDir, "escape-test", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}

	// Sanity: without our env vars, running git from the anvil resolves to
	// the anvil's checkout (the dangerous default we're protecting against).
	cmd := testGitCmd(t, anvilDir, "rev-parse", "--show-toplevel")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("baseline git rev-parse: %v\n%s", err, out)
	}
	baselineTop, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	resolvedAnvil, err := filepath.EvalSymlinks(anvilDir)
	if err != nil {
		t.Fatal(err)
	}
	if baselineTop != resolvedAnvil {
		t.Fatalf("baseline rev-parse: expected anvil %q, got %q", resolvedAnvil, baselineTop)
	}

	// With GitEnv, running git from the anvil dir (i.e. one level above the
	// worktree, equivalent to a "cd ../.." escape from a worktree subdir)
	// must resolve --show-toplevel back to the worktree.
	env := GitEnv(wt.Path)
	if env == nil {
		t.Fatal("GitEnv returned nil for valid worktree")
	}
	cmd = testGitCmd(t, anvilDir, "rev-parse", "--show-toplevel")
	cmd.Env = append(cmd.Env, env...)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("confined git rev-parse: %v\n%s", err, out)
	}
	confinedTop, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	resolvedWt, err := filepath.EvalSymlinks(wt.Path)
	if err != nil {
		t.Fatal(err)
	}
	if confinedTop != resolvedWt {
		t.Errorf("git --show-toplevel with GitEnv from anvil dir = %q; want worktree %q "+
			"(regression: Smith escape from worktree subdir would commit to anvil main)",
			confinedTop, resolvedWt)
	}

	// Running git rev-parse --abbrev-ref HEAD from the anvil dir with GitEnv
	// must report the worktree's branch, not the anvil's main. This is the
	// invariant that prevents the Fhi.Metadata-k41dx scenario where commits
	// landed on the parent repo's checked-out branch.
	cmd = testGitCmd(t, anvilDir, "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Env = append(cmd.Env, env...)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("confined git rev-parse --abbrev-ref HEAD: %v\n%s", err, out)
	}
	branch := strings.TrimSpace(string(out))
	if branch != wt.Branch {
		t.Errorf("git --abbrev-ref HEAD with GitEnv from anvil dir = %q; want worktree branch %q",
			branch, wt.Branch)
	}
}

// envWithoutGitVars returns os.Environ() with every git repo-location override
// removed, so a fixture command is answered by its own cmd.Dir and appending
// our own values stays deterministic even when the host shell has some of them
// set. It delegates to executil, which owns the single strip set the production
// git callers use — a local copy here would drift the day git grows another
// repo-location variable, and the tests would stop covering what ships.
func envWithoutGitVars() []string {
	return executil.CleanGitEnv()
}

// TestFixtureGitCommands_IgnoreAmbientGitEnvironment is the guard for the failure this
// suite kept hitting: run from inside a Forge worker worktree, `go test`
// inherits GIT_DIR/GIT_WORK_TREE (and friends) pointing at the real Forge
// repository, and every fixture command silently answered from there instead
// of from its temp dir. A command built by testGitCmd must report the fixture no
// matter what the surrounding process exported.
func TestFixtureGitCommands_IgnoreAmbientGitEnvironment(t *testing.T) {
	fixture := t.TempDir()
	initTestRepo(t, fixture, "main")

	// Stand in for the ambient repository a worker exports. Point every
	// repo-location variable at it, including the ones beyond the obvious
	// GIT_DIR/GIT_WORK_TREE pair, so a partial strip list fails here.
	ambient := t.TempDir()
	initTestRepo(t, ambient, "main")
	t.Setenv("GIT_DIR", filepath.Join(ambient, ".git"))
	t.Setenv("GIT_WORK_TREE", ambient)
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(fixture))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(ambient, ".git", "index"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(ambient, ".git"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(ambient, ".git", "objects"))

	out, err := testGitCmd(t, fixture, "rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v\n%s", err, out)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("testGitCmd resolved to %q; want the fixture %q "+
			"(the ambient git environment leaked into the command)", got, want)
	}

	// rev-parse only proves GIT_DIR/GIT_WORK_TREE are gone. The quieter members
	// of the family (GIT_INDEX_FILE retargets the staging area, GIT_COMMON_DIR
	// the ref store) corrupt a fixture without failing a command outright, so
	// assert the built environment against executil's strip set directly. This
	// is what catches a hand-rolled partial copy of that set drifting back in.
	for _, e := range testGitCmd(t, fixture, "status").Env {
		if key, _, _ := strings.Cut(e, "="); executil.IsGitRepoEnvVar(key) {
			t.Errorf("%s survived into a fixture git command's environment", key)
		}
	}
}

// TestCleanStaleCoreWorktree_UnsetsMissingPath verifies that
// CleanStaleCoreWorktree removes a core.worktree setting that points to a
// path which no longer exists on disk.
func TestCleanStaleCoreWorktree_UnsetsMissingPath(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "main")

	// Seed a stale core.worktree pointing at a path that doesn't exist.
	stale := filepath.Join(t.TempDir(), "removed-worker")
	runGit(t, dir, "config", "core.worktree", stale)

	if err := CleanStaleCoreWorktree(context.Background(), dir); err != nil {
		t.Fatalf("CleanStaleCoreWorktree: %v", err)
	}

	cmd := testGitCmd(t, dir, "config", "--local", "--get", "core.worktree")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("core.worktree should be unset, but got %q", strings.TrimSpace(string(out)))
	}
}

// TestCleanStaleCoreWorktree_UnsetsEmptyDir verifies that
// CleanStaleCoreWorktree removes a core.worktree setting that points to an
// existing-but-empty directory. An empty directory triggers the same
// `git status --porcelain` exit-128 failure as a missing path.
func TestCleanStaleCoreWorktree_UnsetsEmptyDir(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "main")

	emptyDir := t.TempDir() // exists but contains nothing
	runGit(t, dir, "config", "core.worktree", emptyDir)

	if err := CleanStaleCoreWorktree(context.Background(), dir); err != nil {
		t.Fatalf("CleanStaleCoreWorktree: %v", err)
	}

	cmd := testGitCmd(t, dir, "config", "--local", "--get", "core.worktree")
	if err := cmd.Run(); err == nil {
		t.Error("core.worktree should be unset when value points to an empty directory")
	}
}

// TestCleanStaleCoreWorktree_PreservesValidPath verifies that
// CleanStaleCoreWorktree leaves a core.worktree value alone when it points to
// a directory that exists and is non-empty. We treat such a value as
// potentially intentional and only self-heal the clearly-broken cases.
func TestCleanStaleCoreWorktree_PreservesValidPath(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "main")

	validDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(validDir, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "config", "core.worktree", validDir)

	if err := CleanStaleCoreWorktree(context.Background(), dir); err != nil {
		t.Fatalf("CleanStaleCoreWorktree: %v", err)
	}

	got := gitOutput(t, dir, "config", "--local", "--get", "core.worktree")
	if got != validDir {
		t.Errorf("core.worktree was modified: got %q, want %q", got, validDir)
	}
}

// TestCleanStaleCoreWorktree_NoOpWhenUnset verifies that
// CleanStaleCoreWorktree returns nil and makes no change when core.worktree
// is not set at all (the common case).
func TestCleanStaleCoreWorktree_NoOpWhenUnset(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "main")

	if err := CleanStaleCoreWorktree(context.Background(), dir); err != nil {
		t.Errorf("CleanStaleCoreWorktree on repo with no core.worktree: unexpected error: %v", err)
	}
}

// TestCleanStaleCoreWorktree_RelativePathMissing verifies that a relative
// core.worktree value that resolves (via gitdir) to a non-existent path is
// treated as stale and unset.
func TestCleanStaleCoreWorktree_RelativePathMissing(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "main")

	// Set core.worktree to a relative path. Git resolves relative values
	// relative to the gitdir (<dir>/.git), so "../nonexistent-worker" resolves
	// to <dir>/nonexistent-worker, which does not exist.
	runGit(t, dir, "config", "core.worktree", "../nonexistent-worker")

	if err := CleanStaleCoreWorktree(context.Background(), dir); err != nil {
		t.Fatalf("CleanStaleCoreWorktree: %v", err)
	}

	cmd := testGitCmd(t, dir, "config", "--local", "--get", "core.worktree")
	if err := cmd.Run(); err == nil {
		t.Error("core.worktree should have been unset for a relative path resolving to a missing directory")
	}
}

// TestCleanStaleCoreWorktree_RelativePathValid verifies that a relative
// core.worktree value that resolves to a non-empty directory is preserved.
func TestCleanStaleCoreWorktree_RelativePathValid(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "main")

	// Create <dir>/valid-wt with content. With gitdir = <dir>/.git, the
	// relative value "../valid-wt" resolves to <dir>/valid-wt.
	validWt := filepath.Join(dir, "valid-wt")
	if err := os.MkdirAll(validWt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(validWt, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "config", "core.worktree", "../valid-wt")

	if err := CleanStaleCoreWorktree(context.Background(), dir); err != nil {
		t.Fatalf("CleanStaleCoreWorktree: %v", err)
	}

	got := gitOutput(t, dir, "config", "--local", "--get", "core.worktree")
	if got != "../valid-wt" {
		t.Errorf("core.worktree was modified: got %q, want %q", got, "../valid-wt")
	}
}

// TestCreateWithOptions_DoesNotSetCoreWorktree verifies that creating a
// worktree does not write core.worktree to the main repo's .git/config.
// Per-worktree values belong in .git/worktrees/<name>/config.worktree, which
// `git worktree add` manages on its own.
func TestCreateWithOptions_DoesNotSetCoreWorktree(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	mgr := NewManager()
	if _, err := mgr.CreateWithOptions(context.Background(), anvilDir, "no-cw-test", CreateOptions{}); err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}

	cmd := testGitCmd(t, anvilDir, "config", "--local", "--get", "core.worktree")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("main repo should not have core.worktree set after worktree creation, got %q",
			strings.TrimSpace(string(out)))
	}
}

// TestCreateWithOptions_SelfHealsStaleCoreWorktree verifies that
// CreateWithOptions clears a pre-existing stale core.worktree before
// proceeding. This guards against state left by older Forge versions or
// manual git invocations.
func TestCreateWithOptions_SelfHealsStaleCoreWorktree(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	// Seed a stale core.worktree pointing at a non-existent path.
	stale := filepath.Join(t.TempDir(), "ghost-worker")
	runGit(t, anvilDir, "config", "core.worktree", stale)

	mgr := NewManager()
	if _, err := mgr.CreateWithOptions(context.Background(), anvilDir, "self-heal-test", CreateOptions{}); err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}

	cmd := testGitCmd(t, anvilDir, "config", "--local", "--get", "core.worktree")
	if err := cmd.Run(); err == nil {
		t.Error("CreateWithOptions should have unset stale core.worktree on the main repo")
	}
}

// TestRemove_UnsetsStaleCoreWorktree verifies that Manager.Remove clears any
// stale core.worktree setting on the main repo. This is the regression test
// for the originally reported bug: after a worker was cleaned up, a leftover
// core.worktree caused `git status --porcelain` against the main repo to
// fail with exit 128, breaking `go build` for any subsequent worker.
func TestRemove_UnsetsStaleCoreWorktree(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	mgr := NewManager()
	ctx := context.Background()
	wt, err := mgr.CreateWithOptions(ctx, anvilDir, "remove-cw-test", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}

	// Simulate the stale state described in the bug report: core.worktree
	// pointing at the worker that's about to be removed.
	runGit(t, anvilDir, "config", "core.worktree", wt.Path)

	if err := mgr.Remove(ctx, anvilDir, wt); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// After remove, core.worktree must be unset (the path it pointed to no
	// longer exists, so the self-heal triggers).
	cmd := testGitCmd(t, anvilDir, "config", "--local", "--get", "core.worktree")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("Remove should clear stale core.worktree, got %q", strings.TrimSpace(string(out)))
	}

	// Regression assertion from the bug report: `git status --porcelain`
	// against the main repo must succeed (exit 0) after a worker round-trip.
	cmd = testGitCmd(t, anvilDir, "status", "--porcelain")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("git status --porcelain on main repo failed after worker remove: %v\n%s", err, out)
	}
}

// TestCreateFromBranch_ReconstructsAtExactPath verifies that CreateFromBranch
// recreates a removed worktree at the identical path with the surviving branch
// and its committed work intact.
func TestCreateFromBranch_ReconstructsAtExactPath(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	mgr := NewManager()
	ctx := context.Background()

	// Original worker: create the worktree, commit and push some work.
	wt, err := mgr.CreateWithOptions(ctx, anvilDir, "resume-bead", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	origPath := wt.Path
	work := filepath.Join(wt.Path, "smith-work.txt")
	if err := os.WriteFile(work, []byte("progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, wt.Path, "add", "smith-work.txt")
	runGit(t, wt.Path, "commit", "-m", "smith work")
	runGit(t, wt.Path, "push", "origin", wt.Branch)
	workHash := gitOutput(t, wt.Path, "rev-parse", "HEAD")

	// Tear the worktree down as a restart/teardown would.
	if err := mgr.Remove(ctx, anvilDir, wt); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(origPath); !os.IsNotExist(err) {
		t.Fatalf("worktree dir should be gone after Remove, stat err = %v", err)
	}

	// Resume: recreate from the surviving branch at the exact original path.
	got, err := mgr.CreateFromBranch(ctx, anvilDir, "resume-bead", wt.Branch, origPath)
	if err != nil {
		t.Fatalf("CreateFromBranch: %v", err)
	}
	if got.Path != origPath {
		t.Errorf("CreateFromBranch path = %q; want original %q", got.Path, origPath)
	}
	if got.Branch != wt.Branch {
		t.Errorf("CreateFromBranch branch = %q; want %q", got.Branch, wt.Branch)
	}
	if h := gitOutput(t, got.Path, "rev-parse", "HEAD"); h != workHash {
		t.Errorf("resumed HEAD = %s; want %s (smith work preserved)", h, workHash)
	}
	if _, err := os.Stat(filepath.Join(got.Path, "smith-work.txt")); err != nil {
		t.Errorf("smith-work.txt should be present after resume: %v", err)
	}
	if !isValidWorktree(ctx, got.Path) {
		t.Error("resumed directory should be a valid worktree")
	}
}

// TestCreateFromBranch_RemoteOnly verifies resume works when only the remote
// branch survives (the local ref was pruned, e.g. after a daemon restart on a
// fresh checkout).
func TestCreateFromBranch_RemoteOnly(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	mgr := NewManager()
	ctx := context.Background()

	wt, err := mgr.CreateWithOptions(ctx, anvilDir, "remote-only-bead", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	origPath := wt.Path
	branch := wt.Branch
	if err := os.WriteFile(filepath.Join(wt.Path, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, wt.Path, "add", "f.txt")
	runGit(t, wt.Path, "commit", "-m", "work")
	runGit(t, wt.Path, "push", "origin", branch)
	workHash := gitOutput(t, wt.Path, "rev-parse", "HEAD")

	// Remove worktree, then delete the local branch so only origin/<branch>
	// remains.
	if err := mgr.Remove(ctx, anvilDir, wt); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Remove leaves the local branch (best-effort delete), so force-delete to
	// simulate the remote-only case regardless. testGitCmd's env scrub is not
	// optional here: a GIT_DIR/GIT_WORK_TREE in the ambient environment
	// outranks the target directory, and this would then delete the branch
	// from whatever repository those point at.
	_ = testGitCmd(t, anvilDir, "branch", "-D", branch).Run()

	got, err := mgr.CreateFromBranch(ctx, anvilDir, "remote-only-bead", branch, origPath)
	if err != nil {
		t.Fatalf("CreateFromBranch (remote-only): %v", err)
	}
	if h := gitOutput(t, got.Path, "rev-parse", "HEAD"); h != workHash {
		t.Errorf("resumed HEAD = %s; want %s", h, workHash)
	}
}

// TestCreateFromBranch_BranchDeleted verifies that a missing branch yields a
// clear error wrapping ErrBranchDeleted rather than silently basing off main.
func TestCreateFromBranch_BranchDeleted(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	mgr := NewManager()
	ctx := context.Background()
	targetPath := filepath.Join(anvilDir, ".workers", "gone-bead")

	_, err := mgr.CreateFromBranch(ctx, anvilDir, "gone-bead", "forge/gone-bead", targetPath)
	if err == nil {
		t.Fatal("CreateFromBranch should fail when branch does not exist")
	}
	if !errors.Is(err, ErrBranchDeleted) {
		t.Errorf("error should wrap ErrBranchDeleted; got %v", err)
	}
}

// TestCreateFromBranch_IdempotentRepeat verifies that a second CreateFromBranch
// call for a bead whose worktree already exists as a valid checkout reuses that
// directory as-is rather than tearing it down and recreating it. A repeated
// resume (e.g. the operator clicks twice, or the daemon retries) must not
// discard uncommitted state in the worktree.
func TestCreateFromBranch_IdempotentRepeat(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	mgr := NewManager()
	ctx := context.Background()

	wt, err := mgr.CreateWithOptions(ctx, anvilDir, "idem-bead", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	origPath := wt.Path
	branch := wt.Branch
	runGit(t, wt.Path, "commit", "--allow-empty", "-m", "smith work")
	runGit(t, wt.Path, "push", "origin", branch)

	if err := mgr.Remove(ctx, anvilDir, wt); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// First resume reconstructs the worktree from the branch.
	first, err := mgr.CreateFromBranch(ctx, anvilDir, "idem-bead", branch, origPath)
	if err != nil {
		t.Fatalf("CreateFromBranch (first): %v", err)
	}

	// Write an uncommitted scratch file: if the second call were to remove and
	// recreate the directory, this file would be lost. Reuse preserves it.
	scratch := filepath.Join(first.Path, "uncommitted.txt")
	if err := os.WriteFile(scratch, []byte("in-progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Second resume must reuse the existing valid worktree.
	second, err := mgr.CreateFromBranch(ctx, anvilDir, "idem-bead", branch, origPath)
	if err != nil {
		t.Fatalf("CreateFromBranch (repeat): %v", err)
	}
	if second.Path != origPath {
		t.Errorf("repeat resume path = %q; want %q", second.Path, origPath)
	}
	if second.Branch != branch {
		t.Errorf("repeat resume branch = %q; want %q", second.Branch, branch)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Errorf("uncommitted file should survive an idempotent repeat resume: %v", err)
	}
	if !isValidWorktree(ctx, second.Path) {
		t.Error("reused directory should remain a valid worktree")
	}
}

// TestCreateFromBranch_RejectsInvalidBranchName verifies that a flag-like branch
// name is rejected up front (before any git worktree mutation) rather than being
// passed to git where it could be parsed as an option. Branch names derive from
// bead IDs / labels, so this guard prevents argument injection.
func TestCreateFromBranch_RejectsInvalidBranchName(t *testing.T) {
	_, anvilDir := initBareRemoteCloneWithInitialCommit(t)

	mgr := NewManager()
	ctx := context.Background()
	targetPath := filepath.Join(anvilDir, ".workers", "bad-bead")

	_, err := mgr.CreateFromBranch(ctx, anvilDir, "bad-bead", "--upload-pack=evil", targetPath)
	if err == nil {
		t.Fatal("CreateFromBranch should reject a flag-like branch name")
	}
	// The rejection must not wrap ErrBranchDeleted — this is a validation
	// failure, not a "branch is gone, fall back to fresh" signal.
	if errors.Is(err, ErrBranchDeleted) {
		t.Errorf("invalid branch name should not be reported as a deleted branch: %v", err)
	}
	// No worktree directory should have been created for an invalid name.
	if _, statErr := os.Stat(targetPath); !os.IsNotExist(statErr) {
		t.Errorf("no worktree dir should exist after rejection, stat err = %v", statErr)
	}
}

// gitOutput runs a git command and returns trimmed stdout.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testGitCmd(t, dir, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestCreate_RecoversFromStaleRegistrationAfterKilledWorker covers the failure
// mode that silently abandoned FHIDev/Munin#4616 on 2026-08-04: a worker killed
// mid-run (pod roll, OOM, restart) leaves the worktree REGISTERED with git while
// its directory is gone. Every later Create for that branch then failed with
// "git worktree add ...: exit status 255", and because each lifecycle retry
// burns an attempt, the PR's fix budget was exhausted and it was dropped with no
// visible error on the PR itself.
//
// Note the inverse case (directory present but unusable) was already handled;
// this is specifically directory-absent-registration-present.
func TestCreate_RecoversFromStaleRegistrationAfterKilledWorker(t *testing.T) {
	_, anvil := initBareRemoteCloneWithInitialCommit(t)

	const branch = "forge/Fhi.Metadata-krzku"
	runGit(t, anvil, "checkout", "-b", branch)
	runGit(t, anvil, "push", "-u", "origin", branch)
	runGit(t, anvil, "checkout", "main")

	m := NewManager()
	ctx := context.Background()

	wt, err := m.Create(ctx, anvil, "pr-4616", branch)
	if err != nil {
		t.Fatalf("initial Create: %v", err)
	}

	// Simulate the kill: the directory vanishes, the registration survives.
	if err := os.RemoveAll(wt.Path); err != nil {
		t.Fatalf("removing worktree dir: %v", err)
	}
	// The registration IS the admin directory under .git/worktrees, so assert
	// it on disk rather than by parsing `git worktree list`: git versions
	// differ on whether listing prunes a registration whose directory is
	// already gone, which made this precondition fail on a test that was
	// otherwise fine. Reading the directory involves no git version at all.
	adminDir := filepath.Join(anvil, ".git", "worktrees", filepath.Base(wt.Path))
	if _, statErr := os.Stat(adminDir); statErr != nil {
		t.Fatalf("precondition failed: registration %s should still be present: %v", adminDir, statErr)
	}
	// testGitCmd's env scrub is not cosmetic here either: GIT_DIR/GIT_WORK_TREE in
	// the ambient environment (Forge's own workers run with both set) outrank
	// the target directory, so an unscrubbed list reports the surrounding
	// repository's worktrees.
	if out, err := testGitCmd(t, anvil, "worktree", "list").CombinedOutput(); err != nil {
		t.Fatalf("worktree list: %v\n%s", err, out)
	} else if !strings.Contains(string(out), filepath.Base(wt.Path)) {
		t.Logf("this git version already dropped the stale registration from `worktree list`; "+
			"the recovery below must still succeed:\n%s", out)
	}

	// Before the prune fix this returned "exit status 255" forever.
	if _, err := m.Create(ctx, anvil, "pr-4616", branch); err != nil {
		t.Fatalf("Create after killed worker should recover, got: %v", err)
	}
}

// TestGitTimeoutFor asserts the two timeout tiers: checkout-heavy git commands
// get the (configurable) checkout timeout, cheap metadata commands keep the
// tight default bound. Regression guard for Forge-7du1, where every git command
// shared a hardcoded 60s deadline and `git worktree add` on a large anvil was
// SIGKILLed at 60s on the first attempt of most beads.
func TestGitTimeoutFor(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want time.Duration
	}{
		{"worktree add", []string{"worktree", "add", "-f", "-b", "forge/x", "/tmp/x", "origin/main"}, DefaultGitCheckoutTimeout},
		{"worktree remove", []string{"worktree", "remove", "--force", "/tmp/x"}, DefaultGitCheckoutTimeout},
		{"worktree prune", []string{"worktree", "prune"}, DefaultGitTimeout},
		{"fetch", []string{"fetch", "origin"}, DefaultGitCheckoutTimeout},
		{"push", []string{"push", "-u", "origin", "--", "forge/x"}, DefaultGitCheckoutTimeout},
		{"checkout", []string{"checkout", "--force", "HEAD"}, DefaultGitCheckoutTimeout},
		{"reset", []string{"reset", "--hard", "origin/main"}, DefaultGitCheckoutTimeout},
		{"clean", []string{"clean", "-fd", "-e", "node_modules"}, DefaultGitCheckoutTimeout},
		{"rev-parse", []string{"rev-parse", "--verify", "origin/main"}, DefaultGitTimeout},
		{"show-ref", []string{"show-ref", "--verify", "--quiet", "refs/heads/main"}, DefaultGitTimeout},
		{"branch", []string{"branch", "-D", "forge/x"}, DefaultGitTimeout},
		{"check-ref-format", []string{"check-ref-format", "--branch", "forge/x"}, DefaultGitTimeout},
		{"no args", nil, DefaultGitTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gitTimeoutFor(tt.args); got != tt.want {
				t.Errorf("gitTimeoutFor(%v) = %s; want %s", tt.args, got, tt.want)
			}
		})
	}
}

// TestSetGitCheckoutTimeout covers the configured override and the fallback for
// zero/negative values (settings.worktree_git_timeout omitted or misconfigured).
func TestSetGitCheckoutTimeout(t *testing.T) {
	t.Cleanup(func() { SetGitCheckoutTimeout(0) })

	SetGitCheckoutTimeout(12 * time.Minute)
	if got := gitCheckoutTimeout(); got != 12*time.Minute {
		t.Errorf("gitCheckoutTimeout after override = %s; want 12m0s", got)
	}
	if got := gitTimeoutFor([]string{"worktree", "add"}); got != 12*time.Minute {
		t.Errorf("gitTimeoutFor(worktree add) after override = %s; want 12m0s", got)
	}
	// Cheap commands are unaffected by the checkout-tier override.
	if got := gitTimeoutFor([]string{"rev-parse", "HEAD"}); got != DefaultGitTimeout {
		t.Errorf("gitTimeoutFor(rev-parse) after override = %s; want %s", got, DefaultGitTimeout)
	}

	for _, d := range []time.Duration{0, -1 * time.Second} {
		SetGitCheckoutTimeout(d)
		if got := gitCheckoutTimeout(); got != DefaultGitCheckoutTimeout {
			t.Errorf("gitCheckoutTimeout after SetGitCheckoutTimeout(%s) = %s; want default %s",
				d, got, DefaultGitCheckoutTimeout)
		}
	}
}

// TestGitCmdOutTimeout_ClassifiesDeadline asserts that a git command killed by
// its deadline surfaces as a *GitTimeoutError naming the command and the time it
// got — not the bare "signal: killed" that exec.CommandContext produces and that
// made the original failures unreadable in daemon.log.
func TestGitCmdOutTimeout_ClassifiesDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake git shim is a POSIX shell script")
	}
	// Put a deliberately slow `git` on PATH so the deadline fires mid-flight and
	// the process is really SIGKILLed, exactly as it was in production.
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, "git")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("writing git shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out strings.Builder
	start := time.Now()
	err := gitCmdOutTimeout(context.Background(), shimDir, &out, 100*time.Millisecond,
		"worktree", "add", "-f", "-b", "forge/Forge-7du1", "/tmp/wt", "origin/main")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("command was not killed at its deadline (took %s)", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false; err = %v", err)
	}
	var timeoutErr *GitTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("errors.As(*GitTimeoutError) = false; err = %v", err)
	}
	if timeoutErr.Elapsed <= 0 {
		t.Errorf("GitTimeoutError.Elapsed = %s; want > 0", timeoutErr.Elapsed)
	}
	if timeoutErr.Limit != 100*time.Millisecond {
		t.Errorf("GitTimeoutError.Limit = %s; want 100ms", timeoutErr.Limit)
	}
	msg := err.Error()
	if !strings.Contains(msg, "worktree add") {
		t.Errorf("error message %q does not name the git subcommand", msg)
	}
	if !strings.Contains(msg, "timed out after") {
		t.Errorf("error message %q does not report the elapsed time", msg)
	}
	if strings.Contains(msg, "signal: killed") {
		t.Errorf("error message %q still surfaces the bare kill signal", msg)
	}

	// A wrapping caller (as in CreateWithOptions) keeps the classification.
	wrapped := fmt.Errorf("git worktree add (new): %w", err)
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Errorf("wrapped error lost its DeadlineExceeded classification: %v", wrapped)
	}
}

// TestGitCmdOutTimeout_CallerDeadlineWins asserts that when the caller's context
// expires before the tier timeout, the reported limit is the caller's remaining
// budget rather than the (larger) tier value.
func TestGitCmdOutTimeout_CallerDeadlineWins(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake git shim is a POSIX shell script")
	}
	shimDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("writing git shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := gitCmdOutTimeout(ctx, shimDir, io.Discard, 5*time.Minute, "fetch", "origin")
	var timeoutErr *GitTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("errors.As(*GitTimeoutError) = false; err = %v", err)
	}
	if timeoutErr.Limit > time.Second {
		t.Errorf("GitTimeoutError.Limit = %s; want the caller's ~100ms budget", timeoutErr.Limit)
	}
}

// TestGitCmdOutTimeout_CancelIsNotATimeout asserts that a caller cancelling the
// context (e.g. daemon shutdown) is not misreported as a deadline overrun.
func TestGitCmdOutTimeout_CancelIsNotATimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake git shim is a POSIX shell script")
	}
	shimDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("writing git shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	err := gitCmdOutTimeout(ctx, shimDir, io.Discard, 5*time.Minute, "fetch", "origin")
	if err == nil {
		t.Fatal("expected an error from the cancelled command, got nil")
	}
	var timeoutErr *GitTimeoutError
	if errors.As(err, &timeoutErr) {
		t.Errorf("cancellation was misclassified as a timeout: %v", err)
	}
}
