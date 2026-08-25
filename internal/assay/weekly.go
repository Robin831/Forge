package assay

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/textfmt"
)

// Weekly Assay spend reporting.
//
// A per-run cost is only legible against the runs around it: a review that
// costs $2.80 is unremarkable on its own and alarming if last month's mean was
// $1.20. The per-run telemetry Assay already emits (RenderPassTelemetry, the
// "Assay review completed" line, the assay_runs row) answers "what did THIS run
// cost"; nothing answered "what does a run cost lately", which is why a 2.2x
// step change in the mean sat unnoticed for weeks — every individual line
// looked plausible.
//
// This is the aggregate side of that: the persisted ledger folded into ISO
// weeks, split by coverage outcome, plus a drift flag comparing the current
// week against the weeks behind it. The outcome split is not decoration — the
// step change was a partial run costing MORE than a complete one (a failing
// pass re-running its whole session), which is invisible in a single blended
// mean and obvious the moment the two are printed side by side.

// runOutcome is the coverage outcome a run is bucketed under. The first three
// mirror the persisted state.AssayStatus* values; outcomeUnknown catches rows
// written before coverage was recorded, which are real spend and so are counted
// rather than dropped. It stays unexported because WeeklyStats exposes one
// field per bucket: nothing outside needs to name the axis.
type runOutcome string

const (
	outcomeComplete runOutcome = "complete"
	outcomePartial  runOutcome = "partial"
	outcomeFailed   runOutcome = "failed"
	outcomeUnknown  runOutcome = "unknown"
)

// outcomeOf maps a persisted assay_runs.status onto a bucket.
func outcomeOf(status string) runOutcome {
	switch status {
	case state.AssayStatusComplete:
		return outcomeComplete
	case state.AssayStatusPartial:
		return outcomePartial
	case state.AssayStatusFailed:
		return outcomeFailed
	default:
		return outcomeUnknown
	}
}

// OutcomeStats is the accumulator for one bucket. It holds sums rather than
// means so a bucket can be folded into a wider one without a weighted average:
// WeeklyStats.All is the fold of its own outcome buckets, which is what keeps
// the headline mean and the split from disagreeing.
type OutcomeStats struct {
	Runs          int
	TotalCostUSD  float64
	TotalDuration time.Duration
}

func (o OutcomeStats) add(cost float64, d time.Duration) OutcomeStats {
	o.Runs++
	o.TotalCostUSD += cost
	o.TotalDuration += d
	return o
}

func (o OutcomeStats) merge(other OutcomeStats) OutcomeStats {
	o.Runs += other.Runs
	o.TotalCostUSD += other.TotalCostUSD
	o.TotalDuration += other.TotalDuration
	return o
}

// MeanCostUSD is the mean spend per run, 0 for an empty bucket. An empty bucket
// is a real answer here ("no partial runs this week"), never a division to
// guard at the call site.
func (o OutcomeStats) MeanCostUSD() float64 {
	if o.Runs == 0 {
		return 0
	}
	return o.TotalCostUSD / float64(o.Runs)
}

// MeanDuration is the mean wall-clock duration per run, 0 for an empty bucket.
func (o OutcomeStats) MeanDuration() time.Duration {
	if o.Runs == 0 {
		return 0
	}
	return o.TotalDuration / time.Duration(o.Runs)
}

// WeeklyStats is one ISO week of Assay runs.
type WeeklyStats struct {
	// Year and Week are the ISO-8601 calendar coordinates the bucket is keyed
	// and ordered by. They are NOT the calendar year and month: the last days
	// of December belong to week 1 of the following ISO year, and bucketing on
	// anything else would split one week of spend across two rows.
	Year int
	Week int
	// All is the fold of every outcome bucket below.
	All      OutcomeStats
	Complete OutcomeStats
	Partial  OutcomeStats
	Failed   OutcomeStats
	Unknown  OutcomeStats
}

// Label renders the ISO week as "2026-W34".
func (w WeeklyStats) Label() string {
	return fmt.Sprintf("%04d-W%02d", w.Year, w.Week)
}

