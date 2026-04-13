package worktree

import (
	"fmt"
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
// Only directories where the main checkout has a node_modules are linked.
// If the worktree already has a node_modules (e.g. from a reused worktree),
// the existing directory is left untouched.
func linkNodeModules(anvilPath, worktreePath string) error {
	for _, sub := range nodeModulesDirs {
		srcNM := filepath.Join(anvilPath, sub, "node_modules")
		info, err := os.Stat(srcNM)
		if err != nil || !info.IsDir() {
			continue // no node_modules in the main checkout at this path
		}

		dstParent := filepath.Join(worktreePath, sub)
		dstNM := filepath.Join(dstParent, "node_modules")

		// If the destination already exists (dir, symlink, or junction), skip.
		if _, err := os.Lstat(dstNM); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking existing node_modules destination %s: %w", sub, err)
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

// createDirLink creates a directory symlink (Unix) or junction (Windows)
// pointing from dst to src.
func createDirLink(src, dst string) error {
	if runtime.GOOS == "windows" {
		return createJunction(src, dst)
	}
	return os.Symlink(src, dst)
}
