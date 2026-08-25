package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/state"
	"github.com/spf13/cobra"
)

// defaultAssayStatsWeeks is the current (incomplete) ISO week plus the
// completed weeks the drift check compares it against, so the flag it may print
// is explained by the lines above it.
const defaultAssayStatsWeeks = assay.DriftTrailingWeeks + 1

// maxAssayStatsWeeks bounds --weeks. Two years of history is far past the point
// where a week-over-week comparison says anything, and the query it builds is
// unbounded in rows.
const maxAssayStatsWeeks = 104

var assayStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show Assay run cost and duration by ISO week",
	Long: `Aggregate the recorded Assay runs into ISO weeks and print the mean cost and
mean duration of a run, split by coverage outcome (complete vs partial).

This is the on-demand form of the weekly summary the daemon writes to its log
once a day. Both read the same assay_runs ledger and fold it the same way, so a
step change in what a review costs shows up in the week it happens rather than
in a month-end total.

Runs that never reviewed a diff (skipped by the trigger gate, a failed diff
fetch) are excluded. Runs that failed after spending are included: a failure is
not a refund, and spend moving into failed runs is exactly what this is for.`,
	Example: "  forge assay stats\n  forge assay stats --weeks 12 --json",
	RunE: func(cmd *cobra.Command, args []string) error {
		weeks, _ := cmd.Flags().GetInt("weeks")
		asJSON, _ := cmd.Flags().GetBool("json")
		if weeks < 1 || weeks > maxAssayStatsWeeks {
			return fmt.Errorf("invalid --weeks %d: expected 1..%d", weeks, maxAssayStatsWeeks)
		}

		db, err := state.Open("")
		if err != nil {
			return fmt.Errorf("opening state database: %w", err)
		}
		defer db.Close()

		now := time.Now()
		cutoff := assay.ISOWeekStart(now).AddDate(0, 0, -7*(weeks-1))
		samples, err := db.AssayRunSamplesSince(cutoff)
		if err != nil {
			return fmt.Errorf("reading assay runs: %w", err)
		}
		stats := assay.WeeklyStatsFrom(samples, weeks)
		drift := assay.CostDrift(stats, now)

		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(assayStatsJSON(stats, drift))
		}
		printAssayStats(stats, drift, weeks)
		return nil
	},
}

func printAssayStats(stats []assay.WeeklyStats, drift *assay.Drift, weeks int) {
	if len(stats) == 0 {
		fmt.Printf("No Assay runs recorded in the last %d weeks.\n", weeks)
		return
	}
	total := 0
	for _, w := range stats {
		total += w.All.Runs
	}
	fmt.Printf("Assay runs by ISO week (last %d weeks, %d runs):\n\n", weeks, total)
	for _, w := range stats {
		fmt.Printf("  %s\n", assay.RenderWeeklyCost(w))
	}
	if drift != nil {
		fmt.Printf("\nWARNING cost drift: %s\n", drift.Text())
	}
}

// assayStatsWeekJSON is the machine-readable form: the sums the fold
// accumulated alongside the means derived from them, so a consumer can
// re-aggregate across weeks without re-weighting the means by hand.
type assayStatsWeekJSON struct {
	Week     string                `json:"week"`
	All      assayStatsOutcomeJSON `json:"all"`
	Complete assayStatsOutcomeJSON `json:"complete"`
	Partial  assayStatsOutcomeJSON `json:"partial"`
	Failed   assayStatsOutcomeJSON `json:"failed"`
	Unknown  assayStatsOutcomeJSON `json:"unknown"`
}

type assayStatsOutcomeJSON struct {
	Runs           int     `json:"runs"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	MeanCostUSD    float64 `json:"mean_cost_usd"`
	TotalDurationS float64 `json:"total_duration_seconds"`
	MeanDurationS  float64 `json:"mean_duration_seconds"`
}

type assayStatsPayload struct {
	Weeks []assayStatsWeekJSON `json:"weeks"`
	Drift *assay.Drift         `json:"drift,omitempty"`
}

func assayStatsJSON(stats []assay.WeeklyStats, drift *assay.Drift) assayStatsPayload {
	out := assayStatsPayload{Weeks: make([]assayStatsWeekJSON, 0, len(stats)), Drift: drift}
	for _, w := range stats {
		out.Weeks = append(out.Weeks, assayStatsWeekJSON{
			Week:     w.Label(),
			All:      assayStatsOutcome(w.All),
			Complete: assayStatsOutcome(w.Complete),
			Partial:  assayStatsOutcome(w.Partial),
			Failed:   assayStatsOutcome(w.Failed),
			Unknown:  assayStatsOutcome(w.Unknown),
		})
	}
	return out
}

func assayStatsOutcome(o assay.OutcomeStats) assayStatsOutcomeJSON {
	return assayStatsOutcomeJSON{
		Runs:           o.Runs,
		TotalCostUSD:   o.TotalCostUSD,
		MeanCostUSD:    o.MeanCostUSD(),
		TotalDurationS: o.TotalDuration.Seconds(),
		MeanDurationS:  o.MeanDuration().Seconds(),
	}
}
