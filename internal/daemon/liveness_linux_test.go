//go:build linux

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStop_LiveForgePidfile_SignalsPID proves that when the pidfile points at a
// verified-live forge process and the socket is silent, Stop signals that PID
// directly (the graceful SIGINT path) rather than reaching for the socket. A
// real child process stands in for the daemon, and a procfs fixture makes
// isForgeProcess treat it as a forge binary.
func TestStop_LiveForgePidfile_SignalsPID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".forge"), 0o755))

	// Spawn a real, long-lived child to receive the signal.
	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	pid := cmd.Process.Pid

	// Point isForgeProcess/procStartTime at a fixture procfs where this PID
	// looks like a live forge binary. The fixture dir mtime stands in for the
	// process start time.
	fixture := t.TempDir()
	origProcFS := procFS
	procFS = fixture
	t.Cleanup(func() { procFS = origProcFS })

	pidDir := filepath.Join(fixture, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(pidDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "comm"), []byte("forge\n"), 0o644))

	// Write the pidfile after the fixture so its mtime does not predate the
	// process start time (which would make it look like a stale incarnation).
	pidPath := filepath.Join(home, ".forge", PIDFileName)
	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0o644))

	// No socket listener exists, so a bug that routed to the socket would
	// surface as an error instead of signalling the child.
	require.NoError(t, Stop())

	// Confirm the child actually received SIGINT.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		require.Error(t, err, "child must have been terminated by the signal")
		exitErr, ok := err.(*exec.ExitError)
		require.True(t, ok, "expected an *exec.ExitError, got %T", err)
		ws, ok := exitErr.Sys().(syscall.WaitStatus)
		require.True(t, ok)
		assert.True(t, ws.Signaled(), "child must exit via a signal")
		assert.Equal(t, syscall.SIGINT, ws.Signal(), "Stop must send SIGINT to a live forge pidfile PID")
	case <-time.After(3 * time.Second):
		t.Fatal("child was never signalled — Stop did not take the SIGINT path")
	}
}
