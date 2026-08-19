package state

import (
	"path/filepath"
	"testing"
)

func openReviewFixDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestRecordReviewFixDispatch_CountsSameHead is the circuit breaker's whole
// premise: repeated dispatches against an unchanged head accumulate, so the
// caller can refuse the one that would rebuild identical work.
func TestRecordReviewFixDispatch_CountsSameHead(t *testing.T) {
	db := openReviewFixDB(t)

	for want := 1; want <= 3; want++ {
		got, err := db.RecordReviewFixDispatch("munin", 4727, "7729aad29")
		if err != nil {
			t.Fatalf("RecordReviewFixDispatch: %v", err)
		}
		if got != want {
			t.Fatalf("dispatch %d recorded attempts=%d, want %d", want, got, want)
		}
	}
}

// TestRecordReviewFixDispatch_ResetsOnNewHead pins the other half: a head that
// moved is genuinely new work and must not inherit the previous head's budget.
func TestRecordReviewFixDispatch_ResetsOnNewHead(t *testing.T) {
	db := openReviewFixDB(t)

	for i := 0; i < 3; i++ {
		if _, err := db.RecordReviewFixDispatch("munin", 4727, "old-head"); err != nil {
			t.Fatalf("RecordReviewFixDispatch: %v", err)
		}
	}
	if err := db.SetReviewFixDispatchResult("munin", 4727, ReviewFixResultPreserved); err != nil {
		t.Fatalf("SetReviewFixDispatchResult: %v", err)
	}

	got, err := db.RecordReviewFixDispatch("munin", 4727, "new-head")
	if err != nil {
		t.Fatalf("RecordReviewFixDispatch: %v", err)
	}
	if got != 1 {
		t.Errorf("attempts after head change = %d, want 1", got)
	}

	rec, err := db.GetReviewFixDispatch("munin", 4727)
	if err != nil {
		t.Fatalf("GetReviewFixDispatch: %v", err)
	}
	if rec == nil {
		t.Fatal("expected a dispatch row")
	}
	if rec.HeadSHA != "new-head" {
		t.Errorf("HeadSHA = %q, want new-head", rec.HeadSHA)
	}
	// The previous head's outcome must not be attributed to the new head.
	if rec.LastResult != "" {
		t.Errorf("LastResult = %q, want it cleared with the head change", rec.LastResult)
	}
}

func TestReviewFixDispatch_ResultAndDelete(t *testing.T) {
	db := openReviewFixDB(t)

	if _, err := db.RecordReviewFixDispatch("munin", 4727, "head"); err != nil {
		t.Fatalf("RecordReviewFixDispatch: %v", err)
	}
	if err := db.SetReviewFixDispatchResult("munin", 4727, ReviewFixResultUnverifiedPush); err != nil {
		t.Fatalf("SetReviewFixDispatchResult: %v", err)
	}
	rec, err := db.GetReviewFixDispatch("munin", 4727)
	if err != nil || rec == nil {
		t.Fatalf("GetReviewFixDispatch: %v (rec=%v)", err, rec)
	}
	if rec.LastResult != ReviewFixResultUnverifiedPush {
		t.Errorf("LastResult = %q, want %q", rec.LastResult, ReviewFixResultUnverifiedPush)
	}

	if err := db.DeleteReviewFixDispatch("munin", 4727); err != nil {
		t.Fatalf("DeleteReviewFixDispatch: %v", err)
	}
	rec, err = db.GetReviewFixDispatch("munin", 4727)
	if err != nil {
		t.Fatalf("GetReviewFixDispatch after delete: %v", err)
	}
	if rec != nil {
		t.Errorf("expected no row after delete, got %+v", rec)
	}

	// A missing row is not an error for either read or result-write: the
	// breaker must never fail a dispatch over its own bookkeeping.
	if err := db.SetReviewFixDispatchResult("munin", 4727, ReviewFixResultPushed); err != nil {
		t.Errorf("SetReviewFixDispatchResult on a missing row: %v", err)
	}
}

