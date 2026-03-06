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
	t.Cleanup(func() { db.Close() })
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

// TestEventCIFailed_Dispatch verifies that a CI failure dispatches ActionFixCI.
func TestEventCIFailed_Dispatch(t *testing.T) {
	db := newTestDB(t)
	var got ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		got = req
	}

	m := New(db, testLogger(), handler)
	m.HandleEvent(context.Background(), makeEvent(42, bellows.EventCIFailed))

	if got.Action != ActionFixCI {
		t.Errorf("expected ActionFixCI, got %v", got.Action)
	}
	if got.PRNumber != 42 {
		t.Errorf("expected PR #42, got %d", got.PRNumber)
	}
	if got.BeadID != "bead-1" {
		t.Errorf("expected bead-1, got %q", got.BeadID)
	}

	st := m.GetState("test-anvil", 42)
	if st.CIFixCount != 1 {
		t.Errorf("expected CIFixCount=1, got %d", st.CIFixCount)
	}
}

// TestEventCIFailed_MaxAttemptsExhausted verifies no dispatch after maxCI failures.
func TestEventCIFailed_MaxAttemptsExhausted(t *testing.T) {
	db := newTestDB(t)

	var dispatchCount int
	handler := func(_ context.Context, req ActionRequest) {
		dispatchCount++
	}

	m := New(db, testLogger(), handler)
	ctx := context.Background()
	evFail := makeEvent(10, bellows.EventCIFailed)
	evPass := makeEvent(10, bellows.EventCIPassed)

	// First failure should dispatch (maxCI=2)
	m.HandleEvent(ctx, evFail)
	m.HandleEvent(ctx, evPass)
	// Second failure should dispatch
	m.HandleEvent(ctx, evFail)
	m.HandleEvent(ctx, evPass)
	// Third failure should NOT dispatch, hits exhaustion log
	m.HandleEvent(ctx, evFail)

	if dispatchCount != 2 {
		t.Errorf("expected 2 dispatches, got %d", dispatchCount)
	}

	st := m.GetState("test-anvil", 10)
	if st.CIFixCount != 2 {
		t.Errorf("expected CIFixCount=2, got %d", st.CIFixCount)
	}
	if !st.NeedsFix {
		t.Error("expected NeedsFix=true after CI failure")
	}

	// Verify exhaustion was logged to DB
	events, err := db.RecentEvents(10)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if string(e.Type) == string(state.EventLifecycleExhausted) {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected EventLifecycleExhausted to be logged to DB")
	}
}

// TestEventReviewChanges_Dispatch verifies ActionFixReview is dispatched on review changes.
func TestEventReviewChanges_Dispatch(t *testing.T) {
	db := newTestDB(t)

	var got ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		got = req
	}

	m := New(db, testLogger(), handler)
	m.HandleEvent(context.Background(), makeEvent(7, bellows.EventReviewChanges))

	if got.Action != ActionFixReview {
		t.Errorf("expected ActionFixReview, got %v", got.Action)
	}

	st := m.GetState("test-anvil", 7)
	if st.ReviewFixCnt != 1 {
		t.Errorf("expected ReviewFixCnt=1, got %d", st.ReviewFixCnt)
	}
	if !st.NeedsFix {
		t.Error("expected NeedsFix=true")
	}
}

// TestEventReviewChanges_MaxAttemptsExhausted verifies no dispatch after maxRev failures.
func TestEventReviewChanges_MaxAttemptsExhausted(t *testing.T) {
	db := newTestDB(t)

	var dispatchCount int
	handler := func(_ context.Context, _ ActionRequest) {
		dispatchCount++
	}

	m := New(db, testLogger(), handler)
	ctx := context.Background()
	evChange := makeEvent(20, bellows.EventReviewChanges)
	evApprove := makeEvent(20, bellows.EventReviewApproved)

	m.HandleEvent(ctx, evChange)
	m.HandleEvent(ctx, evApprove)
	m.HandleEvent(ctx, evChange)
	m.HandleEvent(ctx, evApprove)
	m.HandleEvent(ctx, evChange) // exhausted

	if dispatchCount != 2 {
		t.Errorf("expected 2 dispatches, got %d", dispatchCount)
	}

	events, err := db.RecentEvents(10)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if string(e.Type) == string(state.EventLifecycleExhausted) {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected EventLifecycleExhausted to be logged")
	}
}

// TestEventPRMerged_ClosesBead verifies ActionCloseBead is dispatched on merge.
func TestEventPRMerged_ClosesBead(t *testing.T) {
	db := newTestDB(t)
	var got ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		got = req
	}

	m := New(db, testLogger(), handler)
	m.HandleEvent(context.Background(), makeEvent(99, bellows.EventPRMerged))

	if got.Action != ActionCloseBead {
		t.Errorf("expected ActionCloseBead, got %v", got.Action)
	}
	if got.PRNumber != 99 {
		t.Errorf("expected PR #99, got %d", got.PRNumber)
	}

	st := m.GetState("test-anvil", 99)
	if !st.Merged {
		t.Error("expected Merged=true")
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

// TestNewPRState_CreatedOnFirstEvent verifies state is auto-created on first event.
func TestNewPRState_CreatedOnFirstEvent(t *testing.T) {
	db := newTestDB(t)
	m := New(db, testLogger(), nil)
	m.HandleEvent(context.Background(), makeEvent(1, bellows.EventCIPassed))

	st := m.GetState("test-anvil", 1)
	if st == nil {
		t.Fatal("expected state to be created")
	}
	if st.PRNumber != 1 {
		t.Errorf("expected PRNumber=1, got %d", st.PRNumber)
	}
	if st.CIPassing != true {
		t.Error("expected CIPassing=true after EventCIPassed")
	}
}

// TestEventPRClosed_Cleanup verifies ActionCleanup is dispatched when PR is closed.
func TestEventPRClosed_Cleanup(t *testing.T) {
	db := newTestDB(t)
	var got ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		got = req
	}

	m := New(db, testLogger(), handler)
	m.HandleEvent(context.Background(), makeEvent(33, bellows.EventPRClosed))

	if got.Action != ActionCleanup {
		t.Errorf("expected ActionCleanup, got %v", got.Action)
	}

	st := m.GetState("test-anvil", 33)
	if !st.Closed {
		t.Error("expected Closed=true")
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

	// 3. Verify redundant events are ignored
	m2.HandleEvent(ctx, bellows.PREvent{
		PRNumber:  123,
		Anvil:     "test-anvil",
		EventType: bellows.EventCIFailed,
		BeadID:    "bd-1",
	})
	if st2.CIFixCount != 1 {
		t.Errorf("expected CIFixCount still 1 after redundant event, got %d", st2.CIFixCount)
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
