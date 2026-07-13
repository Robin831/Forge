package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSteerTestDaemon builds a minimal Daemon backed by a real on-disk state.db
// so handleSteerBead can look up worker rows via GetWorker.
func newSteerTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "forge-steer-*")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.cfg.Store(&config.Config{})
	return d
}

func steerMsg(t *testing.T, resp ipc.Response) string {
	t.Helper()
	var m map[string]string
	require.NoError(t, json.Unmarshal(resp.Payload, &m))
	return m["message"]
}

func TestHandleSteerBead_Validation(t *testing.T) {
	d := newSteerTestDaemon(t)

	t.Run("invalid payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "steer_bead", Payload: []byte("not json")})
		assert.Equal(t, "error", resp.Type)
		assert.Contains(t, steerMsg(t, resp), "invalid steer_bead payload")
	})

	t.Run("missing bead_id", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.SteerBeadPayload{Message: "go left"})
		resp := d.handleIPC(ipc.Command{Type: "steer_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		assert.Contains(t, steerMsg(t, resp), "bead_id is required")
	})

	t.Run("empty message", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.SteerBeadPayload{BeadID: "BD-1", Message: "   "})
		resp := d.handleIPC(ipc.Command{Type: "steer_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		assert.Contains(t, steerMsg(t, resp), "steer message must not be empty")
	})

	t.Run("no active pipeline", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.SteerBeadPayload{BeadID: "BD-NONE", Message: "go left"})
		resp := d.handleIPC(ipc.Command{Type: "steer_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		assert.Contains(t, steerMsg(t, resp), "no active pipeline for bead BD-NONE")
	})
}

func TestHandleSteerBead_NonClaudeSession(t *testing.T) {
	d := newSteerTestDaemon(t)
	const bead = "BD-GEMINI"

	// A worker with a recorded non-Claude model and no session_id is positively
	// non-Claude and cannot be resumed by a steer.
	require.NoError(t, d.db.InsertWorker(&state.Worker{
		ID:        "w-gemini",
		BeadID:    bead,
		Anvil:     "anvil-1",
		Status:    state.WorkerRunning,
		StartedAt: time.Now(),
	}))
	require.NoError(t, d.db.UpdateWorkerSession("w-gemini", "", "gemini-2.5-pro"))

	ctrl := newControlHandle("w-gemini")
	d.registerControlHandle(bead, ctrl)

	payload, _ := json.Marshal(ipc.SteerBeadPayload{BeadID: bead, Message: "go left"})
	resp := d.handleIPC(ipc.Command{Type: "steer_bead", Payload: payload})
	assert.Equal(t, "error", resp.Type)
	assert.Contains(t, steerMsg(t, resp), "not running a Claude session")

	// The steer must not have been enqueued on a rejected non-Claude session.
	select {
	case <-ctrl.steer:
		t.Fatal("steer message should not be enqueued for a non-Claude session")
	default:
	}
}

func TestHandleSteerBead_SuccessModeA(t *testing.T) {
	d := newSteerTestDaemon(t)
	const bead = "BD-CLAUDE"

	require.NoError(t, d.db.InsertWorker(&state.Worker{
		ID:        "w-claude",
		BeadID:    bead,
		Anvil:     "anvil-1",
		Status:    state.WorkerRunning,
		StartedAt: time.Now(),
	}))
	require.NoError(t, d.db.UpdateWorkerSession("w-claude", "sess-1", "claude-opus-4-6"))

	ctrl := newControlHandle("w-claude")
	// A live spawn is running: the steer must be labelled mode A. Steering must
	// NOT cancel anything at the daemon layer — the pipeline goroutine interrupts
	// only the current spawn — so there is no interrupt func to observe here.
	ctrl.setLiveSpawn(true)
	d.registerControlHandle(bead, ctrl)

	payload, _ := json.Marshal(ipc.SteerBeadPayload{BeadID: bead, Message: "also update the README"})
	resp := d.handleIPC(ipc.Command{Type: "steer_bead", Payload: payload})
	assert.Equal(t, "ok", resp.Type)
	assert.Contains(t, steerMsg(t, resp), "mode A")

	select {
	case msg := <-ctrl.steer:
		assert.Equal(t, "also update the README", msg)
	default:
		t.Fatal("expected the steer message in the mailbox")
	}
}

func TestHandleSteerBead_SuccessModeB(t *testing.T) {
	d := newSteerTestDaemon(t)
	const bead = "BD-CLAUDE-B"

	// No worker session recorded yet (a just-started Claude spawn) — treated as
	// steerable. With no interrupt wired, the steer is queued for the next spawn
	// (mode B).
	require.NoError(t, d.db.InsertWorker(&state.Worker{
		ID:        "w-claude-b",
		BeadID:    bead,
		Anvil:     "anvil-1",
		Status:    state.WorkerRunning,
		StartedAt: time.Now(),
	}))

	ctrl := newControlHandle("w-claude-b")
	d.registerControlHandle(bead, ctrl)

	payload, _ := json.Marshal(ipc.SteerBeadPayload{BeadID: bead, Message: "prioritise caching"})
	resp := d.handleIPC(ipc.Command{Type: "steer_bead", Payload: payload})
	assert.Equal(t, "ok", resp.Type)
	assert.Contains(t, steerMsg(t, resp), "mode B")

	select {
	case msg := <-ctrl.steer:
		assert.Equal(t, "prioritise caching", msg)
	default:
		t.Fatal("expected the steer message in the mailbox")
	}
}

func TestHandleSteerBead_MailboxFull(t *testing.T) {
	d := newSteerTestDaemon(t)
	const bead = "BD-FULL"

	ctrl := newControlHandle("w-full")
	for i := 0; i < steerMailboxSize; i++ {
		require.True(t, ctrl.pushSteer("m"))
	}
	d.registerControlHandle(bead, ctrl)

	payload, _ := json.Marshal(ipc.SteerBeadPayload{BeadID: bead, Message: "overflow"})
	resp := d.handleIPC(ipc.Command{Type: "steer_bead", Payload: payload})
	assert.Equal(t, "error", resp.Type)
	assert.Contains(t, steerMsg(t, resp), "mailbox is full")
}
