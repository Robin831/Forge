package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// runGitAllowFail runs a git command in dir and ignores a non-zero exit. Used
// for teardown steps whose target may already be gone.
func runGitAllowFail(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = envWithoutGitVars()
	_ = cmd.Run()
}

func TestSanitizePreviewID(t *testing.T) {
	tests := []struct {
		name    string
		beadID  string
		want    string
		wantErr bool
	}{
		{name: "plain bead id", beadID: "Forge-abc1", want: "Forge-abc1"},
		{name: "dotted bead id", beadID: "Forge-n1g.4.1", want: "Forge-n1g.4.1"},
		{name: "forward slashes", beadID: "Forge/abc1", want: "Forge-abc1"},
		{name: "backslashes", beadID: `Forge\abc1`, want: "Forge-abc1"},
		{name: "spaces and colons", beadID: "Forge abc:1", want: "Forge-abc-1"},
		{name: "traversal folded to segment", beadID: "../../etc", want: "------etc"},
		{name: "empty", beadID: "", wantErr: true},
		{name: "whitespace only", beadID: "   ", wantErr: true},
		{name: "dot", beadID: ".", wantErr: true},
		{name: "dotdot", beadID: "..", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sanitizePreviewID(tt.beadID)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("sanitizePreviewID(%q) = %q; want error", tt.beadID, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("sanitizePreviewID(%q): unexpected error: %v", tt.beadID, err)
			}
			if got != tt.want {
				t.Errorf("sanitizePreviewID(%q) = %q; want %q", tt.beadID, got, tt.want)
			}
		})
	}
}

func TestPreviewPath(t *testing.T) {
	got, err := PreviewPath(filepath.Join("anvil", "root"), "Forge/abc1")
	if err != nil {
		t.Fatalf("PreviewPath: %v", err)
	}
	want := filepath.Join("anvil", "root", ".previews", "Forge-abc1")
	if got != want {
		t.Errorf("PreviewPath = %q; want %q", got, want)
	}

	// A bead ID that cannot be reduced to a safe segment must not silently
	// produce a path pointing at the anvil root (or above it).
	if _, err := PreviewPath("anvil", ".."); err == nil {
		t.Error("PreviewPath should reject a traversal bead ID")
	}
}

