package state

import (
	"path/filepath"
	"testing"
	"time"
)

func openAssayCostTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func recordRun(t *testing.T, db *DB, anvil string, pr int, at time.Time, cost float64) {
	t.Helper()
	if err := db.RecordAssayRun(&AssayRun{
		Anvil: anvil, PRNumber: pr, HeadSHA: "sha", StartedAt: at,
		CostUSD: cost, Status: AssayStatusComplete,
	}); err != nil {
		t.Fatalf("record run: %v", err)
	}
}

// The window selects PRs, and a selected PR brings its whole history. Without
// that, a repeat review inside the window whose first review predates it would
// be reported as a first review.
func TestAssayRunHistoryForWindowReturnsFullPRHistory(t *testing.T) {
	db := openAssayCostTestDB(t)
	base := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	recordRun(t, db, "a", 1, base.Add(-72*time.Hour), 5) // long before the window
	recordRun(t, db, "a", 1, base, 3)                    // inside it
	recordRun(t, db, "a", 2, base.Add(-72*time.Hour), 9) // a PR with nothing in the window

	rows, err := db.AssayRunHistoryForWindow(base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("AssayRunHistoryForWindow: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (both runs of PR 1, none of PR 2)", len(rows))
	}
	for _, r := range rows {
		if r.PRNumber != 1 {
			t.Errorf("row for PR %d, want only PR 1", r.PRNumber)
		}
	}
	if !rows[0].StartedAt.Before(rows[1].StartedAt) {
		t.Error("rows are not ordered oldest first")
	}
}

func TestAssayRunHistoryForWindowOpenBounds(t *testing.T) {
	db := openAssayCostTestDB(t)
	base := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	recordRun(t, db, "a", 1, base, 1)
	recordRun(t, db, "a", 2, base.Add(48*time.Hour), 2)

	all, err := db.AssayRunHistoryForWindow(time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("unbounded query: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("unbounded query returned %d rows, want 2", len(all))
	}

	fromLater, err := db.AssayRunHistoryForWindow(base.Add(24*time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("open upper bound: %v", err)
	}
	if len(fromLater) != 1 || fromLater[0].PRNumber != 2 {
		t.Errorf("open upper bound returned %d rows, want just PR 2", len(fromLater))
	}
}

// The upper bound is exclusive so consecutive windows tile without a run being
// counted in both.
func TestAssayRunHistoryForWindowUpperBoundIsExclusive(t *testing.T) {
	db := openAssayCostTestDB(t)
	boundary := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	recordRun(t, db, "a", 1, boundary, 1)

	before, err := db.AssayRunHistoryForWindow(boundary.Add(-time.Hour), boundary)
	if err != nil {
		t.Fatalf("earlier window: %v", err)
	}
	if len(before) != 0 {
		t.Errorf("earlier window claimed the boundary run (%d rows)", len(before))
	}

	after, err := db.AssayRunHistoryForWindow(boundary, boundary.Add(time.Hour))
	if err != nil {
		t.Fatalf("later window: %v", err)
	}
	if len(after) != 1 {
		t.Errorf("later window is missing the boundary run (%d rows)", len(after))
	}
}

// PR numbers are per-repository, so two anvils sharing a number must come back
// as two separate PRs' histories.
func TestAssayRunHistoryForWindowKeepsAnvilsApart(t *testing.T) {
	db := openAssayCostTestDB(t)
	base := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	recordRun(t, db, "alpha", 1, base.Add(-48*time.Hour), 1)
	recordRun(t, db, "beta", 1, base, 2)

	rows, err := db.AssayRunHistoryForWindow(base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("AssayRunHistoryForWindow: %v", err)
	}
	if len(rows) != 1 || rows[0].Anvil != "beta" {
		t.Fatalf("got %d rows (first anvil %q), want only beta's run", len(rows), rows[0].Anvil)
	}
}

// Cache columns and the skip reason are what the attribution report classifies
// on, so they must survive the round trip.
func TestAssayRunHistoryForWindowCarriesCacheAndSkipColumns(t *testing.T) {
	db := openAssayCostTestDB(t)
	at := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	if err := db.RecordAssayRun(&AssayRun{
		Anvil: "a", PRNumber: 1, HeadSHA: "sha", StartedAt: at,
		CostUSD: 2.5, FindingsCount: 3,
		CacheCreationTokens: 900, CacheReadTokens: 41500,
		Status: AssayStatusComplete,
	}); err != nil {
		t.Fatalf("record run: %v", err)
	}
	if err := db.RecordAssayRun(&AssayRun{
		Anvil: "a", PRNumber: 1, HeadSHA: "sha2", StartedAt: at.Add(time.Hour),
		SkippedReason: "no_reviewable_changes",
	}); err != nil {
		t.Fatalf("record skipped run: %v", err)
	}

	rows, err := db.AssayRunHistoryForWindow(at.Add(-time.Hour), at.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("AssayRunHistoryForWindow: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].CacheCreationTokens != 900 || rows[0].CacheReadTokens != 41500 {
		t.Errorf("cache tokens = %d/%d, want 900/41500", rows[0].CacheCreationTokens, rows[0].CacheReadTokens)
	}
	if rows[0].FindingsCount != 3 || rows[0].CostUSD != 2.5 {
		t.Errorf("findings/cost = %d/%.2f, want 3/2.50", rows[0].FindingsCount, rows[0].CostUSD)
	}
	if rows[1].SkippedReason != "no_reviewable_changes" {
		t.Errorf("skipped reason = %q, want no_reviewable_changes", rows[1].SkippedReason)
	}
}

func TestAssayRunHistoryForWindowEmptyDatabase(t *testing.T) {
	db := openAssayCostTestDB(t)
	rows, err := db.AssayRunHistoryForWindow(time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("AssayRunHistoryForWindow on an empty table: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want 0", len(rows))
	}
}
