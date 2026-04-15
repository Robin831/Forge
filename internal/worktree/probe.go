package worktree

import (
	"log/slog"
	"os"
	"path/filepath"
)

// ProbeNodeModules logs the mtime and entry count of node_modules in
// the given anvil directory. This is purely diagnostic and never fails.
func ProbeNodeModules(label, anvilPath string) {
	if anvilPath == "" {
		slog.Info("probeNodeModules", "label", label, "status", "skipped_empty_anvil_path")
		return
	}
	nmPath := filepath.Join(anvilPath, "node_modules")
	info, err := os.Stat(nmPath)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("probeNodeModules", "label", label, "path", nmPath, "status", "not_found")
		} else {
			slog.Info("probeNodeModules", "label", label, "path", nmPath, "status", "stat_error", "error", err)
		}
		return
	}
	entries, readErr := os.ReadDir(nmPath)
	count := -1
	if readErr == nil {
		count = len(entries)
	}
	args := []any{
		"label", label,
		"path", nmPath,
		"mtime", info.ModTime().UTC(),
		"entryCount", count,
	}
	if readErr != nil {
		args = append(args, "read_error", readErr)
	}
	slog.Info("probeNodeModules", args...)
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
