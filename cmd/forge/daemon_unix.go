//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detachProcess configures the command to run detached from the parent on Unix.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}
