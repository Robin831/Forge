//go:build !windows

package daemon

import "syscall"

// processGroup returns the process group ID for the given PID.
// Returns (pgid, true) on success; falls back to (pid, false) if the PGID
// cannot be determined (e.g. the process has already exited).
func processGroup(pid int) (int, bool) {
	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid <= 0 {
		return pid, false
	}
	return pgid, true
}
