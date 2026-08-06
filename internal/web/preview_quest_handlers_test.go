package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/ipc"
)

// questHandler answers the two quest commands from the supplied payloads and
// records what it was asked, so a test cannot pass because some unrelated
// command happened to be dispatched.
type questHandler struct {
	runResp    any
	statusResp any
	// runErr, when set, is returned instead of runResp as an IPC error.
	runErr string
	cmds   []ipc.Command
}

func (h *questHandler) handle(cmd ipc.Command) ipc.Response {
	h.cmds = append(h.cmds, cmd)
	switch cmd.Type {
	case "preview_quest_run":
		if h.runErr != "" {
			return ipc.Response{Type: "error", Payload: []byte(`{"message":"` + h.runErr + `"}`)}
		}
		return okPayload(h.runResp)
	case "preview_quest_status":
		return okPayload(h.statusResp)
	default:
		return ipc.Response{Type: "error", Payload: []byte(`{"message":"unexpected command"}`)}
	}
}

func (h *questHandler) types() []string {
	out := make([]string, 0, len(h.cmds))
	for _, c := range h.cmds {
		out = append(out, c.Type)
	}
	return out
}

func okPayload(v any) ipc.Response {
	if v == nil {
		return ipc.Response{Type: "ok", Payload: []byte(`{}`)}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ipc.Response{Type: "error", Payload: []byte(`{"message":"marshal"}`)}
	}
	return ipc.Response{Type: "ok", Payload: raw}
}

func getAuthed(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	cookie := loginAndGetCookie(t, srv)
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func TestQuestRoutes_RequireAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cases := []struct{ method, path string }{
		{"POST", "/api/bead/Forge-abc1/quests"},
		{"GET", "/api/bead/Forge-abc1/quests"},
		{"GET", "/api/bead/Forge-abc1/quests/qr-1-1"},
		{"GET", "/api/bead/Forge-abc1/quests/qr-1-1/screenshot/0"},
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

// TestQuestRunStart_RejectedGatesAre403 is the contract behind hiding the "Run
// quests" action: the two gates it is offered behind — the anvil's
// preview_quests opt-in and a healthy preview — are refusals, not failures, and
// a client that posts anyway is told so with a 403 rather than a 500.
func TestQuestRunStart_RejectedGatesAre403(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   int
	}{
		{"flag off", ipc.PreviewQuestRejectNotEnabled, http.StatusForbidden},
		{"preview unhealthy", ipc.PreviewQuestRejectNotHealthy, http.StatusForbidden},
		{"no preview", ipc.PreviewQuestRejectNoPreview, http.StatusNotFound},
		{"previews disabled", ipc.PreviewQuestRejectDisabled, http.StatusNotFound},
		{"already running", ipc.PreviewQuestRejectAlreadyRunning, http.StatusConflict},
		{"questgiver unwired", ipc.PreviewQuestRejectUnavailable, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &questHandler{runResp: ipc.PreviewQuestRunResponse{
				BeadID:  "Forge-abc1",
				Reason:  tc.reason,
				Message: "refused: " + tc.reason,
			}}
			srv := newServerWithDefaults(t, h.handle)
			cookie := loginAndGetCookie(t, srv)

			rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/quests", map[string]string{})
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if body["error"] != "refused: "+tc.reason {
				t.Errorf("error = %v, want the daemon's message", body["error"])
			}
		})
	}
}

// TestQuestRunStart_HappyPathDispatches pins the async contract: the handler
// answers 202 with a run id as soon as the daemon accepts the run, and does not
// wait for the quests to finish.
func TestQuestRunStart_HappyPathDispatches(t *testing.T) {
	started := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	h := &questHandler{runResp: ipc.PreviewQuestRunResponse{
		Started: true,
		BeadID:  "Forge-abc1",
		RunID:   "qr-17-3",
		Message: "running forge quests against the preview for Forge-abc1",
		Run: &ipc.PreviewQuestRun{
			RunID:     "qr-17-3",
			BeadID:    "Forge-abc1",
			Anvil:     "forge",
			BaseURL:   "http://forge-box:42001",
			Status:    "running",
			StartedAt: started,
			Quests:    []ipc.PreviewQuestOutcome{},
		},
	}}
	srv := newServerWithDefaults(t, h.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/quests", map[string]string{})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body=%s)", rec.Code, rec.Body.String())
	}
	var body QuestRunStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !body.Started || body.RunID != "qr-17-3" {
		t.Fatalf("body = %+v, want started with run id qr-17-3", body)
	}
	if body.Run == nil || body.Run.Status != "running" {
		t.Fatalf("run = %+v, want the freshly created running record", body.Run)
	}

	if got := h.types(); len(got) != 1 || got[0] != "preview_quest_run" {
		t.Fatalf("dispatched %v, want exactly one preview_quest_run", got)
	}
	var payload ipc.PreviewQuestRunPayload
	if err := json.Unmarshal(h.cmds[0].Payload, &payload); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	if payload.BeadID != "Forge-abc1" {
		t.Errorf("payload bead = %q, want Forge-abc1", payload.BeadID)
	}
}

