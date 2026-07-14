package web

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/Robin831/Forge/internal/ingot"
	"github.com/Robin831/Forge/internal/ipc"
)

// handleHealthz is the unauthenticated liveness endpoint.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleLoginPage handles GET /login.
//
// Top-level browser navigations redirect authenticated users to the
// dashboard and serve the SPA for unauthenticated users so the
// LoginPage can render. Non-browser fetch clients (Accept: */*)
// receive a JSON auth-status payload instead.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	sess := SessionFromContext(r.Context())
	if isBrowserNavigation(r) {
		if sess != nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		s.staticH(w, r)
		return
	}
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          sess.Username,
	})
}

// handleAuthStatus is the strict JSON auth probe for GET /api/auth/status.
// It never redirects or serves HTML — the response is always JSON so
// fetch clients can reliably parse it regardless of Accept header.
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	sess := SessionFromContext(r.Context())
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          sess.Username,
	})
}

// isBrowserNavigation reports whether the request looks like a top-level
// HTML page load rather than a fetch/XHR JSON call. Browsers include
// text/html in the Accept header for navigation requests; fetch defaults
// to */* and explicit JSON callers send application/json.
func isBrowserNavigation(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// handleLogin validates form-encoded credentials and issues a session
// cookie on success. Returns 401 with an opaque error message on any
// failure to avoid leaking which half of the credential pair was wrong.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid form")
		return
	}
	username := strings.TrimSpace(r.PostFormValue("user"))
	password := r.PostFormValue("password")
	if username == "" || password == "" {
		writeError(w, http.StatusBadRequest, "user and password are required")
		return
	}
	ip := s.clientIP(r)
	// Throttle before verifying: a run of recent failures for this username
	// or IP delays the response, slowing online guessing. The delay is
	// applied regardless of whether these particular credentials are valid so
	// timing does not leak the outcome.
	if delay := s.throttle.delay(username, ip); delay > 0 {
		select {
		case s.throttleSem <- struct{}{}:
			s.logger.Info("web login throttled", "user", username, "remote", ip, "delay", delay.String())
			s.throttleSleep(delay)
			<-s.throttleSem
		default:
			writeError(w, http.StatusTooManyRequests, "too many login attempts")
			return
		}
	}
	if err := VerifyCredentials(s.cfg.Users, username, password); err != nil {
		s.throttle.recordFailure(username, ip)
		s.logger.Info("web login failed", "user", username, "remote", ip)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	// Rotate: invalidate any session token presented on this request so a
	// pre-login (potentially fixated or captured) token cannot survive a
	// successful sign-in.
	s.deleteSessionFromRequest(r)
	if _, err := s.createSession(w, username); err != nil {
		s.logger.Error("web login session create failed", "user", username, "error", err)
		writeError(w, http.StatusInternalServerError, "session creation failed")
		return
	}
	s.throttle.recordSuccess(username)
	s.logger.Info("web login ok", "user", username, "remote", ip)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"user":          username,
	})
}

// handleLogout deletes the session row and clears the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess := SessionFromContext(r.Context())
	if sess != nil {
		if err := s.db.DeleteWebSession(sess.TokenHash); err != nil {
			s.logger.Warn("web session delete failed", "error", err)
		}
	}
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleMe returns the authenticated user info.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess := SessionFromContext(r.Context())
	if sess == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":       sess.Username,
		"expires_at": sess.ExpiresAt,
	})
}

// dispatchIPC sends a command via the in-process handler and returns the
// raw response. Centralising it keeps the handlers below tiny.
func (s *Server) dispatchIPC(cmdType string) ipc.Response {
	return s.handler(ipc.Command{Type: cmdType})
}

// handleStatus mirrors the IPC "status" command, enriching each active worker
// that has a pr_number with its pr_url so Hearth 2.0 can render a clickable
// GitHub link (the IPC WorkerInfo carries the number but not the URL).
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := s.dispatchIPC("status")
	if resp.Type == "status" {
		resp.Payload = s.enrichWorkerPRURLs(resp.Payload)
	}
	s.writeIPCResponse(w, resp)
}

