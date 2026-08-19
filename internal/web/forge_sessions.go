package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
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
	Stage        string `json:"stage"`
	Plan         string `json:"plan,omitempty"`
}

// forgeMessageDTO is the JSON shape for a single message.
type forgeMessageDTO struct {
	ID        int64  `json:"id"`
	SessionID int64  `json:"session_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
	Kind      string `json:"kind,omitempty"`
	Metadata  string `json:"metadata,omitempty"`
}

// toForgeSessionDTO converts a state row into the API DTO. The caller is
// responsible for supplying the message count — most listings already need
// the count, and computing it inline keeps the helper free of *DB.
func toForgeSessionDTO(s state.ForgeSession, messageCount int) forgeSessionDTO {
	stage := s.Stage
	if stage == "" {
		stage = state.ForgeStageDrafting
	}
	return forgeSessionDTO{
		ID:           s.ID,
		Title:        s.Title,
		Status:       s.Status,
		Anvil:        s.Anvil,
		CreatedBy:    s.CreatedBy,
		CreatedAt:    s.CreatedAt.Format(time.RFC3339),
		UpdatedAt:    s.UpdatedAt.Format(time.RFC3339),
		MessageCount: messageCount,
		Stage:        stage,
		Plan:         s.Plan,
	}
}

// toForgeMessageDTO converts a state row into the API DTO.
func toForgeMessageDTO(m state.ForgeSessionMessage) forgeMessageDTO {
	kind := m.Kind
	if kind == "" {
		kind = state.ForgeMessageKindText
	}
	return forgeMessageDTO{
		ID:        m.ID,
		SessionID: m.SessionID,
		Role:      m.Role,
		Content:   m.Content,
		CreatedAt: m.CreatedAt.Format(time.RFC3339),
		Kind:      kind,
		Metadata:  m.Metadata,
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
	rows, err := s.db.ListForgeSessionsWithCounts(sess.Username, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list sessions: "+err.Error())
		return
	}
	out := make([]forgeSessionDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, toForgeSessionDTO(row.ForgeSession, row.MessageCount))
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
	if msg, ok := s.validateSessionAnvil(&req.Anvil); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if len(req.InitialMessage) > maxForgeMessageBytes {
		writeError(w, http.StatusBadRequest, "initial message exceeds size limit")
		return
	}
	req.Title = truncateTitle(req.Title)
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
			// Delete the orphaned session so a retry starts clean.
			_ = s.db.DeleteForgeSession(row.ID)
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

	count, err := s.db.CountForgeSessionMessages(row.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to count messages: "+err.Error())
		return
	}
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
	if req.Title == nil && req.Status == nil {
		writeError(w, http.StatusBadRequest, "request must include title or status")
		return
	}
	if req.Title != nil {
		t := truncateTitle(*req.Title)
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
	count, err := s.db.CountForgeSessionMessages(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to count messages: "+err.Error())
		return
	}
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
	count, err := s.db.CountForgeSessionMessages(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to count messages: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"message": toForgeMessageDTO(m),
		"session": toForgeSessionDTO(*updated, count),
	})
}

// forgeAnvilDTO is the JSON shape returned by handleForgeAnvilsList. The
// SPA only needs the name to render the dropdown; the on-disk path is
// intentionally omitted so the browser cannot leak filesystem layout.
type forgeAnvilDTO struct {
	Name string `json:"name"`
}

// handleForgeAnvilsList serves GET /api/forge/anvils. The Beads-Forge new
// session form fetches this on mount to populate the anvil-select control.
// Names are sorted so the dropdown order is stable across renders.
//
// When the daemon has not wired the anvil lister yet (early startup, tests
// that omit it) the response is an empty list — the same shape the SPA
// renders for the "no anvils registered" empty state.
func (s *Server) handleForgeAnvilsList(w http.ResponseWriter, r *http.Request) {
	sess := SessionFromContext(r.Context())
	if sess == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	out := []forgeAnvilDTO{}
	if s.anvils != nil {
		registry := s.anvils()
		names := make([]string, 0, len(registry))
		for name := range registry {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			out = append(out, forgeAnvilDTO{Name: name})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"anvils": out})
}

// validateSessionAnvil enforces the anvil rules at session-create time so a
// browser bug or stale client can't strand a draft on a non-routable anvil.
// On success it returns ("", true), possibly rewriting *anvil with the
// canonical (registry-cased) name; on failure it returns the error message
// for a 400 response and (..., false). When the registry callback is not
// wired the function is a no-op — the daemon is responsible for wiring
// SetAnvilLister in production, and tests that don't care about anvil
// routing can omit it.
func (s *Server) validateSessionAnvil(anvil *string) (string, bool) {
	if s.anvils == nil {
		return "", true
	}
	registry := s.anvils()
	if len(registry) == 0 {
		if *anvil != "" {
			return fmt.Sprintf("unknown anvil %s; no anvils are registered", *anvil), false
		}
		return "", true
	}
	if *anvil == "" {
		if len(registry) == 1 {
			for name := range registry {
				*anvil = name
			}
			return "", true
		}
		return "anvil is required when more than one anvil is registered", false
	}
	if _, ok := registry[*anvil]; ok {
		return "", true
	}
	// No exact match: do a case-insensitive scan. Multiple matches are
	// ambiguous because map iteration order is unspecified — refuse rather
	// than pick a winner at random. resolveSessionAnvil applies the same
	// rule when reading a stored session.
	var matched string
	matches := 0
	for name := range registry {
		if strings.EqualFold(name, *anvil) {
			matched = name
			matches++
		}
	}
	if matches == 1 {
		*anvil = matched
		return "", true
	}
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	if matches > 1 {
		return fmt.Sprintf("ambiguous anvil %s; multiple registry entries match case-insensitively (registered: %s)", *anvil, strings.Join(names, ", ")), false
	}
	return fmt.Sprintf("unknown anvil %s; registered: %s", *anvil, strings.Join(names, ", ")), false
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

// forgeSessionVisibleTo enforces the per-user scoping rule. Rows with a
// non-empty created_by are only visible to that user. Legacy rows that
// pre-date attribution (created_by == "") fall back to being visible to
// every signed-in user so they remain readable after the schema change.
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

// truncateTitle trims whitespace from the edges of a title and caps it at
// maxForgeTitleLen runes. Truncation happens on rune boundaries so multi-byte
// characters (emoji, non-ASCII text) cannot be sliced in half.
func truncateTitle(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	runes := []rune(title)
	if len(runes) > maxForgeTitleLen {
		runes = runes[:maxForgeTitleLen]
		title = string(runes)
	}
	return title
}

// autoTitleFromMessage takes the first line (or first ~80 runes) of a
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
