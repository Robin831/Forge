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
	"github.com/Robin831/Forge/internal/state"
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

// startFakePreview registers a live preview with the fake manager so a stop has
// something to tear down.
func startFakePreview(t *testing.T, mgr *fakePreviewManager, beadID string) {
	t.Helper()
	_, err := mgr.Start(context.Background(), kiln.StartOptions{
		BeadID: beadID, Anvil: "forge", AnvilPath: "/tmp/forge", Branch: "forge/" + beadID,
	})
	require.NoError(t, err)
}

func TestHandlePreviewStop_QueuesAndStops(t *testing.T) {
	mgr := newFakePreviewManager()
	startFakePreview(t, mgr, "Forge-abc1")
	d := newPreviewAPIDaemon(t, previewConfig(true, nil), mgr)

	resp := d.handlePreviewStop(ipc.PreviewActionPayload{BeadID: "Forge-abc1"})
	require.Equal(t, "queued", resp.Type)

	outcome := awaitOutcome(t, d, resp.RequestID)
	assert.Equal(t, ipc.RequestStateOK, outcome.State)
	assert.Contains(t, outcome.Message, "Forge-abc1")
	assert.Equal(t, []string{"Forge-abc1"}, mgr.stoppedBeads())

	// The preview is gone, not merely marked stopped: listing reports nothing.
	listResp := d.handlePreviewList()
	require.Equal(t, "ok", listResp.Type)
	var list ipc.PreviewListResponse
	require.NoError(t, json.Unmarshal(listResp.Payload, &list))
	assert.Empty(t, list.Previews)
}

func TestHandlePreviewStop_ReportsTeardownFailure(t *testing.T) {
	mgr := newFakePreviewManager()
	mgr.stopErr = errors.New("teardown script exited 1")
	startFakePreview(t, mgr, "Forge-abc1")
	d := newPreviewAPIDaemon(t, previewConfig(true, nil), mgr)

	resp := d.handlePreviewStop(ipc.PreviewActionPayload{BeadID: "Forge-abc1"})
	require.Equal(t, "queued", resp.Type)

	outcome := awaitOutcome(t, d, resp.RequestID)
	assert.Equal(t, ipc.RequestStateError, outcome.State)
	assert.Contains(t, outcome.Message, "teardown script exited 1")
}

// TestHandlePreviewStop_Rejections covers everything answered synchronously,
// before any teardown is queued — including the mistyped bead id, which must
// not read as "stopped".
func TestHandlePreviewStop_Rejections(t *testing.T) {
	live := newFakePreviewManager()
	startFakePreview(t, live, "Forge-abc1")

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
			payload: ipc.PreviewActionPayload{BeadID: "Forge-abc1"},
			wantMsg: "preview environments are disabled",
		},
		{
			name:    "missing bead id",
			cfg:     previewConfig(true, nil),
			mgr:     live,
			payload: ipc.PreviewActionPayload{},
			wantMsg: "bead_id is required",
		},
		{
			name:    "bead has no preview",
			cfg:     previewConfig(true, nil),
			mgr:     live,
			payload: ipc.PreviewActionPayload{BeadID: "Forge-nope"},
			wantMsg: "no preview running for bead Forge-nope",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newPreviewAPIDaemon(t, tt.cfg, tt.mgr)
			resp := d.handlePreviewStop(tt.payload)
			require.Equal(t, "error", resp.Type)
			var body struct {
				Message string `json:"message"`
			}
			require.NoError(t, json.Unmarshal(resp.Payload, &body))
			assert.Contains(t, body.Message, tt.wantMsg)
			if fake, ok := tt.mgr.(*fakePreviewManager); ok {
				assert.Empty(t, fake.stoppedBeads(), "a rejected stop must not reach the manager")
			}
		})
	}
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

// TestHandlePreviewList_EnabledWithNoPreviews — an enabled Kiln with nothing
// running answers with an empty JSON array, never null, so a client can range
// over it without a nil check.
func TestHandlePreviewList_EnabledWithNoPreviews(t *testing.T) {
	d := newPreviewAPIDaemon(t, previewConfig(true, nil), newFakePreviewManager())

	resp := d.handlePreviewList()
	require.Equal(t, "ok", resp.Type)
	assert.Contains(t, string(resp.Payload), `"previews":[]`)

	var out ipc.PreviewListResponse
	require.NoError(t, json.Unmarshal(resp.Payload, &out))
	assert.True(t, out.Enabled)
	assert.NotNil(t, out.Previews)
	assert.Empty(t, out.Previews)
}

