//go:build !windows

package smith

import "syscall"

// interruptProcessGroup sends SIGINT to the process group led by pid so the
// Smith subprocess and its children (git, node, etc.) all receive a graceful
// stop. Smith spawns are started with executil.SetProcessGroup (Setpgid=true),
// so the process group id equals the leader pid. If the process has already
// exited the group signal fails harmlessly and we fall back to signaling the
// individual pid.
func interruptProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGINT); err != nil {
		_ = syscall.Kill(pid, syscall.SIGINT)
	}
}

// killProcessGroup force-kills (SIGKILL) the process group led by pid, falling
// back to the individual pid. Used as the grace-period fallback when a spawn
// ignores the SIGINT from interruptProcessGroup.
func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
