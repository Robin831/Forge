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
	costFallbackModel   string
	costByModel         bool
	costByPass          bool
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
	costAssayCmd.Flags().StringVar(&costFallbackModel, "fallback-model", "", "Model to price a pass at when neither the pass nor its run recorded one (id or alias; default: "+cost.DefaultFallbackModel+")")
	// --model-tier once chose the one pricing row every token was priced at.
	// Pricing is per pass now, so the only thing a tier can still mean is the
	// fallback for rows that name no model; it is kept as that so existing
	// before/after scripts keep running.
	costAssayCmd.Flags().StringVar(&costFallbackModel, "model-tier", "", "Deprecated alias for --fallback-model")
	_ = costAssayCmd.Flags().MarkDeprecated("model-tier", "pricing is per pass at the model that ran it; use --fallback-model for rows that record no model")
	costAssayCmd.Flags().BoolVar(&costByModel, "by-model", false, "Break priced spend down by the model each pass ran on")
	costAssayCmd.Flags().BoolVar(&costByPass, "by-pass", false, "List every priced pass with its model, where the model came from, tokens and cost")
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

Tokens are priced pass by pass at the rates of the model that pass ran on
(Opus 5, Sonnet 5, Haiku 4.5, … — settings.pricing overrides apply), because a
run is not one model: every pass resolves its own provider chain. A pass that
recorded no model takes its run's model, and a row naming neither takes
--fallback-model. Rows written before per-pass tokens were recorded are priced
whole from their run-level cache tokens. --by-model breaks the priced spend
down per model (the rows sum to the total) and --by-pass lists every pass.

Run ordinals are derived over each PR's full review history and only then
restricted to the window, so a PR first reviewed before the window opens does
not have its second review counted as a first.

Recorded spend (the provider's own cost_usd) and priced attribution are
reported separately and never summed: priced figures cover only the tokens a
row records (older rows carry cache tokens alone), so they are a subset of the
recorded total. Runs
predating cache instrumentation report token class 'unknown' rather than a
misleading zero.

Examples:
  forge cost assay
  forge cost assay --since 2026-06-01 --until 2026-07-01
  forge cost assay --format json --out repeat-cost-before.json
  forge cost assay --by-model
  forge cost assay --since 2026-09-01 --by-pass
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
		FallbackModel:  costFallbackModel,
		ByModel:        costByModel,
		ByPass:         costByPass,
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
			Model:               runLevelModel(r.PassFindings),
			Passes:              projectPassRecords(r.PassFindings),
		})
	}
	return out, nil
}

// projectPassRecords carries the per-pass attribution the pricing reads: the
// model each pass ran on and what it was billed for.
func projectPassRecords(passes []state.AssayPassFindings) []cost.PassRecord {
	if len(passes) == 0 {
		return nil
	}
	out := make([]cost.PassRecord, 0, len(passes))
	for _, p := range passes {
		out = append(out, cost.PassRecord{
			Name:                p.Name,
			Provider:            p.Provider,
			Model:               p.Model,
			CostUSD:             p.CostUSD,
			InputTokens:         p.InputTokens,
			OutputTokens:        p.OutputTokens,
			CacheCreationTokens: p.CacheCreationTokens,
			CacheReadTokens:     p.CacheReadTokens,
		})
	}
	return out
}

// runLevelModel is the run's one model where its pass rows establish it: every
// pass that names a model names the same one. assay_runs has no run-level model
// column, so a run whose passes disagree — or that names none — has no run
// model, and its unnamed rows fall through to the report's fallback instead of
// being priced at whichever pass happened to come first.
func runLevelModel(passes []state.AssayPassFindings) string {
	model := ""
	for _, p := range passes {
		if p.Model == "" {
			continue
		}
		if model != "" && p.Model != model {
			return ""
		}
		model = p.Model
	}
	return model
}
