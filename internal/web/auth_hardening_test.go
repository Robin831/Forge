package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// postLoginNoCSRF submits a login without the X-Forge-Action header.
func postLoginNoCSRF(srv *Server, user, pass string) *httptest.ResponseRecorder {
	form := url.Values{}
	form.Set("user", user)
	form.Set("password", pass)
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func TestLogin_CSRFRejectedWithoutHeader(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	rec := postLoginNoCSRF(srv, "alice", "hunter2")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without X-Forge-Action, got %d body=%s", rec.Code, rec.Body.String())
	}
	// And no session cookie was issued.
	if c := extractSessionCookie(t, rec.Result(), "forge_session"); c != "" {
		t.Fatalf("expected no session cookie on CSRF rejection, got %q", c)
	}
}

func TestLogout_CSRFRejectedWithoutHeader(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("POST", "/logout", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 logout without header, got %d", rec.Code)
	}
	// The session must still be valid — the CSRF-blocked logout is a no-op.
	statusReq := httptest.NewRequest("GET", "/api/status", nil)
	statusReq.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	statusRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("session should survive a CSRF-blocked logout, got %d", statusRec.Code)
	}
}

func TestSession_AbsoluteExpiry(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	srv.cfg.SessionAbsoluteTTL = 7 * 24 * time.Hour

	now := time.Now().UTC()
	// A session created 8 days ago but "touched" recently (sliding expiry far
	// in the future) must still be rejected by the absolute cap.
	token := "abs-expiry-token"
	if err := srv.db.CreateWebSession(state.WebSession{
		TokenHash: hashSessionToken(token),
		Username:  "alice",
		CreatedAt: now.Add(-8 * 24 * time.Hour),
		ExpiresAt: now.Add(24 * time.Hour),
		LastSeen:  now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: token})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for absolutely-expired session, got %d", rec.Code)
	}

	// A fresh session (created just now) is accepted.
	fresh := "fresh-token"
	if err := srv.db.CreateWebSession(state.WebSession{
		TokenHash: hashSessionToken(fresh),
		Username:  "alice",
		CreatedAt: now,
		ExpiresAt: now.Add(24 * time.Hour),
		LastSeen:  now,
	}); err != nil {
		t.Fatalf("create fresh session: %v", err)
	}
	req2 := httptest.NewRequest("GET", "/api/status", nil)
	req2.AddCookie(&http.Cookie{Name: "forge_session", Value: fresh})
	rec2 := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 for fresh session, got %d", rec2.Code)
	}
}

func TestLogin_RotatesPriorToken(t *testing.T) {
	srv := newServerWithDefaults(t, nil)

	// First login → token A.
	tokenA := loginAndGetCookie(t, srv)
	if tokenA == "" {
		t.Fatal("no token from first login")
	}

	// Second login while presenting token A should rotate: token A is
	// invalidated and a new token B is issued.
	form := url.Values{}
	form.Set("user", "alice")
	form.Set("password", "hunter2")
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forge-Action", "1")
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: tokenA})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second login failed: %d", rec.Code)
	}
	tokenB := extractSessionCookie(t, rec.Result(), "forge_session")
	if tokenB == "" || tokenB == tokenA {
		t.Fatalf("expected a fresh rotated token, got %q (A=%q)", tokenB, tokenA)
	}

	// Token A must now be rejected.
	reqA := httptest.NewRequest("GET", "/api/status", nil)
	reqA.AddCookie(&http.Cookie{Name: "forge_session", Value: tokenA})
	recA := httptest.NewRecorder()
	srv.routes().ServeHTTP(recA, reqA)
	if recA.Code != http.StatusUnauthorized {
		t.Fatalf("prior token should be invalid after rotation, got %d", recA.Code)
	}

	// Token B works.
	reqB := httptest.NewRequest("GET", "/api/status", nil)
	reqB.AddCookie(&http.Cookie{Name: "forge_session", Value: tokenB})
	recB := httptest.NewRecorder()
	srv.routes().ServeHTTP(recB, reqB)
	if recB.Code != http.StatusOK {
		t.Fatalf("rotated token should be valid, got %d", recB.Code)
	}
}

func TestLogin_ThrottleDelaysAfterFailures(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	var (
		mu    sync.Mutex
		slept []time.Duration
	)
	srv.throttleSleep = func(d time.Duration) {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
	}

	// Exhaust the free attempts with wrong passwords.
	for i := 0; i < srv.throttle.threshold; i++ {
		rec := postLogin(srv, "alice", "wrong")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i, rec.Code)
		}
	}
	mu.Lock()
	if len(slept) != 0 {
		mu.Unlock()
		t.Fatalf("expected no throttle sleep within free attempts, got %v", slept)
	}
	mu.Unlock()

	// The next attempt must be throttled.
	rec := postLogin(srv, "alice", "wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("throttled attempt: expected 401, got %d", rec.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 1 || slept[0] != srv.throttle.base {
		t.Fatalf("expected one sleep of %v, got %v", srv.throttle.base, slept)
	}
}
