package assay

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// sample builds one run sample: completed at ts, costing cost, lasting secs,
// with the given persisted status.
func sample(ts time.Time, status string, cost float64, secs float64) state.AssayRunSample {
	return state.AssayRunSample{
		CompletedAt: ts,
		DurationMs:  int64(secs * 1000),
		CostUSD:     cost,
		Status:      status,
	}
}

func TestWeeklyBucketingCrossesTheYearBoundary(t *testing.T) {
	// 2025-12-29 is a Monday and belongs to ISO week 1 of 2026, not week 53 of
	// 2025. Bucketing on the calendar year would split that week in two, and
	// the label is what an operator matches one report against the next by.
	cases := []struct {
		ts   time.Time
		want string
	}{
		{time.Date(2025, 12, 28, 23, 0, 0, 0, time.UTC), "2025-W52"},
		{time.Date(2025, 12, 29, 0, 0, 0, 0, time.UTC), "2026-W01"},
		{time.Date(2026, 1, 4, 23, 59, 59, 0, time.UTC), "2026-W01"},
		{time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), "2026-W02"},
	}
	for _, c := range cases {
		stats := WeeklyStatsFrom([]state.AssayRunSample{
			sample(c.ts, state.AssayStatusComplete, 1, 10),
		}, 0)
		if len(stats) != 1 {
			t.Fatalf("%s: expected 1 week, got %d", c.ts.Format(time.RFC3339), len(stats))
		}
		if got := stats[0].Label(); got != c.want {
			t.Errorf("%s bucketed as %q, want %q", c.ts.Format(time.RFC3339), got, c.want)
		}
	}

	// The two sides of the boundary must land in the SAME bucket, which is the
	// failure a calendar-year fold produces and a label check alone can miss.
	spanning := WeeklyStatsFrom([]state.AssayRunSample{
		sample(time.Date(2025, 12, 30, 9, 0, 0, 0, time.UTC), state.AssayStatusComplete, 1, 10),
		sample(time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC), state.AssayStatusComplete, 1, 10),
	}, 0)
	if len(spanning) != 1 || spanning[0].All.Runs != 2 {
		t.Fatalf("a week spanning New Year must be one bucket of 2 runs, got %+v", spanning)
	}
}

func TestISOWeekStartIsMondayMidnightUTC(t *testing.T) {
	// A Sunday must resolve back to the Monday six days earlier, not forward:
	// Go's Weekday puts Sunday at 0, which an unadjusted offset reads as the
	// start of the week.
	sunday := time.Date(2026, 1, 4, 18, 30, 0, 0, time.UTC)
	want := time.Date(2025, 12, 29, 0, 0, 0, 0, time.UTC)
	if got := ISOWeekStart(sunday); !got.Equal(want) {
		t.Errorf("ISOWeekStart(Sunday) = %s, want %s", got, want)
	}
	// And the boundary crossing: the Monday itself is its own week start.
	if got := ISOWeekStart(want); !got.Equal(want) {
		t.Errorf("ISOWeekStart(Monday) = %s, want %s", got, want)
	}
	// A non-UTC input is normalised rather than bucketed in its own zone.
	east := time.FixedZone("east", 10*3600)
	if got := ISOWeekStart(time.Date(2026, 1, 5, 6, 0, 0, 0, east)); !got.Equal(want) {
		t.Errorf("ISOWeekStart(+10:00 Monday morning) = %s, want %s (still the prior ISO week in UTC)", got, want)
	}
}