// TestHandleIPC_PreviewListAliasesPreviews — the CLI's command name and the web
// dashboard's resolve to the same handler and the same payload.
func TestHandleIPC_PreviewListAliasesPreviews(t *testing.T) {
	mgr := newFakePreviewManager()
	startFakePreview(t, mgr, "Forge-abc1")
	d := newPreviewAPIDaemon(t, previewConfig(true, nil), mgr)

	fromList := d.handleIPC(ipc.Command{Type: "preview_list"})
	fromPreviews := d.handleIPC(ipc.Command{Type: "previews"})

	require.Equal(t, "ok", fromList.Type)
	assert.JSONEq(t, string(fromPreviews.Payload), string(fromList.Payload))

	var out ipc.PreviewListResponse
	require.NoError(t, json.Unmarshal(fromList.Payload, &out))
	require.Len(t, out.Previews, 1)
	assert.Equal(t, "Forge-abc1", out.Previews[0].BeadID)
}

// TestPreviewInfo_MapsRecord covers the fields the list derives rather than
// copies: the entry port, the idle countdown and the resource note. It works
// off a record because a *kiln.Environment carrying live services can only be
// built by the manager itself.
func TestPreviewInfo_MapsRecord(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	rec := state.Preview{
		BeadID:       "Forge-abc1",
		Anvil:        "forge",
		Branch:       "forge/Forge-abc1",
		Status:       state.PreviewRunning,
		CreatedAt:    now.Add(-20 * time.Minute),
		LastActiveAt: now.Add(-5 * time.Minute),
		Services: []state.PreviewService{
			{Name: "api", Port: 4310, Health: state.PreviewServiceHealthy},
			{Name: "web", Port: 4311, Health: state.PreviewServiceHealthy, Entry: true},
		},
	}

	info := previewInfo(rec, "http://forge.wg:4311", 30*time.Minute, now)

	assert.Equal(t, "Forge-abc1", info.BeadID)
	assert.Equal(t, state.PreviewRunning, info.Status)
	assert.Equal(t, "http://forge.wg:4311", info.EntryURL)
	// The entry service's port wins over the first allocated one.
	assert.Equal(t, 4311, info.Port)
	require.NotNil(t, info.IdleRemainingSeconds)
	assert.Equal(t, int64(25*60), *info.IdleRemainingSeconds)
	assert.Equal(t, "2 services, ports 4310, 4311", info.ResourceNote)
	require.Len(t, info.Services, 2)
	assert.Equal(t, "api", info.Services[0].Name)
	assert.True(t, info.Services[1].Entry)
}

func TestPreviewEntryPort(t *testing.T) {
	tests := []struct {
		name     string
		services []state.PreviewService
		want     int
	}{
		{name: "no services", want: 0},
		{
			name:     "no ports allocated yet",
			services: []state.PreviewService{{Name: "web", Entry: true}},
			want:     0,
		},
		{
			name:     "falls back to the first port when nothing is the entry",
			services: []state.PreviewService{{Name: "api", Port: 4310}, {Name: "web", Port: 4311}},
			want:     4310,
		},
		{
			name:     "prefers the entry service",
			services: []state.PreviewService{{Name: "api", Port: 4310}, {Name: "web", Port: 4311, Entry: true}},
			want:     4311,
		},
		{
			// Forge-bci1: the entry service still holds its port after it dies,
			// so the old "first port that exists" rule handed out a link that
			// answered ERR_EMPTY_RESPONSE — which reads as a broken tunnel.
			name: "withholds the port when the entry service has exited",
			services: []state.PreviewService{
				{Name: "api", Port: 4310, Health: state.PreviewServiceHealthy},
				{Name: "web", Port: 4311, Health: state.PreviewServiceExited, Entry: true},
			},
			want: 0,
		},
		{
			name: "withholds the port when the entry service failed",
			services: []state.PreviewService{
				{Name: "web", Port: 4311, Health: state.PreviewServiceFailed, Entry: true},
			},
			want: 0,
		},
		{
			// Never a healthy sibling's port: that link works and shows the
			// wrong application, which is worse than no link at all.
			name: "does not fall back to a healthy sibling",
			services: []state.PreviewService{
				{Name: "web", Port: 4311, Health: state.PreviewServiceExited, Entry: true},
				{Name: "api", Port: 4310, Health: state.PreviewServiceHealthy},
			},
			want: 0,
		},
		{
			name: "a starting entry service still gets its port",
			services: []state.PreviewService{
				{Name: "web", Port: 4311, Health: state.PreviewServiceStarting, Entry: true},
			},
			want: 4311,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, previewEntryPort(state.Preview{Services: tt.services}))
		})
	}
}

