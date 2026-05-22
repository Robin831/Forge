package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Robin831/Forge/internal/forgechat"
	"github.com/Robin831/Forge/internal/state"
	"github.com/google/uuid"
)

// Beads-Forge per-turn AI handlers.
//
// The async-scheduling bead (Forge-4mug) converted the /turn endpoint from a
// blocking long-poll into an async dispatcher: validation + user-message
// persistence + stage transitions still run synchronously (so the handler can
// reject bad input with the usual 4xx codes), and the AI runner call moves
// into a background goroutine. The handler returns 202 Accepted with the
// generated turn_id; SSE and polling endpoints (sibling sub-tasks) consume
// the resulting TurnState from the process-local TurnStore.
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

// handleForgeSessionTurn schedules one AI round-trip for a session. The
// handler validates input, persists the user-side mutations (message, answer,
// stage transition), and either returns immediately (for no-AI flows like
// mark_ready) or schedules an AI turn in a background goroutine and returns
// 202 with the generated turn_id.
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

	// Mark-ready is a manual escape hatch: skip claude, just transition. No
	// AI work, no turn_id — return the synchronous 200 the SPA already
	// understands.
	if req.MarkReady {
		updated, statusMsg, err := s.transitionStage(id, state.ForgeStageReady, "Session marked ready by user")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to mark ready: "+err.Error())
			return
		}
		s.respondTurn(w, http.StatusOK, updated, nil, optMsg(statusMsg))
		return
	}

	// Append the user's message (if any) before scheduling the AI turn. We
	// persist the answer metadata when AnswerOptionID is set, regardless of
	// whether Content is non-empty — the user can pick an option without
	// typing.
	var newUserMsg *state.ForgeSessionMessage
	if req.Content != "" || req.AnswerOptionID != "" {
		role := state.ForgeMessageRoleUser
		kind := state.ForgeMessageKindText
		metadata := ""

		if row.Stage == state.ForgeStageGrilling && (req.AnswerOptionID != "" || req.AnswerQuestionID != 0) {
			if req.AnswerOptionID != "" && req.AnswerQuestionID == 0 {
				writeError(w, http.StatusBadRequest, "answer_question_id is required when answer_option_id is set")
				return
			}
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
		updated, _, err := s.transitionStage(id, state.ForgeStageGrilling, "Stage changed to grilling — claude will now interrogate the plan")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to transition stage: "+err.Error())
			return
		}
		row = updated
		stage = updated.Stage
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
		// message (if any) and bail out cleanly via the existing 200 path.
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

	// Build the conversation history fed into the prompt.
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

	turnReq := forgechat.TurnRequest{
		Stage:     forgechat.Stage(stage),
		Mode:      mode,
		Title:     row.Title,
		Plan:      row.Plan,
		History:   history,
		Anvil:     s.resolveSessionAnvil(row.Anvil),
		SessionID: id,
	}

	// Spin up the async turn. The goroutine owns the TurnState's lifecycle:
	// it transitions pending→running→complete|error, persists assistant
	// messages, and closes Events / Done on exit. The 15m backstop (from
	// MaxForgeChatTurnTimeout) is enforced via context.WithTimeout — the
	// ClaudeRunner already imposes its own configurable timeout, this one
	// is the hard cap mandated by the bead.
	turnID := uuid.NewString()
	st := s.turnStore.New(turnID, id)
	go s.runTurnAsync(st, turnReq)

	writeJSON(w, http.StatusAccepted, map[string]any{"turn_id": turnID})
}

// runTurnAsync drives the AI turn in a background goroutine, persists any
// resulting assistant messages, and closes the TurnState channels on exit.
// Errors are recorded on the state (visible via SSE / polling); the goroutine
// never panics out to the runtime.
func (s *Server) runTurnAsync(st *TurnState, req forgechat.TurnRequest) {
	defer close(st.Done)
	defer close(st.Events)

	ctx, cancel := context.WithTimeout(context.Background(), s.turnTimeout)
	defer cancel()

	st.setStatus(TurnStatusRunning)

	if s.chatRunner == nil {
		err := errors.New("AI runner not configured")
		st.SetError(err)
		st.Emit(TurnEvent{Type: TurnEventError, Data: err.Error()})
		return
	}

	turnResp, err := s.chatRunner.Turn(ctx, req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
		st.SetError(err)
		st.Emit(TurnEvent{Type: TurnEventError, Data: err.Error()})
		return
	}
	if turnResp == nil {
		err := errors.New("AI runner returned nil response")
		st.SetError(err)
		st.Emit(TurnEvent{Type: TurnEventError, Data: err.Error()})
		return
	}

	emitted := make([]state.ForgeSessionMessage, 0, len(turnResp.Messages))
	for _, em := range turnResp.Messages {
		role := state.ForgeMessageRoleAssistant
		if em.Kind == state.ForgeMessageKindStatus {
			role = state.ForgeMessageRoleSystem
		}
		m, err := s.db.AppendForgeSessionMessage(state.ForgeSessionMessage{
			SessionID: req.SessionID,
			Role:      role,
			Kind:      em.Kind,
			Content:   em.Content,
			Metadata:  em.Metadata,
		})
		if err != nil {
			st.SetError(err)
			st.Emit(TurnEvent{Type: TurnEventError, Data: err.Error()})
			return
		}
		emitted = append(emitted, m)
		st.AppendText(em.Content)
		st.Emit(TurnEvent{Type: TurnEventTextDelta, Data: em.Content})
		st.Emit(TurnEvent{Type: TurnEventMessage, Data: m})
	}

	// Apply plan + stage transitions emitted by the turn.
	var stagePtr, planPtr *string
	if turnResp.NewStage != "" && string(turnResp.NewStage) != string(req.Stage) {
		stageVal := string(turnResp.NewStage)
		stagePtr = &stageVal
	}
	if turnResp.NewPlan != "" {
		planPtr = &turnResp.NewPlan
	}
	if stagePtr != nil || planPtr != nil {
		if _, err := s.db.UpdateForgeSessionStageAndPlan(req.SessionID, stagePtr, planPtr); err != nil {
			st.SetError(err)
			st.Emit(TurnEvent{Type: TurnEventError, Data: err.Error()})
			return
		}
	}

	if len(emitted) > 0 {
		st.SetFinalMessageID(emitted[len(emitted)-1].ID)
	}
	st.setStatus(TurnStatusComplete)
	st.Emit(TurnEvent{Type: TurnEventComplete, Data: st.FinalMessageID()})
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

// respondTurn writes a unified response shape for the synchronous (no-AI)
// branches of the turn endpoint — mark_ready and the StageReady no-op. The
// async path returns 202 + {turn_id} via the handler directly. The helper
// keeps the JSON contract aligned with the existing list/get endpoints
// (session DTO + message DTOs).
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
