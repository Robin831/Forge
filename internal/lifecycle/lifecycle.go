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

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/state"
)

// PRState tracks the lifecycle of a single PR.
type PRState struct {
	ID           int // Database ID
	PRNumber     int
	BeadID       string
	Anvil        string
	Branch       string
	CIPassing    bool
	Approved     bool
	NeedsFix     bool
	Conflicting  bool
	Merged       bool
	Closed       bool
	CIFixCount   int
	ReviewFixCnt int
	RebaseCount  int
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
	states    map[int]*PRState // PR number → state
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
		states:    make(map[int]*PRState),
		handler:   handler,
		maxCI:     2,
		maxRev:    2,
		maxRebase: 3,
	}
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
		st := &PRState{
			ID:           pr.ID,
			PRNumber:     pr.Number,
			BeadID:       pr.BeadID,
			Anvil:        pr.Anvil,
			Branch:       pr.Branch,
			CIPassing:    pr.CIPassing,
			Approved:     pr.Status == state.PRApproved,
			NeedsFix:     pr.Status == state.PRNeedsFix,
			CIFixCount:   pr.CIFixCount,
			ReviewFixCnt: pr.ReviewFixCount,
		}
		m.states[pr.Number] = st
		m.logger.Info("restored PR from DB",
			"pr", pr.Number,
			"anvil", pr.Anvil,
			"ci_passing", pr.CIPassing,
			"ci_fixes", pr.CIFixCount,
			"review_fixes", pr.ReviewFixCount)
	}
	return nil
}

// HandleEvent processes a Bellows PR event and dispatches any required actions.
func (m *Manager) HandleEvent(ctx context.Context, event bellows.PREvent) {
	m.mu.Lock()
	st, ok := m.states[event.PRNumber]
	if !ok {
		// New PR (or first time we've seen it this session)
		// Try to find it in the DB
		pr, err := m.db.GetPRByNumber(event.Anvil, event.PRNumber)
		if err == nil && pr != nil {
			st = &PRState{
				ID:           pr.ID,
				PRNumber:     pr.Number,
				BeadID:       pr.BeadID,
				Anvil:        pr.Anvil,
				Branch:       pr.Branch,
				CIPassing:    pr.CIPassing,
				Approved:     pr.Status == state.PRApproved,
				NeedsFix:     pr.Status == state.PRNeedsFix,
				CIFixCount:   pr.CIFixCount,
				ReviewFixCnt: pr.ReviewFixCount,
			}
		} else {
			st = &PRState{
				PRNumber:  event.PRNumber,
				BeadID:    event.BeadID,
				Anvil:     event.Anvil,
				Branch:    event.Branch,
				CIPassing: true, // Default to true so first failure triggers fix
			}
		}
		m.states[event.PRNumber] = st
	}
	if event.Branch != "" {
		st.Branch = event.Branch
	}

	var action Action

	switch event.EventType {
	case bellows.EventCIPassed:
		st.CIPassing = true
		st.NeedsFix = false
		m.logger.Info("PR CI passed", "pr", event.PRNumber)

	case bellows.EventCIFailed:
		if !st.CIPassing {
			// Already in failing state, don't increment counter or dispatch new fix
			m.logger.Info("PR CI failed again (already failing), skipping dispatch", "pr", event.PRNumber)
			break
		}
		st.CIPassing = false
		st.NeedsFix = true
		if st.CIFixCount < m.maxCI {
			st.CIFixCount++
			action = ActionFixCI
			m.logger.Info("PR CI failed, dispatching fix", "pr", event.PRNumber, "attempt", st.CIFixCount)
		} else {
			m.logger.Warn("PR CI failed, max fix attempts exhausted", "pr", event.PRNumber, "max", m.maxCI)
			_ = m.db.LogEvent(state.EventLifecycleExhausted,
				fmt.Sprintf("PR #%d: CI fix attempts exhausted (%d)", event.PRNumber, m.maxCI),
				event.BeadID, event.Anvil)
		}

	case bellows.EventReviewApproved:
		st.Approved = true
		m.logger.Info("PR approved", "pr", event.PRNumber)

	case bellows.EventReviewChanges:
		// Only increment if not already in a fix cycle, or if previously approved.
		if !st.NeedsFix || st.Approved {
			st.NeedsFix = true
			st.Approved = false
			if st.ReviewFixCnt < m.maxRev {
				st.ReviewFixCnt++
				action = ActionFixReview
				m.logger.Info("PR changes requested, dispatching fix", "pr", event.PRNumber, "attempt", st.ReviewFixCnt)
			} else {
				m.logger.Warn("PR changes requested, max fix attempts exhausted", "pr", event.PRNumber, "max", m.maxRev)
				_ = m.db.LogEvent(state.EventLifecycleExhausted,
					fmt.Sprintf("PR #%d: Review fix attempts exhausted (%d)", event.PRNumber, m.maxRev),
					event.BeadID, event.Anvil)
			}
		} else {
			m.logger.Info("PR changes requested again, but already in fix cycle", "pr", event.PRNumber)
		}

	case bellows.EventPRConflicting:
		st.Conflicting = true
		if st.RebaseCount < m.maxRebase {
			st.RebaseCount++
			action = ActionRebase
			m.logger.Info("PR merge conflict detected, dispatching rebase", "pr", event.PRNumber, "attempt", st.RebaseCount)
		} else {
			m.logger.Warn("PR merge conflict detected, max rebase attempts exhausted", "pr", event.PRNumber, "max", m.maxRebase)
			_ = m.db.LogEvent(state.EventLifecycleExhausted,
				fmt.Sprintf("PR #%d: rebase attempts exhausted (%d)", event.PRNumber, m.maxRebase),
				event.BeadID, event.Anvil)
		}

	case bellows.EventPRMerged:
		st.Merged = true
		action = ActionCloseBead
		m.logger.Info("PR merged, will close bead", "pr", event.PRNumber)

	case bellows.EventPRClosed:
		st.Closed = true
		action = ActionCleanup
		m.logger.Info("PR closed without merge, cleanup", "pr", event.PRNumber)
	}

	// Persist changes to DB
	if st.ID > 0 {
		err := m.db.UpdatePRLifecycle(st.ID, st.CIFixCount, st.ReviewFixCnt, st.CIPassing)
		if err != nil {
			m.logger.Error("failed to update PR lifecycle in DB", "pr", event.PRNumber, "error", err)
		}
	}

	// Capture values needed for async handler while holding the lock
	req := ActionRequest{
		Action:   action,
		PRNumber: event.PRNumber,
		BeadID:   event.BeadID,
		Anvil:    event.Anvil,
		Branch:   st.Branch,
	}
	m.mu.Unlock()

	if action != ActionNone && m.handler != nil {
		m.handler(ctx, req)
	}
}

// GetState returns the current lifecycle state for a PR.
func (m *Manager) GetState(prNumber int) *PRState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.states[prNumber]
}

// SetBranch sets the branch name for a tracked PR.
func (m *Manager) SetBranch(prNumber int, branch string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[prNumber]
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
func (m *Manager) Remove(prNumber int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.states, prNumber)
}
