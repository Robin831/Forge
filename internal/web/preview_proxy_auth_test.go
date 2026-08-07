package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

const previewHostName = "forge-abc1." + proxyBase

// previewLabelName is the label previewHostName carries, and the one every
// token in this file is scoped to.
const previewLabelName = "forge-abc1"

// authProbe drives one request at the full router — logger, proxy middleware
// and routes — and returns the response. The paired resolveRecorder is what the
// caller inspects to prove a refusal never reached the preview registry.
func authProbe(t *testing.T, srv *Server, req *http.Request) *http.Response {
	t.Helper()
	w := httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	return w.Result()
}

// previewGET builds a request addressed to a preview host.
func previewGET(path string) *http.Request {
	return httptest.NewRequest("GET", "http://"+previewHostName+path, nil)
}

// navigate marks a request as a top-level browser navigation.
func navigate(r *http.Request) *http.Request {
	r.Header.Set("Accept", "text/html,application/xhtml+xml")
	return r
}

// newSessionCookie creates a live Hearth session and returns the cookie that
// carries it.
func newSessionCookie(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()
	token := "preview-auth-session-token"
	now := time.Now().UTC()
	if err := srv.db.CreateWebSession(state.WebSession{
		TokenHash: hashSessionToken(token),
		Username:  "alice",
		CreatedAt: now,
		ExpiresAt: now.Add(24 * time.Hour),
		LastSeen:  now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &http.Cookie{Name: srv.cfg.CookieName, Value: token}
}

// --- refusals ------------------------------------------------------------

// The default posture. A request with nothing to show is refused, and refused
// before the daemon is asked anything: an unauthenticated caller must not be
// able to map which previews are up, nor keep somebody else's idle clock alive.
func TestPreviewProxyAuth_NoCredentialsIsRefusedWithoutResolving(t *testing.T) {
	rec := resolveTo(t, echoUpstream(t).URL)
	srv := newGatedProxyServer(t, proxyBase, rec)

	resp := authProbe(t, srv, previewGET("/api/data"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unauthenticated preview request, got %d", resp.StatusCode)
	}
	if len(rec.recorded()) != 0 {
		t.Fatalf("a refused request must not resolve the preview, saw %v", rec.recorded())
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Hearth session") {
		t.Fatalf("401 body should say what is missing, got %q", body)
	}
}

// A browser navigation gets sent to the login on the apex of the proxy base —
// the host this same server answers the dashboard on — carrying where it was
// trying to go.
func TestPreviewProxyAuth_NavigationRedirectsToHearthLogin(t *testing.T) {
	rec := resolveTo(t, echoUpstream(t).URL)
	srv := newGatedProxyServer(t, proxyBase, rec)

	resp := authProbe(t, srv, navigate(previewGET("/deep/page?x=1")))
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 for an unauthenticated navigation, got %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location %q: %v", resp.Header.Get("Location"), err)
	}
	if loc.Host != proxyBase || loc.Path != "/login" {
		t.Fatalf("expected a redirect to the apex login, got %q", loc)
	}
	if got, want := loc.Query().Get("next"), "http://"+previewHostName+"/deep/page?x=1"; got != want {
		t.Fatalf("next = %q, want %q", got, want)
	}
	if len(rec.recorded()) != 0 {
		t.Fatalf("a redirected request must not resolve the preview, saw %v", rec.recorded())
	}
}

// --- the opt-out ---------------------------------------------------------

// preview_proxy_auth: none is the documented posture for a trusted network:
// the gate is gone entirely and a request with no credentials is served.
func TestPreviewProxyAuth_NoneBypassesTheGate(t *testing.T) {
	rec := resolveTo(t, echoUpstream(t).URL)
	srv := newGatedProxyServer(t, proxyBase, rec)
	srv.SetPreviewProxyAuth(func() string { return config.PreviewProxyAuthNone })

	resp := authProbe(t, srv, previewGET("/"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the request to be proxied, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	if len(rec.recorded()) != 1 {
		t.Fatalf("expected exactly one resolve, saw %v", rec.recorded())
	}
}

// An unknown value is not an opt-out. A typo in forge.yaml (or a hot-reload
// past validation) must leave the gate closed rather than open it.
func TestPreviewProxyAuth_UnknownModeStaysGated(t *testing.T) {
	rec := resolveTo(t, echoUpstream(t).URL)
	srv := newGatedProxyServer(t, proxyBase, rec)
	srv.SetPreviewProxyAuth(func() string { return "nOnE-ish" })

	resp := authProbe(t, srv, previewGET("/"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unrecognised preview_proxy_auth value, got %d", resp.StatusCode)
	}
}

// --- the session path ----------------------------------------------------

// The primary path: the operator's Hearth session cookie reached the preview
// host because sharedCookieDomain widened it, and that is all the proxy needs.
func TestPreviewProxyAuth_HearthSessionIsAccepted(t *testing.T) {
	rec := resolveTo(t, echoUpstream(t).URL)
	srv := newGatedProxyServer(t, proxyBase, rec)

	req := previewGET("/")
	req.AddCookie(newSessionCookie(t, srv))
	resp := authProbe(t, srv, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a valid Hearth session must be proxied, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
}

// The preview upstream is unreviewed branch code. Widening the session cookie
// to a shared parent domain is what makes the path above work, and stripping it
// on the way out is what keeps that from handing the session to the branch.
func TestPreviewProxyAuth_ForgeCookiesNeverReachTheUpstream(t *testing.T) {
	upstream := cookieEchoUpstream(t)
	rec := resolveTo(t, upstream.URL)
	srv := newGatedProxyServer(t, proxyBase, rec)

	req := previewGET("/")
	req.AddCookie(newSessionCookie(t, srv))
	req.AddCookie(&http.Cookie{Name: previewCookieName(previewLabelName), Value: "irrelevant"})
	req.AddCookie(&http.Cookie{Name: "app_theme", Value: "dark"})

	resp := authProbe(t, srv, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the request to be proxied, got %d", resp.StatusCode)
	}
	seen := decodeEcho(t, resp)["cookies"]
	if strings.Contains(seen, srv.cfg.CookieName) {
		t.Fatalf("the Hearth session cookie reached the preview: %q", seen)
	}
	if strings.Contains(seen, previewCookiePrefix) {
		t.Fatalf("a preview grant cookie reached the preview: %q", seen)
	}
	if !strings.Contains(seen, "app_theme=dark") {
		t.Fatalf("the preview's own cookies must pass through, got %q", seen)
	}
}

// --- the token exchange --------------------------------------------------

// The fallback path end to end: a link token is verified, swapped for a
// preview-scoped cookie and redirected off the URL, and the cookie carries the
// next request through.
func TestPreviewProxyAuth_TokenIsExchangedForACookie(t *testing.T) {
	rec := resolveTo(t, echoUpstream(t).URL)
	srv := newGatedProxyServer(t, proxyBase, rec)

	token := mintPreviewToken(srv.previewSecret, previewLabelName, time.Now().Add(previewTokenTTL))
	resp := authProbe(t, srv, previewGET("/page?keep=1&"+previewTokenParam+"="+url.QueryEscape(token)))
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected a 302 completing the exchange, got %d", resp.StatusCode)
	}
	if len(rec.recorded()) != 0 {
		t.Fatalf("the exchange itself must not resolve the preview, saw %v", rec.recorded())
	}

	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Host != previewHostName || loc.Path != "/page" {
		t.Fatalf("the redirect must stay on the requested preview URL, got %q", loc)
	}
	if loc.Query().Has(previewTokenParam) {
		t.Fatalf("the token must be gone from the redirect target, got %q", loc)
	}
	if loc.Query().Get("keep") != "1" {
		t.Fatalf("other query parameters must survive, got %q", loc)
	}

	granted := findCookie(resp.Cookies(), previewCookieName(previewLabelName))
	if granted == nil {
		t.Fatalf("no preview cookie was set, got %v", resp.Cookies())
	}
	if !granted.HttpOnly {
		t.Fatal("the preview grant cookie must be HttpOnly")
	}
	if granted.Domain != proxyBase {
		t.Fatalf("grant domain = %q, want %q so per-service hosts are covered", granted.Domain, proxyBase)
	}
	if granted.Value == token {
		t.Fatal("the exchange must mint a new token, not re-issue the link's")
	}

	// The follow-up request carries the cookie and is proxied.
	next := previewGET("/page?keep=1")
	next.AddCookie(&http.Cookie{Name: granted.Name, Value: granted.Value})
	resp2 := authProbe(t, srv, next)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("the granted cookie must authorise the next request, got %d", resp2.StatusCode)
	}
}

// A grant for one preview is not a grant for another, even though the cookie is
// scoped to the whole proxy base so per-service hostnames are covered.
func TestPreviewProxyAuth_GrantDoesNotCrossPreviews(t *testing.T) {
	rec := resolveTo(t, echoUpstream(t).URL)
	srv := newGatedProxyServer(t, proxyBase, rec)

	other := mintPreviewToken(srv.previewSecret, "forge-zzz9", time.Now().Add(previewCookieTTL))
	req := previewGET("/")
	req.AddCookie(&http.Cookie{Name: previewCookieName("forge-zzz9"), Value: other})
	// Also present it under this preview's cookie name, so the rejection is the
	// label check rather than the cookie lookup missing.
	req.AddCookie(&http.Cookie{Name: previewCookieName(previewLabelName), Value: other})

	resp := authProbe(t, srv, req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a grant for another preview must not authorise this one, got %d", resp.StatusCode)
	}
}

// Rejected tokens say which of the three things went wrong, because each has a
// different remedy.
func TestPreviewProxyAuth_RejectedTokens(t *testing.T) {
	cases := []struct {
		name  string
		token func(srv *Server) string
		want  string
	}{
		{
			name: "expired",
			token: func(srv *Server) string {
				return mintPreviewToken(srv.previewSecret, previewLabelName, time.Now().Add(-time.Minute))
			},
			want: "expired",
		},
		{
			name: "issued for another preview",
			token: func(srv *Server) string {
				return mintPreviewToken(srv.previewSecret, "forge-zzz9", time.Now().Add(previewTokenTTL))
			},
			want: "different preview",
		},
		{
			name: "not signed by this daemon",
			token: func(*Server) string {
				return mintPreviewToken([]byte("not-the-daemons-secret"), previewLabelName, time.Now().Add(previewTokenTTL))
			},
			want: "not valid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := resolveTo(t, echoUpstream(t).URL)
			srv := newGatedProxyServer(t, proxyBase, rec)

			req := previewGET("/?" + previewTokenParam + "=" + url.QueryEscape(tc.token(srv)))
			resp := authProbe(t, srv, req)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", resp.StatusCode)
			}
			if body := readBody(t, resp); !strings.Contains(body, tc.want) {
				t.Fatalf("401 body %q should mention %q", body, tc.want)
			}
			if len(rec.recorded()) != 0 {
				t.Fatalf("a rejected token must not resolve the preview, saw %v", rec.recorded())
			}
		})
	}
}

// --- entry links ---------------------------------------------------------

// With host-based routing on, the link the dashboard renders is the preview
// hostname — and it carries a token exactly when the session cookie cannot
// reach that hostname on its own.
func TestPreviewsList_EntryURLTokenFollowsTheCookieDomain(t *testing.T) {
	cases := []struct {
		name       string
		hearthHost string
		base       string
		authMode   string
		wantHost   string
		wantToken  bool
	}{
		{
			name:       "no shared parent: the link must carry a token",
			hearthHost: "hearth.example.com",
			base:       "preview.other.test",
			wantHost:   previewLabelName + ".preview.other.test",
			wantToken:  true,
		},
		{
			name:       "shared parent: the session cookie already reaches it",
			hearthHost: "hearth.example.com",
			base:       "preview.example.com",
			wantHost:   previewLabelName + ".preview.example.com",
			wantToken:  false,
		},
		{
			name:       "gate off: nothing to prove",
			hearthHost: "hearth.example.com",
			base:       "preview.other.test",
			authMode:   config.PreviewProxyAuthNone,
			wantHost:   previewLabelName + ".preview.other.test",
			wantToken:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServerWithDefaults(t, previewListHandler(previewLabelName))
			srv.SetPreviewProxyBase(func() string { return tc.base })
			if tc.authMode != "" {
				srv.SetPreviewProxyAuth(func() string { return tc.authMode })
			}

			entry := entryURLFor(t, srv, tc.hearthHost)
			u, err := url.Parse(entry)
			if err != nil {
				t.Fatalf("parse entry url %q: %v", entry, err)
			}
			if u.Host != tc.wantHost {
				t.Fatalf("entry host = %q, want %q", u.Host, tc.wantHost)
			}
			token := u.Query().Get(previewTokenParam)
			if tc.wantToken != (token != "") {
				t.Fatalf("token present = %v (%q), want %v", token != "", token, tc.wantToken)
			}
			if token != "" {
				if err := verifyPreviewToken(srv.previewSecret, token, previewLabelName, time.Now()); err != nil {
					t.Fatalf("the minted link token must verify: %v", err)
				}
			}
		})
	}
}

// Without a proxy base the link stays on host:port, unchanged and untokenised.
func TestPreviewsList_EntryURLStaysOnPortWithoutAProxyBase(t *testing.T) {
	srv := newServerWithDefaults(t, previewListHandler(previewLabelName))
	entry := entryURLFor(t, srv, "hearth.example.com")
	if !strings.HasPrefix(entry, "http://hearth.example.com:4310/") {
		t.Fatalf("entry url = %q, want the entry service's port", entry)
	}
	if strings.Contains(entry, previewTokenParam) {
		t.Fatalf("a port link needs no preview token, got %q", entry)
	}
}

// --- the session cookie Domain ------------------------------------------

// Logging in on a host that shares a parent with the proxy base widens the
// session cookie to that parent — the whole mechanism behind the primary auth
// path — and signing out clears it with the same Domain, or it would linger.
func TestLogin_SessionCookieDomainFollowsTheProxyBase(t *testing.T) {
	cases := []struct {
		name       string
		hearthHost string
		base       string
		authMode   string
		wantDomain string
	}{
		{
			name:       "shared parent widens the cookie",
			hearthHost: "hearth.example.com",
			base:       "preview.example.com",
			wantDomain: "example.com",
		},
		{
			name:       "unrelated base leaves it host-only",
			hearthHost: "hearth.example.com",
			base:       "preview.other.test",
			wantDomain: "",
		},
		{
			name:       "no proxy base leaves it host-only",
			hearthHost: "hearth.example.com",
			wantDomain: "",
		},
		{
			name:       "gate off: no reason to widen it",
			hearthHost: "hearth.example.com",
			base:       "preview.example.com",
			authMode:   config.PreviewProxyAuthNone,
			wantDomain: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServerWithDefaults(t, nil)
			if tc.base != "" {
				srv.SetPreviewProxyBase(func() string { return tc.base })
			}
			if tc.authMode != "" {
				srv.SetPreviewProxyAuth(func() string { return tc.authMode })
			}

			resp := loginOn(t, srv, tc.hearthHost)
			session := findCookie(resp.Cookies(), srv.cfg.CookieName)
			if session == nil {
				t.Fatalf("login set no session cookie: %v", resp.Cookies())
			}
			if session.Domain != tc.wantDomain {
				t.Fatalf("session cookie Domain = %q, want %q", session.Domain, tc.wantDomain)
			}

			// Signing out must delete the same cookie, Domain included.
			out := httptest.NewRequest("POST", "http://"+tc.hearthHost+"/logout", nil)
			out.Header.Set("X-Forge-Action", "1")
			out.AddCookie(&http.Cookie{Name: session.Name, Value: session.Value})
			w := httptest.NewRecorder()
			srv.routes().ServeHTTP(w, out)
			// requireAuth slides the cookie before the handler deletes it, so
			// the response carries two — the browser applies the last.
			cleared := lastCookie(w.Result().Cookies(), srv.cfg.CookieName)
			if cleared == nil {
				t.Fatalf("logout cleared no session cookie: %v", w.Result().Cookies())
			}
			if cleared.Domain != tc.wantDomain {
				t.Fatalf("cleared cookie Domain = %q, want %q — a mismatched delete leaves the cookie in place",
					cleared.Domain, tc.wantDomain)
			}
			if cleared.MaxAge >= 0 {
				t.Fatalf("cleared cookie MaxAge = %d, want a negative (delete) value", cleared.MaxAge)
			}
		})
	}
}

