package lifecycle

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/state"
)

// newTestDB opens a temporary SQLite DB for testing and registers cleanup.
func newTestDB(t *testing.T) *state.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close test DB: %v", err)
		}
	})
	return db
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func makeEvent(prNum int, evType string) bellows.PREvent {
	return bellows.PREvent{
		PRNumber:  prNum,
		BeadID:    "bead-1",
		Anvil:     "test-anvil",
		Branch:    "forge/bead-1",
		EventType: evType,
	}
}

// TestHandleEvent_Transitions uses table-driven tests to verify all major event transitions.
func TestHandleEvent_Transitions(t *testing.T) {
	tests := []struct {
		name               string
		prNumber           int
		setupEvents        []bellows.PREvent
		event              bellows.PREvent
		wantAction         Action
		wantCINeedsFix     bool
		wantReviewNeedsFix bool
		wantCIPassing      bool
		wantApproved       bool
		wantMerged         bool
		wantClosed         bool
		wantConflicting    bool
		wantCIFixCount     int
		wantReviewFixCnt   int
		wantRebaseCount    int
		wantDispatches     int // expected dispatches from the final event only
		wantDBEventType    state.EventType
	}{
		{
			name:           "EventCIFailed dispatch",
			prNumber:       42,
			event:          makeEvent(42, bellows.EventCIFailed),
			wantAction:     ActionFixCI,
			wantCINeedsFix: true,
			wantCIFixCount: 1,
			wantDispatches: 1,
		},
		{
			name:     "max CI attempts exhaustion",
			prNumber: 10,
			// maxCI=5: we need 5 full [Failed,Passed] cycles to fill the counter,
			// then the 6th EventCIFailed triggers exhaustion.
			// EventCIPassed between failures clears CINeedsFix=false so the next
			// EventCIFailed passes the st.CINeedsFix guard and increments the counter.
			setupEvents: []bellows.PREvent{
				makeEvent(10, bellows.EventCIFailed),
				makeEvent(10, bellows.EventCIPassed),
				makeEvent(10, bellows.EventCIFailed),
				makeEvent(10, bellows.EventCIPassed),
				makeEvent(10, bellows.EventCIFailed),
				makeEvent(10, bellows.EventCIPassed),
				makeEvent(10, bellows.EventCIFailed),
				makeEvent(10, bellows.EventCIPassed),
				makeEvent(10, bellows.EventCIFailed),
				makeEvent(10, bellows.EventCIPassed),
			},
			event:           makeEvent(10, bellows.EventCIFailed),
			wantAction:      ActionNone,
			wantCINeedsFix:  true,
			wantCIFixCount:  5, // counter must be at max to confirm setup worked
			wantDispatches:  0, // exhaustion path must not dispatch
			wantDBEventType: state.EventLifecycleExhausted,
		},
		{
			name:             "EventReviewChanges dispatch",
			prNumber:         7,
			event:              makeEvent(7, bellows.EventReviewChanges),
			wantAction:         ActionFixReview,
			wantReviewNeedsFix: true,
			wantCIPassing:      true, // InsertPR omits ci_passing so DB default (true) applies; review event does not change CI state
			wantReviewFixCnt: 1,
			wantDispatches:   1,
		},
		{
			name:     "max review attempts exhaustion",
			prNumber: 20,
			// maxRev=5: 5 full [Changes,Approved] cycles fill the counter;
			// the 6th EventReviewChanges triggers exhaustion.
			// EventReviewApproved sets Approved=true, re-opening the fix cycle
			// so the next EventReviewChanges passes the !ReviewNeedsFix||Approved guard.
			setupEvents: []bellows.PREvent{
				makeEvent(20, bellows.EventReviewChanges),
				makeEvent(20, bellows.EventReviewApproved),
				makeEvent(20, bellows.EventReviewChanges),
				makeEvent(20, bellows.EventReviewApproved),
				makeEvent(20, bellows.EventReviewChanges),
				makeEvent(20, bellows.EventReviewApproved),
				makeEvent(20, bellows.EventReviewChanges),
				makeEvent(20, bellows.EventReviewApproved),
				makeEvent(20, bellows.EventReviewChanges),
				makeEvent(20, bellows.EventReviewApproved),
			},
			event:              makeEvent(20, bellows.EventReviewChanges),
			wantAction:         ActionNone,
			wantReviewNeedsFix: true,
			wantCIPassing:      true, // review events do not change CI state
			wantReviewFixCnt: 5,    // counter must be at max to confirm setup worked
			wantDispatches:   0,    // exhaustion path must not dispatch
			wantDBEventType:  state.EventLifecycleExhausted,
		},
		{
			name:           "EventPRMerged closes bead",
			prNumber:       99,
			event:          makeEvent(99, bellows.EventPRMerged),
			wantAction:     ActionCloseBead,
			wantMerged:     true,
			wantCIPassing:  true, // merge event does not change CI state
			wantDispatches: 1,
		},
		{
			name:            "EventPRConflicting dispatch",
			prNumber:        55,
			event:           makeEvent(55, bellows.EventPRConflicting),
			wantAction:      ActionRebase,
			wantCIPassing:   true, // conflict event does not change CI state
			wantConflicting: true,
			wantRebaseCount: 1,
			wantDispatches:  1,
		},
		{
			name:           "EventPRClosed cleanup",
			prNumber:       33,
			event:          makeEvent(33, bellows.EventPRClosed),
			wantAction:     ActionCleanup,
			wantClosed:     true,
			wantCIPassing:  true, // close event does not change CI state
			wantDispatches: 1,
		},
		{
			name:           "EventCIPassed",
			prNumber:       1,
			event:          makeEvent(1, bellows.EventCIPassed),
			wantAction:     ActionNone,
			wantCIPassing:  true,
			wantDispatches: 0,
		},
		{
			name:           "EventReviewApproved",
			prNumber:       2,
			event:          makeEvent(2, bellows.EventReviewApproved),
			wantAction:     ActionNone,
			wantApproved:   true,
			wantCIPassing:  true, // approve event does not change CI state
			wantDispatches: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			var dispatches atomic.Int64
			var got ActionRequest
			handler := func(_ context.Context, req ActionRequest) {
				dispatches.Add(1)
				got = req
			}
			m := New(db, testLogger(), handler)
			ctx := context.Background()

			for _, ev := range tc.setupEvents {
				m.HandleEvent(ctx, ev)
			}

			// Reset counters before the final event so dispatch count is for the
			// final event only, not the setup events.
			dispatches.Store(0)
			got = ActionRequest{}
			m.HandleEvent(ctx, tc.event)

			if got.Action != tc.wantAction {
				t.Errorf("expected Action %v, got %v", tc.wantAction, got.Action)
			}
			if int(dispatches.Load()) != tc.wantDispatches {
				t.Errorf("expected %d dispatch(es) for final event, got %d", tc.wantDispatches, dispatches.Load())
			}
			if tc.wantAction != ActionNone {
				if got.PRNumber != tc.prNumber {
					t.Errorf("expected PR #%d, got %d", tc.prNumber, got.PRNumber)
				}
				if got.BeadID != tc.event.BeadID {
					t.Errorf("expected bead %q, got %q", tc.event.BeadID, got.BeadID)
				}
			}

			st := m.GetState("test-anvil", tc.prNumber)
			if st == nil {
				t.Fatalf("expected state for PR #%d", tc.prNumber)
			}
			if st.CINeedsFix != tc.wantCINeedsFix {
				t.Errorf("expected CINeedsFix=%v, got %v", tc.wantCINeedsFix, st.CINeedsFix)
			}
			if st.ReviewNeedsFix != tc.wantReviewNeedsFix {
				t.Errorf("expected ReviewNeedsFix=%v, got %v", tc.wantReviewNeedsFix, st.ReviewNeedsFix)
			}
			if st.Merged != tc.wantMerged {
				t.Errorf("expected Merged=%v, got %v", tc.wantMerged, st.Merged)
			}
			if st.Closed != tc.wantClosed {
				t.Errorf("expected Closed=%v, got %v", tc.wantClosed, st.Closed)
			}
			if st.CIPassing != tc.wantCIPassing {
				t.Errorf("expected CIPassing=%v, got %v", tc.wantCIPassing, st.CIPassing)
			}
			if st.Approved != tc.wantApproved {
				t.Errorf("expected Approved=%v, got %v", tc.wantApproved, st.Approved)
			}
			if st.Conflicting != tc.wantConflicting {
				t.Errorf("expected Conflicting=%v, got %v", tc.wantConflicting, st.Conflicting)
			}
			if tc.wantCIFixCount != 0 && st.CIFixCount != tc.wantCIFixCount {
				t.Errorf("expected CIFixCount=%d, got %d", tc.wantCIFixCount, st.CIFixCount)
			}
			if tc.wantReviewFixCnt != 0 && st.ReviewFixCnt != tc.wantReviewFixCnt {
				t.Errorf("expected ReviewFixCnt=%d, got %d", tc.wantReviewFixCnt, st.ReviewFixCnt)
			}
			if tc.wantRebaseCount != 0 && st.RebaseCount != tc.wantRebaseCount {
				t.Errorf("expected RebaseCount=%d, got %d", tc.wantRebaseCount, st.RebaseCount)
			}

			if tc.wantDBEventType != "" {
				events, err := db.RecentEvents(50)
				if err != nil {
					t.Fatalf("RecentEvents: %v", err)
				}
				// Each subtest uses a fresh temp DB so asserting on event type alone
				// is sufficient — there is no cross-contamination between subtests.
				found := false
				for _, e := range events {
					if string(e.Type) == string(tc.wantDBEventType) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected DB event %q, found none", tc.wantDBEventType)
				}
			}
		})
	}
}