// TestCreateDetached_BranchCheckedOutInWorkerWorktree is the case previews
// exist for: the worker worktree under .workers/ is still alive on the branch.
// A plain branch checkout would fail with "already checked out"; the detached
// checkout must succeed.
func TestCreateDetached_BranchCheckedOutInWorkerWorktree(t *testing.T) {
	_, anvil := initBareRemoteCloneWithInitialCommit(t)

	mgr := NewManager()
	ctx := context.Background()

	wt, err := mgr.CreateWithOptions(ctx, anvil, "preview-bead", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "work.txt"), []byte("smith\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, wt.Path, "add", "work.txt")
	runGit(t, wt.Path, "commit", "-m", "smith work")
	runGit(t, wt.Path, "push", "origin", wt.Branch)
	tip := gitOutput(t, wt.Path, "rev-parse", "HEAD")

	preview, err := CreateDetached(ctx, anvil, "preview-bead", wt.Branch)
	if err != nil {
		t.Fatalf("CreateDetached while worker worktree holds the branch: %v", err)
	}

	wantPath := filepath.Join(anvil, ".previews", "preview-bead")
	if preview.Path != wantPath {
		t.Errorf("preview path = %q; want %q", preview.Path, wantPath)
	}
	if preview.Branch != wt.Branch {
		t.Errorf("preview branch = %q; want %q", preview.Branch, wt.Branch)
	}
	if !isValidWorktree(ctx, preview.Path) {
		t.Fatal("preview directory should be a valid git worktree")
	}
	if head := gitOutput(t, preview.Path, "rev-parse", "HEAD"); head != tip {
		t.Errorf("preview HEAD = %s; want branch tip %s", head, tip)
	}
	// Detached HEAD is what let the checkout coexist with the worker worktree.
	if branch, err := CurrentBranch(ctx, preview.Path); err != nil {
		t.Fatalf("CurrentBranch in preview: %v", err)
	} else if branch != "HEAD" {
		t.Errorf("preview HEAD should be detached; CurrentBranch = %q", branch)
	}
	if _, err := os.Stat(filepath.Join(preview.Path, "work.txt")); err != nil {
		t.Errorf("preview should contain the branch's committed work: %v", err)
	}

	// The worker worktree must be untouched by the preview.
	if !isValidWorktree(ctx, wt.Path) {
		t.Error("worker worktree should remain valid after creating a preview")
	}
	if branch, err := CurrentBranch(ctx, wt.Path); err != nil {
		t.Fatalf("CurrentBranch in worker worktree: %v", err)
	} else if branch != wt.Branch {
		t.Errorf("worker worktree branch = %q; want %q", branch, wt.Branch)
	}
}

// TestCreateDetached_BranchOnlyOnOrigin covers a preview for a bead whose
// worker worktree and local branch are long gone — only origin/<branch>
// survives, and only after the helper's own fetch.
func TestCreateDetached_BranchOnlyOnOrigin(t *testing.T) {
	_, anvil := initBareRemoteCloneWithInitialCommit(t)

	mgr := NewManager()
	ctx := context.Background()

	wt, err := mgr.CreateWithOptions(ctx, anvil, "remote-preview", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	branch := wt.Branch
	if err := os.WriteFile(filepath.Join(wt.Path, "remote.txt"), []byte("pushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, wt.Path, "add", "remote.txt")
	runGit(t, wt.Path, "commit", "-m", "pushed work")
	runGit(t, wt.Path, "push", "origin", branch)
	tip := gitOutput(t, wt.Path, "rev-parse", "HEAD")

	if err := mgr.Remove(ctx, anvil, wt); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Drop both the local branch and the remote-tracking ref so the only way
	// to resolve the tip is the fetch CreateDetached performs itself. Remove
	// already deletes the local branch best-effort, so both deletions are
	// tolerant of an already-absent ref.
	runGitAllowFail(t, anvil, "branch", "-D", branch)
	runGitAllowFail(t, anvil, "update-ref", "-d", "refs/remotes/origin/"+branch)

	preview, err := CreateDetached(ctx, anvil, "remote-preview", branch)
	if err != nil {
		t.Fatalf("CreateDetached (branch only on origin): %v", err)
	}
	if head := gitOutput(t, preview.Path, "rev-parse", "HEAD"); head != tip {
		t.Errorf("preview HEAD = %s; want origin tip %s", head, tip)
	}
	if _, err := os.Stat(filepath.Join(preview.Path, "remote.txt")); err != nil {
		t.Errorf("preview should contain the pushed work: %v", err)
	}
	// The detached checkout must not have recreated the local branch — a
	// preview must never own a ref a worker could later push.
	if _, err := revParse(ctx, anvil, "refs/heads/"+branch); err == nil {
		t.Error("CreateDetached should not create a local branch")
	}
}

// TestCreateDetached_IdempotentRecreate verifies a second create over an
// existing .previews/<id> directory succeeds and lands at the current tip
// rather than failing on the already-registered worktree or reusing stale
// content.
func TestCreateDetached_IdempotentRecreate(t *testing.T) {
	_, anvil := initBareRemoteCloneWithInitialCommit(t)

	mgr := NewManager()
	ctx := context.Background()

	wt, err := mgr.CreateWithOptions(ctx, anvil, "idem-preview", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	runGit(t, wt.Path, "commit", "--allow-empty", "-m", "first")
	runGit(t, wt.Path, "push", "origin", wt.Branch)
	firstTip := gitOutput(t, wt.Path, "rev-parse", "HEAD")

	first, err := CreateDetached(ctx, anvil, "idem-preview", wt.Branch)
	if err != nil {
		t.Fatalf("CreateDetached (first): %v", err)
	}
	if head := gitOutput(t, first.Path, "rev-parse", "HEAD"); head != firstTip {
		t.Fatalf("first preview HEAD = %s; want %s", head, firstTip)
	}

	// Leave junk behind, then advance the branch and re-create.
	junk := filepath.Join(first.Path, "junk.txt")
	if err := os.WriteFile(junk, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, wt.Path, "commit", "--allow-empty", "-m", "second")
	runGit(t, wt.Path, "push", "origin", wt.Branch)
	secondTip := gitOutput(t, wt.Path, "rev-parse", "HEAD")
	if secondTip == firstTip {
		t.Fatal("test setup: branch tip did not advance")
	}

	second, err := CreateDetached(ctx, anvil, "idem-preview", wt.Branch)
	if err != nil {
		t.Fatalf("CreateDetached (recreate over existing dir): %v", err)
	}
	if second.Path != first.Path {
		t.Errorf("recreated preview path = %q; want %q", second.Path, first.Path)
	}
	if !isValidWorktree(ctx, second.Path) {
		t.Fatal("recreated preview should be a valid git worktree")
	}
	if head := gitOutput(t, second.Path, "rev-parse", "HEAD"); head != secondTip {
		t.Errorf("recreated preview HEAD = %s; want new tip %s", head, secondTip)
	}
	if _, err := os.Stat(junk); !os.IsNotExist(err) {
		t.Errorf("stale file from the previous preview should be gone, stat err = %v", err)
	}
}

// TestRemoveDetached_ToleratesMissingDirectory verifies teardown is safe to
// call for a preview that was never created, and again after a successful
// removal — the daemon's reconciliation and idle reaper both do exactly this.
func TestRemoveDetached_ToleratesMissingDirectory(t *testing.T) {
	_, anvil := initBareRemoteCloneWithInitialCommit(t)
	ctx := context.Background()

	if err := RemoveDetached(ctx, anvil, "never-created"); err != nil {
		t.Errorf("RemoveDetached on a never-created preview: %v", err)
	}

	mgr := NewManager()
	wt, err := mgr.CreateWithOptions(ctx, anvil, "gone-preview", CreateOptions{})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	runGit(t, wt.Path, "commit", "--allow-empty", "-m", "work")
	runGit(t, wt.Path, "push", "origin", wt.Branch)

	preview, err := CreateDetached(ctx, anvil, "gone-preview", wt.Branch)
	if err != nil {
		t.Fatalf("CreateDetached: %v", err)
	}
	if err := RemoveDetached(ctx, anvil, "gone-preview"); err != nil {
		t.Fatalf("RemoveDetached: %v", err)
	}
	if _, err := os.Stat(preview.Path); !os.IsNotExist(err) {
		t.Errorf("preview directory should be gone, stat err = %v", err)
	}
	// Repeat removal must stay a no-op.
	if err := RemoveDetached(ctx, anvil, "gone-preview"); err != nil {
		t.Errorf("repeat RemoveDetached: %v", err)
	}
}

// TestCreateDetached_MissingBranch verifies a branch that exists nowhere yields
// a clear error naming the branch instead of a preview of some other ref.
func TestCreateDetached_MissingBranch(t *testing.T) {
	_, anvil := initBareRemoteCloneWithInitialCommit(t)

	_, err := CreateDetached(context.Background(), anvil, "no-branch-bead", "forge/no-branch-bead")
	if err == nil {
		t.Fatal("CreateDetached should fail when the branch does not exist")
	}
	if _, statErr := os.Stat(filepath.Join(anvil, ".previews", "no-branch-bead")); !os.IsNotExist(statErr) {
		t.Errorf("no preview directory should be left behind, stat err = %v", statErr)
	}
}

// TestCreateDetached_RejectsInvalidBranchName guards against argument
// injection: branch names derive from bead IDs / labels, so a flag-like name
// must be rejected before it reaches git.
func TestCreateDetached_RejectsInvalidBranchName(t *testing.T) {
	_, anvil := initBareRemoteCloneWithInitialCommit(t)

	_, err := CreateDetached(context.Background(), anvil, "bad-branch-bead", "--upload-pack=evil")
	if err == nil {
		t.Fatal("CreateDetached should reject a flag-like branch name")
	}
	if _, statErr := os.Stat(filepath.Join(anvil, ".previews", "bad-branch-bead")); !os.IsNotExist(statErr) {
		t.Errorf("no preview directory should exist after rejection, stat err = %v", statErr)
	}
}
