//go:build windows

package executil

import (
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CREATE_NO_WINDOW prevents the subprocess from inheriting or creating a
// console. Unlike HideWindow (which merely hides the window but still
// attaches a console), this flag ensures the child process cannot call
// Windows Console API functions like SetConsoleTitle that would corrupt
// the parent TUI's terminal tab title.
const createNoWindow = 0x08000000

func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

// PROCESS_SET_QUOTA and PROCESS_TERMINATE are the access rights
// AssignProcessToJobObject requires on the target process handle.
const processAssignAccess = windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE

var (
	workerJobOnce sync.Once
	workerJob     windows.Handle
	workerJobErr  error
)

// getWorkerJob lazily creates the process-wide Job Object that every spawned
// worker is assigned to. The job is configured with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: the daemon holds the only handle, so when
// the daemon process exits (cleanly or via crash) the OS closes that handle and
// terminates every process still assigned to the job. The handle is
// intentionally never closed for the daemon's lifetime — closing it early would
// kill all live workers.
func getWorkerJob() (windows.Handle, error) {
	workerJobOnce.Do(func() {
		job, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			workerJobErr = err
			return
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
			BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
				LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
			},
		}
		if _, err := windows.SetInformationJobObject(
			job,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			uint32(unsafe.Sizeof(info)),
		); err != nil {
			windows.CloseHandle(job)
			workerJobErr = err
			return
		}
		workerJob = job
	})
	return workerJob, workerJobErr
}

// containProcess assigns the started child to the shared worker Job Object so it
// (and its descendants) die with the daemon. Nested jobs are supported on
// Windows 8+, so a child that spins up its own job (e.g. node) is unaffected.
func containProcess(cmd *exec.Cmd) error {
	job, err := getWorkerJob()
	if err != nil {
		return err
	}
	handle, err := windows.OpenProcess(processAssignAccess, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.AssignProcessToJobObject(job, handle)
}

func setProcessGroup(cmd *exec.Cmd) {
	// On Windows, process groups are handled by CreateProcess flags.
	// CREATE_NEW_PROCESS_GROUP lets us target the group with
	// GenerateConsoleCtrlEvent later. Merge with existing flags set by
	// HideWindow if already applied.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// killProcessTree terminates pid and every descendant via
// `taskkill /T /F /PID <pid>`. This is effective while the root process still
// exists. After the root has fully exited, taskkill may not be able to walk the
// process tree by PID alone, so reaping orphaned descendants is best-effort and
// not guaranteed in that case.
//
// A "process not found" error (exit code 128) is treated as success, analogous
// to Unix ESRCH handling, since the target is already gone. Other errors are
// propagated; it is safe to ignore the return value in defer/teardown paths.
func killProcessTree(pid int) error {
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	hideWindow(cmd)
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 128 {
		return nil
	}
	return err
}
