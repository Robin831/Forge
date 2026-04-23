//go:build windows

package executil

import (
	"os/exec"
	"strconv"
	"syscall"
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
