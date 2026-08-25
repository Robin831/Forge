package state

import (
	"path/filepath"
	"testing"
	"time"
)

func openAssayStatsTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestAssayRunSamplesSince(t *testing.T) {
	db := openAssayStatsTestDB(t)
	base := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	finished := base.Add(3 * time.Minute)

	runs := []*AssayRun{
		{Anvil: "forge", PRNumber: 1, StartedAt: base, FinishedAt: &finished,
			DurationMs: 180000, CostUSD: 0.40, Status: AssayStatusComplete},
		{Anvil: "forge", PRNumber: 2, StartedAt: base.Add(time.Hour),
			DurationMs: 240000, CostUSD: 1.10, Status: AssayStatusPartial},
		// Skipped: never reviewed a diff, so it must not dilute a mean.
		{Anvil: "forge", PRNumber: 3, StartedAt: base.Add(2 * time.Hour),
			SkippedReason: "diff fetch failed"},
		// Failed after spending: included, because a failure is not a refund.
		{Anvil: "forge", PRNumber: 4, StartedAt: base.Add(3 * time.Hour),
			DurationMs: 30000, CostUSD: 0.05, Status: AssayStatusFailed},
		// Before the cutoff.
		{Anvil: "forge", PRNumber: 5, StartedAt: base.AddDate(0, 0, -30), CostUSD: 99},
	}
	for _, r := range runs {
		if err := db.RecordAssayRun(r); err != nil {
			t.Fatalf("record run PR#%d: %v", r.PRNumber, err)
		}
	}

	samples, err := db.AssayRunSamplesSince(base.AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("AssayRunSamplesSince: %v", err)
	}
	if len(samples) != 3 {
		t.Fatalf("got %d samples, want 3 (skipped and pre-cutoff runs excluded): %+v", len(samples), samples)
	}
	if samples[0].Status != AssayStatusComplete || samples[0].CostUSD != 0.40 || samples[0].DurationMs != 180000 {
		t.Errorf("first sample = %+v", samples[0])
	}
	// finished_at wins where it exists...
	if !samples[0].CompletedAt.Equal(finished) {
		t.Errorf("CompletedAt = %s, want the recorded finished_at %s", samples[0].CompletedAt, finished)
	}
	// ...and started_at is the fallback where it does not, rather than a zero
	// time that would drop the run out of every week.
	if !samples[1].CompletedAt.Equal(base.Add(time.Hour)) {
		t.Errorf("unfinished run CompletedAt = %s, want the started_at fallback", samples[1].CompletedAt)
	}
	if samples[2].Status != AssayStatusFailed {
		t.Errorf("third sample = %+v, want the failed run", samples[2])
	}
}

func TestAssayRunSamplesSinceEmpty(t *testing.T) {
	db := openAssayStatsTestDB(t)
	samples, err := db.AssayRunSamplesSince(time.Now().AddDate(0, 0, -7))
	if err != nil {
		t.Fatalf("AssayRunSamplesSince: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("expected no samples on a fresh db, got %d", len(samples))
	}
}

func TestRecentAssayRunCostsUSD(t *testing.T) {
	db := openAssayStatsTestDB(t)
	base := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)

	runs := []*AssayRun{
		{Anvil: "forge", PRNumber: 1, StartedAt: base, CostUSD: 1.00, Status: AssayStatusComplete},
		{Anvil: "forge", PRNumber: 2, StartedAt: base.Add(time.Hour), CostUSD: 2.00, Status: AssayStatusComplete},
		// A failure is not a refund: its spend belongs in the sample set.
		{Anvil: "forge", PRNumber: 3, StartedAt: base.Add(2 * time.Hour), CostUSD: 3.00, Status: AssayStatusFailed},
		// Skipped: reviewed nothing, spent nothing — excluded, since it would
		// drag toward zero the mean that sizes an in-flight cost reservation.
		{Anvil: "forge", PRNumber: 4, StartedAt: base.Add(3 * time.Hour), SkippedReason: "no reviewable changes"},
	}
	for _, r := range runs {
		if err := db.RecordAssayRun(r); err != nil {
			t.Fatalf("record run PR#%d: %v", r.PRNumber, err)
		}
	}

	costs, err := db.RecentAssayRunCostsUSD(2)
	if err != nil {
		t.Fatalf("RecentAssayRunCostsUSD: %v", err)
	}
	if len(costs) != 2 {
		t.Fatalf("got %d costs, want the 2 most recent executed runs: %v", len(costs), costs)
	}
	if costs[0] != 3.00 || costs[1] != 2.00 {
		t.Errorf("costs = %v, want newest first [3 2]", costs)
	}

	all, err := db.RecentAssayRunCostsUSD(50)
	if err != nil {
		t.Fatalf("RecentAssayRunCostsUSD: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("got %d costs, want 3 (the skipped run excluded): %v", len(all), all)
	}

	none, err := db.RecentAssayRunCostsUSD(0)
	if err != nil || none != nil {
		t.Errorf("RecentAssayRunCostsUSD(0) = %v, %v; want nil, nil", none, err)
	}
}
