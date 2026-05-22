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
	if st.Events == nil || st.Done == nil {
		t.Fatal("Events/Done channels should be initialised")
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
