package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
)

// beadCloseAttentionPrefix marks the retries.last_error of a needs-attention
// entry raised by the close-after-merge path. Clearing is scoped to rows
// carrying this prefix so a later successful close cannot silently drop a
// needs_human flag some other subsystem raised for the same bead.
const beadCloseAttentionPrefix = "merged but unclosed bead: "

// bdRetryPolicy bounds the close-after-merge retry burst. The defaults give
// four attempts spread over roughly a minute (0s, 5s, 15s, 45s), which covers
// the transient dolt/beads failures seen in practice — serialization
// rollbacks, connection drops, and schema-migration lock timeouts all clear in
// seconds — without holding a goroutine for the many minutes a genuinely
// wedged anvil would need. Anything the burst cannot fix falls through to the
// pending_bead_closes row and is retried on the next Bellows cycle instead.
type bdRetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	Multiplier  float64
	MaxDelay    time.Duration
	// Sleep waits for d honouring ctx, returning false when ctx is done. nil
	// selects the real timer; tests inject a no-op so the burst runs instantly.
	Sleep func(ctx context.Context, d time.Duration) bool
}

func defaultBdRetryPolicy() bdRetryPolicy {
	return bdRetryPolicy{
		MaxAttempts: 4,
		BaseDelay:   5 * time.Second,
		Multiplier:  3,
		MaxDelay:    60 * time.Second,
	}
}

func (p bdRetryPolicy) sleep(ctx context.Context, d time.Duration) bool {
	if p.Sleep != nil {
		return p.Sleep(ctx, d)
	}
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// retryableBdErrorSubstrings are the lowercased markers that identify a bd
// failure worth retrying. Each entry is a transport- or contention-level
// failure where the bead itself is fine and the very next attempt may succeed;
// anything not listed here (unknown bead, bad flag, bd not installed) is
// permanent and must fail fast rather than burn the budget.
//
// The three observed on 2026-08-06 are the first three groups: a MySQL i/o
// timeout with a follow-on "invalid connection", a Dolt serialization failure
// (Error 1213, which Dolt itself documents as retryable), and a schema
// migration lock timeout.
var retryableBdErrorSubstrings = []string{
	// Dolt/MySQL transaction contention. Error 1213 is the canonical
	// serialization-failure code; the prose forms appear in different bd
	// versions and wrappers.
	"error 1213",
	"serialization failure",
	"deadlock",
	"try restarting transaction",
	// Connection-level failures against the Dolt sql-server.
	"i/o timeout",
	"invalid connection",
	"bad connection",
	"connection refused",
	"connection reset",
	"broken pipe",
	"unexpected eof",
	"server has gone away",
	"can't connect to",
	"no such host",
	// Lock acquisition timeouts (schema migration lock, table locks).
	"lock unavailable",
	"lock wait timeout",
	"timeout acquiring lock",
	"could not acquire lock",
	"database is locked",
	// Generic transient shapes.
	"context deadline exceeded",
	"temporarily unavailable",
	"try again",
}

// isRetryableBdError reports whether a `bd close` failure is transient. The
// match is substring-based on the lowercased error text because bd surfaces
// the underlying driver message as combined stdout+stderr rather than as a
// typed error, so there is nothing structured to switch on.
func isRetryableBdError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// An already-closed bead is a success in disguise, not a transient
	// failure — retrying it would just churn Dolt commits.
	if isAlreadyClosedBdError(err) {
		return false
	}
	for _, s := range retryableBdErrorSubstrings {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// isAlreadyClosedBdError reports whether the failure means the bead was
// already closed by another path (the startup catch-up, an operator, a
// concurrent cycle). Treated as success everywhere in this file.
func isAlreadyClosedBdError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already closed") || strings.Contains(msg, "already_closed")
}

// closeBeadWithRetry runs `bd close` with a bounded backoff, retrying only
// transient failures. It returns the number of attempts made alongside the
// final error so the caller can persist an accurate cumulative count.
//
// Each attempt gets its own bd timeout; ctx bounds the whole burst and
// cancels it between attempts.
func (d *Daemon) closeBeadWithRetry(ctx context.Context, beadID, anvil, anvilPath, reason string, prNumber int) (attempts int, err error) {
	policy := d.bdCloseRetry
	if policy.MaxAttempts <= 0 {
		policy = defaultBdRetryPolicy()
	}

	delay := policy.BaseDelay
	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, executil.DefaultBdTimeout)
		err = d.closeBead(attemptCtx, beadID, anvilPath, reason)
		cancel()

		if err == nil || isAlreadyClosedBdError(err) {
			return attempt, nil
		}
		if !isRetryableBdError(err) {
			// Permanent: surface immediately rather than spending the budget.
			d.logger.Warn("failed to close bead after PR merge (permanent error, not retrying)",
				"bead", beadID, "anvil", anvil, "pr", prNumber,
				"attempt", attempt, "max_attempts", policy.MaxAttempts, "error", err)
			return attempt, err
		}

		d.logger.Warn("failed to close bead after PR merge (transient, retrying)",
			"bead", beadID, "anvil", anvil, "pr", prNumber,
			"attempt", attempt, "max_attempts", policy.MaxAttempts, "error", err)

		if attempt == policy.MaxAttempts {
			break
		}
		if !policy.sleep(ctx, delay) {
			return attempt, ctx.Err()
		}
		if policy.Multiplier > 1 {
			delay = time.Duration(float64(delay) * policy.Multiplier)
		}
		if policy.MaxDelay > 0 && delay > policy.MaxDelay {
			delay = policy.MaxDelay
		}
	}
	return policy.MaxAttempts, err
}

