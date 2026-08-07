package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newFixWorktree builds the shape a burnish run leaves behind: a bare remote,
// an anvil clone, and a worktree checked out on a branch that exists on the
// remote. It returns the manager, the anvil path and the worktree.
func newFixWorktree(t *testing.T, branch string) (*Manager, string, *Worktree) {
	t.Helper()
	_, anvil := initBareRemoteCloneWithInitialCommit(t)

	// Publish the fix branch so origin/<branch> resolves, exactly as it does
	// for a PR burnish is fixing.
	runGit(t, anvil, "branch", branch)
	runGit(t, anvil, "push", "origin", branch)

	m := NewManager()
	wt, err := m.CreateWithOptions(context.Background(), anvil, "test-bead", CreateOptions{
		Branch: branch,
		Quiet:  true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	return m, anvil, wt
}

func headSHA(t *testing.T, dir string) string {
	t.Helper()
	out, err := testGitCmd(t, dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD in %s: %v", dir, err)
	}
	return strings.TrimSpace(string(out))
}

// commitInWorktree makes a local commit that exists nowhere but this checkout —
// the burnish fix commit the old teardown threw away.
func commitInWorktree(t *testing.T, dir string) string {
	t.Helper()
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("addressed the review\n"), 0o644); err != nil {
		t.Fatalf("writing fix.txt: %v", err)
	}
	runGit(t, dir, "add", "fix.txt")
	runGit(t, dir, "commit", "-m", "fix: address review comments")
	return headSHA(t, dir)
}

// TestRemoveIfPushed_RefusesUnpushedHead is the data-loss guard: a worktree
// holding a commit that exists nowhere else must survive teardown, and the
// error must name what to recover and where.
func TestRemoveIfPushed_RefusesUnpushedHead(t *testing.T) {
	m, anvil, wt := newFixWorktree(t, "forge/test-bead")
	fixSHA := commitInWorktree(t, wt.Path)

	err := m.RemoveIfPushed(context.Background(), anvil, wt)

	var unpushed *UnpushedHeadError
	if !errors.As(err, &unpushed) {
		t.Fatalf("RemoveIfPushed = %v, want *UnpushedHeadError", err)
	}
	if unpushed.LocalHead != fixSHA {
		t.Errorf("LocalHead = %q, want %q", unpushed.LocalHead, fixSHA)
	}
	if unpushed.Path != wt.Path {
		t.Errorf("Path = %q, want %q", unpushed.Path, wt.Path)
	}
	if unpushed.Branch != "forge/test-bead" {
		t.Errorf("Branch = %q, want forge/test-bead", unpushed.Branch)
	}
	if _, statErr := os.Stat(wt.Path); statErr != nil {
		t.Fatalf("worktree was removed despite the unpushed commit: %v", statErr)
	}
	// The message has to be enough to recover by hand without reading code.
	if !strings.Contains(unpushed.Error(), wt.Path) {
		t.Errorf("error %q does not name the worktree path", unpushed.Error())
	}
}

// TestRemoveIfPushed_RemovesWhenHeadIsOnRemote checks the guard stays out of
// the way for the ordinary case — otherwise every finished fix would strand a
// worktree.
func TestRemoveIfPushed_RemovesWhenHeadIsOnRemote(t *testing.T) {
	m, anvil, wt := newFixWorktree(t, "forge/pushed-bead")
	commitInWorktree(t, wt.Path)
	runGit(t, wt.Path, "push", "origin", "forge/pushed-bead")

	if err := m.RemoveIfPushed(context.Background(), anvil, wt); err != nil {
		t.Fatalf("RemoveIfPushed on a pushed HEAD: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("worktree still present after a pushed HEAD: %v", err)
	}
}

// TestRemoveIfPushed_RemovesWhenRemoteMovedAhead covers the case where somebody
// pushed on top of the fix: HEAD is an ancestor of the remote tip, so the work
// is safe even though the SHAs differ.
func TestRemoveIfPushed_RemovesWhenRemoteMovedAhead(t *testing.T) {
	m, anvil, wt := newFixWorktree(t, "forge/ahead-bead")
	commitInWorktree(t, wt.Path)
	runGit(t, wt.Path, "push", "origin", "forge/ahead-bead")

	// A later commit lands on the remote; the worktree's HEAD is now behind.
	if err := os.WriteFile(filepath.Join(wt.Path, "more.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, wt.Path, "add", "more.txt")
	runGit(t, wt.Path, "commit", "-m", "more")
	runGit(t, wt.Path, "push", "origin", "forge/ahead-bead")
	runGit(t, wt.Path, "reset", "--hard", "HEAD~1")
	runGit(t, wt.Path, "fetch", "origin", "forge/ahead-bead")

	if err := m.RemoveIfPushed(context.Background(), anvil, wt); err != nil {
		t.Fatalf("RemoveIfPushed with the remote ahead: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("worktree still present although HEAD is on the remote: %v", err)
	}
}

// TestRemoveIfPushed_RemovesUnresolvableHead: a checkout with no resolvable
// HEAD holds nothing worth protecting, so the guard must not strand it.
func TestRemoveIfPushed_RemovesUnresolvableHead(t *testing.T) {
	m := NewManager()
	anvil := t.TempDir()
	dir := t.TempDir()

	err := m.RemoveIfPushed(context.Background(), anvil, &Worktree{Path: dir, Branch: "forge/x"})
	var unpushed *UnpushedHeadError
	if errors.As(err, &unpushed) {
		t.Fatalf("refused to remove a directory with no HEAD: %v", err)
	}
}