// TestEventPRConflicting_Dispatch verifies that a conflict event dispatches ActionRebase.
func TestEventPRConflicting_Dispatch(t *testing.T) {
	db := newTestDB(t)

	var dispatched []ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		dispatched = append(dispatched, req)
	}

	m := New(db, testLogger(), handler)
	m.HandleEvent(context.Background(), makeEvent(55, bellows.EventPRConflicting))

	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(dispatched))
	}
	if dispatched[0].Action != ActionRebase {
		t.Errorf("expected ActionRebase, got %v", dispatched[0].Action)
	}
	if dispatched[0].PRNumber != 55 {
		t.Errorf("expected PR #55, got #%d", dispatched[0].PRNumber)
	}

	// Second conflicting event: still within maxRebase (3), should dispatch again.
	m.HandleEvent(context.Background(), makeEvent(55, bellows.EventPRConflicting))
	if len(dispatched) != 2 {
		t.Errorf("expected 2 dispatches after second conflict, got %d", len(dispatched))
	}
}

// TestEventPRConflicting_ExhaustRebase verifies rebase attempts are capped.
func TestEventPRConflicting_ExhaustRebase(t *testing.T) {
	db := newTestDB(t)

	var dispatchCount int
	handler := func(_ context.Context, _ ActionRequest) {
		dispatchCount++
	}

	m := New(db, testLogger(), handler)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		m.HandleEvent(ctx, makeEvent(77, bellows.EventPRConflicting))
	}

	// maxRebase is 3, so only 3 dispatches regardless of how many events fire.
	if dispatchCount != 3 {
		t.Errorf("expected 3 rebase dispatches (maxRebase), got %d", dispatchCount)
	}
}

