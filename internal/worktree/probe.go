package worktree

import (
	"log/slog"
	"os"
	"path/filepath"
)

// ProbeNodeModules logs the mtime and entry count of each known node_modules
// location inside the given anvil directory. Checks the root and each
// subdirectory in nodeModulesDirs (matches linkNodeModules targets) so we
// see activity for client/frontend/etc. setups, not only repo-root
// node_modules. Purely diagnostic; never fails.
func ProbeNodeModules(label, anvilPath string) {
	if anvilPath == "" {
		slog.Info("probeNodeModules", "label", label, "status", "skipped_empty_anvil_path")
		return
	}
	for _, sub := range nodeModulesDirs {
		nmPath := filepath.Join(anvilPath, sub, "node_modules")
		probeOne(label, sub, nmPath)
	}
}

func probeOne(label, sub, nmPath string) {
	info, err := os.Stat(nmPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Only log not_found for the root; sub-dirs that genuinely
			// have no node_modules shouldn't spam the log.
			if sub == "" {
				slog.Info("probeNodeModules", "label", label, "sub", sub, "path", nmPath, "status", "not_found")
			}
			return
		}
		slog.Info("probeNodeModules", "label", label, "sub", sub, "path", nmPath, "status", "stat_error", "error", err)
		return
	}
	entries, readErr := os.ReadDir(nmPath)
	count := -1
	if readErr == nil {
		count = len(entries)
	}
	args := []any{
		"label", label,
		"sub", sub,
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
