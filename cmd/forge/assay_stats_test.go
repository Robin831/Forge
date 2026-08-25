package main

import (
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/state"
)

func statsFixture(t *testing.T) []assay.WeeklyStats {
	t.Helper()
	week := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC) // 2026-W34
	return assay.WeeklyStatsFrom([]state.AssayRunSample{
		{CompletedAt: week, CostUSD: 0.40, DurationMs: 88000, Status: state.AssayStatusComplete},
		{CompletedAt: week.Add(time.Hour), CostUSD: 1.00, DurationMs: 116000, Status: state.AssayStatusPartial},
	}, 5)
}

func TestAssayStatsJSONCarriesSumsAndMeans(t *testing.T) {
	payload := assayStatsJSON(statsFixture(t), nil)
	if len(payload.Weeks) != 1 {
		t.Fatalf("expected 1 week, got %d", len(payload.Weeks))
	}
	w := payload.Weeks[0]
	if w.Week != "2026-W34" {
		t.Errorf("week = %q, want 2026-W34", w.Week)
	}
	if w.All.Runs != 2 || w.All.TotalCostUSD != 1.40 || w.All.MeanCostUSD != 0.70 {
		t.Errorf("all = %+v, want 2 runs totalling $1.40 at a $0.70 mean", w.All)
	}
	if w.Complete.Runs != 1 || w.Partial.Runs != 1 {
		t.Errorf("split = complete %d / partial %d, want 1 / 1", w.Complete.Runs, w.Partial.Runs)
	}
	// The sums are what makes the payload re-aggregatable: a consumer combining
	// weeks must not have to re-weight the means by hand.
	if w.Partial.MeanDurationS != 116 || w.Partial.TotalDurationS != 116 {
		t.Errorf("partial durations = %+v, want 116s", w.Partial)
	}
	if w.Failed.Runs != 0 || w.Failed.MeanCostUSD != 0 {
		t.Errorf("empty failed bucket = %+v, want zeroes rather than NaN", w.Failed)
	}
	if payload.Drift != nil {
		t.Errorf("expected no drift on a one-week fixture, got %+v", payload.Drift)
	}
}

func TestAssayStatsDefaultWindowMatchesTheDriftCheck(t *testing.T) {
	// The printed window must cover every week the drift check considers, or a
	// flagged WARN is unexplained by the lines above it.
	if defaultAssayStatsWeeks != assay.DriftTrailingWeeks+1 {
		t.Errorf("default window = %d weeks, want %d", defaultAssayStatsWeeks, assay.DriftTrailingWeeks+1)
	}
}

func TestPrintAssayStatsEmptyLedger(t *testing.T) {
	out := captureStdout(t, func() { printAssayStats(nil, nil, 5) })
	if !strings.Contains(out, "No Assay runs recorded") {
		t.Errorf("empty ledger output = %q", out)
	}
}

func TestPrintAssayStatsIncludesDriftWarning(t *testing.T) {
	drift := &assay.Drift{Week: "2026-W34", Runs: 128, MeanCostUSD: 0.412,
		TrailingWeeks: 4, TrailingRuns: 512, TrailingMeanCostUSD: 0.184, Ratio: 2.24}
	out := captureStdout(t, func() { printAssayStats(statsFixture(t), drift, 5) })
	for _, want := range []string{"2026-W34:", "complete 1 run", "partial 1 run", "WARNING cost drift", "2.24x"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q is missing %q", out, want)
		}
	}
}
