package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

// previewsHandler returns an in-process IPC handler that answers the "previews"
// command with the given payload and errors on anything else, so a test that
// only exercises the list path cannot accidentally pass because some other
// command was dispatched.
func previewsHandler(t *testing.T, payload ipc.PreviewsResponse) CommandHandler {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal previews payload: %v", err)
	}
	return func(cmd ipc.Command) ipc.Response {
		if cmd.Type != "previews" {
			return ipc.Response{Type: "error", Payload: []byte(`{"message":"unexpected command"}`)}
		}
		return ipc.Response{Type: "ok", Payload: raw}
	}
}

// getPreviews issues an authenticated GET with an explicit Host header, which
// is what the entry-URL fallback reads when preview_public_host is unset.
func getPreviews(t *testing.T, srv *Server, host string) *httptest.ResponseRecorder {
	t.Helper()
	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", "/api/previews", nil)
	if host != "" {
		req.Host = host
	}
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func decodePreviews(t *testing.T, rec *httptest.ResponseRecorder) PreviewsListResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var out PreviewsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse previews response: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

func TestPreviewRoutes_RequireAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cases := []struct{ method, path string }{
		{"GET", "/api/previews"},
		{"GET", "/api/preview/Forge-abc1/log/api"},
		{"POST", "/api/bead/Forge-abc1/preview/start"},
		{"POST", "/api/bead/Forge-abc1/preview/stop"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-Forge-Action", "1")
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: expected 401, got %d", tc.method, tc.path, rec.Code)
		}
	}
}

func TestPreviewStart_QueuedReturns202WithPollURL(t *testing.T) {
	h := &recordingHandler{resp: ipc.Response{
		Type:      "queued",
		RequestID: "forge-1-1",
		Payload:   []byte(`{"message":"starting preview"}`),
	}}
	srv := newServerWithDefaults(t, h.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/preview/start", map[string]string{"anvil": "forge"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body["request_id"] != "forge-1-1" {
		t.Errorf("request_id = %v, want forge-1-1", body["request_id"])
	}
	if body["poll_url"] != "/api/requests/forge-1-1" {
		t.Errorf("poll_url = %v, want /api/requests/forge-1-1", body["poll_url"])
	}

	cmd, ok := h.lastCommand()
	if !ok {
		t.Fatal("no command dispatched")
	}
	if cmd.Type != "preview_start" {
		t.Fatalf("command type = %q, want preview_start", cmd.Type)
	}
	var payload ipc.PreviewActionPayload
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.BeadID != "Forge-abc1" || payload.Anvil != "forge" {
		t.Errorf("payload = %+v, want bead Forge-abc1 anvil forge", payload)
	}
	// No branch in the body means the daemon picks forge/<bead-id>; the handler
	// must not invent one of its own.
	if payload.Branch != "" {
		t.Errorf("branch = %q, want empty so the daemon applies its default", payload.Branch)
	}
}

// An ad-hoc preview names a branch that is not the bead's canonical one — and
// the bead id it is filed under need never have existed in bd. This is the
// browser half of `forge preview start kiln-smoke-1 --anvil forge --branch main`.
func TestPreviewStart_ForwardsBranch(t *testing.T) {
	h := &recordingHandler{resp: ipc.Response{
		Type:      "queued",
		RequestID: "forge-1-1",
		Payload:   []byte(`{"message":"starting preview"}`),
	}}
	srv := newServerWithDefaults(t, h.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/kiln-smoke-1/preview/start", map[string]string{
		"anvil":  "forge",
		"branch": "  main  ",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}

	cmd, ok := h.lastCommand()
	if !ok {
		t.Fatal("no command dispatched")
	}
	var payload ipc.PreviewActionPayload
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.BeadID != "kiln-smoke-1" {
		t.Errorf("bead_id = %q, want kiln-smoke-1", payload.BeadID)
	}
	if payload.Branch != "main" {
		t.Errorf("branch = %q, want the trimmed main", payload.Branch)
	}
}

func TestPreviewStart_RequiresAnvil(t *testing.T) {
	h := &recordingHandler{}
	srv := newServerWithDefaults(t, h.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/preview/start", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if _, dispatched := h.lastCommand(); dispatched {
		t.Error("expected no IPC dispatch for a request without an anvil")
	}
}

