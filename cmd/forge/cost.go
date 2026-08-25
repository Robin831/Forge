package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/state"
	"github.com/spf13/cobra"
)

var (
	costSince           string
	costUntil           string
	costFormat          string
	costOut             string
	costAnvil           string
	costIncludeSkipped  bool
	costModelTier       string
	costExpectCost      float64
	costExpectRepeatRun int
)

func init() {
	costAssayCmd.Flags().StringVar(&costSince, "since", "", "Start of the window, inclusive (YYYY-MM-DD or RFC3339; default: no lower bound)")
	costAssayCmd.Flags().StringVar(&costUntil, "until", "", "End of the window, exclusive (YYYY-MM-DD or RFC3339; default: no upper bound)")
	costAssayCmd.Flags().StringVar(&costFormat, "format", "table", "Output format: table, json or csv")
	costAssayCmd.Flags().StringVar(&costOut, "out", "", "Write the report to this file instead of stdout")
	costAssayCmd.Flags().StringVar(&costAnvil, "anvil", "", "Restrict the report to one anvil")
	costAssayCmd.Flags().BoolVar(&costIncludeSkipped, "include-skipped", false, "Count runs that dispatched no passes (default: excluded, matching the per-PR run cap)")
	costAssayCmd.Flags().StringVar(&costModelTier, "model-tier", "", "Pricing row for token classes: haiku, sonnet, opus or fable (default: sonnet)")
	costAssayCmd.Flags().Float64Var(&costExpectCost, "expect-repeat-cost", 0, "Reconcile the repeat-run total against a published baseline figure (USD)")
	costAssayCmd.Flags().IntVar(&costExpectRepeatRun, "expect-repeat-runs", 0, "Reconcile the repeat-run count against a published baseline figure")

	costCmd.AddCommand(costAssayCmd)
	rootCmd.AddCommand(costCmd)
}

var costCmd = &cobra.Command{
	Use:     "cost",
	Short:   "Cost reporting and attribution",
	GroupID: "daemon",
	RunE:    runCostAssay, // Default: the assay attribution report
}

var costAssayCmd = &cobra.Command{
	Use:   "assay",
	Short: "Report Assay spend split by first-run vs repeat-run and by cache token class",
	Long: `Report what Assay spent over a window, split two ways:

  - first review of a PR vs every re-review of it (run ordinal 1 vs n>1)
  - cache-write vs cache-read tokens, each priced at its own rate

Run ordinals are derived over each PR's full review history and only then
restricted to the window, so a PR first reviewed before the window opens does
not have its second review counted as a first.

Recorded spend (the provider's own cost_usd) and priced cache attribution are
reported separately and never summed: assay_runs stores no plain input/output
token counts, so the cache classes are a subset of the recorded total. Runs
predating cache instrumentation report token class 'unknown' rather than a
misleading zero.

Examples:
  forge cost assay
  forge cost assay --since 2026-06-01 --until 2026-07-01
  forge cost assay --format json --out repeat-cost-before.json
  forge cost assay --expect-repeat-cost 2326.54 --expect-repeat-runs 780`,
	RunE: runCostAssay,
}

func runCostAssay(cmd *cobra.Command, args []string) error {
	since, err := parseCostBound(costSince, "since")
	if err != nil {
		return err
	}
	until, err := parseCostBound(costUntil, "until")
	if err != nil {
		return err
	}
	if !since.IsZero() && !until.IsZero() && !since.Before(until) {
		return fmt.Errorf("--since (%s) must be before --until (%s)", costSince, costUntil)
	}

	format := strings.ToLower(strings.TrimSpace(costFormat))
	if jsonOutput && !cmd.Flags().Changed("format") {
		format = "json"
	}
	switch format {
	case "table", "json", "csv":
	default:
		return fmt.Errorf("unknown --format %q (want table, json or csv)", costFormat)
	}

	db, err := state.Open("")
	if err != nil {
		return fmt.Errorf("opening state database: %w", err)
	}
	defer db.Close()

	report, err := cost.ReportRepeatCost(assayRunSource{db: db}, since, until, cost.Options{
		IncludeSkipped: costIncludeSkipped,
		Anvil:          costAnvil,
		ModelTier:      costModelTier,
	})
	if err != nil {
		return err
	}

	out := os.Stdout
	if costOut != "" {
		f, err := os.Create(costOut)
		if err != nil {
			return fmt.Errorf("creating %s: %w", costOut, err)
		}
		defer f.Close()
		out = f
	}

	switch format {
	case "json":
		err = report.WriteJSON(out)
	case "csv":
		err = report.WriteCSV(out)
	default:
		err = report.WriteTable(out)
	}
	if err != nil {
		return fmt.Errorf("writing report: %w", err)
	}
	if costOut != "" {
		fmt.Printf("Wrote %s report to %s\n", format, costOut)
	}

	// The reconciliation is printed to stdout even when the report went to a
	// file: it is the answer to "does this still reproduce the baseline", and
	// an operator who redirected the report is exactly the one asking.
	if costExpectCost > 0 || costExpectRepeatRun > 0 {
		check := cost.ValidateBaseline(report, cost.BaselineExpectation{
			RepeatRuns:    costExpectRepeatRun,
			RepeatCostUSD: costExpectCost,
		})
		fmt.Printf("\nBaseline reconciliation\n")
		fmt.Printf("  expected  %d repeat run(s), $%.2f\n", check.Expected.RepeatRuns, check.Expected.RepeatCostUSD)
		fmt.Printf("  actual    %d repeat run(s), $%.2f\n", check.ActualRepeatRuns, check.ActualRepeatCostUSD)
		fmt.Printf("  delta     %+d run(s), %+.2f USD\n", check.RunDelta, check.CostDeltaUSD)
		if check.Matches {
			fmt.Printf("  result    reproduced\n")
		} else {
			// Not an error: a mismatch can mean the methodology drifted or
			// that the published figure came from a different database, and
			// only the operator knows which. Exiting non-zero here would
			// turn "these are different datasets" into a failed command.
			fmt.Printf("  result    NOT reproduced — see docs/assay-cost-attribution.md\n")
		}
	}
	return nil
}

// parseCostBound accepts a plain date (interpreted as UTC midnight, which is
// what an operator means by --since 2026-06-01) or a full RFC3339 timestamp.
// An empty value is an open bound, not an error.
func parseCostBound(value, flag string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse("2006-01-02", value); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("--%s: cannot parse %q (want YYYY-MM-DD or RFC3339)", flag, value)
}

// assayRunSource adapts the state database to cost.RunSource. The projection
// lives here rather than in either package so that cost stays free of a state
// import and state stays free of a cost one.
type assayRunSource struct{ db *state.DB }

func (s assayRunSource) AssayRunHistory(since, until time.Time) ([]cost.RunRecord, error) {
	rows, err := s.db.AssayRunHistoryForWindow(since, until)
	if err != nil {
		return nil, err
	}
	out := make([]cost.RunRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, cost.RunRecord{
			RunID:               r.ID,
			Anvil:               r.Anvil,
			PRNumber:            r.PRNumber,
			HeadSHA:             r.HeadSHA,
			StartedAt:           r.StartedAt,
			CostUSD:             r.CostUSD,
			FindingsCount:       r.FindingsCount,
			SkippedReason:       r.SkippedReason,
			ShadowMode:          r.ShadowMode,
			Status:              r.Status,
			Error:               r.Error,
			CacheCreationTokens: r.CacheCreationTokens,
			CacheReadTokens:     r.CacheReadTokens,
		})
	}
	return out, nil
}
