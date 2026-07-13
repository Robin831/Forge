//go:build !windows

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustJSON marshals v into a json.RawMessage for use as an IPC response payload.
func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}

// pongResponse is the reply a live daemon gives to a ping.
func pongResponse(t *testing.T) ipc.Response {
	return ipc.Response{Type: "pong", Payload: mustJSON(t, map[string]string{"message": "pong"})}
}

// startStubDaemon spins up a real ipc.Server listening on the socket path
// derived from $HOME (which the caller must have pointed at a temp dir), wired
// to the given command handler. It blocks until the socket answers a ping so
// callers can rely on liveness immediately, and tears the server down on
// cleanup. This lets tests exercise IsRunning()'s socket-authoritative path
// without a full daemon.
func startStubDaemon(t *testing.T, handler ipc.CommandHandler) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".forge"), 0o755))

	srv := ipc.NewServer()
	srv.OnCommand(handler)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Start(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		srv.Close()
		<-done
	})

	// Wait for the listener to come up and answer a ping.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ipc.Ping() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("stub daemon socket never answered a ping")
}

func TestHandleIPC_PingAnswersPong(t *testing.T) {
	// The ping handler must not touch d.db or any other field, so a bare
	// Daemon is enough — this guards the cheap, dependency-free contract that
	// makes the socket a safe liveness probe.
	resp := (&Daemon{}).handleIPC(ipc.Command{Type: "ping"})
	assert.Equal(t, "pong", resp.Type)
}

func TestIsRunning_SocketAliveNoPidfile_ReportsRunning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	startStubDaemon(t, func(cmd ipc.Command) ipc.Response {
		if cmd.Type == "ping" {
			return pongResponse(t)
		}
		return ipc.Response{Type: "ok"}
	})

	pid, running := IsRunning()
	assert.True(t, running, "a daemon answering the socket must report Running even with no pidfile")
	assert.Equal(t, 0, pid, "no pidfile → diagnostic PID is 0, liveness still true")
}

func TestIsRunning_SocketAliveDoesNotDeleteLivePidfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".forge"), 0o755))

	// Pidfile points at a live but non-forge process (this test binary). The
	// staleness heuristic would delete such a pidfile if it ran — probing the
	// socket first must prevent that for a live daemon.
	pidPath := filepath.Join(home, ".forge", PIDFileName)
	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644))

	startStubDaemon(t, func(cmd ipc.Command) ipc.Response {
		if cmd.Type == "ping" {
			return pongResponse(t)
		}
		return ipc.Response{Type: "ok"}
	})

	pid, running := IsRunning()
	assert.True(t, running)
	assert.Equal(t, os.Getpid(), pid, "reports the pidfile PID for diagnostics")

	_, err := os.Stat(pidPath)
	assert.NoError(t, err, "a live daemon's pidfile must never be deleted by the staleness heuristic")
}

func TestIsRunning_SocketDeadStalePidfile_ReportsNotRunning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".forge"), 0o755))

	// No socket listener; pidfile holds a PID well above pid_max that almost
	// certainly does not exist.
	require.NoError(t, os.WriteFile(filepath.Join(home, ".forge", PIDFileName), []byte("4194303"), 0o644))

	pid, running := IsRunning()
	assert.Equal(t, 0, pid)
	assert.False(t, running, "no socket + a dead pidfile PID must report not-running")
}

func TestIsRunning_NoSocketNoPidfile_ReportsNotRunning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	pid, running := IsRunning()
	assert.Equal(t, 0, pid)
	assert.False(t, running)
}

// TestPauseResumeWorkWithDeletedPidfileOverSocket mirrors the CLI gate used by
// `forge pause`/`forge resume` (dispatch.go: refuse if IsRunning() is false)
// and proves both commands round-trip over the socket when the pidfile has
// been deleted — the exact scenario from the bead's acceptance criteria.
func TestPauseResumeWorkWithDeletedPidfileOverSocket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var gotPause, gotResume bool
	startStubDaemon(t, func(cmd ipc.Command) ipc.Response {
		switch cmd.Type {
		case "ping":
			return pongResponse(t)
		case "pause_dispatch":
			gotPause = true
			return ipc.Response{Type: "ok", Payload: mustJSON(t, map[string]string{"message": "dispatch paused"})}
		case "resume_dispatch":
			gotResume = true
			return ipc.Response{Type: "ok", Payload: mustJSON(t, map[string]string{"message": "dispatch resumed"})}
		}
		return ipc.Response{Type: "error"}
	})

	// No pidfile exists; the CLI gate must still see the daemon as running.
	_, running := IsRunning()
	require.True(t, running, "daemon must be seen as running via socket despite a missing pidfile")

	client, err := ipc.NewClient()
	require.NoError(t, err)
	defer client.Close()

	resp, err := client.Send(ipc.Command{Type: "pause_dispatch"})
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Type)
	assert.True(t, gotPause, "pause_dispatch must reach the daemon over the socket")

	resp, err = client.Send(ipc.Command{Type: "resume_dispatch"})
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Type)
	assert.True(t, gotResume, "resume_dispatch must reach the daemon over the socket")
}

func TestStop_NoDaemon_ReturnsError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	err := Stop()
	require.Error(t, err, "Stop must fail when no daemon is running")
	assert.Contains(t, err.Error(), "no daemon running")
}

func TestStop_SocketAliveNoPidfile_ShutsDownOverSocket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var gotShutdown bool
	startStubDaemon(t, func(cmd ipc.Command) ipc.Response {
		switch cmd.Type {
		case "ping":
			return pongResponse(t)
		case "shutdown":
			gotShutdown = true
			return ipc.Response{Type: "ok"}
		}
		return ipc.Response{Type: "error"}
	})

	// No pidfile: there is no SIGINT target, so Stop must shut the daemon down
	// over the socket.
	require.NoError(t, Stop())
	assert.True(t, gotShutdown, "Stop must route shutdown over the socket when the pidfile is absent")
}

// TestStop_SocketAlivePresentStalePidfile_ShutsDownOverSocket is the
// regression guard for the review feedback: a present-but-stale pidfile (here a
// dead PID left after a crash) must NOT be SIGINT'd. Stop must recognise the
// pidfile as stale via pidfileProcessAlive and shut the live daemon down over
// the socket instead.
func TestStop_SocketAlivePresentStalePidfile_ShutsDownOverSocket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".forge"), 0o755))

	// Pidfile present but points at a PID above pid_max that cannot exist, so
	// Signal(0) fails and the pidfile is stale.
	pidPath := filepath.Join(home, ".forge", PIDFileName)
	require.NoError(t, os.WriteFile(pidPath, []byte("4194303"), 0o644))

	var gotShutdown bool
	startStubDaemon(t, func(cmd ipc.Command) ipc.Response {
		switch cmd.Type {
		case "ping":
			return pongResponse(t)
		case "shutdown":
			gotShutdown = true
			return ipc.Response{Type: "ok"}
		}
		return ipc.Response{Type: "error"}
	})

	require.NoError(t, Stop())
	assert.True(t, gotShutdown, "a present-but-stale pidfile must fall back to socket shutdown, not SIGINT the stale PID")
}
