package worktree

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLinkNodeModules_RootLevel(t *testing.T) {
	anvilPath := t.TempDir()
	worktreePath := t.TempDir()

	// Create node_modules in the anvil root.
	srcNM := filepath.Join(anvilPath, "node_modules")
	if err := os.Mkdir(srcNM, 0o755); err != nil {
		t.Fatal(err)
	}
	// Place a marker file so we can verify the link target.
	if err := os.WriteFile(filepath.Join(srcNM, "marker.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := linkNodeModules(anvilPath, worktreePath); err != nil {
		t.Fatalf("linkNodeModules: %v", err)
	}

	dstNM := filepath.Join(worktreePath, "node_modules")
	// Verify the link exists and is a symlink (Unix) or junction (Windows).
	info, err := os.Lstat(dstNM)
	if err != nil {
		t.Fatalf("Lstat %s: %v", dstNM, err)
	}
	if runtime.GOOS != "windows" {
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected symlink, got %v", info.Mode())
		}
	}

	// Verify we can read through the link.
	data, err := os.ReadFile(filepath.Join(dstNM, "marker.txt"))
	if err != nil {
		t.Fatalf("reading through link: %v", err)
	}
	if string(data) != "ok" {
		t.Fatalf("unexpected content: %q", data)
	}
}

func TestLinkNodeModules_Subdirectory(t *testing.T) {
	anvilPath := t.TempDir()
	worktreePath := t.TempDir()

	// Create client/node_modules in the anvil.
	srcNM := filepath.Join(anvilPath, "client", "node_modules")
	if err := os.MkdirAll(srcNM, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcNM, "marker.txt"), []byte("client"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create the client/ dir in the worktree (as git would).
	if err := os.MkdirAll(filepath.Join(worktreePath, "client"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := linkNodeModules(anvilPath, worktreePath); err != nil {
		t.Fatalf("linkNodeModules: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(worktreePath, "client", "node_modules", "marker.txt"))
	if err != nil {
		t.Fatalf("reading through link: %v", err)
	}
	if string(data) != "client" {
		t.Fatalf("unexpected content: %q", data)
	}
}

func TestLinkNodeModules_SkipsExisting(t *testing.T) {
	anvilPath := t.TempDir()
	worktreePath := t.TempDir()

	// Create node_modules in both anvil and worktree.
	srcNM := filepath.Join(anvilPath, "node_modules")
	if err := os.Mkdir(srcNM, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcNM, "marker.txt"), []byte("anvil"), 0o644); err != nil {
		t.Fatal(err)
	}

	dstNM := filepath.Join(worktreePath, "node_modules")
	if err := os.Mkdir(dstNM, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dstNM, "marker.txt"), []byte("worktree"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := linkNodeModules(anvilPath, worktreePath); err != nil {
		t.Fatalf("linkNodeModules: %v", err)
	}

	// Should not have been replaced.
	data, err := os.ReadFile(filepath.Join(dstNM, "marker.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "worktree" {
		t.Fatalf("existing node_modules was replaced: got %q", data)
	}
}

func TestLinkNodeModules_NoNodeModules(t *testing.T) {
	anvilPath := t.TempDir()
	worktreePath := t.TempDir()

	// No node_modules anywhere — should be a no-op.
	if err := linkNodeModules(anvilPath, worktreePath); err != nil {
		t.Fatalf("linkNodeModules: %v", err)
	}

	entries, err := os.ReadDir(worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty worktree dir, got %d entries", len(entries))
	}
}

func TestLinkNodeModules_MultipleSubdirs(t *testing.T) {
	anvilPath := t.TempDir()
	worktreePath := t.TempDir()

	// Create node_modules in web/ and client/.
	for _, sub := range []string{"web", "client"} {
		srcNM := filepath.Join(anvilPath, sub, "node_modules")
		if err := os.MkdirAll(srcNM, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(srcNM, "id.txt"), []byte(sub), 0o644); err != nil {
			t.Fatal(err)
		}
		// Create parent dir in worktree.
		if err := os.MkdirAll(filepath.Join(worktreePath, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := linkNodeModules(anvilPath, worktreePath); err != nil {
		t.Fatalf("linkNodeModules: %v", err)
	}

	for _, sub := range []string{"web", "client"} {
		data, err := os.ReadFile(filepath.Join(worktreePath, sub, "node_modules", "id.txt"))
		if err != nil {
			t.Fatalf("reading %s link: %v", sub, err)
		}
		if string(data) != sub {
			t.Fatalf("%s: unexpected content: %q", sub, data)
		}
	}
}