// WeeklyStatsFrom folds run samples into ISO weeks, oldest first, keeping at
// most the newest maxWeeks buckets (<= 0 keeps all of them).
//
// Only weeks that actually contain runs become buckets: a silent week is an
// absence of data, and inventing a zero-run row for it would put a 0.0 mean
// into the trailing window the drift check averages, which reads as a spend
// collapse rather than as a quiet week.
func WeeklyStatsFrom(samples []state.AssayRunSample, maxWeeks int) []WeeklyStats {
	type key struct{ year, week int }
	buckets := make(map[key]*WeeklyStats)

	for _, s := range samples {
		if s.CompletedAt.IsZero() {
			// A run with no usable timestamp cannot be placed in a week, and
			// placing it in the current one would attribute old spend to now.
			continue
		}
		y, w := s.CompletedAt.UTC().ISOWeek()
		k := key{y, w}
		b := buckets[k]
		if b == nil {
			b = &WeeklyStats{Year: y, Week: w}
			buckets[k] = b
		}
		cost := s.CostUSD
		dur := time.Duration(s.DurationMs) * time.Millisecond
		switch outcomeOf(s.Status) {
		case outcomeComplete:
			b.Complete = b.Complete.add(cost, dur)
		case outcomePartial:
			b.Partial = b.Partial.add(cost, dur)
		case outcomeFailed:
			b.Failed = b.Failed.add(cost, dur)
		default:
			b.Unknown = b.Unknown.add(cost, dur)
		}
	}

	out := make([]WeeklyStats, 0, len(buckets))
	for _, b := range buckets {
		// Derived, never accumulated in parallel: the headline mean is by
		// construction the count-weighted blend of the splits printed beside
		// it, so the two cannot drift apart.
		b.All = b.Complete.merge(b.Partial).merge(b.Failed).merge(b.Unknown)
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Year != out[j].Year {
			return out[i].Year < out[j].Year
		}
		return out[i].Week < out[j].Week
	})
	if maxWeeks > 0 && len(out) > maxWeeks {
		out = out[len(out)-maxWeeks:]
	}
	return out
}

// DriftThreshold is the ratio at which the current week's mean cost per run is
// reported as drift. 1.5x is well above ordinary week-to-week variation in a
// mean over dozens of runs, and well below the 2.2x step change this signal was
// built to catch.
const DriftThreshold = 1.5

// DriftTrailingWeeks is how many completed weeks the current one is compared
// against, and MinDriftTrailingWeeks is how few of them make the comparison
// meaningless. A single trailing week is one sample: on a cold ledger — a fresh
// install, an anvil that just enabled Assay — it would flag the second week of
// normal operation.
const (
	DriftTrailingWeeks    = 4
	MinDriftTrailingWeeks = 2
)

// CostDrift reports the current week's mean cost per run against the trailing
// weeks', or nil when there is nothing to report: too little history, no runs
// in the current week, a trailing window that spent nothing, or a ratio under
// the threshold.
//
// The trailing figure is pooled (total trailing cost / total trailing runs)
// rather than the mean of the weekly means, so both sides of the ratio are the
// same quantity — cost per run — and a quiet trailing week with three runs
// cannot outweigh a busy one with three hundred.
func CostDrift(weeks []WeeklyStats) *Drift {
	if len(weeks) < MinDriftTrailingWeeks+1 {
		return nil
	}
	current := weeks[len(weeks)-1]
	if current.All.Runs == 0 {
		return nil
	}

	trailing := weeks[:len(weeks)-1]
	if len(trailing) > DriftTrailingWeeks {
		trailing = trailing[len(trailing)-DriftTrailingWeeks:]
	}
	var (
		pooled        OutcomeStats
		weeksWithRuns int
	)
	for _, w := range trailing {
		if w.All.Runs == 0 {
			continue
		}
		weeksWithRuns++
		pooled = pooled.merge(w.All)
	}
	if weeksWithRuns < MinDriftTrailingWeeks || pooled.MeanCostUSD() <= 0 {
		return nil
	}

	ratio := current.All.MeanCostUSD() / pooled.MeanCostUSD()
	if ratio <= DriftThreshold {
		return nil
	}
	return &Drift{
		Week:                current.Label(),
		Runs:                current.All.Runs,
		MeanCostUSD:         current.All.MeanCostUSD(),
		TrailingWeeks:       weeksWithRuns,
		TrailingRuns:        pooled.Runs,
		TrailingMeanCostUSD: pooled.MeanCostUSD(),
		Ratio:               ratio,
	}
}