// closeMergedBead is the single entry point for closing a bead whose PR has
// merged, used both by the live Bellows event and by the pending-close
// reconciler. It runs the retry burst and then reconciles the persisted
// pending row and the needs-attention flag:
//
//	success  → drop the pending row, clear the flag we raised
//	failure  → persist/refresh the pending row, raise needs-attention
//
// prior is the persisted pending row this call is retrying, or nil when the
// close is fresh off the merge event.
//
// It is safe to call concurrently for different beads; concurrent calls for
// the same bead are collapsed so a slow burst is not duplicated by the next
// Bellows cycle.
func (d *Daemon) closeMergedBead(ctx context.Context, beadID, anvil, anvilPath, reason string, prNumber int, prior *state.PendingBeadClose) error {
	key := anvil + "\x00" + beadID
	if _, busy := d.beadCloseInFlight.LoadOrStore(key, true); busy {
		return nil
	}
	defer d.beadCloseInFlight.Delete(key)

	attempts, err := d.closeBeadWithRetry(ctx, beadID, anvil, anvilPath, reason, prNumber)
	if err == nil {
		d.finishPendingBeadClose(beadID, anvil, prNumber, prior != nil)
		d.logger.Info("bead closed after PR merge",
			"bead", beadID, "anvil", anvil, "pr", prNumber, "attempts", attempts)
		return nil
	}

	total := attempts
	if prior != nil {
		total += prior.Attempts
	}
	// Never give up silently: persist the close so later cycles re-attempt it.
	// MergedAt is left zero — the upsert never rewrites merged_at, so the store
	// stamps it the first time a close is found to be stuck, which is the
	// timestamp an operator actually wants to sort a stall by.
	if perr := d.db.UpsertPendingBeadClose(state.PendingBeadClose{
		BeadID:    beadID,
		Anvil:     anvil,
		PRNumber:  prNumber,
		Reason:    reason,
		Attempts:  attempts,
		LastError: err.Error(),
	}); perr != nil {
		d.logger.Error("failed to persist pending bead close", "bead", beadID, "anvil", anvil, "error", perr)
	}

	// A burst cut short by shutdown is not an exhausted budget — the pending
	// row above already guarantees the next daemon lifetime retries it, so
	// escalating here would just hand the operator a self-healing alert.
	if ctx.Err() != nil {
		d.logger.Info("bead close after PR merge interrupted — will retry on the next cycle",
			"bead", beadID, "anvil", anvil, "pr", prNumber, "attempts", attempts)
		return err
	}

	// Put it in front of the operator with the dependent count: a merged-but-open
	// bead blocks everything queued behind it.
	dependents, dependentsKnown := d.countOpenDependents(anvilPath, beadID)
	detail := d.beadCloseAttentionMessage(beadID, prNumber, dependents, dependentsKnown)
	if merr := d.db.MarkNeedsHuman(beadID, anvil, beadCloseAttentionPrefix+detail+" — last error: "+err.Error()); merr != nil {
		d.logger.Error("failed to raise needs-attention for unclosed merged bead", "bead", beadID, "anvil", anvil, "error", merr)
	}
	_ = d.db.LogEvent(state.EventBeadCloseRetryExhausted, detail, beadID, anvil)

	d.logger.Warn("failed to close bead after PR merge — retries exhausted, close stays pending",
		"bead", beadID, "anvil", anvil, "pr", prNumber,
		"attempts", attempts, "total_attempts", total,
		"blocked_dependents", dependents, "dependents_known", dependentsKnown, "error", err)
	return err
}

