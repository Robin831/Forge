//go:build !windows

package kiln

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// inspectProcess returns a snapshot of the live process with the given pid.
//
// Liveness is signal 0, which is portable. The evidence that establishes
// ownership — start time and working directory — comes from /proc, so it is
// available on Linux (the platform Forge's daemon runs previews on) and absent
// on a Unix without /proc. That absence is deliberately not papered over: the
// resulting zero start time and empty cwd both fail the ownership check, so
// reconciliation there reports the stray and leaves it running rather than
// signalling a process it cannot identify.
func inspectProcess(pid int) (ProcessInfo, bool) {
	if pid <= 0 {
		return ProcessInfo{}, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return ProcessInfo{}, false
	}
	// Signal 0 tests existence without side effects.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return ProcessInfo{}, false
	}

	info := ProcessInfo{PID: pid, CwdSupported: true}
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		info.StartTime = processStartTime(data)
	}
	if target, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
		info.Cwd = target
	}
	return info, true
}

// terminateProcessGroup stops the process group led by pid: SIGTERM to the
// whole group, then a kill once grace has elapsed.
//
// The group rather than the leader, because a preview service is a shell line
// that forks (`npm run dev` → node → esbuild) and the children are what hold
// the port. Preview services are started with Setpgid, so the group id equals
// the recorded leader PID.
func terminateProcessGroup(pid int, grace time.Duration) error {
	if pid <= 0 {
		return nil
	}
	// Politely first: a dev server that traps SIGTERM closes its listening
	// socket, which is what makes the port immediately reusable.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return nil // nobody left in the group
		}
		// Anything else (EPERM, say) is reported by the kill below, which is
		// the step that actually has to succeed.
	}

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !processGroupAlive(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return executil.KillProcessGroup(pid)
}

// processGroupAlive reports whether any process remains in the group led by pid.
func processGroupAlive(pid int) bool {
	return syscall.Kill(-pid, syscall.Signal(0)) == nil
}

// processStartTime converts /proc/<pid>/stat into the process's wall-clock
// start time, or the zero time when it cannot be derived.
func processStartTime(stat []byte) time.Time {
	ticks := statStartTicks(stat)
	boot, hz := bootTimeSeconds(), clockTicks()
	if ticks == 0 || boot == 0 || hz == 0 {
		return time.Time{}
	}
	return time.Unix(boot+int64(ticks/uint64(hz)), 0)
}

// statStartTicks extracts the start time in clock ticks since boot (field 22)
// from /proc/<pid>/stat. The comm field (field 2) is parenthesized and may
// itself contain spaces and parentheses, so field splitting resumes after the
// final ')'.
func statStartTicks(data []byte) uint64 {
	s := string(data)
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen+1 >= len(s) {
		return 0
	}
	fields := strings.Fields(s[rparen+1:])
	// fields[0] is field 3 (state), so field N maps to index N-3:
	// starttime = field 22 -> index 19.
	if len(fields) <= 19 {
		return 0
	}
	ticks, _ := strconv.ParseUint(fields[19], 10, 64)
	return ticks
}

// bootTimeSeconds returns the system boot time in seconds since the Unix epoch,
// read from /proc/stat's "btime" line. Returns 0 when unavailable.
func bootTimeSeconds() int64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			v, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				return 0
			}
			return v
		}
	}
	return 0
}

// clockTicks returns the clock ticks per second (USER_HZ) used to convert a
// process start time from ticks to seconds. Linux fixes USER_HZ at 100 on
// effectively all supported architectures and there is no cgo-free way to read
// sysconf(_SC_CLK_TCK), so 100 is assumed — the same assumption the worker
// orphan sweep in internal/shutdown makes.
func clockTicks() int64 { return 100 }
