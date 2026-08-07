package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Robin831/Forge/internal/burnish"
	"github.com/Robin831/Forge/internal/lifecycle"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/worktree"
)

// reviewFixLoopPrefix marks the retries.last_error of a needs-attention entry
// raised by the review-fix circuit breaker, so a later clear can tell it apart
// from every other reason a bead is flagged. It sits alongside burnish's own
// AttentionUnverified / AttentionUnpushed prefixes.
const reviewFixLoopPrefix = "review fix loop: "

// mergeResolvedAttentionPrefixes are the review-fix escalations a merged or
// closed PR genuinely answers, so they can be cleared with it.
//
// burnish.AttentionUnpushed is deliberately absent: a merge says the branch
// landed, not that the preserved commit did — it never reached the remote, and
// its worktree still holds the only copy. Clearing that entry would delete the
// only pointer to work nobody has looked at.
var mergeResolvedAttentionPrefixes = []string{
	burnish.AttentionUnverified,
	reviewFixLoopPrefix,
}

// reviewFixDispatchAllowed is the circuit breaker in front of Burnish. It
// records the dispatch against the PR's current head and refuses once the head
// has not moved across the whole attempt budget.
//
// Why the head SHA and not the existing ReviewFixCnt: that counter bounds
// review fixes for the PR's entire life and never resets, so it treats a PR
// that is genuinely progressing (each round pushes a new head and addresses new
// comments) exactly like one that rebuilds the identical diff every cycle. The
// second is the expensive failure — one full Smith run per Bellows cycle, 25
// minutes of Opus each, producing the same commit every time (Forge-xl50) — and
// an unchanged head is precisely what identifies it.
//
// It fails OPEN: if the head SHA cannot be read, or the bookkeeping write
// fails, the dispatch proceeds. A breaker that blocks fixes because `gh` was
// briefly unreachable would be worse than the loop it prevents.
func (d *Daemon) reviewFixDispatchAllowed(ctx context.Context, req lifecycle.ActionRequest, anvilPath string) bool {
	// A manual dispatch is an operator overriding the machine's judgement;
	// the breaker is exactly what they are overriding.
	if req.IsManual {
		return true
	}

	headSHA := d.prHeadSHA(ctx, req.Anvil, anvilPath, req.PRNumber)
	if headSHA == "" {
		d.logger.Warn("review fix circuit breaker: PR head SHA unavailable, dispatching unchecked",
			"pr", req.PRNumber, "anvil", req.Anvil)
		return true
	}

	attempts, err := d.db.RecordReviewFixDispatch(req.Anvil, req.PRNumber, headSHA)
	if err != nil {
		d.logger.Warn("review fix circuit breaker: failed to record dispatch, dispatching unchecked",
			"pr", req.PRNumber, "anvil", req.Anvil, "error", err)
		return true
	}

	limit := d.cfg.Load().Settings.MaxSameHeadReviewFixes
	if limit <= 0 {
		limit = state.DefaultMaxSameHeadReviewFixes
	}
	if attempts <= limit {
		d.logger.Info("review fix dispatch",
			"pr", req.PRNumber, "anvil", req.Anvil,
			"head", shortSHA(headSHA), "attempt", attempts, "limit", limit)
		return true
	}

	detail := d.reviewFixBreakerMessage(req.Anvil, req.PRNumber, headSHA, attempts, limit)
	d.logger.Warn("review fix circuit breaker tripped — refusing to rebuild identical work",
		"pr", req.PRNumber, "anvil", req.Anvil, "head", shortSHA(headSHA), "attempts", attempts, "limit", limit)
	_ = d.db.LogEvent(state.EventBurnishCircuitBroken, detail, req.BeadID, req.Anvil)
	if req.BeadID != "" {
		if err := d.db.MarkNeedsHuman(req.BeadID, req.Anvil, reviewFixLoopPrefix+detail); err != nil {
			d.logger.Error("failed to raise needs-attention for review fix circuit breaker",
				"bead", req.BeadID, "anvil", req.Anvil, "error", err)
		}
	}
	return false
}

// reviewFixBreakerMessage renders the operator-facing description of a tripped
// breaker. It names how the previous attempts ended, because "3 attempts, all
// preserved" and "3 attempts, all pushed unverified" call for different actions.
func (d *Daemon) reviewFixBreakerMessage(anvil string, prNumber int, headSHA string, attempts, limit int) string {
	msg := fmt.Sprintf("PR #%d head %s dispatched %d review fixes (limit %d) without the head moving — burnish is rebuilding identical work",
		prNumber, shortSHA(headSHA), attempts, limit)
	if rec, err := d.db.GetReviewFixDispatch(anvil, prNumber); err == nil && rec != nil && rec.LastResult != "" {
		msg += fmt.Sprintf("; last outcome: %s", rec.LastResult)
	}
	return msg
}

