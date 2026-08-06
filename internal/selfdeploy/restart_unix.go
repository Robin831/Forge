//go:build !windows

package selfdeploy

import (
	"os/exec"
	"syscall"
)

// detachProcess starts the restart child in a brand new session (setsid), which
// makes it a session and process-group leader in its own right. Without it the
// child inherits the daemon's process group, so any group-directed signal — the
// daemon's own shutdown path, a supervisor's SIGTERM sweep — reaches the process
// that is meant to be performing the restart. Combined with the systemd-run
// scope (which moves it out of forge.service's cgroup) and the absence of a
// context (which removes the deadline kill), this is what lets the restart
// outlive the daemon it is restarting.
func detachProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}
