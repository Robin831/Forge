package worktree

import (
	"log/slog"
	"os"
	"path/filepath"
)

// ProbeNodeModules logs the mtime and entry count of node_modules in
// the given anvil directory. This is purely diagnostic and never fails.
func ProbeNodeModules(label, anvilPath string) {
	nmPath := filepath.Join(anvilPath, "node_modules")
	info, err := os.Stat(nmPath)
	if err != nil {
		slog.Info("probeNodeModules", "label", label, "path", nmPath, "status", "not_found")
		return
	}
	entries, readErr := os.ReadDir(nmPath)
	count := -1
	if readErr == nil {
		count = len(entries)
	}
	slog.Info("probeNodeModules",
		"label", label,
		"path", nmPath,
		"mtime", info.ModTime().UTC(),
		"entryCount", count,
	)
}

// inferAnvilPath attempts to derive the anvil root from a worktree path
// by looking for a parent ".workers" directory component.
func inferAnvilPath(wtPath string) string {
	dir := filepath.Dir(wtPath)
	if filepath.Base(dir) == ".workers" {
		return filepath.Dir(dir)
	}
	return ""
}
