// Package executil provides helpers for spawning subprocesses.
package executil

import "os/exec"

// HideWindow configures cmd to not create a visible console window.
// On Windows this sets CREATE_NO_WINDOW. On other platforms it is a no-op.
func HideWindow(cmd *exec.Cmd) *exec.Cmd {
	hideWindow(cmd)
	return cmd
}

// SetProcessGroup configures cmd to start in its own process group.
// On Unix this sets Setpgid so signals can be sent to the entire group
// via kill(-pid, sig). On Windows this sets CREATE_NEW_PROCESS_GROUP.
func SetProcessGroup(cmd *exec.Cmd) *exec.Cmd {
	setProcessGroup(cmd)
	return cmd
}
