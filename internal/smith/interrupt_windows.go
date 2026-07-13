//go:build windows

package smith

import "os"

// interruptProcessGroup requests a graceful stop on Windows. There is no
// universal SIGINT for arbitrary subprocesses — os.Process.Signal(os.Interrupt)
// delivers CTRL_BREAK_EVENT to processes started with CREATE_NEW_PROCESS_GROUP
// (which the Smith lifecycle sets via executil.SetProcessGroup). For processes
// not started that way this is best-effort; Interrupt's grace-period SIGKILL
// fallback guarantees the process is eventually reaped.
func interruptProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(os.Interrupt)
}

// killProcessGroup force-kills the process (TerminateProcess) as the
// grace-period fallback when it ignores the interrupt. Windows has no Unix-style
// process groups, so only the target pid is terminated.
func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Kill()
}
