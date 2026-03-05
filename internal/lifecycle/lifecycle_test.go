package lifecycle

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func makeEvent(prNum int, evType string) bellows.PREvent {
	return bellows.PREvent{
		PRNumber:  prNum,
		BeadID:    "bead-1",
		Anvil:     "test-anvil",
		Branch:    "forge/bead-1",
		EventType: evType,
	}
}

func makeEventWithID(id, prNum int, evType string) bellows.PREvent {
	return bellows.PREvent{
		ID:        id,
		PRNumber:  prNum,
		BeadID:    "bead-1",
		Anvil:     "test-anvil",
		Branch:    "forge/bead-1",
		EventType: evType,
	}
}

// TestEventCIFailed_Dispatch verifies that a CI failure dispatches ActionFixCI.
func TestEventCIFailed_Dispatch(t *testing.T) {
	var got ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		got = req
	}

	m := New(nil, handler)
	m.HandleEvent(context.Background(), makeEventWithID(42, 42, bellows.EventCIFailed))

	if got.Action != ActionFixCI {
		t.Errorf("expected ActionFixCI, got %v", got.Action)
	}
	if got.PRNumber != 42 {
		t.Errorf("expected PR #42, got %d", got.PRNumber)
	}
	if got.BeadID != "bead-1" {
		t.Errorf("expected bead-1, got %q", got.BeadID)
	}
}

// TestEventCIFailed_MaxAttemptsExhausted verifies no dispatch after maxCI failures.
func TestEventCIFailed_MaxAttemptsExhausted(t *testing.T) {
	db := newTestDB(t)

	pr := state.PR{Number: 10, Anvil: "test-anvil", BeadID: "bead-1", Status: state.PROpen, CreatedAt: time.Now()}
	require.NoError(t, db.InsertPR(&pr))

	var dispatchCount int
	handler := func(_ context.Context, req ActionRequest) {
		dispatchCount++
	}

	m := New(db, handler)
	ctx := context.Background()
	ev := makeEventWithID(pr.ID, 10, bellows.EventCIFailed)

	// First two failures should dispatch (maxCI=2)
	m.HandleEvent(ctx, ev)
	m.HandleEvent(ctx, ev)
	// Third failure should NOT dispatch
	m.HandleEvent(ctx, ev)

	if dispatchCount != 2 {
		t.Errorf("expected 2 dispatches, got %d", dispatchCount)
	}

	st := m.GetState(pr.ID)
	if st == nil {
		t.Fatal("expected state to exist")
	}
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

	pr := state.PR{Number: 7, Anvil: "test-anvil", BeadID: "bead-1", Status: state.PROpen, CreatedAt: time.Now()}
	require.NoError(t, db.InsertPR(&pr))

	var got ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		got = req
	}

	m := New(db, handler)
	m.HandleEvent(context.Background(), makeEventWithID(pr.ID, 7, bellows.EventReviewChanges))

	if got.Action != ActionFixReview {
		t.Errorf("expected ActionFixReview, got %v", got.Action)
	}

	st := m.GetState(pr.ID)
	if st == nil {
		t.Fatal("expected state to exist")
	}
	if st.ReviewFixCount != 1 {
		t.Errorf("expected ReviewFixCount=1, got %d", st.ReviewFixCount)
	}
	if !st.NeedsFix {
		t.Error("expected NeedsFix=true")
	}
}

// TestEventReviewChanges_MaxAttemptsExhausted verifies no dispatch after maxRev failures.
func TestEventReviewChanges_MaxAttemptsExhausted(t *testing.T) {
	db := newTestDB(t)

	pr := state.PR{Number: 20, Anvil: "test-anvil", BeadID: "bead-1", Status: state.PROpen, CreatedAt: time.Now()}
	require.NoError(t, db.InsertPR(&pr))

	var dispatchCount int
	handler := func(_ context.Context, _ ActionRequest) {
		dispatchCount++
	}

	m := New(db, handler)
	ctx := context.Background()
	ev := makeEventWithID(pr.ID, 20, bellows.EventReviewChanges)

	m.HandleEvent(ctx, ev)
	m.HandleEvent(ctx, ev)
	m.HandleEvent(ctx, ev) // exhausted

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
	var got ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		got = req
	}

	m := New(nil, handler)
	m.HandleEvent(context.Background(), makeEventWithID(99, 99, bellows.EventPRMerged))

	if got.Action != ActionCloseBead {
		t.Errorf("expected ActionCloseBead, got %v", got.Action)
	}
	if got.PRNumber != 99 {
		t.Errorf("expected PR #99, got %d", got.PRNumber)
	}
}

