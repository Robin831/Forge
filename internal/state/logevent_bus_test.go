package state

import (
	"path/filepath"
	"testing"
	"time"
)

// TestLogEventPublishesToBus verifies that a LogEvent call, once persisted, is
// fanned out to a Bus subscriber and that the delivered BusEvent's Seq / ID
// matches the persisted row.
func TestLogEventPublishesToBus(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	bus := NewBus(8)
	db.SetBus(bus)
	if db.Bus() != bus {
		t.Fatal("Bus() did not return the wired Bus")
	}

	ch, unsubscribe := bus.Subscribe()
	defer unsubscribe()

	if err := db.LogEvent(EventBeadClaimed, "claimed", "bd-1", "anvil-1"); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}

	var ev BusEvent
	select {
	case ev = <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for published event")
	}

	if ev.GapMarker {
		t.Fatal("unexpected gap marker")
	}
	if ev.Type != EventBeadClaimed {
		t.Errorf("Type = %q, want %q", ev.Type, EventBeadClaimed)
	}
	if ev.Message != "claimed" || ev.BeadID != "bd-1" || ev.Anvil != "anvil-1" {
		t.Errorf("unexpected event payload: %+v", ev.Event)
	}

	// The published Seq / ID must match the persisted row.
	persisted, err := db.RecentEvents(1)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("expected 1 persisted event, got %d", len(persisted))
	}
	if ev.Seq != int64(persisted[0].ID) {
		t.Errorf("Seq = %d, want %d", ev.Seq, persisted[0].ID)
	}
	if ev.ID != persisted[0].ID {
		t.Errorf("Event.ID = %d, want %d", ev.ID, persisted[0].ID)
	}

	// EventsSince(0) must return the same row the subscriber saw, confirming
	// the Seq can drive a Last-Event-ID resume.
	since, err := db.EventsSince(0, 10)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	if len(since) != 1 || int64(since[0].ID) != ev.Seq {
		t.Errorf("EventsSince returned %+v, want single row with ID %d", since, ev.Seq)
	}
}

// TestLogEventWithoutBus verifies LogEvent still succeeds when no Bus is wired.
func TestLogEventWithoutBus(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if db.Bus() != nil {
		t.Fatal("expected nil Bus by default")
	}
	if err := db.LogEvent(EventBeadClaimed, "msg", "bd-2", "anvil-2"); err != nil {
		t.Fatalf("LogEvent with nil Bus: %v", err)
	}

	events, err := db.RecentEvents(1)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event persisted, got %d", len(events))
	}
}

// TestLogEventDoesNotBlockOnFullSubscriber verifies that a slow subscriber
// whose buffer is full never stalls LogEvent — the Bus drops the oldest and
// delivers a gap marker (drop-oldest design), so LogEvent returns promptly.
func TestLogEventDoesNotBlockOnFullSubscriber(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Small buffer so it fills quickly; the subscriber never drains.
	bus := NewBus(2)
	db.SetBus(bus)
	_, unsubscribe := bus.Subscribe()
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			if err := db.LogEvent(EventBeadClaimed, "flood", "bd-3", "anvil-3"); err != nil {
				t.Errorf("LogEvent %d: %v", i, err)
				break
			}
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("LogEvent blocked on a full subscriber buffer")
	}
}