func TestQuestRunStart_InvalidBeadID(t *testing.T) {
	h := &questHandler{}
	srv := newServerWithDefaults(t, h.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/..%2Fetc/quests", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(h.cmds) != 0 {
		t.Errorf("dispatched %v, want no IPC for an invalid bead id", h.types())
	}
}

// failedRun is a run with mixed per-quest outcomes and a screenshot, which is
// what the panel renders for a red run.
func failedRun() ipc.PreviewQuestRun {
	finished := time.Date(2026, 8, 6, 10, 2, 30, 0, time.UTC)
	return ipc.PreviewQuestRun{
		RunID:     "qr-17-3",
		BeadID:    "Forge-abc1",
		Anvil:     "forge",
		PreviewID: "Forge-abc1",
		BaseURL:   "http://forge-box:42001",
		Status:    "failed",
		StartedAt: finished.Add(-150 * time.Second),
		FinishedAt: func() *time.Time {
			f := finished
			return &f
		}(),
		DurationSeconds: 150,
		Quests: []ipc.PreviewQuestOutcome{
			{
				Name:            "login",
				Passed:          true,
				FailedStep:      -1,
				DurationSeconds: 12.5,
				Screenshots:     []string{"/tmp/shots/login.png"},
			},
			{
				Name:            "checkout",
				Passed:          false,
				FailedStep:      3,
				ErrorMessage:    "assert failed: expected 'Order placed'",
				DurationSeconds: 30,
				Screenshots:     []string{"/tmp/shots/checkout-1.png", "/tmp/shots/checkout-2.png"},
			},
		},
	}
}

// TestQuestRunStatus_FailedRun covers what the panel needs to render a red run:
// per-quest verdicts with the failing step and message, and screenshots
// addressed by an endpoint on this server rather than by their path on disk.
func TestQuestRunStatus_FailedRun(t *testing.T) {
	run := failedRun()
	h := &questHandler{statusResp: ipc.PreviewQuestStatusResponse{Found: true, Run: &run}}
	srv := newServerWithDefaults(t, h.handle)

	rec := getAuthed(t, srv, "/api/bead/Forge-abc1/quests")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body QuestRunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !body.Found || body.Run == nil {
		t.Fatalf("body = %+v, want a run", body)
	}
	if body.Run.Status != "failed" {
		t.Errorf("status = %q, want failed", body.Run.Status)
	}
	if len(body.Run.Quests) != 2 {
		t.Fatalf("quests = %d, want 2", len(body.Run.Quests))
	}
	if !body.Run.Quests[0].Passed || body.Run.Quests[1].Passed {
		t.Errorf("verdicts = %v/%v, want pass then fail",
			body.Run.Quests[0].Passed, body.Run.Quests[1].Passed)
	}
	if body.Run.Quests[1].FailedStep != 3 ||
		body.Run.Quests[1].ErrorMessage != "assert failed: expected 'Order placed'" {
		t.Errorf("failure detail = %+v, want step 3 and the assert message", body.Run.Quests[1])
	}

	// Screenshots are numbered run-wide in quest-then-step order, and no
	// filesystem path reaches the client.
	shots := append(body.Run.Quests[0].Screenshots, body.Run.Quests[1].Screenshots...)
	want := []string{
		"/api/bead/Forge-abc1/quests/qr-17-3/screenshot/0",
		"/api/bead/Forge-abc1/quests/qr-17-3/screenshot/1",
		"/api/bead/Forge-abc1/quests/qr-17-3/screenshot/2",
	}
	if len(shots) != len(want) {
		t.Fatalf("screenshots = %d, want %d", len(shots), len(want))
	}
	for i, shot := range shots {
		if shot.URL != want[i] {
			t.Errorf("screenshot %d url = %q, want %q", i, shot.URL, want[i])
		}
		if filepath.IsAbs(shot.Name) {
			t.Errorf("screenshot %d name = %q, want a base name, not a path", i, shot.Name)
		}
	}
	if shots[2].Name != "checkout-2.png" {
		t.Errorf("screenshot 2 name = %q, want checkout-2.png", shots[2].Name)
	}
}

