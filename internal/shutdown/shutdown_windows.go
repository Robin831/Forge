//go:build windows

package shutdown

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// platformSupportsProcessCwd is false on Windows: there is no cheap, reliable
// way to read another process's current working directory (it would require
// reading the target's PEB via ReadProcessMemory with elevated rights), so the
// orphan sweep cannot use a cwd-in-worktree match. Instead, Windows worker
// containment is handled primarily by the Job Object assigned at spawn (see
// executil.ContainProcess), and the sweep confirms ownership of any pre-crash
// stray via a strong PID + creation-time match against a recorded worker row.
// The unrecorded-PID secondary sweep (argv[0]-basename only) is skipped because
// without the cwd check it could not distinguish a stray worker from the
// operator's own Claude session. See identifyForgeOwnedProcesses.
const platformSupportsProcessCwd = false

// listProcessesPlatform enumerates running processes via a Toolhelp snapshot,
// capturing each process's PID, executable basename, and creation time. The
// creation time is used by identifyForgeOwnedProcesses (via startTimeConsistent)
// to guard against PID recycling when matching against recorded worker rows.
//
// It never reads another process's working directory (see
// platformSupportsProcessCwd), so procInfo.cwd is always empty here. Best-effort:
// processes whose creation time cannot be read (e.g. access denied on system
// processes) are still returned with a zero start time, which fails the
// start-time consistency check and is therefore never reaped.
func listProcessesPlatform() ([]procInfo, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	var procs []procInfo
	err = windows.Process32First(snapshot, &entry)
	for err == nil {
		pid := int(entry.ProcessID)
		p := procInfo{
			pid:  pid,
			argv: []string{windows.UTF16ToString(entry.ExeFile[:])},
		}
		if start, ok := processCreationTime(entry.ProcessID); ok {
			p.startTime = start
		}
		procs = append(procs, p)

		err = windows.Process32Next(snapshot, &entry)
	}
	// ERROR_NO_MORE_FILES marks the normal end of enumeration.
	if err != nil && err != windows.ERROR_NO_MORE_FILES {
		return procs, err
	}
	return procs, nil
}

// processCreationTime returns the wall-clock creation time of the given PID,
// converting the Windows FILETIME (100ns ticks since 1601) to a Go time.Time.
// The second return is false when the process cannot be opened or queried.
func processCreationTime(pid uint32) (time.Time, bool) {
	// PROCESS_QUERY_LIMITED_INFORMATION is the least-privileged right that still
	// permits GetProcessTimes, so the sweep works without elevation for
	// processes owned by the same user.
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return time.Time{}, false
	}
	defer windows.CloseHandle(handle)

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return time.Time{}, false
	}
	// Filetime.Nanoseconds() returns nanoseconds since the Unix epoch, matching
	// the representation used for Unix process start times.
	return time.Unix(0, creation.Nanoseconds()), true
}
