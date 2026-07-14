package state

import (
	"testing"
	"time"
)

// newTurnSnapshotSession creates a parent forge_session so snapshot rows have a
// valid session_id foreign key.
func newTurnSnapshotSession(t *testing.T, db *DB) int64 {
	t.Helper()
	s, err := db.CreateForgeSession(ForgeSession{Title: "turn snapshot", Anvil: "anvil-1"})
	if err != nil {
		t.Fatalf("CreateForgeSession: %v", err)
	}
	return s.ID
}

func TestUpsertTurnSnapshot_InsertThenUpdate(t *testing.T) {
	db := openTestDB(t)
	sessionID := newTurnSnapshotSession(t, db)

	// First write inserts the row.
	snap, err := db.UpsertTurnSnapshot(sessionID, "turn-1", ForgeTurnStatusInProgress, "Hello")
	if err != nil {
		t.Fatalf("UpsertTurnSnapshot insert: %v", err)
	}
	if snap.Status != ForgeTurnStatusInProgress || snap.AccumulatedText != "Hello" {
		t.Errorf("unexpected snapshot after insert: %+v", snap)
	}
	if snap.UpdatedAt.IsZero() {
		t.Error("expected updated_at to be set")
	}

	// Second write for the same (session, turn) overwrites in place — no
	// duplicate row, status transitions, and text is replaced (not appended).
	if _, err := db.UpsertTurnSnapshot(sessionID, "turn-1", ForgeTurnStatusComplete, "Hello, world"); err != nil {
		t.Fatalf("UpsertTurnSnapshot update: %v", err)
	}

	got, err := db.GetTurnSnapshot(sessionID, "turn-1")
	if err != nil {
		t.Fatalf("GetTurnSnapshot: %v", err)
	}
	if got == nil {
		t.Fatal("snapshot not found after upsert")
	}
	if got.Status != ForgeTurnStatusComplete {
		t.Errorf("expected status %q, got %q", ForgeTurnStatusComplete, got.Status)
	}
	if got.AccumulatedText != "Hello, world" {
		t.Errorf("expected text overwritten, got %q", got.AccumulatedText)
	}

	// Confirm the upsert did not create a duplicate row for the same turn.
	var count int
	if err := db.conn.QueryRow(
		`SELECT COUNT(*) FROM forge_turn_snapshots WHERE session_id = ? AND turn_id = ?`,
		sessionID, "turn-1",
	).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly 1 row for (session, turn), got %d", count)
	}
}

func TestUpsertTurnSnapshot_DefaultsStatus(t *testing.T) {
	db := openTestDB(t)
	sessionID := newTurnSnapshotSession(t, db)

	snap, err := db.UpsertTurnSnapshot(sessionID, "turn-1", "", "partial")
	if err != nil {
		t.Fatalf("UpsertTurnSnapshot: %v", err)
	}
	if snap.Status != ForgeTurnStatusInProgress {
		t.Errorf("expected default status %q, got %q", ForgeTurnStatusInProgress, snap.Status)
	}
}

func TestUpsertTurnSnapshot_Validation(t *testing.T) {
	db := openTestDB(t)
	sessionID := newTurnSnapshotSession(t, db)

	if _, err := db.UpsertTurnSnapshot(0, "turn-1", ForgeTurnStatusInProgress, "x"); err == nil {
		t.Error("expected error for zero session_id")
	}
	if _, err := db.UpsertTurnSnapshot(sessionID, "", ForgeTurnStatusInProgress, "x"); err == nil {
		t.Error("expected error for empty turn_id")
	}
}

func TestGetLatestTurnSnapshot_ReturnsNewest(t *testing.T) {
	db := openTestDB(t)
	sessionID := newTurnSnapshotSession(t, db)

	if _, err := db.UpsertTurnSnapshot(sessionID, "turn-1", ForgeTurnStatusComplete, "first"); err != nil {
		t.Fatalf("upsert turn-1: %v", err)
	}
	// Sleep so the second snapshot's updated_at is strictly later under the
	// fixed-width dbTimeLayout, making "latest" deterministic.
	time.Sleep(2 * time.Millisecond)
	if _, err := db.UpsertTurnSnapshot(sessionID, "turn-2", ForgeTurnStatusInProgress, "second"); err != nil {
		t.Fatalf("upsert turn-2: %v", err)
	}

	got, err := db.GetLatestTurnSnapshot(sessionID)
	if err != nil {
		t.Fatalf("GetLatestTurnSnapshot: %v", err)
	}
	if got == nil {
		t.Fatal("expected a snapshot, got nil")
	}
	if got.TurnID != "turn-2" {
		t.Errorf("expected newest turn-2, got %q", got.TurnID)
	}
	if got.AccumulatedText != "second" {
		t.Errorf("expected text %q, got %q", "second", got.AccumulatedText)
	}
}

func TestGetLatestTurnSnapshot_NoneReturnsNil(t *testing.T) {
	db := openTestDB(t)
	sessionID := newTurnSnapshotSession(t, db)

	got, err := db.GetLatestTurnSnapshot(sessionID)
	if err != nil {
		t.Fatalf("GetLatestTurnSnapshot: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil snapshot for empty session, got %+v", got)
	}
}

func TestGetTurnSnapshot_MissingReturnsNil(t *testing.T) {
	db := openTestDB(t)
	sessionID := newTurnSnapshotSession(t, db)

	got, err := db.GetTurnSnapshot(sessionID, "does-not-exist")
	if err != nil {
		t.Fatalf("GetTurnSnapshot: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing snapshot, got %+v", got)
	}
}

func TestGetLatestTurnSnapshot_ScopedToSession(t *testing.T) {
	db := openTestDB(t)
	sessionA := newTurnSnapshotSession(t, db)
	sessionB := newTurnSnapshotSession(t, db)

	if _, err := db.UpsertTurnSnapshot(sessionA, "turn-a", ForgeTurnStatusInProgress, "a-text"); err != nil {
		t.Fatalf("upsert session A: %v", err)
	}
	if _, err := db.UpsertTurnSnapshot(sessionB, "turn-b", ForgeTurnStatusInProgress, "b-text"); err != nil {
		t.Fatalf("upsert session B: %v", err)
	}

	got, err := db.GetLatestTurnSnapshot(sessionA)
	if err != nil {
		t.Fatalf("GetLatestTurnSnapshot: %v", err)
	}
	if got == nil || got.TurnID != "turn-a" {
		t.Errorf("expected session A's own snapshot, got %+v", got)
	}
}
