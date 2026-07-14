//go:build !windows

package shutdown

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// platformSupportsProcessCwd is true on Unix: /proc exposes each process's
// working directory via /proc/<pid>/cwd, so the orphan sweep can require a
// cwd-in-worktree match before reaping a process (and can run the secondary
// unrecorded-PID sweep). See identifyForgeOwnedProcesses.
const platformSupportsProcessCwd = true

// listProcessesPlatform walks /proc to build a process table for ownership
// verification. Best-effort: unreadable entries are skipped.
func listProcessesPlatform() ([]procInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	boot, clk := bootTimeSeconds(), clockTicks()
	procs := make([]procInfo, 0, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		p := procInfo{pid: pid}

		if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
			p.argv = splitCmdline(data)
		}
		if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			pgid, start := parseStat(data)
			p.pgid = pgid
			if start > 0 && boot > 0 && clk > 0 {
				p.startTime = time.Unix(boot+int64(start/uint64(clk)), 0)
			}
		}
		if target, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
			p.cwd = target
		}
		procs = append(procs, p)
	}
	return procs, nil
}

// splitCmdline splits a NUL-delimited /proc/<pid>/cmdline into argv.
func splitCmdline(data []byte) []string {
	data = bytes.TrimRight(data, "\x00")
	if len(data) == 0 {
		return nil
	}
	parts := bytes.Split(data, []byte{0})
	argv := make([]string, 0, len(parts))
	for _, part := range parts {
		argv = append(argv, string(part))
	}
	return argv
}

// parseStat extracts the process group id (field 5) and the start time in clock
// ticks since boot (field 22) from /proc/<pid>/stat. The comm field (field 2)
// is wrapped in parentheses and may itself contain spaces and parentheses, so
// parsing of the space-delimited fields resumes after the final ')'.
func parseStat(data []byte) (pgid int, starttime uint64) {
	s := string(data)
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen+1 >= len(s) {
		return 0, 0
	}
	fields := strings.Fields(s[rparen+1:])
	// fields[0] is field 3 (state); field N maps to index N-3.
	// pgrp = field 5 -> index 2; starttime = field 22 -> index 19.
	if len(fields) > 2 {
		pgid, _ = strconv.Atoi(fields[2])
	}
	if len(fields) > 19 {
		starttime, _ = strconv.ParseUint(fields[19], 10, 64)
	}
	return pgid, starttime
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

// clockTicks returns the number of clock ticks per second (USER_HZ), used to
// convert a process start time from ticks to seconds. Linux fixes USER_HZ at
// 100 on effectively all supported architectures, and there is no cgo-free way
// to read sysconf(_SC_CLK_TCK), so 100 is assumed.
func clockTicks() int64 {
	return 100
}
