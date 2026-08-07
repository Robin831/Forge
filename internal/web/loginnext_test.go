package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
)

// The Hearth host in these tests is the apex of the proxy base, which is the
// deployment host-based preview routing describes and the one denyPreview
// assumes when it builds the login URL.
const hearthApex = proxyBase

// previewNextURL is a preview address under proxyBase, i.e. the kind of URL the
// preview proxy puts in `next`.
const previewNextURL = "http://" + previewHostName + "/orders/42?tab=items"

// loginServer is a server with host-based preview routing on and the auth gate
// at its default (session-required) posture.
func loginServer(t *testing.T) *Server {
	t.Helper()
	return newGatedProxyServer(t, proxyBase, resolveMiss(ipc.PreviewResolveNoPreview))
}

// postLoginNext signs in on the given Hearth host with a `next` parameter and
// returns the decoded JSON body.
func postLoginNext(t *testing.T, srv *Server, hearthHost, next string) map[string]any {
	t.Helper()
	form := url.Values{}
	form.Set("user", "alice")
	form.Set("password", "hunter2")
	if next != "" {
		form.Set("next", next)
	}
	req := httptest.NewRequest("POST", "http://"+hearthHost+"/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forge-Action", "1")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	return body
}

// loginRedirect is postLoginNext reduced to the field under test: the URL the
// server told the browser to go to, or "" when it named none.
func loginRedirect(t *testing.T, srv *Server, hearthHost, next string) string {
	t.Helper()
	body := postLoginNext(t, srv, hearthHost, next)
	if body["redirect"] == nil {
		return ""
	}
	target, ok := body["redirect"].(string)
	if !ok {
		t.Fatalf("redirect field is %T, want string", body["redirect"])
	}
	return target
}

