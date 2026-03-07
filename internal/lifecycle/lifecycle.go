// Package lifecycle manages the end-to-end PR lifecycle state machine.
//
// It connects Bellows events to downstream actions: CI fixes, review fixes,
// bead closure on merge, and cleanup on close. The lifecycle manager is the
// central dispatcher that wires together all Bellows-triggered behaviors.
package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/state"
)

// PRState tracks the lifecycle of a single PR.
type PRState struct {
	ID             int // Database ID
	PRNumber       int
	BeadID         string
	Anvil          string
	Branch         string
	CIPassing      bool
	Approved       bool
	CINeedsFix     bool // CI failure fix cycle in progress
	ReviewNeedsFix bool // Review comment fix cycle in progress
	Conflicting    bool
	Merged         bool
	Closed         bool
	CIFixCount     int
	ReviewFixCnt   int
	RebaseCount    int
}

// NeedsFix returns true if either a CI or review fix cycle is active.
func (s *PRState) NeedsFix() bool {
	return s.CINeedsFix || s.ReviewNeedsFix
}

// Action enumerates lifecycle actions to take.
type Action int

const (
	ActionNone      Action = iota
	ActionFixCI            // Spawn CI fix worker
	ActionFixReview        // Spawn review fix worker
	ActionCloseBead        // Close bead after merge
	ActionCleanup          // Clean up worktree/branch after close
	ActionRebase           // Rebase branch on top of main to resolve conflict
)

// ActionRequest is dispatched to the action handler.
type ActionRequest struct {
	Action   Action
	PRNumber int
	BeadID   string
	Anvil    string
	Branch   string
}

// ActionHandler processes lifecycle actions. Implementations should be async-safe.
type ActionHandler func(ctx context.Context, req ActionRequest)

// Manager tracks PR states and dispatches actions based on events.
type Manager struct {
	db        *state.DB
	logger    *slog.Logger
	mu        sync.Mutex
	states    map[string]*PRState // "anvil/number" → state
	handler   ActionHandler
	maxCI     int // max CI fix attempts per PR
	maxRev    int // max review fix attempts per PR
	maxRebase int // max rebase attempts per PR
}

// New creates a lifecycle Manager.
func New(db *state.DB, logger *slog.Logger, handler ActionHandler) *Manager {
	return &Manager{
		db:        db,
		logger:    logger,
		states:    make(map[string]*PRState),
		handler:   handler,
		maxCI:     state.DefaultMaxCIFixAttempts,
		maxRev:    state.DefaultMaxReviewFixAttempts,
		maxRebase: state.DefaultMaxRebaseAttempts,
	}
}

// SetThresholds overrides the default max attempt limits. Zero values are
// ignored (the existing default is kept). Must be called before any goroutines
// call HandleEvent, or the caller must ensure exclusive access.
func (m *Manager) SetThresholds(maxCI, maxRev, maxRebase int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if maxCI > 0 {
		m.maxCI = maxCI
	}
	if maxRev > 0 {
		m.maxRev = maxRev
	}
	if maxRebase > 0 {
		m.maxRebase = maxRebase
	}
}

func (m *Manager) key(anvil string, number int) string {
	return fmt.Sprintf("%s/%d", anvil, number)
}

// Load pre-populates the lifecycle map with open PRs from the database.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	prs, err := m.db.OpenPRs()
	if err != nil {
		return fmt.Errorf("loading open PRs from DB: %w", err)
	}

	for _, pr := range prs {
		// If a PR was persisted as needs_fix, handle it based on CI state:
		//
		// CIPassing=true  → the needs_fix was set by bellows for a review-fix cycle
		//                   that was killed mid-flight (e.g. daemon restart). Reset
		//                   to open so bellows re-detects review comments and
		//                   dispatches a fresh fix cycle.
		//
		// CIPassing=false → the needs_fix was set by bellows for an active CI failure.
		//                   Do NOT reset it, otherwise we briefly hide the CI-failing
		//                   state on restart. Bellows will re-detect and handle it.
		//
		// In both cases, do NOT restore NeedsFix=true in memory — bellows will
		// re-detect and drive any required fix cycles.
		if pr.Status == state.PRNeedsFix {
			if pr.CIPassing {
				if err := m.db.UpdatePRStatus(pr.ID, state.PROpen); err != nil {
					m.logger.Warn("failed to reset stale review needs_fix PR to open on load",
						"pr", pr.Number, "anvil", pr.Anvil, "error", err)
				} else {
					m.logger.Info("reset stale review needs_fix PR to open on load (fix worker was killed)",
						"pr", pr.Number, "anvil", pr.Anvil)
				}
			} else {
				m.logger.Info("preserving needs_fix status for CI-failing PR on load",
					"pr", pr.Number, "anvil", pr.Anvil)
			}
		}
		st := &PRState{
			ID:             pr.ID,
			PRNumber:       pr.Number,
			BeadID:         pr.BeadID,
			Anvil:          pr.Anvil,
			Branch:         pr.Branch,
			CIPassing:      pr.CIPassing,
			Approved:       pr.Status == state.PRApproved,
			CINeedsFix:     false, // never restore; bellows will re-detect CI failures
			ReviewNeedsFix: false, // never restore; bellows will re-detect review comments
			CIFixCount:     pr.CIFixCount,
			ReviewFixCnt:   pr.ReviewFixCount,
			RebaseCount:    pr.RebaseCount,
		}
		m.states[m.key(pr.Anvil, pr.Number)] = st
		m.logger.Info("restored PR from DB",
			"pr", pr.Number,
			"anvil", pr.Anvil,
			"ci_passing", pr.CIPassing,
			"ci_fixes", pr.CIFixCount,
			"review_fixes", pr.ReviewFixCount,
			"rebases", pr.RebaseCount)
	}
	return nil
}

