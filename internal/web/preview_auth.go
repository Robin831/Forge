package web

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/config"
)

// Auth gating for host-based preview routing.
//
// A preview serves an unreviewed branch build of somebody's application. Behind
// a port on loopback that was tolerable; behind a wildcard DNS record it is
// not, so by default a request on a preview hostname has to prove it comes from
// an operator who is signed in to Hearth. There are two ways it can:
//
//   - The Hearth session cookie itself, when preview_proxy_base shares a
//     registrable suffix with the Hearth host and the cookie is therefore
//     scoped to cover both (see sharedCookieDomain, applied in session.go).
//     This is the primary path: nothing extra to mint, nothing extra to expire.
//   - A short-lived signed token in the preview link, exchanged on first
//     contact for a preview-scoped cookie (previewtoken.go). This covers the
//     deployment where widening the session cookie would mean granting it to
//     hosts nobody here controls.
//
// settings.preview_proxy_auth: none turns the gate off for a trusted network.
// There is no third state: a request that proves nothing is refused, never
// quietly served.

// previewCookiePrefix prefixes the per-preview cookie names. The label is part
// of the name so two previews open in one browser do not overwrite each other's
// grant, and so a name alone says which preview it is for.
const previewCookiePrefix = previewTokenCookieName + "_"

// previewAuthMode returns the effective settings.preview_proxy_auth. No
// callback installed (the default, and every test that does not opt in) means
// the gated mode: previews are not opened up by a missing wire-up.
func (s *Server) previewAuthMode() string {
	if s.previewProxyAuth == nil {
		return config.PreviewProxyAuthSession
	}
	mode, err := config.NormalizePreviewProxyAuth(s.previewProxyAuth())
	if err != nil {
		return config.PreviewProxyAuthSession
	}
	return mode
}

// previewAuthGated reports whether proxied preview requests must be
// authenticated.
func (s *Server) previewAuthGated() bool {
	return s.previewAuthMode() != config.PreviewProxyAuthNone
}

// authorizePreviewRequest decides whether a request addressed to preview
// `label` under `base` may be forwarded.
//
// It returns true only when the request is authorised and the caller should
// proxy it. On false it has already written the response — a redirect that
// completes the token exchange, a redirect to the Hearth login, or a 401 — so
// the caller must return without touching w.
func (s *Server) authorizePreviewRequest(w http.ResponseWriter, r *http.Request, label, base string) bool {
	// A grant from an earlier exchange on this preview. Checked first because
	// it is the one credential that costs nothing to verify — a preview serves
	// far more requests than the dashboard does, and every asset would
	// otherwise be a session lookup.
	if c, err := r.Cookie(previewCookieName(label)); err == nil && c.Value != "" {
		if verifyPreviewToken(s.previewSecret, c.Value, label, time.Now()) == nil {
			return true
		}
		// A stale or foreign cookie is not an error yet — the request may also
		// be carrying a fresh token below.
	}

	// A Hearth session, which is the only credential that exists at all in the
	// shared-cookie deployment. It is read without sliding the expiry:
	// browsing a preview is not using the dashboard, and the preview's own
	// idle clock is bumped by the resolve instead.
	if sess := s.loadSession(r); sess != nil {
		return true
	}

	// A link minted by the dashboard. Verify, exchange for a cookie, and
	// redirect to the same URL without the token so it leaves the address bar,
	// the browser history and any referrer the preview goes on to send.
	if token := r.URL.Query().Get(previewTokenParam); token != "" {
		if err := verifyPreviewToken(s.previewSecret, token, label, time.Now()); err != nil {
			s.logger.Info("preview token rejected", "label", label, "remote", s.clientIP(r), "error", err)
			writePreviewProxyError(w, http.StatusUnauthorized, previewTokenRejection(err))
			return false
		}
		s.grantPreviewCookie(w, label, base)
		http.Redirect(w, r, s.previewURLWithoutToken(r), http.StatusFound)
		return false
	}

	s.denyPreview(w, r, label, base)
	return false
}

// previewCookieName is the cookie a granted preview session is carried in.
func previewCookieName(label string) string {
	return previewCookiePrefix + label
}