func TestRecordReviewFixDispatch_RejectsIncompleteKeys(t *testing.T) {
	db := openReviewFixDB(t)

	if _, err := db.RecordReviewFixDispatch("", 1, "head"); err == nil {
		t.Error("expected an error for an empty anvil")
	}
	if _, err := db.RecordReviewFixDispatch("munin", 0, "head"); err == nil {
		t.Error("expected an error for a missing PR number")
	}
	if _, err := db.RecordReviewFixDispatch("munin", 1, ""); err == nil {
		t.Error("expected an error for a missing head SHA")
	}
}

// TestReviewFixDispatch_IsolatedPerPR guards against one PR's loop spending
// another's budget.
func TestReviewFixDispatch_IsolatedPerPR(t *testing.T) {
	db := openReviewFixDB(t)

	if _, err := db.RecordReviewFixDispatch("munin", 1, "h"); err != nil {
		t.Fatalf("RecordReviewFixDispatch: %v", err)
	}
	if _, err := db.RecordReviewFixDispatch("munin", 1, "h"); err != nil {
		t.Fatalf("RecordReviewFixDispatch: %v", err)
	}
	got, err := db.RecordReviewFixDispatch("munin", 2, "h")
	if err != nil {
		t.Fatalf("RecordReviewFixDispatch: %v", err)
	}
	if got != 1 {
		t.Errorf("second PR started at attempts=%d, want 1", got)
	}
}

// TestUndoReviewFixDispatch_GivesBackRateLimitedAttempt covers the fix for the
// 2026-08-19 incident: a dispatch that never ran (every provider rate limited)
// must not consume the same-head budget, so undoing it restores the previous
// count, floors at zero, tags the row rate_limited, and tolerates a missing row.
func TestUndoReviewFixDispatch_GivesBackRateLimitedAttempt(t *testing.T) {
	db := openReviewFixDB(t)

	for i := 0; i < 2; i++ {
		if _, err := db.RecordReviewFixDispatch("munin", 4801, "abc123"); err != nil {
			t.Fatalf("RecordReviewFixDispatch: %v", err)
		}
	}
	if err := db.UndoReviewFixDispatch("munin", 4801); err != nil {
		t.Fatalf("UndoReviewFixDispatch: %v", err)
	}
	rec, err := db.GetReviewFixDispatch("munin", 4801)
	if err != nil {
		t.Fatalf("GetReviewFixDispatch: %v", err)
	}
	if rec.Attempts != 1 {
		t.Fatalf("attempts after undo = %d, want 1", rec.Attempts)
	}
	if rec.LastResult != ReviewFixResultRateLimited {
		t.Fatalf("last_result after undo = %q, want %q", rec.LastResult, ReviewFixResultRateLimited)
	}

	// Undoing past zero must floor, not go negative.
	for i := 0; i < 3; i++ {
		if err := db.UndoReviewFixDispatch("munin", 4801); err != nil {
			t.Fatalf("UndoReviewFixDispatch: %v", err)
		}
	}
	rec, err = db.GetReviewFixDispatch("munin", 4801)
	if err != nil {
		t.Fatalf("GetReviewFixDispatch: %v", err)
	}
	if rec.Attempts != 0 {
		t.Fatalf("attempts after repeated undo = %d, want 0", rec.Attempts)
	}

	// The next real dispatch counts up from the floored value.
	got, err := db.RecordReviewFixDispatch("munin", 4801, "abc123")
	if err != nil {
		t.Fatalf("RecordReviewFixDispatch: %v", err)
	}
	if got != 1 {
		t.Fatalf("attempts after re-record = %d, want 1", got)
	}

	// No row at all: a no-op, not an error.
	if err := db.UndoReviewFixDispatch("munin", 9999); err != nil {
		t.Fatalf("UndoReviewFixDispatch on missing row: %v", err)
	}
}
