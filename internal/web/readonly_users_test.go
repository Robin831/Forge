package web

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
	"golang.org/x/crypto/bcrypt"
)

// A server with one full user (alice) and one read-only user (varde), whose
// command handler records every command that reaches it.
func newServerWithReader(t *testing.T) (*Server, *[]string) {
	t.Helper()
	db, err := state.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	hash := func(pw string) string {
		h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
		if err != nil {
			t.Fatalf("bcrypt: %v", err)
		}
		return string(h)
	}
	var commands []string
	srv, err := New(Config{
		Addr:          ":0",
		Users:         map[string]string{"alice": hash("hunter2")},
		ReadOnlyUsers: map[string]string{"varde": hash("readonly-pw")},
	}, db, func(cmd ipc.Command) ipc.Response {
		commands = append(commands, cmd.Type)
		return ipc.Response{Type: "ok", Payload: []byte(`{}`)}
	}, slog.Default())
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, &commands
}

func asUser(srv *Server, method, path, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-Forge-Action", "1")
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func TestReadOnlyUser_ReadsButNeverActs(t *testing.T) {
	srv, commands := newServerWithReader(t)
	rec := postLogin(srv, "varde", "readonly-pw")
	if rec.Code != http.StatusOK {
		t.Fatalf("read-only login: %d %s", rec.Code, rec.Body.String())
	}
	cookie := extractSessionCookie(t, rec.Result(), "forge_session")
	if got := asUser(srv, "GET", "/api/status", cookie).Code; got != http.StatusOK {
		t.Fatalf("GET /api/status as read-only: %d, want 200", got)
	}
	before := len(*commands)
	for _, path := range []string{"/api/dispatch/pause", "/api/worker/w1/kill", "/api/prs/1/merge", "/api/bead/X-1/close", "/api/queue/X-1/retry"} {
		rec := asUser(srv, "POST", path, cookie)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s as read-only: %d, want 403", path, rec.Code)
		}
	}
	if len(*commands) != before {
		t.Errorf("a read-only POST reached the command handler: %v", (*commands)[before:])
	}
}

func TestReadOnlyUser_FullUserStillActsAndWrongPasswordFails(t *testing.T) {
	srv, commands := newServerWithReader(t)
	cookie := loginAndGetCookie(t, srv)
	if got := asUser(srv, "POST", "/api/dispatch/pause", cookie).Code; got == http.StatusForbidden || got == http.StatusUnauthorized {
		t.Fatalf("POST as the full user: %d, want it let through", got)
	}
	if len(*commands) == 0 {
		t.Errorf("the full user's POST did not reach the command handler")
	}
	if got := postLogin(srv, "varde", "wrong").Code; got != http.StatusUnauthorized {
		t.Errorf("read-only login with a wrong password: %d, want 401", got)
	}
	// The read-only password is not a full user's, nor the other way round.
	if got := postLogin(srv, "alice", "readonly-pw").Code; got != http.StatusUnauthorized {
		t.Errorf("alice with varde's password: %d, want 401", got)
	}
}

func TestReadOnlyUser_CanLogOutButNotOpenPreviews(t *testing.T) {
	srv, _ := newServerWithReader(t)
	rec := postLogin(srv, "varde", "readonly-pw")
	cookie := extractSessionCookie(t, rec.Result(), "forge_session")
	if got := asUser(srv, "POST", "/logout", cookie).Code; got == http.StatusForbidden {
		t.Errorf("POST /logout as read-only: 403, want signing out allowed")
	}
	rec = postLogin(srv, "varde", "readonly-pw")
	cookie = extractSessionCookie(t, rec.Result(), "forge_session")
	req := httptest.NewRequest("POST", "/", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	if srv.authorizePreviewRequest(httptest.NewRecorder(), req, "x-1", "preview.example") {
		t.Errorf("a read-only session opened a preview")
	}
	full := loginAndGetCookie(t, srv)
	req = httptest.NewRequest("POST", "/", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: full})
	if !srv.authorizePreviewRequest(httptest.NewRecorder(), req, "x-1", "preview.example") {
		t.Errorf("a full user's session no longer opens a preview")
	}
}