// TestConcurrentHandleEvent exercises HandleEvent under the race detector.
func TestConcurrentHandleEvent(t *testing.T) {
	db := newTestDB(t)

	var callCount atomic.Int64
	handler := func(_ context.Context, _ ActionRequest) {
		callCount.Add(1)
	}

	m := New(db, testLogger(), handler)
	ctx := context.Background()

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		prNum := i % 5 // share 5 PR numbers across goroutines
		go func(pr int) {
			defer wg.Done()
			evTypes := []string{
				bellows.EventCIFailed,
				bellows.EventCIPassed,
				bellows.EventReviewChanges,
				bellows.EventReviewApproved,
				bellows.EventPRConflicting,
			}
			for _, evType := range evTypes {
				ev := bellows.PREvent{
					PRNumber:  pr,
					BeadID:    "bead-concurrent",
					Anvil:     "test-anvil",
					Branch:    "forge/bead-concurrent",
					EventType: evType,
				}
				m.HandleEvent(ctx, ev)
			}
		}(prNum)
	}

	wg.Wait()

	// Verify manager state is internally consistent
	for i := 0; i < 5; i++ {
		st := m.GetState("test-anvil", i)
		if st == nil {
			t.Errorf("expected state for PR #%d", i)
		}
	}
}

// TestActivePRs verifies ActivePRs count excludes merged/closed PRs.
func TestActivePRs(t *testing.T) {
	db := newTestDB(t)
	m := New(db, testLogger(), nil)
	ctx := context.Background()

	m.HandleEvent(ctx, makeEvent(1, bellows.EventCIPassed)) // active
	m.HandleEvent(ctx, makeEvent(2, bellows.EventCIPassed)) // active
	m.HandleEvent(ctx, makeEvent(3, bellows.EventPRMerged)) // merged
	m.HandleEvent(ctx, makeEvent(4, bellows.EventPRClosed)) // closed

	if count := m.ActivePRs(); count != 2 {
		t.Errorf("expected 2 active PRs, got %d", count)
	}
}

