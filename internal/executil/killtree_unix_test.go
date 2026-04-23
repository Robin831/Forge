//go:build !windows

package executil

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestKillProcessTree_ReapsDetachedChildren verifies that KillProcessTree kills
// a grandchild process that has outlived its immediate parent. The test sets
// up: `sh -c 'sleep 30 & echo $!; wait'`, grabs the grandchild PID from stdout,
// kills the shell via KillProcessTree, and then confirms the sleep process is
// gone. This mirrors the real-world failure mode where a build script spawns a
// background http-server that the parent shell does not wait for.
func TestKillProcessTree_ReapsDetachedChildren(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The shell prints the PID of the backgrounded sleep, then waits for it so
	// the shell itself does not exit before we can kill the tree.
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo $!; wait")
	SetProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start shell: %v", err)
	}

	// Read the grandchild PID that the shell printed.
	buf := make([]byte, 64)
	n, err := stdout.Read(buf)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("read child pid: %v", err)
	}
	childPIDStr := ""
	for _, b := range buf[:n] {
		if b == '\n' || b == '\r' {
			break
		}
		childPIDStr += string(b)
	}
	childPID, err := strconv.Atoi(childPIDStr)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("parse child pid %q: %v", childPIDStr, err)
	}

	// Sanity check: sleep should be alive right now.
	if !processExists(childPID) {
		_ = cmd.Process.Kill()
		t.Fatalf("grandchild PID %d unexpectedly absent before kill", childPID)
	}

	// Kill the entire tree.
	if err := KillProcessTree(cmd); err != nil {
		t.Fatalf("KillProcessTree: %v", err)
	}
	_ = cmd.Wait()

	// The grandchild should be reaped shortly after. Allow a brief window for
	// the kernel to deliver SIGKILL and the process table to update.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processExists(childPID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild PID %d still alive after KillProcessTree", childPID)
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
