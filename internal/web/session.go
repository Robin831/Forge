package web

import (
	"context"
	"net/http"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// createSession issues a new web session for the given user, persists it,
// and writes the session cookie to w. Returns the raw token (which is what
// the client stores in the cookie).
func (s *Server) createSession(w http.ResponseWriter, r *http.Request, username string) (string, error) {
	token, err := generateSessionToken()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	expires := now.Add(s.cfg.SessionTTL)
	if err := s.db.CreateWebSession(state.WebSession{
		TokenHash: hashSessionToken(token),
		Username:  username,
		CreatedAt: now,
		ExpiresAt: expires,
		LastSeen:  now,
	}); err != nil {
		return "", err
	}
	http.SetCookie(w, s.sessionCookie(r, token, expires))
	return token, nil
}

// sessionCookie builds the session cookie. Everything about it is fixed except
// Domain, which is widened to the suffix the Hearth host and
// settings.preview_proxy_base share when they share one — that is what lets a
// proxied preview on a sibling subdomain see the session the operator already
// has, instead of demanding a second credential. sharedCookieDomain refuses
// every case where the widening would be a grant to hosts outside this
// deployment; those fall back to the preview token exchange.
func (s *Server) sessionCookie(r *http.Request, value string, expires time.Time) *http.Cookie {
	c := &http.Cookie{
		Name:     s.cfg.CookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
	if domain, ok := s.sessionCookieDomain(r); ok {
		c.Domain = domain
	}
	return c
}

// sessionCookieDomain resolves the Domain attribute the session cookie should
// carry for this request, or ok=false for a host-only cookie (the default and
// the tighter posture).
func (s *Server) sessionCookieDomain(r *http.Request) (string, bool) {
	if r == nil || !s.previewAuthGated() {
		// With the gate off nothing needs the session on a preview host, so
		// there is no reason to hand it to one.
		return "", false
	}
	base := s.proxyBase()
	if base == "" {
		return "", false
	}
	return sharedCookieDomain(r.Host, base)
}

// loadSession looks up the session token from the request's session cookie
// and returns the associated session row. Returns nil when the cookie is
// missing, the session is unknown, or the session has expired.
func (s *Server) loadSession(r *http.Request) *state.WebSession {
	cookie, err := r.Cookie(s.cfg.CookieName)
	if err != nil {
		return nil
	}
	if cookie.Value == "" {
		return nil
	}
	sess, err := s.db.GetWebSession(hashSessionToken(cookie.Value))
	if err != nil || sess == nil {
		return nil
	}
	// Absolute lifetime cap: a session cannot outlive created_at +
	// SessionAbsoluteTTL no matter how recently it was used, so a stolen
	// cookie kept warm by sliding renewals still dies on schedule. A
	// non-positive cap disables this check (sliding expiry only).
	if s.cfg.SessionAbsoluteTTL > 0 &&
		time.Now().After(sess.CreatedAt.Add(s.cfg.SessionAbsoluteTTL)) {
		if err := s.db.DeleteWebSession(sess.TokenHash); err != nil {
			s.logger.Warn("delete absolutely-expired web session failed", "error", err)
		}
		return nil
	}
	return sess
}

// deleteSessionFromRequest removes whatever session the request's cookie
// points at, if any. Used to rotate the token on login: the pre-login token
// is invalidated before a fresh one is issued. Best-effort — a missing cookie
// or delete error is not fatal to the login.
func (s *Server) deleteSessionFromRequest(r *http.Request) {
	cookie, err := r.Cookie(s.cfg.CookieName)
	if err != nil || cookie.Value == "" {
		return
	}
	if err := s.db.DeleteWebSession(hashSessionToken(cookie.Value)); err != nil {
		s.logger.Warn("rotate: delete prior web session failed", "error", err)
	}
}

// touchSession slides the session expiry forward. Best-effort: errors are
// logged and swallowed so a touch failure does not break the request.
func (s *Server) touchSession(sess *state.WebSession) {
	expires, err := s.db.TouchWebSession(sess.TokenHash, s.cfg.SessionTTL)
	if err != nil {
		s.logger.Warn("touch web session failed", "error", err, "user", sess.Username)
		return
	}
	sess.ExpiresAt = expires
}

// refreshSessionCookie re-issues the session cookie with the updated ExpiresAt
// from a just-touched session, so the browser tracks the slid expiry.
func (s *Server) refreshSessionCookie(w http.ResponseWriter, r *http.Request, sess *state.WebSession) {
	cookie, err := r.Cookie(s.cfg.CookieName)
	if err != nil || cookie.Value == "" {
		return
	}
	http.SetCookie(w, s.sessionCookie(r, cookie.Value, sess.ExpiresAt))
}

// clearSessionCookie deletes the session cookie on the client. The deletion
// must repeat whatever Domain the cookie was issued with — a host-only delete
// leaves a domain-scoped cookie sitting in the browser, and the operator stays
// "logged in" on every preview host after signing out.
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	cookie := s.sessionCookie(r, "", time.Unix(0, 0))
	cookie.MaxAge = -1
	http.SetCookie(w, cookie)
}

type sessionContextKey struct{}

// withSession returns a copy of ctx that carries the given session.
func withSession(ctx context.Context, sess *state.WebSession) context.Context {
	return context.WithValue(ctx, sessionContextKey{}, sess)
}

// SessionFromContext returns the authenticated session attached to ctx by
// the auth middleware, or nil when no session is present.
func SessionFromContext(ctx context.Context) *state.WebSession {
	sess, _ := ctx.Value(sessionContextKey{}).(*state.WebSession)
	return sess
}
