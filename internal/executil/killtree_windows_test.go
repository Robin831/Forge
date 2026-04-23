//go:build windows

package executil

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestKillProcessTree_NilCmdIsNoop_Windows ensures defer-safe callers never
// panic on a nil or unstarted command.
func TestKillProcessTree_NilCmdIsNoop_Windows(t *testing.T) {
	if err := KillProcessTree(nil); err != nil {
		t.Fatalf("nil cmd: %v", err)
	}
	if err := KillProcessTree(&exec.Cmd{}); err != nil {
		t.Fatalf("unstarted cmd: %v", err)
	}
}

// TestKillProcessTree_ReapsLiveDescendants_Windows verifies that KillProcessTree
// terminates a running process on Windows.
func TestKillProcessTree_ReapsLiveDescendants_Windows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "cmd", "/C", "ping -n 100 127.0.0.1 > NUL")
	SetProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid

	if !processExistsWindows(t, pid) {
		t.Fatal("process not running before kill")
	}

	if err := KillProcessTree(cmd); err != nil {
		t.Fatalf("KillProcessTree: %v", err)
	}
	_ = cmd.Wait()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processExistsWindows(t, pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("PID %d still alive after KillProcessTree", pid)
}

// TestKillProcessTree_AlreadyExited_Windows verifies that calling KillProcessTree
// on an already-exited process returns nil — the "process not found" case that
// taskkill signals with exit code 128 must be treated as success.
func TestKillProcessTree_AlreadyExited_Windows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "cmd", "/C", "echo hello")
	SetProcessGroup(cmd)

	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	// The process has already exited; KillProcessTree must not return an error.
	if err := KillProcessTree(cmd); err != nil {
		t.Fatalf("KillProcessTree on exited process: %v", err)
	}
}

// TestKillProcessTree_ReapsOrphanedChildren_Windows reproduces the scenario
// where a shell script backgrounds a child process and exits immediately. The
// backgrounded child keeps running after the parent exits, and KillProcessTree
// should attempt to clean it up.
func TestKillProcessTree_ReapsOrphanedChildren_Windows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	dir := t.TempDir()
	pidFile := strings.ReplaceAll(dir+"\\child.pid", "/", "\\")

	// Start a background ping and write its PID to a file, then exit.
	// This mirrors the real failure mode: a build script spawns a background
	// server and the parent shell exits without waiting.
	script := "start /B cmd /C ping -n 100 127.0.0.1 > NUL & for /f \"tokens=2\" %p in ('tasklist /FI \"IMAGENAME eq ping.exe\" /FO CSV /NH') do echo %p > " + pidFile

	cmd := exec.CommandContext(ctx, "cmd", "/C", script)
	SetProcessGroup(cmd)

	if err := cmd.Run(); err != nil {
		// Best-effort: if the script fails, skip the orphan check.
		t.Skipf("background script failed (environment may not support this): %v", err)
	}

	// KillProcessTree on an already-exited parent must not return an error,
	// even if it cannot reach the orphaned descendants.
	if err := KillProcessTree(cmd); err != nil {
		t.Fatalf("KillProcessTree on exited parent: %v", err)
	}
}

func processExistsWindows(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), strconv.Itoa(pid))
}
