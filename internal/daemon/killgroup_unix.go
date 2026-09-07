//go:build !windows

package daemon

import (
	"errors"
	"syscall"
)

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
// Uses kill(pid, 0), which delivers no signal and succeeds iff the process
// exists and we are permitted to signal it.
//
// Two answers kill(2) gives that the bare `err == nil` test got wrong:
//
//   - A pid of 0 or less is NOT a process. kill(0, sig) addresses the caller's
//     whole process group and kill(-1, sig) every process it may signal, both
//     of which return nil and would report "alive" for a worker row that
//     records no pid at all. Every caller here means one process, so the
//     non-positive values are refused before the syscall rather than answered
//     by it.
//   - EPERM means the process EXISTS and we may not signal it. Read as dead it
//     would tell the kill path to stop escalating against a live process, and
//     the stale detector that a worker whose session is still running has
//     gone. ESRCH — no such process — is the only answer that means dead.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	// errors.Is rather than ==: syscall.Kill returns a bare syscall.Errno
	// today, for which the two are identical, but a wrapped error would make
	// the direct comparison read EPERM as "not EPERM" — i.e. a live process we
	// may not signal as a dead one — silently and in the one direction that
	// costs a running worker its row.
	return err == nil || errors.Is(err, syscall.EPERM)
}
