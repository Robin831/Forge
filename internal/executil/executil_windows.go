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
// `taskkill /T /F /PID <pid>`. Windows does not re-parent orphans, so even
// after the root process has exited, taskkill can still walk the tree by
// ParentProcessID and reap lingering background children (e.g. detached
// http-server processes spawned by a build script). The exec.Cmd keeps a
// process handle open until Wait completes, which prevents PID reuse from
// racing with teardown in the common case.
//
// Output is discarded; callers can rely on the return value (any non-nil
// error from taskkill is propagated) but it is safe to ignore in defers.
func killProcessTree(pid int) error {
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	hideWindow(cmd)
	return cmd.Run()
}
