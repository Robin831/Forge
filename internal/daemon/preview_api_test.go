package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/kiln"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPreviewAPIDaemon builds a Daemon with a live request tracker and the given
// (possibly nil) preview manager already installed, which is what the IPC
// handlers read — they never construct a manager themselves.
func newPreviewAPIDaemon(t *testing.T, cfg *config.Config, mgr previewManager) *Daemon {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := &Daemon{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		runCtx:     ctx,
		reqTracker: *ipc.NewRequestTracker("test-"),
	}
	d.cfg.Store(cfg)
	if mgr != nil {
		d.previewMgr = mgr
	}
	return d
}

// awaitOutcome polls the request tracker until the queued command reaches a
// terminal state, so the assertions run against the async result rather than
// the acknowledgement.
func awaitOutcome(t *testing.T, d *Daemon, requestID string) ipc.RequestOutcome {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		outcome, ok := d.reqTracker.Outcome(requestID)
		if ok && outcome.State != ipc.RequestStatePending {
			return outcome
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("request %s never reached a terminal state", requestID)
	return ipc.RequestOutcome{}
}

func TestHandlePreviewStart_QueuesAndStarts(t *testing.T) {
	mgr := newFakePreviewManager()
	d := newPreviewAPIDaemon(t, previewConfig(true, nil), mgr)

	resp := d.handlePreviewStart(ipc.PreviewActionPayload{BeadID: "Forge-abc1", Anvil: "forge"})
	require.Equal(t, "queued", resp.Type)
	require.NotEmpty(t, resp.RequestID)

	outcome := awaitOutcome(t, d, resp.RequestID)
	assert.Equal(t, ipc.RequestStateOK, outcome.State)

	started := mgr.startedOptions()
	require.Len(t, started, 1)
	assert.Equal(t, kiln.StartOptions{
		BeadID:    "Forge-abc1",
		Anvil:     "forge",
		AnvilPath: "/tmp/forge",
		// An unset branch defaults to the bead's canonical forge branch.
		Branch: "forge/Forge-abc1",
	}, started[0])
}

func TestHandlePreviewStart_HonoursExplicitBranch(t *testing.T) {
	mgr := newFakePreviewManager()
	d := newPreviewAPIDaemon(t, previewConfig(true, nil), mgr)

	resp := d.handlePreviewStart(ipc.PreviewActionPayload{
		BeadID: "Forge-abc1", Anvil: "forge", Branch: "feature/Forge-abc1",
	})
	require.Equal(t, "queued", resp.Type)
	awaitOutcome(t, d, resp.RequestID)

	started := mgr.startedOptions()
	require.Len(t, started, 1)
	assert.Equal(t, "feature/Forge-abc1", started[0].Branch)
}

func TestHandlePreviewStart_ReportsManagerFailure(t *testing.T) {
	mgr := newFakePreviewManager()
	mgr.startErr = errors.New("preview limit reached")
	d := newPreviewAPIDaemon(t, previewConfig(true, nil), mgr)

	resp := d.handlePreviewStart(ipc.PreviewActionPayload{BeadID: "Forge-abc1", Anvil: "forge"})
	require.Equal(t, "queued", resp.Type)

	outcome := awaitOutcome(t, d, resp.RequestID)
	assert.Equal(t, ipc.RequestStateError, outcome.State)
	assert.Contains(t, outcome.Message, "preview limit reached")
}

func TestHandlePreviewStart_Rejections(t *testing.T) {
	disabledAnvil := previewConfig(true, boolPtr(false))

	tests := []struct {
		name    string
		cfg     *config.Config
		mgr     previewManager
		payload ipc.PreviewActionPayload
		wantMsg string
	}{
		{
			name:    "previews disabled",
			cfg:     previewConfig(false, nil),
			mgr:     nil,
			payload: ipc.PreviewActionPayload{BeadID: "Forge-abc1", Anvil: "forge"},
			wantMsg: "preview environments are disabled",
		},
		{
			name:    "missing bead id",
			cfg:     previewConfig(true, nil),
			mgr:     newFakePreviewManager(),
			payload: ipc.PreviewActionPayload{Anvil: "forge"},
			wantMsg: "bead_id is required",
		},
		{
			name:    "missing anvil",
			cfg:     previewConfig(true, nil),
			mgr:     newFakePreviewManager(),
			payload: ipc.PreviewActionPayload{BeadID: "Forge-abc1"},
			wantMsg: "anvil is required",
		},
		{
			name:    "unknown anvil",
			cfg:     previewConfig(true, nil),
			mgr:     newFakePreviewManager(),
			payload: ipc.PreviewActionPayload{BeadID: "Forge-abc1", Anvil: "nope"},
			wantMsg: `anvil "nope" not found`,
		},
		{
			name:    "anvil opted out",
			cfg:     disabledAnvil,
			mgr:     newFakePreviewManager(),
			payload: ipc.PreviewActionPayload{BeadID: "Forge-abc1", Anvil: "forge"},
			wantMsg: `previews are disabled for anvil "forge"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newPreviewAPIDaemon(t, tt.cfg, tt.mgr)
			resp := d.handlePreviewStart(tt.payload)
			require.Equal(t, "error", resp.Type)
			var body struct {
				Message string `json:"message"`
			}
			require.NoError(t, json.Unmarshal(resp.Payload, &body))
			assert.Contains(t, body.Message, tt.wantMsg)
			if fake, ok := tt.mgr.(*fakePreviewManager); ok {
				assert.Empty(t, fake.startedOptions(), "a rejected start must not reach the manager")
			}
		})
	}
}

func TestHandlePreviewStop_QueuesAndStops(t *testing.T) {
	mgr := newFakePreviewManager()
	d := newPreviewAPIDaemon(t, previewConfig(true, nil), mgr)

	resp := d.handlePreviewStop(ipc.PreviewActionPayload{BeadID: "Forge-abc1"})
	require.Equal(t, "queued", resp.Type)

	outcome := awaitOutcome(t, d, resp.RequestID)
	assert.Equal(t, ipc.RequestStateOK, outcome.State)
	assert.Equal(t, []string{"Forge-abc1"}, mgr.stoppedBeads())
}

func TestHandlePreviewStop_ReportsTeardownFailure(t *testing.T) {
	mgr := newFakePreviewManager()
	mgr.stopErr = errors.New("teardown script exited 1")
	d := newPreviewAPIDaemon(t, previewConfig(true, nil), mgr)

	resp := d.handlePreviewStop(ipc.PreviewActionPayload{BeadID: "Forge-abc1"})
	require.Equal(t, "queued", resp.Type)

	outcome := awaitOutcome(t, d, resp.RequestID)
	assert.Equal(t, ipc.RequestStateError, outcome.State)
	assert.Contains(t, outcome.Message, "teardown script exited 1")
}

func TestHandlePreviewStop_Rejections(t *testing.T) {
	d := newPreviewAPIDaemon(t, previewConfig(false, nil), nil)
	resp := d.handlePreviewStop(ipc.PreviewActionPayload{BeadID: "Forge-abc1"})
	assert.Equal(t, "error", resp.Type)

	d = newPreviewAPIDaemon(t, previewConfig(true, nil), newFakePreviewManager())
	resp = d.handlePreviewStop(ipc.PreviewActionPayload{})
	assert.Equal(t, "error", resp.Type)
}

func TestHandlePreviewList_DisabledReportsSettingsOnly(t *testing.T) {
	cfg := previewConfig(false, nil)
	cfg.Settings.PreviewPublicHost = "forge.wg"
	cfg.Settings.PreviewIdleTimeout = 30 * time.Minute
	d := newPreviewAPIDaemon(t, cfg, nil)

	resp := d.handlePreviewList()
	require.Equal(t, "ok", resp.Type)
	var out ipc.PreviewsResponse
	require.NoError(t, json.Unmarshal(resp.Payload, &out))

	assert.False(t, out.Enabled)
	assert.Empty(t, out.Previews)
	// The settings still travel so the UI can explain what previews *would* do.
	assert.Equal(t, "forge.wg", out.PublicHost)
	assert.Equal(t, int64(1800), out.IdleTimeoutSeconds)
}

func TestHandlePreviewList_ReportsLivePreviews(t *testing.T) {
	mgr := newFakePreviewManager()
	cfg := previewConfig(true, nil)
	cfg.Settings.PreviewIdleTimeout = 30 * time.Minute
	d := newPreviewAPIDaemon(t, cfg, mgr)

	// Two previews, registered out of order — List sorts by bead id.
	for _, bead := range []string{"Forge-zzz9", "Forge-abc1"} {
		_, err := mgr.Start(context.Background(), kiln.StartOptions{
			BeadID: bead, Anvil: "forge", AnvilPath: "/tmp/forge", Branch: "forge/" + bead,
		})
		require.NoError(t, err)
	}

	resp := d.handlePreviewList()
	require.Equal(t, "ok", resp.Type)
	var out ipc.PreviewsResponse
	require.NoError(t, json.Unmarshal(resp.Payload, &out))

	assert.True(t, out.Enabled)
	require.Len(t, out.Previews, 2)
	assert.Equal(t, "Forge-abc1", out.Previews[0].BeadID)
	assert.Equal(t, "Forge-zzz9", out.Previews[1].BeadID)
	assert.Equal(t, "forge", out.Previews[0].Anvil)
	assert.Equal(t, "forge/Forge-abc1", out.Previews[0].Branch)
}
