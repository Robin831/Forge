// Package bellows monitors open PRs for status changes, CI results, and reviews.
//
// Bellows periodically polls all open PRs in the state DB and updates their
// status. It triggers downstream actions: CI fix workers, review comment
// forwarding, and PR lifecycle state tracking.
package bellows

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/ingot"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/Robin831/Forge/internal/vcs/github"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/Robin831/Forge/internal/worktree"
)

// Event types emitted by the Bellows monitor.
const (
	EventCIPassed       = "ci_passed"
	EventCIFailed       = "ci_failed"
	EventReviewApproved = "review_approved"
	EventReviewChanges  = "review_changes_requested"
	EventPRMerged       = "pr_merged"
	EventPRClosed       = "pr_closed"
	EventPRConflicting  = "pr_conflicting"
	EventPRReadyToMerge = "pr_ready_to_merge"
	// EventPRReviewNeeded signals that a PR's current head should be reviewed
	// by Assay. It is emitted by the Assay trigger gate in checkPR when the
	// head SHA differs from the last reviewed SHA and all gating conditions
	// (managed, open, not draft, CI settled, debounce, daily cost) are met.
	EventPRReviewNeeded = "pr_review_needed"
)

// defaultAssayDebounceSeconds is the fallback minimum interval between Assay
// runs for the same (anvil, PR) when the configured debounce is unset (<= 0).
const defaultAssayDebounceSeconds = 300

// PREvent is emitted when a PR status changes.
type PREvent struct {
	PRNumber  int
	BeadID    string
	Anvil     string
	Branch    string
	EventType string
	Details   string
	Timestamp time.Time
	// PRURL is the GitHub URL of the PR, populated for events that need it
	// (e.g. pr_ready_to_merge).
	PRURL string
	// HeadSHA is the commit OID at the head of the PR branch, populated for
	// events that need it (e.g. pr_review_needed, so the Assay reviewer knows
	// which head it is reviewing and can record the run against it).
	HeadSHA string
}

// AssayGateConfig holds the resolved Assay settings the trigger gate needs to
// decide whether to emit EventPRReviewNeeded for a PR. The daemon supplies a
// per-anvil accessor via Monitor.SetAssayConfig so hot-reloaded config changes
// take effect without restarting.
type AssayGateConfig struct {
	// Enabled gates the whole feature: when false the trigger never fires.
	Enabled bool
	// SkipDrafts suppresses the trigger for draft PRs when true.
	SkipDrafts bool
	// DebounceSeconds is the minimum interval between Assay runs for the same
	// (anvil, PR). Values <= 0 fall back to defaultAssayDebounceSeconds.
	DebounceSeconds int
	// DailyCostLimitUSD caps total Assay spend per day; 0 means no limit.
	DailyCostLimitUSD float64
	// MaxRuns caps the number of executed Assay reviews per PR; once reached the
	// trigger never fires again for that PR, stopping the Assay→Burnish→new-head
	// loop. Values <= 0 mean no cap.
	MaxRuns int
}

// Handler is called when a PR event is detected.
type Handler func(ctx context.Context, event PREvent)

// Monitor watches open PRs and dispatches events on status changes.
type Monitor struct {
	db                   *state.DB
	vcsLookup            func(anvil string) vcs.Provider
	interval             time.Duration
	anvilPaths           map[string]string // anvil name → path
	pathsMu              sync.RWMutex      // protects anvilPaths
	handlers             []Handler
	mu                   sync.Mutex
	lastStatuses         map[string]*prSnapshot                               // anvil/PR number → last known state
	refresh              chan struct{}                                        // channel to trigger immediate poll
	autoLearnRules       func() bool                                          // auto-learn warden rules from Copilot comments on PR merge
	maxCIFixAttempts     func() int                                           // returns current max CI fix attempts from config
	maxReviewFixAttempts func() int                                           // returns current max review fix attempts from config
	maxRebaseAttempts    func() int                                           // returns current max rebase attempts from config
	learnMuGuard         sync.Mutex                                           // protects learnMu map
	learnMu              map[string]*sync.Mutex                               // per-anvil mutex serializing auto-learn
	learnSem             chan struct{}                                        // caps overall concurrent auto-learn goroutines
	wasUnmanaged         map[string]bool                                      // keys of ext- PRs seen as unmanaged (for managed transition detection)
	wasDetached          map[string]bool                                      // keys of PRs seen as bellows-detached (for resume transition detection)
	autoMergeHandler     func(ctx context.Context, anvil string, pr state.PR) // called when a PR becomes ready-to-merge
	smelterEnabled       func() bool                                          // when true, route learned rules to pending table instead of PR
	assayConfig          func(anvil string) AssayGateConfig                   // resolved Assay gate config; nil disables the trigger
	beadInFlight         func(beadID string) bool                             // reports whether a lifecycle fix worker is active for a bead; nil disables (tests)
	cycleHook            func(ctx context.Context)                            // called at the start of every poll cycle, before any PR work; nil disables

	// ciStuckNotified records the head SHA a "CI appears stuck" note was last
	// raised for, keyed by "anvil/number", so a wedged run is surfaced once per
	// head instead of on every poll. Guarded by mu.
	ciStuckNotified map[string]string

	// assaySuppressNotified records "headSHA|reason" per "anvil/number" so a
	// budget-suppressed Assay review (daily cost cap, per-PR run cap) is
	// surfaced once per head instead of on every poll. Guarded by mu.
	assaySuppressNotified map[string]string

	// now returns the current time for CI age evaluation. nil selects
	// time.Now; tests set it to pin the stuck threshold.
	now func() time.Time

	// retryBackoff overrides the inline retry backoff used when wrapping gh
	// status fetches in transient-failure retries. nil selects
	// github.DefaultRetryBackoff(); tests set a zero-delay backoff to avoid
	// real sleeps.
	retryBackoff *github.RetryBackoff
}

// prSnapshot tracks the last seen state of a PR.
type prSnapshot struct {
	CIPassing            bool
	CIInProgress         bool
	HasApproval          bool
	NeedsChanges         bool
	HasUnresolvedThreads bool
	HasPendingReviews    bool
	IsMerged             bool
	IsClosed             bool
	IsConflicting        bool
	// AssayUpToDate is true when the PR's current head has been reviewed by
	// Assay (or Assay is disabled for the anvil). It gates the ready-to-merge
	// transition so a PR is not announced ready while an Assay review is still
	// pending or in-flight for this head (Forge-75cx). It is tracked per
	// snapshot so the ready transition fires on the rising edge — once the
	// assay lands — rather than prematurely on first sighting.
	AssayUpToDate bool
}

// New creates a Bellows monitor. The vcsLookup function returns the VCS
// provider for a given anvil name, enabling per-anvil platform support.
// The autoLearnRules function is called on each PR merge to check whether
// warden rule learning is enabled, so hot-reloaded config changes take effect
// without restarting the daemon. The maxCIFixAttempts, maxReviewFixAttempts,
// and maxRebaseAttempts functions return the current max fix attempts from
// config (any may be nil, in which case the corresponding state.Default* is
// used).
func New(db *state.DB, vcsLookup func(anvil string) vcs.Provider, interval time.Duration, anvilPaths map[string]string, autoLearnRules func() bool, maxCIFixAttempts func() int, maxReviewFixAttempts func() int, maxRebaseAttempts func() int) *Monitor {
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	if maxCIFixAttempts == nil {
		maxCIFixAttempts = func() int { return state.DefaultMaxCIFixAttempts }
	}
	if maxReviewFixAttempts == nil {
		maxReviewFixAttempts = func() int { return state.DefaultMaxReviewFixAttempts }
	}
	if maxRebaseAttempts == nil {
		maxRebaseAttempts = func() int { return state.DefaultMaxRebaseAttempts }
	}
	return &Monitor{
		db:                    db,
		vcsLookup:             vcsLookup,
		interval:              interval,
		anvilPaths:            anvilPaths,
		lastStatuses:          make(map[string]*prSnapshot),
		refresh:               make(chan struct{}, 1),
		autoLearnRules:        autoLearnRules,
		maxCIFixAttempts:      maxCIFixAttempts,
		maxReviewFixAttempts:  maxReviewFixAttempts,
		maxRebaseAttempts:     maxRebaseAttempts,
		learnMu:               make(map[string]*sync.Mutex),
		learnSem:              make(chan struct{}, 4), // allow up to 4 concurrent auto-learn goroutines
		wasUnmanaged:          make(map[string]bool),
		wasDetached:           make(map[string]bool),
		ciStuckNotified:       make(map[string]string),
		assaySuppressNotified: make(map[string]string),
	}
}

// nowFn returns the monitor's clock, defaulting to time.Now.
func (m *Monitor) nowFn() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// SetAutoMergeHandler registers a callback invoked when a PR transitions to
// the ready-to-merge state. The daemon uses this to trigger automatic merging
// for anvils that have auto_merge enabled.
func (m *Monitor) SetAutoMergeHandler(h func(ctx context.Context, anvil string, pr state.PR)) {
	m.autoMergeHandler = h
}

// SetSmelterEnabled registers a callback that returns whether the smelter is
// enabled. When true, learned warden rules are inserted into the pending
// table instead of creating immediate PRs.
func (m *Monitor) SetSmelterEnabled(f func() bool) {
	m.smelterEnabled = f
}

// SetAssayConfig registers an accessor returning the resolved Assay gate config
// for a given anvil. When set and enabled, the trigger gate in checkPR may emit
// EventPRReviewNeeded. When nil (the default), the Assay trigger is disabled.
func (m *Monitor) SetAssayConfig(f func(anvil string) AssayGateConfig) {
	m.assayConfig = f
}

// SetInFlightChecker wires a predicate reporting whether a lifecycle fix worker
// is currently running for a bead. The "still failing/unresolved" retry
// branches use it to decide whether to re-dispatch a fix. It replaces the older
// `pr.Status != needs_fix` proxy, which silently wedged any PR parked in
// needs_fix with no active worker — e.g. when a parked lifecycle action was
// dropped (single-slot latest-wins overwrite) or the daemon restarted mid-fix:
// the status never flips back, so the retry never re-fires. When no checker is
// wired (tests), inFlight() returns false so retries fall through as before.
func (m *Monitor) SetInFlightChecker(f func(beadID string) bool) {
	m.beadInFlight = f
}

// inFlight reports whether a lifecycle fix worker is currently running for
// beadID. Safe with no checker wired (returns false).
func (m *Monitor) inFlight(beadID string) bool {
	return m.beadInFlight != nil && m.beadInFlight(beadID)
}

// SetCycleHook registers a callback invoked at the top of every poll cycle,
// before any PR is examined — including cycles where there are no open PRs at
// all. It exists for per-cycle reconciliation that is not tied to a specific
// PR (the merged-but-unclosed bead sweep). The hook runs on the poll goroutine,
// so it must return promptly; anything slow belongs in a goroutine it owns.
func (m *Monitor) SetCycleHook(f func(ctx context.Context)) {
	m.cycleHook = f
}

