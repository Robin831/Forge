package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Robin831/Forge/internal/ipc"
)

// handleHealthz is the unauthenticated liveness endpoint.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleLoginStatus reports whether a session is already attached.
//
// For JSON clients (typically the frontend's auth-refresh fetch), the
// session state is returned as a JSON payload. Top-level browser
// navigations to /login are handled differently: an authenticated
// browser is redirected to the dashboard so the user does not land on a
// redundant login form, and an unauthenticated browser is served the
// SPA bundle so the LoginPage can render and accept credentials.
func (s *Server) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	sess := SessionFromContext(r.Context())
	if isBrowserNavigation(r) {
		if sess != nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if s.staticH != nil {
			s.staticH(w, r)
			return
		}
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
	if err := VerifyCredentials(s.cfg.Users, username, password); err != nil {
		s.logger.Info("web login failed", "user", username, "remote", clientIP(r))
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if _, err := s.createSession(w, username); err != nil {
		s.logger.Error("web login session create failed", "user", username, "error", err)
		writeError(w, http.StatusInternalServerError, "session creation failed")
		return
	}
	s.logger.Info("web login ok", "user", username, "remote", clientIP(r))
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

// handleStatus mirrors the IPC "status" command.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := s.dispatchIPC("status")
	s.writeIPCResponse(w, resp)
}

// handleQueue mirrors the IPC "queue" command.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	resp := s.dispatchIPC("queue")
	s.writeIPCResponse(w, resp)
}

// handleWorkers mirrors the IPC "workers" command.
func (s *Server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	resp := s.dispatchIPC("workers")
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
// {"message": "..."}.
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
