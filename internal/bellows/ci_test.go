package bellows

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	headSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	oldSHA  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// evalNow is the fixed "current time" the evaluator tests inject.
var evalNow = time.Date(2026, 8, 6, 19, 0, 0, 0, time.UTC)

// TestEvaluateCI covers the head-SHA filtering and the stuck/pending/failed
// classification that gates quench dispatch (Forge-81kg).
func TestEvaluateCI(t *testing.T) {
	tests := []struct {
		name         string
		head         string
		checks       []vcs.CheckRun
		wantState    ciState
		wantStale    int
		wantInFlight int
	}{
		{
			name:      "no checks at all is passing",
			head:      headSHA,
			checks:    nil,
			wantState: ciPassed,
		},
		{
			name: "stale failure on an old SHA with no runs on head is pending",
			head: headSHA,
			checks: []vcs.CheckRun{
				{Name: "changelog-check", Status: "COMPLETED", Conclusion: "FAILURE", HeadSHA: oldSHA,
					StartedAt: evalNow.Add(-3 * time.Hour), CompletedAt: evalNow.Add(-3 * time.Hour)},
			},
			wantState: ciPending,
			wantStale: 1,
		},
		{
			name: "stale failure alongside a passing run on head is passing",
			head: headSHA,
			checks: []vcs.CheckRun{
				{Name: "changelog-check", Status: "COMPLETED", Conclusion: "FAILURE", HeadSHA: oldSHA},
				{Name: "changelog-check", Status: "COMPLETED", Conclusion: "SUCCESS", HeadSHA: headSHA},
			},
			wantState: ciPassed,
			wantStale: 1,
		},
		{
			name: "failing completed check on head is failed",
			head: headSHA,
			checks: []vcs.CheckRun{
				{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS", HeadSHA: headSHA},
				{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE", HeadSHA: headSHA},
			},
			wantState: ciFailed,
		},
		{
			name: "failing check with no reported SHA is still failed",
			head: headSHA,
			checks: []vcs.CheckRun{
				{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"},
			},
			wantState: ciFailed,
		},
		{
			name: "check queued past the threshold is stuck",
			head: headSHA,
			checks: []vcs.CheckRun{
				{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS", HeadSHA: headSHA},
				{Name: "changelog-check", Status: "QUEUED", HeadSHA: headSHA, StartedAt: evalNow.Add(-45 * time.Minute)},
			},
			wantState:    ciStuck,
			wantInFlight: 1,
		},
		{
			name: "a stuck run masks a failure on the same head",
			head: headSHA,
			checks: []vcs.CheckRun{
				{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE", HeadSHA: headSHA},
				{Name: "changelog-check", Status: "QUEUED", HeadSHA: headSHA, StartedAt: evalNow.Add(-2 * time.Hour)},
			},
			wantState:    ciStuck,
			wantInFlight: 1,
		},
		{
			name: "check queued below the threshold is pending",
			head: headSHA,
			checks: []vcs.CheckRun{
				{Name: "changelog-check", Status: "QUEUED", HeadSHA: headSHA, StartedAt: evalNow.Add(-5 * time.Minute)},
			},
			wantState:    ciPending,
			wantInFlight: 1,
		},
		{
			name: "queued check without a timestamp is pending, never stuck",
			head: headSHA,
			checks: []vcs.CheckRun{
				{Name: "pipeline", Status: "QUEUED", HeadSHA: headSHA},
			},
			wantState:    ciPending,
			wantInFlight: 1,
		},
		{
			name: "a long-running check is slow, not stuck",
			head: headSHA,
			checks: []vcs.CheckRun{
				{Name: "e2e", Status: "IN_PROGRESS", HeadSHA: headSHA, StartedAt: evalNow.Add(-2 * time.Hour)},
			},
			wantState:    ciPending,
			wantInFlight: 1,
		},
		{
			name: "a start time in the future does not read as stuck",
			head: headSHA,
			checks: []vcs.CheckRun{
				{Name: "build", Status: "IN_PROGRESS", HeadSHA: headSHA, StartedAt: evalNow.Add(10 * time.Minute)},
			},
			wantState:    ciPending,
			wantInFlight: 1,
		},
		{
			name: "head SHA comparison is case-insensitive",
			head: strings.ToUpper(headSHA),
			checks: []vcs.CheckRun{
				{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE", HeadSHA: headSHA},
			},
			wantState: ciFailed,
		},
		{
			name: "an unknown head keeps the legacy behaviour",
			head: "",
			checks: []vcs.CheckRun{
				{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE", HeadSHA: oldSHA},
			},
			wantState: ciFailed,
		},
		{
			name: "pending StatusContext on head uses its created time",
			head: headSHA,
			checks: []vcs.CheckRun{
				{Context: "ci/legacy", State: "PENDING", HeadSHA: headSHA, CreatedAt: evalNow.Add(-90 * time.Minute)},
			},
			wantState:    ciStuck,
			wantInFlight: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateCI(tt.head, tt.checks, evalNow)
			assert.Equal(t, tt.wantState, got.State, "state (reason: %s)", got.Reason)
			assert.Equal(t, tt.wantStale, got.StaleChecks, "stale check count")
			assert.Equal(t, tt.wantInFlight, got.InFlight, "in-flight check count")
			assert.NotEmpty(t, got.Reason, "every verdict must carry a reason for logging")
			// Only a genuine failure on the head may reach the quench path.
			assert.Equal(t, tt.wantState == ciFailed, !got.passing() && !got.inProgress(),
				"only ciFailed may be treated as an actionable failure")
		})
	}
}

// ciTestPR inserts an open PR seeded as CI-passing, so any CI failure the
// monitor detects shows up as a passing→failing transition (the branch that
// emits ci_failed and thus dispatches quench).
func ciTestPR(t *testing.T, db *state.DB, number int, beadID string) *state.PR {
	t.Helper()
	pr := &state.PR{
		Number:    number,
		Anvil:     "test-anvil",
		BeadID:    beadID,
		Branch:    "forge/" + beadID,
		Status:    state.PROpen,
		CIPassing: true,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	return pr
}

func ciTestMonitor(t *testing.T, db *state.DB, fake vcs.Provider, now time.Time) (*Monitor, *[]string) {
	t.Helper()
	var events []string
	m := New(db, func(_ string) vcs.Provider { return fake }, time.Minute,
		map[string]string{"test-anvil": "/fake"}, nil, nil, nil, nil)
	m.now = func() time.Time { return now }
	m.OnEvent(func(_ context.Context, e PREvent) {
		events = append(events, e.EventType)
	})
	return m, &events
}

// TestCheckPR_StaleFailureOnOldSHADoesNotDispatchQuench is the regression test
// for the 2026-08-06 outage: a FAILURE belonging to a superseded commit, with
// no runs on the head, must leave CI pending instead of spawning CI-fix workers.
func TestCheckPR_StaleFailureOnOldSHADoesNotDispatchQuench(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := ciTestPR(t, db, 779, "forge-stale-ci")
	now := time.Now()
	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State:   "OPEN",
		HeadSHA: headSHA,
		StatusCheckRollup: []vcs.CheckRun{
			{Name: "changelog-check", Status: "COMPLETED", Conclusion: "FAILURE", HeadSHA: oldSHA,
				StartedAt: now.Add(-3 * time.Hour), CompletedAt: now.Add(-3 * time.Hour)},
		},
	}}
	m, events := ciTestMonitor(t, db, fake, now)

	m.checkAll(context.Background())

	assert.NotContains(t, *events, EventCIFailed,
		"a failure on a superseded commit must not dispatch a CI-fix worker")
	updated, err := db.GetPRByID(pr.ID)
	require.NoError(t, err)
	assert.NotEqual(t, state.PRNeedsFix, updated.Status, "PR must not be parked in needs_fix on a stale failure")
	assert.False(t, updated.CIPassing, "an unverified head is not ready to merge")

	// A stale failure is not a stuck run — no operator note.
	r, err := db.GetRetry(pr.BeadID, pr.Anvil)
	require.NoError(t, err)
	assert.True(t, r == nil || !r.NeedsHuman, "pending CI must not raise needs-attention")
}

// TestCheckPR_FailureOnHeadSHADispatchesQuench pins the unchanged behaviour: a
// completed failure on the current head still fires ci_failed.
func TestCheckPR_FailureOnHeadSHADispatchesQuench(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := ciTestPR(t, db, 780, "forge-head-ci-fail")
	now := time.Now()
	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State:   "OPEN",
		HeadSHA: headSHA,
		StatusCheckRollup: []vcs.CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS", HeadSHA: headSHA},
			{Name: "changelog-check", Status: "COMPLETED", Conclusion: "FAILURE", HeadSHA: headSHA},
		},
	}}
	m, events := ciTestMonitor(t, db, fake, now)

	m.checkAll(context.Background())

	assert.Contains(t, *events, EventCIFailed, "a failure on the head must still dispatch a CI-fix worker")
	updated, err := db.GetPRByID(pr.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PRNeedsFix, updated.Status)
}

// TestCheckPR_StuckRunRaisesNeedsAttention verifies the outage path: a run
// queued past the threshold raises an informational needs-attention entry
// (once), dispatches nothing, and is retracted once CI settles.
func TestCheckPR_StuckRunRaisesNeedsAttention(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := ciTestPR(t, db, 781, "forge-stuck-ci")
	now := time.Now()
	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State:   "OPEN",
		HeadSHA: headSHA,
		StatusCheckRollup: []vcs.CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS", HeadSHA: headSHA},
			{Name: "changelog-check", Status: "QUEUED", HeadSHA: headSHA, StartedAt: now.Add(-2 * time.Hour)},
		},
	}}
	m, events := ciTestMonitor(t, db, fake, now)

	m.checkAll(context.Background())

	assert.NotContains(t, *events, EventCIFailed, "a stuck run must not dispatch a CI-fix worker")
	r, err := db.GetRetry(pr.BeadID, pr.Anvil)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.True(t, r.NeedsHuman, "a wedged run must surface in Needs Attention")
	assert.True(t, strings.HasPrefix(r.LastError, ciStuckAttentionPrefix), "note must carry the stuck-CI marker: %s", r.LastError)

	// Second poll on the same head: the note is not re-raised.
	before, err := db.RecentEvents(100)
	require.NoError(t, err)
	m.checkAll(context.Background())
	after, err := db.RecentEvents(100)
	require.NoError(t, err)
	assert.Equal(t, countEventsOfType(before, state.EventCIStuck), countEventsOfType(after, state.EventCIStuck),
		"the stuck note must fire once per head, not once per poll")

	// CI settles green on the same head: the note is retracted.
	fake.status = &vcs.PRStatus{
		State:   "OPEN",
		HeadSHA: headSHA,
		StatusCheckRollup: []vcs.CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS", HeadSHA: headSHA},
			{Name: "changelog-check", Status: "COMPLETED", Conclusion: "SUCCESS", HeadSHA: headSHA},
		},
	}
	m.checkAll(context.Background())

	r, err = db.GetRetry(pr.BeadID, pr.Anvil)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.False(t, r.NeedsHuman, "the stuck-CI note must clear once CI settles")
}

// TestCheckPR_StuckNoteDoesNotClobberOtherAttention verifies clearCIStuck only
// retracts its own note — an unrelated needs-attention reason must survive.
func TestCheckPR_StuckNoteDoesNotClobberOtherAttention(t *testing.T) {
	db, cleanup := openTempDB(t)
	defer cleanup()

	pr := ciTestPR(t, db, 782, "forge-other-attention")
	require.NoError(t, db.MarkNeedsHuman(pr.BeadID, pr.Anvil, "circuit breaker: 3 dispatch failures"))

	now := time.Now()
	fake := &fakeVCSProvider{status: &vcs.PRStatus{
		State:   "OPEN",
		HeadSHA: headSHA,
		StatusCheckRollup: []vcs.CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS", HeadSHA: headSHA},
		},
	}}
	m, _ := ciTestMonitor(t, db, fake, now)

	m.checkAll(context.Background())

	r, err := db.GetRetry(pr.BeadID, pr.Anvil)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.True(t, r.NeedsHuman, "an unrelated needs-attention flag must survive a settled CI poll")
}

func countEventsOfType(events []state.Event, t state.EventType) int {
	n := 0
	for _, e := range events {
		if e.Type == t {
			n++
		}
	}
	return n
}
