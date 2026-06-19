package bellows

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/ingot"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/Robin831/Forge/internal/vcs/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_MinimumInterval(t *testing.T) {
	// Intervals below 30s should be clamped to 30s
	m := New(nil, nil, 5*time.Second, nil, nil, nil, nil, nil)
	assert.Equal(t, 30*time.Second, m.interval)
}

func TestNew_IntervalAboveMin(t *testing.T) {
	m := New(nil, nil, 2*time.Minute, nil, nil, nil, nil, nil)
	assert.Equal(t, 2*time.Minute, m.interval)
}

func TestNew_ExactMinimum(t *testing.T) {
	m := New(nil, nil, 30*time.Second, nil, nil, nil, nil, nil)
	assert.Equal(t, 30*time.Second, m.interval)
}

func TestOnEvent_RegistersHandler(t *testing.T) {
	m := New(nil, nil, time.Minute, nil, nil, nil, nil, nil)
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
	m := New(nil, nil, time.Minute, nil, nil, nil, nil, nil)

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
			old:  prSnapshot{CIPassing: false, AssayUpToDate: true},
			new:  prSnapshot{CIPassing: true, AssayUpToDate: true},
			wantEvents: []string{EventPRReadyToMerge},
		},
		{
			name: "already ready → no pr_ready_to_merge event",
			old:  prSnapshot{CIPassing: true, AssayUpToDate: true},
			new:  prSnapshot{CIPassing: true, AssayUpToDate: true},
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
			name:     "CI checks in progress → no ci_failed even though CIPassing is false",
			old:      prSnapshot{CIPassing: true},
			new:      prSnapshot{CIPassing: false, CIInProgress: true},
			noEvents: []string{EventCIFailed},
		},
		{
			name:     "CI checks in progress with mix of completed failures → no ci_failed",
			old:      prSnapshot{CIPassing: true},
			new:      prSnapshot{CIPassing: false, CIInProgress: true},
			noEvents: []string{EventCIFailed, EventCIPassed},
		},
		{
			name:       "CI checks all completed with failure → ci_failed",
			old:        prSnapshot{CIPassing: true},
			new:        prSnapshot{CIPassing: false, CIInProgress: false},
			wantEvents: []string{EventCIFailed},
		},
		{
			name: "threads resolved while CI passing → pr_ready_to_merge",
			old:  prSnapshot{CIPassing: true, HasUnresolvedThreads: true, AssayUpToDate: true},
			new:  prSnapshot{CIPassing: true, HasUnresolvedThreads: false, AssayUpToDate: true},
			wantEvents: []string{EventPRReadyToMerge},
		},
		{
			name:       "assay pending for current head → not ready even with green CI and no threads",
			old:        prSnapshot{CIPassing: false, AssayUpToDate: false},
			new:        prSnapshot{CIPassing: true, AssayUpToDate: false},
			noEvents:   []string{EventPRReadyToMerge},
		},
		{
			name:       "assay lands clean after green CI → pr_ready_to_merge fires once",
			old:        prSnapshot{CIPassing: true, AssayUpToDate: false},
			new:        prSnapshot{CIPassing: true, AssayUpToDate: true},
			wantEvents: []string{EventPRReadyToMerge},
		},
		{
			// Regression: CI in-progress with overridden CIPassing=true must NOT fire ready-to-merge.
			// In the real code path, when ciInProgress=true the snapshot's CIPassing is overridden
			// to the last completed value; CIInProgress=true is preserved to prevent premature events.
			name:     "CI in-progress (CIPassing overridden to true) → no pr_ready_to_merge",
			old:      prSnapshot{CIPassing: false},
			new:      prSnapshot{CIPassing: true, CIInProgress: true},
			noEvents: []string{EventPRReadyToMerge},
		},
		{
			// After CI completes passing following an in-progress poll, pr_ready_to_merge must fire.
			// lastSnap has CIInProgress=true (from the in-progress poll); newSnap has CIInProgress=false.
			name:       "CI completes passing after in-progress poll → pr_ready_to_merge fires",
			old:        prSnapshot{CIPassing: true, CIInProgress: true, AssayUpToDate: true},
			new:        prSnapshot{CIPassing: true, CIInProgress: false, AssayUpToDate: true},
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
	return computeTransitionEventsWithPR(old, new, "", 0, 5, 0, 5)
}

// computeTransitionEventsWithPR extends computeTransitionEvents with PR-level
// state for the secondary CI and review-fix retry checks. prStatus, ciFixCount
// and reviewFixCount come from the state.PR record; maxCI / maxReview are the
// configured caps.
func computeTransitionEventsWithPR(old, new *prSnapshot, prStatus string, ciFixCount, maxCI, reviewFixCount, maxReview int) []string {
	var events []string

	if new.IsMerged && !old.IsMerged {
		events = append(events, EventPRMerged)
	} else if new.IsClosed && !old.IsClosed {
		events = append(events, EventPRClosed)
	}

	if new.CIPassing && !old.CIPassing {
		events = append(events, EventCIPassed)
	} else if !new.CIPassing && !new.CIInProgress && old.CIPassing {
		events = append(events, EventCIFailed)
	} else if !new.CIPassing && !new.CIInProgress && !old.CIPassing {
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
	} else if new.HasUnresolvedThreads && old.HasUnresolvedThreads {
		// Secondary check: review threads still unresolved after a completed fix attempt.
		if prStatus != "needs_fix" && reviewFixCount > 0 && reviewFixCount < maxReview {
			events = append(events, EventReviewChanges)
		}
	}

	if new.IsConflicting && !old.IsConflicting {
		events = append(events, EventPRConflicting)
	}

	// Ready-to-merge transition: CI passing + no conflicts, unresolved
	// threads, or pending reviews. HasApproval is intentionally excluded
	// because Copilot only submits COMMENTED reviews, never APPROVED.
	// CIInProgress is excluded: a PR is not ready while CI is still running.
	// AssayUpToDate is required so the PR is not declared ready while an Assay
	// review is still pending/in-flight for the current head (Forge-75cx).
	newReady := new.CIPassing && !new.CIInProgress && !new.IsConflicting && !new.HasUnresolvedThreads && !new.HasPendingReviews && new.AssayUpToDate
	lastReady := old.CIPassing && !old.CIInProgress && !old.IsConflicting && !old.HasUnresolvedThreads && !old.HasPendingReviews && old.AssayUpToDate
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

func float64Ptr(v float64) *float64 { return &v }

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

	m := New(db, nil, time.Minute, map[string]string{"my-anvil": "/fake"}, nil, nil, nil, nil)
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

	m := New(db, nil, time.Minute, map[string]string{"my-anvil": "/fake"}, nil, nil, nil, nil)
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
// re-emits EventCIFailed when a previous quench attempt completed but CI
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
		{
			name:       "CI checks in progress after fix attempt → no ci_failed",
			old:        prSnapshot{CIPassing: false},
			new:        prSnapshot{CIPassing: false, CIInProgress: true},
			prStatus:   "open",
			ciFixCount: 1,
			maxCI:      5,
			wantCIFail: false,
		},
		{
			name:       "CI checks in progress on initial transition → no ci_failed",
			old:        prSnapshot{CIPassing: true},
			new:        prSnapshot{CIPassing: false, CIInProgress: true},
			prStatus:   "open",
			ciFixCount: 0,
			maxCI:      5,
			wantCIFail: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fired := computeTransitionEventsWithPR(&tt.old, &tt.new, tt.prStatus, tt.ciFixCount, tt.maxCI, 0, 5)
			if tt.wantCIFail {
				assert.Contains(t, fired, EventCIFailed)
			} else {
				assert.NotContains(t, fired, EventCIFailed)
			}
		})
	}
}

