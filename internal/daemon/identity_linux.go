//go:build linux

package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
// the process is gone (ENOENT). Returns (false, err) when identity cannot
// be verified — including permission-denied errors from procfs hidepid
// mounts — so the caller can treat the process as alive rather than
// deleting the pidfile.
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
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		// Permission errors (e.g. hidepid) mean we cannot verify identity;
		// return the error so IsRunning() assumes alive rather than removing
		// a potentially valid pidfile.
		return false, err
	}
	return filepath.Base(target) == "forge", nil
}

// procStartTime returns the start time of the process with the given pid,
// derived from the modification time of /proc/<pid>/ — which the Linux
// kernel sets to the process's creation time.
//
// IsRunning() uses this to detect a class of stale pidfile that the
// isForgeProcess check cannot: when a container is recycled (SIGKILL,
// OOM, grace-period expiry), the new container's PID namespace is empty
// and the freshly-spawned forge daemon almost always lands at the same
// low PID the previous incarnation held (typically 7 here). The new
// process's /proc/<pid>/comm equals "forge" — basename matches — so
// isForgeProcess returns true and the new daemon refuses to start. But
// the *previous* daemon's pidfile predates the new process: its mtime
// is older than /proc/<pid>/'s mtime. That timestamp ordering is the
// signal that the pidfile is from a dead earlier incarnation.
//
// Returns (zero, err) on any procfs read failure so the caller can
// treat the staleness check as inconclusive and fall back to the
// existing identity + liveness verdict.
func procStartTime(pid int) (time.Time, error) {
	pidDir := filepath.Join(procFS, strconv.Itoa(pid))
	fi, err := os.Stat(pidDir)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}
