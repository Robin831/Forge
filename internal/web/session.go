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
func (s *Server) createSession(w http.ResponseWriter, username string) (string, error) {
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
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	return token, nil
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
	return sess
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

// clearSessionCookie deletes the session cookie on the client.
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.CookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
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
