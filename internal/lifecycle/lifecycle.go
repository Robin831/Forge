// Package lifecycle manages the end-to-end PR lifecycle state machine.
//
// It connects Bellows events to downstream actions: CI fixes, review fixes,
// bead closure on merge, and cleanup on close. The lifecycle manager is the
// central dispatcher that wires together all Bellows-triggered behaviors.
package lifecycle

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/state"
)

// PRState tracks the lifecycle of a single PR.
type PRState struct {
	PRNumber     int
	BeadID       string
	Anvil        string
	Branch       string
	CIPassing    bool
	Approved     bool
	NeedsFix     bool
	Merged       bool
	Closed       bool
	CIFixCount   int
	ReviewFixCnt int
}

// Action enumerates lifecycle actions to take.
type Action int

const (
	ActionNone       Action = iota
	ActionFixCI             // Spawn CI fix worker
	ActionFixReview         // Spawn review fix worker
	ActionCloseBead         // Close bead after merge
	ActionCleanup           // Clean up worktree/branch after close
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
	db      *state.DB
	mu      sync.Mutex
	states  map[int]*PRState // PR number → state
	handler ActionHandler
	maxCI   int // max CI fix attempts per PR
	maxRev  int // max review fix attempts per PR
}

// New creates a lifecycle Manager.
func New(db *state.DB, handler ActionHandler) *Manager {
	return &Manager{
		db:      db,
		states:  make(map[int]*PRState),
		handler: handler,
		maxCI:   2,
		maxRev:  2,
	}
}

// HandleEvent processes a Bellows PR event and dispatches any required actions.
func (m *Manager) HandleEvent(ctx context.Context, event bellows.PREvent) {
	m.mu.Lock()
	st, ok := m.states[event.PRNumber]
	if !ok {
		st = &PRState{
			PRNumber: event.PRNumber,
			BeadID:   event.BeadID,
			Anvil:    event.Anvil,
			Branch:   event.Branch,
		}
		m.states[event.PRNumber] = st
	}
	if event.Branch != "" {
		st.Branch = event.Branch
	}
	m.mu.Unlock()

	var action Action

	switch event.EventType {
	case bellows.EventCIPassed:
		st.CIPassing = true
		st.NeedsFix = false
		log.Printf("[lifecycle] PR #%d: CI passed", event.PRNumber)

	case bellows.EventCIFailed:
		st.CIPassing = false
		st.NeedsFix = true
		if st.CIFixCount < m.maxCI {
			st.CIFixCount++
			action = ActionFixCI
			log.Printf("[lifecycle] PR #%d: CI failed, dispatching fix (attempt %d)", event.PRNumber, st.CIFixCount)
		} else {
			log.Printf("[lifecycle] PR #%d: CI failed, max fix attempts exhausted", event.PRNumber)
			_ = m.db.LogEvent(state.EventLifecycleExhausted,
				fmt.Sprintf("PR #%d: CI fix attempts exhausted (%d)", event.PRNumber, m.maxCI),
				event.BeadID, event.Anvil)
		}

	case bellows.EventReviewApproved:
		st.Approved = true
		log.Printf("[lifecycle] PR #%d: Approved", event.PRNumber)

	case bellows.EventReviewChanges:
		st.NeedsFix = true
		if st.ReviewFixCnt < m.maxRev {
			st.ReviewFixCnt++
			action = ActionFixReview
			log.Printf("[lifecycle] PR #%d: Changes requested, dispatching fix (attempt %d)", event.PRNumber, st.ReviewFixCnt)
		} else {
			log.Printf("[lifecycle] PR #%d: Changes requested, max fix attempts exhausted", event.PRNumber)
			_ = m.db.LogEvent(state.EventLifecycleExhausted,
				fmt.Sprintf("PR #%d: Review fix attempts exhausted (%d)", event.PRNumber, m.maxRev),
				event.BeadID, event.Anvil)
		}

	case bellows.EventPRConflicting:
		log.Printf("[lifecycle] PR #%d: merge conflict detected, manual rebase required", event.PRNumber)
		_ = m.db.LogEvent(state.EventPRConflicting,
			fmt.Sprintf("PR #%d: conflict detected — rebase required before merge", event.PRNumber),
			event.BeadID, event.Anvil)

	case bellows.EventPRMerged:
		st.Merged = true
		action = ActionCloseBead
		log.Printf("[lifecycle] PR #%d: Merged, will close bead", event.PRNumber)

	case bellows.EventPRClosed:
		st.Closed = true
		action = ActionCleanup
		log.Printf("[lifecycle] PR #%d: Closed without merge, cleanup", event.PRNumber)
	}

	if action != ActionNone && m.handler != nil {
		m.handler(ctx, ActionRequest{
			Action:   action,
			PRNumber: event.PRNumber,
			BeadID:   event.BeadID,
			Anvil:    event.Anvil,
			Branch:   st.Branch,
		})
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