// TestReviewFixRetryLogic verifies the secondary review-changes detection that
// re-emits EventReviewChanges when a previous burnish attempt completed but
// the PR still has unresolved review threads. Mirrors TestCIFixRetryLogic for
// the review-fix path (Forge-gwu4).
func TestReviewFixRetryLogic(t *testing.T) {
	tests := []struct {
		name           string
		old            prSnapshot
		new            prSnapshot
		prStatus       string
		reviewFixCount int
		maxReview      int
		wantReview     bool
	}{
		{
			name:           "threads still unresolved, fix completed (status=open), retries remain → review_changes",
			old:            prSnapshot{HasUnresolvedThreads: true},
			new:            prSnapshot{HasUnresolvedThreads: true},
			prStatus:       "open",
			reviewFixCount: 1,
			maxReview:      5,
			wantReview:     true,
		},
		{
			name:           "threads still unresolved, fix in progress (status=needs_fix) → no event",
			old:            prSnapshot{HasUnresolvedThreads: true},
			new:            prSnapshot{HasUnresolvedThreads: true},
			prStatus:       "needs_fix",
			reviewFixCount: 1,
			maxReview:      5,
			wantReview:     false,
		},
		{
			name:           "threads still unresolved, max attempts reached → no event",
			old:            prSnapshot{HasUnresolvedThreads: true},
			new:            prSnapshot{HasUnresolvedThreads: true},
			prStatus:       "open",
			reviewFixCount: 5,
			maxReview:      5,
			wantReview:     false,
		},
		{
			name:           "threads still unresolved, no previous fix attempts → no event",
			old:            prSnapshot{HasUnresolvedThreads: true},
			new:            prSnapshot{HasUnresolvedThreads: true},
			prStatus:       "open",
			reviewFixCount: 0,
			maxReview:      5,
			wantReview:     false,
		},
		{
			name:           "threads still unresolved, attempt 4 of 5 → review_changes",
			old:            prSnapshot{HasUnresolvedThreads: true},
			new:            prSnapshot{HasUnresolvedThreads: true},
			prStatus:       "open",
			reviewFixCount: 4,
			maxReview:      5,
			wantReview:     true,
		},
		{
			name:           "threads transition 0→>0 still fires via primary branch",
			old:            prSnapshot{HasUnresolvedThreads: false},
			new:            prSnapshot{HasUnresolvedThreads: true},
			prStatus:       "open",
			reviewFixCount: 0,
			maxReview:      5,
			wantReview:     true,
		},
		{
			name:           "genuinely zero threads on both polls → no event",
			old:            prSnapshot{HasUnresolvedThreads: false},
			new:            prSnapshot{HasUnresolvedThreads: false},
			prStatus:       "open",
			reviewFixCount: 1,
			maxReview:      5,
			wantReview:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fired := computeTransitionEventsWithPR(&tt.old, &tt.new, tt.prStatus, 0, 5, tt.reviewFixCount, tt.maxReview)
			if tt.wantReview {
				assert.Contains(t, fired, EventReviewChanges)
			} else {
				assert.NotContains(t, fired, EventReviewChanges)
			}
		})
	}
}

// TestCheckPR_ReviewStillUnresolved_ReemitsEvent is the end-to-end regression
// test for Forge-gwu4: after a burnish cycle completes (PR.Status flipped back
// to open, ReviewFixCount=1) and unresolved review threads remain, the next
// bellows poll must re-emit EventReviewChanges and flip the PR back to
// needs_fix so a new burnish dispatches.
func TestCheckPR_ReviewStillUnresolved_ReemitsEvent(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	// Insert a PR that already went through one burnish cycle: open status,
	// ReviewFixCount=1, threads still unresolved. The seeding guard at
	// bellows.go:415-417 preserves HasUnresolvedThreads=true when
	// ReviewFixCount > 0, so lastSnap.HasUnresolvedThreads=true on the first
	// poll. Combined with newSnap.HasUnresolvedThreads=true from the VCS
	// status, the primary transition branch (new && !last) cannot fire — only
	// the secondary "still unresolved" branch can.
	pr := &state.PR{
		Number:         700,
		Anvil:          "test-anvil",
		BeadID:         "forge-stillunresolved",
		Branch:         "forge/forge-stillunresolved",
		Status:         state.PROpen,
		ReviewFixCount: 1,
		CreatedAt:      time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	// InsertPR does not write the mergeability flags; set them explicitly.
	require.NoError(t, db.UpdatePRMergeability(pr.ID, true, false, true, false, false, true))

	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State:             "OPEN",
		UnresolvedThreads: 2,
	}}

	var events []string
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute,
		map[string]string{"test-anvil": "/fake"}, nil, nil,
		func() int { return 5 }, nil)
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	assert.Contains(t, events, EventReviewChanges,
		"EventReviewChanges must re-fire when threads stay unresolved across a burnish cycle")

	updated, err := db.GetPRByID(pr.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PRNeedsFix, updated.Status,
		"PR must be flipped back to needs_fix so a new burnish dispatches")
}

// TestCheckPR_ReviewStillUnresolved_RespectsMaxAttempts verifies that the
// retry cap honoured by the still-unresolved branch matches max_review_fix_attempts.
func TestCheckPR_ReviewStillUnresolved_RespectsMaxAttempts(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:         701,
		Anvil:          "test-anvil",
		BeadID:         "forge-reviewcap",
		Branch:         "forge/forge-reviewcap",
		Status:         state.PROpen,
		ReviewFixCount: 5, // at the cap
		CreatedAt:      time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	require.NoError(t, db.UpdatePRMergeability(pr.ID, true, false, true, false, false, true))

	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State:             "OPEN",
		UnresolvedThreads: 2,
	}}

	var events []string
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute,
		map[string]string{"test-anvil": "/fake"}, nil, nil,
		func() int { return 5 }, nil)
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	assert.NotContains(t, events, EventReviewChanges,
		"EventReviewChanges must NOT fire once the review-fix cap is reached")

	updated, err := db.GetPRByID(pr.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PROpen, updated.Status,
		"PR status must not be flipped to needs_fix when the cap is reached")
}

// TestCheckPR_ReviewStillUnresolved_SkipsInFlightBurnish verifies that a PR
// already in needs_fix (a burnish is in flight) does not receive a duplicate
// dispatch from the still-unresolved branch.
func TestCheckPR_ReviewStillUnresolved_SkipsInFlightBurnish(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:         702,
		Anvil:          "test-anvil",
		BeadID:         "forge-reviewinflight",
		Branch:         "forge/forge-reviewinflight",
		Status:         state.PRNeedsFix,
		ReviewFixCount: 1,
		CreatedAt:      time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	require.NoError(t, db.UpdatePRMergeability(pr.ID, true, false, true, false, false, true))

	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State:             "OPEN",
		UnresolvedThreads: 2,
	}}

	var events []string
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute,
		map[string]string{"test-anvil": "/fake"}, nil, nil,
		func() int { return 5 }, nil)
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	assert.NotContains(t, events, EventReviewChanges,
		"EventReviewChanges must NOT fire while a burnish is already in flight (status=needs_fix)")
}

