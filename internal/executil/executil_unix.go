//go:build !windows

package executil

import (
	"os/exec"
	"syscall"
)

func hideWindow(cmd *exec.Cmd) {}

func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessTree sends SIGKILL to the process group whose leader is pid,
// falling back to a single-PID kill if the group cannot be resolved. When pid
// was started with Setpgid=true the group contains every descendant that did
// not explicitly call setsid, so this reaps orphaned background processes.
// ESRCH (process/group already gone) is treated as success.
func killProcessTree(pid int) error {
	pgid, err := syscall.Getpgid(pid)
	if err == nil && pgid > 0 {
		if killErr := syscall.Kill(-pgid, syscall.SIGKILL); killErr == nil || killErr == syscall.ESRCH {
			return nil
		}
	}
	if killErr := syscall.Kill(pid, syscall.SIGKILL); killErr != nil && killErr != syscall.ESRCH {
		return killErr
	}
	return nil
}