// --- helpers -------------------------------------------------------------

// cookieEchoUpstream reports the Cookie header exactly as it arrived.
func cookieEchoUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"cookies": r.Header.Get("Cookie")})
	}))
	t.Cleanup(ts.Close)
	return ts
}

// previewListHandler answers `previews` with a single running preview whose
// entry service binds port 4310.
func previewListHandler(beadID string) CommandHandler {
	return func(cmd ipc.Command) ipc.Response {
		if cmd.Type != "previews" && cmd.Type != "preview_list" {
			return ipc.Response{Type: "ok", Payload: []byte(`{}`)}
		}
		raw, _ := json.Marshal(ipc.PreviewsResponse{
			Enabled: true,
			Previews: []ipc.PreviewInfo{{
				BeadID: beadID,
				Anvil:  "forge",
				Status: "running",
				Services: []ipc.PreviewServiceInfo{
					{Name: "web", Port: 4310, Health: "healthy", Entry: true},
				},
				CreatedAt:    time.Now(),
				LastActiveAt: time.Now(),
			}},
		})
		return ipc.Response{Type: "ok", Payload: raw}
	}
}

// entryURLFor reads the single preview's entry_url out of GET /api/previews as
// seen from the given Hearth host.
func entryURLFor(t *testing.T, srv *Server, hearthHost string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "http://"+hearthHost+"/api/previews", nil)
	req.AddCookie(newSessionCookie(t, srv))
	w := httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/previews = %d: %s", w.Code, w.Body.String())
	}
	var out PreviewsListResponse
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode previews: %v", err)
	}
	if len(out.Previews) != 1 {
		t.Fatalf("expected one preview, got %d", len(out.Previews))
	}
	return out.Previews[0].EntryURL
}

// loginOn signs alice in against the given Hearth host.
func loginOn(t *testing.T, srv *Server, hearthHost string) *http.Response {
	t.Helper()
	form := url.Values{}
	form.Set("user", "alice")
	form.Set("password", "hunter2")
	req := httptest.NewRequest("POST", "http://"+hearthHost+"/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forge-Action", "1")
	w := httptest.NewRecorder()
	srv.routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login on %s = %d: %s", hearthHost, w.Code, w.Body.String())
	}
	return w.Result()
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func lastCookie(cookies []*http.Cookie, name string) *http.Cookie {
	var last *http.Cookie
	for _, c := range cookies {
		if c.Name == name {
			last = c
		}
	}
	return last
}
