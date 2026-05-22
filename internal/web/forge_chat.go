package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Robin831/Forge/internal/forgechat"
	"github.com/Robin831/Forge/internal/state"
)

// Beads-Forge per-turn AI handlers.
//
// The foundation bead in Forge-qcqv only persisted user messages. This file
// adds the actual claude integration: each turn drives a forgechat.Runner,
// then writes the resulting assistant messages and any stage / plan
// transitions to the DB inside a single best-effort flow.
//
// Stages drive the prompts and the UI affordances:
//   - drafting: open conversation; user can request a fresh plan.
//   - grilling: claude emits structured questions; user picks an option or
//     writes a free-form answer.
//   - ready:   grilling exhausted, plan + answers are settled. Emitting
//              actual beads from the session is the next bead's concern.

// turnRequest is the JSON body for POST /api/forge/sessions/{id}/turn.
// All fields are optional and combinable: Content appends a user message,
// AnswerOptionID + AnswerQuestionID record a structured answer, and the
// flag fields trigger stage transitions / plan requests.
type turnRequest struct {
	Content          string `json:"content,omitempty"`
	AnswerOptionID   string `json:"answer_option_id,omitempty"`
	AnswerQuestionID int64  `json:"answer_question_id,omitempty"`
	RequestPlan      bool   `json:"request_plan,omitempty"`
	StartGrilling    bool   `json:"start_grilling,omitempty"`
	MarkReady        bool   `json:"mark_ready,omitempty"`
}

