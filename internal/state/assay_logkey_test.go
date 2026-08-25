package state

import (
	"path/filepath"
	"testing"
	"time"
)

func openLogKeyTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestAssayRunsByLogKeys(t *testing.T) {
	db := openLogKeyTestDB(t)

	base := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	first := &AssayRun{
		Anvil: "forge", PRNumber: 347, HeadSHA: "abc", StartedAt: base,
		LogKey: "1755000000000", Status: AssayStatusComplete,
		CompletedPasses: 5, TotalPasses: 5, FindingsCount: 1,
		CostUSD: 8.75, DurationMs: 213700,
		PassFindings: []AssayPassFindings{{Name: "triage"}, {Name: "logic", Findings: 1}},
	}
	if err := db.RecordAssayRun(first); err != nil {
		t.Fatalf("record first: %v", err)
	}
	second := &AssayRun{
		Anvil: "forge", PRNumber: 347, HeadSHA: "def", StartedAt: base.Add(time.Hour),
		LogKey: "1755003600000", Status: AssayStatusPartial,
		CompletedPasses: 3, TotalPasses: 5,
		FailedPasses: []AssayPassFailure{{Name: "logic", Reason: "error_max_turns"}},
	}
	if err := db.RecordAssayRun(second); err != nil {
		t.Fatalf("record second: %v", err)
	}
	// A run with no key at all must never be picked up by a key lookup.
	if err := db.RecordAssayRun(&AssayRun{Anvil: "forge", PRNumber: 347, StartedAt: base}); err != nil {
		t.Fatalf("record keyless: %v", err)
	}

	got, err := db.AssayRunsByLogKeys([]string{"1755000000000", "1755003600000", "missing"})
	if err != nil {
		t.Fatalf("AssayRunsByLogKeys: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 runs, got %d: %+v", len(got), got)
	}
	// A key with no row is absent, not a zeroed row: "not recorded" and "cost
	// nothing, found nothing" must not read the same downstream.
	if _, ok := got["missing"]; ok {
		t.Error("unknown key resolved to a run")
	}

	a := got["1755000000000"]
	if a.ID != first.ID || a.Status != AssayStatusComplete || a.FindingsCount != 1 || a.CostUSD != 8.75 {
		t.Errorf("first run round-trip = %+v", a)
	}
	if len(a.PassFindings) != 2 || a.PassFindings[1].Name != "logic" || a.PassFindings[1].Findings != 1 {
		t.Errorf("pass findings round-trip = %+v", a.PassFindings)
	}
	if !a.StartedAt.Equal(base) {
		t.Errorf("started_at = %v, want %v", a.StartedAt, base)
	}

	b := got["1755003600000"]
	if b.Status != AssayStatusPartial || b.CompletedPasses != 3 || len(b.FailedPasses) != 1 {
		t.Errorf("second run round-trip = %+v", b)
	}
	if len(b.PassFindings) != 0 {
		t.Errorf("unrecorded pass findings decoded as %+v, want none", b.PassFindings)
	}

	if empty, err := db.AssayRunsByLogKeys(nil); err != nil || len(empty) != 0 {
		t.Errorf("empty key list = %v, %v", empty, err)
	}
}