// assayEnabledMonitor builds a Monitor whose Assay trigger is enabled for the
// given anvil, with a generous debounce/cost budget so the gate logic — not a
// secondary limit — is what is under test.
func assayEnabledMonitor(db *state.DB, fake vcs.Provider, anvil string) *Monitor {
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute,
		map[string]string{anvil: "/fake"}, nil, nil, func() int { return 5 }, nil)
	m.SetAssayConfig(func(string) AssayGateConfig {
		return AssayGateConfig{Enabled: true, DebounceSeconds: 1}
	})
	return m
}

// TestCheckPR_AssayPending_BlocksReadyToMerge is the core regression test for
// Forge-75cx: a PR with green CI, no conflicts, and no unresolved threads must
// NOT be announced ready-to-merge while its current head has not yet been
// assayed. Otherwise the in-flight Assay would post findings a poll later and
// bounce the PR back to Burnish after it had already been declared ready.
func TestCheckPR_AssayPending_BlocksReadyToMerge(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    900,
		Anvil:     "test-anvil",
		BeadID:    "forge-assaypending",
		Branch:    "forge/forge-assaypending",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	// Green CI, no threads, no pending reviews — would be ready if not for the
	// missing Assay. No assay_runs row exists, so LastReviewedSHA != head.
	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State:   "OPEN",
		HeadSHA: "deadbeefcafe",
	}}

	var events []string
	m := assayEnabledMonitor(db, fake, "test-anvil")
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	assert.NotContains(t, events, EventPRReadyToMerge,
		"ready-to-merge must NOT fire while the current head is unassayed")
	assert.Contains(t, events, EventPRReviewNeeded,
		"an Assay review should be requested for the unassayed head")

	// The DB-level readiness queries must agree: a pending Assay worker blocks
	// readiness even though CI/threads/conflicts are clean.
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID:        "assay-test-anvil-900",
		BeadID:    pr.BeadID,
		Anvil:     pr.Anvil,
		Status:    state.WorkerRunning,
		Phase:     "assay",
		PRNumber:  pr.Number,
		StartedAt: time.Now(),
	}))
	ready, err := db.IsPRReadyToMerge(pr.ID)
	require.NoError(t, err)
	assert.False(t, ready, "IsPRReadyToMerge must be false while an Assay worker is in flight")
	readyList, err := db.ReadyToMergePRs()
	require.NoError(t, err)
	assert.Empty(t, readyList, "ReadyToMergePRs must exclude PRs with an in-flight Assay worker")
}

// TestCheckPR_AssayClean_FiresReadyToMerge verifies the rising edge: once the
// current head has been assayed cleanly (LastReviewedSHA == head, no findings),
// the next poll emits EventPRReadyToMerge exactly once. This mirrors production,
// where the Assay worker calls ResetPRState on completion, forcing a re-seed.
func TestCheckPR_AssayClean_FiresReadyToMerge(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	const head = "deadbeefcafe"
	pr := &state.PR{
		Number:    901,
		Anvil:     "test-anvil",
		BeadID:    "forge-assayclean",
		Branch:    "forge/forge-assayclean",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State:   "OPEN",
		HeadSHA: head,
	}}

	var events []string
	m := assayEnabledMonitor(db, fake, "test-anvil")
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	// Poll 1: head unassayed → not ready.
	m.checkAll(context.Background())
	assert.NotContains(t, events, EventPRReadyToMerge,
		"ready must not fire before the head is assayed")

	// Assay lands cleanly for the current head and resets the snapshot, exactly
	// as the Assay worker does on completion.
	require.NoError(t, db.RecordAssayRun(&state.AssayRun{
		Anvil:    pr.Anvil,
		PRNumber: pr.Number,
		HeadSHA:  head,
	}))
	m.ResetPRState(pr.Anvil, pr.Number)

	events = nil
	// Poll 2: head is now assayed → ready fires.
	m.checkAll(context.Background())
	readyCount := 0
	for _, e := range events {
		if e == EventPRReadyToMerge {
			readyCount++
		}
	}
	assert.Equal(t, 1, readyCount,
		"ready-to-merge must fire exactly once after a clean assay lands; got events: %v", events)
}

// TestCheckPR_AssayDisabled_ReadyFiresImmediately verifies that the new gate is
// inert when Assay is disabled: a clean PR is announced ready on first sighting
// just as before, and the DB readiness queries surface it.
func TestCheckPR_AssayDisabled_ReadyFiresImmediately(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    902,
		Anvil:     "test-anvil",
		BeadID:    "forge-assaydisabled",
		Branch:    "forge/forge-assaydisabled",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State:   "OPEN",
		HeadSHA: "deadbeefcafe",
	}}

	var events []string
	// No SetAssayConfig → Assay disabled.
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute,
		map[string]string{"test-anvil": "/fake"}, nil, nil, func() int { return 5 }, nil)
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	assert.Contains(t, events, EventPRReadyToMerge,
		"ready-to-merge must fire immediately for a clean PR when Assay is disabled")

	ready, err := db.IsPRReadyToMerge(pr.ID)
	require.NoError(t, err)
	assert.True(t, ready, "IsPRReadyToMerge must be true when Assay is disabled and the PR is clean")
}

// TestCheckPR_AssayEmptyHeadSHA_DoesNotBlockReady verifies that when GitHub
// returns an empty HeadSHA, AssayUpToDate is true and does not permanently
// block the ready-to-merge transition.
func TestCheckPR_AssayEmptyHeadSHA_DoesNotBlockReady(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    903,
		Anvil:     "test-anvil",
		BeadID:    "forge-assayemptysha",
		Branch:    "forge/forge-assayemptysha",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State:   "OPEN",
		HeadSHA: "", // empty — GitHub did not report it
	}}

	var events []string
	m := assayEnabledMonitor(db, fake, "test-anvil")
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	assert.Contains(t, events, EventPRReadyToMerge,
		"ready-to-merge must fire when HeadSHA is empty (no Assay can be dispatched)")
}

// TestCheckPR_AssayCostQueryError_DoesNotBlockReady verifies that when the
// daily Assay cost query fails (dailyAssayCost is nil), the Assay gate does
// not block the ready-to-merge transition — since no Assay can be dispatched
// without a cost check, blocking readiness would be permanent.
func TestCheckPR_AssayCostQueryError_DoesNotBlockReady(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    904,
		Anvil:     "test-anvil",
		BeadID:    "forge-assaycosterr",
		Branch:    "forge/forge-assaycosterr",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State:   "OPEN",
		HeadSHA: "deadbeefcafe",
	}}

	var events []string
	m := assayEnabledMonitor(db, fake, "test-anvil")
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	// Simulate nil dailyAssayCost by calling checkPR directly with nil.
	m.checkPR(context.Background(), pr, nil)

	assert.Contains(t, events, EventPRReadyToMerge,
		"ready-to-merge must fire when dailyAssayCost is nil (no Assay can be dispatched)")
}

