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

// signalInterrupt sends SIGINT to the process group when known, falling
// back to the individual PID if group signaling fails.
func signalInterrupt(pid, pgid int, pgidKnown bool) {
	if pgidKnown {
		if err := syscall.Kill(-pgid, syscall.SIGINT); err != nil {
			_ = syscall.Kill(pid, syscall.SIGINT)
		}
		return
	}
	_ = syscall.Kill(pid, syscall.SIGINT)
}

// signalKill sends SIGKILL to the process group when known, falling
// back to the individual PID if group signaling fails.
func signalKill(pid, pgid int, pgidKnown bool) {
	if pgidKnown {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// processAlive reports whether the process with the given PID is still running.
// Uses kill(pid, 0) which succeeds iff the process exists and we can signal it.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
