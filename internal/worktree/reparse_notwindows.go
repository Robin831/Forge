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