// TestRemove verifies that Remove deletes PR state.
func TestRemove(t *testing.T) {
	db := newTestDB(t)
	m := New(db, testLogger(), nil)
	m.HandleEvent(context.Background(), makeEvent(5, bellows.EventCIPassed))

	m.Remove("test-anvil", 5)
	if st := m.GetState("test-anvil", 5); st != nil {
		t.Error("expected state to be nil after Remove")
	}
}

func TestManager_Persistence(t *testing.T) {
	// Setup DB
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// db lifecycle managed manually below to simulate a daemon restart.

	// 1. Create a PR and some state
	ctx := context.Background()
	pr := &state.PR{
		Number:    123,
		Anvil:     "test-anvil",
		BeadID:    "bd-1",
		Branch:    "fix-1",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
		CIPassing: true,
	}
	if err := db.InsertPR(pr); err != nil {
		t.Fatal(err)
	}

	m := New(db, testLogger(), func(ctx context.Context, req ActionRequest) {})

	// Simulate event to change state
	m.HandleEvent(ctx, bellows.PREvent{
		PRNumber:  123,
		Anvil:     "test-anvil",
		EventType: bellows.EventCIFailed,
		BeadID:    "bd-1",
	})

	st := m.GetState("test-anvil", 123)
	if st == nil {
		t.Fatal("state not found")
	}
	if st.CIFixCount != 1 {
		t.Errorf("expected CIFixCount 1, got %d", st.CIFixCount)
	}
	if st.CIPassing {
		t.Error("expected CIPassing false")
	}

	// 2. Simulate daemon restart: close the current DB handle and reopen it to
	// exercise SQLite close/reopen behaviour rather than reusing the same connection.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	m2 := New(db, testLogger(), func(ctx context.Context, req ActionRequest) {})
	if err := m2.Load(ctx); err != nil {
		t.Fatal(err)
	}

	st2 := m2.GetState("test-anvil", 123)
	if st2 == nil {
		t.Fatal("state not found after load")
	}
	if st2.CIFixCount != 1 {
		t.Errorf("expected CIFixCount 1 after load, got %d", st2.CIFixCount)
	}
	if st2.CIPassing {
		t.Error("expected CIPassing false after load")
	}

	// 3. Verify that after a daemon restart (CINeedsFix=false, CIPassing=false),
	// a new EventCIFailed dispatches a fresh fix cycle rather than being
	// suppressed. The guard now keys off CINeedsFix (fix-cycle-in-progress),
	// not CIPassing (CI result), so a restart correctly re-arms dispatch.
	var restarted int
	restartHandler := func(_ context.Context, _ ActionRequest) { restarted++ }
	m3 := New(db, testLogger(), restartHandler)
	if err := m3.Load(ctx); err != nil {
		t.Fatal(err)
	}
	m3.HandleEvent(ctx, bellows.PREvent{
		PRNumber:  123,
		Anvil:     "test-anvil",
		EventType: bellows.EventCIFailed,
		BeadID:    "bd-1",
	})
	if restarted != 1 {
		t.Errorf("expected 1 dispatch after restart (CINeedsFix=false), got %d", restarted)
	}
	st3 := m3.GetState("test-anvil", 123)
	if st3 == nil {
		t.Fatal("state not found after second load")
	}
	if st3.CIFixCount != 2 {
		t.Errorf("expected CIFixCount=2 after re-dispatch, got %d", st3.CIFixCount)
	}
}

func TestManager_AnvilCollision(t *testing.T) {
	// Setup DB
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	m := New(db, testLogger(), func(ctx context.Context, req ActionRequest) {})

	// PR #1 in Anvil A
	m.HandleEvent(ctx, bellows.PREvent{
		PRNumber:  1,
		Anvil:     "anvil-a",
		EventType: bellows.EventCIFailed,
		BeadID:    "bead-a",
	})

	// PR #1 in Anvil B
	m.HandleEvent(ctx, bellows.PREvent{
		PRNumber:  1,
		Anvil:     "anvil-b",
		EventType: bellows.EventCIPassed,
		BeadID:    "bead-b",
	})

	stA := m.GetState("anvil-a", 1)
	stB := m.GetState("anvil-b", 1)

	if stA == nil || stB == nil {
		t.Fatal("states not found")
	}

	if stA.Anvil != "anvil-a" {
		t.Errorf("expected anvil-a, got %s", stA.Anvil)
	}
	if stB.Anvil != "anvil-b" {
		t.Errorf("expected anvil-b, got %s", stB.Anvil)
	}

	if stA.CIPassing {
		t.Error("expected anvil-a to be failing")
	}
	if !stB.CIPassing {
		t.Error("expected anvil-b to be passing")
	}

	// Verify both were inserted into DB with different IDs
	if stA.ID == stB.ID {
		t.Errorf("expected different DB IDs, both got %d", stA.ID)
	}
}

