//go:build windows

package daemon

// processGroup returns the process group ID for the given PID.
// On Windows, Unix-style process groups are not supported;
// returns (pid, false) so callers fall back to single-PID signaling.
func processGroup(pid int) (int, bool) {
	return pid, false
}
