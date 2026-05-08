package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
	"golang.org/x/crypto/bcrypt"
)

func TestHealthz_NoAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPI_RequiresAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	for _, path := range []string{"/api/status", "/api/queue", "/api/workers", "/api/me", "/api/events"} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: expected 401, got %d", path, rec.Code)
		}
	}
}

func TestLogin_BadCredentials(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	rec := postLogin(srv, "alice", "wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestLogin_OK_SetsCookie(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	rec := postLogin(srv, "alice", "hunter2")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cookie := extractSessionCookie(t, rec.Result(), "forge_session")
	if cookie == "" {
		t.Fatalf("expected session cookie in response")
	}
}

func TestLoginThenStatus_AuthenticatedAccess(t *testing.T) {
	statusPayload := `{"running":true,"pid":42}`
	handler := func(cmd ipc.Command) ipc.Response {
		if cmd.Type != "status" {
			return ipc.Response{Type: "error", Payload: []byte(`{"message":"unexpected"}`)}
		}
		return ipc.Response{Type: "status", Payload: []byte(statusPayload)}
	}
	srv := newServerWithDefaults(t, handler)

	loginRec := postLogin(srv, "alice", "hunter2")
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login failed: %d", loginRec.Code)
	}
	cookie := extractSessionCookie(t, loginRec.Result(), "forge_session")

	statusReq := httptest.NewRequest("GET", "/api/status", nil)
	statusReq.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	statusRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status request failed: %d", statusRec.Code)
	}
	body, _ := io.ReadAll(statusRec.Body)
	if string(body) != statusPayload {
		t.Errorf("expected payload to pass through unchanged, got %q", string(body))
	}
}

func TestSessionPersistsAcrossNewServer(t *testing.T) {
	dbPath := t.TempDir() + "/state.db"
	db, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	hash, _ := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	users := map[string]string{"alice": string(hash)}
	handler := func(cmd ipc.Command) ipc.Response {
		return ipc.Response{Type: "status", Payload: []byte(`{"ok":true}`)}
	}

	// First server instance: log in.
	srv1, err := New(Config{Addr: ":0", Users: users}, db, handler, slog.Default())
	if err != nil {
		t.Fatalf("server1: %v", err)
	}
	loginRec := postLogin(srv1, "alice", "hunter2")
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login failed: %d", loginRec.Code)
	}
	cookie := extractSessionCookie(t, loginRec.Result(), "forge_session")

	// Second server instance using the same DB simulates a daemon
	// restart. The session cookie should still be valid.
	srv2, err := New(Config{Addr: ":0", Users: users}, db, handler, slog.Default())
	if err != nil {
		t.Fatalf("server2: %v", err)
	}
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv2.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after restart, got %d", rec.Code)
	}
}

func TestLogout_ClearsSession(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	loginRec := postLogin(srv, "alice", "hunter2")
	cookie := extractSessionCookie(t, loginRec.Result(), "forge_session")

	logoutReq := httptest.NewRequest("POST", "/logout", nil)
	logoutReq.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	logoutRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("logout: %d", logoutRec.Code)
	}

	// Session should now be invalid.
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 after logout, got %d", rec.Code)
	}
}

func TestLoginGet_BrowserAuthenticated_RedirectsToRoot(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/login", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("expected Location=/, got %q", loc)
	}
}

func TestLoginGet_BrowserUnauthenticated_ServesSPA(t *testing.T) {
	srv := newServerWithDefaults(t, nil)

	req := httptest.NewRequest("GET", "/login", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<!doctype html>") && !strings.Contains(body, "<!DOCTYPE html>") {
		t.Errorf("expected HTML body, got %q", body)
	}
}

