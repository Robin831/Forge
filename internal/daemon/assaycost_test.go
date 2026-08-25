package daemon

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/state"
)

// stubSampler answers AssayRunSamplesSince from a fixed slice and records the
// cutoff it was asked for.
type stubSampler struct {
	cutoff  time.Time
	samples []state.AssayRunSample
	err     error
}

func (s *stubSampler) AssayRunSamplesSince(since time.Time) ([]state.AssayRunSample, error) {
	s.cutoff = since
	return s.samples, s.err
}

func TestAssayWeeklyStatsAlignsTheCutoffToAWeekBoundary(t *testing.T) {
	// A Thursday: a cutoff taken as "N*7 days ago" would land on a Thursday
	// too, making the oldest bucket four days of a week reported as a week.
	now := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	s := &stubSampler{}
	if _, err := assayWeeklyStats(s, now, 5); err != nil {
		t.Fatalf("assayWeeklyStats: %v", err)
	}
	want := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC) // Monday of 2026-W30
	if !s.cutoff.Equal(want) {
		t.Errorf("cutoff = %s, want %s", s.cutoff, want)
	}
}

func TestAssayWeeklyStatsFoldsSamples(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	s := &stubSampler{samples: []state.AssayRunSample{
		{CompletedAt: now.AddDate(0, 0, -7), CostUSD: 0.5, DurationMs: 60000, Status: state.AssayStatusComplete},
		{CompletedAt: now, CostUSD: 1.5, DurationMs: 90000, Status: state.AssayStatusPartial},
	}}
	weeks, err := assayWeeklyStats(s, now, 5)
	require.NoError(t, err)
	require.Len(t, weeks, 2)
	require.Equal(t, "2026-W33", weeks[0].Label())
	require.Equal(t, "2026-W34", weeks[1].Label())
	require.Equal(t, 1, weeks[1].Partial.Runs)
}

// reportDaemon builds a Daemon whose log output is captured.
func reportDaemon(t *testing.T) (*Daemon, *state.DB, *bytes.Buffer) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	buf := &bytes.Buffer{}
	d := &Daemon{db: db, logger: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	return d, db, buf
}

func recordRun(t *testing.T, db *state.DB, at time.Time, status string, cost float64, durMs int64) {
	t.Helper()
	finished := at.Add(time.Duration(durMs) * time.Millisecond)
	require.NoError(t, db.RecordAssayRun(&state.AssayRun{
		Anvil: "forge", PRNumber: 1, StartedAt: at, FinishedAt: &finished,
		DurationMs: durMs, CostUSD: cost, Status: status,
	}))
}

func TestReportAssayCostEmitsOneLinePerWeekAndFlagsDrift(t *testing.T) {
	d, db, buf := reportDaemon(t)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC) // 2026-W34

	// Four trailing weeks at $0.50/run, then a 2.2x step change this week.
	for w := 4; w >= 1; w-- {
		at := now.AddDate(0, 0, -7*w)
		for i := 0; i < 5; i++ {
			recordRun(t, db, at.Add(time.Duration(i)*time.Minute), state.AssayStatusComplete, 0.50, 90000)
		}
	}
	for i := 0; i < 4; i++ {
		recordRun(t, db, now.Add(time.Duration(i)*time.Minute), state.AssayStatusComplete, 1.00, 120000)
	}
	// A partial run that costs more than a complete one is the shape this
	// report exists to expose, so it must land in its own split.
	recordRun(t, db, now.Add(time.Hour), state.AssayStatusPartial, 1.50, 200000)

	d.reportAssayCost(now)
	out := buf.String()

	weekLines := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `msg="Assay weekly cost"`) {
			weekLines++
		}
	}
	require.Equal(t, 5, weekLines, "expected one summary line per week in the window:\n%s", out)
	require.Contains(t, out, "2026-W30", "the oldest week in the window is missing")
	require.Contains(t, out, "2026-W34", "the current week is missing")
	require.Contains(t, out, "complete 4 runs", "the complete split is missing")
	require.Contains(t, out, "partial 1 run", "the partial split is missing")
	require.Contains(t, out, "Assay weekly cost drift", "a 2.2x step change must be flagged")
	require.Contains(t, out, "level=WARN")
}

func TestReportAssayCostDoesNotFlagAWeekThatIsNoLongerCurrent(t *testing.T) {
	d, db, buf := reportDaemon(t)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC) // 2026-W34, no runs

	// The step change happened LAST week and was already reported then. This
	// week is quiet, so it has no bucket at all — and without a guard the
	// newest bucket (last week) would be re-read as "current" and re-flagged
	// every day for as long as the ledger stays silent.
	for w := 4; w >= 2; w-- {
		at := now.AddDate(0, 0, -7*w)
		for i := 0; i < 5; i++ {
			recordRun(t, db, at.Add(time.Duration(i)*time.Minute), state.AssayStatusComplete, 0.50, 90000)
		}
	}
	last := now.AddDate(0, 0, -7)
	for i := 0; i < 5; i++ {
		recordRun(t, db, last.Add(time.Duration(i)*time.Minute), state.AssayStatusComplete, 1.10, 120000)
	}

	d.reportAssayCost(now)
	out := buf.String()
	require.Contains(t, out, "Assay weekly cost", "the weekly lines are still the report")
	require.NotContains(t, out, "drift", "a completed week must not be re-flagged as the current one:\n%s", out)
}

func TestReportAssayCostDoesNotFlagASingleRunWeek(t *testing.T) {
	d, db, buf := reportDaemon(t)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	for w := 4; w >= 1; w-- {
		at := now.AddDate(0, 0, -7*w)
		for i := 0; i < 5; i++ {
			recordRun(t, db, at.Add(time.Duration(i)*time.Minute), state.AssayStatusComplete, 0.50, 90000)
		}
	}
	// One expensive run into a fresh week: a 10x ratio over a single sample,
	// which is one large PR rather than a step change in what a review costs.
	recordRun(t, db, now, state.AssayStatusComplete, 5.00, 300000)

	d.reportAssayCost(now)
	out := buf.String()
	require.Contains(t, out, "2026-W34", "the current week is still reported")
	require.NotContains(t, out, "drift", "one run must not set off the drift WARN:\n%s", out)
}

func TestReportAssayCostIsSilentWithNoRuns(t *testing.T) {
	d, _, buf := reportDaemon(t)
	d.reportAssayCost(time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC))
	require.Empty(t, buf.String(), "a ledger with no runs must not emit a daily zero")
}

func TestReportAssayCostDoesNotFlagAFlatWeek(t *testing.T) {
	d, db, buf := reportDaemon(t)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	for w := 4; w >= 0; w-- {
		at := now.AddDate(0, 0, -7*w)
		for i := 0; i < 5; i++ {
			recordRun(t, db, at.Add(time.Duration(i)*time.Minute), state.AssayStatusComplete, 0.50, 90000)
		}
	}
	d.reportAssayCost(now)
	out := buf.String()
	require.Contains(t, out, "Assay weekly cost")
	require.NotContains(t, out, "drift", "a flat series must not be flagged:\n%s", out)
}
