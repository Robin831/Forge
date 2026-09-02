package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateEphemeral_ProducesAValidatableCheckout(t *testing.T) {
	anvil := t.TempDir()
	initTestRepo(t, anvil, "main")
	head := gitOutput(t, anvil, "rev-parse", "HEAD")

	wt, err := CreateEphemeral(context.Background(), anvil)
	if err != nil {
		t.Fatalf("CreateEphemeral: %v", err)
	}

	if wt.Head != head {
		t.Errorf("Head = %q, want %q", wt.Head, head)
	}
	if strings.HasPrefix(wt.Path, anvil) {
		t.Errorf("ephemeral checkout %s must live outside the anvil %s", wt.Path, anvil)
	}
	// The property the whole thing exists for: Smith's pre-flight refuses
	// the anvil and accepts this.
	if err := ValidateWorktreeDir(anvil); err == nil {
		t.Fatal("fixture is not a main checkout: the pre-flight accepted it")
	}
	if err := ValidateWorktreeDir(wt.Path); err != nil {
		t.Errorf("pre-flight refused the ephemeral checkout: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(wt.Path, "README")); err != nil || string(body) != "test\n" {
		t.Errorf("checkout is not materialized at HEAD: %q, %v", body, err)
	}

	if err := wt.Remove(context.Background()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("checkout survived Remove: %v", err)
	}
	if list := gitOutput(t, anvil, "worktree", "list"); strings.Contains(list, wt.Path) {
		t.Errorf("registration survived Remove:\n%s", list)
	}
}

// The invariant the teardown ordering serves: a `git worktree remove` that
// declines must leave behind neither the checkout directory nor a
// registration in the anvil. `git worktree prune` keeps an entry whose
// directory still exists, so it runs after the directory is deleted rather
// than before — pruned first, every declined removal would leave a prunable
// entry, and since the basename is always "worktree" they accumulate as
// worktree, worktree1, ...
//
// The refusal is forced by deleting the checkout's .git pointer, which is
// what a half-removed worktree looks like to git. (git prunes that
// particular entry from either position; what this pins is the end state,
// which is what a reader of `git worktree list` cares about.)
func TestEphemeralRemove_PrunesAfterAFailedWorktreeRemove(t *testing.T) {
	anvil := t.TempDir()
	initTestRepo(t, anvil, "main")

	wt, err := CreateEphemeral(context.Background(), anvil)
	if err != nil {
		t.Fatalf("CreateEphemeral: %v", err)
	}
	if err := os.Remove(filepath.Join(wt.Path, ".git")); err != nil {
		t.Fatalf("removing .git pointer: %v", err)
	}

	err = wt.Remove(context.Background())
	if err == nil {
		t.Fatal("a declining git worktree remove must be reported")
	}

	// Reported, but not left behind: neither the directory nor the entry.
	if _, statErr := os.Stat(wt.Path); !os.IsNotExist(statErr) {
		t.Errorf("checkout survived Remove: %v", statErr)
	}
	if list := gitOutput(t, anvil, "worktree", "list"); strings.Contains(list, wt.Path) {
		t.Errorf("registration was not pruned after the failed remove:\n%s", list)
	}
}
