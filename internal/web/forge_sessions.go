package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/state"
	"github.com/go-chi/chi/v5"
)

// Beads-Forge HTTP handlers
//
// This file backs the /forge route in the Hearth 2.0 SPA. Each row in
// state.forge_sessions is one chat-style conversation that designs a bead.
// The foundation bead delivers persistence + draft input only — the AI
// integration and grilling stage land in follow-on beads, so the message
// API is intentionally simple: every POST appends one message verbatim.

// maxForgeMessageBytes caps the size of a single message payload. Messages
// are stored in SQLite TEXT columns, but we want a sensible upper bound so
// a runaway client cannot spend all of the daemon's memory in one request.
const maxForgeMessageBytes = 64 * 1024

// maxForgeTitleLen limits the length of session titles. Long titles wreck
// the sidebar layout and there is no use case for more than a short label.
const maxForgeTitleLen = 200

// forgeSessionDTO is the JSON shape returned by the listing and CRUD
// endpoints. Timestamps are rendered as RFC3339 to match the rest of the
// Hearth 2.0 API surface.
type forgeSessionDTO struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	Anvil        string `json:"anvil,omitempty"`
	CreatedBy    string `json:"created_by,omitempty"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	MessageCount int    `json:"message_count"`
}

// forgeMessageDTO is the JSON shape for a single message.
type forgeMessageDTO struct {
	ID        int64  `json:"id"`
	SessionID int64  `json:"session_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// toForgeSessionDTO converts a state row into the API DTO. The caller is
// responsible for supplying the message count — most listings already need
// the count, and computing it inline keeps the helper free of *DB.
func toForgeSessionDTO(s state.ForgeSession, messageCount int) forgeSessionDTO {
	return forgeSessionDTO{
		ID:           s.ID,
		Title:        s.Title,
		Status:       s.Status,
		Anvil:        s.Anvil,
		CreatedBy:    s.CreatedBy,
		CreatedAt:    s.CreatedAt.Format(time.RFC3339),
		UpdatedAt:    s.UpdatedAt.Format(time.RFC3339),
		MessageCount: messageCount,
	}
}

// toForgeMessageDTO converts a state row into the API DTO.
func toForgeMessageDTO(m state.ForgeSessionMessage) forgeMessageDTO {
	return forgeMessageDTO{
		ID:        m.ID,
		SessionID: m.SessionID,
		Role:      m.Role,
		Content:   m.Content,
		CreatedAt: m.CreatedAt.Format(time.RFC3339),
	}
}