// TestNotifyReviewFixCompleted_AllowsNextCycle verifies that after a review fix
// worker completes (NotifyReviewFixCompleted is called), a subsequent
// EventReviewChanges triggered by the reviewer re-examining the updated push
// dispatches a new fix cycle instead of being suppressed by the "already in
// fix cycle" guard.
//
// This covers the real-world scenario where:
//  1. First wave: reviewer requests changes → reviewfix dispatched (ReviewNeedsFix=true)
//  2. Second wave arrives WHILE fix is running → "already in fix cycle" (expected)
//  3. Fix worker pushes changes → NotifyReviewFixCompleted clears ReviewNeedsFix
//  4. Reviewer re-examines the push, still requests changes → new EventReviewChanges
//     must be dispatched (was previously dropped because ReviewNeedsFix stayed true)
func TestNotifyReviewFixCompleted_AllowsNextCycle(t *testing.T) {
	db := newTestDB(t)
	var dispatched []ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		dispatched = append(dispatched, req)
	}
	m := New(db, testLogger(), handler)
	ctx := context.Background()

	// Step 1: first wave — EventReviewChanges dispatches fix cycle 1.
	m.HandleEvent(ctx, makeEvent(101, bellows.EventReviewChanges))
	if len(dispatched) != 1 || dispatched[0].Action != ActionFixReview {
		t.Fatalf("step 1: expected 1 ActionFixReview dispatch, got %v", dispatched)
	}

	// Step 2: second wave arrives while fix is running — must be suppressed.
	dispatched = dispatched[:0]
	m.HandleEvent(ctx, makeEvent(101, bellows.EventReviewChanges))
	if len(dispatched) != 0 {
		t.Fatalf("step 2: expected 0 dispatches (already in fix cycle), got %d", len(dispatched))
	}

	// Verify ReviewNeedsFix is still set and ReviewFixCnt is 1 after the suppressed event.
	st := m.GetState("test-anvil", 101)
	if st == nil {
		t.Fatal("no state for PR 101")
	}
	if !st.ReviewNeedsFix {
		t.Error("step 2: expected ReviewNeedsFix=true")
	}
	if st.ReviewFixCnt != 1 {
		t.Errorf("step 2: expected ReviewFixCnt=1, got %d", st.ReviewFixCnt)
	}

	// Step 3: review fix worker finishes — notify lifecycle.
	m.NotifyReviewFixCompleted("test-anvil", 101)

	st = m.GetState("test-anvil", 101)
	if st.ReviewNeedsFix {
		t.Error("step 3: expected ReviewNeedsFix=false after NotifyReviewFixCompleted")
	}

	// Step 4: re-review after pushed fixes — EventReviewChanges must be dispatched.
	m.HandleEvent(ctx, makeEvent(101, bellows.EventReviewChanges))
	if len(dispatched) != 1 || dispatched[0].Action != ActionFixReview {
		t.Fatalf("step 4: expected 1 ActionFixReview dispatch after fix completed, got %v", dispatched)
	}
	if dispatched[0].PRNumber != 101 {
		t.Errorf("step 4: expected PR #101, got #%d", dispatched[0].PRNumber)
	}

	st = m.GetState("test-anvil", 101)
	if st.ReviewFixCnt != 2 {
		t.Errorf("step 4: expected ReviewFixCnt=2, got %d", st.ReviewFixCnt)
	}
}

