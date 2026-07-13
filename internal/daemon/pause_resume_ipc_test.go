package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerWorkerWithStatus inserts a worker row in the given status and
// registers a fresh control handle for its bead, returning the handle so tests
// can assert on the pause/resume mailboxes.
func registerWorkerWithStatus(t *testing.T, d *Daemon, bead, workerID string, status state.WorkerStatus) *controlHandle {
	t.Helper()
	require.NoError(t, d.db.InsertWorker(&state.Worker{
		ID:        workerID,
		BeadID:    bead,
		Anvil:     "anvil-1",
		Status:    status,
		StartedAt: time.Now(),
	}))
	ctrl := newControlHandle(workerID)
	d.registerControlHandle(bead, ctrl)
	return ctrl
}

func TestHandlePauseBead_Validation(t *testing.T) {
	d := newSteerTestDaemon(t)

	t.Run("invalid payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "pause_bead", Payload: []byte("not json")})
		assert.Equal(t, "error", resp.Type)
		assert.Contains(t, steerMsg(t, resp), "invalid pause_bead payload")
	})

	t.Run("missing bead_id", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.PauseBeadPayload{})
		resp := d.handleIPC(ipc.Command{Type: "pause_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		assert.Contains(t, steerMsg(t, resp), "bead_id is required")
	})

	t.Run("bead not found", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.PauseBeadPayload{BeadID: "BD-NONE"})
		resp := d.handleIPC(ipc.Command{Type: "pause_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		assert.Contains(t, steerMsg(t, resp), "bead BD-NONE not found")
	})
}

func TestHandlePauseBead_Success(t *testing.T) {
	d := newSteerTestDaemon(t)
	const bead = "BD-RUN"
	ctrl := registerWorkerWithStatus(t, d, bead, "w-run", state.WorkerRunning)

	payload, _ := json.Marshal(ipc.PauseBeadPayload{BeadID: bead})
	resp := d.handleIPC(ipc.Command{Type: "pause_bead", Payload: payload})
	require.Equal(t, "ok", resp.Type)

	var pr ipc.PauseBeadResponse
	require.NoError(t, json.Unmarshal(resp.Payload, &pr))
	assert.Equal(t, bead, pr.BeadID)
	assert.Equal(t, string(state.WorkerPaused), pr.Status)

	// The pause request must have been dispatched into the control handle.
	select {
	case <-ctrl.pause:
	default:
		t.Fatal("expected a pause signal on the control handle")
	}
}

func TestHandlePauseBead_IllegalTransition(t *testing.T) {
	d := newSteerTestDaemon(t)
	const bead = "BD-REVIEW"
	// A reviewing worker is not running, so pause is an illegal transition.
	ctrl := registerWorkerWithStatus(t, d, bead, "w-review", state.WorkerReviewing)

	payload, _ := json.Marshal(ipc.PauseBeadPayload{BeadID: bead})
	resp := d.handleIPC(ipc.Command{Type: "pause_bead", Payload: payload})
	assert.Equal(t, "error", resp.Type)
	assert.Contains(t, steerMsg(t, resp), "cannot be paused from status")

	// No pause signal should have been dispatched on the rejected transition.
	select {
	case <-ctrl.pause:
		t.Fatal("pause must not be dispatched on an illegal transition")
	default:
	}
}

func TestHandlePauseBead_AlreadyPending(t *testing.T) {
	d := newSteerTestDaemon(t)
	const bead = "BD-DUP"
	ctrl := registerWorkerWithStatus(t, d, bead, "w-dup", state.WorkerRunning)
	// Pre-fill the single pause slot so the next request cannot be dispatched.
	require.True(t, ctrl.requestPause())

	payload, _ := json.Marshal(ipc.PauseBeadPayload{BeadID: bead})
	resp := d.handleIPC(ipc.Command{Type: "pause_bead", Payload: payload})
	assert.Equal(t, "error", resp.Type)
	assert.Contains(t, steerMsg(t, resp), "pause is already pending")
}

func TestHandleResumeBead_Validation(t *testing.T) {
	d := newSteerTestDaemon(t)

	t.Run("invalid payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "resume_bead", Payload: []byte("not json")})
		assert.Equal(t, "error", resp.Type)
		assert.Contains(t, steerMsg(t, resp), "invalid resume_bead payload")
	})

	t.Run("missing bead_id", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.ResumeBeadPayload{Message: "go"})
		resp := d.handleIPC(ipc.Command{Type: "resume_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		assert.Contains(t, steerMsg(t, resp), "bead_id is required")
	})

	t.Run("bead not found", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.ResumeBeadPayload{BeadID: "BD-NONE"})
		resp := d.handleIPC(ipc.Command{Type: "resume_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		assert.Contains(t, steerMsg(t, resp), "bead BD-NONE not found")
	})
}

func TestHandleResumeBead_SuccessCustomMessage(t *testing.T) {
	d := newSteerTestDaemon(t)
	const bead = "BD-PAUSED"
	ctrl := registerWorkerWithStatus(t, d, bead, "w-paused", state.WorkerPaused)

	payload, _ := json.Marshal(ipc.ResumeBeadPayload{BeadID: bead, Message: "focus on the tests"})
	resp := d.handleIPC(ipc.Command{Type: "resume_bead", Payload: payload})
	require.Equal(t, "ok", resp.Type)

	var rr ipc.ResumeBeadResponse
	require.NoError(t, json.Unmarshal(resp.Payload, &rr))
	assert.Equal(t, bead, rr.BeadID)
	assert.Equal(t, string(state.WorkerRunning), rr.Status)

	select {
	case msg := <-ctrl.resume:
		assert.Equal(t, "focus on the tests", msg)
	default:
		t.Fatal("expected the resume message on the control handle")
	}
}

func TestHandleResumeBead_DefaultMessage(t *testing.T) {
	d := newSteerTestDaemon(t)
	const bead = "BD-PAUSED-DEFAULT"
	ctrl := registerWorkerWithStatus(t, d, bead, "w-paused-default", state.WorkerPaused)

	// Whitespace-only message must fall back to the default resume prompt.
	payload, _ := json.Marshal(ipc.ResumeBeadPayload{BeadID: bead, Message: "   "})
	resp := d.handleIPC(ipc.Command{Type: "resume_bead", Payload: payload})
	require.Equal(t, "ok", resp.Type)

	select {
	case msg := <-ctrl.resume:
		assert.Equal(t, DefaultResumeMessage, msg)
	default:
		t.Fatal("expected the default resume message on the control handle")
	}
}

func TestHandleResumeBead_IllegalTransition(t *testing.T) {
	d := newSteerTestDaemon(t)
	const bead = "BD-STILL-RUNNING"
	// A running worker is not paused, so resume is an illegal transition.
	ctrl := registerWorkerWithStatus(t, d, bead, "w-still-running", state.WorkerRunning)

	payload, _ := json.Marshal(ipc.ResumeBeadPayload{BeadID: bead})
	resp := d.handleIPC(ipc.Command{Type: "resume_bead", Payload: payload})
	assert.Equal(t, "error", resp.Type)
	assert.Contains(t, steerMsg(t, resp), "cannot be resumed from status")

	select {
	case <-ctrl.resume:
		t.Fatal("resume must not be dispatched on an illegal transition")
	default:
	}
}