// TestQuestRunStatus_NotFound: a bead that has never had a run is a normal 200
// with found=false, not an error — most beads are in exactly that state.
func TestQuestRunStatus_NotFound(t *testing.T) {
	h := &questHandler{statusResp: ipc.PreviewQuestStatusResponse{}}
	srv := newServerWithDefaults(t, h.handle)

	rec := getAuthed(t, srv, "/api/bead/Forge-abc1/quests")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body QuestRunResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if body.Found || body.Run != nil {
		t.Fatalf("body = %+v, want found=false with no run", body)
	}
}

func TestQuestRunStatus_InvalidRunID(t *testing.T) {
	h := &questHandler{}
	srv := newServerWithDefaults(t, h.handle)

	rec := getAuthed(t, srv, "/api/bead/Forge-abc1/quests/not-a-run-id")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(h.cmds) != 0 {
		t.Errorf("dispatched %v, want no IPC for an invalid run id", h.types())
	}
}

// TestQuestScreenshot_ServesTheIndexedImage: the client addresses a screenshot
// by position, and the handler resolves that to the path the run recorded.
func TestQuestScreenshot_ServesTheIndexedImage(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "checkout-1.png")
	if err := os.WriteFile(png, []byte("\x89PNG\r\n\x1a\nfake"), 0o600); err != nil {
		t.Fatalf("write screenshot: %v", err)
	}
	run := failedRun()
	run.Quests[1].Screenshots = []string{png}
	run.Quests[0].Screenshots = nil
	h := &questHandler{statusResp: ipc.PreviewQuestStatusResponse{Found: true, Run: &run}}
	srv := newServerWithDefaults(t, h.handle)

	rec := getAuthed(t, srv, "/api/bead/Forge-abc1/quests/qr-17-3/screenshot/0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("content-type = %q, want image/png", ct)
	}
	if rec.Body.String() != "\x89PNG\r\n\x1a\nfake" {
		t.Errorf("body = %q, want the file's bytes", rec.Body.String())
	}
}

// TestQuestScreenshot_RefusesNonImages: quest files live in the anvil and may
// name any path, so the endpoint still refuses to stream something that is not
// an image.
func TestQuestScreenshot_RefusesNonImages(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	run := failedRun()
	run.Quests[0].Screenshots = []string{secret}
	run.Quests[1].Screenshots = nil
	h := &questHandler{statusResp: ipc.PreviewQuestStatusResponse{Found: true, Run: &run}}
	srv := newServerWithDefaults(t, h.handle)

	rec := getAuthed(t, srv, "/api/bead/Forge-abc1/quests/qr-17-3/screenshot/0")
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() == "PRIVATE KEY" {
		t.Fatal("the file's contents were served")
	}
}

func TestQuestScreenshot_OutOfRange(t *testing.T) {
	run := failedRun()
	h := &questHandler{statusResp: ipc.PreviewQuestStatusResponse{Found: true, Run: &run}}
	srv := newServerWithDefaults(t, h.handle)

	rec := getAuthed(t, srv, "/api/bead/Forge-abc1/quests/qr-17-3/screenshot/9")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestPreviewsList_CarriesQuestAnvils: the SPA gates the "Run quests" action on
// this list rather than probing per bead, so it has to survive the mapping.
func TestPreviewsList_CarriesQuestAnvils(t *testing.T) {
	srv := newServerWithDefaults(t, previewsHandler(t, ipc.PreviewsResponse{
		Enabled:     true,
		Anvils:      []string{"forge", "hytte"},
		QuestAnvils: []string{"forge"},
		Previews:    []ipc.PreviewInfo{},
	}))

	out := decodePreviews(t, getPreviews(t, srv, "forge-box:8080"))
	if len(out.QuestAnvils) != 1 || out.QuestAnvils[0] != "forge" {
		t.Fatalf("quest_anvils = %v, want [forge]", out.QuestAnvils)
	}
}

// TestPreviewsList_QuestAnvilsNeverNull keeps the SPA free of a nil check on a
// field it filters with.
func TestPreviewsList_QuestAnvilsNeverNull(t *testing.T) {
	srv := newServerWithDefaults(t, previewsHandler(t, ipc.PreviewsResponse{Enabled: true}))

	rec := getPreviews(t, srv, "forge-box:8080")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if string(raw["quest_anvils"]) != "[]" {
		t.Errorf("quest_anvils = %s, want []", raw["quest_anvils"])
	}
}