// TestCheckPR_StillConflicting_ReemitsEvent is the end-to-end regression test
// for Forge-h2a6: after a rebase action dispatched (whether it ran or bailed
// at worktree creation) and the PR is still CONFLICTING, the next bellows
// poll must re-emit EventPRConflicting and flip the PR back to needs_fix so a
// new rebase dispatches.
func TestCheckPR_StillConflicting_ReemitsEvent(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	// Insert a PR that already went through one rebase cycle: open status,
	// RebaseCount=1, conflict still present. The seeding guard preserves
	// IsConflicting=true when RebaseCount > 0, so lastSnap.IsConflicting=true
	// on the first poll. Combined with newSnap.IsConflicting=true from the
	// VCS status, the primary transition branch (new && !last) cannot fire —
	// only the secondary "still conflicting" branch can.
	pr := &state.PR{
		Number:      800,
		Anvil:       "test-anvil",
		BeadID:      "forge-stillconflict",
		Branch:      "forge/forge-stillconflict",
		Status:      state.PROpen,
		RebaseCount: 1,
		CreatedAt:   time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	// InsertPR does not persist rebase_count or mergeability flags; set them explicitly.
	require.NoError(t, db.UpdatePRLifecycle(pr.ID, 0, 0, 1, true))
	require.NoError(t, db.UpdatePRMergeability(pr.ID, true, true, false, false, false, true))

	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State:     "OPEN",
		Mergeable: "CONFLICTING",
	}}

	var events []string
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute,
		map[string]string{"test-anvil": "/fake"}, nil, nil, nil,
		func() int { return 3 })
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	assert.Contains(t, events, EventPRConflicting,
		"EventPRConflicting must re-fire when the PR stays conflicting across a rebase cycle")

	updated, err := db.GetPRByID(pr.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PRNeedsFix, updated.Status,
		"PR must be flipped back to needs_fix so a new rebase dispatches")
}

// TestCheckPR_StillConflicting_RespectsMaxAttempts verifies that the retry
// cap honoured by the still-conflicting branch matches max_rebase_attempts.
func TestCheckPR_StillConflicting_RespectsMaxAttempts(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:      801,
		Anvil:       "test-anvil",
		BeadID:      "forge-rebasecap",
		Branch:      "forge/forge-rebasecap",
		Status:      state.PROpen,
		RebaseCount: 3, // at the cap
		CreatedAt:   time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	require.NoError(t, db.UpdatePRLifecycle(pr.ID, 0, 0, 3, true))
	require.NoError(t, db.UpdatePRMergeability(pr.ID, true, true, false, false, false, true))

	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State:     "OPEN",
		Mergeable: "CONFLICTING",
	}}

	var events []string
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute,
		map[string]string{"test-anvil": "/fake"}, nil, nil, nil,
		func() int { return 3 })
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	assert.NotContains(t, events, EventPRConflicting,
		"EventPRConflicting must NOT fire once the rebase cap is reached")

	updated, err := db.GetPRByID(pr.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PROpen, updated.Status,
		"PR status must not be flipped to needs_fix when the cap is reached")
}

// TestCheckPR_StillConflicting_SkipsInFlightRebase verifies that a PR
// already in needs_fix (a rebase is in flight) does not receive a duplicate
// dispatch from the still-conflicting branch.
func TestCheckPR_StillConflicting_SkipsInFlightRebase(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:      802,
		Anvil:       "test-anvil",
		BeadID:      "forge-rebaseinflight",
		Branch:      "forge/forge-rebaseinflight",
		Status:      state.PRNeedsFix,
		RebaseCount: 1,
		CreatedAt:   time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	require.NoError(t, db.UpdatePRLifecycle(pr.ID, 0, 0, 1, true))
	require.NoError(t, db.UpdatePRMergeability(pr.ID, true, true, false, false, false, true))

	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State:     "OPEN",
		Mergeable: "CONFLICTING",
	}}

	var events []string
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute,
		map[string]string{"test-anvil": "/fake"}, nil, nil, nil,
		func() int { return 3 })
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	assert.NotContains(t, events, EventPRConflicting,
		"EventPRConflicting must NOT fire while a rebase is already in flight (status=needs_fix)")
}

// TestCheckPR_CIInProgressDoesNotTriggerFailure verifies that bellows does not
// emit ci_failed when CI checks are still running (in_progress/queued).
func TestCheckPR_CIInProgressDoesNotTriggerFailure(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    40,
		Anvil:     "test-anvil",
		BeadID:    "forge-inprogress",
		Branch:    "forge/forge-inprogress",
		Status:    state.PROpen,
		CIPassing: true, // seed as passing so transition would normally fire
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	// Simulate CI with one passing check and one still running.
	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State: "OPEN",
		StatusCheckRollup: []vcs.CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Name: "test", Status: "IN_PROGRESS", Conclusion: ""},
		},
	}}

	var events []string
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute, map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	assert.NotContains(t, events, EventCIFailed, "ci_failed must not fire while checks are in progress")
	assert.NotContains(t, events, EventCIPassed, "ci_passed must not fire while checks are incomplete")

	// Regression: ci_passing must be false in the DB while CI is still running
	// so the Ready-to-Merge panel does not show the PR prematurely.
	updated, err := db.GetPRByID(pr.ID)
	require.NoError(t, err)
	assert.False(t, updated.CIPassing, "ci_passing must be false in DB while CI is in progress")
}

// TestCheckPR_CICompletedFailureTriggersCIFailed verifies that bellows correctly
// emits ci_failed when all checks have completed and at least one has failed.
func TestCheckPR_CICompletedFailureTriggersCIFailed(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    41,
		Anvil:     "test-anvil",
		BeadID:    "forge-cifail",
		Branch:    "forge/forge-cifail",
		Status:    state.PROpen,
		CIPassing: true, // seed as passing so the transition fires
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	// All checks completed, one failure.
	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State: "OPEN",
		StatusCheckRollup: []vcs.CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"},
		},
	}}

	var events []string
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute, map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	assert.Contains(t, events, EventCIFailed, "ci_failed should fire when all checks completed with failure")
}

// TestCheckPR_PassingThenInProgressThenFailure is a regression test for the
// sequence: passing → in_progress (no event) → completed with failure (must
// emit ci_failed exactly once). This verifies that bellows does not lose the
// last completed CIPassing state when a poll sees checks still running.
func TestCheckPR_PassingThenInProgressThenFailure(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    42,
		Anvil:     "test-anvil",
		BeadID:    "forge-pending-then-fail",
		Branch:    "forge/forge-pending-then-fail",
		Status:    state.PROpen,
		CIPassing: true, // seed as passing
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	fake := &fakeVCSProvider{}
	var events []string
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute, map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	// Poll 1: CI was passing, now one check is in-progress → no ci_failed.
	fake.status = &vcs.PRStatus{
		State: "OPEN",
		StatusCheckRollup: []vcs.CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Name: "test", Status: "IN_PROGRESS", Conclusion: ""},
		},
	}
	m.checkAll(context.Background())
	assert.NotContains(t, events, EventCIFailed, "ci_failed must not fire while checks are in progress")

	// Poll 2: All checks completed, one failure → ci_failed must fire.
	fake.status = &vcs.PRStatus{
		State: "OPEN",
		StatusCheckRollup: []vcs.CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"},
		},
	}
	m.checkAll(context.Background())
	assert.Contains(t, events, EventCIFailed, "ci_failed must fire when checks finish failing after a pending period")
}

