package web

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Robin831/Forge/internal/state"
)

// newIPServer builds a bare Server with the given trusted-proxy spec parsed
// into TrustedProxies. It does not open a DB — clientIP needs none.
func newIPServer(t *testing.T, spec string) *Server {
	t.Helper()
	nets, err := ParseTrustedProxies(spec)
	if err != nil {
		t.Fatalf("ParseTrustedProxies(%q): %v", spec, err)
	}
	return &Server{trustedProxies: nets, logger: slog.Default()}
}

func reqWith(remoteAddr, xff string) *http.Request {
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.RemoteAddr = remoteAddr
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestClientIP_UntrustedPeerIgnoresXFF(t *testing.T) {
	s := newIPServer(t, "10.0.0.1")
	// Peer 203.0.113.9 is NOT a trusted proxy, so the spoofed XFF is ignored.
	got := s.clientIP(reqWith("203.0.113.9:5555", "1.2.3.4"))
	if got != "203.0.113.9" {
		t.Errorf("clientIP = %q, want the direct peer 203.0.113.9", got)
	}
}

func TestClientIP_NoTrustedProxiesAlwaysPeer(t *testing.T) {
	s := newIPServer(t, "")
	got := s.clientIP(reqWith("198.51.100.7:443", "1.2.3.4, 5.6.7.8"))
	if got != "198.51.100.7" {
		t.Errorf("clientIP = %q, want 198.51.100.7 (no trusted proxies configured)", got)
	}
}

func TestClientIP_TrustedPeerHonorsXFF(t *testing.T) {
	s := newIPServer(t, "10.0.0.0/8")
	// Direct peer is the trusted proxy; the single XFF entry is the client.
	got := s.clientIP(reqWith("10.0.0.5:12345", "203.0.113.42"))
	if got != "203.0.113.42" {
		t.Errorf("clientIP = %q, want the forwarded client 203.0.113.42", got)
	}
}

func TestClientIP_TrustedPeerSkipsTrustedHops(t *testing.T) {
	// Both the direct peer and an inner hop are trusted; the rightmost
	// UNtrusted address is the real client.
	s := newIPServer(t, "10.0.0.0/8, 192.168.0.0/16")
	got := s.clientIP(reqWith("10.0.0.5:1", "203.0.113.42, 192.168.1.1"))
	if got != "203.0.113.42" {
		t.Errorf("clientIP = %q, want 203.0.113.42 (rightmost untrusted hop)", got)
	}
}

func TestClientIP_TrustedPeerAllTrustedFallsBackToPeer(t *testing.T) {
	s := newIPServer(t, "10.0.0.0/8")
	// Every XFF hop is a trusted proxy, so fall back to the peer.
	got := s.clientIP(reqWith("10.0.0.5:1", "10.1.2.3, 10.4.5.6"))
	if got != "10.0.0.5" {
		t.Errorf("clientIP = %q, want the peer 10.0.0.5 when XFF is all trusted", got)
	}
}

func TestClientIP_PeerWithoutPort(t *testing.T) {
	s := newIPServer(t, "10.0.0.1")
	got := s.clientIP(reqWith("203.0.113.9", ""))
	if got != "203.0.113.9" {
		t.Errorf("clientIP = %q, want 203.0.113.9 for a bare RemoteAddr", got)
	}
}

func TestParseTrustedProxies_InvalidEntry(t *testing.T) {
	if _, err := ParseTrustedProxies("10.0.0.1, not-an-ip"); err == nil {
		t.Fatal("expected an error for a malformed trusted-proxy entry")
	}
	if nets, err := ParseTrustedProxies("  , ,"); err != nil || nets != nil {
		t.Fatalf("blank input: got (%v, %v), want (nil, nil)", nets, err)
	}
}

// sessionReq returns a request carrying a session with the given token hash so
// acquireSSESlot keys the cap on it.
func sessionReq(tokenHash string) *http.Request {
	r := httptest.NewRequest("GET", "/api/activity/stream", nil)
	sess := &state.WebSession{TokenHash: tokenHash, Username: "alice"}
	return r.WithContext(withSession(r.Context(), sess))
}

func newCapServer() *Server {
	return &Server{logger: slog.Default(), sseConns: make(map[string]int)}
}

func TestAcquireSSESlot_CapEnforcedAndReleased(t *testing.T) {
	s := newCapServer()
	var releases []func()
	for i := 0; i < maxSSEPerSession; i++ {
		rel, ok := s.acquireSSESlot(sessionReq("tok-a"))
		if !ok {
			t.Fatalf("slot %d unexpectedly denied", i)
		}
		releases = append(releases, rel)
	}

	// One past the cap must be denied.
	if _, ok := s.acquireSSESlot(sessionReq("tok-a")); ok {
		t.Fatal("acquire past the cap should be denied")
	}

	// Releasing one slot frees room for exactly one more.
	releases[0]()
	rel, ok := s.acquireSSESlot(sessionReq("tok-a"))
	if !ok {
		t.Fatal("acquire after release should succeed")
	}
	rel()
	if _, ok := s.acquireSSESlot(sessionReq("tok-a")); !ok {
		// still at cap-1 after the extra release above frees another slot
		t.Fatal("expected room after releasing the extra slot")
	}
}

func TestAcquireSSESlot_ReleaseIsIdempotent(t *testing.T) {
	s := newCapServer()
	rel, ok := s.acquireSSESlot(sessionReq("tok-a"))
	if !ok {
		t.Fatal("first acquire denied")
	}
	rel()
	rel() // double release must not underflow the counter or the map entry
	if n := s.sseConns["tok-a"]; n != 0 {
		t.Fatalf("count after double release = %d, want 0", n)
	}
	// A double release must not have created capacity beyond the cap.
	var rels []func()
	for i := 0; i < maxSSEPerSession; i++ {
		r, ok := s.acquireSSESlot(sessionReq("tok-a"))
		if !ok {
			t.Fatalf("slot %d denied after idempotent release", i)
		}
		rels = append(rels, r)
	}
	if _, ok := s.acquireSSESlot(sessionReq("tok-a")); ok {
		t.Fatal("cap breached after a double release")
	}
	for _, r := range rels {
		r()
	}
}

func TestAcquireSSESlot_SessionsIndependent(t *testing.T) {
	s := newCapServer()
	// Fill session A to the cap.
	for i := 0; i < maxSSEPerSession; i++ {
		if _, ok := s.acquireSSESlot(sessionReq("tok-a")); !ok {
			t.Fatalf("A slot %d denied", i)
		}
	}
	if _, ok := s.acquireSSESlot(sessionReq("tok-a")); ok {
		t.Fatal("A should be at cap")
	}
	// Session B is unaffected.
	if _, ok := s.acquireSSESlot(sessionReq("tok-b")); !ok {
		t.Fatal("B should have its own budget")
	}
}

func TestAcquireSSESlot_NoSessionNotCapped(t *testing.T) {
	s := newCapServer()
	r := httptest.NewRequest("GET", "/api/activity/stream", nil) // no session in ctx
	for i := 0; i < maxSSEPerSession+5; i++ {
		if _, ok := s.acquireSSESlot(r); !ok {
			t.Fatalf("sessionless request %d should not be capped", i)
		}
	}
}

func TestSSECapped_Returns429AtCap(t *testing.T) {
	s := newCapServer()
	// Occupy every slot for tok-a.
	for i := 0; i < maxSSEPerSession; i++ {
		if _, ok := s.acquireSSESlot(sessionReq("tok-a")); !ok {
			t.Fatalf("prefill slot %d denied", i)
		}
	}

	called := false
	h := s.sseCapped(func(w http.ResponseWriter, r *http.Request) { called = true })

	rec := httptest.NewRecorder()
	h(rec, sessionReq("tok-a"))

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if called {
		t.Error("wrapped handler must not run when the cap is hit")
	}
}

func TestSSECapped_ReleasesOnHandlerReturn(t *testing.T) {
	s := newCapServer()
	h := s.sseCapped(func(w http.ResponseWriter, r *http.Request) {})
	// Run the capped handler more times than the cap; because each returns
	// immediately the slot is released every time, so none should 429.
	for i := 0; i < maxSSEPerSession+3; i++ {
		rec := httptest.NewRecorder()
		h(rec, sessionReq("tok-a"))
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("iteration %d 429'd despite prompt release", i)
		}
	}
	if n := s.sseConns["tok-a"]; n != 0 {
		t.Fatalf("leaked %d slots after all handlers returned", n)
	}
}
