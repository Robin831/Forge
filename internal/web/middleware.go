package web

import (
	"net/http"
	"time"
)

// requireAuth is middleware that rejects requests without a valid session
// cookie with HTTP 401. When valid, it slides the session expiry and
// attaches the session to the request context.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := s.loadSession(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// Slide the expiry forward so active users keep their session.
		s.touchSession(sess)
		// Refresh the cookie so the browser sees the updated expiry.
		s.refreshSessionCookie(w, r, sess)
		next.ServeHTTP(w, r.WithContext(withSession(r.Context(), sess)))
	})
}

// optionalAuth attaches the session to the request context when one is
// present, but does not reject requests that lack one. Used for /login so
// the page can pre-fill the username when already signed in.
func (s *Server) optionalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sess := s.loadSession(r); sess != nil {
			next.ServeHTTP(w, r.WithContext(withSession(r.Context(), sess)))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestLogger logs each request once after the handler returns. Goes to
// the structured logger so it ends up in the daemon log alongside other
// daemon output.
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		s.logger.Info(
			"web request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start).Round(time.Millisecond).String(),
			"remote", clientIP(r),
		)
	})
}

// statusRecorder is a tiny ResponseWriter wrapper that captures the status
// code so the request logger can include it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush delegates to the underlying ResponseWriter when it implements
// http.Flusher so SSE handlers under the requestLogger middleware can still
// flush the response body. Without this delegation the type assertion in
// the SSE handlers fails and they return 500.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// csrfCheck rejects state-changing requests (POST/PUT/DELETE/PATCH) that lack
// the X-Forge-Action header. The SPA sets this header on every action fetch;
// cross-origin scripts cannot set custom headers, so this provides explicit
// CSRF defence for the new destructive endpoints introduced in Hearth 2.0.
func (s *Server) csrfCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// Safe methods — no CSRF risk.
		default:
			if r.Header.Get("X-Forge-Action") == "" {
				writeError(w, http.StatusForbidden, "CSRF check failed: X-Forge-Action header required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the request's client IP, preferring X-Forwarded-For when
// present (the daemon is expected to run behind a reverse proxy in
// Kubernetes).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	return r.RemoteAddr
}