// escalateFixExhausted flags a PR needs_human once when an auto-fix loop
// (CI-fix or review-fix) has used all its retries but the underlying problem
// (failing CI / unresolved review threads) persists and no worker is running.
// Without this the PR silently stalls: auto-fix stops at the cap (the
// re-dispatch branches require count < max) but nothing surfaces it, so it sits
// green-but-blocked and invisible until someone notices and clicks Fix Comments
// by hand (observed: PR #4471 / Fhi.Metadata-eyizi). Marking needs_human routes
// it into the Needs-Attention panel for triage, consistent with the dispatch
// and recovery circuit breakers. Fires once — it no-ops if the bead is already
// flagged, so it neither re-logs nor churns updated_at every poll. Uses
// LogEvent (not emit) so it does not re-trigger the lifecycle fix handlers.
func (m *Monitor) escalateFixExhausted(pr *state.PR, reason string) {
	if rec, err := m.db.GetRetry(pr.BeadID, pr.Anvil); err == nil && rec != nil && rec.NeedsHuman {
		return
	}
	if err := m.db.MarkNeedsHuman(pr.BeadID, pr.Anvil, reason); err != nil {
		log.Printf("[bellows] PR #%d: failed to flag needs_human after fix-loop exhaustion: %v", pr.Number, err)
		return
	}
	// EventSmithFailed so the Needs-Attention panel classifies it as a
	// worker-level failure (retry / clear / re-review actions), the closest
	// existing terminal escalation type for an exhausted fix loop.
	_ = m.db.LogEvent(state.EventSmithFailed, fmt.Sprintf("PR #%d: %s", pr.Number, reason), pr.BeadID, pr.Anvil)
	log.Printf("[bellows] PR #%d flagged needs_human: %s", pr.Number, reason)
}

// ciStuckAttentionPrefix marks the needs-attention entries raised by
// noteCIStuck, so clearCIStuck can retract its own note without touching a flag
// some other subsystem set (circuit breaker, crucible failure, bead close).
const ciStuckAttentionPrefix = "CI appears stuck/outage: "

// noteCIStuck raises an informational needs-attention entry for a PR whose head
// has a check queued past stuckRunThreshold. This is the alternative to the old
// behaviour during a CI outage: rather than reading a stale conclusion as a
// failure and burning quench attempts on a problem that does not exist on the
// head, we put the wedged run in front of the operator and wait. The note fires
// once per (PR, head SHA) so a two-hour outage does not spam the panel, and
// clearCIStuck retracts it as soon as CI settles.
func (m *Monitor) noteCIStuck(pr *state.PR, ci ciResult) {
	if m.db == nil {
		return
	}
	key := fmt.Sprintf("%s/%d", pr.Anvil, pr.Number)
	m.mu.Lock()
	if seen, ok := m.ciStuckNotified[key]; ok && seen == ci.HeadSHA {
		m.mu.Unlock()
		return
	}
	m.ciStuckNotified[key] = ci.HeadSHA
	m.mu.Unlock()

	detail := fmt.Sprintf("PR #%d: %s (no CI fix worker dispatched)", pr.Number, ci.Reason)
	if err := m.db.MarkNeedsHuman(pr.BeadID, pr.Anvil, ciStuckAttentionPrefix+detail); err != nil {
		log.Printf("[bellows] PR #%d: failed to raise needs-attention for stuck CI: %v", pr.Number, err)
		// Allow a later poll to retry the note rather than swallowing it.
		m.mu.Lock()
		delete(m.ciStuckNotified, key)
		m.mu.Unlock()
		return
	}
	_ = m.db.LogEvent(state.EventCIStuck, detail, pr.BeadID, pr.Anvil)
	log.Printf("[bellows] %s%s", ciStuckAttentionPrefix, detail)
}

// clearCIStuck retracts the note raised by noteCIStuck once the head's CI is no
// longer wedged. It only clears a flag carrying ciStuckAttentionPrefix, so an
// unrelated needs-attention reason survives. The DB read happens on every settled
// poll (not just when this monitor instance raised the note) so a note left
// behind by a previous daemon lifetime is cleaned up too.
func (m *Monitor) clearCIStuck(pr *state.PR) {
	if m.db == nil {
		return
	}
	key := fmt.Sprintf("%s/%d", pr.Anvil, pr.Number)
	m.mu.Lock()
	delete(m.ciStuckNotified, key)
	m.mu.Unlock()

	r, err := m.db.GetRetry(pr.BeadID, pr.Anvil)
	if err != nil || r == nil || !r.NeedsHuman {
		return
	}
	if !strings.HasPrefix(r.LastError, ciStuckAttentionPrefix) {
		return
	}
	if err := m.db.ClearNeedsAttention(pr.BeadID, pr.Anvil); err != nil {
		log.Printf("[bellows] PR #%d: failed to clear stuck-CI needs-attention: %v", pr.Number, err)
	}
}

// lastReviewedSHA returns the head SHA most recently reviewed by Assay for the
// given PR, or "" when no review has run (or on DB error). It wraps the
// state-layer LastReviewedSHA helper (added in sub-task 1) so the trigger gate
// can compare it against the PR's current head.
func (m *Monitor) lastReviewedSHA(pr *state.PR) string {
	sha, err := m.db.LastReviewedSHA(pr.Anvil, pr.Number)
	if err != nil {
		log.Printf("[bellows] Failed to query last reviewed SHA for PR #%d (%s): %v", pr.Number, pr.Anvil, err)
		return ""
	}
	return sha
}

// assayWorkerInFlight reports whether an Assay worker is already pending or
// running for the PR, so the trigger gate can avoid queuing a second review for
// the same head. On DB error it returns false (fail open — better a rare
// duplicate than a missed review).
func (m *Monitor) assayWorkerInFlight(anvil string, prNumber int) bool {
	inFlight, err := m.db.AssayWorkerInFlight(anvil, prNumber)
	if err != nil {
		log.Printf("[bellows] Failed to check in-flight Assay worker for PR #%d (%s): %v", prNumber, anvil, err)
		return false
	}
	return inFlight
}

// beadFixWorkerActive reports whether a non-Assay worker is currently active for
// the bead, so the trigger gate can avoid emitting pr_review_needed that the
// daemon would only skip (busy-looping every debounce). On DB error it returns
// false (fail open — don't permanently suppress reviews on a transient error).
func (m *Monitor) beadFixWorkerActive(anvil, beadID string) bool {
	active, err := m.db.BeadFixWorkerActive(anvil, beadID)
	if err != nil {
		log.Printf("[bellows] Failed to check active fix worker for bead %s (%s): %v", beadID, anvil, err)
		return false
	}
	return active
}

// assayEnabled reports whether the Assay trigger is enabled for the anvil. It is
// false when no Assay config accessor has been registered (the feature is off).
func (m *Monitor) assayEnabled(anvil string) bool {
	return m.assayConfig != nil && m.assayConfig(anvil).Enabled
}

// assayUpToDate reports whether the PR's current head SHA has already been
// reviewed by Assay, or whether Assay is disabled for the anvil. It gates the
// ready-to-merge transition (and auto-merge) so a PR is not announced as ready
// while an Assay review is still pending or in-flight for the head: the assay's
// inline findings would otherwise land a poll or two later, flip
// HasUnresolvedThreads 0->1, and bounce the PR back to needs_fix/Burnish after
// it had already been declared ready (Forge-75cx). Comparing LastReviewedSHA to
// the current head covers BOTH the pre-dispatch window (assay needed but not yet
// started) and the in-flight window (assay running but findings not yet posted),
// which a simple "no pending assay worker" check would miss.
//
// When headSHA is empty (GitHub did not report it), we return true: no Assay
// review will be dispatched (shouldEmitReviewNeeded skips empty SHAs), so
// blocking readiness would be permanent.
func (m *Monitor) assayUpToDate(pr *state.PR, headSHA string, dailyAssayCost *float64) bool {
	if !m.assayEnabled(pr.Anvil) {
		return true
	}
	if headSHA == "" {
		return true
	}
	if m.lastReviewedSHA(pr) == headSHA {
		return true
	}
	cfg := m.assayConfig(pr.Anvil)
	// Per-PR run cap reached: shouldEmitReviewNeeded will not dispatch another
	// review, so the head can never become "reviewed". Blocking readiness on it
	// would deadlock the PR forever (e.g. Burnish fixes comments, pushes a new
	// head, but the cap stops the confirming re-review). Treat an exhausted cap
	// as "Assay done" — readiness is still gated by has_unresolved_threads, so
	// only genuinely clean PRs are released. (Forge-btpw)
	if cfg.MaxRuns > 0 {
		if n, err := m.db.CountAssayRuns(pr.Anvil, pr.Number); err == nil && n >= cfg.MaxRuns {
			return true
		}
	}
	// Daily Assay cost cap reached: shouldEmitReviewNeeded bails on the exact
	// same condition (dailyCostUSD >= dailyCostLimit), so no re-review will be
	// dispatched until the budget resets at UTC midnight. Without releasing the
	// gate here, every otherwise-green PR whose head advanced after its last
	// assay (e.g. via Burnish) silently stalls out of ready-to-merge for the
	// rest of the day — invisible in both the Ready-to-Merge and Needs-Attention
	// panels. Treat the budget-exhausted head as "Assay done" for the day, same
	// rationale as the run cap; readiness is still gated by has_unresolved_threads.
	if dailyAssayCost != nil && cfg.DailyCostLimitUSD > 0 && *dailyAssayCost >= cfg.DailyCostLimitUSD {
		return true
	}
	return false
}

// noteAssaySuppressed surfaces a budget-suppressed Assay review — once per
// (PR, head, reason) — in the daemon log and the events feed. Both budget
// gates also release the head to merge readiness via assayUpToDate, so without
// this note the operator gets no signal at all that a PR is about to merge
// without an Assay review (2026-08-09: the default daily cost cap silently
// swallowed every review after the day's first; 2026-08-07: twenty PRs
// auto-merged unreviewed the same way).
func (m *Monitor) noteAssaySuppressed(pr *state.PR, headSHA, reason string, in reviewGateInputs) {
	if m.db == nil {
		return
	}
	key := fmt.Sprintf("%s/%d", pr.Anvil, pr.Number)
	val := headSHA + "|" + reason
	m.mu.Lock()
	if m.assaySuppressNotified[key] == val {
		m.mu.Unlock()
		return
	}
	m.assaySuppressNotified[key] = val
	m.mu.Unlock()

	var detail string
	switch reason {
	case assaySuppressedDailyCost:
		detail = fmt.Sprintf(
			"PR #%d: Assay review skipped — daily Assay budget exhausted ($%.2f spent >= $%.2f limit, resets at UTC midnight); head %s will count as reviewed for merge readiness",
			pr.Number, in.dailyCostUSD, in.dailyCostLimit, shortSHA(headSHA))
	case assaySuppressedMaxRuns:
		detail = fmt.Sprintf(
			"PR #%d: Assay review skipped — per-PR run cap reached (%d/%d); head %s will count as reviewed for merge readiness",
			pr.Number, in.runCount, in.maxRuns, shortSHA(headSHA))
	default:
		return
	}
	log.Printf("[bellows] %s", detail)
	_ = m.db.LogEvent(state.EventAssaySkipped, detail, pr.BeadID, pr.Anvil)
}

