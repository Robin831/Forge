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
		// Assay findings for one PR, plus a live SSE channel that re-emits the
		// findings/run snapshot whenever it changes so the PR detail panel
		// updates without a manual refresh.
		r.Get("/prs/{id}/findings", s.handlePRFindings)
		r.Get("/prs/{id}/findings/stream", s.handlePRFindingsStream)
		r.Get("/bead/{bead_id}", s.handleBeadDetail)
		r.Get("/bead/{bead_id}/deps", s.handleBeadDeps)

		// Daemon-wide dispatch control (Hearth 2.0). Pause/resume the
		// auto-dispatch of new workers without touching running ones.
		r.Post("/dispatch/pause", s.handleDispatchPause)
		r.Post("/dispatch/resume", s.handleDispatchResume)

		// Destructive admin actions (Hearth 2.0).
		r.Post("/worker/{id}/kill", s.handleKillWorker)
		r.Post("/queue/{bead_id}/retry", s.handleQueueRetry)
		r.Post("/queue/{bead_id}/dispatch", s.handleQueueDispatch)
		r.Post("/queue/{bead_id}/clarify", s.handleQueueClarify)
		r.Post("/queue/{bead_id}/unclarify", s.handleQueueUnclarify)
		r.Post("/queue/{bead_id}/stop", s.handleQueueStop)
		r.Post("/queue/{bead_id}/apply-dispatch-tag", s.handleQueueApplyDispatchTag)
		r.Post("/bead/{bead_id}/close", s.handleBeadClose)
		r.Post("/bead/{bead_id}/label/add", s.handleBeadLabelAdd)
		r.Post("/bead/{bead_id}/label/remove", s.handleBeadLabelRemove)
		r.Post("/bead/{bead_id}/note", s.handleBeadNote)
		r.Post("/bead/{bead_id}/comment", s.handleBeadAddComment)
		r.Post("/bead/{bead_id}/steer", s.handleBeadSteer)

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
		r.Post("/prs/{id}/rerun-assay", s.handlePRRerunAssay)

		// Beads-Forge sessions (Hearth 2.0). Sessions are scoped per
		// signed-in user. The /turn endpoint drives the AI loop —
		// running claude per-turn, requesting plans, and stepping
		// through the grilling stage.
		r.Get("/forge/anvils", s.handleForgeAnvilsList)
		r.Get("/forge/sessions", s.handleForgeSessionsList)
		r.Post("/forge/sessions", s.handleForgeSessionsCreate)
		r.Get("/forge/sessions/{id}", s.handleForgeSessionGet)
		r.Patch("/forge/sessions/{id}", s.handleForgeSessionUpdate)
		r.Delete("/forge/sessions/{id}", s.handleForgeSessionDelete)
		r.Post("/forge/sessions/{id}/messages", s.handleForgeSessionAppend)
		r.Post("/forge/sessions/{id}/turn", s.handleForgeSessionTurn)
		r.Get("/forge/sessions/{id}/turn/{turn_id}", s.handleForgeSessionTurnGet)
		r.Get("/forge/sessions/{id}/turn/{turn_id}/stream", s.handleForgeSessionTurnStream)
		r.Post("/forge/sessions/{id}/create-beads", s.handleForgeSessionCreateBeads)

		// Hearth 2.0 resolve-needs-attention page. The POST endpoint is a
		// thin façade over the daemon's queue_* IPC verbs so the SPA
		// renders a single action picker; the GET endpoint returns the
		// full untruncated escalation message plus git context shelled
		// from the worker's worktree.
		r.Post("/forge/resolve", s.handleForgeResolve)
		r.Get("/forge/escalation/{bead_id}", s.handleForgeEscalation)
		// Bead-centric needs-attention list (Forge-iz6s). Driven by the
		// retries table (NeedsHumanBeads + ClarificationNeededBeads) so
		// escalations are findable and resolvable regardless of whether a
		// live worker row still exists.
		r.Get("/forge/needs-attention", s.handleForgeNeedsAttention)

		// Forge config read/write (Forge-e4xe). GET returns the managed
		// boolean settings with per-key metadata; PATCH persists one or
		// more of them to ~/.forge/config.yaml via a YAML node-tree edit
		// (preserving comments/unrelated keys) so the fsnotify watcher in
		// internal/hotreload picks the change up.
		r.Get("/forge/config", s.handleForgeConfigGet)
		r.Patch("/forge/config", s.handleForgeConfigPatch)
		// Per-anvil settings write (Forge-bfch). PATCH accepts a flat JSON
		// object of allowlisted keys (e.g. {"auto_merge": true, "schematic_enabled": null})
		// — the anvil name is taken from the URL, not the body. Validates the
		// anvil name (404 if unknown) and every key, distinguishes tri-state
		// *bool clears (JSON null → inherit) from explicit true/false, and
		// persists to the same config.yaml so internal/hotreload picks the
		// change up. The response reports per-key hot-reload coverage.
		r.Patch("/forge/config/anvils/{name}", s.handleForgeAnvilConfigPatch)
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