// grantPreviewCookie completes the token exchange: a fresh, longer-lived token
// for the same label, stored in a cookie the browser will send on subsequent
// requests to this preview.
//
// Domain is set to the proxy base so the grant also covers the preview's
// per-service hostnames (`<label>--<service>.<base>`), which are separate hosts
// a host-only cookie would never reach. Widening it that far is safe because
// the token inside names its label and is verified against the host's on every
// request — another preview receiving the cookie learns nothing and is
// authorised by nothing. A single-label base carries no Domain at all: browsers
// reject a Domain attribute without an embedded dot, and a rejected cookie
// would mean an unexplained 401 on the next request.
func (s *Server) grantPreviewCookie(w http.ResponseWriter, label, base string) {
	expires := time.Now().Add(previewCookieTTL)
	cookie := &http.Cookie{
		Name:     previewCookieName(label),
		Value:    mintPreviewToken(s.previewSecret, label, expires),
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(previewCookieTTL / time.Second),
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
	if strings.Contains(base, ".") {
		cookie.Domain = base
	}
	http.SetCookie(w, cookie)
}

// previewURLWithoutToken renders the current request's URL with the token
// parameter dropped, keeping every other parameter and the path untouched.
func (s *Server) previewURLWithoutToken(r *http.Request) string {
	u := *r.URL
	q := u.Query()
	q.Del(previewTokenParam)
	u.RawQuery = q.Encode()
	// r.URL on a server request carries no scheme or host; supplying both
	// keeps the redirect absolute and on the preview hostname the client
	// already asked for.
	u.Scheme = s.requestScheme(r)
	u.Host = r.Host
	return u.String()
}

// denyPreview answers a request that presented no credential at all.
//
// A browser navigation is sent to the Hearth login with a next parameter, since
// a plain 401 in a fresh tab is a dead end and the operator almost certainly
// just has no session yet. Hearth's own host is taken to be the apex of the
// proxy base: that is the deployment host-based routing describes — a wildcard
// record beside the dashboard's own name, with apex traffic falling through
// this middleware to the router. Anything else (an XHR, a fetch, an asset) gets
// a 401, because redirecting a subresource to a login page produces a confusing
// parse error instead of an auth failure.
func (s *Server) denyPreview(w http.ResponseWriter, r *http.Request, label, base string) {
	s.logger.Info("preview request unauthenticated", "label", label, "remote", s.clientIP(r), "path", r.URL.Path)
	if wantsHTML(r) {
		http.Redirect(w, r, s.previewLoginURL(r, base), http.StatusFound)
		return
	}
	writePreviewProxyError(w, http.StatusUnauthorized,
		"preview "+label+" requires a Hearth session — sign in to Hearth and open the preview from the dashboard")
}

// previewTokenRejection turns a verification failure into a sentence for the
// 401 body. Each case has a different remedy, so each says which it is.
func previewTokenRejection(err error) string {
	switch {
	case errors.Is(err, errTokenExpired):
		return "preview link has expired — reopen the preview from the Hearth dashboard"
	case errors.Is(err, errTokenLabelMismatch):
		return "preview link was issued for a different preview"
	default:
		return "preview link is not valid"
	}
}

// previewLoginURL points at the Hearth login on the apex of the proxy base,
// carrying the originally requested preview URL as `next`. The port is carried
// over from the request: Hearth answers the preview host and its own host on
// the same listener.
//
// The login consumes `next` and bounces back here once a session exists — see
// loginnext.go, which re-validates the URL rather than trusting the round trip,
// and falls back to the dashboard in the deployment where the fresh session
// would not reach the preview host anyway.
func (s *Server) previewLoginURL(r *http.Request, base string) string {
	scheme := s.requestScheme(r)
	host := base
	if _, port, err := net.SplitHostPort(r.Host); err == nil && port != "" {
		host = net.JoinHostPort(base, port)
	}
	next := scheme + "://" + r.Host + r.URL.RequestURI()
	return scheme + "://" + host + "/login?next=" + url.QueryEscape(next)
}

// wantsHTML reports whether the request is a browser navigation rather than a
// subresource or API call.
func wantsHTML(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		// Modern browsers say so outright, which beats guessing from Accept.
		return mode == "navigate"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// requestScheme reports the scheme the client used, honouring
// X-Forwarded-Proto only from a configured trusted proxy — the same rule
// clientIP applies to X-Forwarded-For, and for the same reason: the header is
// otherwise attacker-controlled.
func (s *Server) requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if s.isTrustedProxy(remoteHost(r.RemoteAddr)) {
		if proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); proto != "" {
			if i := strings.Index(proto, ","); i >= 0 {
				proto = strings.TrimSpace(proto[:i])
			}
			if proto == "https" || proto == "http" {
				return proto
			}
		}
	}
	return "http"
}

// stripForgeAuthCookies removes Hearth's own cookies from a request about to be
// forwarded to a preview.
//
// A preview runs unreviewed code from a worker branch. With the session cookie
// widened to a shared parent domain (the primary auth path) the browser would
// otherwise hand that cookie straight to it, and one `console.log(req.headers)`
// in a branch would be a Hearth session in a preview's log. Nothing upstream
// has any use for these, so they stop here.
func (s *Server) stripForgeAuthCookies(r *http.Request) {
	if r.Header.Get("Cookie") == "" {
		return
	}
	var kept []*http.Cookie
	for _, c := range r.Cookies() {
		if c.Name == s.cfg.CookieName || strings.HasPrefix(c.Name, previewCookiePrefix) {
			continue
		}
		kept = append(kept, c)
	}
	r.Header.Del("Cookie")
	for _, c := range kept {
		r.AddCookie(c)
	}
}

// previewAccessToken mints a link token for label when one is needed — that is,
// when the gate is on and the Hearth session cookie is not scoped to reach
// preview hostnames from hearthHost. It returns "" when the link needs no
// token, which is both the opt-out case and the shared-cookie case.
func (s *Server) previewAccessToken(label, hearthHost, base string) string {
	if !s.previewAuthGated() {
		return ""
	}
	if _, ok := sharedCookieDomain(hearthHost, base); ok {
		return ""
	}
	return mintPreviewToken(s.previewSecret, label, time.Now().Add(previewTokenTTL))
}
