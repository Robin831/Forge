package daemon

import (
	"encoding/json"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/kiln"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingPreviewInstance is a kiln.Instance backed by a fixed record, so a
// test can give a preview real services and ports without spawning anything.
type recordingPreviewInstance struct {
	rec state.Preview
}

func (f recordingPreviewInstance) Stop() error           { return nil }
func (f recordingPreviewInstance) Status() string        { return f.rec.Status }
func (f recordingPreviewInstance) EntryURL() string      { return "" }
func (f recordingPreviewInstance) Ports() []int          { return nil }
func (f recordingPreviewInstance) Record() state.Preview { return f.rec }

// resolveDaemon builds a daemon holding one preview for beadID with the given
// status and services.
func resolveDaemon(t *testing.T, beadID, status string, services []state.PreviewService) (*Daemon, *fakePreviewManager) {
	t.Helper()
	mgr := newFakePreviewManager()
	if beadID != "" {
		env := &kiln.Environment{BeadID: beadID, Anvil: "forge", Branch: "forge/" + beadID}
		env.AttachInstance(recordingPreviewInstance{rec: state.Preview{
			BeadID:   beadID,
			Anvil:    "forge",
			Status:   status,
			Services: services,
		}})
		mgr.envs = map[string]*kiln.Environment{beadID: env}
	}
	return newPreviewAPIDaemon(t, previewConfig(true, nil), mgr), mgr
}

func decodeResolve(t *testing.T, resp ipc.Response) ipc.PreviewResolveResponse {
	t.Helper()
	require.Equal(t, "ok", resp.Type, "payload: %s", string(resp.Payload))
	var out ipc.PreviewResolveResponse
	require.NoError(t, json.Unmarshal(resp.Payload, &out))
	return out
}

func TestHandlePreviewResolve_EntryServiceAndTouch(t *testing.T) {
	d, mgr := resolveDaemon(t, "Forge_abc1", state.PreviewRunning, []state.PreviewService{
		{Name: "api", Port: 40001, Health: state.PreviewServiceHealthy},
		{Name: "web", Port: 40002, Health: state.PreviewServiceHealthy, Entry: true},
	})

	// "Forge_abc1" folds to the label "forge-abc1" — the underscore is the one
	// character SanitizePreviewID emits that a hostname may not carry.
	out := decodeResolve(t, d.handlePreviewResolve(ipc.PreviewResolvePayload{Label: "forge-abc1"}))
	assert.True(t, out.Found)
	assert.Equal(t, "Forge_abc1", out.BeadID)
	assert.Equal(t, "web", out.Service, "an unqualified host resolves to the entry service")
	assert.Equal(t, 40002, out.Port)
	assert.Equal(t, "127.0.0.1", out.Host)
	assert.Equal(t, state.PreviewRunning, out.Status)

	// Proxied traffic is activity: the resolve is what keeps the idle reaper off
	// a preview somebody is actively browsing.
	assert.Equal(t, []string{"Forge_abc1"}, mgr.touchedBeads())
}

func TestHandlePreviewResolve_NamedService(t *testing.T) {
	d, _ := resolveDaemon(t, "Forge-abc1", state.PreviewRunning, []state.PreviewService{
		{Name: "web", Port: 40002, Entry: true},
		{Name: "API_v2", Port: 40003},
	})

	out := decodeResolve(t, d.handlePreviewResolve(ipc.PreviewResolvePayload{
		Label: "forge-abc1", Service: "api-v2",
	}))
	assert.True(t, out.Found)
	assert.Equal(t, "API_v2", out.Service, "a service name is matched through its DNS fold")
	assert.Equal(t, 40003, out.Port)
}

func TestHandlePreviewResolve_Refusals(t *testing.T) {
	tests := []struct {
		name       string
		beadID     string
		status     string
		services   []state.PreviewService
		label      string
		service    string
		wantReason string
		wantBead   string
	}{
		{
			name:       "no preview by that label",
			beadID:     "Forge-abc1",
			status:     state.PreviewRunning,
			services:   []state.PreviewService{{Name: "web", Port: 1, Entry: true}},
			label:      "forge-zzz9",
			wantReason: ipc.PreviewResolveNoPreview,
		},
		{
			name:       "stopped preview",
			beadID:     "Forge-abc1",
			status:     state.PreviewStopped,
			services:   []state.PreviewService{{Name: "web", Port: 1, Entry: true}},
			label:      "forge-abc1",
			wantReason: ipc.PreviewResolveStopped,
			wantBead:   "Forge-abc1",
		},
		{
			name:       "failed preview never served",
			beadID:     "Forge-abc1",
			status:     state.PreviewFailed,
			services:   []state.PreviewService{{Name: "web", Port: 1, Entry: true}},
			label:      "forge-abc1",
			wantReason: ipc.PreviewResolveStopped,
			wantBead:   "Forge-abc1",
		},
		{
			name:       "unknown service",
			beadID:     "Forge-abc1",
			status:     state.PreviewRunning,
			services:   []state.PreviewService{{Name: "web", Port: 1, Entry: true}},
			label:      "forge-abc1",
			service:    "api",
			wantReason: ipc.PreviewResolveNoService,
			wantBead:   "Forge-abc1",
		},
		{
			name:       "ports not allocated yet",
			beadID:     "Forge-abc1",
			status:     state.PreviewStarting,
			services:   []state.PreviewService{{Name: "web", Entry: true}},
			label:      "forge-abc1",
			wantReason: ipc.PreviewResolveNoPort,
			wantBead:   "Forge-abc1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, mgr := resolveDaemon(t, tc.beadID, tc.status, tc.services)
			out := decodeResolve(t, d.handlePreviewResolve(ipc.PreviewResolvePayload{
				Label: tc.label, Service: tc.service,
			}))
			assert.False(t, out.Found)
			assert.Equal(t, tc.wantReason, out.Reason)
			assert.Equal(t, tc.wantBead, out.BeadID)
			assert.Zero(t, out.Port)
			assert.Empty(t, mgr.touchedBeads(), "a refused resolve must not reset the idle clock")
		})
	}
}

