package web

import (
	"net/http"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// Escalation-type discriminators returned in the needs-attention list and
// understood by the SPA's ResolveNeedsAttentionPanel. They drive which
// resolve actions the panel renders for each row. Kept as plain strings
// (rather than reusing state.EventType) because the wire contract is
// independent of the internal event names that happen to back them today.
const (
	escTypeStrandedBranch = "dispatch_blocked_stranded_branch"
	escTypeSmithFailed    = "smith_failed"
	escTypeRecoveryFailed = "recovery_failed"
	escTypeDispatchFailed = "dispatch_failed"
	escTypeClarification  = "clarification"
	// escTypePRCreateFailed marks a bead whose branch was pushed but for which
	// PR creation failed (Part D's pr_create_failed recorded state). It drives
	// the panel's "Create PR" action set, which recovers the bead by opening a
	// PR for the existing branch without re-running Smith.
	escTypePRCreateFailed = "pr_create_failed"
)

// needsAttentionItem is one row of GET /api/forge/needs-attention. It bundles
// the authoritative needs-human / clarification state from the retries table
// with enough context for the operator to triage without a shell:
//   - escalation_type drives the panel's action set (open-PR vs retry vs
//     clarify, …) — derived from the bead's most recent lifecycle event.
//   - last_error is the full, untruncated escalation message.
//   - worker_row_exists tells the UI whether a worker row still exists, so it
//     can explain why a bead is not also surfaced on a failed worker row
//     (graceful escalations exit status=done; stale ones aged out).
type needsAttentionItem struct {
	BeadID              string `json:"bead_id"`
	Anvil               string `json:"anvil"`
	Title               string `json:"title,omitempty"`
	EscalationType      string `json:"escalation_type"`
	NeedsHuman          bool   `json:"needs_human"`
	ClarificationNeeded bool   `json:"clarification_needed"`
	DispatchFailures    int    `json:"dispatch_failures"`
	RecoveryFailures    int    `json:"recovery_failures"`
	LastError           string `json:"last_error,omitempty"`
	UpdatedAt           string `json:"updated_at,omitempty"`
	WorkerRowExists     bool   `json:"worker_row_exists"`
}

// needsAttentionResponse is the JSON shape served by
// GET /api/forge/needs-attention. Items is always a non-nil slice so the SPA
// can iterate without a null check.
type needsAttentionResponse struct {
	Items []needsAttentionItem `json:"items"`
}

// handleForgeNeedsAttention returns every bead currently flagged needs_human
// or clarification_needed across all anvils, decoupled from the workers
// table. This is the bead-centric surface that closes the gap left by the
// worker-row-anchored resolve affordance (Forge-iz6s): graceful Smith
// escalations (worker exited status=done) and stale escalations (worker aged
// out of the live set) are invisible to a worker-row-driven UI but appear
// here because the list is driven entirely by the retries table.
//
// Reads are inherently scoped to this forge's state.db, so the list only ever
// contains this forge's escalations; the resolve POST (handleForgeResolve)
// applies the per-forge_id ownership check before any mutating action.
func (s *Server) handleForgeNeedsAttention(w http.ResponseWriter, r *http.Request) {
	resp := needsAttentionResponse{Items: []needsAttentionItem{}}

	// Authoritative state: needs_human rows plus the clarification class.
	// NeedsHumanBeads and ClarificationNeededBeads are disjoint
	// (ClarificationNeededBeads excludes needs_human=1), so concatenation
	// cannot double-list a bead.
	var records []state.RetryRecord
	if recs, err := s.db.NeedsHumanBeads(); err == nil {
		records = append(records, recs...)
	} else {
		writeError(w, http.StatusInternalServerError, "failed to load needs-human beads: "+err.Error())
		return
	}
	if recs, err := s.db.ClarificationNeededBeads(); err == nil {
		records = append(records, recs...)
	} else {
		writeError(w, http.StatusInternalServerError, "failed to load clarification-needed beads: "+err.Error())
		return
	}

	// Title lookup from the queue cache (the same source the dashboard uses).
	// Built once so we don't re-scan the cache per row. Keyed on bead+anvil
	// because the same bead id can legitimately exist in more than one anvil.
	titles := map[string]string{}
	if items, err := s.db.QueueCache(); err == nil {
		for _, it := range items {
			titles[it.BeadID+"\x00"+it.Anvil] = it.Title
		}
	}

	for _, rec := range records {
		item := needsAttentionItem{
			BeadID:              rec.BeadID,
			Anvil:               rec.Anvil,
			Title:               titles[rec.BeadID+"\x00"+rec.Anvil],
			EscalationType:      s.deriveEscalationType(rec),
			NeedsHuman:          rec.NeedsHuman,
			ClarificationNeeded: rec.ClarificationNeeded,
			DispatchFailures:    rec.DispatchFailures,
			RecoveryFailures:    rec.RecoveryFailures,
			LastError:           rec.LastError,
		}
		if !rec.UpdatedAt.IsZero() {
			item.UpdatedAt = rec.UpdatedAt.Format(time.RFC3339)
		}
		// Whether a worker row still exists for this bead. Cheap per-row
		// lookup (LIMIT 1) — the needs-attention set is small in practice.
		if workers, err := s.db.WorkersByBead(rec.BeadID, rec.Anvil, 1); err == nil {
			item.WorkerRowExists = len(workers) > 0
		}
		resp.Items = append(resp.Items, item)
	}

	writeJSON(w, http.StatusOK, resp)
}

// deriveEscalationType classifies a needs-attention bead so the SPA can render
// the right resolve action set. The bead's most recent relevant lifecycle
// event wins (a bead that was first dispatch-blocked and later smith-failed
// reflects its latest state); when no classifying event is on record we fall
// back to the retry-row flags. Reads are scoped to the bead's owning anvil so
// a same-id bead in another anvil cannot leak its event history in.
func (s *Server) deriveEscalationType(rec state.RetryRecord) string {
	if events, err := s.db.EventsByBead(rec.BeadID, rec.Anvil, 50); err == nil {
		for _, e := range events {
			switch e.Type {
			case state.EventDispatchBlockedStrandedBranch:
				return escTypeStrandedBranch
			case state.EventPRCreationFailed:
				// PR creation failed after the branch was pushed (Part D records
				// the pr_create_failed state alongside this event). The work is
				// committed; only the final PR open failed, so the operator can
				// recover with "Create PR".
				return escTypePRCreateFailed
			case state.EventSmithFailed:
				return escTypeSmithFailed
			case state.EventRecoveryCircuitBreak:
				return escTypeRecoveryFailed
			case state.EventDispatchFailed, state.EventDispatchCircuitBreak:
				return escTypeDispatchFailed
			case state.EventClarificationNeeded:
				return escTypeClarification
			}
		}
	}

	// No classifying event found — infer from the retry-row flags. A pure
	// clarification flag (no needs_human) maps to the clarify class; a bead
	// that only ever failed to dispatch (no recovery failures) maps to
	// dispatch_failed; everything else defaults to smith_failed, the broadest
	// action set.
	if rec.ClarificationNeeded && !rec.NeedsHuman {
		return escTypeClarification
	}
	if rec.DispatchFailures > 0 && rec.RecoveryFailures == 0 {
		return escTypeDispatchFailed
	}
	return escTypeSmithFailed
}
