package worktree

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// nodeModulesDirs is the list of directories where a Node.js project might
// have a node_modules folder. An empty string means the repository root.
// This mirrors the subdirectories checked by temper's detectNodeDirs.
var nodeModulesDirs = []string{"", "web", "frontend", "client", "app", "ui"}

// linkNodeModules creates symlinks (or junctions on Windows) from the
// worktree's node_modules directories to the corresponding directories in
// the main anvil checkout. This allows Smiths to run npm scripts without
// a fresh npm ci, since the main checkout already has node_modules populated.
//
// Only directories where the main checkout has a node_modules are linked, and
// only when that install matches the worktree's package-lock.json. A reused
// worktree's own directory is left untouched; its stale link is removed.
func linkNodeModules(anvilPath, worktreePath string) error {
	return linkNodeModulesWith(anvilPath, worktreePath, true)
}

// linkNodeModulesWith is linkNodeModules; with onlyMatching false (a preview, which has no install step and
// would otherwise start without any node_modules) a stale install is still shared, with a warning.
func linkNodeModulesWith(anvilPath, worktreePath string, onlyMatching bool) error {
	for _, sub := range nodeModulesDirs {
		srcNM := filepath.Join(anvilPath, sub, "node_modules")
		info, err := os.Stat(srcNM)
		if err != nil || !info.IsDir() {
			continue // no node_modules in the main checkout at this path
		}

		dstParent := filepath.Join(worktreePath, sub)
		dstNM := filepath.Join(dstParent, "node_modules")
		// Only a tree installed from this worktree's own lockfile is shared: Temper refuses npm ci on a link, so a
		// stale shared tree would never be refreshed (DevInfra-rjb.3). Unknown (no lockfile) keeps the old sharing.
		staleTree := func() (bool, string) {
			match, known, why := installedMatchesLock(srcNM, filepath.Join(dstParent, "package-lock.json"))
			if known && !match && !onlyMatching {
				slog.Warn("worktree: sharing an install that does not match this lockfile (no install step here)", "dir", sub, "worktree", worktreePath, "why", why)
				return false, why
			}
			return known && !match, why
		}

		// An existing destination: a real directory is the worktree's own and stays; a link to a stale tree goes.
		if _, err := os.Lstat(dstNM); err == nil {
			linked, _ := isReparsePoint(dstNM)
			if !linked {
				continue
			}
			if stale, why := staleTree(); stale {
				// os.Remove takes the link itself, never the anvil's tree behind it.
				if err := os.Remove(dstNM); err != nil {
					return fmt.Errorf("removing stale node_modules link %s: %w", sub, err)
				}
				slog.Info("worktree: node_modules link removed, the anvil's install does not match this lockfile; npm ci installs here", "dir", sub, "worktree", worktreePath, "why", why)
			}
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking existing node_modules destination %s: %w", sub, err)
		}
		if stale, why := staleTree(); stale {
			slog.Info("worktree: node_modules not linked, the anvil's install does not match this lockfile; npm ci installs here", "dir", sub, "worktree", worktreePath, "why", why)
			continue
		}

		// Ensure the parent directory exists in the worktree. For root-level
		// node_modules this is always true, but subdirectories like "client/"
		// might not be checked out yet if they are empty.
		if err := os.MkdirAll(dstParent, 0o755); err != nil {
			return fmt.Errorf("creating parent dir for node_modules link %s: %w", sub, err)
		}

		if err := createDirLink(srcNM, dstNM); err != nil {
			return fmt.Errorf("linking node_modules %s: %w", sub, err)
		}
	}
	return nil
}

// LinkNodeModules is the exported form of linkNodeModules for use by packages
// that need to re-establish node_modules links after git operations.
func LinkNodeModules(anvilPath, worktreePath string) error {
	return linkNodeModules(anvilPath, worktreePath)
}

// createDirLink creates a directory symlink (Unix) or junction (Windows)
// pointing from dst to src.
func createDirLink(src, dst string) error {
	if runtime.GOOS == "windows" {
		return createJunction(src, dst)
	}
	return os.Symlink(src, dst)
}
