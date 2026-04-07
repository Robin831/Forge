//go:build windows

package executil

import (
	"os/exec"
	"syscall"
)

// CREATE_NO_WINDOW prevents the subprocess from inheriting or creating a
// console. Unlike HideWindow (which merely hides the window but still
// attaches a console), this flag ensures the child process cannot call
// Windows Console API functions like SetConsoleTitle that would corrupt
// the parent TUI's terminal tab title.
const createNoWindow = 0x08000000

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
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