// TestCIAndReviewFixesAreIndependent verifies that CI failures and review
// changes dispatch independent fix cycles. This was the root cause of
// Forge-uxg: a single NeedsFix flag caused review fixes to be skipped when
// a CI fix cycle was already in progress.
func TestCIAndReviewFixesAreIndependent(t *testing.T) {
	db := newTestDB(t)
	var dispatched []ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		dispatched = append(dispatched, req)
	}
	m := New(db, testLogger(), handler)
	ctx := context.Background()

	// Step 1: CI fails → dispatches ActionFixCI.
	m.HandleEvent(ctx, makeEvent(200, bellows.EventCIFailed))
	if len(dispatched) != 1 || dispatched[0].Action != ActionFixCI {
		t.Fatalf("step 1: expected ActionFixCI, got %v", dispatched)
	}

	st := m.GetState("test-anvil", 200)
	if !st.CINeedsFix {
		t.Error("step 1: expected CINeedsFix=true")
	}

	// Step 2: Review changes arrive while CI fix is running.
	// With the split flags, this must NOT be blocked.
	dispatched = dispatched[:0]
	m.HandleEvent(ctx, makeEvent(200, bellows.EventReviewChanges))
	if len(dispatched) != 1 || dispatched[0].Action != ActionFixReview {
		t.Fatalf("step 2: expected ActionFixReview (independent of CI), got %v", dispatched)
	}

	st = m.GetState("test-anvil", 200)
	if !st.CINeedsFix {
		t.Error("step 2: CINeedsFix should still be true")
	}
	if !st.ReviewNeedsFix {
		t.Error("step 2: ReviewNeedsFix should be true")
	}

	// Step 3: CI passes → clears CINeedsFix but NOT ReviewNeedsFix.
	dispatched = dispatched[:0]
	m.HandleEvent(ctx, makeEvent(200, bellows.EventCIPassed))

	st = m.GetState("test-anvil", 200)
	if st.CINeedsFix {
		t.Error("step 3: CINeedsFix should be false after CI passed")
	}
	if !st.ReviewNeedsFix {
		t.Error("step 3: ReviewNeedsFix must still be true — CI passing should not clear it")
	}

	// Step 4: Review fix completes → clears ReviewNeedsFix.
	m.NotifyReviewFixCompleted("test-anvil", 200)
	st = m.GetState("test-anvil", 200)
	if st.ReviewNeedsFix {
		t.Error("step 4: ReviewNeedsFix should be false after NotifyReviewFixCompleted")
	}
	if st.CINeedsFix {
		t.Error("step 4: CINeedsFix should still be false")
	}
}

// TestResetPRState_AllowsFreshDispatchAfterExhaustion verifies that calling
// ResetPRState clears the in-memory lifecycle state so that new Bellows events
// dispatch fresh fix workers. Without this reset, the lifecycle manager still
// thinks the PR is exhausted and silently drops events after a DB-only retry.
func TestResetPRState_AllowsFreshDispatchAfterExhaustion(t *testing.T) {
	db := newTestDB(t)
	var dispatched []ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		dispatched = append(dispatched, req)
	}
	m := New(db, testLogger(), handler)
	ctx := context.Background()

	// Exhaust CI fix attempts: fail/pass cycles fill the counter up to the cap.
	for i := 0; i < state.DefaultMaxCIFixAttempts; i++ {
		m.HandleEvent(ctx, makeEvent(300, bellows.EventCIFailed))
		m.HandleEvent(ctx, makeEvent(300, bellows.EventCIPassed))
	}

	// Next CI failure should be exhausted (no dispatch).
	dispatched = dispatched[:0]
	m.HandleEvent(ctx, makeEvent(300, bellows.EventCIFailed))
	if len(dispatched) != 0 {
		t.Fatalf("expected 0 dispatches after exhaustion, got %d", len(dispatched))
	}

	st := m.GetState("test-anvil", 300)
	if st.CIFixCount != state.DefaultMaxCIFixAttempts {
		t.Fatalf("expected CIFixCount=%d, got %d", state.DefaultMaxCIFixAttempts, st.CIFixCount)
	}

	// Simulate retry: reset in-memory state.
	m.ResetPRState("test-anvil", 300)

	st = m.GetState("test-anvil", 300)
	if st.CIFixCount != 0 {
		t.Errorf("expected CIFixCount=0 after reset, got %d", st.CIFixCount)
	}
	if !st.CIPassing {
		t.Error("expected CIPassing=true after reset")
	}
	if st.CINeedsFix {
		t.Error("expected CINeedsFix=false after reset")
	}

	// New CI failure should now dispatch a fix worker.
	m.HandleEvent(ctx, makeEvent(300, bellows.EventCIFailed))
	if len(dispatched) != 1 || dispatched[0].Action != ActionFixCI {
		t.Fatalf("expected 1 ActionFixCI dispatch after reset, got %v", dispatched)
	}
	st = m.GetState("test-anvil", 300)
	if st.CIFixCount != 1 {
		t.Errorf("expected CIFixCount=1 after fresh dispatch, got %d", st.CIFixCount)
	}

	// Test rebase exhaustion reset
	for i := 0; i < state.DefaultMaxRebaseAttempts; i++ {
		m.HandleEvent(ctx, makeEvent(400, bellows.EventPRConflicting))
	}
	dispatched = dispatched[:0]
	m.HandleEvent(ctx, makeEvent(400, bellows.EventPRConflicting))
	if len(dispatched) != 0 {
		t.Errorf("expected 0 rebase dispatches after exhaustion, got %d", len(dispatched))
	}

	m.ResetPRState("test-anvil", 400)
	m.HandleEvent(ctx, makeEvent(400, bellows.EventPRConflicting))
	if len(dispatched) != 1 || dispatched[0].Action != ActionRebase {
		t.Errorf("expected 1 ActionRebase dispatch after reset, got %v", dispatched)
	}
}