// OnEvent registers a handler for PR events.
func (m *Monitor) OnEvent(h Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers = append(m.handlers, h)
}

// UpdateAnvilPaths replaces the set of monitored anvil paths. This is safe to
// call while Run is active and takes effect on the next poll cycle.
func (m *Monitor) UpdateAnvilPaths(paths map[string]string) {
	copied := make(map[string]string, len(paths))
	for k, v := range paths {
		copied[k] = v
	}
	m.pathsMu.Lock()
	// Retain paths for anvils that still have open PRs so removed anvils
	// don't produce repeated "Unknown anvil" warnings every poll cycle.
	if prs, err := m.db.OpenPRs(); err == nil {
		for i := range prs {
			name := prs[i].Anvil
			if _, inNew := copied[name]; !inNew {
				if oldPath, inOld := m.anvilPaths[name]; inOld {
					copied[name] = oldPath
				}
			}
		}
	}
	m.anvilPaths = copied
	m.pathsMu.Unlock()
}

// Refresh triggers an immediate poll cycle.
func (m *Monitor) Refresh() {
	select {
	case m.refresh <- struct{}{}:
	default:
		// Refresh already pending
	}
}

// Run starts the polling loop. Blocks until ctx is canceled.
func (m *Monitor) Run(ctx context.Context) error {
	log.Printf("[bellows] Starting PR monitor (interval: %s)", m.interval)
	_ = m.db.LogEvent(state.EventBellowsStarted, fmt.Sprintf("PR monitor started (interval: %s)", m.interval), "", "")

	// Startup backfill: directly reconcile any open PR rows whose GitHub state
	// is MERGED or CLOSED. This is a cheap belt-and-braces pass that runs once,
	// independent of the in-memory snapshot machinery in checkPR, so a stuck
	// needs_fix row whose underlying PR has already been merged on GitHub gets
	// corrected on the very first daemon tick rather than waiting for (or
	// hitting bugs in) the normal transition path.
	m.reconcileTerminalStates(ctx)

	// Initial check
	m.checkAll(ctx)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[bellows] Shutting down PR monitor")
			return ctx.Err()
		case <-ticker.C:
			m.checkAll(ctx)
		case <-m.refresh:
			log.Println("[bellows] Immediate poll triggered via refresh")
			m.checkAll(ctx)
		}
	}
}

