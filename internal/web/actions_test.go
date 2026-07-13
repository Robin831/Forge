package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Robin831/Forge/internal/ipc"
)

// recordingHandler captures the last command dispatched through the
// in-process IPC handler so action tests can assert on the wire payload.
type recordingHandler struct {
	cmd  atomic.Value // ipc.Command
	resp ipc.Response
}

func (h *recordingHandler) handle(cmd ipc.Command) ipc.Response {
	h.cmd.Store(cmd)
	if h.resp.Type == "" {
		return ipc.Response{Type: "ok", Payload: []byte(`{"message":"ok"}`)}
	}
	return h.resp
}

func (h *recordingHandler) lastCommand() (ipc.Command, bool) {
	v := h.cmd.Load()
	if v == nil {
		return ipc.Command{}, false
	}
	cmd, ok := v.(ipc.Command)
	return cmd, ok
}

// postAction is a helper that posts a JSON body to an authenticated endpoint
// and returns the recorder. The cookie comes from a shared login.
func postAction(t *testing.T, srv *Server, cookie, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	var reader io.Reader
	if raw != nil {
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest("POST", path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forge-Action", "1")
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func TestActions_RequireCSRFHeader(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	// POST without X-Forge-Action header should be rejected with 403.
	req := httptest.NewRequest("POST", "/api/bead/Forge-abc1/close", strings.NewReader(`{"anvil":"forge"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 without CSRF header, got %d", rec.Code)
	}
}

func TestActions_RequireAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	for _, path := range []string{
		"/api/worker/abc/kill",
		"/api/queue/Forge-abc1/retry",
		"/api/queue/Forge-abc1/dispatch",
		"/api/queue/Forge-abc1/clarify",
		"/api/queue/Forge-abc1/unclarify",
		"/api/queue/Forge-abc1/stop",
		"/api/queue/Forge-abc1/apply-dispatch-tag",
		"/api/bead/Forge-abc1/close",
		"/api/bead/Forge-abc1/label/add",
		"/api/bead/Forge-abc1/label/remove",
		"/api/bead/Forge-abc1/note",
		"/api/bead/Forge-abc1/comment",
		"/api/bead/Forge-abc1/steer",
		"/api/bead/Forge-abc1/pause",
		"/api/bead/Forge-abc1/resume",
	} {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: expected 401, got %d", path, rec.Code)
		}
	}
}

func TestActions_KillWorker(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/worker/worker-abc-123/kill", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, ok := rh.lastCommand()
	if !ok || cmd.Type != "kill_worker" {
		t.Fatalf("expected kill_worker, got %v", cmd)
	}
	var p ipc.KillWorkerPayload
	if err := json.Unmarshal(cmd.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.WorkerID != "worker-abc-123" {
		t.Errorf("worker_id mismatch: got %q", p.WorkerID)
	}
}

func TestActions_KillWorker_InvalidID(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/worker/..%2Fevil/kill", nil)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Errorf("expected 400 or 404 for invalid id, got %d", rec.Code)
	}
}

func TestActions_QueueRetry_RequiresAnvil(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/queue/Forge-abc1/retry", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (missing anvil), got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestActions_QueueRetry_OK(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/queue/Forge-abc1/retry", map[string]any{
		"anvil": "forge",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	if cmd.Type != "retry_bead" {
		t.Fatalf("expected retry_bead, got %s", cmd.Type)
	}
	var p ipc.RetryBeadPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.BeadID != "Forge-abc1" || p.Anvil != "forge" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestActions_QueueDispatch_ForceRun(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/queue/Forge-abc1/dispatch", map[string]any{
		"anvil":     "forge",
		"force_run": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	if cmd.Type != "run_bead" {
		t.Fatalf("expected run_bead, got %s", cmd.Type)
	}
	var p ipc.RunBeadPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if !p.ForceRun || p.BeadID != "Forge-abc1" || p.Anvil != "forge" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestActions_QueueClarify_RequiresReason(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/queue/Forge-abc1/clarify", map[string]any{
		"anvil": "forge",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (missing reason), got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestActions_QueueClarify_OK(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/queue/Forge-abc1/clarify", map[string]any{
		"anvil":  "forge",
		"reason": "spec is ambiguous",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	cmd, _ := rh.lastCommand()
	var p ipc.ClarificationPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.BeadID != "Forge-abc1" || p.Anvil != "forge" || p.Reason != "spec is ambiguous" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestActions_QueueUnclarify_OK(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/queue/Forge-abc1/unclarify", map[string]any{
		"anvil": "forge",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	cmd, _ := rh.lastCommand()
	if cmd.Type != "clear_clarification" {
		t.Fatalf("expected clear_clarification, got %s", cmd.Type)
	}
}

func TestActions_QueueStop_OK(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/queue/Forge-abc1/stop", map[string]any{
		"anvil":  "forge",
		"reason": "rolling back",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	if cmd.Type != "stop_bead" {
		t.Fatalf("expected stop_bead, got %s", cmd.Type)
	}
	var p ipc.StopBeadPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.BeadID != "Forge-abc1" || p.Anvil != "forge" || p.Reason != "rolling back" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestActions_BeadClose_OK(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/close", map[string]any{
		"anvil": "forge",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	cmd, _ := rh.lastCommand()
	if cmd.Type != "close_bead" {
		t.Fatalf("expected close_bead, got %s", cmd.Type)
	}
}

func TestActions_BeadLabelAdd_RequiresLabel(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/label/add", map[string]any{
		"anvil": "forge",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestActions_BeadLabelAdd_OK(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/label/add", map[string]any{
		"anvil": "forge",
		"label": "forgeReady",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	if cmd.Type != "update_label" {
		t.Fatalf("expected update_label, got %s", cmd.Type)
	}
	var p ipc.UpdateLabelPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.Action != "add" || p.Label != "forgeReady" || p.BeadID != "Forge-abc1" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestActions_BeadLabelRemove_OK(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/label/remove", map[string]any{
		"anvil": "forge",
		"label": "blocked",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	cmd, _ := rh.lastCommand()
	var p ipc.UpdateLabelPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.Action != "remove" || p.Label != "blocked" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestActions_BeadLabel_RejectsBadCharacters(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/label/add", map[string]any{
		"anvil": "forge",
		"label": "evil; rm -rf /",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad label, got %d", rec.Code)
	}
}

func TestActions_QueueApplyDispatchTag_OK(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	srv.SetAnvilDispatchTagLister(func() map[string]string {
		return map[string]string{"forge": "forgeReady"}
	})
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/queue/Forge-abc1/apply-dispatch-tag", map[string]any{
		"anvil": "forge",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, ok := rh.lastCommand()
	if !ok || cmd.Type != "update_label" {
		t.Fatalf("expected update_label, got %v", cmd)
	}
	var p ipc.UpdateLabelPayload
	if err := json.Unmarshal(cmd.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.Action != "add" || p.Label != "forgeReady" || p.BeadID != "Forge-abc1" || p.Anvil != "forge" {
		t.Errorf("payload mismatch: %+v", p)
	}
	var body struct {
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if body.Tag != "forgeReady" {
		t.Errorf("response tag: got %q, want %q", body.Tag, "forgeReady")
	}
}

func TestActions_QueueApplyDispatchTag_PerAnvilTag(t *testing.T) {
	// Each anvil resolves its own tag — the same UI button applies
	// different labels depending on which anvil owns the bead.
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	srv.SetAnvilDispatchTagLister(func() map[string]string {
		return map[string]string{
			"hetzner": "forgeReady",
			"skybert": "forgeSkybert",
		}
	})
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/queue/Forge-abc1/apply-dispatch-tag", map[string]any{
		"anvil": "skybert",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	var p ipc.UpdateLabelPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.Label != "forgeSkybert" {
		t.Errorf("expected forgeSkybert, got %q", p.Label)
	}
}

func TestActions_QueueApplyDispatchTag_RequiresAnvil(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	srv.SetAnvilDispatchTagLister(func() map[string]string {
		return map[string]string{"forge": "forgeReady"}
	})
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/queue/Forge-abc1/apply-dispatch-tag", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (missing anvil), got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestActions_QueueApplyDispatchTag_NoTagConfigured(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	srv.SetAnvilDispatchTagLister(func() map[string]string {
		return map[string]string{} // anvil exists, but has no auto_dispatch_tag
	})
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/queue/Forge-abc1/apply-dispatch-tag", map[string]any{
		"anvil": "forge",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when tag is unset, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.Contains(body.Error, "auto_dispatch_tag") {
		t.Errorf("expected error to mention auto_dispatch_tag, got %q", body.Error)
	}
	if _, dispatched := rh.lastCommand(); dispatched {
		t.Errorf("update_label should not be dispatched when no tag is configured")
	}
}

func TestActions_QueueApplyDispatchTag_NoListerConfigured(t *testing.T) {
	// When SetAnvilDispatchTagLister is never called the handler must still
	// fail cleanly with a 400 rather than dispatching an empty-label IPC.
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/queue/Forge-abc1/apply-dispatch-tag", map[string]any{
		"anvil": "forge",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no lister, got %d", rec.Code)
	}
	if _, dispatched := rh.lastCommand(); dispatched {
		t.Errorf("update_label should not be dispatched without a tag lister")
	}
}

func TestActions_BeadNote_RequiresNote(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/note", map[string]any{
		"anvil": "forge",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestActions_BeadNote_OK(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/note", map[string]any{
		"anvil": "forge",
		"note":  "manual triage step",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	if cmd.Type != "append_notes" {
		t.Fatalf("expected append_notes, got %s", cmd.Type)
	}
	var p ipc.AppendNotesPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.Notes != "manual triage step" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestActions_BeadSteer_RequiresMessage(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/steer", map[string]any{
		"message": "   ",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestActions_BeadSteer_OK(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/steer", map[string]any{
		"message": "also update the README",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	if cmd.Type != "steer_bead" {
		t.Fatalf("expected steer_bead, got %s", cmd.Type)
	}
	var p ipc.SteerBeadPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.BeadID != "Forge-abc1" || p.Message != "also update the README" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestActions_BeadSteer_PropagatesDaemonError(t *testing.T) {
	rh := &recordingHandler{
		resp: ipc.Response{
			Type:    "error",
			Payload: []byte(`{"message":"no active pipeline for bead Forge-abc1; steering requires a running Smith worker"}`),
		},
	}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/steer", map[string]any{
		"message": "go left",
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no active pipeline") {
		t.Errorf("expected daemon error surfaced, got %s", rec.Body.String())
	}
}

func TestActions_BeadPause_OK(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/pause", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	if cmd.Type != "pause_bead" {
		t.Fatalf("expected pause_bead, got %s", cmd.Type)
	}
	var p ipc.PauseBeadPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.BeadID != "Forge-abc1" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestActions_BeadPause_InvalidID(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/not%20a%20bead/pause", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestActions_BeadPause_PropagatesDaemonError(t *testing.T) {
	rh := &recordingHandler{
		resp: ipc.Response{
			Type:    "error",
			Payload: []byte(`{"message":"bead Forge-abc1 cannot be paused from status \"failed\"; only a running bead may be paused"}`),
		},
	}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/pause", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cannot be paused") {
		t.Errorf("expected daemon error surfaced, got %s", rec.Body.String())
	}
}

func TestActions_BeadResume_OK(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/resume", map[string]any{
		"message": "carry on where you left off",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	if cmd.Type != "resume_bead" {
		t.Fatalf("expected resume_bead, got %s", cmd.Type)
	}
	var p ipc.ResumeBeadPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.BeadID != "Forge-abc1" || p.Message != "carry on where you left off" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestActions_BeadResume_OmittedMessageDispatchesEmpty(t *testing.T) {
	// An empty body is valid — the daemon substitutes its default resume prompt.
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/resume", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	if cmd.Type != "resume_bead" {
		t.Fatalf("expected resume_bead, got %s", cmd.Type)
	}
	var p ipc.ResumeBeadPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.BeadID != "Forge-abc1" || p.Message != "" {
		t.Errorf("expected empty message, got %+v", p)
	}
}

func TestActions_BeadResume_InvalidID(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/not%20a%20bead/resume", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestActions_QueuedResponseTranslatesTo202(t *testing.T) {
	rh := &recordingHandler{
		resp: ipc.Response{
			Type:      "queued",
			Payload:   []byte(`{"message":"closing bead"}`),
			RequestID: "forge-1",
		},
	}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/close", map[string]any{
		"anvil": "forge",
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Queued    bool   `json:"queued"`
		RequestID string `json:"request_id"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if !body.Queued || body.RequestID != "forge-1" || body.Message != "closing bead" {
		t.Errorf("unexpected body: %+v", body)
	}
}

func TestActions_DaemonErrorBubblesUp(t *testing.T) {
	rh := &recordingHandler{
		resp: ipc.Response{
			Type:    "error",
			Payload: []byte(`{"message":"anvil \"forge\" not found"}`),
		},
	}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/close", map[string]any{
		"anvil": "forge",
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Errorf("expected error message in body, got %s", rec.Body.String())
	}
}

func TestActions_InvalidBeadID(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	// chi will route this; our handler validates bead id shape.
	rec := postAction(t, srv, cookie, "/api/queue/!@#bad/retry", map[string]any{
		"anvil": "forge",
	})
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Errorf("expected 400 or 404, got %d", rec.Code)
	}
}

func TestActions_BeadAddComment_OK(t *testing.T) {
	var gotBeadID, gotBody string
	stubBdCommentsAdd(t, func(beadID, body string) ([]byte, error) {
		gotBeadID = beadID
		gotBody = body
		return []byte(`{
			"id": "comment-xyz",
			"issue_id": "Forge-abc1",
			"author": "Test User",
			"text": "Looks good to me",
			"created_at": "2026-05-14T08:00:00Z"
		}`), nil
	})

	srv := newServerWithDefaults(t, nil)
	srv.SetAnvilLister(func() map[string]string { return map[string]string{"forge": "/anvils/forge"} })
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/comment", map[string]any{
		"anvil": "forge",
		"body":  "Looks good to me",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	if gotBeadID != "Forge-abc1" {
		t.Errorf("bd invoked with wrong bead id: %q", gotBeadID)
	}
	if gotBody != "Looks good to me" {
		t.Errorf("bd invoked with wrong body: %q", gotBody)
	}

	var body struct {
		Comment struct {
			ID        string `json:"id"`
			Author    string `json:"author"`
			Body      string `json:"body"`
			CreatedAt string `json:"created_at"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if body.Comment.ID != "comment-xyz" {
		t.Errorf("comment id: got %q", body.Comment.ID)
	}
	if body.Comment.Author != "Test User" {
		t.Errorf("comment author: got %q", body.Comment.Author)
	}
	// The handler renames bd's `text` field to `body` so the SPA can render
	// it uniformly with other markdown-ish text blocks (matches the read path).
	if body.Comment.Body != "Looks good to me" {
		t.Errorf("comment body: got %q", body.Comment.Body)
	}
	if body.Comment.CreatedAt != "2026-05-14T08:00:00Z" {
		t.Errorf("comment created_at: got %q", body.Comment.CreatedAt)
	}
}

func TestActions_BeadAddComment_BdFailure(t *testing.T) {
	stubBdCommentsAdd(t, func(_, _ string) ([]byte, error) {
		return nil, errors.New("exit status 1: bd: issue not found")
	})

	srv := newServerWithDefaults(t, nil)
	srv.SetAnvilLister(func() map[string]string { return map[string]string{"forge": "/anvils/forge"} })
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/comment", map[string]any{
		"anvil": "forge",
		"body":  "anything",
	})
	if rec.Code < 500 || rec.Code > 599 {
		t.Fatalf("expected 5xx on bd failure, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse error body: %v", err)
	}
	if body.Error == "" || !strings.Contains(body.Error, "bd comments add") {
		t.Errorf("expected structured error containing 'bd comments add', got %q", body.Error)
	}
}

func TestActions_BeadAddComment_RequiresBody(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/comment", map[string]any{
		"anvil": "forge",
		"body":  "   ",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty body, got %d", rec.Code)
	}
}

func TestActions_BeadAddComment_RequiresAnvil(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/comment", map[string]any{
		"body": "hi",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing anvil, got %d", rec.Code)
	}
}

func TestActions_BeadAddComment_NoJSONFromBdReturns204(t *testing.T) {
	stubBdCommentsAdd(t, func(_, _ string) ([]byte, error) {
		// Older bd versions may print non-JSON output even with --json. The
		// handler treats this as a successful no-content write so the SPA
		// falls back to its next poll for the canonical list.
		return []byte("comment added\n"), nil
	})

	srv := newServerWithDefaults(t, nil)
	srv.SetAnvilLister(func() map[string]string { return map[string]string{"forge": "/anvils/forge"} })
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/comment", map[string]any{
		"anvil": "forge",
		"body":  "noted",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestActions_BeadAddComment_RejectsUnknownAnvil(t *testing.T) {
	// Without a guard, an unknown anvil name would resolve to an empty path
	// and `bd comments add` would run in the daemon's cwd against the wrong
	// beads DB. The handler must reject the request before reaching bd, even
	// if the bd shim were stubbed to succeed.
	called := false
	stubBdCommentsAdd(t, func(_, _ string) ([]byte, error) {
		called = true
		return []byte(`{}`), nil
	})

	srv := newServerWithDefaults(t, nil)
	srv.SetAnvilLister(func() map[string]string { return map[string]string{"forge": "/anvils/forge"} })
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/comment", map[string]any{
		"anvil": "ghost",
		"body":  "hi",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown anvil, got %d body=%s", rec.Code, rec.Body.String())
	}
	if called {
		t.Errorf("bd comments add should not be invoked for an unknown anvil")
	}
}

func TestActions_BeadAddComment_RejectsOversizedBody(t *testing.T) {
	// Bodies over 8 KiB are rejected outright so a single argv entry never
	// risks blowing past Windows' 32k command-line limit. The bd shim must
	// not be invoked.
	called := false
	stubBdCommentsAdd(t, func(_, _ string) ([]byte, error) {
		called = true
		return []byte(`{}`), nil
	})

	srv := newServerWithDefaults(t, nil)
	srv.SetAnvilLister(func() map[string]string { return map[string]string{"forge": "/anvils/forge"} })
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/comment", map[string]any{
		"anvil": "forge",
		"body":  strings.Repeat("x", 8*1024+1),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversize body, got %d body=%s", rec.Code, rec.Body.String())
	}
	if called {
		t.Errorf("bd comments add should not be invoked for an oversized body")
	}
}

func TestActions_BeadAddComment_ArrayResponseFromBd(t *testing.T) {
	// bd may return an array (the updated comment list) instead of a single
	// object. The handler should use the last element and return 201.
	stubBdCommentsAdd(t, func(_, _ string) ([]byte, error) {
		return []byte(`[
			{"id":"comment-old","author":"Alice","text":"first","created_at":"2026-05-14T07:00:00Z"},
			{"id":"comment-new","author":"Bob","text":"second","created_at":"2026-05-14T08:00:00Z"}
		]`), nil
	})

	srv := newServerWithDefaults(t, nil)
	srv.SetAnvilLister(func() map[string]string { return map[string]string{"forge": "/anvils/forge"} })
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/comment", map[string]any{
		"anvil": "forge",
		"body":  "second",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for array bd response, got %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Comment struct {
			ID   string `json:"id"`
			Body string `json:"body"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if body.Comment.ID != "comment-new" {
		t.Errorf("expected last array element id %q, got %q", "comment-new", body.Comment.ID)
	}
	if body.Comment.Body != "second" {
		t.Errorf("expected last array element body %q, got %q", "second", body.Comment.Body)
	}
}

func TestActions_BeadAddComment_CaseInsensitiveAnvil(t *testing.T) {
	// The registry uses lowercase "forge"; submitting with "Forge" must resolve.
	stubBdCommentsAdd(t, func(_, _ string) ([]byte, error) {
		return []byte(`{"id":"c1","author":"u","text":"hi","created_at":"2026-05-14T08:00:00Z"}`), nil
	})

	srv := newServerWithDefaults(t, nil)
	srv.SetAnvilLister(func() map[string]string { return map[string]string{"forge": "/anvils/forge"} })
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/comment", map[string]any{
		"anvil": "Forge",
		"body":  "hi",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for mixed-case anvil, got %d body=%s", rec.Code, rec.Body.String())
	}
}
