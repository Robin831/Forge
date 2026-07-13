//go:build !windows

package temper

import (
	"os"
	"syscall"
)

// processSignaled reports whether the process was terminated by a signal that
// indicates an infrastructure/host failure rather than a normal non-zero exit
// (SIGKILL — e.g. an OOM kill — SIGSEGV, SIGBUS, or SIGABRT). A context-timeout
// kill also raises SIGKILL, so callers MUST check the context deadline before
// consulting this. The returned string names the signal for logging.
func processSignaled(ps *os.ProcessState) (bool, string) {
	if ps == nil {
		return false, ""
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok {
		return false, ""
	}
	if !ws.Signaled() {
		return false, ""
	}
	sig := ws.Signal()
	switch sig {
	case syscall.SIGKILL, syscall.SIGSEGV, syscall.SIGBUS, syscall.SIGABRT:
		return true, sig.String()
	}
	return false, ""
}
