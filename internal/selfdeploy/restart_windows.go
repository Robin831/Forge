//go:build windows

package selfdeploy

import (
	"os/exec"
	"syscall"
)

// detachedProcess is the Win32 DETACHED_PROCESS creation flag (the child gets no
// console). It is not exposed by the syscall package.
const detachedProcess = 0x00000008

// detachProcess is the Windows analogue of the Unix setsid detach. Self-deploy
// is a systemd feature and there is no unit cgroup to escape here, but the same
// intent applies: CREATE_NEW_PROCESS_GROUP keeps the child out of the daemon's
// Ctrl-C/console group and DETACHED_PROCESS gives it no console to lose when the
// daemon exits. This exists so the package builds and behaves sanely off Linux.
func detachProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess
}
