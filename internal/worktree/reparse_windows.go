//go:build windows

package worktree

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// unlinkReparsePoints removes any reparse points (junctions or symlinks)
// under root so that a subsequent os.RemoveAll or git worktree remove does
// not follow them into the target directory. This prevents failures (and
// destructive writes to the main checkout) on Windows where locked files
// inside a junctioned node_modules cause external tools to walk the
// junction contents and delete files one by one from the target.
//
// Implementation: we MUST NOT rely on filepath.WalkDir alone here.
// On Windows, filepath.WalkDir descends into NTFS junctions as if they
// were regular directories (the callback is never invoked on the
// junction node itself — only on its target's contents). So a walk-only
// approach silently misses the junction and leaves it in place.
//
// Instead, first probe the well-known junction locations directly
// (matches the targets in linkNodeModules: "", web, frontend, client,
// app, ui). Each of those is a candidate for `<root>/<sub>/node_modules`
// being a junction we created. Then follow up with a walk as a safety
// net for any other reparse points (e.g. a Smith that made its own
// symlink somewhere).
func unlinkReparsePoints(root string) error {
	// Pass 1: directly check known node_modules junction locations.
	for _, sub := range nodeModulesDirs {
		candidate := filepath.Join(root, sub, "node_modules")
		tryUnlinkReparsePoint(candidate)
	}

	// Pass 2: walk the tree for any other reparse points (best-effort).
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

		slog.Info("unlinking reparse point before worktree removal", "path", path, "via", "walk")
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

// tryUnlinkReparsePoint checks whether path is a reparse point and, if so,
// removes it. os.Remove on a Windows directory junction removes the junction
// link itself without following it to the target.
func tryUnlinkReparsePoint(path string) {
	if _, err := os.Lstat(path); err != nil {
		return // path does not exist — nothing to do
	}
	isReparse, rpErr := isReparsePoint(path)
	if rpErr != nil {
		return // best-effort: skip if we can't read attributes
	}
	if !isReparse {
		return
	}
	slog.Info("unlinking reparse point before worktree removal", "path", path, "via", "direct")
	if rmErr := os.Remove(path); rmErr != nil {
		slog.Warn("failed to unlink reparse point", "path", path, "error", rmErr)
	}
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