// Previews switched off answers with a reason rather than an error: the proxy
// renders the same "nothing is serving this name" 404 either way.
func TestHandlePreviewResolve_PreviewsDisabled(t *testing.T) {
	d := newPreviewAPIDaemon(t, previewConfig(false, nil), nil)
	out := decodeResolve(t, d.handlePreviewResolve(ipc.PreviewResolvePayload{Label: "forge-abc1"}))
	assert.False(t, out.Found)
	assert.Equal(t, ipc.PreviewResolveDisabled, out.Reason)
}

func TestHandlePreviewResolve_EmptyLabelIsAnError(t *testing.T) {
	d, _ := resolveDaemon(t, "Forge-abc1", state.PreviewRunning, nil)
	resp := d.handlePreviewResolve(ipc.PreviewResolvePayload{Label: "  "})
	assert.Equal(t, "error", resp.Type)
}

// A wildcard bind names no address to connect to, so the proxy is told to dial
// loopback — which a wildcard listener also answers on.
func TestPreviewDialHost(t *testing.T) {
	tests := []struct {
		bind string
		want string
	}{
		{"", "127.0.0.1"},
		{"0.0.0.0", "127.0.0.1"},
		{"::", "127.0.0.1"},
		{"[::]", "127.0.0.1"},
		{"127.0.0.1", "127.0.0.1"},
		{"192.168.1.10", "192.168.1.10"},
	}
	for _, tc := range tests {
		cfg := &config.Config{Settings: config.SettingsConfig{PreviewBindHost: tc.bind}}
		assert.Equal(t, tc.want, previewDialHost(cfg), "bind host %q", tc.bind)
	}
	assert.Equal(t, "127.0.0.1", previewDialHost(nil))
}