// recordReviewFixOutcome stores how a review-fix dispatch ended, so a later
// tripped breaker can say what the wasted attempts were doing.
func (d *Daemon) recordReviewFixOutcome(req lifecycle.ActionRequest, res *burnish.FixResult) {
	if res == nil {
		return
	}
	var outcome string
	switch {
	case res.UnpushedHead != "":
		outcome = state.ReviewFixResultPreserved
	case res.Unverified:
		outcome = state.ReviewFixResultUnverifiedPush
	case res.Addressed:
		outcome = state.ReviewFixResultPushed
	default:
		outcome = state.ReviewFixResultFailed
	}
	if err := d.db.SetReviewFixDispatchResult(req.Anvil, req.PRNumber, outcome); err != nil {
		d.logger.Warn("failed to record review fix outcome", "pr", req.PRNumber, "anvil", req.Anvil, "error", err)
	}
	if res.Unverified {
		d.logger.Warn("review fix pushed UNVERIFIED — verification never completed",
			"pr", req.PRNumber, "bead", req.BeadID, "anvil", req.Anvil, "head", shortSHA(res.HeadSHA))
	}
}

// clearReviewFixDispatch drops the head-scoped bookkeeping once a PR has left
// the fix loop for good (merged or closed), so a PR number reused by nothing
// still cannot inherit a stale count.
func (d *Daemon) clearReviewFixDispatch(req lifecycle.ActionRequest) {
	if req.PRNumber <= 0 {
		return
	}
	if err := d.db.DeleteReviewFixDispatch(req.Anvil, req.PRNumber); err != nil {
		d.logger.Warn("failed to clear review fix dispatch bookkeeping",
			"pr", req.PRNumber, "anvil", req.Anvil, "error", err)
	}
	d.clearReviewFixAttention(req.BeadID, req.Anvil)
}

// clearReviewFixAttention drops a needs-attention entry the review-fix path
// raised, but only one the PR leaving the loop actually resolves — identified
// by the marker prefix in last_error. Every other reason a bead is flagged
// (circuit breaker on a different PR, crucible failure, clarification) is left
// alone, and so is a preserved unpushed commit, which a merge does not recover.
func (d *Daemon) clearReviewFixAttention(beadID, anvil string) {
	if beadID == "" {
		return
	}
	r, err := d.db.GetRetry(beadID, anvil)
	if err != nil || r == nil || !r.NeedsHuman {
		return
	}
	resolved := false
	for _, prefix := range mergeResolvedAttentionPrefixes {
		if strings.HasPrefix(r.LastError, prefix) {
			resolved = true
			break
		}
	}
	if !resolved {
		return
	}
	if err := d.db.ClearNeedsAttention(beadID, anvil); err != nil {
		d.logger.Error("failed to clear review fix needs-attention",
			"bead", beadID, "anvil", anvil, "error", err)
	}
}

// prHeadSHA resolves a PR's current head commit, or "" when it cannot be read.
func (d *Daemon) prHeadSHA(ctx context.Context, anvil, anvilPath string, prNumber int) string {
	st, err := d.vcsForAnvil(anvil).CheckStatusLight(ctx, anvilPath, prNumber)
	if err != nil {
		d.logger.Warn("failed to read PR head SHA", "pr", prNumber, "anvil", anvil, "error", err)
		return ""
	}
	if st == nil {
		return ""
	}
	return st.HeadSHA
}

// removeLifecycleWorktree tears down a fix worker's worktree.
//
// For a review fix it goes through worktree.RemoveIfPushed, which refuses to
// delete a checkout whose HEAD is not on the remote. Burnish commits into the
// worktree and only pushes once the fix is confirmed, so the old unconditional
// removal turned every unconfirmed outcome into silent data loss: a finished,
// correct fix commit reachable from nothing (Forge-xl50). Keeping the checkout
// costs one directory — the next dispatch for the same bead reuses it — against
// a 25-minute Smith run thrown away.
//
// The other fix workers still remove unconditionally. Quench and Rebase have
// their own push semantics and their own reasons for leaving a branch behind,
// so extending the guard to them is a separate decision, not a side effect of
// this one.
func (d *Daemon) removeLifecycleWorktree(ctx context.Context, req lifecycle.ActionRequest, anvilPath string, wt *worktree.Worktree) {
	if req.Action != lifecycle.ActionFixReview {
		d.worktreeMgr.Remove(ctx, anvilPath, wt)
		return
	}

	err := d.worktreeMgr.RemoveIfPushed(ctx, anvilPath, wt)
	var unpushed *worktree.UnpushedHeadError
	if !errors.As(err, &unpushed) {
		if err != nil {
			d.logger.Warn("failed to remove review fix worktree", "bead", req.BeadID, "path", wt.Path, "error", err)
		}
		return
	}

	detail := fmt.Sprintf("PR #%d: review fix commit %s is UNPUSHED and kept in worktree %s (branch %s) — push or cherry-pick it before the checkout is reclaimed",
		req.PRNumber, shortSHA(unpushed.LocalHead), unpushed.Path, unpushed.Branch)
	d.logger.Warn("kept review fix worktree: HEAD is not on the remote",
		"bead", req.BeadID, "anvil", req.Anvil, "pr", req.PRNumber,
		"head", shortSHA(unpushed.LocalHead), "path", unpushed.Path)
	_ = d.db.LogEvent(state.EventBurnishWorkPreserved, detail, req.BeadID, req.Anvil)
	if req.BeadID != "" {
		if merr := d.db.MarkNeedsHuman(req.BeadID, req.Anvil, reviewFixLoopPrefix+detail); merr != nil {
			d.logger.Error("failed to raise needs-attention for preserved review fix worktree",
				"bead", req.BeadID, "anvil", req.Anvil, "error", merr)
		}
	}
}

// shortSHA abbreviates a commit id for operator-facing messages.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
