//go:build windows

package kiln

import (
	"time"

	"golang.org/x/sys/windows"

	"github.com/Robin831/Forge/internal/executil"
)

// stillActive is the exit code GetExitCodeProcess reports for a process that
// has not exited (STILL_ACTIVE). x/sys/windows does not export it.
const stillActive = 259

// inspectProcess returns a snapshot of the live process with the given pid.
//
// Windows exposes a process's creation time cheaply but not its working
// directory (that would mean reading the target's PEB), so ProcessInfo.Cwd is
// always empty here and CwdSupported is false: ownership rests on the PID +
// creation-time match alone, exactly as the worker orphan sweep in
// internal/shutdown does. A process whose times cannot be read (access denied
// on a system process) keeps a zero start time, which fails the ownership check
// and is therefore never signalled.
func inspectProcess(pid int) (ProcessInfo, bool) {
	if pid <= 0 {
		return ProcessInfo{}, false
	}
	// PROCESS_QUERY_LIMITED_INFORMATION is the least-privileged right that
	// still permits GetProcessTimes and GetExitCodeProcess, so this works
	// without elevation for processes owned by the same user.
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ProcessInfo{}, false
	}
	defer windows.CloseHandle(handle)

	// A handle can outlive the process, so "opened" is not "running".
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil || code != stillActive {
		return ProcessInfo{}, false
	}

	info := ProcessInfo{PID: pid}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err == nil {
		// Filetime.Nanoseconds() is nanoseconds since the Unix epoch, matching
		// the representation used for Unix process start times.
		info.StartTime = time.Unix(0, creation.Nanoseconds())
	}
	return info, true
}

// terminateProcessGroup stops the process tree rooted at pid via
// `taskkill /T /F`, which is the Windows equivalent of signalling the group.
// The grace period is unused: taskkill's tree walk is immediate and there is no
// deliverable polite signal to wait on.
func terminateProcessGroup(pid int, _ time.Duration) error {
	if pid <= 0 {
		return nil
	}
	return executil.KillProcessGroup(pid)
}
