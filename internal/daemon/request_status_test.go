package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/ipc"
)

// newRequestStatusDaemon builds the minimal Daemon needed to exercise the
// request_status command: a request tracker and a logger, nothing else.
func newRequestStatusDaemon() *Daemon {
	return &Daemon{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		reqTracker: *ipc.NewRequestTracker("test-"),
	}
}

func requestStatus(t *testing.T, d *Daemon, requestID string) ipc.RequestStatusResponse {
	t.Helper()
	payload, err := json.Marshal(ipc.RequestStatusPayload{RequestID: requestID})
	require.NoError(t, err)
	resp := d.handleIPC(ipc.Command{Type: "request_status", Payload: payload})
	require.Equal(t, "ok", resp.Type, "request_status must answer ok, payload=%s", string(resp.Payload))
	var out ipc.RequestStatusResponse
	require.NoError(t, json.Unmarshal(resp.Payload, &out))
	return out
}

// TestRequestStatus_ResolvesAsyncFailure is the daemon half of the Forge-4r2n
// regression: a queued command whose background execution fails must resolve
// to a terminal error through its request_id, not vanish behind the
// acknowledgement.
func TestRequestStatus_ResolvesAsyncFailure(t *testing.T) {
	d := newRequestStatusDaemon()

	reqID, _ := d.reqTracker.Track()
	// While in flight the request reads as pending — neither success nor
	// failure may be claimed yet.
	assert.Equal(t, ipc.RequestStatePending, requestStatus(t, d, reqID).State)

	d.completeAsync(reqID, errorResponse("bd update failed: exit status 1"))

	got := requestStatus(t, d, reqID)
	assert.Equal(t, ipc.RequestStateError, got.State)
	assert.Equal(t, "bd update failed: exit status 1", got.Message)
	assert.NotEmpty(t, got.UpdatedAt)
}

func TestRequestStatus_ResolvesAsyncSuccess(t *testing.T) {
	d := newRequestStatusDaemon()

	reqID, _ := d.reqTracker.Track()
	d.completeAsync(reqID, okResponse(map[string]string{"message": "label \"forgeReady\" added"}))

	got := requestStatus(t, d, reqID)
	assert.Equal(t, ipc.RequestStateOK, got.State)
	assert.Equal(t, `label "forgeReady" added`, got.Message)
}

// TestRequestStatus_UnknownID checks that an evicted or bogus id is reported
// as unknown rather than as an error — the store is bounded, so a stale tab
// must not read a dropped record as a failed write.
func TestRequestStatus_UnknownID(t *testing.T) {
	d := newRequestStatusDaemon()

	got := requestStatus(t, d, "test-nonexistent")
	assert.Equal(t, ipc.RequestStateUnknown, got.State)
	assert.Equal(t, "test-nonexistent", got.RequestID)
}

func TestRequestStatus_RejectsEmptyAndInvalidPayloads(t *testing.T) {
	d := newRequestStatusDaemon()

	resp := d.handleIPC(ipc.Command{Type: "request_status", Payload: []byte("not json")})
	assert.Equal(t, "error", resp.Type)

	payload, _ := json.Marshal(ipc.RequestStatusPayload{RequestID: "  "})
	resp = d.handleIPC(ipc.Command{Type: "request_status", Payload: payload})
	assert.Equal(t, "error", resp.Type)
	var msg map[string]string
	_ = json.Unmarshal(resp.Payload, &msg)
	assert.Contains(t, msg["message"], "request_id is required")
}