// reconcileTerminalStates does a one-shot pass over every needs_fix PR in
// state.db, asks GitHub for its current state, and corrects the local row to
// merged or closed when GitHub disagrees. It is a deliberately minimal
// belt-and-braces backfill: it does not emit PREvents to in-process OnEvent
// handlers (only the DB event log and ingot status are updated), and it does
// not touch the in-memory snapshot map, so it cannot regress the
// snapshot-based flow in checkPR. Only needs_fix rows are checked here
// because open/approved PRs are covered by the normal checkAll poll that
// immediately follows; limiting scope avoids a double GitHub API call for
// every open PR on startup. The motivating case is a needs_fix PR that a
// human merged as-is on GitHub — without this pass, the row could remain
// stuck in needs_fix indefinitely if the transition path ever fails to fire.
func (m *Monitor) reconcileTerminalStates(ctx context.Context) {
	if m.vcsLookup == nil {
		return
	}
	prs, err := m.db.OpenPRs()
	if err != nil {
		log.Printf("[bellows] reconcileTerminalStates: error listing open PRs: %v", err)
		return
	}
	for i := range prs {
		if prs[i].Status != state.PRNeedsFix {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		pr := &prs[i]
		anvilVCS := m.vcsLookup(pr.Anvil)
		if anvilVCS == nil {
			continue
		}
		m.pathsMu.RLock()
		anvilPath, ok := m.anvilPaths[pr.Anvil]
		m.pathsMu.RUnlock()
		if !ok {
			continue
		}
		var status *vcs.PRStatus
		err := m.retryTransient(ctx, fmt.Sprintf("reconcile CheckStatus PR #%d", pr.Number), func() error {
			var e error
			status, e = anvilVCS.CheckStatus(ctx, anvilPath, pr.Number)
			return e
		})
		if err != nil {
			log.Printf("[bellows] reconcileTerminalStates: error checking PR #%d (%s): %v", pr.Number, pr.Anvil, err)
			continue
		}
		switch {
		case status.IsMerged() && pr.Status != state.PRMerged:
			log.Printf("[bellows] reconcileTerminalStates: PR #%d (%s) GitHub=MERGED, local=%s — correcting", pr.Number, pr.Anvil, pr.Status)
			_ = m.db.UpdatePRStatus(pr.ID, state.PRMerged)
			_ = m.db.LogEvent(state.EventPRMerged, fmt.Sprintf("PR #%d merged (startup reconciliation)", pr.Number), pr.BeadID, pr.Anvil)
			_ = m.db.CompleteWorkersByBead(pr.BeadID)
			if err := ingot.UpdateIngotStatus(m.db.Conn(), pr.BeadID, pr.Anvil, ingot.StatusPRMerged); err != nil {
				log.Printf("[bellows] reconcileTerminalStates: failed to update ingot status to pr_merged for %s (%s): %v", pr.BeadID, pr.Anvil, err)
			}
		case status.IsClosed() && pr.Status != state.PRClosed && pr.Status != state.PRMerged:
			log.Printf("[bellows] reconcileTerminalStates: PR #%d (%s) GitHub=CLOSED, local=%s — correcting", pr.Number, pr.Anvil, pr.Status)
			_ = m.db.UpdatePRStatus(pr.ID, state.PRClosed)
			_ = m.db.LogEvent(state.EventPRClosed, fmt.Sprintf("PR #%d closed (startup reconciliation)", pr.Number), pr.BeadID, pr.Anvil)
			_ = m.db.CompleteWorkersByBead(pr.BeadID)
			if err := ingot.UpdateIngotStatus(m.db.Conn(), pr.BeadID, pr.Anvil, ingot.StatusFailed); err != nil {
				log.Printf("[bellows] reconcileTerminalStates: failed to update ingot status to failed for %s (%s): %v", pr.BeadID, pr.Anvil, err)
			}
		}
	}
}

// sweepOrphanedMonitoringWorkers transitions any bellows worker stuck in
// "monitoring" whose underlying PR is terminal (merged/closed) or missing
// from the prs table to "done". This covers the case where an unmanaged
// external PR's prs.status was persisted as merged/closed before it was
// flipped to bellows_managed=1: from that point on OpenPRs() never surfaces
// it again, so checkPR's CompleteWorkersByBead path cannot fire and the
// worker would otherwise remain in monitoring forever. Forge-managed PRs
// continue to transition via the existing CompleteWorkersByBead call in
// checkPR; the sweep is a no-op for them because the worker is already
// "done" before this pass runs.
func (m *Monitor) sweepOrphanedMonitoringWorkers() {
	orphans, err := m.db.OrphanedBellowsWorkers()
	if err != nil {
		log.Printf("[bellows] sweepOrphanedMonitoringWorkers: error listing orphaned workers: %v", err)
		return
	}
	for _, o := range orphans {
		reason := "no matching PR row"
		if o.PRStatus != "" {
			reason = fmt.Sprintf("PR status=%s", o.PRStatus)
		}
		log.Printf("[bellows] Sweeping orphaned monitoring worker %s (%s)", o.WorkerID, reason)
		if err := m.db.UpdateWorkerStatus(o.WorkerID, state.WorkerDone); err != nil {
			log.Printf("[bellows] sweepOrphanedMonitoringWorkers: failed to update worker %s to done: %v", o.WorkerID, err)
		}
	}
}

// checkAll polls all open PRs and emits events for state changes.
func (m *Monitor) checkAll(ctx context.Context) {
	m.sweepOrphanedMonitoringWorkers()

	// Runs before the open-PR query (and its len==0 early return) because the
	// work it drives — closing beads whose PR already merged — outlives the PRs
	// that triggered it: by the time a close is stuck and pending, the PR is no
	// longer open and would never bring us back here.
	if m.cycleHook != nil {
		m.cycleHook(ctx)
	}

	prs, err := m.db.OpenPRs()
	if err != nil {
		log.Printf("[bellows] Error listing open PRs: %v", err)
		return
	}

	if len(prs) == 0 {
		return
	}

	log.Printf("[bellows] Checking %d open PRs", len(prs))

	// Ensure a bellows worker entry exists for each managed PR so they appear
	// in the Hearth Workers panel. Uses INSERT OR IGNORE so we only write when
	// the row is genuinely new, avoiding unnecessary WAL churn on every poll.
	// Unmanaged external PRs (ext-* with bellows_managed=0) are display-only
	// in the PR panel and intentionally excluded from the Workers panel.
	for i := range prs {
		pr := &prs[i]
		if strings.HasPrefix(pr.BeadID, "ext-") && !pr.BellowsManaged {
			continue
		}
		workerID := fmt.Sprintf("bellows-%s-%d", pr.Anvil, pr.Number)
		title := pr.Title
		if title == "" {
			title = fmt.Sprintf("PR #%d", pr.Number)
		}
		// A detached PR keeps its row rather than being skipped — a PR that
		// disappears from the Workers panel reads as a bug, not as a mute —
		// but carries the detached status, because the row is the one place
		// the panel asserts bellows is watching, and for this PR it is not.
		workerStatus := state.WorkerMonitoring
		if pr.BellowsDetached {
			workerStatus = state.WorkerDetached
		}
		// Remove any stale pipeline worker row repurposed as bellows
		// (phase='bellows' but lacking a "bellows-" prefix in its ID).
		// Without this, Hearth shows two workers for the same PR.
		_ = m.db.DeletePipelineBellowsWorker(pr.BeadID, pr.Anvil)
		if err := m.db.InsertWorkerIfMissing(&state.Worker{
			ID:        workerID,
			BeadID:    pr.BeadID,
			Anvil:     pr.Anvil,
			Branch:    pr.Branch,
			Status:    workerStatus,
			Phase:     "bellows",
			Title:     title,
			PRNumber:  pr.Number,
			StartedAt: time.Now(),
		}); err != nil {
			log.Printf("[bellows] Failed to upsert worker row for PR #%d (%s): %v", pr.Number, pr.Anvil, err)
		}
		// The insert above only writes new rows, so a PR detached (or resumed)
		// after its row was created is reconciled here. The update is a no-op
		// unless the row's status actually disagrees with the flag.
		if err := m.db.SetBellowsWorkerDetached(workerID, pr.BellowsDetached); err != nil {
			log.Printf("[bellows] Failed to sync detached state on worker row for PR #%d (%s): %v", pr.Number, pr.Anvil, err)
		}
	}

	var dailyAssayCost *float64
	if m.assayConfig != nil {
		now := time.Now().UTC()
		dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		cost, err := m.db.AssayCostUSDSince(dayStart)
		if err != nil {
			log.Printf("[bellows] Failed to query daily Assay cost: %v", err)
		} else {
			dailyAssayCost = &cost
		}
	}

	for i := range prs {
		if ctx.Err() != nil {
			return
		}
		m.checkPR(ctx, &prs[i], dailyAssayCost)
	}
}

// suppress records that this PR is being passed over — external-and-unmanaged,
// or detached — and refreshes the only DB state bellows still owes it:
// mergeability plus terminal status. A passed-over PR is unwatched, not
// unknown, so the PR panel keeps telling the truth about it while nothing is
// emitted and no lifecycle worker is dispatched.
//
// The marker in seen is what the matching resumeFromSuppression call reads on
// the poll after the suppression lifts. A PR that has reached a terminal state
// hands the whole key to forgetPR instead: it leaves OpenPRs() for good, so
// nothing will ever consume any of the state bellows holds for it.
//
// The persistence is best-effort but never silent — a suppressed PR's
// mergeability and terminal status are the one thing bellows still owes it, so
// a failed write that leaves a merged PR being polled forever has to say so.
// A terminal write that fails also keeps the state: the PR is still open as far
// as the DB is concerned, so the next poll will find it and try again.
//
// Both suppression maps are only ever touched from checkPR, which runs on the
// single checkAll goroutine, so they need no lock of their own — unlike
// lastStatuses, which ResetPRState can reach concurrently.
func (m *Monitor) suppress(seen map[string]bool, key string, pr *state.PR, snap *prSnapshot, ciInProgress bool) {
	seen[key] = true
	if err := m.db.UpdatePRMergeability(pr.ID, snap.CIPassing && !ciInProgress, snap.IsConflicting, snap.HasUnresolvedThreads, snap.HasPendingReviews, snap.HasApproval, snap.AssayUpToDate); err != nil {
		log.Printf("[bellows] Failed to persist mergeability for passed-over PR #%d (%s): %v", pr.Number, pr.Anvil, err)
	}
	var terminal state.PRStatus
	switch {
	case snap.IsMerged:
		terminal = state.PRMerged
	case snap.IsClosed:
		terminal = state.PRClosed
	default:
		return
	}
	if err := m.db.UpdatePRStatus(pr.ID, terminal); err != nil {
		log.Printf("[bellows] Failed to persist %s status for passed-over PR #%d (%s): %v", terminal, pr.Number, pr.Anvil, err)
		return
	}
	m.forgetPR(key)
}

// forgetPR drops every per-PR entry this monitor holds for key: both
// suppression markers and the cached snapshot. It is called once a PR is known
// terminal, which is the moment all three become unreadable — the PR leaves
// OpenPRs() for good, so keeping them would grow each map by one entry per
// merged or closed PR for the daemon's lifetime.
//
// Both markers go, not only the one belonging to the regime that was
// suppressing this PR. The two are written by different branches of checkPR and
// an operator can hand a PR from one to the other: unassigning a detached ext-
// PR from bellows makes every later poll take the ext-unmanaged branch, which
// precedes the detached one, so the wasDetached marker is never read again.
//
// Dropping the snapshot with them is what makes a reopened PR re-seed. A closed
// PR can be reopened on GitHub and re-enter OpenPRs(); a snapshot left from
// before it closed recorded failing CI, conflicts and unresolved threads as
// already-seen, so the standing problems would never fire as transitions again.
func (m *Monitor) forgetPR(key string) {
	delete(m.wasUnmanaged, key)
	delete(m.wasDetached, key)
	m.mu.Lock()
	delete(m.lastStatuses, key)
	m.mu.Unlock()
}

// resumeFromSuppression reports whether key was suppressed by the given map on
// an earlier poll and, when it was, clears both the marker and the cached
// snapshot so the caller can re-enter checkPR and run the nil-snapshot seeding
// path. Every problem the PR accumulated while suppressed is then re-detected
// as a fresh transition rather than sitting there as state bellows believes it
// has already seen.
//
// Deleting the marker before the caller recurses is what bounds that recursion
// to a single re-entry.
func (m *Monitor) resumeFromSuppression(seen map[string]bool, key string) bool {
	if !seen[key] {
		return false
	}
	delete(seen, key)
	m.mu.Lock()
	delete(m.lastStatuses, key)
	m.mu.Unlock()
	return true
}

// checkPR polls a single PR and emits events for any state changes.
// dailyAssayCost is the precomputed global Assay cost for the current day,
// queried once per checkAll cycle to avoid redundant per-PR queries. A nil
// value means the query failed and the Assay gate should be skipped.
func (m *Monitor) checkPR(ctx context.Context, pr *state.PR, dailyAssayCost *float64) {
	m.pathsMu.RLock()
	anvilPath, ok := m.anvilPaths[pr.Anvil]
	m.pathsMu.RUnlock()
	if !ok {
		log.Printf("[bellows] Unknown anvil %s for PR #%d", pr.Anvil, pr.Number)
		return
	}

	if m.vcsLookup == nil {
		log.Printf("[bellows] No VCS provider configured; skipping status check for PR #%d", pr.Number)
		return
	}
	anvilVCS := m.vcsLookup(pr.Anvil)
	if anvilVCS == nil {
		log.Printf("[bellows] No VCS provider for anvil %s; skipping status check for PR #%d", pr.Anvil, pr.Number)
		return
	}
	var status *vcs.PRStatus
	err := m.retryTransient(ctx, fmt.Sprintf("CheckStatus PR #%d", pr.Number), func() error {
		var e error
		status, e = anvilVCS.CheckStatus(ctx, anvilPath, pr.Number)
		return e
	})
	if err != nil {
		log.Printf("[bellows] Error checking PR #%d: %v", pr.Number, err)
		return
	}

	// Persist title if it was missing or has changed (e.g. PRs created before this
	// fix, PRs that were renamed, or a fresh state.db that didn't carry over titles).
	if status.Title != "" && status.Title != pr.Title {
		if err := m.db.UpdatePRTitle(pr.ID, status.Title); err != nil {
			log.Printf("[bellows] Failed to backfill PR title for PR #%d (%s): %v", pr.Number, pr.Anvil, err)
		}
		workerID := fmt.Sprintf("bellows-%s-%d", pr.Anvil, pr.Number)
		if err := m.db.UpdateWorkerTitle(workerID, status.Title); err != nil {
			log.Printf("[bellows] Failed to backfill worker title for PR #%d (%s): %v", pr.Number, pr.Anvil, err)
		}
		pr.Title = status.Title
	}

	// Reduce the rollup to a verdict for the CURRENT head. Results carried over
	// from superseded commits, and heads whose runs have not finished (or never
	// started), stay out of the failure path entirely — see evaluateCI.
	ci := evaluateCI(status.HeadSHA, status.StatusCheckRollup, m.nowFn())
	ciInProgress := ci.inProgress()
	newSnap := &prSnapshot{
		CIPassing:            ci.passing(),
		CIInProgress:         ciInProgress,
		HasApproval:          status.HasApproval(),
		NeedsChanges:         status.NeedsChanges(),
		HasUnresolvedThreads: status.UnresolvedThreads > 0,
		HasPendingReviews:    status.HasPendingReviewRequests(),
		IsMerged:             status.IsMerged(),
		IsClosed:             status.IsClosed(),
		IsConflicting:        status.Mergeable == "CONFLICTING",
	}

	// Whether the current head has been reviewed by Assay (or Assay is disabled
	// for the anvil). Gates ready-to-merge so a PR is not declared ready while an
	// Assay review is pending or in-flight for this head (Forge-75cx). Computed
	// before the snapshot lock so it can also seed lastSnap on first sighting.
	// When dailyAssayCost is nil (query error), no Assay will be dispatched this
	// cycle, so treat as up-to-date to avoid permanently blocking readiness for
	// an Assay that cannot run.
	assayUpToDate := m.assayUpToDate(pr, status.HeadSHA, dailyAssayCost)
	if dailyAssayCost == nil && m.assayEnabled(pr.Anvil) {
		assayUpToDate = true
	}
	newSnap.AssayUpToDate = assayUpToDate

	switch ci.State {
	case ciStuck:
		// A wedged run or a platform outage. Surface it once for the operator
		// instead of feeding a fix loop that cannot reproduce anything. This
		// evaluation runs ahead of the detached guard below (the CI verdict
		// feeds the snapshot either way), so the note — a needs-attention entry
		// plus an event — is suppressed here rather than there.
		log.Printf("[bellows] PR #%d (%s): CI appears stuck/outage — %s; skipping failure evaluation",
			pr.Number, pr.Anvil, ci.Reason)
		if !pr.BellowsDetached {
			m.noteCIStuck(pr, ci)
		}
	case ciPending:
		log.Printf("[bellows] PR #%d (%s): CI pending — %s; skipping failure evaluation",
			pr.Number, pr.Anvil, ci.Reason)
		m.clearCIStuck(pr)
	default:
		if ci.StaleChecks > 0 {
			log.Printf("[bellows] PR #%d (%s): ignored %d check result(s) from superseded commits; CI %s — %s",
				pr.Number, pr.Anvil, ci.StaleChecks, ci.State, ci.Reason)
		}
		m.clearCIStuck(pr)
	}

	// Detect transitions and emit events. We re-acquire the lock and re-check the
	// last status to ensure a concurrent ResetPRState call hasn't cleared it.
	m.mu.Lock()
	key := fmt.Sprintf("%s/%d", pr.Anvil, pr.Number)
	lastSnap := m.lastStatuses[key]
	if lastSnap == nil {
		// Seed from the DB's last persisted state so that daemon restarts correctly
		// detect the ready-to-merge transition for PRs that became ready while the
		// daemon was down. Using the DB state means lastReady = newReady only if the PR
		// was ALREADY ready at the previous poll — preventing both spurious re-fires on
		// restart AND missed notifications when the state changed between restarts.
		// For brand-new PRs (has_approval=0, has_pending_reviews=1 from InsertPR defaults)
		// lastReady will be false, so the transition fires correctly on first readiness.
		// If CI is failing but no fix cycle is in progress and no fix has
		// been attempted, seed CIPassing as true so the first poll detects
		// a true→false transition and emits EventCIFailed. Without this,
		// PRs whose CI failed before the daemon started (or between restarts)
		// would never trigger a quench worker.
		ciSeed := pr.CIPassing
		if !ciSeed && pr.Status != state.PRNeedsFix && pr.CIFixCount == 0 {
			ciSeed = true
		}
		// Seed unresolved threads/conflicts as false when no fix has been
		// attempted yet, so the first poll detects a false→true transition
		// and triggers a burnish/rebase worker. Without this, PRs that
		// already had issues when assigned to bellows would never fire.
		threadsSeed := pr.HasUnresolvedThreads
		if threadsSeed && pr.Status != state.PRNeedsFix && pr.ReviewFixCount == 0 {
			threadsSeed = false
		}
		conflictSeed := pr.IsConflicting
		if conflictSeed && pr.Status != state.PRNeedsFix && pr.RebaseCount == 0 {
			conflictSeed = false
		}
		lastSnap = &prSnapshot{
			CIPassing:            ciSeed,
			HasApproval:          pr.HasApproval,
			HasPendingReviews:    pr.HasPendingReviews,
			IsConflicting:        conflictSeed,
			HasUnresolvedThreads: threadsSeed,
			// PRStatus.NeedsChanges() returns true when unresolved threads exist,
			// so without seeding this in lockstep with threadsSeed the primary
			// review-changes branch would re-fire on the first poll for any PR
			// already in needs_fix or post-fix state — masking the secondary
			// still-unresolved retry branch and dispatching duplicates.
			NeedsChanges: threadsSeed,
			// Seed the assay term as "up to date" only when Assay is disabled for
			// the anvil, preserving the existing restart behaviour there. When Assay
			// is enabled, seed it false so the ready transition fires on the rising
			// edge once the head is assayed. This matters because the Assay worker
			// calls ResetPRState when it finishes, forcing a re-seed on the next
			// poll: seeding false keeps lastReady=false so a clean assay still emits
			// EventPRReadyToMerge (and triggers auto-merge) rather than being masked
			// by the DB's already-green mergeability state (Forge-75cx).
			AssayUpToDate: !m.assayEnabled(pr.Anvil),
		}
	}
	// When CI is still in progress, preserve the last *completed* CIPassing value
	// so that a pending→completed-failure transition still fires on the next poll.
	// Without this, the snapshot would record CIPassing=false while in-progress,
	// and when checks finish failing, the transition branch (!new && old) would not
	// fire because old.CIPassing is already false.
	if ciInProgress {
		newSnap.CIPassing = lastSnap.CIPassing
	}
	// Update snapshot while holding the lock
	m.lastStatuses[key] = newSnap
	m.mu.Unlock()

	// When an external PR is newly assigned to bellows, clear the cached
	// snapshot so the seeding logic runs fresh and detects pre-existing
	// issues (unresolved threads, CI failures, conflicts) as transitions.
	if strings.HasPrefix(pr.BeadID, "ext-") && pr.BellowsManaged {
		if m.resumeFromSuppression(m.wasUnmanaged, key) {
			// Re-enter checkPR so the nil-snapshot seeding path runs.
			m.checkPR(ctx, pr, dailyAssayCost)
			return
		}
	}

	// Same treatment when a detached PR is resumed. The snapshots taken while
	// detached recorded every standing problem as already-seen, so without this
	// a PR resumed with failing CI, conflicts or unresolved threads would sit
	// there as unchanged state and never fire again: the transition happened
	// while nobody was listening. Clearing the snapshot re-seeds from the DB, so
	// the problems that outlived the mute are re-detected as fresh transitions.
	if !pr.BellowsDetached {
		if m.resumeFromSuppression(m.wasDetached, key) {
			// Re-enter checkPR so the nil-snapshot seeding path runs.
			m.checkPR(ctx, pr, dailyAssayCost)
			return
		}
	}

	// External PRs (not created by Forge) are tracked for display in the
	// Hearth PR panel. Unless explicitly assigned to bellows, they must not
	// trigger lifecycle workers (quench, burnish, rebase). Persist their
	// mergeability state and return early.
	if strings.HasPrefix(pr.BeadID, "ext-") && !pr.BellowsManaged {
		m.suppress(m.wasUnmanaged, key, pr, newSnap, ciInProgress)
		return
	}

	// A detached PR is muted, not untracked: an operator took it off bellows,
	// so it emits nothing and drives no lifecycle worker (quench, burnish,
	// rebase, Assay), but its mergeability and terminal state are still
	// refreshed so the PR panel keeps showing the truth about it. Independent
	// of the managed flags above — any PR can be detached, Forge-created or
	// external, managed or not.
	//
	// This one return is what silences every branch below, and the steady-state
	// re-emit branches ("CI still failing", "still conflicting", "still
	// unresolved threads") are the reason it has to be a return rather than a
	// per-emit check: those fire on unchanged state, so a detached PR with a
	// standing problem would otherwise re-announce it on every single poll.
	if pr.BellowsDetached {
		m.suppress(m.wasDetached, key, pr, newSnap, ciInProgress)
		return
	}

	// Terminal-state detection: GitHub is the source of truth. The persisted
	// local status (open/needs_fix/approved) is advisory and must NOT block
	// recognising a real merge — a human can merge a needs_fix PR as-is on
	// GitHub, and we have to honour that. Gating on pr.Status (the DB value)
	// rather than the in-memory snapshot also makes the transition robust to
	// daemon restarts and any prior write that left the snapshot stale.
	//
	// We return early after firing so that the subsequent CI/review/conflict
	// branches below cannot overwrite the terminal status back to needs_fix.
	if newSnap.IsMerged && pr.Status != state.PRMerged {
		m.emit(ctx, PREvent{
			PRNumber:  pr.Number,
			BeadID:    pr.BeadID,
			Anvil:     pr.Anvil,
			Branch:    status.HeadRefName,
			EventType: EventPRMerged,
			Details:   fmt.Sprintf("PR #%d has been merged", pr.Number),
			Timestamp: time.Now(),
		})
		_ = m.db.UpdatePRStatus(pr.ID, state.PRMerged)
		_ = m.db.LogEvent(state.EventPRMerged, fmt.Sprintf("PR #%d merged", pr.Number), pr.BeadID, pr.Anvil)
		_ = m.db.CompleteWorkersByBead(pr.BeadID)
		// Same cleanup the suppressed path does: the PR is terminal, so the
		// snapshot just written above is the last thing that will ever read it.
		m.forgetPR(key)

		// Best-effort ingot lifecycle update
		if err := ingot.UpdateIngotStatus(m.db.Conn(), pr.BeadID, pr.Anvil, ingot.StatusPRMerged); err != nil {
			log.Printf("[bellows] Failed to update ingot status to pr_merged for %s (%s): %v", pr.BeadID, pr.Anvil, err)
		}

		if m.autoLearnRules != nil && m.autoLearnRules() {
			anvilMu := m.getLearnMu(pr.Anvil)
			prNum := pr.Number
			go func() {
				// Acquire the concurrency semaphore, but bail on shutdown.
				select {
				case m.learnSem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-m.learnSem }()
				// Bound the gh/claude subprocesses so a hang cannot hold
				// the semaphore or per-anvil mutex indefinitely.
				learnCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
				defer cancel()
				// Serialize per-anvil so load→add→save is atomic per repo.
				anvilMu.Lock()
				defer anvilMu.Unlock()
				m.learnRulesFromPR(learnCtx, pr.Anvil, anvilPath, pr.BeadID, prNum)
			}()
		}
		return
	} else if newSnap.IsClosed && pr.Status != state.PRClosed && pr.Status != state.PRMerged {
		m.emit(ctx, PREvent{
			PRNumber:  pr.Number,
			BeadID:    pr.BeadID,
			Anvil:     pr.Anvil,
			Branch:    status.HeadRefName,
			EventType: EventPRClosed,
			Details:   fmt.Sprintf("PR #%d has been closed", pr.Number),
			Timestamp: time.Now(),
		})
		_ = m.db.UpdatePRStatus(pr.ID, state.PRClosed)
		_ = m.db.LogEvent(state.EventPRClosed, fmt.Sprintf("PR #%d closed without merge", pr.Number), pr.BeadID, pr.Anvil)
		_ = m.db.CompleteWorkersByBead(pr.BeadID)
		// A closed PR can be reopened, so forgetting it is not just hygiene:
		// it is what makes the reopened PR re-seed instead of coming back with
		// every standing problem already marked as seen.
		m.forgetPR(key)

		// Best-effort ingot lifecycle update
		if err := ingot.UpdateIngotStatus(m.db.Conn(), pr.BeadID, pr.Anvil, ingot.StatusFailed); err != nil {
			log.Printf("[bellows] Failed to update ingot status to failed for %s (%s): %v", pr.BeadID, pr.Anvil, err)
		}
		return
	}

	if newSnap.CIPassing && !lastSnap.CIPassing {
		m.emit(ctx, PREvent{
			PRNumber:  pr.Number,
			BeadID:    pr.BeadID,
			Anvil:     pr.Anvil,
			Branch:    status.HeadRefName,
			EventType: EventCIPassed,
			Details:   "All CI checks passed",
			Timestamp: time.Now(),
		})
	} else if !newSnap.CIPassing && !newSnap.CIInProgress && lastSnap.CIPassing {
		// Only flag CI as failed when all checks have completed and at least
		// one has a non-success conclusion. If any checks are still in
		// progress we wait for the next poll cycle.
		m.emit(ctx, PREvent{
			PRNumber:  pr.Number,
			BeadID:    pr.BeadID,
			Anvil:     pr.Anvil,
			Branch:    status.HeadRefName,
			EventType: EventCIFailed,
			Details:   "CI checks failed",
			Timestamp: time.Now(),
		})
		_ = m.db.UpdatePRStatus(pr.ID, state.PRNeedsFix)
		_ = m.db.LogEvent(state.EventCIFailed, fmt.Sprintf("PR #%d CI checks failed", pr.Number), pr.BeadID, pr.Anvil)
		_ = m.db.LogEvent(state.EventPRNeedsFix, fmt.Sprintf("PR #%d CI failed", pr.Number), pr.BeadID, pr.Anvil)
	} else if !newSnap.CIPassing && !newSnap.CIInProgress && !lastSnap.CIPassing {
		// CI is still failing with no transition. Re-emit when no quench worker
		// is currently in flight for the bead and retries remain. This catches
		// the gap where NotifyCIFixCompleted() clears the fix state but bellows
		// never re-emits EventCIFailed because it only detected transitions —
		// AND the gap where the PR is parked in needs_fix with no active worker
		// (dropped parked action / mid-fix restart), which the old
		// `pr.Status != needs_fix` proxy left wedged forever.
		// Skip if checks are still in progress — wait for completion.
		maxCI := m.maxCIFixAttempts()
		if !m.inFlight(pr.BeadID) {
			if pr.CIFixCount > 0 && pr.CIFixCount < maxCI {
				m.emit(ctx, PREvent{
					PRNumber:  pr.Number,
					BeadID:    pr.BeadID,
					Anvil:     pr.Anvil,
					Branch:    status.HeadRefName,
					EventType: EventCIFailed,
					Details:   fmt.Sprintf("CI checks still failing after fix attempt %d/%d", pr.CIFixCount, maxCI),
					Timestamp: time.Now(),
				})
				_ = m.db.UpdatePRStatus(pr.ID, state.PRNeedsFix)
				_ = m.db.LogEvent(state.EventCIFailed, fmt.Sprintf("PR #%d CI still failing (attempt %d/%d)", pr.Number, pr.CIFixCount, maxCI), pr.BeadID, pr.Anvil)
				_ = m.db.LogEvent(state.EventPRNeedsFix, fmt.Sprintf("PR #%d CI fix retry needed", pr.Number), pr.BeadID, pr.Anvil)
			} else if pr.CIFixCount >= maxCI {
				// Quench used all its retries and CI is still failing: no more
				// auto-fixes will dispatch. Surface it instead of stalling silently.
				m.escalateFixExhausted(pr, fmt.Sprintf("CI-fix loop exhausted after %d/%d attempts; CI still failing", pr.CIFixCount, maxCI))
			}
		}
	}

	if newSnap.HasApproval && !lastSnap.HasApproval {
		m.emit(ctx, PREvent{
			PRNumber:  pr.Number,
			BeadID:    pr.BeadID,
			Anvil:     pr.Anvil,
			Branch:    status.HeadRefName,
			EventType: EventReviewApproved,
			Details:   "PR received approval",
			Timestamp: time.Now(),
		})
		_ = m.db.UpdatePRStatus(pr.ID, state.PRApproved)
	}

	// Detect merge conflicts (CONFLICTING → fire event so operator / lifecycle can rebase)
	if newSnap.IsConflicting && !lastSnap.IsConflicting {
		m.emit(ctx, PREvent{
			PRNumber:  pr.Number,
			BeadID:    pr.BeadID,
			Anvil:     pr.Anvil,
			Branch:    status.HeadRefName,
			EventType: EventPRConflicting,
			Details:   fmt.Sprintf("PR #%d has merge conflicts with base branch", pr.Number),
			Timestamp: time.Now(),
		})
		_ = m.db.UpdatePRStatus(pr.ID, state.PRNeedsFix)
		_ = m.db.LogEvent(state.EventPRConflicting,
			fmt.Sprintf("PR #%d: merge conflict detected", pr.Number),
			pr.BeadID, pr.Anvil)
	} else if newSnap.IsConflicting && lastSnap.IsConflicting {
		// PR is still conflicting with no transition. Catches the gap where a
		// rebase action dispatched but failed at the prep step (e.g. transient
		// git fetch failure) or where rebase itself failed and the PR stayed
		// CONFLICTING. Without this, the seeding guard's RebaseCount > 0 clause
		// keeps lastSnap.IsConflicting=true forever and no new event ever fires.
		// Mirrors the CI still-failing and review still-unresolved branches.
		maxR := m.maxRebaseAttempts()
		if !m.inFlight(pr.BeadID) && pr.RebaseCount > 0 && pr.RebaseCount < maxR {
			m.emit(ctx, PREvent{
				PRNumber:  pr.Number,
				BeadID:    pr.BeadID,
				Anvil:     pr.Anvil,
				Branch:    status.HeadRefName,
				EventType: EventPRConflicting,
				Details:   fmt.Sprintf("PR still has merge conflicts after rebase attempt %d/%d", pr.RebaseCount, maxR),
				Timestamp: time.Now(),
			})
			_ = m.db.UpdatePRStatus(pr.ID, state.PRNeedsFix)
			_ = m.db.LogEvent(state.EventPRConflicting,
				fmt.Sprintf("PR #%d rebase retry needed (attempt %d/%d)", pr.Number, pr.RebaseCount, maxR),
				pr.BeadID, pr.Anvil)
		}
	}

	// Trigger on "CHANGES_REQUESTED" or transition from 0 to >0 unresolved threads (Bug 1)
	if (newSnap.NeedsChanges && !lastSnap.NeedsChanges) || (newSnap.HasUnresolvedThreads && !lastSnap.HasUnresolvedThreads) {
		details := "PR has changes requested"
		if newSnap.HasUnresolvedThreads && !lastSnap.HasUnresolvedThreads {
			details = "PR has unresolved review threads"
		}
		m.emit(ctx, PREvent{
			PRNumber:  pr.Number,
			BeadID:    pr.BeadID,
			Anvil:     pr.Anvil,
			Branch:    status.HeadRefName,
			EventType: EventReviewChanges,
			Details:   details,
			Timestamp: time.Now(),
		})
		_ = m.db.UpdatePRStatus(pr.ID, state.PRNeedsFix)
		_ = m.db.LogEvent(state.EventReviewChanges, fmt.Sprintf("PR #%d: %s", pr.Number, details), pr.BeadID, pr.Anvil)
		_ = m.db.LogEvent(state.EventPRNeedsFix, fmt.Sprintf("PR #%d: review fix needed", pr.Number), pr.BeadID, pr.Anvil)
	} else if newSnap.HasUnresolvedThreads && lastSnap.HasUnresolvedThreads {
		// Review still has unresolved threads with no transition. Re-emit when no
		// burnish worker is in flight for the bead and retries remain. Catches the
		// gap where NotifyReviewFixCompleted() resets pr.Status to open after a
		// burnish cycle but bellows never re-emits because threads stayed > 0
		// across the cycle — AND the gap where the PR is parked in needs_fix with
		// no active worker (a dropped parked review-fix action, or a mid-fix
		// restart), which the old `pr.Status != needs_fix` proxy wedged forever
		// (observed: PR #4257 / Fhi.Metadata-hyc4g). Mirrors the CI-fix branch above.
		maxR := m.maxReviewFixAttempts()
		if !m.inFlight(pr.BeadID) {
			if pr.ReviewFixCount > 0 && pr.ReviewFixCount < maxR {
				m.emit(ctx, PREvent{
					PRNumber:  pr.Number,
					BeadID:    pr.BeadID,
					Anvil:     pr.Anvil,
					Branch:    status.HeadRefName,
					EventType: EventReviewChanges,
					Details:   fmt.Sprintf("PR still has unresolved review threads after fix attempt %d/%d", pr.ReviewFixCount, maxR),
					Timestamp: time.Now(),
				})
				_ = m.db.UpdatePRStatus(pr.ID, state.PRNeedsFix)
				_ = m.db.LogEvent(state.EventReviewChanges, fmt.Sprintf("PR #%d review fix retry needed (attempt %d/%d)", pr.Number, pr.ReviewFixCount, maxR), pr.BeadID, pr.Anvil)
				_ = m.db.LogEvent(state.EventPRNeedsFix, fmt.Sprintf("PR #%d review fix retry needed", pr.Number), pr.BeadID, pr.Anvil)
			} else if pr.ReviewFixCount >= maxR {
				// Burnish used all its retries and threads are still unresolved:
				// no more auto-fixes will dispatch. Surface it for triage instead
				// of stalling silently (PR #4471 / Fhi.Metadata-eyizi).
				m.escalateFixExhausted(pr, fmt.Sprintf("review-fix loop exhausted after %d/%d attempts; unresolved review threads remain", pr.ReviewFixCount, maxR))
			}
		}
	}

	// If all merge-readiness conditions are met and the PR was in needs_fix,
	// restore it to approved so the Ready-to-Merge panel picks it up again.
	// Note: HasApproval is intentionally excluded — Copilot only submits
	// COMMENTED reviews, never APPROVED, so requiring it would prevent PRs
	// from ever reaching the Ready-to-Merge state.
	// ciInProgress is excluded: a PR is not truly ready while CI is still running.
	// assayUpToDate is required so the status is not restored to approved while an
	// Assay review is still pending/in-flight for the current head (Forge-75cx).
	if newSnap.CIPassing && !ciInProgress && !newSnap.IsConflicting && !newSnap.HasUnresolvedThreads && !newSnap.HasPendingReviews && assayUpToDate {
		_ = m.db.UpdatePRStatusIfNeedsFix(pr.ID, state.PRApproved)
	}

	// Assay trigger gate: emit EventPRReviewNeeded when the current head has not
	// yet been reviewed and all gating conditions hold (see
	// shouldEmitReviewNeeded). Assay reviews on first sighting regardless of CI
	// state — like Copilot — so its findings are available early enough to feed
	// the Burnish fix loop. No-op unless an Assay config accessor is registered.
	m.maybeEmitReviewNeeded(ctx, pr, status, newSnap, dailyAssayCost)

	// Detect transition to fully ready-to-merge state (CI passing +
	// no conflicts, unresolved threads, or pending reviews).
	// This matches the ReadyToMergePRs query in state/db.go.
	// ciInProgress is excluded from both sides: a PR is not ready while CI is
	// still running, and the previous poll must also have been fully completed
	// (not in-progress) to count as "was already ready".
	// assayUpToDate gates both sides so the rising-edge transition fires only once
	// the current head has been assayed (or Assay is disabled). Without it, a PR
	// with green CI and no threads would be announced ready on first sighting,
	// before its in-flight Assay posts findings — bouncing it back to Burnish a
	// poll or two later (Forge-75cx). Tracking it per snapshot keeps lastReady
	// honest so the transition still fires exactly once when the assay lands.
	newReady := newSnap.CIPassing && !ciInProgress && !newSnap.IsConflicting && !newSnap.HasUnresolvedThreads && !newSnap.HasPendingReviews && newSnap.AssayUpToDate
	lastReady := lastSnap.CIPassing && !lastSnap.CIInProgress && !lastSnap.IsConflicting && !lastSnap.HasUnresolvedThreads && !lastSnap.HasPendingReviews && lastSnap.AssayUpToDate
	if newReady && !lastReady {
		m.emit(ctx, PREvent{
			PRNumber:  pr.Number,
			BeadID:    pr.BeadID,
			Anvil:     pr.Anvil,
			Branch:    status.HeadRefName,
			EventType: EventPRReadyToMerge,
			Details:   fmt.Sprintf("PR #%d is ready to merge (CI passing, no blocking reviews)", pr.Number),
			Timestamp: time.Now(),
			PRURL:     status.URL,
		})
		_ = m.db.LogEvent(state.EventPRReadyToMerge,
			fmt.Sprintf("PR #%d ready to merge", pr.Number),
			pr.BeadID, pr.Anvil)

		// Trigger auto-merge if configured for this anvil.
		if m.autoMergeHandler != nil {
			m.autoMergeHandler(ctx, pr.Anvil, *pr)
		}
	}

	// Persist mergeability state so the ready-to-merge panel stays current.
	// Include ci_passing so the Ready to Merge panel reflects the latest CI
	// status every poll cycle, not just on CI transition events.
	// Use !ciInProgress: when checks are still running, ci_passing must be
	// false in the DB so the Ready-to-Merge panel does not show the PR
	// prematurely. (newSnap.CIPassing may have been overridden to preserve
	// the last completed value for transition detection — see above.)
	_ = m.db.UpdatePRMergeability(pr.ID, newSnap.CIPassing && !ciInProgress, newSnap.IsConflicting, newSnap.HasUnresolvedThreads, newSnap.HasPendingReviews, newSnap.HasApproval, newSnap.AssayUpToDate)

}

// ResetPRState clears the internal status cache for a PR. This should be called
// when a PR is manually reset so that status changes (e.g. from failing back
// to passing) are re-detected on the next poll cycle even if the state
// is the same as it was before the reset.
func (m *Monitor) ResetPRState(anvil string, prNumber int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fmt.Sprintf("%s/%d", anvil, prNumber)
	delete(m.lastStatuses, key)
}

// getLearnMu returns the per-anvil mutex used to serialize auto-learn operations,
// creating it on first use.
func (m *Monitor) getLearnMu(anvil string) *sync.Mutex {
	m.learnMuGuard.Lock()
	defer m.learnMuGuard.Unlock()
	if m.learnMu[anvil] == nil {
		m.learnMu[anvil] = &sync.Mutex{}
	}
	return m.learnMu[anvil]
}

// learnRulesFromPR fetches Copilot review comments from a merged PR,
// distills them into warden rules, and creates a PR with the updated rules
// file so the changes are reviewable. The caller is responsible for holding
// the per-anvil learn mutex so that concurrent learns don't race.
func (m *Monitor) learnRulesFromPR(ctx context.Context, anvilName, anvilPath, beadID string, prNumber int) {
	if ctx.Err() != nil {
		return
	}
	if m.vcsLookup == nil {
		log.Printf("[bellows] No VCS provider configured; skipping auto-learn for PR #%d", prNumber)
		return
	}
	anvilVCS := m.vcsLookup(anvilName)
	if anvilVCS == nil {
		log.Printf("[bellows] No VCS provider for anvil %s; skipping auto-learn for PR #%d", anvilName, prNumber)
		return
	}

	var comments []warden.PRComment
	err := m.retryTransient(ctx, fmt.Sprintf("FetchCopilotComments PR #%d", prNumber), func() error {
		var fetchErr error
		comments, fetchErr = warden.FetchCopilotComments(ctx, anvilPath, prNumber)
		return fetchErr
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Printf("[bellows] Auto-learn: error fetching Copilot comments for PR #%d (%s): %v", prNumber, anvilName, err)
		_ = m.db.LogEvent(state.EventAutoLearnError, fmt.Sprintf("PR #%d: failed to fetch Copilot comments: %v", prNumber, err), "", anvilName)
		return
	}
	if len(comments) == 0 {
		log.Printf("[bellows] Auto-learn: no Copilot comments on PR #%d (%s), skipping", prNumber, anvilName)
		_ = m.db.LogEvent(state.EventAutoLearnSkipped, fmt.Sprintf("%s PR #%d: no Copilot comments, skipping auto-learn", anvilName, prNumber), beadID, anvilName)
		return
	}

	groups := warden.GroupComments(comments)

	// Create a temporary worktree to prepare the rules update branch.
	branchName := fmt.Sprintf("forge/warden-learn-%d", prNumber)
	wtPath := filepath.Join(anvilPath, ".workers", fmt.Sprintf("warden-learn-%d", prNumber))

	defer func() {
		// Clean up worktree and local branch (best effort).
		// Unlink junctions/symlinks first so removal doesn't destroy target content.
		worktree.UnlinkReparsePoints(wtPath)
		worktree.ProbeNodeModules("bellows-before-worktree-remove", anvilPath)
		_ = bellowsGit(ctx, anvilPath, "worktree", "remove", "--force", wtPath)
		worktree.ProbeNodeModules("bellows-after-worktree-remove", anvilPath)
		worktree.ProbeNodeModules("bellows-before-worktree-prune", anvilPath)
		_ = bellowsGit(ctx, anvilPath, "worktree", "prune")
		worktree.ProbeNodeModules("bellows-after-worktree-prune", anvilPath)
		worktree.ProbeNodeModules("bellows-before-branch-delete", anvilPath)
		_ = bellowsGit(ctx, anvilPath, "branch", "-D", branchName)
		worktree.ProbeNodeModules("bellows-after-branch-delete", anvilPath)
		worktree.ProbeNodeModules("bellows-before-removeall", anvilPath)
		_ = os.RemoveAll(wtPath)
		worktree.ProbeNodeModules("bellows-after-removeall", anvilPath)
	}()

	// Fetch and resolve base ref.
	if err := bellowsGit(ctx, anvilPath, "fetch", "origin"); err != nil {
		log.Printf("[bellows] Auto-learn: git fetch failed for %s: %v", anvilName, err)
		_ = m.db.LogEvent(state.EventAutoLearnError, fmt.Sprintf("PR #%d: git fetch failed: %v", prNumber, err), "", anvilName)
		return
	}

	baseRef, err := resolveBaseRef(ctx, anvilPath)
	if err != nil {
		log.Printf("[bellows] Auto-learn: no base branch for %s: %v", anvilName, err)
		_ = m.db.LogEvent(state.EventAutoLearnError, fmt.Sprintf("PR #%d: no base branch found: %v", prNumber, err), "", anvilName)
		return
	}

	// Create the .workers directory and worktree.
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		log.Printf("[bellows] Auto-learn: failed to create workers dir for %s: %v", anvilName, err)
		return
	}

	if err := bellowsGit(ctx, anvilPath, "worktree", "add", "-f", "-b", branchName, wtPath, baseRef); err != nil {
		log.Printf("[bellows] Auto-learn: worktree add failed for %s: %v", anvilName, err)
		_ = m.db.LogEvent(state.EventAutoLearnError, fmt.Sprintf("PR #%d: worktree creation failed: %v", prNumber, err), "", anvilName)
		return
	}

	// Load existing rules from the worktree (reflects main branch state).
	rf, err := warden.LoadRules(wtPath)
	if err != nil {
		log.Printf("[bellows] Auto-learn: error loading existing rules for %s: %v", anvilName, err)
		_ = m.db.LogEvent(state.EventAutoLearnError, fmt.Sprintf("PR #%d: failed to load rules: %v", prNumber, err), "", anvilName)
		return
	}

	added := 0
	distillErrors := 0
	var newRules []warden.Rule
	for _, group := range groups {
		if ctx.Err() != nil {
			return
		}
		rule, err := warden.DistillRule(ctx, group, wtPath)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[bellows] Auto-learn: error distilling rule from PR #%d (%s): %v", prNumber, anvilName, err)
			distillErrors++
			continue
		}
		if rf.AddRule(*rule) {
			added++
			newRules = append(newRules, *rule)
		}
	}

	if added == 0 {
		if distillErrors > 0 {
			log.Printf("[bellows] Auto-learn: no new rules from PR #%d (%s); %d distillation error(s)", prNumber, anvilName, distillErrors)
			_ = m.db.LogEvent(state.EventAutoLearnError, fmt.Sprintf("%s PR #%d: no new rules — %d of %d comment group(s) failed to distill", anvilName, prNumber, distillErrors, len(groups)), beadID, anvilName)
		} else {
			log.Printf("[bellows] Auto-learn: no new rules from PR #%d (%s)", prNumber, anvilName)
			_ = m.db.LogEvent(state.EventAutoLearnSkipped, fmt.Sprintf("%s PR #%d: no new rules from %d comment(s)", anvilName, prNumber, len(comments)), beadID, anvilName)
		}
		return
	}

	// When smelter is enabled, insert rules into the pending table for batch
	// processing instead of creating an immediate PR.
	if m.smelterEnabled != nil && m.smelterEnabled() {
		if err := warden.InsertRulesAsPending(newRules, anvilName, m.db.InsertPendingRule); err != nil {
			log.Printf("[bellows] Auto-learn: error inserting pending rules for %s: %v", anvilName, err)
			_ = m.db.LogEvent(state.EventAutoLearnError, fmt.Sprintf("PR #%d: failed to insert pending rules: %v", prNumber, err), beadID, anvilName)
			return
		}
		log.Printf("[bellows] Auto-learn: inserted %d pending rule(s) from PR #%d (%s)", added, prNumber, anvilName)
		_ = m.db.LogEvent(state.EventWardenRuleLearned, fmt.Sprintf("PR #%d: learned %d rule(s), inserted as pending", prNumber, added), beadID, anvilName)
		return
	}

	// Save rules to the worktree, then commit, push, and create a PR.
	if err := warden.SaveRules(wtPath, rf); err != nil {
		log.Printf("[bellows] Auto-learn: error saving rules for %s: %v", anvilName, err)
		_ = m.db.LogEvent(state.EventAutoLearnError, fmt.Sprintf("PR #%d: failed to save rules: %v", prNumber, err), "", anvilName)
		return
	}

	if err := bellowsGit(ctx, wtPath, "add", warden.RulesFileName); err != nil {
		log.Printf("[bellows] Auto-learn: git add failed for %s: %v", anvilName, err)
		_ = m.db.LogEvent(state.EventAutoLearnError, fmt.Sprintf("PR #%d: git add failed: %v", prNumber, err), "", anvilName)
		return
	}

	commitMsg := fmt.Sprintf("forge: learn %d warden rule(s) from PR #%d", added, prNumber)
	if err := bellowsGit(ctx, wtPath, "commit", "-m", commitMsg); err != nil {
		log.Printf("[bellows] Auto-learn: git commit failed for %s: %v", anvilName, err)
		_ = m.db.LogEvent(state.EventAutoLearnError, fmt.Sprintf("PR #%d: git commit failed: %v", prNumber, err), "", anvilName)
		return
	}

	if err := bellowsGit(ctx, wtPath, "push", "-u", "origin", branchName); err != nil {
		log.Printf("[bellows] Auto-learn: git push failed for %s: %v", anvilName, err)
		_ = m.db.LogEvent(state.EventAutoLearnError, fmt.Sprintf("PR #%d: git push failed: %v", prNumber, err), "", anvilName)
		return
	}

	// Create a reviewable PR with the learned rules.
	prBody := fmt.Sprintf(
		"## Warden Rule Learning\n\n"+
			"Learned **%d** new review rule(s) from Copilot comments on PR #%d.\n\n"+
			"These rules will be applied by Warden during future code reviews.\n"+
			"Review the rules in `%s` and merge if they look correct.\n\n"+
			"---\n*Generated by [The Forge](https://github.com/Robin831/Forge) auto-learn*",
		added, prNumber, warden.RulesFileName,
	)

	pr, err := anvilVCS.CreatePR(ctx, vcs.CreateParams{
		WorktreePath: wtPath,
		Title:        fmt.Sprintf("forge: learn %d warden rule(s) from PR #%d [no-changelog]", added, prNumber),
		Body:         prBody,
		Branch:       branchName,
		AnvilName:    anvilName,
	})
	if err != nil {
		log.Printf("[bellows] Auto-learn: PR creation failed for %s: %v", anvilName, err)
		_ = m.db.LogEvent(state.EventAutoLearnError, fmt.Sprintf("PR #%d: rule PR creation failed: %v", prNumber, err), "", anvilName)
		return
	}

	log.Printf("[bellows] Auto-learn: created PR #%d with %d new rule(s) from PR #%d (%s)", pr.Number, added, prNumber, anvilName)
	_ = m.db.LogEvent(state.EventWardenRuleLearned, fmt.Sprintf("PR #%d: learned %d rule(s), created PR #%d", prNumber, added, pr.Number), "", anvilName)
}

