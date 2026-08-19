package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Robin831/Forge/internal/forgechat"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/textfmt"
)

// Beads-Forge bead-emission handlers.
//
// Once a session has reached stage=ready the user clicks "Create Bead(s)"
// and the daemon shells out to claude one last time to produce a structured
// list of beads (title, description, type, priority, depends_on, anvil).
// We then materialise those beads via `bd create` in each anvil's
// directory, wiring sibling deps with `--deps`. The whole emission is
// atomic: if any bd subprocess fails partway, the previously-created beads
// are rolled back via `bd close --reason="rollback: ..."`.

// createBeadsResponse is the JSON returned by POST /create-beads. The
// frontend renders the assistant message directly, but also displays a
// toast/banner with the created bead IDs so the user has a clickable trail.
type createBeadsResponse struct {
	Session  forgeSessionDTO   `json:"session"`
	Messages []forgeMessageDTO `json:"messages"`
	Beads    []createdBeadDTO  `json:"beads"`
	// Summary is claude's optional one-liner describing the proposed split.
	Summary string `json:"summary,omitempty"`
}

// createdBeadDTO mirrors forgechat.MaterializedBead minus the on-disk
// AnvilPath, which is internal-only.
type createdBeadDTO struct {
	BeadID string `json:"bead_id"`
	Anvil  string `json:"anvil"`
	Title  string `json:"title"`
}

// handleForgeSessionCreateBeads serves POST /api/forge/sessions/{id}/create-beads.
// The handler is a single round-trip: it runs claude in ModeEmit, validates
// the envelope, materialises the beads via bd, then persists a system
// status message + a structured assistant message recording the emission.
func (s *Server) handleForgeSessionCreateBeads(w http.ResponseWriter, r *http.Request) {
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
	if row.Stage != state.ForgeStageReady {
		writeError(w, http.StatusBadRequest, "session must be in stage=ready before emitting beads")
		return
	}
	if strings.TrimSpace(row.Plan) == "" {
		writeError(w, http.StatusBadRequest, "session has no plan to emit beads from")
		return
	}
	// Drain and discard the request body so the connection can be reused
	// (we take no parameters today, so any payload is ignored).
	_, _ = io.ReadAll(io.LimitReader(r.Body, 8*1024))

	if s.chatRunner == nil {
		writeError(w, http.StatusServiceUnavailable, "AI runner not configured on this daemon")
		return
	}
	if s.anvils == nil {
		writeError(w, http.StatusServiceUnavailable, "anvil registry not configured on this daemon")
		return
	}
	anvils := s.anvils()
	if len(anvils) == 0 {
		writeError(w, http.StatusBadRequest, "no anvils registered — add at least one before emitting beads")
		return
	}

	history, err := s.db.ListForgeSessionMessages(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load history: "+err.Error())
		return
	}
	hist := make([]forgechat.HistoryMessage, 0, len(history))
	for _, m := range history {
		hist = append(hist, forgechat.HistoryMessage{
			Role:     m.Role,
			Kind:     m.Kind,
			Content:  m.Content,
			Metadata: m.Metadata,
		})
	}

	// Rely on ClaudeRunner's own timeout (settings.forgechat.turn_timeout)
	// rather than a hardcoded handler deadline that would shadow it.
	turnReq := forgechat.TurnRequest{
		Stage:     forgechat.StageReady,
		Mode:      forgechat.ModeEmit,
		Title:     row.Title,
		Plan:      row.Plan,
		Anvils:    forgechat.AnvilContext(toHints(anvils)),
		History:   hist,
		SessionID: id,
	}
	turnResp, err := s.chatRunner.Turn(r.Context(), turnReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "AI emission failed: "+err.Error())
		return
	}
	if turnResp == nil || turnResp.Emission == nil {
		writeError(w, http.StatusBadGateway, "AI did not return a parseable bead list")
		return
	}

	// Validate before touching bd. The handler refuses to create anything
	// when the envelope has a cycle, missing fields, unknown anvils, or
	// cross-anvil deps — listing every problem at once helps the user fix
	// the prompt rather than playing whack-a-mole.
	known := make(map[string]bool, len(anvils))
	for name := range anvils {
		known[name] = true
	}
	if problems := forgechat.ValidateEmission(turnResp.Emission, known); len(problems) > 0 {
		writeError(w, http.StatusUnprocessableEntity, "emission failed validation:\n  - "+strings.Join(problems, "\n  - "))
		return
	}

	lookup := func(name string) (string, bool) {
		path, ok := anvils[name]
		return path, ok
	}
	runner := s.bdRunner
	if runner == nil {
		runner = forgechat.DefaultBdRunner
	}

	// Use the parent request context for materialisation so a slow client
	// disconnect can still abort partway, prompting rollback.
	mres := forgechat.MaterializeEmission(r.Context(), s.logger, turnResp.Emission, lookup, runner)
	if mres.Err != nil {
		// Persist a status message with the failure + rollback summary so
		// the conversation has a record. Then surface the original error
		// to the client. We deliberately do NOT bump session.stage on a
		// failure — the user can retry without reconfiguring anything.
		statusMsg := buildEmissionFailureStatus(mres)
		_, _ = s.db.AppendForgeSessionMessage(state.ForgeSessionMessage{
			SessionID: id,
			Role:      state.ForgeMessageRoleSystem,
			Kind:      state.ForgeMessageKindStatus,
			Content:   statusMsg,
		})
		writeError(w, http.StatusBadGateway, mres.Err.Error())
		return
	}

	// Success: append a structured assistant message and a status note.
	persisted, err := s.persistEmissionSuccess(id, turnResp.Emission, mres)
	if err != nil {
		// The beads are already created — we can't atomically undo them
		// just because the chat-history append failed. Log loudly and
		// surface the bead list in the response anyway so the user has
		// the IDs even if the conversation didn't get the receipt.
		s.logger.Error("forgechat: failed to persist emission outcome",
			"session_id", id,
			"error", err,
			"created", len(mres.Created),
		)
	}

	final, err := s.db.GetForgeSession(id)
	if err != nil || final == nil {
		writeError(w, http.StatusInternalServerError, "failed to reload session after emission")
		return
	}
	count, _ := s.db.CountForgeSessionMessages(final.ID)

	dtoMsgs := make([]forgeMessageDTO, 0, len(persisted))
	for _, m := range persisted {
		dtoMsgs = append(dtoMsgs, toForgeMessageDTO(m))
	}
	dtoBeads := make([]createdBeadDTO, 0, len(mres.Created))
	for _, b := range mres.Created {
		dtoBeads = append(dtoBeads, createdBeadDTO{
			BeadID: b.BeadID,
			Anvil:  b.Anvil,
			Title:  b.Title,
		})
	}
	writeJSON(w, http.StatusOK, createBeadsResponse{
		Session:  toForgeSessionDTO(*final, count),
		Messages: dtoMsgs,
		Beads:    dtoBeads,
		Summary:  turnResp.Emission.Summary,
	})
}

