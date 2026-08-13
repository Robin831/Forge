package state

// Tests for the repeat-review state changes: failed runs no longer mark a head
// reviewed, and the finding queries the cross-run dedupe / posting guards read.

import (
	"path/filepath"
	"testing"
	"time"
)

func openRereviewTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestLastReviewedSHASkipsFailedRuns(t *testing.T) {
	db := openRereviewTestDB(t)
	now := time.Now()

	if err := db.RecordAssayRun(&AssayRun{
		Anvil: "a", PRNumber: 7, HeadSHA: "good", StartedAt: now,
		Status: AssayStatusComplete,
	}); err != nil {
		t.Fatalf("record complete run: %v", err)
	}
	if err := db.RecordAssayRun(&AssayRun{
		Anvil: "a", PRNumber: 7, HeadSHA: "bad", StartedAt: now,
		Status: AssayStatusFailed, Error: "every pass died",
	}); err != nil {
		t.Fatalf("record failed run: %v", err)
	}

	sha, err := db.LastReviewedSHA("a", 7)
	if err != nil {
		t.Fatalf("LastReviewedSHA: %v", err)
	}
	if sha != "good" {
		t.Errorf("failed run must not mark its head reviewed; got %q, want \"good\"", sha)
	}
}

func TestLastReviewedSHACountsLegacyStatuslessRuns(t *testing.T) {
	db := openRereviewTestDB(t)
	if err := db.RecordAssayRun(&AssayRun{
		Anvil: "a", PRNumber: 7, HeadSHA: "legacy", StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("record legacy run: %v", err)
	}
	sha, err := db.LastReviewedSHA("a", 7)
	if err != nil {
		t.Fatalf("LastReviewedSHA: %v", err)
	}
	if sha != "legacy" {
		t.Errorf("status-less legacy row should count as reviewed; got %q", sha)
	}
}

func TestAllFindingsIncludesResolved(t *testing.T) {
	db := openRereviewTestDB(t)
	for _, f := range []Finding{
		{Anvil: "a", PRNumber: 7, FindingHash: "open-1", Anchor: "a.go:1", Category: "logic", Body: "x"},
		{Anvil: "a", PRNumber: 7, FindingHash: "res-1", Anchor: "a.go:2", Category: "logic", Body: "y"},
	} {
		if err := db.InsertFinding(f); err != nil {
			t.Fatalf("insert %s: %v", f.FindingHash, err)
		}
	}
	if err := db.MarkResolved("res-1"); err != nil {
		t.Fatalf("MarkResolved: %v", err)
	}

	all, err := db.AllFindings("a", 7)
	if err != nil {
		t.Fatalf("AllFindings: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 findings (resolved included), got %d", len(all))
	}
	byHash := map[string]Finding{}
	for _, f := range all {
		byHash[f.FindingHash] = f
	}
	if byHash["open-1"].ResolvedAt != nil {
		t.Error("open finding should have nil ResolvedAt")
	}
	if byHash["res-1"].ResolvedAt == nil {
		t.Error("resolved finding should carry its ResolvedAt")
	}

	active, err := db.ActiveFindings("a", 7)
	if err != nil {
		t.Fatalf("ActiveFindings: %v", err)
	}
	if len(active) != 1 || active[0].FindingHash != "open-1" {
		t.Errorf("ActiveFindings should exclude resolved rows, got %+v", active)
	}
}

func TestResolvedFindingHashes(t *testing.T) {
	db := openRereviewTestDB(t)
	for _, f := range []Finding{
		{Anvil: "a", PRNumber: 7, FindingHash: "open-1"},
		{Anvil: "a", PRNumber: 7, FindingHash: "res-1"},
		{Anvil: "a", PRNumber: 8, FindingHash: "other-pr"},
	} {
		if err := db.InsertFinding(f); err != nil {
			t.Fatalf("insert %s: %v", f.FindingHash, err)
		}
	}
	for _, h := range []string{"res-1", "other-pr"} {
		if err := db.MarkResolved(h); err != nil {
			t.Fatalf("MarkResolved %s: %v", h, err)
		}
	}

	got, err := db.ResolvedFindingHashes("a", 7)
	if err != nil {
		t.Fatalf("ResolvedFindingHashes: %v", err)
	}
	if len(got) != 1 || !got["res-1"] {
		t.Errorf("expected exactly {res-1}, got %v", got)
	}
}