// resolveBaseRef determines whether the repo uses origin/main or origin/master.
func resolveBaseRef(ctx context.Context, repoPath string) (string, error) {
	if err := bellowsGit(ctx, repoPath, "rev-parse", "--verify", "origin/main"); err == nil {
		return "origin/main", nil
	}
	if err := bellowsGit(ctx, repoPath, "rev-parse", "--verify", "origin/master"); err == nil {
		return "origin/master", nil
	}
	return "", fmt.Errorf("neither origin/main nor origin/master found")
}

// bellowsGit runs a git command in the given directory, capturing stderr for
// error reporting. Uses a 60-second timeout to prevent hangs.
func bellowsGit(ctx context.Context, dir string, args ...string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", args...))
	cmd.Dir = dir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %s (%w)", args[0], stderr.String(), err)
	}
	return nil
}

// retryBackoffOrDefault returns the inline retry backoff for gh status fetches.
// It uses the production default unless a test has installed an override
// (typically a zero-delay backoff to avoid real sleeps).
func (m *Monitor) retryBackoffOrDefault() github.RetryBackoff {
	if m.retryBackoff != nil {
		return *m.retryBackoff
	}
	return github.DefaultRetryBackoff()
}

// retryTransient wraps fn in the shared transient-failure retry. A momentary
// gh/GitHub blip (transient 401, rate-limited 403, 5xx, network) is retried
// with bounded exponential backoff instead of surfacing immediately and
// flapping the PR through needs_fix/needs_human; permanent errors return at
// once so the caller's existing error handling runs unchanged.
func (m *Monitor) retryTransient(ctx context.Context, what string, fn func() error) error {
	return github.RetryTransient(ctx, m.retryBackoffOrDefault(),
		func(attempt int, delay time.Duration, err error) {
			log.Printf("[bellows] %s transient failure (retry %d in %s): %v", what, attempt, delay, err)
		}, fn)
}

