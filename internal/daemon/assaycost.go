package daemon

import (
	"context"
	"time"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/state"
)

// Weekly Assay spend report.
//
// Every other Assay surface is per-run: the "Assay review completed" line, the
// terminal feed event, the assay_runs row. A per-run cost only reads as wrong
// against the runs around it, so a step change in the mean — the kind a change
// to pass execution can introduce — was invisible until somebody totalled a
// month by hand. This is the aggregate the operator already has a place to
// look at: one report per day into the daemon log, the window's ISO weeks one
// line each, split by coverage outcome, plus a WARN when the current week's
// mean cost per run has stepped away from the weeks behind it.

const (
	// DefaultAssayCostReportInterval is how often the report is emitted. Daily
	// rather than hourly because the number it reports is a weekly mean: it
	// barely moves between two reports on the same day, and a signal repeated
	// often enough to become wallpaper is not a signal.
	DefaultAssayCostReportInterval = 24 * time.Hour

	// assayCostReportWeeks is the reported window: the current (incomplete)
	// week plus assay.DriftTrailingWeeks completed ones, which is exactly the
	// history the drift check compares against. Reporting a window the check
	// cannot see would leave the WARN unexplained by the lines beside it.
	assayCostReportWeeks = assay.DriftTrailingWeeks + 1

	// assayCostReportStartupDelay keeps the first report out of the daemon's
	// startup burst, where it competes with the first poll and the first
	// Bellows cycle for the operator's attention and the log's first page.
	assayCostReportStartupDelay = 90 * time.Second
)

// assayRunSampler is the slice of *state.DB the report needs, so the fold and
// the log lines can be exercised without a database.
type assayRunSampler interface {
	AssayRunSamplesSince(since time.Time) ([]state.AssayRunSample, error)
}

// runAssayCostReport is a blocking loop that emits the report on the given
// interval. Launch it as a goroutine; an interval <= 0 disables it.
func (d *Daemon) runAssayCostReport(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(assayCostReportStartupDelay):
	}
	d.reportAssayCost(time.Now())

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.reportAssayCost(time.Now())
		}
	}
}

// reportAssayCost emits one report. Errors are logged, never returned: a report
// is an observation of the ledger, and failing to read it must not disturb
// anything that writes to it.
func (d *Daemon) reportAssayCost(now time.Time) {
	if d.db == nil {
		return
	}
	weeks, err := assayWeeklyStats(d.db, now, assayCostReportWeeks)
	if err != nil {
		d.logger.Warn("Assay weekly cost report: could not read run ledger", "error", err)
		return
	}
	if len(weeks) == 0 {
		// No runs in the window. A silent report is right here: an anvil with
		// Assay off would otherwise emit a zero every day forever.
		return
	}

	// One line per week, oldest first, every cycle — so each report is a
	// self-contained snapshot of the trend rather than a single number whose
	// meaning depends on scrolling back through yesterday's log.
	for _, w := range weeks {
		d.logger.Info("Assay weekly cost", "summary", assay.RenderWeeklyCost(w))
	}

	if drift := assay.CostDrift(weeks, now); drift != nil {
		d.logger.Warn("Assay weekly cost drift",
			"detail", drift.Text(),
			"week", drift.Week,
			"mean_cost_usd", drift.MeanCostUSD,
			"trailing_mean_cost_usd", drift.TrailingMeanCostUSD,
			"ratio", drift.Ratio,
			"threshold", assay.DriftThreshold,
		)
	}
}

// assayWeeklyStats reads the last `weeks` ISO weeks of Assay runs and folds
// them. The cutoff is aligned to a week boundary rather than taken as N*7 days
// back, so the oldest bucket is a whole week like every other one.
func assayWeeklyStats(db assayRunSampler, now time.Time, weeks int) ([]assay.WeeklyStats, error) {
	if weeks < 1 {
		weeks = 1
	}
	cutoff := assay.ISOWeekStart(now).AddDate(0, 0, -7*(weeks-1))
	samples, err := db.AssayRunSamplesSince(cutoff)
	if err != nil {
		return nil, err
	}
	return assay.WeeklyStatsFrom(samples, weeks), nil
}
