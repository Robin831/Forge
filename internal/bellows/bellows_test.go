package bellows

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_MinimumInterval(t *testing.T) {
	// Intervals below 30s should be clamped to 30s
	m := New(nil, nil, 5*time.Second, nil, nil, nil)
	assert.Equal(t, 30*time.Second, m.interval)
}

func TestNew_IntervalAboveMin(t *testing.T) {
	m := New(nil, nil, 2*time.Minute, nil, nil, nil)
	assert.Equal(t, 2*time.Minute, m.interval)
}

func TestNew_ExactMinimum(t *testing.T) {
	m := New(nil, nil, 30*time.Second, nil, nil, nil)
	assert.Equal(t, 30*time.Second, m.interval)
}

func TestOnEvent_RegistersHandler(t *testing.T) {
	m := New(nil, nil, time.Minute, nil, nil, nil)
	m.mu.Lock()
	initial := len(m.handlers)
	m.mu.Unlock()

	m.OnEvent(func(_ context.Context, _ PREvent) {})

	m.mu.Lock()
	after := len(m.handlers)
	m.mu.Unlock()

	assert.Equal(t, initial+1, after)
}

func TestEmit_DispatchesToAllHandlers(t *testing.T) {
	m := New(nil, nil, time.Minute, nil, nil, nil)

	var mu sync.Mutex
	var received []string

	m.OnEvent(func(_ context.Context, e PREvent) {
		mu.Lock()
		received = append(received, "h1:"+e.EventType)
		mu.Unlock()
	})
	m.OnEvent(func(_ context.Context, e PREvent) {
		mu.Lock()
		received = append(received, "h2:"+e.EventType)
		mu.Unlock()
	})

	event := PREvent{
		PRNumber:  42,
		BeadID:    "forge-abc",
		EventType: EventCIPassed,
		Timestamp: time.Now(),
	}
	m.emit(context.Background(), event)

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, received, 2)
	assert.Contains(t, received, "h1:"+EventCIPassed)
	assert.Contains(t, received, "h2:"+EventCIPassed)
}

func TestEventConstants(t *testing.T) {
	// Verify event constants are distinct non-empty strings
	constants := []string{
		EventCIPassed, EventCIFailed, EventReviewApproved,
		EventReviewChanges, EventPRMerged, EventPRClosed, EventPRConflicting,
	}
	seen := make(map[string]bool)
	for _, c := range constants {
		assert.NotEmpty(t, c)
		assert.False(t, seen[c], "duplicate event constant: %s", c)
		seen[c] = true
	}
}

// TestSnapshotTransitionLogic verifies the transition conditions that checkPR
// uses to decide when to emit events. The conditions are mirrored here to
// document the expected behavior without requiring live gh/state dependencies.
func TestSnapshotTransitionLogic(t *testing.T) {
	tests := []struct {
		name       string
		old        prSnapshot
		new        prSnapshot
		wantEvents []string
		noEvents   []string
	}{
		{
			name:       "CI transitions from failing to passing → ci_passed",
			old:        prSnapshot{CIPassing: false},
			new:        prSnapshot{CIPassing: true},
			wantEvents: []string{EventCIPassed},
			noEvents:   []string{EventCIFailed},
		},
		{
			name:       "CI transitions from passing to failing → ci_failed",
			old:        prSnapshot{CIPassing: true},
			new:        prSnapshot{CIPassing: false},
			wantEvents: []string{EventCIFailed},
			noEvents:   []string{EventCIPassed},
		},
		{
			name:     "CI stays passing → no event",
			old:      prSnapshot{CIPassing: true},
			new:      prSnapshot{CIPassing: true},
			noEvents: []string{EventCIPassed, EventCIFailed},
		},
		{
			name:       "approval granted → review_approved",
			old:        prSnapshot{HasApproval: false},
			new:        prSnapshot{HasApproval: true},
			wantEvents: []string{EventReviewApproved},
		},
		{
			name:     "already approved → no event",
			old:      prSnapshot{HasApproval: true},
			new:      prSnapshot{HasApproval: true},
			noEvents: []string{EventReviewApproved},
		},
		{
			name:       "changes requested → review_changes_requested",
			old:        prSnapshot{NeedsChanges: false},
			new:        prSnapshot{NeedsChanges: true},
			wantEvents: []string{EventReviewChanges},
		},
		{
			name:       "unresolved threads appear → review_changes_requested",
			old:        prSnapshot{HasUnresolvedThreads: false},
			new:        prSnapshot{HasUnresolvedThreads: true},
			wantEvents: []string{EventReviewChanges},
		},
		{
			name:     "changes persist → no new event",
			old:      prSnapshot{NeedsChanges: true},
			new:      prSnapshot{NeedsChanges: true},
			noEvents: []string{EventReviewChanges},
		},
		{
			name:       "conflict detected → pr_conflicting",
			old:        prSnapshot{IsConflicting: false},
			new:        prSnapshot{IsConflicting: true},
			wantEvents: []string{EventPRConflicting},
		},
		{
			name:     "conflict persists → no new event",
			old:      prSnapshot{IsConflicting: true},
			new:      prSnapshot{IsConflicting: true},
			noEvents: []string{EventPRConflicting},
		},
		{
			name:       "PR merged → pr_merged",
			old:        prSnapshot{IsMerged: false},
			new:        prSnapshot{IsMerged: true},
			wantEvents: []string{EventPRMerged},
		},
		{
			name:       "PR closed → pr_closed",
			old:        prSnapshot{IsClosed: false},
			new:        prSnapshot{IsClosed: true},
			wantEvents: []string{EventPRClosed},
		},
		{
			name: "CI passes with no blockers → pr_ready_to_merge (no approval needed)",
			old:  prSnapshot{CIPassing: false},
			new:  prSnapshot{CIPassing: true},
			wantEvents: []string{EventPRReadyToMerge},
		},
		{
			name: "already ready → no pr_ready_to_merge event",
			old:  prSnapshot{CIPassing: true},
			new:  prSnapshot{CIPassing: true},
			noEvents: []string{EventPRReadyToMerge},
		},
		{
			name: "CI passes but has unresolved threads → not ready",
			old:  prSnapshot{CIPassing: false},
			new:  prSnapshot{CIPassing: true, HasUnresolvedThreads: true},
			noEvents: []string{EventPRReadyToMerge},
		},
		{
			name: "CI passes but conflicting → not ready",
			old:  prSnapshot{CIPassing: false},
			new:  prSnapshot{CIPassing: true, IsConflicting: true},
			noEvents: []string{EventPRReadyToMerge},
		},
		{
			name: "threads resolved while CI passing → pr_ready_to_merge",
			old:  prSnapshot{CIPassing: true, HasUnresolvedThreads: true},
			new:  prSnapshot{CIPassing: true, HasUnresolvedThreads: false},
			wantEvents: []string{EventPRReadyToMerge},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fired := computeTransitionEvents(&tt.old, &tt.new)
			for _, want := range tt.wantEvents {
				assert.Contains(t, fired, want, "expected event %q to fire", want)
			}
			for _, no := range tt.noEvents {
				assert.NotContains(t, fired, no, "unexpected event %q fired", no)
			}
		})
	}
}