// HandleEvent processes a Bellows PR event and dispatches any required actions.
func (m *Manager) HandleEvent(ctx context.Context, event bellows.PREvent) {
	key := m.key(event.Anvil, event.PRNumber)

	m.mu.Lock()
	st, ok := m.states[key]
	m.mu.Unlock()

	if !ok {
		// New PR (or first time we've seen it this session)
		// Try to find it in the DB outside the lock
		pr, err := m.db.GetPRByNumber(event.Anvil, event.PRNumber)
		var newSt *PRState
		if err != nil {
			// DB error — log and fall back to in-memory-only tracking
			m.logger.Error("failed to query PR from DB, falling back to in-memory tracking",
				"pr", event.PRNumber, "anvil", event.Anvil, "error", err)
			newSt = &PRState{
				PRNumber:  event.PRNumber,
				BeadID:    event.BeadID,
				Anvil:     event.Anvil,
				Branch:    event.Branch,
				CIPassing: true,
			}
		} else if pr != nil {
			newSt = &PRState{
				ID:             pr.ID,
				PRNumber:       pr.Number,
				BeadID:         pr.BeadID,
				Anvil:          pr.Anvil,
				Branch:         pr.Branch,
				CIPassing:      pr.CIPassing,
				Approved:       pr.Status == state.PRApproved,
				CINeedsFix:     pr.Status == state.PRNeedsFix && !pr.CIPassing,
				ReviewNeedsFix: pr.Status == state.PRNeedsFix && pr.CIPassing,
				CIFixCount:     pr.CIFixCount,
				ReviewFixCnt:   pr.ReviewFixCount,
				RebaseCount:    pr.RebaseCount,
			}
		} else {
			// Not found in DB — create new state and persist it
			newSt = &PRState{
				PRNumber:  event.PRNumber,
				BeadID:    event.BeadID,
				Anvil:     event.Anvil,
				Branch:    event.Branch,
				CIPassing: true, // Default to true so first failure triggers fix
			}
			// Persist new PR to DB
			dbPR := &state.PR{
				Number:    newSt.PRNumber,
				Anvil:     newSt.Anvil,
				BeadID:    newSt.BeadID,
				Branch:    newSt.Branch,
				Status:    state.PROpen,
				CreatedAt: time.Now(),
			}
			if err := m.db.InsertPR(dbPR); err != nil {
				m.logger.Error("failed to insert new PR into DB", "pr", newSt.PRNumber, "anvil", newSt.Anvil, "error", err)
			} else {
				newSt.ID = dbPR.ID
				m.logger.Info("tracked new PR in DB", "pr", newSt.PRNumber, "anvil", newSt.Anvil, "db_id", newSt.ID)
			}
		}

		m.mu.Lock()
		if existing, exists := m.states[key]; exists {
			st = existing
		} else {
			m.states[key] = newSt
			st = newSt
		}
		m.mu.Unlock()
	}

	m.mu.Lock()
	if event.Branch != "" {
		st.Branch = event.Branch
	}

	var action Action
	var logMsg string

	switch event.EventType {
	case bellows.EventCIPassed:
		st.CIPassing = true
		st.CINeedsFix = false
		m.logger.Info("PR CI passed", "pr", event.PRNumber, "anvil", event.Anvil)

	case bellows.EventCIFailed:
		if !st.CIPassing {
			// Already in failing state, don't increment counter or dispatch new fix
			m.logger.Info("PR CI failed again (already failing), skipping dispatch", "pr", event.PRNumber, "anvil", event.Anvil)
			break
		}
		st.CIPassing = false
		st.CINeedsFix = true
		if st.CIFixCount < m.maxCI {
			st.CIFixCount++
			action = ActionFixCI
			m.logger.Info("PR CI failed, dispatching fix", "pr", event.PRNumber, "anvil", event.Anvil, "attempt", st.CIFixCount)
		} else {
			m.logger.Warn("PR CI failed, max fix attempts exhausted", "pr", event.PRNumber, "anvil", event.Anvil, "max", m.maxCI)
			logMsg = fmt.Sprintf("PR #%d (%s): CI fix attempts exhausted (%d)", event.PRNumber, event.Anvil, m.maxCI)
		}

	case bellows.EventReviewApproved:
		st.Approved = true
		m.logger.Info("PR approved", "pr", event.PRNumber, "anvil", event.Anvil)

	case bellows.EventReviewChanges:
		// Only increment if not already in a review fix cycle, or if previously approved.
		// CINeedsFix is independent — a CI fix in progress does not block review fixes.
		if !st.ReviewNeedsFix || st.Approved {
			st.ReviewNeedsFix = true
			st.Approved = false
			if st.ReviewFixCnt < m.maxRev {
				st.ReviewFixCnt++
				action = ActionFixReview
				m.logger.Info("PR changes requested, dispatching fix", "pr", event.PRNumber, "anvil", event.Anvil, "attempt", st.ReviewFixCnt)
			} else {
				m.logger.Warn("PR changes requested, max fix attempts exhausted", "pr", event.PRNumber, "anvil", event.Anvil, "max", m.maxRev)
				logMsg = fmt.Sprintf("PR #%d (%s): Review fix attempts exhausted (%d)", event.PRNumber, event.Anvil, m.maxRev)
			}
		} else {
			m.logger.Info("PR changes requested again, but already in review fix cycle", "pr", event.PRNumber, "anvil", event.Anvil)
		}

	case bellows.EventPRConflicting:
		st.Conflicting = true
		if st.RebaseCount < m.maxRebase {
			st.RebaseCount++
			action = ActionRebase
			m.logger.Info("PR merge conflict detected, dispatching rebase", "pr", event.PRNumber, "anvil", event.Anvil, "attempt", st.RebaseCount)
		} else {
			m.logger.Warn("PR merge conflict detected, max rebase attempts exhausted", "pr", event.PRNumber, "anvil", event.Anvil, "max", m.maxRebase)
			logMsg = fmt.Sprintf("PR #%d (%s): rebase attempts exhausted (%d)", event.PRNumber, event.Anvil, m.maxRebase)
		}

	case bellows.EventPRMerged:
		st.Merged = true
		action = ActionCloseBead
		m.logger.Info("PR merged, will close bead", "pr", event.PRNumber, "anvil", event.Anvil)

	case bellows.EventPRClosed:
		st.Closed = true
		action = ActionCleanup
		m.logger.Info("PR closed without merge, cleanup", "pr", event.PRNumber, "anvil", event.Anvil)
	}

	// Capture all values needed after the lock is released.
	dbID := st.ID
	ciFixCount := st.CIFixCount
	reviewFixCnt := st.ReviewFixCnt
	rebaseCount := st.RebaseCount
	ciPassing := st.CIPassing
	req := ActionRequest{
		Action:   action,
		PRNumber: event.PRNumber,
		BeadID:   event.BeadID,
		Anvil:    event.Anvil,
		Branch:   st.Branch,
	}
	m.mu.Unlock()

	// Persist changes to DB outside the lock to avoid blocking other event handlers
	// during SQLite contention (busy timeout / WAL checkpoint).
	if logMsg != "" {
		_ = m.db.LogEvent(state.EventLifecycleExhausted, logMsg, event.BeadID, event.Anvil)
	}

	if dbID > 0 {
		if err := m.db.UpdatePRLifecycle(dbID, ciFixCount, reviewFixCnt, rebaseCount, ciPassing); err != nil {
			m.logger.Error("failed to update PR lifecycle in DB", "pr", event.PRNumber, "anvil", event.Anvil, "error", err)
		}
	}

	if action != ActionNone && m.handler != nil {
		m.handler(ctx, req)
	}
}