// persistEmissionSuccess appends two messages to the session: a structured
// "beads_created" assistant message holding the JSON list (so the UI can
// render clickable links + future replay), and a friendly system status
// note summarising the emission for chat readers.
func (s *Server) persistEmissionSuccess(
	sessionID int64,
	env *forgechat.EmissionEnvelope,
	mres forgechat.MaterializeResult,
) ([]state.ForgeSessionMessage, error) {
	type beadsCreatedPayload struct {
		Summary string           `json:"summary,omitempty"`
		Beads   []createdBeadDTO `json:"beads"`
	}
	beads := make([]createdBeadDTO, 0, len(mres.Created))
	for _, b := range mres.Created {
		beads = append(beads, createdBeadDTO{BeadID: b.BeadID, Anvil: b.Anvil, Title: b.Title})
	}
	body, err := json.Marshal(beadsCreatedPayload{
		Summary: env.Summary,
		Beads:   beads,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal beads_created metadata: %w", err)
	}

	human := buildEmissionSuccessMessage(env.Summary, mres)
	assistantMsg, err := s.db.AppendForgeSessionMessage(state.ForgeSessionMessage{
		SessionID: sessionID,
		Role:      state.ForgeMessageRoleAssistant,
		Kind:      state.ForgeMessageKindBeadsCreated,
		Content:   human,
		Metadata:  string(body),
	})
	if err != nil {
		return nil, fmt.Errorf("append beads_created message: %w", err)
	}

	statusContent := fmt.Sprintf("Created %d bead%s from this session.", len(mres.Created), textfmt.Suffix(len(mres.Created)))
	statusMsg, err := s.db.AppendForgeSessionMessage(state.ForgeSessionMessage{
		SessionID: sessionID,
		Role:      state.ForgeMessageRoleSystem,
		Kind:      state.ForgeMessageKindStatus,
		Content:   statusContent,
	})
	if err != nil {
		return []state.ForgeSessionMessage{assistantMsg}, fmt.Errorf("append status message: %w", err)
	}
	return []state.ForgeSessionMessage{assistantMsg, statusMsg}, nil
}

// buildEmissionSuccessMessage formats a chat-friendly recap of the created
// beads. The structured payload is in the message metadata so the UI can
// render clickable bead pills; this is the fallback prose for clients that
// can't parse it.
func buildEmissionSuccessMessage(summary string, mres forgechat.MaterializeResult) string {
	var b strings.Builder
	if s := strings.TrimSpace(summary); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	b.WriteString(fmt.Sprintf("Created %d bead%s:\n", len(mres.Created), textfmt.Suffix(len(mres.Created))))
	for _, mb := range mres.Created {
		fmt.Fprintf(&b, "- %s (%s) — %s\n", mb.BeadID, mb.Anvil, mb.Title)
	}
	return strings.TrimRight(b.String(), "\n")
}

// buildEmissionFailureStatus formats the system status note appended after
// a partial-failure rollback. We name the original error and the rollback
// outcome separately so operators can tell whether bd is in a clean state.
func buildEmissionFailureStatus(mres forgechat.MaterializeResult) string {
	var b strings.Builder
	b.WriteString("Bead emission failed.")
	if mres.Err != nil {
		fmt.Fprintf(&b, "\n\nError: %s", mres.Err.Error())
	}
	if mres.RolledBack {
		if len(mres.Created) == 0 {
			b.WriteString("\n\nNothing was created — no rollback needed.")
		} else {
			fmt.Fprintf(&b, "\n\nRolled back %d previously-created bead%s.", len(mres.Created), textfmt.Suffix(len(mres.Created)))
			if mres.RollbackError != nil {
				fmt.Fprintf(&b, " Rollback was incomplete: %s", mres.RollbackError.Error())
			}
		}
	}
	return b.String()
}

// toHints converts the anvils map to the AnvilContext shape expected by the
// forgechat prompt builder. We pass paths through as the hint so claude can
// distinguish anvils with similar names (rare, but cheap insurance).
func toHints(anvils map[string]string) map[string]string {
	out := make(map[string]string, len(anvils))
	for name, path := range anvils {
		out[name] = path
	}
	return out
}
