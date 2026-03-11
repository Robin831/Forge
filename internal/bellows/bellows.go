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
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/ghpr"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/warden"
)

// Event types emitted by the Bellows monitor.
const (
	EventCIPassed          = "ci_passed"
	EventCIFailed          = "ci_failed"
	EventReviewApproved    = "review_approved"
	EventReviewChanges     = "review_changes_requested"
	EventPRMerged          = "pr_merged"
	EventPRClosed          = "pr_closed"
	EventPRConflicting     = "pr_conflicting"
	EventPRReadyToMerge    = "pr_ready_to_merge"
)

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
}

// Handler is called when a PR event is detected.
type Handler func(ctx context.Context, event PREvent)

// Monitor watches open PRs and dispatches events on status changes.
type Monitor struct {
	db               *state.DB
	interval         time.Duration
	anvilPaths       map[string]string // anvil name → path
	pathsMu          sync.RWMutex     // protects anvilPaths
	handlers         []Handler
	mu               sync.Mutex
	lastStatuses     map[string]*prSnapshot // anvil/PR number → last known state
	refresh          chan struct{}           // channel to trigger immediate poll
	autoLearnRules   func() bool            // auto-learn warden rules from Copilot comments on PR merge
	maxCIFixAttempts func() int             // returns current max CI fix attempts from config
	learnMuGuard     sync.Mutex             // protects learnMu map
	learnMu          map[string]*sync.Mutex // per-anvil mutex serializing auto-learn
	learnSem         chan struct{}           // caps overall concurrent auto-learn goroutines
}

// prSnapshot tracks the last seen state of a PR.
type prSnapshot struct {
	CIPassing            bool
	HasApproval          bool
	NeedsChanges         bool
	HasUnresolvedThreads bool
	HasPendingReviews    bool
	IsMerged             bool
	IsClosed             bool
	IsConflicting        bool
}