// handleForgeSessionTurn drives one AI round-trip for a session. The handler
// is intentionally a single chunky endpoint rather than several narrow ones
// so the SPA can compose user actions ("answer this question and ask for
// the next batch") in one HTTP call.
func (s *Server) handleForgeSessionTurn(w http.ResponseWriter, r *http.Request) {
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
	var req turnRequest
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	req.Content = strings.TrimSpace(req.Content)
	if len(req.Content) > maxForgeMessageBytes {
		writeError(w, http.StatusBadRequest, "content exceeds size limit")
		return
	}

	// Mark-ready is a manual escape hatch: skip claude, just transition.
	if req.MarkReady {
		updated, statusMsg, err := s.transitionStage(id, state.ForgeStageReady, "Session marked ready by user")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to mark ready: "+err.Error())
			return
		}
		s.respondTurn(w, http.StatusOK, updated, nil, optMsg(statusMsg))
		return
	}

	// Append the user's message (if any) before invoking claude. We persist
	// the answer metadata when AnswerOptionID is set, regardless of whether
	// Content is non-empty — the user can pick an option without typing.
	var newUserMsg *state.ForgeSessionMessage
	if req.Content != "" || req.AnswerOptionID != "" {
		role := state.ForgeMessageRoleUser
		kind := state.ForgeMessageKindText
		metadata := ""

		if row.Stage == state.ForgeStageGrilling && (req.AnswerOptionID != "" || req.AnswerQuestionID != 0) {
			// Require answer_question_id whenever answer_option_id is provided.
			if req.AnswerOptionID != "" && req.AnswerQuestionID == 0 {
				writeError(w, http.StatusBadRequest, "answer_question_id is required when answer_option_id is set")
				return
			}
			// Verify the referenced question belongs to this session and is kind=question.
			if req.AnswerQuestionID != 0 && !s.sessionHasQuestion(id, req.AnswerQuestionID) {
				writeError(w, http.StatusBadRequest, "answer_question_id not found in this session")
				return
			}
			kind = state.ForgeMessageKindAnswer
			payload := forgechat.AnswerPayload{
				QuestionID: req.AnswerQuestionID,
				OptionID:   req.AnswerOptionID,
			}
			if md, mderr := json.Marshal(payload); mderr == nil {
				metadata = string(md)
			}
		}

		content := req.Content
		// When an option is picked without free-form text, use the option
		// label as the persisted content so the chat reads naturally.
		if content == "" && req.AnswerOptionID != "" && req.AnswerQuestionID != 0 {
			if label, ok := s.optionLabel(id, req.AnswerQuestionID, req.AnswerOptionID); ok {
				content = label
			}
		}
		if content == "" {
			writeError(w, http.StatusBadRequest, "content or answer_option_id is required")
			return
		}

		m, err := s.db.AppendForgeSessionMessage(state.ForgeSessionMessage{
			SessionID: id,
			Role:      role,
			Kind:      kind,
			Content:   content,
			Metadata:  metadata,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to append message: "+err.Error())
			return
		}
		newUserMsg = &m
	}

	// Stage transition: starting grilling requires a plan to grill against.
	stage := row.Stage
	var transitionMsg *state.ForgeSessionMessage
	if req.StartGrilling {
		reload, err := s.db.GetForgeSession(id)
		if err != nil || reload == nil {
			writeError(w, http.StatusInternalServerError, "failed to reload session")
			return
		}
		if strings.TrimSpace(reload.Plan) == "" {
			writeError(w, http.StatusBadRequest, "request a plan before starting the grilling stage")
			return
		}
		updated, msg, err := s.transitionStage(id, state.ForgeStageGrilling, "Stage changed to grilling — claude will now interrogate the plan")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to transition stage: "+err.Error())
			return
		}
		row = updated
		stage = updated.Stage
		transitionMsg = msg
	}

	// Decide the mode for the AI turn from the (possibly transitioned) stage.
	mode := forgechat.ModeChat
	switch stage {
	case state.ForgeStageDrafting:
		if req.RequestPlan {
			mode = forgechat.ModePlan
		}
	case state.ForgeStageGrilling:
		mode = forgechat.ModeGrill
	case state.ForgeStageReady:
		// No AI turn for a settled session. Surface the user's appended
		// message (if any) and bail out cleanly.
		fresh, _ := s.db.GetForgeSession(id)
		s.respondTurn(w, http.StatusOK, fresh, nil, optMsg(newUserMsg))
		return
	}

	if s.chatRunner == nil {
		// User message is already persisted; the SPA will see it on the
		// next refresh. Surface 503 so callers know AI is unavailable.
		writeError(w, http.StatusServiceUnavailable, "AI runner not configured on this daemon")
		return
	}

	// Build the conversation history fed into the prompt. Convert state
	// messages into forgechat.HistoryMessage; include the current plan
	// once via the dedicated TurnRequest field.
	msgs, err := s.db.ListForgeSessionMessages(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load history: "+err.Error())
		return
	}
	history := make([]forgechat.HistoryMessage, 0, len(msgs))
	for _, m := range msgs {
		history = append(history, forgechat.HistoryMessage{
			Role:     m.Role,
			Kind:     m.Kind,
			Content:  m.Content,
			Metadata: m.Metadata,
		})
	}

	// Run the AI turn. Rely on ClaudeRunner's own timeout (configured via
	// settings.forgechat.turn_timeout) rather than imposing a separate
	// handler-level deadline that would shadow the operator's setting.
	turnReq := forgechat.TurnRequest{
		Stage:     forgechat.Stage(stage),
		Mode:      mode,
		Title:     row.Title,
		Plan:      row.Plan,
		History:   history,
		Anvil:     s.resolveSessionAnvil(row.Anvil),
		SessionID: id,
	}
	turnResp, err := s.chatRunner.Turn(r.Context(), turnReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "AI turn failed: "+err.Error())
		return
	}

	// Persist assistant messages.
	emitted := make([]state.ForgeSessionMessage, 0, len(turnResp.Messages))
	for _, em := range turnResp.Messages {
		role := state.ForgeMessageRoleAssistant
		if em.Kind == state.ForgeMessageKindStatus {
			role = state.ForgeMessageRoleSystem
		}
		m, err := s.db.AppendForgeSessionMessage(state.ForgeSessionMessage{
			SessionID: id,
			Role:      role,
			Kind:      em.Kind,
			Content:   em.Content,
			Metadata:  em.Metadata,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to persist assistant message: "+err.Error())
			return
		}
		emitted = append(emitted, m)
	}

	// Apply plan + stage transitions emitted by the turn.
	var stagePtr, planPtr *string
	if turnResp.NewStage != "" && string(turnResp.NewStage) != stage {
		stageVal := string(turnResp.NewStage)
		stagePtr = &stageVal
	}
	if turnResp.NewPlan != "" {
		planPtr = &turnResp.NewPlan
	}
	if stagePtr != nil || planPtr != nil {
		if _, err := s.db.UpdateForgeSessionStageAndPlan(id, stagePtr, planPtr); err != nil {
			if errors.Is(err, state.ErrForgeSessionNotFound) {
				writeError(w, http.StatusNotFound, "session not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "failed to update session: "+err.Error())
			return
		}
	}

	final, err := s.db.GetForgeSession(id)
	if err != nil || final == nil {
		writeError(w, http.StatusInternalServerError, "failed to reload session")
		return
	}

	// Compose the response: user message → stage-transition status (if any) →
	// all assistant messages, ordered to match their DB insertion order.
	out := []state.ForgeSessionMessage{}
	if newUserMsg != nil {
		out = append(out, *newUserMsg)
	}
	if transitionMsg != nil {
		out = append(out, *transitionMsg)
	}
	out = append(out, emitted...)
	s.respondTurn(w, http.StatusOK, final, nil, out)
}

// optMsg returns a slice with the single user message, or nil when m is nil.
func optMsg(m *state.ForgeSessionMessage) []state.ForgeSessionMessage {
	if m == nil {
		return nil
	}
	return []state.ForgeSessionMessage{*m}
}

// resolveSessionAnvil maps a session's anvil name to its registered absolute
// path so the drafting / grilling prompt can tell the AI where the code
// lives. Returns nil when the registry isn't wired (no daemon-side anvils
// callback), when the session has no anvil association, or when the named
// anvil is no longer registered — in those cases we'd rather emit no anvil
// context than feed claude a wrong or stale path.
//
// Name resolution is case-insensitive to match the daemon's anvil routing
// (e.g. a session created with "Munin" still resolves to the configured
// "munin" key), and the canonical key + path are returned so the prompt
// renders the names the daemon actually uses.
func (s *Server) resolveSessionAnvil(name string) *forgechat.AnvilTarget {
	if s.anvils == nil {
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	registry := s.anvils()
	if len(registry) == 0 {
		return nil
	}
	if path, ok := registry[name]; ok && strings.TrimSpace(path) != "" {
		return &forgechat.AnvilTarget{Name: name, Path: path}
	}
	lower := strings.ToLower(name)
	var matched *forgechat.AnvilTarget
	for k, path := range registry {
		if strings.ToLower(k) == lower && strings.TrimSpace(path) != "" {
			if matched != nil {
				// Multiple keys differ only by case — ambiguous, return nil to
				// avoid feeding claude a nondeterministically chosen path.
				return nil
			}
			matched = &forgechat.AnvilTarget{Name: k, Path: path}
		}
	}
	return matched
}

// respondTurn writes a unified response shape for the turn endpoint. The
// helper keeps the JSON contract aligned with the existing list/get
// endpoints (session DTO + message DTOs).
func (s *Server) respondTurn(w http.ResponseWriter, status int, sess *state.ForgeSession, _ error, msgs []state.ForgeSessionMessage) {
	if sess == nil {
		writeError(w, http.StatusInternalServerError, "missing session in response")
		return
	}
	count, _ := s.db.CountForgeSessionMessages(sess.ID)
	dtoMsgs := make([]forgeMessageDTO, 0, len(msgs))
	for _, m := range msgs {
		dtoMsgs = append(dtoMsgs, toForgeMessageDTO(m))
	}
	writeJSON(w, status, map[string]any{
		"session":  toForgeSessionDTO(*sess, count),
		"messages": dtoMsgs,
	})
}

// optionLabel resolves a (sessionID, questionID, optionID) triple to the
// human-readable label stored in the question's metadata. The query is scoped
// to sessionID so a client cannot reference a question from another session.
// Returns ok=false when the question doesn't exist or the option id is unknown.
func (s *Server) optionLabel(sessionID, questionID int64, optionID string) (string, bool) {
	if sessionID == 0 || questionID == 0 || optionID == "" {
		return "", false
	}
	conn := s.db.Conn()
	row := conn.QueryRow(
		`SELECT metadata FROM forge_session_messages WHERE id = ? AND session_id = ? AND kind = ?`,
		questionID, sessionID, state.ForgeMessageKindQuestion,
	)
	var metadata string
	if err := row.Scan(&metadata); err != nil {
		return "", false
	}
	if metadata == "" {
		return "", false
	}
	var payload forgechat.QuestionPayload
	if err := json.Unmarshal([]byte(metadata), &payload); err != nil {
		return "", false
	}
	for _, opt := range payload.Options {
		if opt.ID == optionID {
			return opt.Label, true
		}
	}
	return "", false
}

// sessionHasQuestion reports whether the message with the given ID exists in
// this session and has kind=question. Used to validate answer_question_id
// before persisting an answer to prevent referencing questions from other
// sessions or messages that are not questions.
func (s *Server) sessionHasQuestion(sessionID, questionID int64) bool {
	if sessionID == 0 || questionID == 0 {
		return false
	}
	conn := s.db.Conn()
	row := conn.QueryRow(
		`SELECT 1 FROM forge_session_messages WHERE id = ? AND session_id = ? AND kind = ?`,
		questionID, sessionID, state.ForgeMessageKindQuestion,
	)
	var dummy int
	return row.Scan(&dummy) == nil
}

// transitionStage updates the session's stage, appends a system status
// message describing the transition, and returns the refreshed row plus the
// persisted status message. The status message is what the SPA renders as the
// in-line "Stage changed to X" note in the chat view.
// Returns (updatedSession, statusMsg, error). statusMsg is nil when the
// stage was already the target (no-op) or when statusContent is empty.
func (s *Server) transitionStage(id int64, newStage, statusContent string) (*state.ForgeSession, *state.ForgeSessionMessage, error) {
	row, err := s.db.GetForgeSession(id)
	if err != nil {
		return nil, nil, err
	}
	if row == nil {
		return nil, nil, errors.New("session not found")
	}
	if row.Stage == newStage {
		return row, nil, nil
	}
	stageStr := newStage
	updated, err := s.db.UpdateForgeSessionStageAndPlan(id, &stageStr, nil)
	if err != nil {
		return nil, nil, err
	}
	var statusMsg *state.ForgeSessionMessage
	if statusContent != "" {
		m, merr := s.db.AppendForgeSessionMessage(state.ForgeSessionMessage{
			SessionID: id,
			Role:      state.ForgeMessageRoleSystem,
			Kind:      state.ForgeMessageKindStatus,
			Content:   statusContent,
		})
		if merr == nil {
			statusMsg = &m
		}
	}
	if updated == nil {
		updated, err = s.db.GetForgeSession(id)
		if err != nil {
			return nil, nil, err
		}
	}
	return updated, statusMsg, nil
}
