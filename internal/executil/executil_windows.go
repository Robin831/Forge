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
