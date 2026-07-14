package web

import (
	"errors"
	"testing"
	"time"
)

func TestTurnStore_NewAssignsPendingStatus(t *testing.T) {
	store := NewTurnStore()
	st := store.New("turn-1", 42)
	if st == nil {
		t.Fatal("New returned nil")
	}
	if st.Status() != TurnStatusPending {
		t.Fatalf("expected pending, got %s", st.Status())
	}
	if st.ID != "turn-1" {
		t.Fatalf("expected id turn-1, got %s", st.ID)
	}
	if st.SessionID != 42 {
		t.Fatalf("expected session id 42, got %d", st.SessionID)
	}
	if st.Done == nil {
		t.Fatal("Done channel should be initialised")
	}
}

func TestTurnStore_GetReturnsRegisteredEntry(t *testing.T) {
	store := NewTurnStore()
	st := store.New("turn-1", 7)
	got, ok := store.Get("turn-1")
	if !ok || got != st {
		t.Fatalf("Get should return the registered TurnState, got %v ok=%v", got, ok)
	}
	if _, ok := store.Get("missing"); ok {
		t.Fatal("Get on unknown id should return ok=false")
	}
}

func TestTurnStore_DeleteRemovesEntry(t *testing.T) {
	store := NewTurnStore()
	store.New("turn-1", 1)
	store.New("turn-2", 2)
	if got := store.Len(); got != 2 {
		t.Fatalf("expected 2 entries, got %d", got)
	}
	store.Delete("turn-1")
	if _, ok := store.Get("turn-1"); ok {
		t.Fatal("Delete should remove the entry")
	}
	if got := store.Len(); got != 1 {
		t.Fatalf("expected 1 entry after delete, got %d", got)
	}
}

func TestTurnState_StatusMutationsAndSnapshot(t *testing.T) {
	st := newTurnState("t", 1, 4)
	st.AppendText("hello ")
	st.AppendText("world")
	if got := st.Text(); got != "hello world" {
		t.Fatalf("expected accumulated text, got %q", got)
	}
	st.RecordToolEvent(TurnEvent{Type: TurnEventToolUse, Data: "Grep"})
	st.SetFinalMessageID(99)
	st.setStatus(TurnStatusComplete)

	snap := st.Snapshot()
	if snap.Status != TurnStatusComplete {
		t.Fatalf("expected complete in snapshot, got %s", snap.Status)
	}
	if snap.Text != "hello world" {
		t.Fatalf("snapshot text wrong: %q", snap.Text)
	}
	if snap.FinalMessageID != 99 {
		t.Fatalf("snapshot final id wrong: %d", snap.FinalMessageID)
	}
	if len(snap.ToolEvents) != 1 || snap.ToolEvents[0].Type != TurnEventToolUse {
		t.Fatalf("snapshot tool events wrong: %+v", snap.ToolEvents)
	}
}

func TestTurnState_SetErrorRecordsAndTransitions(t *testing.T) {
	st := newTurnState("t", 1, 4)
	st.SetError(errors.New("boom"))
	if st.Status() != TurnStatusError {
		t.Fatalf("expected error status, got %s", st.Status())
	}
	if st.Err() == nil || st.Err().Error() != "boom" {
		t.Fatalf("expected boom error, got %v", st.Err())
	}
	snap := st.Snapshot()
	if snap.Error != "boom" {
		t.Fatalf("snapshot error wrong: %q", snap.Error)
	}
}

// TestTurnStore_ExpiresCompletedTurnAfterExpiry drives a fake clock to assert
// a completed turn survives until expiry elapses, then is garbage-collected by
// the sweep (and reads as not-found through Get) once it does.
func TestTurnStore_ExpiresCompletedTurnAfterExpiry(t *testing.T) {
	now := time.Unix(1_000, 0)
	store := NewTurnStore()
	store.now = func() time.Time { return now }
	store.Configure(30*time.Minute, 0)

	st := store.New("turn-1", 1)
	st.setStatus(TurnStatusComplete)

	// Still fresh: 29 minutes after completion.
	now = now.Add(29 * time.Minute)
	if _, ok := store.Get("turn-1"); !ok {
		t.Fatal("turn should still be present before expiry elapses")
	}
	if removed := store.sweep(); removed != 0 {
		t.Fatalf("sweep should not remove an unexpired turn, removed %d", removed)
	}

	// Cross the 30m boundary.
	now = now.Add(2 * time.Minute)
	if _, ok := store.Get("turn-1"); ok {
		t.Fatal("Get should treat an expired turn as not-found (lazy expiry)")
	}
	if store.Len() != 0 {
		t.Fatalf("lazy Get should have removed the expired turn, len=%d", store.Len())
	}
}