// A withheld entry URL has to say why, or the link simply disappears and the
// operator is left guessing. Nothing to explain — a preview still allocating
// ports — produces no note at all, so the absence of a note stays meaningful.
func TestPreviewEntryNote(t *testing.T) {
	tests := []struct {
		name     string
		services []state.PreviewService
		want     string
	}{
		{name: "no services"},
		{
			name:     "still coming up",
			services: []state.PreviewService{{Name: "web", Port: 4311, Health: state.PreviewServiceStarting, Entry: true}},
		},
		{
			name:     "healthy",
			services: []state.PreviewService{{Name: "web", Port: 4311, Health: state.PreviewServiceHealthy, Entry: true}},
		},
		{
			name: "exited entry service names its cause",
			services: []state.PreviewService{{
				Name: "client", Port: 4311, Entry: true,
				Health: state.PreviewServiceExited,
				Error:  "exited (exit 1, lived 7m31s)",
			}},
			want: `entry service "client" is not serving: exited (exit 1, lived 7m31s)`,
		},
		{
			name: "exited without recorded detail falls back to the state",
			services: []state.PreviewService{{
				Name: "client", Port: 4311, Entry: true, Health: state.PreviewServiceExited,
			}},
			want: `entry service "client" is not serving: exited`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, previewEntryNote(state.Preview{Services: tt.services}))
		})
	}
}

// The exit has to survive the mapping onto the IPC payload, or it stops at the
// daemon and neither front end ever hears about it.
func TestPreviewInfo_CarriesServiceExits(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	code := 1
	rec := state.Preview{
		BeadID: "Forge-abc1",
		Status: state.PreviewDegraded,
		Services: []state.PreviewService{
			{Name: "api", Port: 4310, Health: state.PreviewServiceHealthy, StartedAt: now.Add(-10 * time.Minute)},
			{
				Name: "client", Port: 4311, Entry: true,
				Health:    state.PreviewServiceExited,
				Error:     "exited (exit 1, lived 7m31s)",
				StartedAt: now.Add(-10 * time.Minute),
				ExitedAt:  now.Add(-2*time.Minute - 29*time.Second),
				ExitCode:  &code,
			},
		},
	}

	info := previewInfo(rec, "", 0, now)

	assert.Empty(t, info.EntryURL, "a dead entry service hands out no link")
	assert.Zero(t, info.Port)
	assert.Contains(t, info.EntryNote, `entry service "client" is not serving`)
	require.Len(t, info.Services, 2)
	client := info.Services[1]
	assert.Equal(t, state.PreviewServiceExited, client.Health)
	require.NotNil(t, client.ExitCode)
	assert.Equal(t, 1, *client.ExitCode)
	assert.False(t, client.ExitedAt.IsZero())
	assert.Equal(t, rec.Services[1].StartedAt, client.StartedAt)
	assert.True(t, info.Services[0].ExitedAt.IsZero(), "a live service reports no exit")
}