// beadCloseAttentionMessage renders the operator-facing description. The
// dependent count is omitted rather than guessed when the lookup itself failed
// — a wrong "blocking 0 dependents" would read as harmless when it isn't.
func (d *Daemon) beadCloseAttentionMessage(beadID string, prNumber, dependents int, dependentsKnown bool) string {
	if !dependentsKnown {
		return fmt.Sprintf("merged but unclosed bead %s (PR #%d)", beadID, prNumber)
	}
	plural := "dependents"
	if dependents == 1 {
		plural = "dependent"
	}
	return fmt.Sprintf("merged but unclosed bead %s (PR #%d) blocking %d %s", beadID, prNumber, dependents, plural)
}

// finishPendingBeadClose drops the pending row and clears the needs-attention
// entry this path raised. recovered marks the case where the bead had
// previously exhausted its retries, so the recovery is worth an event.
func (d *Daemon) finishPendingBeadClose(beadID, anvil string, prNumber int, recovered bool) {
	if err := d.db.DeletePendingBeadClose(beadID, anvil); err != nil {
		d.logger.Error("failed to drop pending bead close", "bead", beadID, "anvil", anvil, "error", err)
	}
	d.clearBeadCloseAttention(beadID, anvil)
	if recovered {
		_ = d.db.LogEvent(state.EventBeadCloseRecovered,
			fmt.Sprintf("bead %s closed on a later cycle after PR #%d merged", beadID, prNumber), beadID, anvil)
	}
}

// clearBeadCloseAttention clears needs_human only when the flag was raised by
// this path, identified by the marker prefix in last_error. Any other reason
// (circuit breaker, crucible failure, clarification escalation) is left alone.
func (d *Daemon) clearBeadCloseAttention(beadID, anvil string) {
	r, err := d.db.GetRetry(beadID, anvil)
	if err != nil || r == nil || !r.NeedsHuman {
		return
	}
	if !strings.HasPrefix(r.LastError, beadCloseAttentionPrefix) {
		return
	}
	if err := d.db.ClearNeedsAttention(beadID, anvil); err != nil {
		d.logger.Error("failed to clear needs-attention after bead close", "bead", beadID, "anvil", anvil, "error", err)
	}
}

