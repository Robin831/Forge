//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// detachProcess configures the command to run detached from the parent on Windows.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x00000008, // DETACHED_PROCESS
	}
}