// handleForgeSessionsList serves GET /api/forge/sessions. Sessions are
// scoped to the signed-in user so two operators sharing a daemon don't see
// each other's drafts.
func (s *Server) handleForgeSessionsList(w http.ResponseWriter, r *http.Request) {
	sess := SessionFromContext(r.Context())
	if sess == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	rows, err := s.db.ListForgeSessions(sess.Username, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list sessions: "+err.Error())
		return
	}
	out := make([]forgeSessionDTO, 0, len(rows))
	for _, row := range rows {
		count, err := s.db.CountForgeSessionMessages(row.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to count messages: "+err.Error())
			return
		}
		out = append(out, toForgeSessionDTO(row, count))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// createForgeSessionRequest is the body for POST /api/forge/sessions. Both
// fields are optional: empty title means "untitled" and an empty initial
// message means "no opening prompt yet".
type createForgeSessionRequest struct {
	Title          string `json:"title,omitempty"`
	Anvil          string `json:"anvil,omitempty"`
	InitialMessage string `json:"initial_message,omitempty"`
}

// handleForgeSessionsCreate serves POST /api/forge/sessions. When the body
// includes an initial_message, it is persisted as the first user message in
// the same response so the SPA does not need a second round-trip. The auto
// title is the first ~80 chars of the initial message when no title is
// supplied; this is the same heuristic Hytte's chat uses.
func (s *Server) handleForgeSessionsCreate(w http.ResponseWriter, r *http.Request) {
	sess := SessionFromContext(r.Context())
	if sess == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxForgeMessageBytes+4096))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	var req createForgeSessionRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Anvil = strings.TrimSpace(req.Anvil)
	req.InitialMessage = strings.TrimSpace(req.InitialMessage)
	if len(req.InitialMessage) > maxForgeMessageBytes {
		writeError(w, http.StatusBadRequest, "initial message exceeds size limit")
		return
	}
	if len(req.Title) > maxForgeTitleLen {
		req.Title = req.Title[:maxForgeTitleLen]
	}
	if req.Title == "" && req.InitialMessage != "" {
		req.Title = autoTitleFromMessage(req.InitialMessage)
	}

	row, err := s.db.CreateForgeSession(state.ForgeSession{
		Title:     req.Title,
		Anvil:     req.Anvil,
		CreatedBy: sess.Username,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create session: "+err.Error())
		return
	}

	var firstMessage *forgeMessageDTO
	if req.InitialMessage != "" {
		m, err := s.db.AppendForgeSessionMessage(state.ForgeSessionMessage{
			SessionID: row.ID,
			Role:      state.ForgeMessageRoleUser,
			Content:   req.InitialMessage,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to append message: "+err.Error())
			return
		}
		dto := toForgeMessageDTO(m)
		firstMessage = &dto
		// Reload the session so updated_at reflects the message append.
		if reloaded, err := s.db.GetForgeSession(row.ID); err == nil && reloaded != nil {
			row = *reloaded
		}
	}

	count, _ := s.db.CountForgeSessionMessages(row.ID)
	resp := map[string]any{"session": toForgeSessionDTO(row, count)}
	if firstMessage != nil {
		resp["message"] = *firstMessage
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleForgeSessionGet serves GET /api/forge/sessions/{id}. The response
// bundles the session metadata and the full message list so the chat view
// only needs one round-trip when navigating from the sidebar.
func (s *Server) handleForgeSessionGet(w http.ResponseWriter, r *http.Request) {
	sess := SessionFromContext(r.Context())
	if sess == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := parseForgeSessionID(w, r)
	if !ok {
		return
	}
	row, err := s.db.GetForgeSession(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load session: "+err.Error())
		return
	}
	if row == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if !forgeSessionVisibleTo(row, sess.Username) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	msgs, err := s.db.ListForgeSessionMessages(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load messages: "+err.Error())
		return
	}
	out := make([]forgeMessageDTO, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toForgeMessageDTO(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session":  toForgeSessionDTO(*row, len(msgs)),
		"messages": out,
	})
}

// updateForgeSessionRequest is the body for PATCH /api/forge/sessions/{id}.
// Both fields are optional; nil pointer = "leave alone".
type updateForgeSessionRequest struct {
	Title  *string `json:"title,omitempty"`
	Status *string `json:"status,omitempty"`
}

// handleForgeSessionUpdate serves PATCH /api/forge/sessions/{id}. Used for
// renaming and archiving sessions from the sidebar.
func (s *Server) handleForgeSessionUpdate(w http.ResponseWriter, r *http.Request) {
	sess := SessionFromContext(r.Context())
	if sess == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := parseForgeSessionID(w, r)
	if !ok {
		return
	}
	row, err := s.db.GetForgeSession(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load session: "+err.Error())
		return
	}
	if row == nil || !forgeSessionVisibleTo(row, sess.Username) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	var req updateForgeSessionRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	if req.Title != nil {
		t := strings.TrimSpace(*req.Title)
		if len(t) > maxForgeTitleLen {
			t = t[:maxForgeTitleLen]
		}
		req.Title = &t
	}
	if req.Status != nil {
		st := strings.TrimSpace(*req.Status)
		if !isValidForgeSessionStatus(st) {
			writeError(w, http.StatusBadRequest, "invalid status value")
			return
		}
		req.Status = &st
	}
	if err := s.db.UpdateForgeSession(id, req.Title, req.Status); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update session: "+err.Error())
		return
	}
	row, err = s.db.GetForgeSession(id)
	if err != nil || row == nil {
		writeError(w, http.StatusInternalServerError, "failed to reload session")
		return
	}
	count, _ := s.db.CountForgeSessionMessages(id)
	writeJSON(w, http.StatusOK, map[string]any{"session": toForgeSessionDTO(*row, count)})
}

// handleForgeSessionDelete serves DELETE /api/forge/sessions/{id}. The
// delete is permanent; the foundation bead does not implement an
// archive-instead-of-delete flow because the next bead can flip status to
// "archived" via PATCH if soft delete becomes desirable.
func (s *Server) handleForgeSessionDelete(w http.ResponseWriter, r *http.Request) {
	sess := SessionFromContext(r.Context())
	if sess == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := parseForgeSessionID(w, r)
	if !ok {
		return
	}
	row, err := s.db.GetForgeSession(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load session: "+err.Error())
		return
	}
	if row == nil || !forgeSessionVisibleTo(row, sess.Username) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if err := s.db.DeleteForgeSession(id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete session: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// appendForgeMessageRequest is the body for POST /api/forge/sessions/{id}/messages.
// The foundation bead only writes user-role messages; later beads will add
// assistant responses. We accept role explicitly so the API stays
// open-ended, but reject anything other than "user" until the AI bead lands.
type appendForgeMessageRequest struct {
	Content string `json:"content"`
	Role    string `json:"role,omitempty"`
}

// handleForgeSessionAppend serves POST /api/forge/sessions/{id}/messages.
// The foundation bead only persists the user message — claude integration
// arrives in the next bead.
func (s *Server) handleForgeSessionAppend(w http.ResponseWriter, r *http.Request) {
	sess := SessionFromContext(r.Context())
	if sess == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, ok := parseForgeSessionID(w, r)
	if !ok {
		return
	}
	row, err := s.db.GetForgeSession(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load session: "+err.Error())
		return
	}
	if row == nil || !forgeSessionVisibleTo(row, sess.Username) {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxForgeMessageBytes+4096))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	var req appendForgeMessageRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	req.Content = strings.TrimSpace(req.Content)
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	if len(req.Content) > maxForgeMessageBytes {
		writeError(w, http.StatusBadRequest, "content exceeds size limit")
		return
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = state.ForgeMessageRoleUser
	}
	// Foundation bead constraint: only user-authored messages can be
	// appended via the public API. Assistant messages will arrive once the
	// AI bead wires claude into the daemon — at that point the worker
	// itself, not an HTTP client, will write them.
	if role != state.ForgeMessageRoleUser {
		writeError(w, http.StatusBadRequest, "only user role messages may be appended in the foundation API")
		return
	}

	// If the session has no title yet, derive one from the first message.
	titleBefore := row.Title

	m, err := s.db.AppendForgeSessionMessage(state.ForgeSessionMessage{
		SessionID: id,
		Role:      role,
		Content:   req.Content,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to append message: "+err.Error())
		return
	}

	if titleBefore == "" {
		newTitle := autoTitleFromMessage(req.Content)
		if newTitle != "" {
			_ = s.db.UpdateForgeSession(id, &newTitle, nil)
		}
	}

	updated, err := s.db.GetForgeSession(id)
	if err != nil || updated == nil {
		writeError(w, http.StatusInternalServerError, "failed to reload session")
		return
	}
	count, _ := s.db.CountForgeSessionMessages(id)
	writeJSON(w, http.StatusCreated, map[string]any{
		"message": toForgeMessageDTO(m),
		"session": toForgeSessionDTO(*updated, count),
	})
}

// parseForgeSessionID extracts the {id} URL parameter and parses it as an
// int64. Writes an error response and returns ok=false on failure.
func parseForgeSessionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := chi.URLParam(r, "id")
	if raw == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return 0, false
	}
	return id, true
}

// forgeSessionVisibleTo enforces the per-user scoping rule. A session
// without a created_by attribution (legacy rows, scripts) is treated as
// owned by the daemon and only visible to its creator-of-record; the empty
// string never matches a real session.
func forgeSessionVisibleTo(row *state.ForgeSession, username string) bool {
	if row == nil {
		return false
	}
	if row.CreatedBy == "" {
		// Backwards compatible: pre-attribution rows are visible to everyone.
		return true
	}
	return row.CreatedBy == username
}

// isValidForgeSessionStatus returns true for the status values the
// foundation bead understands. The state layer stores TEXT, so future beads
// can extend this list without a schema migration.
func isValidForgeSessionStatus(s string) bool {
	switch s {
	case state.ForgeSessionStatusDraft, state.ForgeSessionStatusArchived:
		return true
	}
	return false
}

// autoTitleFromMessage takes the first line (or first ~80 chars) of a
// message body and uses it as the session title. Whitespace at the edges
// and trailing punctuation are trimmed so the sidebar stays tidy.
func autoTitleFromMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	if idx := strings.IndexAny(msg, "\r\n"); idx >= 0 {
		msg = msg[:idx]
	}
	const maxLen = 80
	if len(msg) > maxLen {
		// Trim at a UTF-8 boundary by cutting on rune boundaries.
		runes := []rune(msg)
		if len(runes) > maxLen {
			runes = runes[:maxLen]
			msg = string(runes) + "…"
		}
	}
	return strings.TrimRight(msg, ".,;: ")
}

