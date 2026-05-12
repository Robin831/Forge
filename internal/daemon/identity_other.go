//go:build !linux

package daemon

import (
	"errors"
	"time"
)

// errNoProcStartTime is returned by the non-Linux procStartTime stub. The
// staleness comparison in IsRunning() this enables only matters in a
// container PID namespace, which is a Linux concern.
var errNoProcStartTime = errors.New("procStartTime not supported on this platform")

// isForgeProcess is a no-op on non-Linux platforms. Windows relies on the
// named pipe as a liveness proxy in IsRunning(), and macOS keeps the
// historical behavior of trusting the Signal(0) liveness check. Returning
// true preserves the prior semantics: IsRunning() proceeds as if the
// liveness check were authoritative.
func isForgeProcess(pid int) (bool, error) {
	return true, nil
}

// procStartTime is a no-op on non-Linux platforms. The PID-namespace
// collision this guards against is a Linux container concern; on macOS
// dev machines and Windows hosts, plain PID reuse without container
// restarts is statistically negligible. Returning an error signals
// IsRunning() to skip the staleness comparison.
func procStartTime(pid int) (time.Time, error) {
	return time.Time{}, errNoProcStartTime
}
