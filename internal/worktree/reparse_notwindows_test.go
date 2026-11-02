//go:build !windows

package worktree

import (
	"os"
	"path/filepath"
	"testing"
)

// Reuse cleanup unlinks shared links but keeps a worktree's own install whole, .bin links included (DevInfra-rjb.3).
func TestUnlinkReparsePointsKeepsAWorktreesOwnNodeModules(t *testing.T) {
	root, target := t.TempDir(), t.TempDir()
	bin := filepath.Join(root, "client", "node_modules", ".bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../vite/bin/vite.js", filepath.Join(bin, "vite")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "shared")); err != nil {
		t.Fatal(err)
	}
	if err := unlinkReparsePoints(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(bin, "vite")); err != nil {
		t.Errorf("the worktree's own .bin link must stay: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "shared")); !os.IsNotExist(err) {
		t.Errorf("a shared link must go (err=%v)", err)
	}
}