// TestCheckPR_StatusContextPendingBlocksCIFailed verifies that a StatusContext
// check in PENDING state prevents ci_failed from firing, even if a CheckRun
// has already completed with failure. This covers repos that use both GitHub
// Actions (CheckRun) and legacy commit statuses (StatusContext).
func TestCheckPR_StatusContextPendingBlocksCIFailed(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    50,
		Anvil:     "test-anvil",
		BeadID:    "forge-statusctx",
		Branch:    "forge/forge-statusctx",
		Status:    state.PROpen,
		CIPassing: true,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	// One CheckRun completed with failure, one StatusContext still pending.
	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State: "OPEN",
		StatusCheckRollup: []vcs.CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "FAILURE"},
			{State: "PENDING", Context: "ci/integration"},
		},
	}}

	var events []string
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute, map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	assert.NotContains(t, events, EventCIFailed, "ci_failed must not fire while StatusContext checks are pending")
	assert.NotContains(t, events, EventCIPassed, "ci_passed must not fire while StatusContext checks are pending")
}

// TestCheckPR_CompletedEmptyConclusionBlocksCIFailed verifies that a CheckRun
// with status COMPLETED but empty conclusion (transient state) prevents
// premature ci_failed.
func TestCheckPR_CompletedEmptyConclusionBlocksCIFailed(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    51,
		Anvil:     "test-anvil",
		BeadID:    "forge-transient",
		Branch:    "forge/forge-transient",
		Status:    state.PROpen,
		CIPassing: true,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	// One check completed with failure, another COMPLETED but no conclusion yet (transient).
	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State: "OPEN",
		StatusCheckRollup: []vcs.CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "FAILURE"},
			{Name: "deploy", Status: "COMPLETED", Conclusion: ""},
		},
	}}

	var events []string
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute, map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	assert.NotContains(t, events, EventCIFailed, "ci_failed must not fire when a check has COMPLETED with empty conclusion (transient)")
}

// TestCheckPR_TitleBackfill verifies that when a PR has an empty title in the
// database and CheckStatus returns a title from the VCS API, the title is
// persisted to both the prs and workers tables.
func TestCheckPR_TitleBackfill(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    99,
		Anvil:     "test-anvil",
		BeadID:    "forge-titletest",
		Branch:    "forge/forge-titletest",
		Status:    state.PROpen,
		Title:     "", // empty — simulates a PR created before the fix
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State: "OPEN",
		Title: "My PR title",
	}}

	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute, map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)
	m.checkAll(context.Background())

	updated, err := db.GetPRByID(pr.ID)
	require.NoError(t, err)
	assert.Equal(t, "My PR title", updated.Title, "PR title should be backfilled from VCS status")

	workers, err := db.ActiveWorkers()
	require.NoError(t, err)
	var workerTitle string
	for _, w := range workers {
		if w.ID == "bellows-test-anvil-99" {
			workerTitle = w.Title
			break
		}
	}
	assert.Equal(t, "My PR title", workerTitle, "worker title should be backfilled from VCS status")
}

// fakeVCSProvider is a minimal vcs.Provider that returns a fixed PRStatus.
type fakeVCSProvider struct {
	status *vcs.PRStatus
	// checkStatusFunc, when non-nil, overrides the fixed status and is passed
	// the 1-based call count so tests can simulate a transient CheckStatus
	// failure followed by success on a retry.
	checkStatusFunc  func(call int) (*vcs.PRStatus, error)
	checkStatusCalls int
}

func (f *fakeVCSProvider) CreatePR(context.Context, vcs.CreateParams) (*vcs.PR, error) {
	return nil, nil
}
func (f *fakeVCSProvider) MergePR(context.Context, string, int, string) error { return nil }
func (f *fakeVCSProvider) CheckStatus(context.Context, string, int) (*vcs.PRStatus, error) {
	f.checkStatusCalls++
	if f.checkStatusFunc != nil {
		return f.checkStatusFunc(f.checkStatusCalls)
	}
	return f.status, nil
}
func (f *fakeVCSProvider) CheckStatusLight(context.Context, string, int) (*vcs.PRStatus, error) {
	return f.status, nil
}
func (f *fakeVCSProvider) ListOpenPRs(context.Context, string) ([]vcs.OpenPR, error) {
	return nil, nil
}
func (f *fakeVCSProvider) GetPRByHeadBranch(_ context.Context, _ string, _ string) (*vcs.OpenPR, error) {
	return nil, nil
}
func (f *fakeVCSProvider) GetRepoOwnerAndName(context.Context, string) (string, string, error) {
	return "owner", "repo", nil
}
func (f *fakeVCSProvider) FetchUnresolvedThreadCount(context.Context, string, int) (int, error) {
	return 0, nil
}
func (f *fakeVCSProvider) FetchPendingReviewRequests(context.Context, string, int) ([]vcs.ReviewRequest, error) {
	return nil, nil
}
func (f *fakeVCSProvider) FetchPRChecks(context.Context, string, int) (string, []vcs.CICheck, error) {
	return "", nil, nil
}
func (f *fakeVCSProvider) FetchCILogs(context.Context, string, []vcs.CICheck) (map[string]string, error) {
	return nil, nil
}
func (f *fakeVCSProvider) FetchReviewComments(context.Context, string, int) ([]vcs.ReviewComment, error) {
	return nil, nil
}
func (f *fakeVCSProvider) ResolveThread(context.Context, string, string) error { return nil }
func (f *fakeVCSProvider) Platform() vcs.Platform                              { return "fake" }

// TestCheckPR_TransientCheckStatusRetried verifies that a transient gh/GitHub
// failure on the CI/review status fetch (e.g. a momentary 401 during the
// 2026-06-10 blip) is retried rather than flapping the PR through needs_fix.
// Covers Forge-ficr acceptance criterion (6).
func TestCheckPR_TransientCheckStatusRetried(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    800,
		Anvil:     "test-anvil",
		BeadID:    "forge-transient",
		Branch:    "forge/forge-transient",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	require.NoError(t, db.UpdatePRMergeability(pr.ID, true, false, false, false, false, true))

	fake := &fakeVCSProvider{
		checkStatusFunc: func(call int) (*vcs.PRStatus, error) {
			if call == 1 {
				return nil, fmt.Errorf("gh api failed: HTTP 401: Bad credentials")
			}
			return &vcs.PRStatus{State: "OPEN"}, nil
		},
	}

	var events []string
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute,
		map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)
	zero := github.RetryBackoff{} // no real sleeps in tests
	m.retryBackoff = &zero
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	assert.Equal(t, 2, fake.checkStatusCalls,
		"CheckStatus must be retried once after the transient 401")
	assert.NotContains(t, events, EventCIFailed,
		"a transient status-fetch blip must not flap the PR into a CI-failed event")

	updated, err := db.GetPRByID(pr.ID)
	require.NoError(t, err)
	assert.NotEqual(t, state.PRNeedsFix, updated.Status,
		"a transient status-fetch blip must not flap the PR into needs_fix")
}

