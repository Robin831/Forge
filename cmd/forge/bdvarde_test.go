package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Robin831/Forge/internal/config"
)

// A relative anvil path is read against the daemon's directory (--base), not the anvil it runs in.
func TestAnvilParentForCwdReadsRelativePathsAgainstTheBase(t *testing.T) {
	base := t.TempDir()
	anvil := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(anvil, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := cfg
	defer func() { cfg = old }()
	cfg = &config.Config{Anvils: map[string]config.AnvilConfig{"r": {Path: "./repo", BeadsParent: "EP-1"}, "other": {Path: "/elsewhere", BeadsParent: "EP-2"}}}
	t.Chdir(filepath.Join(anvil, "sub"))
	if got := anvilParentForCwd(base); got != "EP-1" {
		t.Errorf("relative anvil: %q", got)
	}
	if got := anvilParentForCwd(""); got != "" {
		t.Errorf("without a base a relative path matches nothing: %q", got)
	}
}
