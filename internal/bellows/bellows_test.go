package bellows

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
)

func TestNew_MinimumInterval(t *testing.T) {
	// Intervals below 30s should be clamped to 30s
	m := New(nil, 5*time.Second, nil)
	assert.Equal(t, 30*time.Second, m.interval)
}

func TestNew_IntervalAboveMin(t *testing.T) {
	m := New(nil, 2*time.Minute, nil)
	assert.Equal(t, 2*time.Minute, m.interval)
}

func TestNew_ExactMinimum(t *testing.T) {
	m := New(nil, 30*time.Second, nil)
	assert.Equal(t, 30*time.Second, m.interval)
}

func TestOnEvent_RegistersHandler(t *testing.T) {
	m := New(nil, time.Minute, nil)
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
	m := New(nil, time.Minute, nil)

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
		EventReviewChanges, EventPRMerged, EventPRClosed,
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
		name        string
		old         prSnapshot
		new         prSnapshot
		wantEvents  []string
		noEvents    []string
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
			name:    "CI stays passing → no event",
			old:     prSnapshot{CIPassing: true},
			new:     prSnapshot{CIPassing: true},
			noEvents: []string{EventCIPassed, EventCIFailed},
		},
		{
			name:       "approval granted → review_approved",
			old:        prSnapshot{HasApproval: false},
			new:        prSnapshot{HasApproval: true},
			wantEvents: []string{EventReviewApproved},
		},
		{
			name:    "already approved → no event",
			old:     prSnapshot{HasApproval: true},
			new:     prSnapshot{HasApproval: true},
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
			name:    "changes persist → no new event",
			old:     prSnapshot{NeedsChanges: true},
			new:     prSnapshot{NeedsChanges: true},
			noEvents: []string{EventReviewChanges},
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

// TestSnapshotFromPR verifies that snapshotFromPR produces the right initial
// state for each PR status, preventing re-fires of already-known conditions.
func TestSnapshotFromPR(t *testing.T) {
	tests := []struct {
		name          string
		pr            state.PR
		wantCIPassing bool
		wantApproval  bool
		wantNeedsFix  bool
		wantConflict  bool
		wantMerged    bool
		wantClosed    bool
	}{
		{
			name:          "open PR assumes CI passing",
			pr:            state.PR{Status: state.PROpen, CIPassing: true},
			wantCIPassing: true,
		},
		{
			name:          "approved PR has approval + CI passing",
			pr:            state.PR{Status: state.PRApproved, CIPassing: true},
			wantCIPassing: true,
			wantApproval:  true,
		},
		{
			name:         "needs_fix PR seeds all fix flags to suppress re-fires",
			pr:           state.PR{Status: state.PRNeedsFix, CIPassing: false},
			wantNeedsFix: true,
		},
		{
			name:          "open PR with persisted conflict seeds IsConflicting",
			pr:            state.PR{Status: state.PROpen, IsConflicting: true, CIPassing: true},
			wantCIPassing: true,
			wantConflict:  true,
		},
		{
			name:          "needs_fix PR with persisted conflict",
			pr:            state.PR{Status: state.PRNeedsFix, IsConflicting: true, CIPassing: false},
			wantNeedsFix:  true,
			wantConflict:  true,
		},
		{
			name:          "merged PR",
			pr:            state.PR{Status: state.PRMerged, CIPassing: true},
			wantCIPassing: true,
			wantMerged:    true,
		},
		{
			name:          "closed PR",
			pr:            state.PR{Status: state.PRClosed, CIPassing: true},
			wantCIPassing: true,
			wantClosed:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := snapshotFromPR(&tt.pr)
			assert.Equal(t, tt.wantCIPassing, snap.CIPassing, "CIPassing")
			assert.Equal(t, tt.wantApproval, snap.HasApproval, "HasApproval")
			assert.Equal(t, tt.wantConflict, snap.IsConflicting, "IsConflicting")
			assert.Equal(t, tt.wantMerged, snap.IsMerged, "IsMerged")
			assert.Equal(t, tt.wantClosed, snap.IsClosed, "IsClosed")
			if tt.wantNeedsFix {
				assert.False(t, snap.CIPassing, "needs_fix: CIPassing should be false")
				assert.True(t, snap.NeedsChanges, "needs_fix: NeedsChanges should be true")
				assert.True(t, snap.HasUnresolvedThreads, "needs_fix: HasUnresolvedThreads should be true")
			}
		})
	}
}

// TestSeedFromDB verifies that SeedFromDB populates lastStatuses from open PRs.
func TestSeedFromDB(t *testing.T) {
	db, err := state.Open(":memory:")
	if err != nil {
		t.Fatalf("open in-memory DB: %v", err)
	}
	defer db.Close()

	// Insert a few PRs in different states
	prs := []state.PR{
		{Number: 1, Anvil: "test", BeadID: "b1", Status: state.PROpen, CreatedAt: time.Now(), CIPassing: true},
		{Number: 2, Anvil: "test", BeadID: "b2", Status: state.PRNeedsFix, CreatedAt: time.Now(), CIPassing: false},
		{Number: 3, Anvil: "test", BeadID: "b3", Status: state.PRApproved, CreatedAt: time.Now(), CIPassing: true},
	}
	for i := range prs {
		if err := db.InsertPR(&prs[i]); err != nil {
			t.Fatalf("insert PR: %v", err)
		}
	}
	// Set PR #1 as conflicting
	if err := db.UpdatePRConflicting(prs[0].ID, true); err != nil {
		t.Fatalf("set conflicting: %v", err)
	}

	m := New(db, time.Minute, nil)
	if err := m.SeedFromDB(); err != nil {
		t.Fatalf("SeedFromDB: %v", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	assert.Len(t, m.lastStatuses, 3)

	snap1 := m.lastStatuses[prs[0].ID]
	assert.True(t, snap1.CIPassing, "PR#1 (open): CIPassing")
	assert.True(t, snap1.IsConflicting, "PR#1 (open+conflict): IsConflicting")

	snap2 := m.lastStatuses[prs[1].ID]
	assert.False(t, snap2.CIPassing, "PR#2 (needs_fix): CIPassing should be false")
	assert.True(t, snap2.NeedsChanges, "PR#2 (needs_fix): NeedsChanges should be true")

	snap3 := m.lastStatuses[prs[2].ID]
	assert.True(t, snap3.CIPassing, "PR#3 (approved): CIPassing")
	assert.True(t, snap3.HasApproval, "PR#3 (approved): HasApproval")
	}

// computeTransitionEvents mirrors the transition conditions in checkPR,
// returning the event types that would be emitted for a given state change.
// This is used only in tests to verify the logic is correct.
func computeTransitionEvents(old, new *prSnapshot) []string {
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
	}

	if new.HasApproval && !old.HasApproval {
		events = append(events, EventReviewApproved)
	}

	if (new.NeedsChanges && !old.NeedsChanges) || (new.HasUnresolvedThreads && !old.HasUnresolvedThreads) {
		events = append(events, EventReviewChanges)
	}

	return events
}