func TestPreviewStart_InvalidBeadID(t *testing.T) {
	h := &recordingHandler{}
	srv := newServerWithDefaults(t, h.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/..%2Fetc/preview/start", map[string]string{"anvil": "forge"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPreviewStart_ErrorPropagates(t *testing.T) {
	h := &recordingHandler{resp: ipc.Response{
		Type:    "error",
		Payload: []byte(`{"message":"preview environments are disabled"}`),
	}}
	srv := newServerWithDefaults(t, h.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/preview/start", map[string]string{"anvil": "forge"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body["error"] != "preview environments are disabled" {
		t.Errorf("error = %q, want the daemon's message", body["error"])
	}
}

func TestPreviewStop_QueuedWithoutAnvil(t *testing.T) {
	h := &recordingHandler{resp: ipc.Response{
		Type:      "queued",
		RequestID: "forge-2-1",
		Payload:   []byte(`{"message":"stopping preview"}`),
	}}
	srv := newServerWithDefaults(t, h.handle)
	cookie := loginAndGetCookie(t, srv)

	// No body at all: stop is keyed purely by bead id.
	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/preview/stop", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body["poll_url"] != "/api/requests/forge-2-1" {
		t.Errorf("poll_url = %v, want /api/requests/forge-2-1", body["poll_url"])
	}

	cmd, ok := h.lastCommand()
	if !ok {
		t.Fatal("no command dispatched")
	}
	if cmd.Type != "preview_stop" {
		t.Fatalf("command type = %q, want preview_stop", cmd.Type)
	}
	var payload ipc.PreviewActionPayload
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.BeadID != "Forge-abc1" {
		t.Errorf("bead_id = %q, want Forge-abc1", payload.BeadID)
	}
}

