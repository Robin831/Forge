package daemon

import (
	"encoding/json"
	"testing"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleResumeBeadWithMessage_Validation covers the handler-level guards of
// the "resume_bead_with_message" IPC verb: malformed payload and a missing bead
// id are rejected before the ResumeBeadWithMessage entrypoint is reached.
func TestHandleResumeBeadWithMessage_Validation(t *testing.T) {
	d, _ := newQueueActionDaemon(t, "forge-a")

	t.Run("invalid payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "resume_bead_with_message", Payload: []byte("not json")})
		assert.Equal(t, "error", resp.Type)
		assert.Contains(t, steerMsg(t, resp), "invalid resume_bead_with_message payload")
	})

	t.Run("missing bead_id", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.ResumeBeadWithMessagePayload{Message: "keep going"})
		resp := d.handleIPC(ipc.Command{Type: "resume_bead_with_message", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		assert.Contains(t, steerMsg(t, resp), "bead_id is required")
	})
}

// TestHandleResumeBeadWithMessage_PropagatesEntrypointError verifies the handler
// surfaces the ResumeBeadWithMessage entrypoint's actionable errors verbatim.
// A live control handle makes the entrypoint reject the request (the caller
// should use resume, not resume-with-message).
func TestHandleResumeBeadWithMessage_PropagatesEntrypointError(t *testing.T) {
	d, _ := newQueueActionDaemon(t, "forge-a")
	d.registerControlHandle("BD-LIVE", newControlHandle("worker-live"))

	payload, _ := json.Marshal(ipc.ResumeBeadWithMessagePayload{BeadID: "BD-LIVE", Message: "keep going"})
	resp := d.handleIPC(ipc.Command{Type: "resume_bead_with_message", Payload: payload})
	assert.Equal(t, "error", resp.Type)
	assert.Contains(t, steerMsg(t, resp), "already has a live pipeline")
}

// TestHandleResumeBeadWithMessage_NoResumableWorker verifies a bead with no
// resumable worker row (no recorded branch + session) is rejected with the
// entrypoint's actionable error, surfaced through the handler.
func TestHandleResumeBeadWithMessage_NoResumableWorker(t *testing.T) {
	d, _ := newQueueActionDaemon(t, "forge-a")

	payload, _ := json.Marshal(ipc.ResumeBeadWithMessagePayload{BeadID: "BD-NONE", Message: "keep going"})
	resp := d.handleIPC(ipc.Command{Type: "resume_bead_with_message", Payload: payload})
	assert.Equal(t, "error", resp.Type)
	assert.Contains(t, steerMsg(t, resp), "no resumable worker row")

	// The rejected call must not leave a control handle or in-flight reservation.
	_, hasCtrl := d.lookupControlHandle("BD-NONE")
	assert.False(t, hasCtrl, "no control handle should linger after a rejected resume")
	_, inFlight := d.activeBeads.Load("BD-NONE")
	assert.False(t, inFlight, "no in-flight reservation should linger after a rejected resume")
}

// TestResumeBeadWithMessageResponse_Shape documents the success response shape
// so a change to ipc.ResumeBeadWithMessageResponse is caught here rather than in
// the SPA. It marshals/unmarshals the response the handler emits.
func TestResumeBeadWithMessageResponse_Shape(t *testing.T) {
	data, err := json.Marshal(ipc.ResumeBeadWithMessageResponse{
		BeadID:   "BD-1",
		WorkerID: "worker-1",
		Message:  "ok",
	})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, "BD-1", got["bead_id"])
	assert.Equal(t, "worker-1", got["worker_id"])
	assert.Equal(t, "ok", got["message"])
}