func TestLoginGet_FetchAuthenticated_ReturnsJSON(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/login", nil)
	// No Accept header — mirrors fetch()'s default of */*.
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body struct {
		Authenticated bool   `json:"authenticated"`
		User          string `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !body.Authenticated || body.User != "alice" {
		t.Errorf("unexpected payload: %+v", body)
	}
}

func TestAuthStatus_ReturnsJSON(t *testing.T) {
	srv := newServerWithDefaults(t, nil)

	// Unauthenticated probe.
	req := httptest.NewRequest("GET", "/api/auth/status", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unauth: expected 200, got %d", rec.Code)
	}
	var body struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unauth parse: %v", err)
	}
	if body.Authenticated {
		t.Errorf("expected authenticated=false, got %+v", body)
	}

	// Authenticated probe.
	cookie := loginAndGetCookie(t, srv)
	req = httptest.NewRequest("GET", "/api/auth/status", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth: expected 200, got %d", rec.Code)
	}
	var ok struct {
		Authenticated bool   `json:"authenticated"`
		User          string `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatalf("auth parse: %v", err)
	}
	if !ok.Authenticated || ok.User != "alice" {
		t.Errorf("expected authenticated=true user=alice, got %+v", ok)
	}
}

func TestEventsEndpointForwardsIPC(t *testing.T) {
	eventsPayload := `{"events":[{"id":1,"timestamp":"2026-05-07T12:00:00Z","type":"bead_claimed","message":"alice claimed bd-1","bead_id":"bd-1","anvil":"forge"}]}`
	var seenLimit int
	handler := func(cmd ipc.Command) ipc.Response {
		if cmd.Type != "events" {
			return ipc.Response{Type: "error", Payload: []byte(`{"message":"unexpected"}`)}
		}
		if len(cmd.Payload) > 0 {
			var p struct {
				Limit int `json:"limit"`
			}
			if err := json.Unmarshal(cmd.Payload, &p); err != nil {
				t.Errorf("unmarshal events payload: %v", err)
			}
			seenLimit = p.Limit
		}
		return ipc.Response{Type: "ok", Payload: []byte(eventsPayload)}
	}
	srv := newServerWithDefaults(t, handler)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/api/events?limit=25", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("events: %d body=%s", rec.Code, rec.Body.String())
	}
	if seenLimit != 25 {
		t.Errorf("expected daemon to receive limit=25, got %d", seenLimit)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	events, ok := got["events"].([]any)
	if !ok || len(events) != 1 {
		t.Errorf("expected 1 event, got %v", got)
	}
}

func TestQueueEndpointForwardsIPC(t *testing.T) {
	queuePayload := `{"items":[{"bead_id":"X","anvil":"Y","title":"T","priority":2,"status":"open","labels":[],"section":"ready"}]}`
	handler := func(cmd ipc.Command) ipc.Response {
		switch cmd.Type {
		case "queue":
			return ipc.Response{Type: "ok", Payload: []byte(queuePayload)}
		}
		return ipc.Response{Type: "error", Payload: []byte(`{"message":"unexpected"}`)}
	}
	srv := newServerWithDefaults(t, handler)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/api/queue", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("queue: %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	items, ok := got["items"].([]any)
	if !ok || len(items) != 1 {
		t.Errorf("unexpected items: %v", got)
	}
}

// --- helpers ---

func newServerWithDefaults(t *testing.T, handler CommandHandler) *Server {
	t.Helper()
	dbPath := t.TempDir() + "/state.db"
	db, err := state.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if handler == nil {
		handler = func(cmd ipc.Command) ipc.Response {
			return ipc.Response{Type: "ok", Payload: []byte(`{}`)}
		}
	}
	srv, err := New(Config{
		Addr:  ":0",
		Users: map[string]string{"alice": string(hash)},
	}, db, handler, slog.Default())
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv
}

func postLogin(srv *Server, user, pass string) *httptest.ResponseRecorder {
	form := url.Values{}
	form.Set("user", user)
	form.Set("password", pass)
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func loginAndGetCookie(t *testing.T, srv *Server) string {
	t.Helper()
	rec := postLogin(srv, "alice", "hunter2")
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed: %d", rec.Code)
	}
	return extractSessionCookie(t, rec.Result(), "forge_session")
}

func extractSessionCookie(t *testing.T, resp *http.Response, name string) string {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}
