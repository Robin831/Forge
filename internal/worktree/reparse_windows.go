//go:build windows

package worktree

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// unlinkReparsePoints walks root and removes any reparse points (junctions or
// symlinks) so that a subsequent os.RemoveAll does not follow them into the
// target directory. This prevents failures on Windows where locked files inside
// a junctioned node_modules cause os.RemoveAll to fail repeatedly.
func unlinkReparsePoints(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort: skip entries we cannot stat
		}
		if path == root {
			return nil
		}
		if !d.IsDir() && d.Type()&fs.ModeSymlink == 0 {
			return nil // regular file, skip
		}

		isReparse, rpErr := isReparsePoint(path)
		if rpErr != nil {
			return nil // best-effort
		}
		if !isReparse {
			return nil
		}

		slog.Info("unlinking reparse point before worktree removal", "path", path)
		if rmErr := os.Remove(path); rmErr != nil {
			slog.Warn("failed to unlink reparse point", "path", path, "error", rmErr)
			return nil // best-effort
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
}

// isReparsePoint checks the FILE_ATTRIBUTE_REPARSE_POINT flag via the Windows
// syscall. This covers both junctions and symlinks.
func isReparsePoint(path string) (bool, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attrs, err := syscall.GetFileAttributes(p)
	if err != nil {
		return false, err
	}
	return attrs&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}