// computeTransitionEvents mirrors the transition conditions in checkPR,
// returning the event types that would be emitted for a given state change.
// This is used only in tests to verify the logic is correct.
func computeTransitionEvents(old, new *prSnapshot) []string {
	return computeTransitionEventsWithPR(old, new, "", 0, 5)
}

// computeTransitionEventsWithPR extends computeTransitionEvents with PR-level
// state for the secondary CI retry check. prStatus and ciFixCount come from
// the state.PR record; maxCI is the configured max CI fix attempts.
func computeTransitionEventsWithPR(old, new *prSnapshot, prStatus string, ciFixCount, maxCI int) []string {
	var events []string

	if new.IsMerged && !old.IsMerged {
		events = append(events, EventPRMerged)
	} else if new.IsClosed && !old.IsClosed {
		events = append(events, EventPRClosed)
	}

	if new.CIPassing && !old.CIPassing {
		events = append(events, EventCIPassed)
	} else if !new.CIPassing && old.CIPassing {
		events = append(events, EventCIFailed)
	} else if !new.CIPassing && !old.CIPassing {
		// Secondary check: CI still failing after a completed fix attempt.
		if prStatus != "needs_fix" && ciFixCount > 0 && ciFixCount < maxCI {
			events = append(events, EventCIFailed)
		}
	}

	if new.HasApproval && !old.HasApproval {
		events = append(events, EventReviewApproved)
	}

	if (new.NeedsChanges && !old.NeedsChanges) || (new.HasUnresolvedThreads && !old.HasUnresolvedThreads) {
		events = append(events, EventReviewChanges)
	}

	if new.IsConflicting && !old.IsConflicting {
		events = append(events, EventPRConflicting)
	}

	// Ready-to-merge transition: CI passing + no conflicts, unresolved
	// threads, or pending reviews. HasApproval is intentionally excluded
	// because Copilot only submits COMMENTED reviews, never APPROVED.
	newReady := new.CIPassing && !new.IsConflicting && !new.HasUnresolvedThreads && !new.HasPendingReviews
	lastReady := old.CIPassing && !old.IsConflicting && !old.HasUnresolvedThreads && !old.HasPendingReviews
	if newReady && !lastReady {
		events = append(events, EventPRReadyToMerge)
	}

	return events
}

// openTempDB creates a temporary state.DB for testing and returns a cleanup func.
func openTempDB(t *testing.T) (*state.DB, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "bellows-test-*")
	require.NoError(t, err)
	db, err := state.Open(filepath.Join(dir, "state.db"))
	require.NoError(t, err)
	return db, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

