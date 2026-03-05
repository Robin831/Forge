// Package bellows monitors open PRs for status changes, CI results, and reviews.
//
// Bellows periodically polls all open PRs in the state DB and updates their
// status. It triggers downstream actions: CI fix workers, review comment
// forwarding, and PR lifecycle state tracking.
package bellows

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/ghpr"
	"github.com/Robin831/Forge/internal/state"
)

// Event types emitted by the Bellows monitor.
const (
	EventCIPassed        = "ci_passed"
	EventCIFailed        = "ci_failed"
	EventReviewApproved  = "review_approved"
	EventReviewChanges   = "review_changes_requested"
	EventPRMerged        = "pr_merged"
	EventPRClosed        = "pr_closed"
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
}

// Handler is called when a PR event is detected.
type Handler func(ctx context.Context, event PREvent)

// Monitor watches open PRs and dispatches events on status changes.
type Monitor struct {
	db           *state.DB
	interval     time.Duration
	anvilPaths   map[string]string // anvil name → path
	handlers     []Handler
	mu           sync.Mutex
	lastStatuses map[int]*prSnapshot // PR number → last known state
}

// prSnapshot tracks the last seen state of a PR.
type prSnapshot struct {
	CIPassing            bool
	HasApproval          bool
	NeedsChanges         bool
	HasUnresolvedThreads bool
	IsMerged             bool
	IsClosed             bool
}

// New creates a Bellows monitor.
func New(db *state.DB, interval time.Duration, anvilPaths map[string]string) *Monitor {
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	return &Monitor{
		db:           db,
		interval:     interval,
		anvilPaths:   anvilPaths,
		lastStatuses: make(map[int]*prSnapshot),
	}
}

// OnEvent registers a handler for PR events.
func (m *Monitor) OnEvent(h Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers = append(m.handlers, h)
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
	anvilPath, ok := m.anvilPaths[pr.Anvil]
	if !ok {
		log.Printf("[bellows] Unknown anvil %s for PR #%d", pr.Anvil, pr.Number)
		return
	}

	status, err := ghpr.CheckStatus(ctx, anvilPath, pr.Number)
	if err != nil {
		log.Printf("[bellows] Error checking PR #%d: %v", pr.Number, err)
		return
	}

	m.mu.Lock()
	lastSnap := m.lastStatuses[pr.Number]
	if lastSnap == nil {
		lastSnap = &prSnapshot{}
	}
	m.mu.Unlock()

	newSnap := &prSnapshot{
		CIPassing:            status.CIsPassing(),
		HasApproval:          status.HasApproval(),
		NeedsChanges:         status.NeedsChanges(),
		HasUnresolvedThreads: status.UnresolvedThreads > 0,
		IsMerged:             status.IsMerged(),
		IsClosed:             status.IsClosed(),
	}

	// Detect transitions and emit events
	if status.IsMerged() && !lastSnap.IsMerged {
		m.emit(ctx, PREvent{
			PRNumber:  pr.Number,
			BeadID:    pr.BeadID,
			Anvil:     pr.Anvil,
			Branch:    status.HeadRefName,
			EventType: EventPRMerged,
			Details:   fmt.Sprintf("PR #%d has been merged", pr.Number),
			Timestamp: time.Now(),
		})
		_ = m.db.UpdatePRStatus(pr.Number, state.PRMerged)
		_ = m.db.LogEvent(state.EventPRMerged, fmt.Sprintf("PR #%d merged", pr.Number), pr.BeadID, pr.Anvil)
		_ = m.db.CompleteWorkersByBead(pr.BeadID)
	} else if status.IsClosed() && !lastSnap.IsClosed {
		m.emit(ctx, PREvent{
			PRNumber:  pr.Number,
			BeadID:    pr.BeadID,
			Anvil:     pr.Anvil,
			Branch:    status.HeadRefName,
			EventType: EventPRClosed,
			Details:   fmt.Sprintf("PR #%d has been closed", pr.Number),
			Timestamp: time.Now(),
		})
		_ = m.db.UpdatePRStatus(pr.Number, state.PRClosed)
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
		_ = m.db.UpdatePRStatus(pr.Number, state.PRNeedsFix)
		_ = m.db.LogEvent(state.EventCIFailed, fmt.Sprintf("PR #%d CI checks failed", pr.Number), pr.BeadID, pr.Anvil)
		_ = m.db.LogEvent(state.EventPRNeedsFix, fmt.Sprintf("PR #%d CI failed", pr.Number), pr.BeadID, pr.Anvil)
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
		_ = m.db.UpdatePRStatus(pr.Number, state.PRApproved)
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
		_ = m.db.UpdatePRStatus(pr.Number, state.PRNeedsFix)
		_ = m.db.LogEvent(state.EventReviewChanges, fmt.Sprintf("PR #%d: %s", pr.Number, details), pr.BeadID, pr.Anvil)
		_ = m.db.LogEvent(state.EventPRNeedsFix, fmt.Sprintf("PR #%d: review fix needed", pr.Number), pr.BeadID, pr.Anvil)
	}

	// Update snapshot
	m.mu.Lock()
	m.lastStatuses[pr.Number] = newSnap
	m.mu.Unlock()
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