// reviewGateInputs bundles every signal the Assay trigger gate evaluates.
// Extracting the decision into a pure function (shouldEmitReviewNeeded) keeps
// it unit-testable in isolation, mirroring the computeTransitionEvents pattern
// used for the other Bellows transitions.
type reviewGateInputs struct {
	enabled         bool
	managed         bool
	open            bool
	draft           bool
	skipDrafts      bool
	headSHA         string
	lastReviewedSHA string
	lastAssayRun    time.Time // zero when no prior run exists
	now             time.Time
	debounceSeconds int
	dailyCostUSD    float64
	dailyCostLimit  float64 // 0 = no limit
	runCount        int     // executed Assay reviews so far for this PR
	maxRuns         int     // 0 = no cap
	// assayInFlight is true when an Assay worker is already pending/running for
	// this PR. We suppress a fresh dispatch so a review that outlasts the
	// debounce window is not re-queued for the same head (wasting an Assay run).
	assayInFlight bool
	// beadFixWorkerActive is true when a non-Assay worker (smith/quench/burnish/
	// rebase/...) is active for the bead. While one is in flight the daemon skips
	// the Assay dispatch, so emitting would just busy-loop every debounce; we
	// suppress the emit and let it fire once the worker finishes (Forge-dkso).
	beadFixWorkerActive bool
}