// TestResetPRState_Noop verifies ResetPRState is safe to call for unknown PRs.
func TestResetPRState_Noop(t *testing.T) {
	db := newTestDB(t)
	m := New(db, testLogger(), nil)
	// Should not panic.
	m.ResetPRState("nonexistent-anvil", 999)
}

// TestNotifyReviewFixCompleted_DoesNotClearCINeedsFixWhileFailing verifies that
// completing a review fix does not clear a CI-related needs-fix state when CI
// is still failing. This guards against regressions where NotifyReviewFixCompleted
// would incorrectly clear CI-related flags or status.
func TestNotifyReviewFixCompleted_DoesNotClearCINeedsFixWhileFailing(t *testing.T) {
	db := newTestDB(t)
	var dispatched []ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		dispatched = append(dispatched, req)
	}
	m := New(db, testLogger(), handler)
	ctx := context.Background()

	// Step 1: CI fails → sets CINeedsFix.
	m.HandleEvent(ctx, makeEvent(200, bellows.EventCIFailed))
	st := m.GetState("test-anvil", 200)
	if !st.CINeedsFix {
		t.Fatal("step 1: expected CINeedsFix=true after CI failure")
	}

	// Step 2: Review changes arrive while CI is still failing → sets ReviewNeedsFix.
	dispatched = dispatched[:0]
	m.HandleEvent(ctx, makeEvent(200, bellows.EventReviewChanges))
	st = m.GetState("test-anvil", 200)
	if !st.CINeedsFix {
		t.Fatal("step 2: expected CINeedsFix to remain true while CI is still failing")
	}
	if !st.ReviewNeedsFix {
		t.Fatal("step 2: expected ReviewNeedsFix=true after review changes")
	}

	// Step 3: Review fix completes while CI is still failing.
	m.NotifyReviewFixCompleted("test-anvil", 200)
	st = m.GetState("test-anvil", 200)

	// ReviewNeedsFix should be cleared, but CINeedsFix must remain true.
	if st.ReviewNeedsFix {
		t.Error("step 3: ReviewNeedsFix should be false after NotifyReviewFixCompleted")
	}
	if !st.CINeedsFix {
		t.Error("step 3: CINeedsFix must remain true while CI is still failing")
	}
}

// TestNotifyCIFixCompleted_AllowsNextCycle verifies that after a CI fix worker
// finishes, NotifyCIFixCompleted clears CINeedsFix so the next CI failure event
// from bellows can dispatch a new fix cycle. The guard in HandleEvent for
// EventCIFailed now keys off CINeedsFix (fix-cycle-in-progress) rather than
// CIPassing (CI result), so CIPassing intentionally remains false until Bellows
// reports a real CI pass — it must not be faked here.
func TestNotifyCIFixCompleted_AllowsNextCycle(t *testing.T) {
	db := newTestDB(t)
	var dispatched []ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		dispatched = append(dispatched, req)
	}
	m := New(db, testLogger(), handler)
	ctx := context.Background()

	// Step 1: CI fails → dispatches ActionFixCI.
	m.HandleEvent(ctx, makeEvent(500, bellows.EventCIFailed))
	if len(dispatched) != 1 || dispatched[0].Action != ActionFixCI {
		t.Fatalf("step 1: expected ActionFixCI, got %v", dispatched)
	}
	st := m.GetState("test-anvil", 500)
	if !st.CINeedsFix {
		t.Fatal("step 1: expected CINeedsFix=true")
	}
	if st.CIPassing {
		t.Fatal("step 1: expected CIPassing=false")
	}

	// Step 2: CI fix worker completes → notify lifecycle.
	// CIPassing must remain false (CI hasn't actually passed yet);
	// only CINeedsFix is cleared to re-arm the dispatch guard.
	m.NotifyCIFixCompleted("test-anvil", 500)
	st = m.GetState("test-anvil", 500)
	if st.CINeedsFix {
		t.Error("step 2: expected CINeedsFix=false after NotifyCIFixCompleted")
	}
	if st.CIPassing {
		t.Error("step 2: CIPassing must remain false — CI has not actually passed")
	}

	// Step 3: CI fails again → should dispatch a new fix (not be suppressed by
	// the CINeedsFix guard, which is now false).
	dispatched = dispatched[:0]
	m.HandleEvent(ctx, makeEvent(500, bellows.EventCIFailed))
	if len(dispatched) != 1 || dispatched[0].Action != ActionFixCI {
		t.Fatalf("step 3: expected ActionFixCI after reset, got %v", dispatched)
	}
	st = m.GetState("test-anvil", 500)
	if st.CIFixCount != 2 {
		t.Errorf("step 3: expected CIFixCount=2, got %d", st.CIFixCount)
	}
}

