package web

import (
	"net/http"
	"strconv"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
	"github.com/go-chi/chi/v5"
)

// prActionContext bundles the data the PR action handlers need: the PR row
// from state.db plus the parsed numeric ID. The state lookup is done once per
// request so each action only needs to dispatch the IPC payload.
type prActionContext struct {
	prID int
	pr   *state.PR
}

// requirePR validates the URL path's {id} parameter and loads the PR row.
// Writes the error response and returns ok=false on missing/invalid ID or
// when the PR is not found in state.db. The caller can rely on ctx.pr being
// non-nil whenever ok is true.
func (s *Server) requirePR(w http.ResponseWriter, r *http.Request) (prActionContext, bool) {
	raw := chi.URLParam(r, "id")
	id, err := strconv.Atoi(raw)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid pr id")
		return prActionContext{}, false
	}
	pr, err := s.db.GetPRByID(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load PR: "+err.Error())
		return prActionContext{}, false
	}
	if pr == nil {
		writeError(w, http.StatusNotFound, "PR not found")
		return prActionContext{}, false
	}
	return prActionContext{prID: id, pr: pr}, true
}

// dispatchPRAction builds a pr_action IPC payload for the given PR and
// dispatches it through the daemon's in-process handler. The action string
// matches the daemon's pr_action switch (e.g. "merge", "quench", "burnish",
// "rebase", "close", "assign_bellows", "approve").
func (s *Server) dispatchPRAction(w http.ResponseWriter, r *http.Request, action string) {
	ctx, ok := s.requirePR(w, r)
	if !ok {
		return
	}
	s.logActor(r, "pr_action", "pr", ctx.pr.Number, "anvil", ctx.pr.Anvil, "action", action)
	s.dispatchAction(w, "pr_action", ipc.PRActionPayload{
		PRID:     ctx.prID,
		PRNumber: ctx.pr.Number,
		Anvil:    ctx.pr.Anvil,
		BeadID:   ctx.pr.BeadID,
		Branch:   ctx.pr.Branch,
		Action:   action,
	})
}

// handlePRMerge handles POST /api/prs/{id}/merge — squash-merges (or uses the
// configured strategy on) a PR via the daemon's pr_action(merge) handler,
// which calls vcs.Provider.MergePR (gh pr merge under the hood).
func (s *Server) handlePRMerge(w http.ResponseWriter, r *http.Request) {
	s.dispatchPRAction(w, r, "merge")
}

// handlePRClose handles POST /api/prs/{id}/close — closes the PR via gh pr
// close. Works for both Forge-managed and external PRs.
func (s *Server) handlePRClose(w http.ResponseWriter, r *http.Request) {
	s.dispatchPRAction(w, r, "close")
}

// handlePRApprove handles POST /api/prs/{id}/approve — approves the PR via
// gh pr review --approve. The daemon shells out so external PRs work without
// going through the bellows lifecycle pipeline.
func (s *Server) handlePRApprove(w http.ResponseWriter, r *http.Request) {
	s.dispatchPRAction(w, r, "approve")
}

// handlePRBellows handles POST /api/prs/{id}/bellows — manually assigns this
// Forge instance to manage the PR's lifecycle (CI fix, review fix, rebase,
// merge). This is intentionally manual-only: per Forge-i1g7, the
// `<!-- forge-managed: <instance> -->` body marker scopes auto-adoption to
// the instance that created the PR, so taking over a sibling instance's PR
// requires an explicit user action via this endpoint.
//
// NOTE: this action only has a durable effect on PRs that Forge created (i.e.
// rows with a real bead ID, not a synthetic ext-* one). The daemon's reconcile
// loop (reevaluateTrackedPR) unconditionally clears bellows_managed on ext-*
// rows on every cycle, so the assignment would be immediately reverted for
// truly external (hand-rolled) PRs.
func (s *Server) handlePRBellows(w http.ResponseWriter, r *http.Request) {
	s.dispatchPRAction(w, r, "assign_bellows")
}

// handlePRFixCI handles POST /api/prs/{id}/fix-ci — kicks off a quench
// (CI failure fix) worker against the PR's branch.
func (s *Server) handlePRFixCI(w http.ResponseWriter, r *http.Request) {
	ctx, ok := s.requirePR(w, r)
	if !ok {
		return
	}
	if ctx.pr.Branch == "" {
		writeError(w, http.StatusBadRequest, "PR has no branch on record; cannot run CI fix")
		return
	}
	s.logActor(r, "pr_action", "pr", ctx.pr.Number, "anvil", ctx.pr.Anvil, "action", "quench")
	s.dispatchAction(w, "pr_action", ipc.PRActionPayload{
		PRID:     ctx.prID,
		PRNumber: ctx.pr.Number,
		Anvil:    ctx.pr.Anvil,
		BeadID:   ctx.pr.BeadID,
		Branch:   ctx.pr.Branch,
		Action:   "quench",
	})
}

// handlePRFixComments handles POST /api/prs/{id}/fix-comments — kicks off a
// burnish (review comment fix) worker against the PR's branch.
func (s *Server) handlePRFixComments(w http.ResponseWriter, r *http.Request) {
	ctx, ok := s.requirePR(w, r)
	if !ok {
		return
	}
	if ctx.pr.Branch == "" {
		writeError(w, http.StatusBadRequest, "PR has no branch on record; cannot run review fix")
		return
	}
	s.logActor(r, "pr_action", "pr", ctx.pr.Number, "anvil", ctx.pr.Anvil, "action", "burnish")
	s.dispatchAction(w, "pr_action", ipc.PRActionPayload{
		PRID:     ctx.prID,
		PRNumber: ctx.pr.Number,
		Anvil:    ctx.pr.Anvil,
		BeadID:   ctx.pr.BeadID,
		Branch:   ctx.pr.Branch,
		Action:   "burnish",
	})
}

// handlePRFixConflicts handles POST /api/prs/{id}/fix-conflicts — kicks off a
// rebase against the PR's base branch.
func (s *Server) handlePRFixConflicts(w http.ResponseWriter, r *http.Request) {
	ctx, ok := s.requirePR(w, r)
	if !ok {
		return
	}
	if ctx.pr.Branch == "" {
		writeError(w, http.StatusBadRequest, "PR has no branch on record; cannot rebase")
		return
	}
	s.logActor(r, "pr_action", "pr", ctx.pr.Number, "anvil", ctx.pr.Anvil, "action", "rebase")
	s.dispatchAction(w, "pr_action", ipc.PRActionPayload{
		PRID:     ctx.prID,
		PRNumber: ctx.pr.Number,
		Anvil:    ctx.pr.Anvil,
		BeadID:   ctx.pr.BeadID,
		Branch:   ctx.pr.Branch,
		Action:   "rebase",
	})
}

// handlePRResetCounters handles POST /api/prs/{id}/reset-counters — clears
// the per-PR fix/rebase counters and flips status back to open so bellows
// re-detects the PR. Routes through retry_bead with PRID, which is the
// existing daemon path for "rescue an exhausted PR".
func (s *Server) handlePRResetCounters(w http.ResponseWriter, r *http.Request) {
	ctx, ok := s.requirePR(w, r)
	if !ok {
		return
	}
	s.logActor(r, "retry_bead", "pr_id", ctx.prID, "anvil", ctx.pr.Anvil, "scope", "reset_counters")
	s.dispatchAction(w, "retry_bead", ipc.RetryBeadPayload{
		BeadID: ctx.pr.BeadID,
		Anvil:  ctx.pr.Anvil,
		PRID:   ctx.prID,
	})
}