// GetState returns the current lifecycle state for a PR.
func (m *Manager) GetState(anvil string, prNumber int) *PRState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.states[m.key(anvil, prNumber)]
}

// SetBranch sets the branch name for a tracked PR.
func (m *Manager) SetBranch(anvil string, prNumber int, branch string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[m.key(anvil, prNumber)]
	if ok {
		st.Branch = branch
	}
}

// ActivePRs returns the count of non-merged, non-closed tracked PRs.
func (m *Manager) ActivePRs() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, st := range m.states {
		if !st.Merged && !st.Closed {
			count++
		}
	}
	return count
}

// Remove deletes tracking state for a PR (e.g., after cleanup).
func (m *Manager) Remove(anvil string, prNumber int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, m.key(anvil, prNumber))
}

// ResetPRState resets the in-memory lifecycle state for a PR so that new
// Bellows events can dispatch fresh fix/rebase workers. This must be called
// alongside ResetPRFixCounts (which resets the DB) to keep the two in sync.
// Without this, the lifecycle manager still believes the PR is exhausted
// after a retry and silently drops incoming events.
func (m *Manager) ResetPRState(anvil string, prNumber int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[m.key(anvil, prNumber)]
	if !ok {
		return
	}
	st.CIFixCount = 0
	st.ReviewFixCnt = 0
	st.RebaseCount = 0
	st.CINeedsFix = false
	st.ReviewNeedsFix = false
	st.CIPassing = true
	st.Conflicting = false
	st.Approved = false
}

