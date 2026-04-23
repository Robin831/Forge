//go:build !windows

package executil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestKillProcessTree_ReapsOrphanedChildren reproduces the exact scenario the
// bead reports: a shell script backgrounds a long-running process and exits
// immediately without waiting for it. By the time KillProcessTree runs, the
// parent shell has already been reaped by cmd.Wait() — which means the naive
// approach of calling syscall.Getpgid(pid) fails with ESRCH because the
// leader PID no longer exists. The group itself, however, still lives on
// because the orphaned grandchild keeps it alive, and that is what must be
// signalled.
//
// This mirrors the real failure mode for Fhi.Metadata-2dj8t: a storybook smoke
// test spawned `npx http-server storybook-static &` and the shell exited,
// leaving the http-server running for 17h and holding the worktree's client/
// directory open on Windows.
func TestKillProcessTree_ReapsOrphanedChildren(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Use a temp file for the child PID handoff: the parent shell exits
	// immediately so reading from its stdout pipe races the exit.
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")

	// Background a sleep and exit without waiting. This is precisely the
	// storybook smoke-test pattern (`npx http-server storybook-static &`).
	script := "sleep 30 & echo $! > " + pidFile
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	SetProcessGroup(cmd)

	if err := cmd.Run(); err != nil {
		t.Fatalf("run shell: %v", err)
	}
	// cmd.Run returned — the parent shell has exited and been reaped. The
	// backgrounded sleep is now an orphan, reparented to init/subreaper.

	childPID := readPIDFile(t, pidFile)

	// Sanity check: the orphan is still alive despite the parent being gone.
	// If this fails the test is not exercising the intended scenario.
	if !processExists(childPID) {
		t.Fatalf("orphan PID %d unexpectedly absent after parent exited", childPID)
	}

	// The critical call: parent is dead and reaped, so Getpgid(pid) returns
	// ESRCH. The implementation must still reach the orphan via the process
	// group (pgid == pid by the Setpgid contract).
	if err := KillProcessTree(cmd); err != nil {
		t.Fatalf("KillProcessTree: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(childPID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Last-ditch cleanup so a failing test does not leak a 30s sleep.
	_ = syscall.Kill(childPID, syscall.SIGKILL)
	t.Fatalf("orphaned PID %d still alive after KillProcessTree", childPID)
}

// TestKillProcessTree_ReapsLiveDescendants verifies the other half of the
// contract: when the parent is still running, KillProcessTree also reaps
// descendants that are members of the group. This is the easier case where
// the process group is trivially resolvable.
func TestKillProcessTree_ReapsLiveDescendants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo $!; wait")
	SetProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start shell: %v", err)
	}

	buf := make([]byte, 64)
	n, err := stdout.Read(buf)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("read child pid: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimRight(string(buf[:n]), "\r\n"))
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("parse child pid: %v", err)
	}

	if !processExists(childPID) {
		_ = cmd.Process.Kill()
		t.Fatalf("grandchild PID %d unexpectedly absent before kill", childPID)
	}

	if err := KillProcessTree(cmd); err != nil {
		t.Fatalf("KillProcessTree: %v", err)
	}
	_ = cmd.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(childPID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(childPID, syscall.SIGKILL)
	t.Fatalf("grandchild PID %d still alive after KillProcessTree", childPID)
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	// The shell writes the PID asynchronously before exiting; give the
	// filesystem a moment to flush on slow CI.
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr != nil {
				t.Fatalf("parse pid %q: %v", string(data), convErr)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid file %s never became readable: %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestKillProcessTree_NilCmdIsNoop ensures defer-safe callers never panic on a
// nil or unstarted command.
func TestKillProcessTree_NilCmdIsNoop(t *testing.T) {
	if err := KillProcessTree(nil); err != nil {
		t.Fatalf("nil cmd: %v", err)
	}
	if err := KillProcessTree(&exec.Cmd{}); err != nil {
		t.Fatalf("unstarted cmd: %v", err)
	}
}

func processExists(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