func TestWeeklyStatsMeansAndOutcomeSplit(t *testing.T) {
	week := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC) // 2026-W34, a Tuesday
	samples := []state.AssayRunSample{
		sample(week, state.AssayStatusComplete, 0.30, 80),
		sample(week.Add(time.Hour), state.AssayStatusComplete, 0.50, 100),
		sample(week.Add(2*time.Hour), state.AssayStatusPartial, 1.00, 200),
	}

	stats := WeeklyStatsFrom(samples, 0)
	if len(stats) != 1 {
		t.Fatalf("expected 1 week, got %d", len(stats))
	}
	w := stats[0]
	if w.Label() != "2026-W34" {
		t.Errorf("label = %q, want 2026-W34", w.Label())
	}
	if w.All.Runs != 3 || w.Complete.Runs != 2 || w.Partial.Runs != 1 {
		t.Fatalf("run counts: all=%d complete=%d partial=%d", w.All.Runs, w.Complete.Runs, w.Partial.Runs)
	}
	if got, want := w.Complete.MeanCostUSD(), 0.40; math.Abs(got-want) > 1e-9 {
		t.Errorf("complete mean cost = %v, want %v", got, want)
	}
	if got, want := w.Partial.MeanCostUSD(), 1.00; math.Abs(got-want) > 1e-9 {
		t.Errorf("partial mean cost = %v, want %v", got, want)
	}
	if got, want := w.All.MeanDuration(), 380*time.Second/3; got != want {
		t.Errorf("overall mean duration = %v, want %v", got, want)
	}

	// The headline mean must be the count-weighted blend of the splits printed
	// beside it — the whole point of folding All from the outcome buckets
	// rather than accumulating it in parallel.
	blend := (w.Complete.MeanCostUSD()*float64(w.Complete.Runs) + w.Partial.MeanCostUSD()*float64(w.Partial.Runs)) /
		float64(w.All.Runs)
	if math.Abs(w.All.MeanCostUSD()-blend) > 1e-9 {
		t.Errorf("overall mean %v is not the blend of the splits %v", w.All.MeanCostUSD(), blend)
	}
}

func TestWeeklyStatsBucketsFailedAndUnknownRunsAsSpend(t *testing.T) {
	week := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	stats := WeeklyStatsFrom([]state.AssayRunSample{
		sample(week, state.AssayStatusFailed, 0.90, 40),
		sample(week, "", 0.10, 20), // pre-coverage row: real spend, no status
	}, 0)
	if len(stats) != 1 {
		t.Fatalf("expected 1 week, got %d", len(stats))
	}
	w := stats[0]
	if w.Failed.Runs != 1 || w.Unknown.Runs != 1 {
		t.Fatalf("failed=%d unknown=%d, want 1 and 1", w.Failed.Runs, w.Unknown.Runs)
	}
	if w.All.Runs != 2 {
		t.Fatalf("all runs = %d, want 2 (a failure is not a refund)", w.All.Runs)
	}
	if got, want := w.All.TotalCostUSD, 1.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("total cost = %v, want %v", got, want)
	}
}

func TestWeeklyStatsEdgeCases(t *testing.T) {
	t.Run("empty ledger", func(t *testing.T) {
		if got := WeeklyStatsFrom(nil, 5); len(got) != 0 {
			t.Fatalf("expected no weeks, got %d", len(got))
		}
	})

	t.Run("single run week has no NaN", func(t *testing.T) {
		w := WeeklyStatsFrom([]state.AssayRunSample{
			sample(time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC), state.AssayStatusComplete, 0.5, 60),
		}, 5)[0]
		if math.IsNaN(w.All.MeanCostUSD()) || math.IsNaN(w.Partial.MeanCostUSD()) {
			t.Fatal("NaN mean from an empty bucket")
		}
		if w.Partial.Runs != 0 || w.Partial.MeanCostUSD() != 0 || w.Partial.MeanDuration() != 0 {
			t.Errorf("empty partial bucket should read 0/0/0, got %+v", w.Partial)
		}
	})

	t.Run("partial-only week", func(t *testing.T) {
		w := WeeklyStatsFrom([]state.AssayRunSample{
			sample(time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC), state.AssayStatusPartial, 1.2, 300),
		}, 5)[0]
		line := RenderWeeklyCost(w)
		if !strings.Contains(line, "complete 0 runs") {
			t.Errorf("a zero complete split must still be rendered: %q", line)
		}
	})

	t.Run("run with no usable timestamp is dropped", func(t *testing.T) {
		if got := WeeklyStatsFrom([]state.AssayRunSample{
			sample(time.Time{}, state.AssayStatusComplete, 5, 10),
		}, 5); len(got) != 0 {
			t.Fatalf("expected the undateable run to be dropped, got %d weeks", len(got))
		}
	})
}