// TestCheckPR_PermanentCheckStatusNoRetry verifies that a permanent gh error on
// the status fetch surfaces immediately without burning retries.
func TestCheckPR_PermanentCheckStatusNoRetry(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    801,
		Anvil:     "test-anvil",
		BeadID:    "forge-permanent",
		Branch:    "forge/forge-permanent",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	fake := &fakeVCSProvider{
		checkStatusFunc: func(int) (*vcs.PRStatus, error) {
			return nil, fmt.Errorf("gh api failed: HTTP 404: Not Found")
		},
	}

	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute,
		map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)
	zero := github.RetryBackoff{}
	m.retryBackoff = &zero

	m.checkAll(context.Background())

	assert.Equal(t, 1, fake.checkStatusCalls,
		"a permanent 404 must NOT be retried")
}

// seedIngot inserts an ingot row so UpdateIngotStatus has something to update.
func seedIngot(t *testing.T, sqlDB *sql.DB, beadID, anvil string) {
	t.Helper()
	require.NoError(t, ingot.InsertIngot(sqlDB, &ingot.Ingot{
		BeadID: beadID,
		Anvil:  anvil,
		Status: ingot.StatusPROpen,
	}))
}

// TestCheckPR_PRMergedUpdatesIngotStatus verifies that when bellows detects a
// PR merge, it updates the corresponding ingot status to "pr_merged".
func TestCheckPR_PRMergedUpdatesIngotStatus(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    10,
		Anvil:     "test-anvil",
		BeadID:    "forge-merge",
		Branch:    "forge/forge-merge",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	seedIngot(t, db.Conn(), pr.BeadID, pr.Anvil)

	fake := &fakeVCSProvider{status: &vcs.PRStatus{State: "MERGED"}}
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute, map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)

	m.checkAll(context.Background())

	got, err := ingot.GetIngot(db.Conn(), pr.BeadID, pr.Anvil)
	require.NoError(t, err)
	assert.Equal(t, ingot.StatusPRMerged, got.Status)
}

// TestCheckPR_PRClosedUpdatesIngotStatus verifies that when bellows detects a
// PR closed without merge, it updates the corresponding ingot status to "failed".
func TestCheckPR_PRClosedUpdatesIngotStatus(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    20,
		Anvil:     "test-anvil",
		BeadID:    "forge-close",
		Branch:    "forge/forge-close",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	seedIngot(t, db.Conn(), pr.BeadID, pr.Anvil)

	fake := &fakeVCSProvider{status: &vcs.PRStatus{State: "CLOSED"}}
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute, map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)

	m.checkAll(context.Background())

	got, err := ingot.GetIngot(db.Conn(), pr.BeadID, pr.Anvil)
	require.NoError(t, err)
	assert.Equal(t, ingot.StatusFailed, got.Status)
}

// TestCheckPR_MissingIngotIsNoOp verifies that when no ingot row exists for a
// PR, UpdateIngotStatus silently does nothing (returns nil, 0 rows updated)
// and bellows still completes its normal PR lifecycle handling.
func TestCheckPR_MissingIngotIsNoOp(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    30,
		Anvil:     "test-anvil",
		BeadID:    "forge-noingot",
		Branch:    "forge/forge-noingot",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	// Intentionally do NOT seed an ingot — update will be a no-op

	var events []string
	fake := &fakeVCSProvider{status: &vcs.PRStatus{State: "MERGED"}}
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute, map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	// The PR merged event should still fire even though there's no ingot row.
	assert.Contains(t, events, EventPRMerged)

	// PR status should still be updated in state DB.
	updated, err := db.GetPRByID(pr.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PRMerged, updated.Status)
}

// TestCheckPR_NeedsFixToMergedTransition simulates a PR that has been bounced
// into needs_fix locally (e.g. by review comments) and is then merged as-is on
// GitHub by a human. The poller must recognise the GitHub merge regardless of
// the local needs_fix flag — GitHub is the source of truth. This is a
// regression test for a bug where a stuck needs_fix row stayed in needs_fix
// for days after its underlying PR was merged.
func TestCheckPR_NeedsFixToMergedTransition(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	// Insert a PR that is already in needs_fix locally (ReviewFixCount>0
	// to mirror the "bounced by review" production case).
	pr := &state.PR{
		Number:         672,
		Anvil:          "test-anvil",
		BeadID:         "forge-bouncedmerge",
		Branch:         "forge/forge-bouncedmerge",
		Status:         state.PRNeedsFix,
		ReviewFixCount: 1,
		CreatedAt:      time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	// Move the row directly into needs_fix to mimic the production state.
	require.NoError(t, db.UpdatePRStatus(pr.ID, state.PRNeedsFix))
	seedIngot(t, db.Conn(), pr.BeadID, pr.Anvil)

	// GitHub now reports the PR as MERGED (mergedAt non-null translates to
	// state="MERGED" in gh's PRStatus output).
	fake := &fakeVCSProvider{status: &vcs.PRStatus{State: "MERGED"}}

	var events []string
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute, map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	updated, err := db.GetPRByID(pr.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PRMerged, updated.Status, "needs_fix → merged transition must fire when GitHub reports MERGED")
	assert.Contains(t, events, EventPRMerged, "EventPRMerged must be emitted")
}

// TestCheckPR_NeedsFixToClosedTransition mirrors the merged case for the
// closed-without-merge path: a needs_fix PR that the operator (or a bot)
// closes on GitHub without merging must transition the local row to closed.
func TestCheckPR_NeedsFixToClosedTransition(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    673,
		Anvil:     "test-anvil",
		BeadID:    "forge-bouncedclose",
		Branch:    "forge/forge-bouncedclose",
		Status:    state.PRNeedsFix,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	require.NoError(t, db.UpdatePRStatus(pr.ID, state.PRNeedsFix))
	seedIngot(t, db.Conn(), pr.BeadID, pr.Anvil)

	fake := &fakeVCSProvider{status: &vcs.PRStatus{State: "CLOSED"}}

	var events []string
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute, map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})

	m.checkAll(context.Background())

	updated, err := db.GetPRByID(pr.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PRClosed, updated.Status, "needs_fix → closed transition must fire when GitHub reports CLOSED")
	assert.Contains(t, events, EventPRClosed, "EventPRClosed must be emitted")
}

// TestCheckPR_OpenWithCIFailureStillTransitionsToNeedsFix is a regression test
// guarding the original needs_fix path. Tightening the merged/closed gate
// must not affect the open → needs_fix transition when CI fails.
func TestCheckPR_OpenWithCIFailureStillTransitionsToNeedsFix(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    674,
		Anvil:     "test-anvil",
		BeadID:    "forge-cifailregression",
		Branch:    "forge/forge-cifailregression",
		Status:    state.PROpen,
		CIPassing: true,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State: "OPEN",
		StatusCheckRollup: []vcs.CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"},
		},
	}}

	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute, map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)
	m.checkAll(context.Background())

	updated, err := db.GetPRByID(pr.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PRNeedsFix, updated.Status, "open + failing CI must still transition to needs_fix")
}

