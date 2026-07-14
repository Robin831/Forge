//go:build !windows

package executil

import (
	"os/exec"
	"syscall"
)

func hideWindow(cmd *exec.Cmd) {}

// containProcess is a no-op on Unix: worker containment is provided by
// SetProcessGroup (Setpgid) + KillProcessTree and the /proc-based orphan sweep.
func containProcess(cmd *exec.Cmd) error { return nil }

func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessTree sends SIGKILL to the process group whose leader is pid. When
// cmd was started with SetProcessGroup (Setpgid=true) the kernel creates a new
// process group whose pgid equals pid, containing every descendant that did not
// explicitly call setsid. A process group outlives its leader: the group is
// reaped only once every member has exited, so signalling -pid still reaches
// orphaned background descendants even after the parent has returned from
// cmd.Wait() and had its PID reaped. This is the real-world failure mode the
// caller cares about — a build script spawns `npx http-server &` and exits
// without waiting, leaving the server running and holding worktree files open.
//
// Getpgid is deliberately NOT called here: once the leader PID has been reaped
// it returns ESRCH, which would previously cause this function to fall back to
// a single-PID kill of the already-dead leader and do nothing. We instead rely
// on the Setpgid=true contract (pgid == pid) and signal -pid directly. ESRCH
// from the group kill (nobody left in the group) is treated as success.
func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
