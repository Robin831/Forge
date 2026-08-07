package web

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/Robin831/Forge/internal/kiln"
)

// Returning to a preview after signing in.
//
// An unauthenticated browser navigation to a proxied preview host is bounced to
// the Hearth login on the apex of settings.preview_proxy_base, carrying the URL
// it was trying to reach as `next` (see denyPreview in preview_auth.go). This is
// where that parameter is consumed, on both halves of the login: the GET that
// renders the page for someone who turns out to already have a session, and the
// POST that creates one.
//
// `next` arrives from a redirect the browser followed, so it is
// attacker-supplyable — anyone can hand out a `/login?next=<anywhere>` link and
// a login page that obeys it is an open redirect. It is therefore never used as
// given: a value is honoured only when it names a preview hostname under the
// *current* preview_proxy_base, which is the one set of destinations this
// parameter exists to reach. Everything else falls back to the dashboard rather
// than being reported as an error — a stale or malformed `next` is not a reason
// to refuse a valid sign-in.
//
// The check lives here rather than in the SPA for two reasons: the SPA does not
// know the proxy base, and a redirect target validated on the client is
// validated by whoever controls the client. The frontend only ever follows the
// URL this file hands back.

// loginNextParam is the query (GET /login) and form (POST /login) parameter
// naming where a successful login should land.
const loginNextParam = "next"

// loginRedirectTarget resolves where a login on this request should send the
// browser: the validated `next` URL, or "" for the dashboard default.
//
// r supplies the Hearth host the sign-in is happening on, which is what decides
// whether the session about to be issued can authorise the destination at all.
func (s *Server) loginRedirectTarget(r *http.Request, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	base := s.proxyBase()
	if base == "" {
		// Host-based routing is off: no preview has a hostname, so no `next`
		// can name one.
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	if u.User != nil {
		// Credentials embedded in a redirect target are a phishing shape, and
		// nothing Hearth mints ever carries them.
		return ""
	}
	if _, _, ok := kiln.ParsePreviewHost(u.Host, base); !ok {
		// Not a preview hostname — including the apex itself, which
		// ParsePreviewHost rejects and which "/" already covers.
		return ""
	}
	if !s.loginReachesPreviewHosts(r.Host, base) {
		// The session this login issues would not be sent to the preview host,
		// so following `next` would land on the gate again and bounce straight
		// back here. Send the operator to the dashboard instead, where the
		// preview link carries a token that does work.
		return ""
	}
	return u.String()
}

// loginRedirectTargetOrRoot is loginRedirectTarget with the dashboard as the
// fallback, for the callers that need a URL rather than a decision.
func (s *Server) loginRedirectTargetOrRoot(r *http.Request, raw string) string {
	if target := s.loginRedirectTarget(r, raw); target != "" {
		return target
	}
	return "/"
}

// loginReachesPreviewHosts reports whether a session issued on hearthHost is
// one that preview hostnames under base will actually receive.
//
// That is exactly the shared-cookie condition sessionCookieDomain applies when
// it widens the session cookie (session.go): where the two hosts share no
// registrable parent the cookie stays host-only and previews are reached with a
// link token instead, which a login cannot mint on the operator's behalf. With
// the gate off there is nothing to prove at the preview host in the first place.
func (s *Server) loginReachesPreviewHosts(hearthHost, base string) bool {
	if !s.previewAuthGated() {
		return true
	}
	_, ok := sharedCookieDomain(hearthHost, base)
	return ok
}
