package worktree

import (
	"context"
	"path/filepath"
	"testing"
)

// TestCheckAnvilOrigin_ReportsAWorkerWorktree is the measured fault: origin
// repointed at a path inside one of the anvil's own worker worktrees, which is
// deleted when the bead finishes. Reported at teardown, because that is the
// last moment the bead holding that worktree is known.
func TestCheckAnvilOrigin_ReportsAWorkerWorktree(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "main")

	bad := filepath.Join(dir, ".workers", "Fhi.Metadata-l9l2n.44", ".git")
	runGit(t, dir, "remote", "add", "origin", bad)

	got := CheckAnvilOrigin(context.Background(), dir, "Fhi.Metadata-l9l2n.44")
	if got == "" {
		t.Fatal("a remote pointing inside the anvil must be reported")
	}
	if filepath.Clean(got) != filepath.Clean(bad) {
		t.Errorf("got %q, want %q", got, bad)
	}
}

// A normal remote is left entirely alone — the check must never report the
// healthy state, or the log line stops meaning anything.
func TestCheckAnvilOrigin_LeavesARealRemoteAlone(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "main")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/FHIDev/Fhi.Munin.Explorer.git")

	if got := CheckAnvilOrigin(context.Background(), dir, "bd-1"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// An anvil with no origin configured at all is a different fault, and not this
// one's to report.
func TestCheckAnvilOrigin_NoOriginIsNotThisFault(t *testing.T) {
	dir := t.TempDir()
	initTestRepo(t, dir, "main")

	if got := CheckAnvilOrigin(context.Background(), dir, "bd-1"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// Not a repository at all: the read fails, and a check that cannot read the
// config reports nothing rather than guessing.
func TestCheckAnvilOrigin_NotARepository(t *testing.T) {
	if got := CheckAnvilOrigin(context.Background(), t.TempDir(), "bd-1"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