// Assay suppression reasons reported by shouldEmitReviewNeeded. Only the two
// budget gates get a named reason: they are the ones that also RELEASE the PR
// to merge readiness via assayUpToDate, so silently hitting them means a head
// merges with no Assay review and no trace anywhere (observed 2026-08-09:
// the default $5 daily cost cap swallowed every review after the day's first,
// and 20 PRs on 2026-08-07 auto-merged unreviewed). The other gates are either
// transient (debounce, worker in flight) or intentional filters (draft,
// unmanaged) that do not end in an unreviewed merge.
const (
	assaySuppressedDailyCost = "daily_cost_limit"
	assaySuppressedMaxRuns   = "max_runs"
)

// shouldEmitReviewNeeded returns true when EventPRReviewNeeded should fire for a
// PR. It emits only when ALL hold: Assay is enabled; the PR is managed, open,
// and not a (skipped) draft; the current head differs from the last reviewed
// head; no Assay run occurred within the debounce window; the daily Assay
// cost is below the configured limit; and the per-PR run cap (maxRuns) has not
// been reached.
//
// CI state is deliberately NOT a gate. Assay reviews the diff (logic, security,
// test gaps), which does not depend on CI colour, and Forge's Temper already
// runs build/test/lint before the PR is created so PR-time CI failures are rare.
// Gating on green CI made Assay fire only once the PR had stabilised (≈ ready to
// merge) — too late to feed the Burnish fix loop. Reviewing on first sighting,
// like Copilot, is the intended behaviour; the debounce + immutable head SHA +
// cross-run dedup absorb any churn from rapid pushes.
// The second return value names the suppression reason when a budget gate
// (daily cost cap, per-PR run cap) blocked an otherwise-due review; it is ""
// for every other outcome, including a true first value.
func shouldEmitReviewNeeded(in reviewGateInputs) (bool, string) {
	if !in.enabled {
		return false, ""
	}
	if !in.managed {
		return false, ""
	}
	if !in.open {
		return false, ""
	}
	if in.draft && in.skipDrafts {
		return false, ""
	}
	// An Assay worker is already pending/running for this PR; don't queue a
	// second one. When it finishes, a head still unreviewed re-triggers the
	// normal way (head != lastReviewedSHA).
	if in.assayInFlight {
		return false, ""
	}
	// A non-Assay worker (smith/quench/burnish/rebase/...) holds the bead in
	// flight; the daemon would skip the Assay dispatch, so emitting now just
	// busy-loops every debounce. Suppress and let it fire once the worker
	// finishes — the head it produces is reviewed on the next poll (Forge-dkso).
	if in.beadFixWorkerActive {
		return false, ""
	}
	// An empty head SHA means GitHub did not report one; we cannot decide it is
	// unreviewed, so skip rather than risk a spurious review.
	if in.headSHA == "" || in.headSHA == in.lastReviewedSHA {
		return false, ""
	}
	debounce := in.debounceSeconds
	if debounce <= 0 {
		debounce = defaultAssayDebounceSeconds
	}
	if !in.lastAssayRun.IsZero() && in.now.Sub(in.lastAssayRun) < time.Duration(debounce)*time.Second {
		return false, ""
	}
	if in.dailyCostLimit > 0 && in.dailyCostUSD >= in.dailyCostLimit {
		return false, assaySuppressedDailyCost
	}
	// Per-PR run cap: once a PR has been reviewed maxRuns times, stop re-firing
	// so the Assay→Burnish→new-head loop terminates instead of running until a
	// pass finds nothing.
	if in.maxRuns > 0 && in.runCount >= in.maxRuns {
		return false, assaySuppressedMaxRuns
	}
	return true, ""
}