// New creates a Bellows monitor. The autoLearnRules function is called on each
// PR merge to check whether warden rule learning is enabled, so hot-reloaded
// config changes take effect without restarting the daemon. The maxCIFixAttempts
// function returns the current max CI fix attempts from config (may be nil, in
// which case the state.DefaultMaxCIFixAttempts is used).
func New(db *state.DB, interval time.Duration, anvilPaths map[string]string, autoLearnRules func() bool, maxCIFixAttempts func() int) *Monitor {
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	if maxCIFixAttempts == nil {
		maxCIFixAttempts = func() int { return state.DefaultMaxCIFixAttempts }
	}
	return &Monitor{
		db:               db,
		interval:         interval,
		anvilPaths:       anvilPaths,
		lastStatuses:     make(map[string]*prSnapshot),
		refresh:          make(chan struct{}, 1),
		autoLearnRules:   autoLearnRules,
		maxCIFixAttempts: maxCIFixAttempts,
		learnMu:          make(map[string]*sync.Mutex),
		learnSem:         make(chan struct{}, 4), // allow up to 4 concurrent auto-learn goroutines
	}
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

// checkAll polls all open PRs and emits events for state changes.
func (m *Monitor) checkAll(ctx context.Context) {
	prs, err := m.db.OpenPRs()
	if err != nil {
		log.Printf("[bellows] Error listing open PRs: %v", err)
		return
	}

	if len(prs) == 0 {
		return
	}

	log.Printf("[bellows] Checking %d open PRs", len(prs))

	for i := range prs {
		if ctx.Err() != nil {
			return
		}
		m.checkPR(ctx, &prs[i])
	}
}

// checkPR polls a single PR and emits events for any state changes.
func (m *Monitor) checkPR(ctx context.Context, pr *state.PR) {
	m.pathsMu.RLock()
	anvilPath, ok := m.anvilPaths[pr.Anvil]
	m.pathsMu.RUnlock()
	if !ok {
		log.Printf("[bellows] Unknown anvil %s for PR #%d", pr.Anvil, pr.Number)
		return
	}

	status, err := ghpr.CheckStatus(ctx, anvilPath, pr.Number)
	if err != nil {
		log.Printf("[bellows] Error checking PR #%d: %v", pr.Number, err)
		return
	}

	newSnap := &prSnapshot{
		CIPassing:            status.CIsPassing(),
		HasApproval:          status.HasApproval(),
		NeedsChanges:         status.NeedsChanges(),
		HasUnresolvedThreads: status.UnresolvedThreads > 0,
		HasPendingReviews:    status.HasPendingReviewRequests(),
		IsMerged:             status.IsMerged(),
		IsClosed:             status.IsClosed(),
		IsConflicting:        status.Mergeable == "CONFLICTING",
	}

	// Detect transitions and emit events. We re-acquire the lock and re-check the
	// last status to ensure a concurrent ResetPRState call hasn't cleared it.
	m.mu.Lock()
	key := fmt.Sprintf("%s/%d", pr.Anvil, pr.Number)
	lastSnap := m.lastStatuses[key]
	if lastSnap == nil {
		// Reset occurred during poll: treat as first check to ensure transitions are detected.
		// Seed with "good" states so that if the PR is already in a "bad" state (failing,
		// conflicting, etc.), the transition will be detected on this first poll.
		lastSnap = &prSnapshot{CIPassing: true}
	}
	// Update snapshot while holding the lock
	m.lastStatuses[key] = newSnap
	m.mu.Unlock()

	if newSnap.IsMerged && !lastSnap.IsMerged {
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
	} else if newSnap.IsClosed && !lastSnap.IsClosed {
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
	} else if !newSnap.CIPassing && lastSnap.CIPassing {
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
	} else if !newSnap.CIPassing && !lastSnap.CIPassing {
		// CI is still failing with no transition. Check if a previous cifix
		// attempt completed (PR status reset to open) and retries remain.
		// This catches the gap where NotifyCIFixCompleted() clears the fix
		// state but bellows never re-emits EventCIFailed because it only
		// detected transitions.
		maxCI := m.maxCIFixAttempts()
		if pr.Status != state.PRNeedsFix && pr.CIFixCount > 0 && pr.CIFixCount < maxCI {
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
	}

	// If all merge-readiness conditions are met and the PR was in needs_fix,
	// restore it to approved so the Ready-to-Merge panel picks it up again.
	if newSnap.HasApproval && newSnap.CIPassing && !newSnap.IsConflicting && !newSnap.HasUnresolvedThreads && !newSnap.HasPendingReviews {
		_ = m.db.UpdatePRStatusIfNeedsFix(pr.ID, state.PRApproved)
	}

	// Detect transition to fully ready-to-merge state (approved + CI passing +
	// no conflicts, unresolved threads, or pending reviews).
	newReady := newSnap.HasApproval && newSnap.CIPassing && !newSnap.IsConflicting && !newSnap.HasUnresolvedThreads && !newSnap.HasPendingReviews
	lastReady := lastSnap.HasApproval && lastSnap.CIPassing && !lastSnap.IsConflicting && !lastSnap.HasUnresolvedThreads && !lastSnap.HasPendingReviews
	if newReady && !lastReady {
		m.emit(ctx, PREvent{
			PRNumber:  pr.Number,
			BeadID:    pr.BeadID,
			Anvil:     pr.Anvil,
			Branch:    status.HeadRefName,
			EventType: EventPRReadyToMerge,
			Details:   fmt.Sprintf("PR #%d is ready to merge (CI passing, approved, no blocking reviews)", pr.Number),
			Timestamp: time.Now(),
			PRURL:     status.URL,
		})
		_ = m.db.LogEvent(state.EventPRReadyToMerge,
			fmt.Sprintf("PR #%d ready to merge", pr.Number),
			pr.BeadID, pr.Anvil)
	}

	// Persist mergeability state so the ready-to-merge panel stays current.
	// Include ci_passing so the Ready to Merge panel reflects the latest CI
	// status every poll cycle, not just on CI transition events.
	_ = m.db.UpdatePRMergeability(pr.ID, newSnap.CIPassing, newSnap.IsConflicting, newSnap.HasUnresolvedThreads, newSnap.HasPendingReviews)

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

	comments, err := warden.FetchCopilotComments(ctx, anvilPath, prNumber)
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
		_ = bellowsGit(ctx, anvilPath, "worktree", "remove", "--force", wtPath)
		_ = bellowsGit(ctx, anvilPath, "worktree", "prune")
		_ = bellowsGit(ctx, anvilPath, "branch", "-D", branchName)
		_ = os.RemoveAll(wtPath)
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

	pr, err := ghpr.Create(ctx, ghpr.CreateParams{
		WorktreePath: wtPath,
		Title:        fmt.Sprintf("forge: learn %d warden rule(s) from PR #%d [no-changelog]", added, prNumber),
		Body:         prBody,
		Branch:       branchName,
		AnvilName:    anvilName,
		DB:           m.db,
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
