package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/go-chi/chi/v5"
)

// validLabel restricts labels to GitHub's accepted character set so a typo or
// stray punctuation can't slip into bd update. The daemon execs bd directly
// (no shell), so this is a format check, not shell escaping. 64 chars matches
// the GitHub limit.
var validLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\-]{0,63}$`)

// actionRequest is the common JSON body shape for action endpoints. Every
// destructive action is scoped to a single (bead, anvil) pair; many of them
// also carry an optional reason (for clarify/stop) or note text.
type actionRequest struct {
	Anvil    string `json:"anvil"`
	Reason   string `json:"reason,omitempty"`
	Note     string `json:"note,omitempty"`
	Label    string `json:"label,omitempty"`
	ForceRun bool   `json:"force_run,omitempty"`
}

// decodeActionRequest reads and validates the shared JSON body. It accepts an
// empty body so endpoints whose request shape is purely the URL (e.g. kill by
// worker id) can share the helper. The 32 KiB cap keeps handlers cheap to
// reason about even though chi already limits routing.
func decodeActionRequest(r *http.Request) (actionRequest, error) {
	var req actionRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 32*1024))
	if err != nil {
		return req, err
	}
	if len(body) == 0 {
		return req, nil
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return req, err
	}
	req.Anvil = strings.TrimSpace(req.Anvil)
	req.Reason = strings.TrimSpace(req.Reason)
	req.Label = strings.TrimSpace(req.Label)
	return req, nil
}

// requireBeadAndAnvil validates the URL path bead id and the body anvil. It
// writes the error response and returns ok=false when either fails so the
// handler can early-return.
func requireBeadAndAnvil(w http.ResponseWriter, r *http.Request) (string, actionRequest, bool) {
	beadID := chi.URLParam(r, "bead_id")
	if beadID == "" {
		beadID = chi.URLParam(r, "id")
	}
	if !isValidBeadID(beadID) {
		writeError(w, http.StatusBadRequest, "invalid bead id")
		return "", actionRequest{}, false
	}
	req, err := decodeActionRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return "", actionRequest{}, false
	}
	if req.Anvil == "" {
		writeError(w, http.StatusBadRequest, "anvil is required")
		return "", actionRequest{}, false
	}
	return beadID, req, true
}

// dispatch sends an IPC command and writes the result. The command type and
// payload are passed through unchanged. Returns the response so individual
// handlers can log or augment it if needed; callers that only need to forward
// the response should ignore the return value.
func (s *Server) dispatchAction(w http.ResponseWriter, cmdType string, payload any) ipc.Response {
	body, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode payload: "+err.Error())
		return ipc.Response{Type: "error"}
	}
	resp := s.handler(ipc.Command{Type: cmdType, Payload: body})
	s.writeIPCResponse(w, resp)
	return resp
}

// handleKillWorker proxies POST /api/worker/{id}/kill to the daemon's
// kill_worker IPC. The body may be empty.
func (s *Server) handleKillWorker(w http.ResponseWriter, r *http.Request) {
	workerID := chi.URLParam(r, "id")
	if workerID == "" || !validWorkerID.MatchString(workerID) {
		writeError(w, http.StatusBadRequest, "invalid worker id")
		return
	}
	s.logActor(r, "kill_worker", "worker", workerID)
	s.dispatchAction(w, "kill_worker", ipc.KillWorkerPayload{WorkerID: workerID})
}

// handleQueueRetry proxies POST /api/queue/{bead_id}/retry to the daemon's
// retry_bead IPC.
func (s *Server) handleQueueRetry(w http.ResponseWriter, r *http.Request) {
	beadID, req, ok := requireBeadAndAnvil(w, r)
	if !ok {
		return
	}
	s.logActor(r, "retry_bead", "bead", beadID, "anvil", req.Anvil)
	s.dispatchAction(w, "retry_bead", ipc.RetryBeadPayload{BeadID: beadID, Anvil: req.Anvil})
}

// handleQueueDispatch proxies POST /api/queue/{bead_id}/dispatch to the
// daemon's run_bead IPC.
func (s *Server) handleQueueDispatch(w http.ResponseWriter, r *http.Request) {
	beadID, req, ok := requireBeadAndAnvil(w, r)
	if !ok {
		return
	}
	s.logActor(r, "run_bead", "bead", beadID, "anvil", req.Anvil, "force_run", req.ForceRun)
	s.dispatchAction(w, "run_bead", ipc.RunBeadPayload{
		BeadID:   beadID,
		Anvil:    req.Anvil,
		ForceRun: req.ForceRun,
	})
}

// handleQueueClarify proxies POST /api/queue/{bead_id}/clarify to the daemon's
// set_clarification IPC. A non-empty reason is required.
func (s *Server) handleQueueClarify(w http.ResponseWriter, r *http.Request) {
	beadID, req, ok := requireBeadAndAnvil(w, r)
	if !ok {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}
	s.logActor(r, "set_clarification", "bead", beadID, "anvil", req.Anvil)
	s.dispatchAction(w, "set_clarification", ipc.ClarificationPayload{
		BeadID: beadID, Anvil: req.Anvil, Reason: req.Reason,
	})
}

// handleQueueUnclarify proxies POST /api/queue/{bead_id}/unclarify to the
// daemon's clear_clarification IPC.
func (s *Server) handleQueueUnclarify(w http.ResponseWriter, r *http.Request) {
	beadID, req, ok := requireBeadAndAnvil(w, r)
	if !ok {
		return
	}
	s.logActor(r, "clear_clarification", "bead", beadID, "anvil", req.Anvil)
	s.dispatchAction(w, "clear_clarification", ipc.ClarificationPayload{
		BeadID: beadID, Anvil: req.Anvil,
	})
}

// handleQueueStop proxies POST /api/queue/{bead_id}/stop to the daemon's
// stop_bead IPC.
func (s *Server) handleQueueStop(w http.ResponseWriter, r *http.Request) {
	beadID, req, ok := requireBeadAndAnvil(w, r)
	if !ok {
		return
	}
	s.logActor(r, "stop_bead", "bead", beadID, "anvil", req.Anvil)
	s.dispatchAction(w, "stop_bead", ipc.StopBeadPayload{
		BeadID: beadID, Anvil: req.Anvil, Reason: req.Reason,
	})
}

// handleBeadClose proxies POST /api/bead/{id}/close to the daemon's close_bead
// IPC, which shells out to bd close.
func (s *Server) handleBeadClose(w http.ResponseWriter, r *http.Request) {
	beadID, req, ok := requireBeadAndAnvil(w, r)
	if !ok {
		return
	}
	s.logActor(r, "close_bead", "bead", beadID, "anvil", req.Anvil)
	s.dispatchAction(w, "close_bead", ipc.CloseBeadPayload{BeadID: beadID, Anvil: req.Anvil})
}

// handleBeadLabelAdd proxies POST /api/bead/{id}/label/add to the daemon's
// update_label IPC. The label is taken from the request body.
func (s *Server) handleBeadLabelAdd(w http.ResponseWriter, r *http.Request) {
	s.handleBeadLabelChange(w, r, "add")
}

// handleBeadLabelRemove proxies POST /api/bead/{id}/label/remove to the
// daemon's update_label IPC.
func (s *Server) handleBeadLabelRemove(w http.ResponseWriter, r *http.Request) {
	s.handleBeadLabelChange(w, r, "remove")
}

func (s *Server) handleBeadLabelChange(w http.ResponseWriter, r *http.Request, action string) {
	beadID, req, ok := requireBeadAndAnvil(w, r)
	if !ok {
		return
	}
	if req.Label == "" {
		writeError(w, http.StatusBadRequest, "label is required")
		return
	}
	if !validLabel.MatchString(req.Label) {
		writeError(w, http.StatusBadRequest, "invalid label")
		return
	}
	s.logActor(r, "update_label", "bead", beadID, "anvil", req.Anvil, "label", req.Label, "action", action)
	s.dispatchAction(w, "update_label", ipc.UpdateLabelPayload{
		BeadID: beadID, Anvil: req.Anvil, Label: req.Label, Action: action,
	})
}

// handleBeadNote proxies POST /api/bead/{id}/note to the daemon's append_notes
// IPC, which shells out to bd update --append-notes.
func (s *Server) handleBeadNote(w http.ResponseWriter, r *http.Request) {
	beadID, req, ok := requireBeadAndAnvil(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Note) == "" {
		writeError(w, http.StatusBadRequest, "note is required")
		return
	}
	s.logActor(r, "append_notes", "bead", beadID, "anvil", req.Anvil)
	s.dispatchAction(w, "append_notes", ipc.AppendNotesPayload{
		BeadID: beadID, Anvil: req.Anvil, Notes: req.Note,
	})
}

// maxCommentBodyBytes caps the payload size on POST /api/bead/{id}/comment.
// The body is forwarded to bd as a single argv entry, and Windows caps the
// entire CreateProcess command line at 32,767 characters — so the 32 KiB
// budget we use elsewhere is unsafe here. 8 KiB leaves comfortable headroom
// for the bd path, subcommand, bead id, --json flag, and quoting overhead
// while still being well above the comment lengths anyone types in practice.
const maxCommentBodyBytes = 8 * 1024

// addCommentRequest is the JSON shape for POST /api/bead/{id}/comment. anvil
// is required so the handler can resolve the on-disk path for cmd.Dir; body
// is the comment text passed verbatim to `bd comments add`.
type addCommentRequest struct {
	Anvil string `json:"anvil"`
	Body  string `json:"body"`
}

// handleBeadAddComment proxies POST /api/bead/{id}/comment to
// `bd comments add <id> <body> --json`. It mirrors the read path's shell-out
// pattern (bdCommentsJSON in views.go) rather than going through IPC because
// the daemon's comment-write path is the bd CLI directly. On success the
// created comment is parsed out of bd's JSON output and returned with 201;
// when bd returns nothing parseable the response is 204 so the SPA can fall
// back to its next poll. bd failures become 502 (a 5xx upstream-failure)
// with the standard {"error": "..."} body.
func (s *Server) handleBeadAddComment(w http.ResponseWriter, r *http.Request) {
	beadID := chi.URLParam(r, "bead_id")
	if !isValidBeadID(beadID) {
		writeError(w, http.StatusBadRequest, "invalid bead id")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxCommentBodyBytes+1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	var req addCommentRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
	}
	req.Anvil = strings.TrimSpace(req.Anvil)
	body := strings.TrimSpace(req.Body)
	if body == "" {
		writeError(w, http.StatusBadRequest, "body is required")
		return
	}
	if len(body) > maxCommentBodyBytes {
		writeError(w, http.StatusBadRequest, "body too long")
		return
	}
	if req.Anvil == "" {
		writeError(w, http.StatusBadRequest, "anvil is required")
		return
	}

	// Resolve the anvil to its on-disk path *before* shelling out. Without
	// this guard an unknown (or unregistered) anvil name would leave cmd.Dir
	// empty, causing `bd comments add` to run in the daemon's cwd against
	// whichever beads DB happens to be there — silently writing to the wrong
	// repository. Reject the request instead.
	anvilPath := s.resolveAnvilPath(req.Anvil)
	if anvilPath == "" {
		writeError(w, http.StatusBadRequest, "unknown anvil")
		return
	}
	s.logActor(r, "add_comment", "bead", beadID, "anvil", req.Anvil)

	ctx, cancel := context.WithTimeout(r.Context(), executil.DefaultBdTimeout)
	defer cancel()
	out, err := bdCommentsAdd(ctx, anvilPath, beadID, body)
	if err != nil {
		s.logger.Warn("bd comments add failed", "bead_id", beadID, "anvil", req.Anvil, "error", err)
		writeError(w, http.StatusBadGateway, "bd comments add failed: "+err.Error())
		return
	}

	var parsed struct {
		ID        string `json:"id"`
		Author    string `json:"author"`
		Text      string `json:"text"`
		CreatedAt string `json:"created_at"`
	}
	if len(out) > 0 {
		_ = executil.DecodeJSON(out, &parsed)
	}
	if parsed.ID == "" && parsed.Text == "" {
		// bd succeeded but produced no parseable JSON (e.g. older bd
		// versions). Return 204 so the SPA falls back to its next poll
		// for the canonical list.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"comment": beadDetailComment{
			ID:        parsed.ID,
			Author:    parsed.Author,
			Body:      parsed.Text,
			CreatedAt: parsed.CreatedAt,
		},
	})
}

// logActor emits an audit-trail log line tagged with the authenticated user
// for every destructive action. The session is guaranteed by the auth
// middleware, but we tolerate a missing one defensively.
func (s *Server) logActor(r *http.Request, action string, kv ...any) {
	user := "unknown"
	if sess := SessionFromContext(r.Context()); sess != nil {
		user = sess.Username
	}
	all := append([]any{"action", action, "user", user, "remote", clientIP(r)}, kv...)
	s.logger.Info("web action", all...)
}