// TestReconcileTerminalStates verifies the startup backfill: needs_fix PR rows
// whose GitHub state is MERGED or CLOSED get corrected before the normal poll
// loop runs. Only needs_fix rows are reconciled here; open/approved PRs are
// covered by the checkAll that immediately follows, avoiding a double GitHub
// API call for every open PR on startup.
func TestReconcileTerminalStates(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	// PR 1: local needs_fix, GitHub merged.
	mergedPR := &state.PR{
		Number:    701,
		Anvil:     "test-anvil",
		BeadID:    "forge-reconcile-merged",
		Branch:    "forge/forge-reconcile-merged",
		Status:    state.PRNeedsFix,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(mergedPR))
	require.NoError(t, db.UpdatePRStatus(mergedPR.ID, state.PRNeedsFix))
	seedIngot(t, db.Conn(), mergedPR.BeadID, mergedPR.Anvil)

	fake := &fakeVCSProvider{status: &vcs.PRStatus{State: "MERGED"}}
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute, map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)

	m.reconcileTerminalStates(context.Background())

	updated, err := db.GetPRByID(mergedPR.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PRMerged, updated.Status, "reconcile must correct needs_fix → merged for GitHub-merged PRs")

	// PR 2: local needs_fix, GitHub closed (closed without merging).
	closedPR := &state.PR{
		Number:    702,
		Anvil:     "test-anvil",
		BeadID:    "forge-reconcile-closed",
		Branch:    "forge/forge-reconcile-closed",
		Status:    state.PRNeedsFix,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(closedPR))
	require.NoError(t, db.UpdatePRStatus(closedPR.ID, state.PRNeedsFix))
	seedIngot(t, db.Conn(), closedPR.BeadID, closedPR.Anvil)

	fake.status = &vcs.PRStatus{State: "CLOSED"}
	m.reconcileTerminalStates(context.Background())

	updated, err = db.GetPRByID(closedPR.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PRClosed, updated.Status, "reconcile must correct needs_fix → closed for GitHub-closed PRs")

	// PR 3: local open, GitHub closed — NOT reconciled here, handled by checkAll.
	openPR := &state.PR{
		Number:    703,
		Anvil:     "test-anvil",
		BeadID:    "forge-reconcile-open",
		Branch:    "forge/forge-reconcile-open",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(openPR))

	fake.status = &vcs.PRStatus{State: "CLOSED"}
	m.reconcileTerminalStates(context.Background())

	updated, err = db.GetPRByID(openPR.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PROpen, updated.Status, "reconcile must not touch open rows (checkAll handles those)")
}

// TestSweepOrphanedMonitoringWorkers_TerminalPR is the primary regression test
// for the orphaned-worker bug: when an ext-* PR's prs.status is already
// merged/closed (because the unmanaged early-return persisted it before the
// user flipped bellows_managed=1), OpenPRs() never surfaces it again, so the
// normal CompleteWorkersByBead path in checkPR cannot fire. The sweep must
// transition the stranded worker to "done" on its own.
func TestSweepOrphanedMonitoringWorkers_TerminalPR(t *testing.T) {
	cases := []struct {
		name      string
		prStatus  state.PRStatus
		wantSwept bool
	}{
		{name: "merged ext-* PR sweeps its monitoring worker", prStatus: state.PRMerged, wantSwept: true},
		{name: "closed ext-* PR sweeps its monitoring worker", prStatus: state.PRClosed, wantSwept: true},
		{name: "open ext-* PR leaves monitoring worker in place", prStatus: state.PROpen, wantSwept: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := openTempDB(t)
			defer cleanup()

			pr := &state.PR{
				Number:         3073,
				Anvil:          "munin",
				BeadID:         "ext-3073",
				Branch:         "feature/ext-3073",
				Status:         state.PROpen,
				BellowsManaged: true,
				CreatedAt:      time.Now(),
			}
			require.NoError(t, db.InsertPR(pr))
			require.NoError(t, db.UpdatePRStatus(pr.ID, tc.prStatus))

			workerID := "bellows-munin-3073"
			require.NoError(t, db.InsertWorker(&state.Worker{
				ID:        workerID,
				BeadID:    pr.BeadID,
				Anvil:     pr.Anvil,
				Branch:    pr.Branch,
				Status:    state.WorkerMonitoring,
				Phase:     "bellows",
				PRNumber:  pr.Number,
				StartedAt: time.Now(),
			}))

			m := New(db, nil, time.Minute, map[string]string{"munin": "/fake"}, nil, nil, nil, nil)
			m.sweepOrphanedMonitoringWorkers()

			workers, err := db.WorkersByBead(pr.BeadID, pr.Anvil, 0)
			require.NoError(t, err)
			require.Len(t, workers, 1)

			if tc.wantSwept {
				assert.Equal(t, state.WorkerDone, workers[0].Status, "sweep must transition orphaned worker to done")
				require.NotNil(t, workers[0].CompletedAt, "completed_at must be set on swept worker")
				completed, err := db.CompletedWorkers(0)
				require.NoError(t, err)
				found := false
				for _, w := range completed {
					if w.ID == workerID {
						found = true
						break
					}
				}
				assert.True(t, found, "swept worker must surface in CompletedWorkers")
			} else {
				assert.Equal(t, state.WorkerMonitoring, workers[0].Status, "sweep must not touch monitoring workers whose PR is still open")
			}
		})
	}
}

// TestSweepOrphanedMonitoringWorkers_MissingPR verifies that a monitoring
// bellows worker whose prs row has been deleted is still swept to done. This
// guards against the orphan persisting after a manual SQL tweak or a future
// code path that removes a PR row without completing its worker.
func TestSweepOrphanedMonitoringWorkers_MissingPR(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	workerID := "bellows-munin-9999"
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID:        workerID,
		BeadID:    "ext-9999",
		Anvil:     "munin",
		Branch:    "feature/missing",
		Status:    state.WorkerMonitoring,
		Phase:     "bellows",
		PRNumber:  9999,
		StartedAt: time.Now(),
	}))

	m := New(db, nil, time.Minute, map[string]string{"munin": "/fake"}, nil, nil, nil, nil)
	m.sweepOrphanedMonitoringWorkers()

	workers, err := db.WorkersByBead("ext-9999", "munin", 0)
	require.NoError(t, err)
	require.Len(t, workers, 1)
	assert.Equal(t, state.WorkerDone, workers[0].Status)
	require.NotNil(t, workers[0].CompletedAt)
}

// TestSweepOrphanedMonitoringWorkers_IgnoresNonBellowsWorkers verifies that
// the sweep is scoped to phase=bellows and status=monitoring — pipeline
// workers in other phases or statuses must remain untouched.
func TestSweepOrphanedMonitoringWorkers_IgnoresNonBellowsWorkers(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    100,
		Anvil:     "munin",
		BeadID:    "forge-running",
		Branch:    "forge/forge-running",
		Status:    state.PRMerged, // terminal — would be swept if phase matched
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	require.NoError(t, db.UpdatePRStatus(pr.ID, state.PRMerged))

	// A non-bellows worker (e.g. a smith) — must not be swept even if its
	// bead's PR is merged.
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID:        "smith-forge-running",
		BeadID:    pr.BeadID,
		Anvil:     pr.Anvil,
		Branch:    pr.Branch,
		Status:    state.WorkerRunning,
		Phase:     "smith",
		PRNumber:  pr.Number,
		StartedAt: time.Now(),
	}))

	m := New(db, nil, time.Minute, map[string]string{"munin": "/fake"}, nil, nil, nil, nil)
	m.sweepOrphanedMonitoringWorkers()

	w, err := db.WorkersByBead(pr.BeadID, pr.Anvil, 0)
	require.NoError(t, err)
	require.Len(t, w, 1)
	assert.Equal(t, state.WorkerRunning, w[0].Status, "non-bellows workers must not be affected by the sweep")
}