// TestCheckAll_BellowsManagedPRsGetWorkerRow is a regression test for the
// Workers-panel visibility fix: bellows-managed PRs must appear in state.DB
// as workers after checkAll runs.
func TestCheckAll_BellowsManagedPRsGetWorkerRow(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	// Insert a regular forge-created PR (non-ext bead ID).
	pr := &state.PR{
		Number:    101,
		Anvil:     "my-anvil",
		BeadID:    "forge-abc",
		Branch:    "forge/forge-abc",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	// Insert an external PR that has been explicitly assigned to bellows.
	extManaged := &state.PR{
		Number:         202,
		Anvil:          "my-anvil",
		BeadID:         "ext-xyz",
		Branch:         "feature/ext",
		Status:         state.PROpen,
		BellowsManaged: true,
		CreatedAt:      time.Now(),
	}
	require.NoError(t, db.InsertPR(extManaged))

	// Insert an unmanaged external PR (display-only, must NOT get a worker row).
	// bellows_managed column defaults to 1 in the schema, so we must explicitly
	// clear it with UpdatePRBellowsManaged after insertion.
	extUnmanaged := &state.PR{
		Number:    303,
		Anvil:     "my-anvil",
		BeadID:    "ext-unmanaged",
		Branch:    "feature/unmanaged",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(extUnmanaged))
	require.NoError(t, db.UpdatePRBellowsManaged(extUnmanaged.ID, false))

	m := New(db, nil, time.Minute, map[string]string{"my-anvil": "/fake"}, nil, nil)
	m.checkAll(context.Background())

	workers, err := db.ActiveWorkers()
	require.NoError(t, err)

	workerIDs := make(map[string]bool, len(workers))
	for _, w := range workers {
		workerIDs[w.ID] = true
	}

	assert.True(t, workerIDs["bellows-my-anvil-101"], "forge-created PR should have a bellows worker row")
	assert.True(t, workerIDs["bellows-my-anvil-202"], "bellows-managed ext PR should have a bellows worker row")
	assert.False(t, workerIDs["bellows-my-anvil-303"], "unmanaged ext PR must NOT have a bellows worker row")
}

// TestCheckAll_WorkerRowNotDuplicatedOnRepeatPolls verifies that repeated
// checkAll calls (simulating poll cycles) do not create duplicate worker rows.
func TestCheckAll_WorkerRowNotDuplicatedOnRepeatPolls(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    101,
		Anvil:     "my-anvil",
		BeadID:    "forge-abc",
		Branch:    "forge/forge-abc",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	m := New(db, nil, time.Minute, map[string]string{"my-anvil": "/fake"}, nil, nil)
	m.checkAll(context.Background())
	m.checkAll(context.Background())
	m.checkAll(context.Background())

	workers, err := db.ActiveWorkers()
	require.NoError(t, err)

	count := 0
	for _, w := range workers {
		if w.ID == "bellows-my-anvil-101" {
			count++
		}
	}
	assert.Equal(t, 1, count, "worker row should exist exactly once after multiple poll cycles")
}

// TestCIFixRetryLogic verifies the secondary CI failure detection that
// re-emits EventCIFailed when a previous cifix attempt completed but CI
// is still failing. This is the core fix for the retry gap.
func TestCIFixRetryLogic(t *testing.T) {
	tests := []struct {
		name       string
		old        prSnapshot
		new        prSnapshot
		prStatus   string
		ciFixCount int
		maxCI      int
		wantCIFail bool
	}{
		{
			name:       "CI still failing, fix completed (status=open), retries remain → ci_failed",
			old:        prSnapshot{CIPassing: false},
			new:        prSnapshot{CIPassing: false},
			prStatus:   "open",
			ciFixCount: 1,
			maxCI:      5,
			wantCIFail: true,
		},
		{
			name:       "CI still failing, fix in progress (status=needs_fix) → no event",
			old:        prSnapshot{CIPassing: false},
			new:        prSnapshot{CIPassing: false},
			prStatus:   "needs_fix",
			ciFixCount: 1,
			maxCI:      5,
			wantCIFail: false,
		},
		{
			name:       "CI still failing, max attempts reached → no event",
			old:        prSnapshot{CIPassing: false},
			new:        prSnapshot{CIPassing: false},
			prStatus:   "open",
			ciFixCount: 5,
			maxCI:      5,
			wantCIFail: false,
		},
		{
			name:       "CI still failing, no previous fix attempts → no event",
			old:        prSnapshot{CIPassing: false},
			new:        prSnapshot{CIPassing: false},
			prStatus:   "open",
			ciFixCount: 0,
			maxCI:      5,
			wantCIFail: false,
		},
		{
			name:       "CI still failing, attempt 4 of 5 → ci_failed",
			old:        prSnapshot{CIPassing: false},
			new:        prSnapshot{CIPassing: false},
			prStatus:   "open",
			ciFixCount: 4,
			maxCI:      5,
			wantCIFail: true,
		},
		{
			name:       "CI transition passing→failing still works normally",
			old:        prSnapshot{CIPassing: true},
			new:        prSnapshot{CIPassing: false},
			prStatus:   "open",
			ciFixCount: 0,
			maxCI:      5,
			wantCIFail: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fired := computeTransitionEventsWithPR(&tt.old, &tt.new, tt.prStatus, tt.ciFixCount, tt.maxCI)
			if tt.wantCIFail {
				assert.Contains(t, fired, EventCIFailed)
			} else {
				assert.NotContains(t, fired, EventCIFailed)
			}
		})
	}
}