// getLoginPage issues the browser navigation to /login, optionally carrying a
// session, and returns the recorder so the caller can read the redirect.
func getLoginPage(t *testing.T, srv *Server, hearthHost, next string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	target := "http://" + hearthHost + "/login"
	if next != "" {
		target += "?next=" + url.QueryEscape(next)
	}
	req := httptest.NewRequest("GET", target, nil)
	req.Header.Set("Accept", "text/html")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

// --- the flow the parameter exists for -----------------------------------

// The point of the whole mechanism: signing in after being bounced off a
// preview lands on that preview, not on the dashboard.
func TestLoginNext_PreviewURLIsHonoured(t *testing.T) {
	srv := loginServer(t)

	if got := loginRedirect(t, srv, hearthApex, previewNextURL); got != previewNextURL {
		t.Fatalf("login redirect = %q, want %q", got, previewNextURL)
	}
}

// The redirect the proxy emits and the parameter the login consumes are the
// same string — the round trip is what makes the two halves one feature.
func TestLoginNext_ProxyRedirectRoundTrips(t *testing.T) {
	srv := loginServer(t)

	resp := authProbe(t, srv, navigate(previewGET("/orders/42?tab=items")))
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("unauthenticated navigation: got %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	next := loc.Query().Get("next")
	if next == "" {
		t.Fatalf("proxy redirect carried no next: %s", loc)
	}

	if got := loginRedirect(t, srv, loc.Host, next); got != next {
		t.Fatalf("login redirect = %q, want the next the proxy emitted %q", got, next)
	}
}

// An operator who already has a session never sees the form; the GET has to
// honour `next` too, or the redirect chain ends on the dashboard.
func TestLoginNext_AuthenticatedGetRedirectsToPreview(t *testing.T) {
	srv := loginServer(t)
	cookie := newSessionCookie(t, srv)

	rec := getLoginPage(t, srv, hearthApex, previewNextURL, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("got %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != previewNextURL {
		t.Fatalf("Location = %q, want %q", loc, previewNextURL)
	}
}

// Without a session the page still renders: `next` must not turn the login
// form into a redirect for someone who has nothing to redirect with.
func TestLoginNext_UnauthenticatedGetStillServesTheForm(t *testing.T) {
	srv := loginServer(t)

	rec := getLoginPage(t, srv, hearthApex, previewNextURL, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (the SPA), Location %q", rec.Code, rec.Header().Get("Location"))
	}
}

// --- what must never be followed -----------------------------------------

// The open-redirect case. `next` reaches the server from a link anybody can
// send, so a host that is not a preview under the configured base is dropped
// and the sign-in still succeeds.
func TestLoginNext_ForeignHostIsRefused(t *testing.T) {
	srv := loginServer(t)

	for _, next := range []string{
		"https://evil.example.com/",
		"http://evil.example.com/?x=" + proxyBase,
		// A name that merely contains the base rather than sitting under it.
		"http://forge-abc1." + proxyBase + ".evil.example.com/",
		// Credentials in the target are a phishing shape, never something
		// Hearth mints.
		"http://user:pass@" + previewHostName + "/",
		// Non-HTTP schemes, including the ones that execute.
		"javascript:alert(1)",
		"//evil.example.com/",
		"/dashboard",
		"::not a url",
	} {
		if got := loginRedirect(t, srv, hearthApex, next); got != "" {
			t.Errorf("next %q produced redirect %q, want none", next, got)
		}
	}
}

// The apex is the dashboard's own host, not a preview: ParsePreviewHost rejects
// it and so does this, since "/" already covers it.
func TestLoginNext_ApexIsNotAPreview(t *testing.T) {
	srv := loginServer(t)

	if got := loginRedirect(t, srv, hearthApex, "http://"+proxyBase+"/prs"); got != "" {
		t.Fatalf("apex next produced redirect %q, want none", got)
	}
}

// An authenticated GET with a hostile `next` lands on the dashboard rather than
// erroring — a bad parameter is not a reason to refuse a valid session.
func TestLoginNext_AuthenticatedGetFallsBackToDashboard(t *testing.T) {
	srv := loginServer(t)
	cookie := newSessionCookie(t, srv)

	rec := getLoginPage(t, srv, hearthApex, "https://evil.example.com/", cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("got %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("Location = %q, want /", loc)
	}
}

// With host-based routing switched off no preview has a hostname at all, so
// nothing can be a legitimate target.
func TestLoginNext_NoProxyBaseHonoursNothing(t *testing.T) {
	srv := newServerWithDefaults(t, nil)

	if got := loginRedirect(t, srv, hearthApex, previewNextURL); got != "" {
		t.Fatalf("redirect %q with no preview_proxy_base, want none", got)
	}
}

// The deployment where the session cookie cannot reach preview hosts: following
// `next` would hit the gate again and bounce straight back to the login, so the
// round trip is refused and the operator lands on the dashboard, where the
// preview link carries a token that does work.
func TestLoginNext_RefusedWhenTheSessionCannotReachThePreview(t *testing.T) {
	srv := loginServer(t)

	// hearth.test and preview.test share only the public suffix ".test", so
	// sharedCookieDomain declines to widen the cookie.
	if got := loginRedirect(t, srv, "hearth.test", previewNextURL); got != "" {
		t.Fatalf("redirect %q from a host with no shared cookie domain, want none", got)
	}
}

// With the gate off there is nothing to prove at the preview host, so the
// shared-cookie condition does not apply and `next` is followed regardless.
func TestLoginNext_UngatedPreviewsFollowNextAnyway(t *testing.T) {
	srv := loginServer(t)
	srv.SetPreviewProxyAuth(func() string { return config.PreviewProxyAuthNone })

	if got := loginRedirect(t, srv, "hearth.test", previewNextURL); got != previewNextURL {
		t.Fatalf("login redirect = %q, want %q", got, previewNextURL)
	}
}

// A plain sign-in is unchanged: no `next`, no redirect field, and the SPA keeps
// routing to the dashboard itself.
func TestLoginNext_AbsentParameterLeavesTheResponseAlone(t *testing.T) {
	srv := loginServer(t)

	body := postLoginNext(t, srv, hearthApex, "")
	if _, ok := body["redirect"]; ok {
		t.Fatalf("login without next carried a redirect: %v", body["redirect"])
	}
	if body["authenticated"] != true {
		t.Fatalf("authenticated = %v, want true", body["authenticated"])
	}
}