// Drift is a flagged step change in the mean cost per Assay run.
type Drift struct {
	Week                string  `json:"week"`
	Runs                int     `json:"runs"`
	MeanCostUSD         float64 `json:"mean_cost_usd"`
	TrailingWeeks       int     `json:"trailing_weeks"`
	TrailingRuns        int     `json:"trailing_runs"`
	TrailingMeanCostUSD float64 `json:"trailing_mean_cost_usd"`
	Ratio               float64 `json:"ratio"`
}

// Text renders the drift for the daemon log and the CLI, e.g.
//
//	2026-W34 mean $0.412/run over 128 runs is 2.24x the trailing 4-week mean $0.184/run (512 runs)
func (d *Drift) Text() string {
	if d == nil {
		return ""
	}
	return fmt.Sprintf("%s mean %s/run over %s is %.2fx the trailing %s mean %s/run (%s)",
		d.Week,
		money(d.MeanCostUSD),
		textfmt.Count(d.Runs, "run"),
		d.Ratio,
		textfmt.Count(d.TrailingWeeks, "week"),
		money(d.TrailingMeanCostUSD),
		textfmt.Count(d.TrailingRuns, "run"),
	)
}

// RenderWeeklyCost renders one week as a single line, e.g.
//
//	2026-W34: 128 runs, mean $0.412, mean 94s | complete 101 runs $0.395/88s | partial 27 runs $0.475/116s
//
// complete and partial are always rendered, a zero count included: "no partial
// runs this week" is the answer the split exists to give, and a section that
// disappears when it is zero reads as a section that was forgotten. failed and
// unknown are appended only when non-empty — a healthy week has neither, and
// two permanent zeroes at the end of every line train the eye to stop reading
// before the numbers that matter.
func RenderWeeklyCost(w WeeklyStats) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s, mean %s, mean %s",
		w.Label(), textfmt.Count(w.All.Runs, "run"), money(w.All.MeanCostUSD()), dur(w.All.MeanDuration()))
	b.WriteString(" | " + renderSplit("complete", w.Complete))
	b.WriteString(" | " + renderSplit("partial", w.Partial))
	if w.Failed.Runs > 0 {
		b.WriteString(" | " + renderSplit("failed", w.Failed))
	}
	if w.Unknown.Runs > 0 {
		b.WriteString(" | " + renderSplit("unknown", w.Unknown))
	}
	return b.String()
}

func renderSplit(name string, o OutcomeStats) string {
	return fmt.Sprintf("%s %s %s/%s", name, textfmt.Count(o.Runs, "run"), money(o.MeanCostUSD()), dur(o.MeanDuration()))
}

// money renders a USD figure to the milli-dollar. Per-run Assay costs sit in
// the tens of cents, where the cent the rest of Forge rounds to is a 2% error.
func money(v float64) string {
	return fmt.Sprintf("$%.3f", v)
}

// dur renders a mean duration in seconds — the unit Assay runs are measured in
// — with a decimal below ten seconds, where whole seconds would round a real
// difference to nothing.
func dur(d time.Duration) string {
	secs := d.Seconds()
	if secs < 10 {
		return fmt.Sprintf("%.1fs", secs)
	}
	return fmt.Sprintf("%.0fs", secs)
}

// ISOWeekStart returns midnight UTC on the Monday of the ISO week the given
// time falls in. It is what a caller bounds its query with: a cutoff that is
// simply "N*7 days ago" lands mid-week and makes the oldest bucket a fraction
// of a week, which is fine for a mean and misleading for anything read as a
// week's worth of activity.
func ISOWeekStart(t time.Time) time.Time {
	u := t.UTC()
	// Go's Weekday runs Sunday=0; the ISO week starts on Monday.
	offset := (int(u.Weekday()) + 6) % 7
	day := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	return day.AddDate(0, 0, -offset)
}
