package web

import (
	"bytes"
	"encoding/json"
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
		"/api/bead/Forge-abc1/close",
		"/api/bead/Forge-abc1/label/add",
		"/api/bead/Forge-abc1/label/remove",
		"/api/bead/Forge-abc1/note",
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
