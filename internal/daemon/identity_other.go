//go:build !linux

package daemon

// isForgeProcess is a no-op on non-Linux platforms. Windows relies on the
// named pipe as a liveness proxy in IsRunning(), and macOS keeps the
// historical behavior of trusting the Signal(0) liveness check. Returning
// true preserves the prior semantics: IsRunning() proceeds as if the
// liveness check were authoritative.
func isForgeProcess(pid int) (bool, error) {
	return true, nil
}
