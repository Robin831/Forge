package lifecycle

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
		name            string
		prNumber        int
		setupEvents     []bellows.PREvent
		event           bellows.PREvent
		wantAction      Action
		wantNeedsFix    bool
		wantCIPassing   bool
		wantApproved    bool
		wantMerged      bool
		wantClosed      bool
		wantDBEventType state.EventType
	}{
		{
			name:         "EventCIFailed dispatch",
			prNumber:     42,
			event:        makeEvent(42, bellows.EventCIFailed),
			wantAction:   ActionFixCI,
			wantNeedsFix: true,
		},
		{
			name:     "max CI attempts exhaustion",
			prNumber: 10,
			// maxCI=2: we need 2 full [Failed,Passed] cycles to fill the counter,
			// then the 3rd EventCIFailed triggers exhaustion.
			// EventCIPassed between failures resets CIPassing=true so the next
			// EventCIFailed can pass the !st.CIPassing guard and increment the counter.
			setupEvents: []bellows.PREvent{
				makeEvent(10, bellows.EventCIFailed),
				makeEvent(10, bellows.EventCIPassed),
				makeEvent(10, bellows.EventCIFailed),
				makeEvent(10, bellows.EventCIPassed),
			},
			event:           makeEvent(10, bellows.EventCIFailed),
			wantAction:      ActionNone,
			wantNeedsFix:    true,
			wantDBEventType: state.EventLifecycleExhausted,
		},
		{
			name:          "EventReviewChanges dispatch",
			prNumber:      7,
			event:         makeEvent(7, bellows.EventReviewChanges),
			wantAction:    ActionFixReview,
			wantNeedsFix:  true,
			wantCIPassing: true, // InsertPR omits ci_passing so DB default (true) applies; review event does not change CI state
		},
		{
			name:     "max review attempts exhaustion",
			prNumber: 20,
			// maxRev=2: 2 full [Changes,Approved] cycles fill the counter;
			// the 3rd EventReviewChanges triggers exhaustion.
			// EventReviewApproved sets Approved=true, re-opening the fix cycle
			// so the next EventReviewChanges passes the !NeedsFix||Approved guard.
			setupEvents: []bellows.PREvent{
				makeEvent(20, bellows.EventReviewChanges),
				makeEvent(20, bellows.EventReviewApproved),
				makeEvent(20, bellows.EventReviewChanges),
				makeEvent(20, bellows.EventReviewApproved),
			},
			event:           makeEvent(20, bellows.EventReviewChanges),
			wantAction:      ActionNone,
			wantNeedsFix:    true,
			wantCIPassing:   true, // review events do not change CI state
			wantDBEventType: state.EventLifecycleExhausted,
		},
		{
			name:          "EventPRMerged closes bead",
			prNumber:      99,
			event:         makeEvent(99, bellows.EventPRMerged),
			wantAction:    ActionCloseBead,
			wantMerged:    true,
			wantCIPassing: true, // merge event does not change CI state
		},
		{
			name:          "EventPRConflicting dispatch",
			prNumber:      55,
			event:         makeEvent(55, bellows.EventPRConflicting),
			wantAction:    ActionRebase,
			wantCIPassing: true, // conflict event does not change CI state
		},
		{
			name:          "EventPRClosed cleanup",
			prNumber:      33,
			event:         makeEvent(33, bellows.EventPRClosed),
			wantAction:    ActionCleanup,
			wantClosed:    true,
			wantCIPassing: true, // close event does not change CI state
		},
		{
			name:          "EventCIPassed",
			prNumber:      1,
			event:         makeEvent(1, bellows.EventCIPassed),
			wantAction:    ActionNone,
			wantNeedsFix:  false,
			wantCIPassing: true,
		},
		{
			name:          "EventReviewApproved",
			prNumber:      2,
			event:         makeEvent(2, bellows.EventReviewApproved),
			wantAction:    ActionNone,
			wantApproved:  true,
			wantCIPassing: true, // approve event does not change CI state
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			var got ActionRequest
			handler := func(_ context.Context, req ActionRequest) {
				got = req
			}
			m := New(db, testLogger(), handler)
			ctx := context.Background()

			for _, ev := range tc.setupEvents {
				m.HandleEvent(ctx, ev)
			}

			// Reset got before the final event
			got = ActionRequest{}
			m.HandleEvent(ctx, tc.event)

			if got.Action != tc.wantAction {
				t.Errorf("expected Action %v, got %v", tc.wantAction, got.Action)
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
			if st.NeedsFix != tc.wantNeedsFix {
				t.Errorf("expected NeedsFix=%v, got %v", tc.wantNeedsFix, st.NeedsFix)
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

			if tc.wantDBEventType != "" {
				events, err := db.RecentEvents(50)
				if err != nil {
					t.Fatalf("RecentEvents: %v", err)
				}
				found := false
				expectedPrefix := fmt.Sprintf("PR #%d", tc.prNumber)
				for _, e := range events {
					if string(e.Type) == string(tc.wantDBEventType) && strings.Contains(e.Message, expectedPrefix) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected DB event %q for PR #%d, found none", tc.wantDBEventType, tc.prNumber)
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