// TestShouldEmitReviewNeeded verifies the Assay trigger gate's pure decision
// function. The base inputs satisfy every condition (so the gate fires); each
// case mutates exactly one signal to confirm it suppresses the trigger,
// mirroring the CI/review still-failing gate-test patterns above.
func TestShouldEmitReviewNeeded(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	base := func() reviewGateInputs {
		return reviewGateInputs{
			enabled:         true,
			managed:         true,
			open:            true,
			draft:           false,
			skipDrafts:      true,
			headSHA:         "abc1234deadbeef",
			lastReviewedSHA: "old999",
			lastAssayRun:    time.Time{},
			now:             now,
			debounceSeconds: 300,
			dailyCostUSD:    0,
			dailyCostLimit:  10,
			runCount:        0,
			maxRuns:         2,
		}
	}

	tests := []struct {
		name   string
		mutate func(*reviewGateInputs)
		want   bool
	}{
		{"all conditions hold", func(in *reviewGateInputs) {}, true},
		{"disabled", func(in *reviewGateInputs) { in.enabled = false }, false},
		{"unmanaged", func(in *reviewGateInputs) { in.managed = false }, false},
		{"not open", func(in *reviewGateInputs) { in.open = false }, false},
		{"draft with skip_drafts", func(in *reviewGateInputs) { in.draft = true }, false},
		{"draft without skip_drafts", func(in *reviewGateInputs) { in.draft = true; in.skipDrafts = false }, true},
		{"head already reviewed", func(in *reviewGateInputs) { in.lastReviewedSHA = in.headSHA }, false},
		{"empty head SHA", func(in *reviewGateInputs) { in.headSHA = "" }, false},
		{"within debounce window", func(in *reviewGateInputs) { in.lastAssayRun = now.Add(-100 * time.Second) }, false},
		{"outside debounce window", func(in *reviewGateInputs) { in.lastAssayRun = now.Add(-400 * time.Second) }, true},
		{"within default debounce window", func(in *reviewGateInputs) { in.debounceSeconds = 0; in.lastAssayRun = now.Add(-100 * time.Second) }, false},
		{"outside default debounce window", func(in *reviewGateInputs) { in.debounceSeconds = 0; in.lastAssayRun = now.Add(-400 * time.Second) }, true},
		{"daily cost at limit", func(in *reviewGateInputs) { in.dailyCostUSD = 10; in.dailyCostLimit = 10 }, false},
		{"daily cost over limit", func(in *reviewGateInputs) { in.dailyCostUSD = 15; in.dailyCostLimit = 10 }, false},
		{"daily cost with no limit", func(in *reviewGateInputs) { in.dailyCostUSD = 99; in.dailyCostLimit = 0 }, true},
		{"under run cap", func(in *reviewGateInputs) { in.runCount = 1; in.maxRuns = 2 }, true},
		{"at run cap", func(in *reviewGateInputs) { in.runCount = 2; in.maxRuns = 2 }, false},
		{"over run cap", func(in *reviewGateInputs) { in.runCount = 5; in.maxRuns = 2 }, false},
		{"run cap disabled", func(in *reviewGateInputs) { in.runCount = 99; in.maxRuns = 0 }, true},
		{"assay already in-flight", func(in *reviewGateInputs) { in.assayInFlight = true }, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base()
			tt.mutate(&in)
			assert.Equal(t, tt.want, shouldEmitReviewNeeded(in))
		})
	}
}

// TestMaybeEmitReviewNeeded_EmitsWhenUnreviewed exercises the gate end-to-end
// against a real state DB: a managed, open, CI-passing PR whose head has never
// been reviewed should emit EventPRReviewNeeded with the head SHA populated.
func TestMaybeEmitReviewNeeded_EmitsWhenUnreviewed(t *testing.T) {
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

	m := New(db, nil, time.Minute, map[string]string{"my-anvil": "/fake"}, nil, nil, nil, nil)
	m.SetAssayConfig(func(anvil string) AssayGateConfig {
		return AssayGateConfig{Enabled: true, SkipDrafts: true, DebounceSeconds: 300, DailyCostLimitUSD: 10}
	})

	var got []PREvent
	var mu sync.Mutex
	m.OnEvent(func(_ context.Context, ev PREvent) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})

	status := &vcs.PRStatus{State: "OPEN", HeadRefName: "forge/forge-abc", HeadSHA: "abc1234", URL: "https://example/pr/101"}
	snap := &prSnapshot{CIPassing: true}
	m.maybeEmitReviewNeeded(context.Background(), pr, status, snap, float64Ptr(0))

	require.Len(t, got, 1)
	assert.Equal(t, EventPRReviewNeeded, got[0].EventType)
	assert.Equal(t, "abc1234", got[0].HeadSHA)
	assert.Equal(t, 101, got[0].PRNumber)
}

// TestMaybeEmitReviewNeeded_SuppressedWhenHeadAlreadyReviewed verifies the gate
// does not re-fire once an Assay run has recorded the current head SHA.
func TestMaybeEmitReviewNeeded_SuppressedWhenHeadAlreadyReviewed(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{
		Number:    202,
		Anvil:     "my-anvil",
		BeadID:    "forge-xyz",
		Branch:    "forge/forge-xyz",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))

	// Record a run against the current head, well outside the debounce window,
	// so only the head-SHA match (not debounce) suppresses the trigger.
	require.NoError(t, db.RecordAssayRun(&state.AssayRun{
		Anvil:     "my-anvil",
		PRNumber:  202,
		HeadSHA:   "abc1234",
		StartedAt: time.Now().Add(-1 * time.Hour),
	}))

	m := New(db, nil, time.Minute, map[string]string{"my-anvil": "/fake"}, nil, nil, nil, nil)
	m.SetAssayConfig(func(anvil string) AssayGateConfig {
		return AssayGateConfig{Enabled: true, SkipDrafts: true, DebounceSeconds: 300, DailyCostLimitUSD: 10}
	})

	var got []PREvent
	var mu sync.Mutex
	m.OnEvent(func(_ context.Context, ev PREvent) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})

	status := &vcs.PRStatus{State: "OPEN", HeadRefName: "forge/forge-xyz", HeadSHA: "abc1234"}
	snap := &prSnapshot{CIPassing: true}
	m.maybeEmitReviewNeeded(context.Background(), pr, status, snap, float64Ptr(0))

	assert.Empty(t, got, "gate must not emit when the current head has already been reviewed")
}

// TestMaybeEmitReviewNeeded_NoConfigIsNoop verifies the gate is inert when no
// Assay config accessor is registered (feature disabled).
func TestMaybeEmitReviewNeeded_NoConfigIsNoop(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := &state.PR{Number: 303, Anvil: "my-anvil", BeadID: "forge-q", Branch: "b", Status: state.PROpen, CreatedAt: time.Now()}
	require.NoError(t, db.InsertPR(pr))

	m := New(db, nil, time.Minute, map[string]string{"my-anvil": "/fake"}, nil, nil, nil, nil)
	var got []PREvent
	m.OnEvent(func(_ context.Context, ev PREvent) { got = append(got, ev) })

	status := &vcs.PRStatus{State: "OPEN", HeadSHA: "abc1234"}
	m.maybeEmitReviewNeeded(context.Background(), pr, status, &prSnapshot{CIPassing: true}, float64Ptr(0))
	assert.Empty(t, got)
}