// maybeEmitReviewNeeded evaluates the Assay trigger gate for a managed, open PR
// and emits EventPRReviewNeeded when all conditions hold. It is a no-op when no
// Assay config accessor has been registered (the feature is disabled).
func (m *Monitor) maybeEmitReviewNeeded(ctx context.Context, pr *state.PR, status *vcs.PRStatus, newSnap *prSnapshot, dailyAssayCost *float64) {
	if m.assayConfig == nil {
		return
	}
	cfg := m.assayConfig(pr.Anvil)
	if !cfg.Enabled {
		return
	}

	lastRun, err := m.db.LastAssayRunAt(pr.Anvil, pr.Number)
	if err != nil {
		log.Printf("[bellows] Failed to query last Assay run for PR #%d (%s): %v", pr.Number, pr.Anvil, err)
		return
	}
	runCount, err := m.db.CountAssayRuns(pr.Anvil, pr.Number)
	if err != nil {
		log.Printf("[bellows] Failed to count Assay runs for PR #%d (%s): %v", pr.Number, pr.Anvil, err)
		return
	}
	if dailyAssayCost == nil {
		return
	}
	now := time.Now().UTC()

	managed := !(strings.HasPrefix(pr.BeadID, "ext-") && !pr.BellowsManaged)
	in := reviewGateInputs{
		enabled:             cfg.Enabled,
		managed:             managed,
		open:                !newSnap.IsMerged && !newSnap.IsClosed,
		draft:               status.IsDraft,
		skipDrafts:          cfg.SkipDrafts,
		headSHA:             status.HeadSHA,
		lastReviewedSHA:     m.lastReviewedSHA(pr),
		lastAssayRun:        lastRun,
		now:                 now,
		debounceSeconds:     cfg.DebounceSeconds,
		dailyCostUSD:        *dailyAssayCost,
		dailyCostLimit:      cfg.DailyCostLimitUSD,
		runCount:            runCount,
		maxRuns:             cfg.MaxRuns,
		assayInFlight:       m.assayWorkerInFlight(pr.Anvil, pr.Number),
		beadFixWorkerActive: m.beadFixWorkerActive(pr.Anvil, pr.BeadID),
	}
	emit, suppressedBy := shouldEmitReviewNeeded(in)
	if !emit {
		if suppressedBy != "" {
			m.noteAssaySuppressed(pr, status.HeadSHA, suppressedBy, in)
		}
		return
	}

	m.emit(ctx, PREvent{
		PRNumber:  pr.Number,
		BeadID:    pr.BeadID,
		Anvil:     pr.Anvil,
		Branch:    status.HeadRefName,
		EventType: EventPRReviewNeeded,
		Details:   fmt.Sprintf("PR #%d head %s needs Assay review", pr.Number, shortSHA(status.HeadSHA)),
		Timestamp: now,
		PRURL:     status.URL,
		HeadSHA:   status.HeadSHA,
	})
	_ = m.db.LogEvent(state.EventPRReviewNeeded, fmt.Sprintf("PR #%d queued for Assay review (head %s)", pr.Number, shortSHA(status.HeadSHA)), pr.BeadID, pr.Anvil)
}

// shortSHA truncates a commit OID to its first 8 characters for log/event
// readability, returning the input unchanged when shorter. An unreported head
// renders as "(unknown)" rather than leaving an empty gap in the message.
func shortSHA(sha string) string {
	if sha == "" {
		return "(unknown)"
	}
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// emit calls all registered handlers with the given event.
func (m *Monitor) emit(ctx context.Context, event PREvent) {
	m.mu.Lock()
	handlers := make([]Handler, len(m.handlers))
	copy(handlers, m.handlers)
	m.mu.Unlock()

	for _, h := range handlers {
		h(ctx, event)
	}
}
