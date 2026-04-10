//go:build windows

package daemon

import (
	"os"
	"syscall"
)

// Windows constants not re-exported by the syscall package.
// PROCESS_QUERY_LIMITED_INFORMATION (0x1000) is the minimal access right
// needed to call GetExitCodeProcess — it works even for elevated processes
// when the caller runs unprivileged.
// STILL_ACTIVE (259) is the exit code GetExitCodeProcess returns while a
// process is still running.
const (
	processQueryLimitedInformation = 0x1000
	stillActive                    = 259
)

// processGroup returns the process group ID for the given PID.
// On Windows, Unix-style process groups are not supported;
// returns (pid, false) so callers fall back to single-PID signaling.
func processGroup(pid int) (int, bool) {
	return pid, false
}

// signalInterrupt requests a graceful stop on Windows. There is no universal
// SIGINT for arbitrary subprocesses — os.Process.Signal(os.Interrupt) delivers
// CTRL_BREAK_EVENT to processes started with CREATE_NEW_PROCESS_GROUP (which
// the Smith lifecycle does via executil.SetProcessGroup). For processes that
// weren't started that way, this is a best-effort call and the caller should
// rely on the SIGKILL fallback after the grace period.
func signalInterrupt(pid, pgid int, pgidKnown bool) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(os.Interrupt)
}

// signalKill forcibly terminates the process via TerminateProcess
// (os.Process.Kill on Windows calls TerminateProcess under the hood).
// There are no process groups on Windows, so pgid is ignored.
func signalKill(pid, pgid int, pgidKnown bool) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Kill()
}

// processAlive reports whether the process with the given PID is still running.
// Uses OpenProcess + GetExitCodeProcess, the Windows equivalent of Unix
// kill(pid, 0). A process is considered alive iff its exit code is STILL_ACTIVE.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var exitCode uint32
	if err := syscall.GetExitCodeProcess(h, &exitCode); err != nil {
		// Handle is valid but we can't read the exit code; assume alive so
		// the caller falls through to its SIGKILL fallback.
		return true
	}
	return exitCode == stillActive
}
