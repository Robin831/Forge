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

// initTestRepo creates a minimal git repository in dir with one commit on
// the given branch. It configures a local user identity to avoid relying on
// global git config (which may be absent in CI).
func initTestRepo(t *testing.T, dir, branch string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--initial-branch="+branch)
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	// Create an initial commit so HEAD is resolvable.
	readme := filepath.Join(dir, "README")
	if err := os.WriteFile(readme, []byte("test\n"), 0o644); err != nil {
		t.Fatalf("writing README: %v", err)
	}
	run("add", "README")
	run("commit", "-m", "init")
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
	// Set up a "remote" bare repo with one commit on main.
	remoteDir := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}
	run(remoteDir, "init", "--bare", "--initial-branch=main")

	// Clone the bare repo to serve as our "anvil".
	anvilDir := t.TempDir()
	run(anvilDir, "clone", remoteDir, ".")
	run(anvilDir, "config", "user.email", "test@example.com")
	run(anvilDir, "config", "user.name", "Test")
	readme := filepath.Join(anvilDir, "README")
	if err := os.WriteFile(readme, []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(anvilDir, "add", "README")
	run(anvilDir, "commit", "-m", "init")
	run(anvilDir, "push", "origin", "main")

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
	run(wt.Path, "add", "bad-change.txt")
	run(wt.Path, "commit", "-m", "bad commit from failed smith")
	run(wt.Path, "push", "origin", wt.Branch)

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
