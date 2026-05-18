package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// initRemoteAndClone creates a bare origin with a single commit on main and a
// local clone configured with a user identity. Returns the clone (anvil) path.
func initRemoteAndClone(t *testing.T) string {
	t.Helper()
	remote := t.TempDir()
	runGit(t, remote, "init", "--bare", "--initial-branch=main")

	clone := t.TempDir()
	runGit(t, clone, "clone", remote, ".")
	runGit(t, clone, "config", "user.email", "test@example.com")
	runGit(t, clone, "config", "user.name", "Test")

	if err := os.WriteFile(filepath.Join(clone, "README"), []byte("init\n"), 0o644); err != nil {
		t.Fatalf("seed README: %v", err)
	}
	runGit(t, clone, "add", "README")
	runGit(t, clone, "commit", "-m", "initial")
	runGit(t, clone, "push", "origin", "main")
	return clone
}

func TestCheckRemoteBranchState_Absent(t *testing.T) {
	anvil := initRemoteAndClone(t)
	branch := BranchName("Forge-absent")

	state, info, err := CheckRemoteBranchState(context.Background(), anvil, branch, "")
	if err != nil {
		t.Fatalf("CheckRemoteBranchState: %v", err)
	}
	if state != RemoteBranchAbsent {
		t.Fatalf("state = %s; want absent", state)
	}
	if info.SHA != "" {
		t.Errorf("SHA = %q; want empty for absent branch", info.SHA)
	}
}

func TestCheckRemoteBranchState_Stranded(t *testing.T) {
	anvil := initRemoteAndClone(t)
	branch := BranchName("Forge-stranded")

	// Create a forge branch with a commit not reachable from main and push it.
	runGit(t, anvil, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(anvil, "work.txt"), []byte("smith\n"), 0o644); err != nil {
		t.Fatalf("write work.txt: %v", err)
	}
	runGit(t, anvil, "add", "work.txt")
	runGit(t, anvil, "commit", "-m", "smith work")
	runGit(t, anvil, "push", "origin", branch)
	runGit(t, anvil, "checkout", "main")

	state, info, err := CheckRemoteBranchState(context.Background(), anvil, branch, "")
	if err != nil {
		t.Fatalf("CheckRemoteBranchState: %v", err)
	}
	if state != RemoteBranchStranded {
		t.Fatalf("state = %s; want stranded", state)
	}
	if info.SHA == "" {
		t.Error("SHA should be populated for stranded branch")
	}
	if info.BaseRef == "" {
		t.Error("BaseRef should be populated for stranded branch")
	}
}

func TestCheckRemoteBranchState_Merged(t *testing.T) {
	anvil := initRemoteAndClone(t)
	branch := BranchName("Forge-merged")

	// Create a branch at main's current tip and push it. The tip commit is
	// reachable from main (it IS the tip), so the branch is classified as merged.
	runGit(t, anvil, "checkout", "-b", branch)
	runGit(t, anvil, "push", "origin", branch)
	runGit(t, anvil, "checkout", "main")

	state, _, err := CheckRemoteBranchState(context.Background(), anvil, branch, "")
	if err != nil {
		t.Fatalf("CheckRemoteBranchState: %v", err)
	}
	if state != RemoteBranchMerged {
		t.Fatalf("state = %s; want merged", state)
	}
}

// TestCheckRemoteBranchState_MergedViaBaseBranch exercises the baseBranch
// override: a crucible child whose commits have been merged into the epic
// branch (but NOT into main) should be classified as merged when baseBranch
// is set to the epic branch, and as stranded when baseBranch is "".
func TestCheckRemoteBranchState_MergedViaBaseBranch(t *testing.T) {
	anvil := initRemoteAndClone(t)

	epicBranch := "feature/epic-test"
	forgeBranch := BranchName("Forge-child-epic")

	// Build epic branch with one extra commit, then create a forge child off it.
	runGit(t, anvil, "checkout", "-b", epicBranch)
	if err := os.WriteFile(filepath.Join(anvil, "epic.txt"), []byte("epic\n"), 0o644); err != nil {
		t.Fatalf("write epic.txt: %v", err)
	}
	runGit(t, anvil, "add", "epic.txt")
	runGit(t, anvil, "commit", "-m", "epic work")
	runGit(t, anvil, "push", "origin", epicBranch)

	runGit(t, anvil, "checkout", "-b", forgeBranch)
	if err := os.WriteFile(filepath.Join(anvil, "child.txt"), []byte("child\n"), 0o644); err != nil {
		t.Fatalf("write child.txt: %v", err)
	}
	runGit(t, anvil, "add", "child.txt")
	runGit(t, anvil, "commit", "-m", "child work")
	runGit(t, anvil, "push", "origin", forgeBranch)

	// Merge the forge branch into the epic branch (fast-forward: epic tip =
	// forge tip, so the forge commit is reachable from origin/epicBranch).
	runGit(t, anvil, "checkout", epicBranch)
	runGit(t, anvil, "merge", forgeBranch)
	runGit(t, anvil, "push", "origin", epicBranch)
	runGit(t, anvil, "checkout", "main")

	// With baseBranch=epicBranch: the forge tip is reachable from the epic
	// branch → classified as merged.
	got, _, err := CheckRemoteBranchState(context.Background(), anvil, forgeBranch, epicBranch)
	if err != nil {
		t.Fatalf("CheckRemoteBranchState(baseBranch=%s): %v", epicBranch, err)
	}
	if got != RemoteBranchMerged {
		t.Fatalf("state = %s; want merged (reachable from epic branch)", got)
	}

	// With baseBranch="": the forge commits are not on main → stranded.
	got, _, err = CheckRemoteBranchState(context.Background(), anvil, forgeBranch, "")
	if err != nil {
		t.Fatalf("CheckRemoteBranchState(baseBranch=''): %v", err)
	}
	if got != RemoteBranchStranded {
		t.Fatalf("state = %s; want stranded (not reachable from main)", got)
	}
}

func TestDeleteRemoteBranch(t *testing.T) {
	anvil := initRemoteAndClone(t)
	branch := BranchName("Forge-delete")

	runGit(t, anvil, "checkout", "-b", branch)
	runGit(t, anvil, "push", "origin", branch)
	runGit(t, anvil, "checkout", "main")

	if err := DeleteRemoteBranch(context.Background(), anvil, branch); err != nil {
		t.Fatalf("DeleteRemoteBranch: %v", err)
	}

	sha, err := lsRemoteBranchSHA(context.Background(), anvil, branch)
	if err != nil {
		t.Fatalf("lsRemoteBranchSHA: %v", err)
	}
	if sha != "" {
		t.Errorf("branch still present on origin after delete; sha=%s", sha)
	}

	// Deleting again is idempotent: a second delete on a missing ref must
	// not return an error.
	if err := DeleteRemoteBranch(context.Background(), anvil, branch); err != nil {
		t.Errorf("second DeleteRemoteBranch should be idempotent: %v", err)
	}
}