// TestTurnStore_SweepRemovesExpiredButKeepsRunning verifies the background
// sweep reclaims expired completed turns while leaving in-flight turns (no
// completion timestamp) untouched even when they are older than expiry.
func TestTurnStore_SweepRemovesExpiredButKeepsRunning(t *testing.T) {
	now := time.Unix(5_000, 0)
	store := NewTurnStore()
	store.now = func() time.Time { return now }
	store.Configure(10*time.Minute, 0)

	done := store.New("done", 1)
	done.setStatus(TurnStatusComplete)
	running := store.New("running", 1)
	running.setStatus(TurnStatusRunning)

	now = now.Add(11 * time.Minute)
	removed := store.sweep()
	if removed != 1 {
		t.Fatalf("expected 1 turn swept, got %d", removed)
	}
	if _, ok := store.Get("done"); ok {
		t.Fatal("completed+expired turn should be swept")
	}
	if _, ok := store.Get("running"); !ok {
		t.Fatal("still-running turn must never be expired regardless of age")
	}
}

// TestTurnStore_RetentionCapEvictsOldest inserts cap+N completed turns and
// asserts only the newest `cap` survive, with the oldest-completed evicted.
func TestTurnStore_RetentionCapEvictsOldest(t *testing.T) {
	now := time.Unix(0, 0)
	store := NewTurnStore()
	store.now = func() time.Time { return now }
	// Disable expiry so this test isolates the retention-cap policy.
	store.Configure(0, 3)

	// Complete five turns, each one minute apart so completion order is
	// deterministic. New() enforces the cap as each turn is added.
	for i := 0; i < 5; i++ {
		id := "turn-" + string(rune('a'+i))
		st := store.New(id, 1)
		st.setStatus(TurnStatusComplete)
		now = now.Add(time.Minute)
	}
	// Adding turns past the cap while every prior turn is already completed
	// only evicts on the next New(); force a final reconciliation.
	store.sweep()

	if store.Len() != 3 {
		t.Fatalf("expected cap of 3 retained turns, got %d", store.Len())
	}
	// The two oldest (a, b) should be gone; the three newest (c, d, e) kept.
	for _, gone := range []string{"turn-a", "turn-b"} {
		if _, ok := store.Get(gone); ok {
			t.Fatalf("oldest completed turn %s should have been evicted", gone)
		}
	}
	for _, kept := range []string{"turn-c", "turn-d", "turn-e"} {
		if _, ok := store.Get(kept); !ok {
			t.Fatalf("newest completed turn %s should be retained", kept)
		}
	}
}

// TestTurnStore_RetentionCapKeepsRunningTurns confirms the cap never evicts an
// in-flight turn even when the completed set alone cannot bring the count back
// under the cap.
func TestTurnStore_RetentionCapKeepsRunningTurns(t *testing.T) {
	store := NewTurnStore()
	store.Configure(0, 2)

	r1 := store.New("run-1", 1)
	r1.setStatus(TurnStatusRunning)
	r2 := store.New("run-2", 1)
	r2.setStatus(TurnStatusRunning)
	r3 := store.New("run-3", 1)
	r3.setStatus(TurnStatusRunning)

	// All three are in flight; none may be evicted even though 3 > cap of 2.
	if store.Len() != 3 {
		t.Fatalf("running turns must not be evicted by the cap, len=%d", store.Len())
	}
}

// TestTurnStore_ConfigureDisablesExpiryWithZero asserts a non-positive expiry
// disables GC entirely so completed turns are retained indefinitely.
func TestTurnStore_ConfigureDisablesExpiryWithZero(t *testing.T) {
	now := time.Unix(0, 0)
	store := NewTurnStore()
	store.now = func() time.Time { return now }
	store.Configure(0, 0)

	st := store.New("turn-1", 1)
	st.setStatus(TurnStatusComplete)

	now = now.Add(1000 * time.Hour)
	if removed := store.sweep(); removed != 0 {
		t.Fatalf("expiry disabled: sweep must not remove anything, removed %d", removed)
	}
	if _, ok := store.Get("turn-1"); !ok {
		t.Fatal("expiry disabled: completed turn should be retained indefinitely")
	}
}

// Emit is non-blocking: when the buffer is full, the producer drops the
// event rather than stalling the goroutine that owns the TurnState. This
// matters because the SSE consumer may be slow or absent.
func TestTurnState_EmitDoesNotBlockWhenFull(t *testing.T) {
	st := newTurnState("t", 1, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			st.Emit(TurnEvent{Type: TurnEventTextDelta, Data: i})
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Emit should not block when the buffer is full")
	}
}