// enrichWorkerPRURLs adds a "pr_url" field to each worker in the status payload
// that has a positive "pr_number". The URL is constructed from the anvil's
// GitHub repo base ("<repo>/pull/<n>"), which works whether the PR was opened
// by Forge or adopted from a hand-opened branch; it falls back to the bead's
// ingot URL when the repo base for that anvil is unknown. All other payload
// fields are preserved verbatim. Best-effort: any decode/lookup failure returns
// the original payload unchanged, so the status endpoint never breaks on this.
func (s *Server) enrichWorkerPRURLs(payload json.RawMessage) json.RawMessage {
	if len(payload) == 0 {
		return payload
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}
	rawWorkers, ok := root["workers"]
	if !ok {
		return payload
	}
	var workers []map[string]any
	if err := json.Unmarshal(rawWorkers, &workers); err != nil {
		return payload
	}
	var conn *sql.DB
	if s.db != nil {
		conn = s.db.Conn()
	}
	changed := false
	for _, wm := range workers {
		prNum, ok := wm["pr_number"].(float64)
		if !ok || prNum <= 0 {
			continue
		}
		beadID, _ := wm["bead_id"].(string)
		anvil, _ := wm["anvil"].(string)
		if anvil == "" {
			continue
		}
		prURL := ""
		if base := s.anvilRepoURLs[anvil]; base != "" {
			prURL = fmt.Sprintf("%s/pull/%d", base, int(prNum))
		} else if conn != nil && beadID != "" {
			// Fallback: the ingot stores the URL for Forge-created PRs.
			if ig, err := ingot.GetIngot(conn, beadID, anvil); err == nil && ig != nil {
				prURL = ig.PRURL
			}
		}
		if prURL == "" {
			continue
		}
		wm["pr_url"] = prURL
		changed = true
	}
	if !changed {
		return payload
	}
	newWorkers, err := json.Marshal(workers)
	if err != nil {
		return payload
	}
	root["workers"] = newWorkers
	out, err := json.Marshal(root)
	if err != nil {
		return payload
	}
	return out
}

// handleQueue mirrors the IPC "queue" command.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	resp := s.dispatchIPC("queue")
	s.writeIPCResponse(w, resp)
}

// handleWorkers mirrors the IPC "workers" command, enriching each worker that
// has a pr_number with its pr_url so Hearth 2.0's pipeline view and Workers
// pane (both fed by /api/workers) can render a clickable GitHub link.
func (s *Server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	resp := s.dispatchIPC("workers")
	resp.Payload = s.enrichWorkerPRURLs(resp.Payload)
	s.writeIPCResponse(w, resp)
}

// handleEvents mirrors the IPC "events" command. It optionally accepts a
// ?limit= query parameter (1-500) to control how many recent events are
// returned; the daemon clamps anything outside that range to its default.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	cmd := ipc.Command{Type: "events"}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "limit must be an integer")
			return
		}
		if n < 1 {
			n = 1
		} else if n > 500 {
			n = 500
		}
		payload, _ := json.Marshal(map[string]int{"limit": n})
		cmd.Payload = payload
	}
	s.writeIPCResponse(w, s.handler(cmd))
}

// writeIPCResponse forwards an ipc.Response to the HTTP client. Successful
// responses pass through their JSON payload as-is so the wire format stays
// 1:1 with the IPC schema. Error responses are converted to a 500 with the
// embedded message, since the daemon's error payloads are typed as
// {"message": "..."}. "queued" responses (async commands accepted by the
// daemon) are returned as 202 Accepted so the SPA can show optimistic UI
// while the goroutine completes.
func (s *Server) writeIPCResponse(w http.ResponseWriter, resp ipc.Response) {
	switch resp.Type {
	case "ok", "status":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if len(resp.Payload) == 0 {
			_, _ = w.Write([]byte("{}"))
			return
		}
		_, _ = w.Write(resp.Payload)
	case "queued":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		body := map[string]any{"queued": true, "request_id": resp.RequestID}
		if len(resp.Payload) > 0 {
			var qp ipc.QueuedPayload
			if err := json.Unmarshal(resp.Payload, &qp); err == nil && qp.Message != "" {
				body["message"] = qp.Message
			}
		}
		_ = json.NewEncoder(w).Encode(body)
	case "error":
		var body struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(resp.Payload, &body)
		if body.Message == "" {
			body.Message = "command failed"
		}
		writeError(w, http.StatusInternalServerError, body.Message)
	default:
		writeError(w, http.StatusInternalServerError, "unexpected response type "+resp.Type)
	}
}