// TestEventPRConflicting_Dispatch verifies that a conflict event dispatches ActionRebase.
func TestEventPRConflicting_Dispatch(t *testing.T) {
	db := newTestDB(t)

	pr := state.PR{Number: 55, Anvil: "test-anvil", BeadID: "bead-1", Status: state.PROpen, CreatedAt: time.Now()}
	require.NoError(t, db.InsertPR(&pr))

	var dispatched []ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		dispatched = append(dispatched, req)
	}

	m := New(db, handler)
	ev := makeEventWithID(pr.ID, 55, bellows.EventPRConflicting)

	m.HandleEvent(context.Background(), ev)

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
	m.HandleEvent(context.Background(), ev)
	if len(dispatched) != 2 {
		t.Errorf("expected 2 dispatches after second conflict, got %d", len(dispatched))
	}
}

// TestEventPRConflicting_ExhaustRebase verifies rebase attempts are capped.
func TestEventPRConflicting_ExhaustRebase(t *testing.T) {
	db := newTestDB(t)

	pr := state.PR{Number: 77, Anvil: "test-anvil", BeadID: "bead-1", Status: state.PROpen, CreatedAt: time.Now()}
	require.NoError(t, db.InsertPR(&pr))

	var dispatchCount int
	handler := func(_ context.Context, _ ActionRequest) {
		dispatchCount++
	}

	m := New(db, handler)
	ctx := context.Background()
	ev := makeEventWithID(pr.ID, 77, bellows.EventPRConflicting)
	for i := 0; i < 5; i++ {
		m.HandleEvent(ctx, ev)
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

	m := New(db, handler)
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
					ID:        pr + 1, // non-zero ID (pr is 0-4, so ID is 1-5)
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
	// Just verify no races occurred — states with ID=0 aren't stored but handler calls are safe
}

// TestNewPRState_CreatedOnFirstEvent verifies state is auto-created on first event.
func TestNewPRState_CreatedOnFirstEvent(t *testing.T) {
	m := New(nil, nil)
	m.HandleEvent(context.Background(), makeEventWithID(1, 1, bellows.EventCIPassed))

	st := m.GetState(1)
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
	var got ActionRequest
	handler := func(_ context.Context, req ActionRequest) {
		got = req
	}

	m := New(nil, handler)
	m.HandleEvent(context.Background(), makeEventWithID(33, 33, bellows.EventPRClosed))

	if got.Action != ActionCleanup {
		t.Errorf("expected ActionCleanup, got %v", got.Action)
	}
}

// TestActivePRs verifies ActivePRs count excludes merged/closed PRs.
func TestActivePRs(t *testing.T) {
	db := newTestDB(t)

	prs := []state.PR{
		{Number: 1, Anvil: "test-anvil", BeadID: "bead-1", Status: state.PROpen, CreatedAt: time.Now()},
		{Number: 2, Anvil: "test-anvil", BeadID: "bead-2", Status: state.PROpen, CreatedAt: time.Now()},
		{Number: 3, Anvil: "test-anvil", BeadID: "bead-3", Status: state.PROpen, CreatedAt: time.Now()},
		{Number: 4, Anvil: "test-anvil", BeadID: "bead-4", Status: state.PROpen, CreatedAt: time.Now()},
	}
	for i := range prs {
		require.NoError(t, db.InsertPR(&prs[i]))
	}

	m := New(db, nil)
	ctx := context.Background()

	m.HandleEvent(ctx, makeEventWithID(prs[0].ID, 1, bellows.EventCIPassed)) // active
	m.HandleEvent(ctx, makeEventWithID(prs[1].ID, 2, bellows.EventCIPassed)) // active
	m.HandleEvent(ctx, makeEventWithID(prs[2].ID, 3, bellows.EventPRMerged)) // merged
	m.HandleEvent(ctx, makeEventWithID(prs[3].ID, 4, bellows.EventPRClosed)) // closed

	if count := m.ActivePRs(); count != 2 {
		t.Errorf("expected 2 active PRs, got %d", count)
	}
}

// TestRemove verifies that Remove deletes PR state.
func TestRemove(t *testing.T) {
	db := newTestDB(t)

	pr := state.PR{Number: 5, Anvil: "test-anvil", BeadID: "bead-1", Status: state.PROpen, CreatedAt: time.Now()}
	require.NoError(t, db.InsertPR(&pr))

	m := New(db, nil)
	m.HandleEvent(context.Background(), makeEventWithID(pr.ID, 5, bellows.EventCIPassed))

	m.Remove(pr.ID)
	if st := m.GetState(pr.ID); st != nil {
		t.Error("expected state to be nil after Remove")
	}
}

func TestSeedFromDB(t *testing.T) {
	db, err := state.Open(":memory:")
	require.NoError(t, err)
	defer db.Close()

	// Insert some PRs
	now := time.Now()
	prs := []state.PR{
		{
			Number:         1,
			Anvil:          "anvil1",
			BeadID:         "bead1",
			Branch:         "branch1",
			Status:         state.PRNeedsFix,
			CreatedAt:      now,
			CIPassing:      false,
			CIFixCount:     1,
			ReviewFixCount: 0,
			IsConflicting:  true,
		},
		{
			Number:         2,
			Anvil:          "anvil1",
			BeadID:         "bead2",
			Branch:         "branch2",
			Status:         state.PRApproved,
			CreatedAt:      now,
			CIPassing:      true,
			CIFixCount:     0,
			ReviewFixCount: 0,
			IsConflicting:  false,
		},
	}

	for i := range prs {
		err := db.InsertPR(&prs[i])
		require.NoError(t, err)
		if prs[i].IsConflicting {
			err = db.UpdatePRConflicting(prs[i].ID, true)
			require.NoError(t, err)
		}
	}

	mgr := New(db, nil)
	err = mgr.SeedFromDB()
	require.NoError(t, err)

	st1 := mgr.GetState(prs[0].ID)
	require.NotNil(t, st1)
	assert.Equal(t, 1, st1.PRNumber)
	assert.Equal(t, "bead1", st1.BeadID)
	assert.False(t, st1.CIPassing)
	assert.True(t, st1.NeedsFix)
	assert.Equal(t, 1, st1.CIFixCount)
	assert.True(t, st1.IsConflicting)

	st2 := mgr.GetState(prs[1].ID)
	require.NotNil(t, st2)
	assert.True(t, st2.Approved)
}

func TestHandleEvent_Persistence(t *testing.T) {
	db, err := state.Open(":memory:")
	require.NoError(t, err)
	defer db.Close()

	// Insert PR to get an ID
	pr := state.PR{
		Number:    1,
		Anvil:     "anvil1",
		BeadID:    "bead1",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	err = db.InsertPR(&pr)
	require.NoError(t, err)

	mgr := New(db, nil)

	ctx := context.Background()
	event := bellows.PREvent{
		ID:        pr.ID,
		PRNumber:  1,
		BeadID:    "bead1",
		Anvil:     "anvil1",
		Branch:    "branch1",
		EventType: bellows.EventCIFailed,
	}

	mgr.HandleEvent(ctx, event)

	st := mgr.GetState(pr.ID)
	require.NotNil(t, st)
	assert.Equal(t, 1, st.CIFixCount)

	// Verify DB was updated
	openPRs, err := db.OpenPRs()
	require.NoError(t, err)

	found := false
	for _, p := range openPRs {
		if p.ID == pr.ID {
			assert.Equal(t, 1, p.CIFixCount)
			assert.False(t, p.CIPassing)
			found = true
		}
	}
	assert.True(t, found)
}
