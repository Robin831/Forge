//go:build !windows

package worktree

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// unlinkReparsePoints walks root and removes any symlinks so that a subsequent
// os.RemoveAll does not follow them into the target directory. On non-Windows
// platforms this handles symlinks; junctions are a Windows-only concept.
func unlinkReparsePoints(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}

		if d.Type()&fs.ModeSymlink == 0 {
			// A worktree's own node_modules: RemoveAll never follows a symlink here, and its .bin links and
			// workspace links are part of the install a reused worktree keeps (DevInfra-rjb.3).
			if d.IsDir() && d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}

		slog.Info("unlinking symlink before worktree removal", "path", path)
		if rmErr := os.Remove(path); rmErr != nil {
			slog.Warn("failed to unlink symlink", "path", path, "error", rmErr)
			return nil
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
}

// isReparsePoint reports whether path is a symlink: the only directory link outside Windows.
func isReparsePoint(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return info.Mode()&os.ModeSymlink != 0, nil
}
