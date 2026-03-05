// Package executil provides helpers for spawning subprocesses.
package executil

import "os/exec"

// HideWindow configures cmd to not create a visible console window.
// On Windows this sets CREATE_NO_WINDOW. On other platforms it is a no-op.
func HideWindow(cmd *exec.Cmd) *exec.Cmd {
	hideWindow(cmd)
	return cmd
}