// TestPreviewEntryURL — the operator-facing link. It is the one every client
// that has no HTTP request of its own prints (`forge preview list`, the queued
// start's outcome), so the proxy base has to win here and not only in the web
// layer, or the CLI would keep handing out loopback ports nobody can reach.
func TestPreviewEntryURL(t *testing.T) {
	rec := state.Preview{
		BeadID: "Forge-abc1",
		Services: []state.PreviewService{
			{Name: "api", Port: 4310},
			{Name: "web", Port: 4311, Entry: true},
		},
	}

	tests := []struct {
		name     string
		settings config.SettingsConfig
		rec      state.Preview
		want     string
	}{
		{
			name:     "no proxy base: the entry port on the public host",
			settings: config.SettingsConfig{PreviewPublicHost: "forge.wg"},
			rec:      rec,
			want:     "http://forge.wg:4311/",
		},
		{
			name:     "no proxy base and no public host: the bind host",
			settings: config.SettingsConfig{PreviewBindHost: "127.0.0.1"},
			rec:      rec,
			want:     "http://127.0.0.1:4311/",
		},
		{
			name: "proxy base: the preview hostname, not the port",
			settings: config.SettingsConfig{
				PreviewPublicHost: "forge.wg",
				PreviewProxyBase:  "preview.example.com",
			},
			rec:  rec,
			want: "https://forge-abc1.preview.example.com/",
		},
		{
			name:     "proxy base: a preview whose ports are not allocated yet still has a link",
			settings: config.SettingsConfig{PreviewProxyBase: "preview.example.com"},
			rec:      state.Preview{BeadID: "Forge-abc1"},
			want:     "https://forge-abc1.preview.example.com/",
		},
		{
			name:     "no proxy base and no ports: no link",
			settings: config.SettingsConfig{PreviewPublicHost: "forge.wg"},
			rec:      state.Preview{BeadID: "Forge-abc1"},
			want:     "",
		},
		{
			// The host-based form is built from the bead id alone, so nothing
			// about the port suppression reaches it: without an explicit check
			// a proxy deployment printed a preview hostname beside the note
			// explaining why there was no link.
			name: "proxy base: an exited entry service withholds the hostname link too",
			settings: config.SettingsConfig{
				PreviewPublicHost: "forge.wg",
				PreviewProxyBase:  "preview.example.com",
			},
			rec: state.Preview{
				BeadID: "Forge-abc1",
				Services: []state.PreviewService{
					{Name: "api", Port: 4310, Health: state.PreviewServiceHealthy},
					{Name: "web", Port: 4311, Entry: true, Health: state.PreviewServiceExited},
				},
			},
			want: "",
		},
		{
			name: "proxy base: a failed entry service withholds the hostname link",
			settings: config.SettingsConfig{
				PreviewProxyBase: "preview.example.com",
			},
			rec: state.Preview{
				BeadID: "Forge-abc1",
				Services: []state.PreviewService{
					{Name: "web", Port: 4311, Entry: true, Health: state.PreviewServiceFailed},
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := previewEntryURL(&config.Config{Settings: tt.settings}, tt.rec)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestPreviewEntryURLAndNoteAgree pins the contract on ipc.PreviewInfo.EntryNote:
// the note is set exactly when the link was withheld because the entry service
// is not serving — in *either* addressing form. A client that renders "Open"
// from EntryURL and the reason from EntryNote would otherwise show both at once,
// which is how a dead preview kept an Open button in proxy deployments.
func TestPreviewEntryURLAndNoteAgree(t *testing.T) {
	dead := state.Preview{
		BeadID: "Forge-abc1",
		Status: state.PreviewDegraded,
		Services: []state.PreviewService{
			{Name: "api", Port: 4310, Health: state.PreviewServiceHealthy},
			{Name: "web", Port: 4311, Entry: true, Health: state.PreviewServiceExited,
				Error: "exited (exit 1, lived 7m31s)"},
		},
	}
	settings := map[string]config.SettingsConfig{
		"direct": {PreviewPublicHost: "forge.wg"},
		"proxy":  {PreviewPublicHost: "forge.wg", PreviewProxyBase: "preview.example.com"},
	}

	for name, s := range settings {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{Settings: s}
			info := previewInfo(dead, previewEntryURL(cfg, dead), 0, time.Now())
			assert.Empty(t, info.EntryURL, "a dead entry service hands out no link")
			assert.NotEmpty(t, info.EntryNote, "and says why instead")
		})
	}
}

// TestHandlePreviewList_EntryURLFollowsTheProxyBase — the whole path the CLI
// reads: the payload `forge preview list` prints carries the proxy link.
func TestHandlePreviewList_EntryURLFollowsTheProxyBase(t *testing.T) {
	mgr := newFakePreviewManager()
	startFakePreview(t, mgr, "Forge-abc1")
	cfg := previewConfig(true, nil)
	cfg.Settings.PreviewProxyBase = "preview.example.com"
	d := newPreviewAPIDaemon(t, cfg, mgr)

	resp := d.handlePreviewList()
	require.Equal(t, "ok", resp.Type)
	var out ipc.PreviewListResponse
	require.NoError(t, json.Unmarshal(resp.Payload, &out))

	require.Len(t, out.Previews, 1)
	assert.Equal(t, "https://forge-abc1.preview.example.com/", out.Previews[0].EntryURL)
}

func TestPreviewIdleRemaining(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

	// The reaper is disabled: there is no deadline to count down to.
	assert.Nil(t, previewIdleRemaining(state.Preview{LastActiveAt: now}, 0, now))
	// Never touched: the same, rather than a countdown from the zero time.
	assert.Nil(t, previewIdleRemaining(state.Preview{}, 30*time.Minute, now))

	remaining := previewIdleRemaining(state.Preview{LastActiveAt: now.Add(-10 * time.Minute)}, 30*time.Minute, now)
	require.NotNil(t, remaining)
	assert.Equal(t, int64(20*60), *remaining)

	// Past its deadline, waiting for the next reaper tick — clamped, not negative.
	overdue := previewIdleRemaining(state.Preview{LastActiveAt: now.Add(-40 * time.Minute)}, 30*time.Minute, now)
	require.NotNil(t, overdue)
	assert.Equal(t, int64(0), *overdue)
}

func TestPreviewResourceNote(t *testing.T) {
	tests := []struct {
		name     string
		services []state.PreviewService
		want     string
	}{
		{name: "nothing supervised yet", want: "no services"},
		{
			name:     "singular",
			services: []state.PreviewService{{Name: "web", Port: 4310}},
			want:     "1 service, ports 4310",
		},
		{
			name:     "ports still being allocated",
			services: []state.PreviewService{{Name: "web"}, {Name: "api"}},
			want:     "2 services, no ports allocated",
		},
		{
			name:     "every allocated port is listed",
			services: []state.PreviewService{{Name: "web", Port: 4310}, {Name: "api", Port: 4311}},
			want:     "2 services, ports 4310, 4311",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, previewResourceNote(state.Preview{Services: tt.services}))
		})
	}
}