// NotifyCIFixCompleted clears the CINeedsFix flag and resets CIPassing after
// a CI fix worker finishes. This allows the next EventCIFailed (if CI still
// fails after the fix attempt) to dispatch a new fix cycle rather than being
// suppressed by the "already failing, skipping dispatch" guard at line 267.
//
// Without this, CIPassing stays false and CINeedsFix stays true after the fix
// worker completes. When bellows re-detects CI failure, HandleEvent sees
// CIPassing=false and silently drops the event, permanently sticking the PR.
func (m *Manager) NotifyCIFixCompleted(anvil string, prNumber int) {
	m.mu.Lock()
	st, ok := m.states[m.key(anvil, prNumber)]
	if !ok {
		m.mu.Unlock()
		return
	}
	st.CIPassing = true
	st.CINeedsFix = false
	dbID := st.ID
	reviewNeedsFix := st.ReviewNeedsFix
	ciFixCount := st.CIFixCount
	reviewFixCnt := st.ReviewFixCnt
	rebaseCount := st.RebaseCount
	m.mu.Unlock()

	m.logger.Info("CI fix cycle completed, reset CIPassing=true", "pr", prNumber, "anvil", anvil)

	// Persist the cleared state so a daemon restart does not reload a stale
	// needs_fix DB status. Only transition needs_fix → open when no review
	// fix cycle is also active; if ReviewNeedsFix is set, the DB status must
	// remain needs_fix so the review failure is preserved across restarts.
	if dbID > 0 && !reviewNeedsFix {
		if err := m.db.UpdatePRStatusIfNeedsFix(dbID, state.PROpen); err != nil {
			m.logger.Warn("failed to persist PROpen after CI fix completed",
				"pr", prNumber, "anvil", anvil, "error", err)
		}
	}

	// Persist the updated CIPassing flag to the DB so bellows sees the
	// correct state after a daemon restart.
	if dbID > 0 {
		if err := m.db.UpdatePRLifecycle(dbID, ciFixCount, reviewFixCnt, rebaseCount, true); err != nil {
			m.logger.Warn("failed to persist CIPassing after CI fix completed",
				"pr", prNumber, "anvil", anvil, "error", err)
		}
	}
}

// NotifyReviewFixCompleted clears the ReviewNeedsFix flag after a review fix
// worker finishes. This allows the next EventReviewChanges (triggered
// automatically by GitHub when the reviewer re-examines the updated push) to
// dispatch a new fix cycle rather than being suppressed by the "already in fix
// cycle" guard.
//
// Without this, ReviewNeedsFix stays true after the fix worker pushes its
// changes. When the reviewer re-reviews and still requests changes, HandleEvent
// sees ReviewNeedsFix=true and silently drops the event.
func (m *Manager) NotifyReviewFixCompleted(anvil string, prNumber int) {
	m.mu.Lock()
	st, ok := m.states[m.key(anvil, prNumber)]
	if !ok {
		m.mu.Unlock()
		return
	}
	st.ReviewNeedsFix = false
	dbID := st.ID
	ciNeedsFix := st.CINeedsFix
	m.mu.Unlock()

	m.logger.Info("review fix cycle completed, cleared ReviewNeedsFix", "pr", prNumber, "anvil", anvil)

	// Persist the cleared state so a daemon restart does not reload a stale
	// needs_fix DB status and get permanently stuck.
	// Use a conditional update that only transitions from needs_fix → open;
	// this prevents overwriting a terminal status (merged/closed) if the PR
	// was closed while the review-fix worker was still running.
	//
	// Only update when CI is not still failing: if CINeedsFix is set, the DB
	// status must remain needs_fix so the CI failure is preserved across daemon
	// restarts. Bellows only sets PRNeedsFix on a passing→failing transition, so
	// clearing it here while CI is still failing would leave the DB permanently
	// inconsistent (needs_fix would never be re-set while CI stays failing).
	if dbID > 0 && !ciNeedsFix {
		if err := m.db.UpdatePRStatusIfNeedsFix(dbID, state.PROpen); err != nil {
			m.logger.Warn("failed to persist PROpen after review fix completed",
				"pr", prNumber, "anvil", anvil, "error", err)
		}
	}
}