// countOpenDependents returns how many not-yet-closed beads depend on beadID.
// The second return value reports whether the lookup succeeded — callers must
// not present a zero from a failed lookup as a real count.
func (d *Daemon) countOpenDependents(anvilPath, beadID string) (int, bool) {
	if d.beadShower == nil {
		return 0, false
	}
	out, stderr, err := d.beadShower(anvilPath, beadID)
	if err != nil {
		d.logger.Debug("countOpenDependents: bd show failed", "bead", beadID, "error", err, "stderr", stderr)
		return 0, false
	}
	var resp struct {
		Dependents []struct {
			Status string `json:"status"`
		} `json:"dependents"`
	}
	if err := executil.DecodeJSON(out, &resp); err != nil {
		var resps []struct {
			Dependents []struct {
				Status string `json:"status"`
			} `json:"dependents"`
		}
		if arrErr := json.Unmarshal(out, &resps); arrErr != nil || len(resps) == 0 {
			d.logger.Debug("countOpenDependents: failed to parse bd show", "bead", beadID, "error", err)
			return 0, false
		}
		resp.Dependents = resps[0].Dependents
	}
	n := 0
	for _, dep := range resp.Dependents {
		if !strings.EqualFold(dep.Status, "closed") {
			n++
		}
	}
	return n, true
}

// reconcilePendingBeadCloses re-attempts every close that a previous cycle
// could not complete. Each entry is first re-derived from bd: a bead that is
// already closed (by an operator, the startup catch-up, or a racing cycle)
// drops its row without spending a single `bd close`.
func (d *Daemon) reconcilePendingBeadCloses(ctx context.Context) {
	pending, err := d.db.PendingBeadCloses()
	if err != nil {
		d.logger.Error("failed to list pending bead closes", "error", err)
		return
	}
	if len(pending) == 0 {
		return
	}

	cfg := d.cfg.Load()
	for _, p := range pending {
		if ctx.Err() != nil {
			return
		}
		anvilCfg, ok := cfg.Anvils[p.Anvil]
		if !ok || anvilCfg.Path == "" {
			// The anvil is gone from config; the row can never be actioned
			// again, so drop it rather than retrying it forever.
			d.logger.Warn("dropping pending bead close for unconfigured anvil",
				"bead", p.BeadID, "anvil", p.Anvil, "pr", p.PRNumber)
			if derr := d.db.DeletePendingBeadClose(p.BeadID, p.Anvil); derr != nil {
				d.logger.Error("failed to drop pending bead close", "bead", p.BeadID, "anvil", p.Anvil, "error", derr)
			}
			d.clearBeadCloseAttention(p.BeadID, p.Anvil)
			continue
		}

		if status := d.fetchBeadStatus(anvilCfg.Path, p.BeadID); status == "closed" {
			d.logger.Info("pending bead close resolved externally — bead already closed",
				"bead", p.BeadID, "anvil", p.Anvil, "pr", p.PRNumber)
			d.finishPendingBeadClose(p.BeadID, p.Anvil, p.PRNumber, true)
			continue
		}

		reason := p.Reason
		if reason == "" {
			reason = fmt.Sprintf("PR #%d merged", p.PRNumber)
		}
		_ = d.closeMergedBead(ctx, p.BeadID, p.Anvil, anvilCfg.Path, reason, p.PRNumber, &p)
	}
}

// kickPendingBeadCloseReconcile runs the reconciler off the Bellows poll
// goroutine. Bellows calls its handlers synchronously, so a retry burst run
// inline would stall PR monitoring for the length of the backoff. A single
// in-flight guard keeps successive cycles from stacking reconcilers when one
// run outlives the poll interval.
func (d *Daemon) kickPendingBeadCloseReconcile(ctx context.Context) {
	if !d.beadCloseReconciling.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer d.beadCloseReconciling.Store(false)
		// Detach from the poll context: it is cancelled at the end of the
		// cycle, which would abort a burst mid-backoff. The budget below keeps
		// the goroutine bounded regardless.
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), beadCloseBudget)
		defer cancel()
		d.reconcilePendingBeadCloses(runCtx)
	}()
}

// beadCloseBudget bounds one close pass end to end: the retry burst per bead
// (four bd timeouts plus the backoff between them), across however many
// pending beads there are.
const beadCloseBudget = 10 * time.Minute