func TestPreviewStop_ErrorPropagates(t *testing.T) {
	h := &recordingHandler{resp: ipc.Response{
		Type:    "error",
		Payload: []byte(`{"message":"teardown script failed"}`),
	}}
	srv := newServerWithDefaults(t, h.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/preview/stop", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPreviewsList_EmptySerializesAsArray(t *testing.T) {
	srv := newServerWithDefaults(t, previewsHandler(t, ipc.PreviewsResponse{Enabled: true}))
	rec := getPreviews(t, srv, "hearth.example:9000")

	if got := rec.Body.String(); !jsonHasEmptyPreviewArray(t, got) {
		t.Fatalf("expected previews to serialize as [], got %s", got)
	}
	out := decodePreviews(t, rec)
	if !out.Enabled {
		t.Error("expected enabled=true")
	}
	if len(out.Previews) != 0 {
		t.Errorf("expected no previews, got %d", len(out.Previews))
	}
}

// jsonHasEmptyPreviewArray asserts the wire form is [] rather than null, which
// a typed decode would hide.
func jsonHasEmptyPreviewArray(t *testing.T, body string) bool {
	t.Helper()
	var raw struct {
		Previews json.RawMessage `json:"previews"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return string(raw.Previews) == "[]"
}

func TestPreviewsList_PassesThroughPreviewableAnvils(t *testing.T) {
	srv := newServerWithDefaults(t, previewsHandler(t, ipc.PreviewsResponse{
		Enabled: true,
		Anvils:  []string{"forge", "hytte"},
	}))
	out := decodePreviews(t, getPreviews(t, srv, "hearth.example:9000"))
	if len(out.Anvils) != 2 || out.Anvils[0] != "forge" || out.Anvils[1] != "hytte" {
		t.Errorf("anvils: got %v, want [forge hytte]", out.Anvils)
	}
}

// The SPA gates its Preview button on this list, so an absent one has to reach
// the browser as [] — a null would be a type error at the consumer, not an
// empty gate.
func TestPreviewsList_AnvilsSerializeAsArrayWhenAbsent(t *testing.T) {
	srv := newServerWithDefaults(t, previewsHandler(t, ipc.PreviewsResponse{Enabled: false}))
	rec := getPreviews(t, srv, "hearth.example:9000")
	var raw struct {
		Anvils json.RawMessage `json:"anvils"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if string(raw.Anvils) != "[]" {
		t.Errorf("expected anvils to serialize as [], got %s", raw.Anvils)
	}
}

func TestPreviewsList_DisabledReportsEnabledFalse(t *testing.T) {
	srv := newServerWithDefaults(t, previewsHandler(t, ipc.PreviewsResponse{Enabled: false}))
	out := decodePreviews(t, getPreviews(t, srv, "hearth.example:9000"))
	if out.Enabled {
		t.Error("expected enabled=false when the daemon has no Kiln manager")
	}
}

// samplePreview is the two-service preview the DTO-mapping tests share.
func samplePreview(created, lastActive time.Time) ipc.PreviewInfo {
	return ipc.PreviewInfo{
		BeadID: "Forge-abc1",
		Anvil:  "forge",
		Branch: "forge/Forge-abc1",
		Status: state.PreviewRunning,
		Services: []ipc.PreviewServiceInfo{
			{Name: "api", Port: 42001, Health: state.PreviewServiceHealthy},
			{Name: "client", Port: 42002, Health: state.PreviewServiceHealthy, Entry: true},
		},
		CreatedAt:    created,
		LastActiveAt: lastActive,
	}
}

func TestPreviewsList_MapsDTOWithLogURLsAndIdleDeadline(t *testing.T) {
	created := time.Now().Add(-90 * time.Second).UTC()
	lastActive := time.Now().Add(-30 * time.Second).UTC()
	info := samplePreview(created, lastActive)
	remaining := int64(1770)
	info.IdleRemainingSeconds = &remaining
	info.ResourceNote = "2 services, ports 42001, 42002"
	srv := newServerWithDefaults(t, previewsHandler(t, ipc.PreviewsResponse{
		Enabled:            true,
		IdleTimeoutSeconds: 1800,
		Previews:           []ipc.PreviewInfo{info},
	}))

	out := decodePreviews(t, getPreviews(t, srv, "hearth.example:9000"))
	if len(out.Previews) != 1 {
		t.Fatalf("expected 1 preview, got %d", len(out.Previews))
	}
	p := out.Previews[0]
	if p.BeadID != "Forge-abc1" || p.Anvil != "forge" || p.Branch != "forge/Forge-abc1" {
		t.Errorf("identity fields = %+v", p)
	}
	if p.Status != state.PreviewRunning {
		t.Errorf("status = %q, want %q", p.Status, state.PreviewRunning)
	}
	if len(p.Services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(p.Services))
	}
	if got, want := p.Services[0].LogURL, "/api/preview/Forge-abc1/log/api"; got != want {
		t.Errorf("api log_url = %q, want %q", got, want)
	}
	if got, want := p.Services[1].LogURL, "/api/preview/Forge-abc1/log/client"; got != want {
		t.Errorf("client log_url = %q, want %q", got, want)
	}
	if !p.Services[1].Entry {
		t.Error("expected the client service to be flagged as the entry service")
	}
	// Uptime is measured from the preview's start, so it must be at least the
	// 90 seconds since createdAt (and not wildly more).
	if up := p.Services[0].UptimeSeconds; up < 90 || up > 300 {
		t.Errorf("uptime_seconds = %d, want ~90", up)
	}
	if p.IdleDeadline == nil {
		t.Fatal("expected an idle deadline")
	}
	if want := lastActive.Add(30 * time.Minute); !p.IdleDeadline.Equal(want) {
		t.Errorf("idle_deadline = %v, want %v", p.IdleDeadline, want)
	}
	// The countdown and the resource note are the manager's, not this layer's:
	// they must survive the mapping byte for byte so the dashboard and
	// `forge preview list` read the same numbers.
	if p.IdleRemainingSeconds == nil {
		t.Fatal("expected idle_remaining_seconds to be forwarded")
	}
	if *p.IdleRemainingSeconds != 1770 {
		t.Errorf("idle_remaining_seconds = %d, want 1770", *p.IdleRemainingSeconds)
	}
	if p.ResourceNote != "2 services, ports 42001, 42002" {
		t.Errorf("resource_note = %q, want the manager's summary", p.ResourceNote)
	}
}

// TestPreviewsList_IdleFieldsSerialize pins the wire shape the SPA reads: both
// fields are present under the names the IPC payload uses, and the countdown is
// an explicit null (not an omitted key) when the reaper is disabled, so the
// client can tell "no deadline" from "due now".
func TestPreviewsList_IdleFieldsSerialize(t *testing.T) {
	created := time.Now().Add(-time.Minute).UTC()
	info := samplePreview(created, created)
	remaining := int64(600)
	info.IdleRemainingSeconds = &remaining
	info.ResourceNote = "2 services, ports 42001, 42002"
	srv := newServerWithDefaults(t, previewsHandler(t, ipc.PreviewsResponse{
		Enabled:            true,
		IdleTimeoutSeconds: 1800,
		Previews:           []ipc.PreviewInfo{info},
	}))

	rec := getPreviews(t, srv, "hearth.example:9000")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Previews []map[string]json.RawMessage `json:"previews"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse previews response: %v", err)
	}
	if len(body.Previews) != 1 {
		t.Fatalf("expected 1 preview, got %d", len(body.Previews))
	}
	if got := string(body.Previews[0]["idle_remaining_seconds"]); got != "600" {
		t.Errorf("idle_remaining_seconds = %s, want 600", got)
	}
	if got := string(body.Previews[0]["resource_note"]); got != `"2 services, ports 42001, 42002"` {
		t.Errorf("resource_note = %s", got)
	}
}

func TestPreviewsList_NoIdleDeadlineWhenReaperDisabled(t *testing.T) {
	now := time.Now().UTC()
	srv := newServerWithDefaults(t, previewsHandler(t, ipc.PreviewsResponse{
		Enabled:            true,
		IdleTimeoutSeconds: 0,
		Previews:           []ipc.PreviewInfo{samplePreview(now, now)},
	}))
	rec := getPreviews(t, srv, "hearth.example:9000")
	out := decodePreviews(t, rec)
	if out.Previews[0].IdleDeadline != nil {
		t.Errorf("idle_deadline = %v, want null when the reaper is off", out.Previews[0].IdleDeadline)
	}
	// The manager reports no countdown either — and it stays an explicit null
	// rather than disappearing from the payload.
	if out.Previews[0].IdleRemainingSeconds != nil {
		t.Errorf("idle_remaining_seconds = %v, want null when the reaper is off",
			out.Previews[0].IdleRemainingSeconds)
	}
	if !strings.Contains(rec.Body.String(), `"idle_remaining_seconds":null`) {
		t.Errorf("expected an explicit null countdown in %s", rec.Body.String())
	}
}

func TestPreviewsList_FailedServiceReportsZeroUptime(t *testing.T) {
	created := time.Now().Add(-5 * time.Minute).UTC()
	p := samplePreview(created, created)
	p.Status = state.PreviewDegraded
	p.Services[0].Health = state.PreviewServiceFailed
	p.Services[0].Error = "health check timed out"
	srv := newServerWithDefaults(t, previewsHandler(t, ipc.PreviewsResponse{
		Enabled:  true,
		Previews: []ipc.PreviewInfo{p},
	}))

	out := decodePreviews(t, getPreviews(t, srv, "hearth.example:9000"))
	api := out.Previews[0].Services[0]
	if api.UptimeSeconds != 0 {
		t.Errorf("failed service uptime_seconds = %d, want 0", api.UptimeSeconds)
	}
	if api.Error != "health check timed out" {
		t.Errorf("error = %q, want the service failure detail", api.Error)
	}
}

// Forge-bci1: a service that became healthy and later died must report the
// lifetime it had — frozen — rather than a clock that keeps counting over a
// dead process, and the browser must not be offered its address.
func TestPreviewsList_ExitedServiceFreezesUptimeAndWithholdsTheLink(t *testing.T) {
	created := time.Now().Add(-10 * time.Minute).UTC()
	startedAt := created
	exitedAt := created.Add(7*time.Minute + 31*time.Second)
	code := 1

	p := samplePreview(created, created)
	p.Status = state.PreviewDegraded
	p.Services[0].StartedAt = startedAt
	// The entry service is the one that died, so there is no link to give.
	p.Services[1].Health = state.PreviewServiceExited
	p.Services[1].Error = "exited (exit 1, lived 7m31s)"
	p.Services[1].StartedAt = startedAt
	p.Services[1].ExitedAt = exitedAt
	p.Services[1].ExitCode = &code
	p.EntryNote = `entry service "client" is not serving: exited (exit 1, lived 7m31s)`

	srv := newServerWithDefaults(t, previewsHandler(t, ipc.PreviewsResponse{
		Enabled:  true,
		Previews: []ipc.PreviewInfo{p},
	}))

	out := decodePreviews(t, getPreviews(t, srv, "hearth.example:9000"))
	preview := out.Previews[0]
	if preview.EntryURL != "" {
		t.Errorf("entry_url = %q, want it withheld once the entry service exited", preview.EntryURL)
	}
	if !strings.Contains(preview.EntryNote, "not serving") {
		t.Errorf("entry_note = %q, want it to explain the withheld link", preview.EntryNote)
	}

	client := preview.Services[1]
	if client.Health != state.PreviewServiceExited {
		t.Errorf("health = %q, want %q", client.Health, state.PreviewServiceExited)
	}
	if client.UptimeSeconds != 451 {
		t.Errorf("uptime_seconds = %d, want 451 (7m31s frozen at the exit)", client.UptimeSeconds)
	}
	if client.ExitCode == nil || *client.ExitCode != 1 {
		t.Errorf("exit_code = %v, want 1", client.ExitCode)
	}
	if client.ExitedAt == nil || !client.ExitedAt.Equal(exitedAt) {
		t.Errorf("exited_at = %v, want %v", client.ExitedAt, exitedAt)
	}

	// The surviving sibling keeps counting, measured from its own start.
	if up := preview.Services[0].UptimeSeconds; up < 600 || up > 900 {
		t.Errorf("live service uptime_seconds = %d, want ~600", up)
	}
	if preview.Services[0].ExitedAt != nil {
		t.Errorf("live service reported exited_at = %v", preview.Services[0].ExitedAt)
	}
}

func TestPreviewsList_EntryURLUsesConfiguredPublicHost(t *testing.T) {
	now := time.Now().UTC()
	srv := newServerWithDefaults(t, previewsHandler(t, ipc.PreviewsResponse{
		Enabled:    true,
		PublicHost: "forge.wg",
		Previews:   []ipc.PreviewInfo{samplePreview(now, now)},
	}))

	out := decodePreviews(t, getPreviews(t, srv, "hearth.example:9000"))
	// The entry service is "client" on 42002 — not the first service.
	if got, want := out.Previews[0].EntryURL, "http://forge.wg:42002/"; got != want {
		t.Errorf("entry_url = %q, want %q", got, want)
	}
}

func TestPreviewsList_EntryURLFallsBackToRequestHost(t *testing.T) {
	now := time.Now().UTC()
	srv := newServerWithDefaults(t, previewsHandler(t, ipc.PreviewsResponse{
		Enabled:  true,
		Previews: []ipc.PreviewInfo{samplePreview(now, now)},
	}))

	// Hearth's own port is dropped; the preview's port is used instead.
	out := decodePreviews(t, getPreviews(t, srv, "hearth.example:9000"))
	if got, want := out.Previews[0].EntryURL, "http://hearth.example:42002/"; got != want {
		t.Errorf("entry_url = %q, want %q", got, want)
	}
}

func TestPreviewsList_EntryURLFallsBackToFirstPortedService(t *testing.T) {
	now := time.Now().UTC()
	p := samplePreview(now, now)
	// A single-service manifest needs no `entry: true` — link to it anyway.
	p.Services = []ipc.PreviewServiceInfo{{Name: "api", Port: 42001, Health: state.PreviewServiceHealthy}}
	srv := newServerWithDefaults(t, previewsHandler(t, ipc.PreviewsResponse{
		Enabled:  true,
		Previews: []ipc.PreviewInfo{p},
	}))

	out := decodePreviews(t, getPreviews(t, srv, "hearth.example"))
	if got, want := out.Previews[0].EntryURL, "http://hearth.example:42001/"; got != want {
		t.Errorf("entry_url = %q, want %q", got, want)
	}
}

func TestPreviewsList_NoEntryURLWithoutPorts(t *testing.T) {
	now := time.Now().UTC()
	p := samplePreview(now, now)
	p.Status = state.PreviewStarting
	p.Services = nil
	srv := newServerWithDefaults(t, previewsHandler(t, ipc.PreviewsResponse{
		Enabled:  true,
		Previews: []ipc.PreviewInfo{p},
	}))

	out := decodePreviews(t, getPreviews(t, srv, "hearth.example:9000"))
	if out.Previews[0].EntryURL != "" {
		t.Errorf("entry_url = %q, want empty when nothing has a port", out.Previews[0].EntryURL)
	}
}

func TestPreviewsList_DaemonErrorSurfacesAs500(t *testing.T) {
	handler := func(cmd ipc.Command) ipc.Response {
		return ipc.Response{Type: "error", Payload: []byte(`{"message":"kiln exploded"}`)}
	}
	srv := newServerWithDefaults(t, handler)
	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", "/api/previews", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "kiln exploded" {
		t.Errorf("error = %q, want the daemon's message", body["error"])
	}
}

func TestPreviewLog_TailsServiceLog(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newServerWithDefaults(t, nil)
	writePreservedLog(t, "Forge-prv1", "preview-api.log", "line one\nline two\nline three\n", time.Now())

	rec := authedGet(t, srv, "/api/preview/Forge-prv1/log/api?tail=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(body.Lines) != 2 || body.Lines[0] != "line two" || body.Lines[1] != "line three" {
		t.Errorf("lines = %#v, want the last two", body.Lines)
	}
}

func TestPreviewLog_UnknownServiceIs404(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newServerWithDefaults(t, nil)
	writePreservedLog(t, "Forge-prv1", "preview-api.log", "hi\n", time.Now())

	rec := authedGet(t, srv, "/api/preview/Forge-prv1/log/client")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPreviewLog_RejectsMalformedInput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := newServerWithDefaults(t, nil)

	// A traversal attempt in the service segment must be rejected as a bad
	// service name, never turned into a filename.
	rec := authedGet(t, srv, "/api/preview/Forge-prv1/log/..%2F..%2Fdaemon")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("service traversal: expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	rec = authedGet(t, srv, "/api/preview/..%2Fetc/log/api")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bead traversal: expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}