func TestWeeklyStatsKeepsNewestWeeksInOrder(t *testing.T) {
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC) // 2026-W28
	var samples []state.AssayRunSample
	for i := 0; i < 6; i++ {
		samples = append(samples, sample(base.AddDate(0, 0, 7*i), state.AssayStatusComplete, 1, 10))
	}
	// Deliberately out of order at the source: assay_runs is queried without a
	// guarantee that the fold sees them chronologically.
	samples[0], samples[5] = samples[5], samples[0]

	stats := WeeklyStatsFrom(samples, 3)
	if len(stats) != 3 {
		t.Fatalf("expected 3 weeks, got %d", len(stats))
	}
	want := []string{"2026-W31", "2026-W32", "2026-W33"}
	for i, w := range stats {
		if w.Label() != want[i] {
			t.Fatalf("week %d = %q, want %q (oldest first, newest kept)", i, w.Label(), want[i])
		}
	}
}

// weeklyFixture builds `n` consecutive weeks, each with `runs` complete runs at
// the given per-run cost, ending at the week containing `end`.
func weeklyFixture(end time.Time, n, runs int, costs ...float64) []WeeklyStats {
	var samples []state.AssayRunSample
	for i := 0; i < n; i++ {
		ts := end.AddDate(0, 0, -7*(n-1-i))
		cost := costs[len(costs)-1]
		if i < len(costs) {
			cost = costs[i]
		}
		for r := 0; r < runs; r++ {
			samples = append(samples, sample(ts.Add(time.Duration(r)*time.Minute), state.AssayStatusComplete, cost, 60))
		}
	}
	return WeeklyStatsFrom(samples, 0)
}

