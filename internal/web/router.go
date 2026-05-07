package web

import (
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// routes builds the chi router with all middleware and handlers wired up.
func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(s.requestLogger)

	// Public endpoints — no authentication.
	r.Get("/healthz", s.handleHealthz)

	// Login: optional auth so the page can detect an existing session.
	r.Group(func(r chi.Router) {
		r.Use(s.optionalAuth)
		r.Post("/login", s.handleLogin)
		r.Get("/login", s.handleLoginStatus)
	})

	// Logout requires a valid session — otherwise it is a no-op.
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Post("/logout", s.handleLogout)
	})

	// All /api/* endpoints require auth.
	r.Route("/api", func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/me", s.handleMe)
		r.Get("/status", s.handleStatus)
		r.Get("/queue", s.handleQueue)
		r.Get("/workers", s.handleWorkers)
	})

	// Static UI fallback. The next bead replaces this with the embedded
	// React build; for now we serve the placeholder index.html out of the
	// embedded dist directory so the deployment works end-to-end.
	r.Handle("/*", s.staticHandler())
	return r
}

// staticHandler serves the embedded UI bundle with SPA fallback to
// index.html. When the dist directory is empty (typical until the frontend
// bead lands), it serves a minimal placeholder.
func (s *Server) staticHandler() http.HandlerFunc {
	dist, err := distFS()
	if err != nil {
		// No embedded UI — return the placeholder for any path.
		return s.placeholderHandler
	}
	fileServer := http.FileServer(http.FS(dist))
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" || path == "" {
			path = "/index.html"
		}
		// SPA fallback: when the requested file does not exist, serve
		// index.html so client-side routing can take over.
		if _, err := fs.Stat(dist, trimLeadingSlash(path)); err != nil {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/index.html"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	}
}

// placeholderHandler is used when no embedded UI is present. It returns a
// minimal HTML page acknowledging the daemon is up so operators have
// something to point a browser at while the frontend bead is in flight.
func (s *Server) placeholderHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(placeholderHTML))
}

const placeholderHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Hearth 2.0</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 2rem auto; max-width: 40rem; padding: 0 1rem; color: #222; }
h1 { font-size: 1.4rem; }
code { background: #f4f4f4; padding: 0 0.25rem; border-radius: 3px; }
</style>
</head>
<body>
<h1>Hearth 2.0 backend is running</h1>
<p>The forge daemon is serving the Hearth web API. The frontend bundle has not been embedded yet.</p>
<p>Try <code>POST /login</code> with a form-encoded <code>user</code> and <code>password</code>, then <code>GET /api/status</code>.</p>
</body>
</html>
`

func trimLeadingSlash(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p[1:]
	}
	return p
}
