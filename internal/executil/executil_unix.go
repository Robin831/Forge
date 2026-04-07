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