func TestCostDrift(t *testing.T) {
	end := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	t.Run("step change flags", func(t *testing.T) {
		stats := weeklyFixture(end, 5, 10, 0.50, 0.50, 0.50, 0.50, 1.10) // 2.2x
		d := CostDrift(stats)
		if d == nil {
			t.Fatal("expected a drift flag for a 2.2x step change")
		}
		if math.Abs(d.Ratio-2.2) > 1e-9 {
			t.Errorf("ratio = %v, want 2.2", d.Ratio)
		}
		if d.Week != "2026-W34" || d.Runs != 10 {
			t.Errorf("drift names the wrong week/runs: %+v", d)
		}
		if d.TrailingWeeks != 4 || d.TrailingRuns != 40 {
			t.Errorf("trailing window = %d weeks / %d runs, want 4 / 40", d.TrailingWeeks, d.TrailingRuns)
		}
		if !strings.Contains(d.Text(), "2.20x") {
			t.Errorf("drift text should name the ratio: %q", d.Text())
		}
	})

	t.Run("flat series does not flag", func(t *testing.T) {
		if d := CostDrift(weeklyFixture(end, 5, 10, 0.50)); d != nil {
			t.Fatalf("flat series flagged drift: %+v", d)
		}
	})

	t.Run("a rise under the threshold does not flag", func(t *testing.T) {
		if d := CostDrift(weeklyFixture(end, 5, 10, 0.50, 0.50, 0.50, 0.50, 0.70)); d != nil {
			t.Fatalf("1.4x flagged drift: %+v", d)
		}
	})

	t.Run("fewer than two trailing weeks does not flag", func(t *testing.T) {
		if d := CostDrift(weeklyFixture(end, 2, 10, 0.10, 5.00)); d != nil {
			t.Fatalf("cold ledger flagged drift: %+v", d)
		}
	})

	t.Run("trailing window is capped at four weeks", func(t *testing.T) {
		// Six weeks of history: the two oldest (cheap) weeks must fall out of
		// the window, so the comparison is against the four weeks before the
		// current one and the step change is measured against 0.50, not a mean
		// dragged down by history the check does not claim to consider.
		stats := weeklyFixture(end, 7, 10, 0.01, 0.01, 0.50, 0.50, 0.50, 0.50, 1.10)
		d := CostDrift(stats)
		if d == nil {
			t.Fatal("expected drift")
		}
		if d.TrailingWeeks != 4 || math.Abs(d.TrailingMeanCostUSD-0.50) > 1e-9 {
			t.Errorf("trailing window = %d weeks, mean %v; want 4 weeks at 0.50", d.TrailingWeeks, d.TrailingMeanCostUSD)
		}
	})

	t.Run("trailing weeks are pooled, not equally weighted", func(t *testing.T) {
		// One trailing week with a single expensive run must not outweigh three
		// busy cheap ones: pooled cost-per-run keeps both sides of the ratio
		// the same quantity.
		var samples []state.AssayRunSample
		for i := 0; i < 3; i++ {
			ts := end.AddDate(0, 0, -7*(4-i))
			for r := 0; r < 100; r++ {
				samples = append(samples, sample(ts.Add(time.Duration(r)*time.Second), state.AssayStatusComplete, 0.10, 60))
			}
		}
		samples = append(samples, sample(end.AddDate(0, 0, -7), state.AssayStatusComplete, 10.0, 60))
		for r := 0; r < 100; r++ {
			samples = append(samples, sample(end.Add(time.Duration(r)*time.Second), state.AssayStatusComplete, 0.30, 60))
		}
		d := CostDrift(WeeklyStatsFrom(samples, 0))
		if d == nil {
			t.Fatal("expected drift: 0.30/run against a pooled ~0.133/run")
		}
		if d.TrailingRuns != 301 {
			t.Errorf("trailing runs = %d, want 301", d.TrailingRuns)
		}
	})

	t.Run("empty current week does not flag", func(t *testing.T) {
		if d := CostDrift([]WeeklyStats{{Year: 2026, Week: 32}, {Year: 2026, Week: 33}, {Year: 2026, Week: 34}}); d != nil {
			t.Fatalf("empty weeks flagged drift: %+v", d)
		}
	})

	t.Run("a free trailing window does not divide by zero", func(t *testing.T) {
		stats := weeklyFixture(end, 5, 10, 0, 0, 0, 0, 1.10)
		if d := CostDrift(stats); d != nil {
			t.Fatalf("zero-cost trailing window flagged drift (ratio would be +Inf): %+v", d)
		}
	})
}

func TestRenderWeeklyCostNamesEveryFigure(t *testing.T) {
	week := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	var samples []state.AssayRunSample
	for i := 0; i < 3; i++ {
		samples = append(samples, sample(week.Add(time.Duration(i)*time.Minute), state.AssayStatusComplete, 0.40, 88))
	}
	samples = append(samples, sample(week.Add(time.Hour), state.AssayStatusPartial, 1.00, 116))
	samples = append(samples, sample(week.Add(2*time.Hour), state.AssayStatusFailed, 0.05, 12))

	line := RenderWeeklyCost(WeeklyStatsFrom(samples, 0)[0])
	for _, want := range []string{
		"2026-W34:",
		"5 runs",
		"complete 3 runs $0.400/88s",
		"partial 1 run $1.000/116s",
		"failed 1 run $0.050/12s",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("rendered line %q is missing %q", line, want)
		}
	}
	if strings.Contains(line, "unknown") {
		t.Errorf("an empty unknown split should not be rendered: %q", line)
	}
}

func TestRenderWeeklyCostSubTenSecondMeansKeepADecimal(t *testing.T) {
	week := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	line := RenderWeeklyCost(WeeklyStatsFrom([]state.AssayRunSample{
		sample(week, state.AssayStatusComplete, 0.01, 1.4),
	}, 0)[0])
	if !strings.Contains(line, "1.4s") {
		t.Errorf("a sub-ten-second mean must not round to a whole second: %q", line)
	}
}

func TestDriftTextOnNilIsEmpty(t *testing.T) {
	var d *Drift
	if d.Text() != "" {
		t.Errorf("nil drift should render empty, got %q", d.Text())
	}
}
