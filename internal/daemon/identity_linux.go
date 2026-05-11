//go:build linux

package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procFS is the procfs root used to verify daemon process identity.
// Tests override this to point at a fixture directory.
var procFS = "/proc"

// isForgeProcess reports whether the process with the given pid is a forge
// binary. It exists so IsRunning() can distinguish a live forge daemon from
// an unrelated process that inherited a stale pidfile's PID — common in
// container PID namespaces where init or its children sit at low PIDs.
//
// Verification:
//  1. Read /proc/<pid>/comm and compare basename to "forge".
//  2. If comm is unreadable for a reason other than ENOENT, fall back to
//     /proc/<pid>/exe and compare the symlink target's basename to "forge".
//
// Returns (false, nil) when the process exists but is not forge, or when
// the process is gone (ENOENT). Returns (false, err) only on unexpected
// I/O errors that the caller should surface.
func isForgeProcess(pid int) (bool, error) {
	pidDir := filepath.Join(procFS, strconv.Itoa(pid))

	data, err := os.ReadFile(filepath.Join(pidDir, "comm"))
	switch {
	case err == nil:
		return filepath.Base(strings.TrimSpace(string(data))) == "forge", nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}

	target, err := os.Readlink(filepath.Join(pidDir, "exe"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return false, nil
		}
		return false, err
	}
	return filepath.Base(target) == "forge", nil
}