// TestNotifyCIFixCompleted_DoesNotClearReviewNeedsFix verifies that completing
// a CI fix does not clear a review-related needs-fix state when a review fix
// cycle is also active.
func TestNotifyCIFixCompleted_DoesNotClearReviewNeedsFix(t *testing.T) {
	db := newTestDB(t)
	var dispatched []ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		dispatched = append(dispatched, req)
	}
	m := New(db, testLogger(), handler)
	ctx := context.Background()

	// Step 1: CI fails and review changes both arrive.
	m.HandleEvent(ctx, makeEvent(600, bellows.EventCIFailed))
	m.HandleEvent(ctx, makeEvent(600, bellows.EventReviewChanges))
	st := m.GetState("test-anvil", 600)
	if !st.CINeedsFix {
		t.Fatal("step 1: expected CINeedsFix=true")
	}
	if !st.ReviewNeedsFix {
		t.Fatal("step 1: expected ReviewNeedsFix=true")
	}

	// Step 2: CI fix completes while review fix is still running.
	m.NotifyCIFixCompleted("test-anvil", 600)
	st = m.GetState("test-anvil", 600)
	if st.CINeedsFix {
		t.Error("step 2: CINeedsFix should be false after CI fix completed")
	}
	if !st.ReviewNeedsFix {
		t.Error("step 2: ReviewNeedsFix must remain true — CI fix should not clear it")
	}
}

// TestNotifyCIFixCompleted_Noop verifies NotifyCIFixCompleted is safe to call
// for unknown PRs.
func TestNotifyCIFixCompleted_Noop(t *testing.T) {
	db := newTestDB(t)
	m := New(db, testLogger(), nil)
	// Should not panic.
	m.NotifyCIFixCompleted("nonexistent-anvil", 999)
}

// TestCIFixRetryFlowToExhaustion exercises the lifecycle CI fix retry loop:
// repeated EventCIFailed → dispatch → NotifyCIFixCompleted → next EventCIFailed,
// verifying that each cycle dispatches until maxCI is reached and then stops.
// This simulates the sequence of events bellows would produce after each cache
// reset (re-detecting CI failure and emitting EventCIFailed), but only tests
// lifecycle's handling of those events, not bellows' snapshot behavior itself.
func TestCIFixRetryFlowToExhaustion(t *testing.T) {
	db := newTestDB(t)
	var dispatched []ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		dispatched = append(dispatched, req)
	}
	m := New(db, testLogger(), handler)
	m.SetThresholds(5, 0, 0) // maxCI = 5
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		dispatched = dispatched[:0]
		m.HandleEvent(ctx, makeEvent(700, bellows.EventCIFailed))
		if len(dispatched) != 1 || dispatched[0].Action != ActionFixCI {
			t.Fatalf("cycle %d: expected ActionFixCI dispatch, got %v", i, dispatched)
		}
		st := m.GetState("test-anvil", 700)
		if st.CIFixCount != i {
			t.Fatalf("cycle %d: expected CIFixCount=%d, got %d", i, i, st.CIFixCount)
		}
		// Simulate cifix worker completing (CI still broken).
		m.NotifyCIFixCompleted("test-anvil", 700)
	}

	// Cycle 6: maxCI exhausted — should NOT dispatch.
	dispatched = dispatched[:0]
	m.HandleEvent(ctx, makeEvent(700, bellows.EventCIFailed))
	for _, d := range dispatched {
		if d.Action == ActionFixCI {
			t.Fatal("cycle 6: expected no ActionFixCI after exhaustion, but got one")
		}
	}
	st := m.GetState("test-anvil", 700)
	if st.CIFixCount != 5 {
		t.Errorf("after exhaustion: expected CIFixCount=5, got %d", st.CIFixCount)
	}
}
