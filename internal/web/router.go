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

	// Build the SPA static handler once so both the catch-all route and
	// handleLoginStatus (which may fall back to it for unauthenticated
	// browser navigations) can share a single instance.
	s.staticH = s.staticHandler()

	// Public endpoints — no authentication.
	r.Get("/healthz", s.handleHealthz)

	// Login: optional auth so the page can detect an existing session.
	r.Group(func(r chi.Router) {
		r.Use(s.optionalAuth)
		r.Post("/login", s.handleLogin)
		r.Get("/login", s.handleLoginPage)
	})

	// Logout requires a valid session — otherwise it is a no-op.
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Post("/logout", s.handleLogout)
	})

	// All /api/* endpoints require auth except the auth status probe,
	// which uses optional auth so an unauthenticated client can also
	// learn that they are unauthenticated.
	r.Group(func(r chi.Router) {
		r.Use(s.optionalAuth)
		r.Get("/api/auth/status", s.handleAuthStatus)
	})
	r.Route("/api", func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Use(s.csrfCheck)
		r.Get("/me", s.handleMe)
		r.Get("/status", s.handleStatus)
		r.Get("/queue", s.handleQueue)
		r.Get("/workers", s.handleWorkers)
		r.Get("/events", s.handleEvents)
		r.Get("/activity/stream", s.handleActivityStream)
		r.Get("/worker/{id}/log", s.handleWorkerLogTail)
		r.Get("/worker/{id}/stream", s.handleWorkerLogStream)
		r.Get("/crucibles", s.handleCrucibles)
		r.Get("/ingots", s.handleIngots)
		r.Get("/ingots/{bead_id}", s.handleIngot)
		r.Get("/history/workers", s.handleHistoryWorkers)
		r.Get("/costs", s.handleCosts)
		r.Get("/prs/all", s.handlePRs)
		r.Get("/bead/{bead_id}", s.handleBeadDetail)

		// Destructive admin actions (Hearth 2.0).
		r.Post("/worker/{id}/kill", s.handleKillWorker)
		r.Post("/queue/{bead_id}/retry", s.handleQueueRetry)
		r.Post("/queue/{bead_id}/dispatch", s.handleQueueDispatch)
		r.Post("/queue/{bead_id}/clarify", s.handleQueueClarify)
		r.Post("/queue/{bead_id}/unclarify", s.handleQueueUnclarify)
		r.Post("/queue/{bead_id}/stop", s.handleQueueStop)
		r.Post("/bead/{bead_id}/close", s.handleBeadClose)
		r.Post("/bead/{bead_id}/label/add", s.handleBeadLabelAdd)
		r.Post("/bead/{bead_id}/label/remove", s.handleBeadLabelRemove)
		r.Post("/bead/{bead_id}/note", s.handleBeadNote)

		// Per-PR actions on the /prs tab. Each route resolves the PR row
		// from state.db and dispatches an in-process IPC command. External
		// PRs (ext-* bead IDs) reach the same merge/approve/close/bellows
		// handlers; the UI hides bellows-managed actions (fix-ci /
		// fix-comments / fix-conflicts) for them.
		r.Post("/prs/{id}/merge", s.handlePRMerge)
		r.Post("/prs/{id}/close", s.handlePRClose)
		r.Post("/prs/{id}/approve", s.handlePRApprove)
		r.Post("/prs/{id}/bellows", s.handlePRBellows)
		r.Post("/prs/{id}/fix-ci", s.handlePRFixCI)
		r.Post("/prs/{id}/fix-comments", s.handlePRFixComments)
		r.Post("/prs/{id}/fix-conflicts", s.handlePRFixConflicts)
		r.Post("/prs/{id}/reset-counters", s.handlePRResetCounters)

		// Beads-Forge sessions (Hearth 2.0). Sessions are scoped per
		// signed-in user. The /turn endpoint drives the AI loop —
		// running claude per-turn, requesting plans, and stepping
		// through the grilling stage.
		r.Get("/forge/sessions", s.handleForgeSessionsList)
		r.Post("/forge/sessions", s.handleForgeSessionsCreate)
		r.Get("/forge/sessions/{id}", s.handleForgeSessionGet)
		r.Patch("/forge/sessions/{id}", s.handleForgeSessionUpdate)
		r.Delete("/forge/sessions/{id}", s.handleForgeSessionDelete)
		r.Post("/forge/sessions/{id}/messages", s.handleForgeSessionAppend)
		r.Post("/forge/sessions/{id}/turn", s.handleForgeSessionTurn)
	})

	// Static UI fallback. The next bead replaces this with the embedded
	// React build; for now we serve the placeholder index.html out of the
	// embedded dist directory so the deployment works end-to-end.
	r.Handle("/*", s.staticH)
	return r
}

// staticHandler serves the embedded UI bundle with SPA fallback to
// index.html. When the dist directory is empty (typical until the frontend
// bead lands), it serves a minimal placeholder.
func (s *Server) staticHandler() http.HandlerFunc {
	dist, err := distFS()
	if err != nil {
		// No embedded UI — return the placeholder handler, which serves
		// a minimal page for / and /index.html and 404s everything else.
		return s.placeholderHandler
	}
	fileServer := http.FileServer(http.FS(dist))
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" || path == "" {
			path = "/index.html"
		}
		// SPA fallback: when the requested file does not exist, serve
		// index.html so client-side routing can take over. Rewrite the
		// path to "/" rather than "/index.html" because http.FileServer
		// canonicalises explicit /index.html requests with a 301 to /.
		if _, err := fs.Stat(dist, trimLeadingSlash(path)); err != nil {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	}
}

// placeholderHandler is used when no embedded UI is present. It serves
// the minimal placeholder HTML for all paths so that SPA routes like
// /login work in the documented "no embedded UI" fallback mode.
func (s *Server) placeholderHandler(w http.ResponseWriter, r *http.Request) {
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
