package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/state"
	"github.com/spf13/cobra"
)

var (
	zeroFindSince       string
	zeroFindUntil       string
	zeroFindFormat      string
	zeroFindOut         string
	zeroFindAnvil       string
	zeroFindSkipped     bool
	zeroFindExpectRuns  int
	zeroFindExpectCost  float64
	zeroFindShowDetails bool
)

func init() {
	costZeroFindingsCmd.Flags().StringVar(&zeroFindSince, "since", "", "Start of the window, inclusive (YYYY-MM-DD or RFC3339; default: no lower bound)")
	costZeroFindingsCmd.Flags().StringVar(&zeroFindUntil, "until", "", "End of the window, exclusive (YYYY-MM-DD or RFC3339; default: no upper bound)")
	costZeroFindingsCmd.Flags().StringVar(&zeroFindFormat, "format", "table", "Output format: table, json or csv")
	costZeroFindingsCmd.Flags().StringVar(&zeroFindOut, "out", "", "Write the report to this file instead of stdout")
	costZeroFindingsCmd.Flags().StringVar(&zeroFindAnvil, "anvil", "", "Restrict the report to one anvil")
	costZeroFindingsCmd.Flags().BoolVar(&zeroFindSkipped, "include-skipped", false, "Count runs that dispatched no passes (default: excluded, matching the per-PR run cap)")
	costZeroFindingsCmd.Flags().IntVar(&zeroFindExpectRuns, "expect-runs", 0, "Reconcile the zero-finding run count against a published baseline figure")
	costZeroFindingsCmd.Flags().Float64Var(&zeroFindExpectCost, "expect-cost", 0, "Reconcile the zero-finding spend against a published baseline figure (USD)")
	costZeroFindingsCmd.Flags().BoolVar(&zeroFindShowDetails, "details", false, "Print the per-run detail behind each cell")

	costCmd.AddCommand(costZeroFindingsCmd)
}

var costZeroFindingsCmd = &cobra.Command{
	Use:   "zero-findings",
	Short: "Classify Assay runs that reported no findings by run ordinal and substance change",
	Long: `Isolate the Assay runs that reported ZERO findings and classify each one:

  first_or_no_prior        the PR's first review — finding nothing is the answer
  repeat_clean_unchanged   the nth review of the SAME head commit
  repeat_clean_changed     the nth review, but the head moved since the last one
  repeat_clean_unknown     a repeat whose head SHA (or its predecessor's) is absent

Only the second cell is spend a skip/short-circuit could ever recover; the rest
reviewed content nothing had reviewed before. The report prints that cell's
share of zero-finding spend and of all Assay spend in the window, which is the
number a decision to build such a heuristic should be taken against.

The substance axis is assay_runs.head_sha and nothing else — the table stores no
diff hash, changed-file set, diff size or base SHA, and this command infers none
of them from timestamps or cost. Run ordinals come from cost.DeriveRunOrdinals,
the same definition ` + "`forge cost assay`" + ` uses, derived over each PR's full review
history before the window filter is applied.

This command is read-only analysis. It changes no Assay behaviour and gates
nothing.

Examples:
  forge cost zero-findings
  forge cost zero-findings --since 2026-07-01 --details
  forge cost zero-findings --format json --out zero-findings.json
  forge cost zero-findings --expect-runs 208 --expect-cost 429.95`,
	RunE: runCostZeroFindings,
}

func runCostZeroFindings(cmd *cobra.Command, args []string) error {
	since, err := parseCostBound(zeroFindSince, "since")
	if err != nil {
		return err
	}
	until, err := parseCostBound(zeroFindUntil, "until")
	if err != nil {
		return err
	}
	if !since.IsZero() && !until.IsZero() && !since.Before(until) {
		return fmt.Errorf("--since (%s) must be before --until (%s)", zeroFindSince, zeroFindUntil)
	}

	format := strings.ToLower(strings.TrimSpace(zeroFindFormat))
	if jsonOutput && !cmd.Flags().Changed("format") {
		format = "json"
	}
	switch format {
	case "table", "json", "csv":
	default:
		return fmt.Errorf("unknown --format %q (want table, json or csv)", zeroFindFormat)
	}

	db, err := state.Open("")
	if err != nil {
		return fmt.Errorf("opening state database: %w", err)
	}
	defer db.Close()

	report, err := cost.ReportZeroFindings(assayRunSource{db: db}, since, until, cost.Options{
		IncludeSkipped: zeroFindSkipped,
		Anvil:          zeroFindAnvil,
	})
	if err != nil {
		return err
	}

	out := os.Stdout
	if zeroFindOut != "" {
		f, err := os.Create(zeroFindOut)
		if err != nil {
			return fmt.Errorf("creating %s: %w", zeroFindOut, err)
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
		if err = report.WriteTable(out); err == nil && zeroFindShowDetails {
			err = writeZeroFindingDetails(out, report)
		}
	}
	if err != nil {
		return fmt.Errorf("writing report: %w", err)
	}
	if zeroFindOut != "" {
		fmt.Printf("Wrote %s report to %s\n", format, zeroFindOut)
	}

	// Printed to stdout even when the report went to a file, for the same
	// reason `forge cost assay` does it: an operator who redirected the report
	// is exactly the one asking whether the published figure reproduces.
	if zeroFindExpectRuns > 0 || zeroFindExpectCost > 0 {
		check := cost.ValidateZeroFindingBaseline(report, cost.ZeroFindingBaseline{
			Runs:    zeroFindExpectRuns,
			CostUSD: zeroFindExpectCost,
		})
		fmt.Printf("\nBaseline reconciliation\n")
		fmt.Printf("  expected  %d zero-finding run(s), $%.2f\n", check.Expected.Runs, check.Expected.CostUSD)
		fmt.Printf("  actual    %d zero-finding run(s), $%.2f\n", check.ActualRuns, check.ActualCost)
		fmt.Printf("  delta     %+d run(s), %+.2f USD\n", check.RunDelta, check.CostDeltaUSD)
		if check.Matches {
			fmt.Printf("  result    reproduced\n")
		} else {
			// Not an error, for the same reason the attribution report's
			// reconciliation is not: a mismatch can mean the methodology
			// drifted or that the published figure came from a different
			// dataset, and only the operator knows which.
			fmt.Printf("  result    NOT reproduced — see docs/assay-zero-finding-analysis.md\n")
		}
	}
	return nil
}

// writeZeroFindingDetails prints the rows behind the cells, so a cell count can
// be checked against the runs that produced it rather than taken on trust.
func writeZeroFindingDetails(out *os.File, report *cost.ZeroFindingReport) error {
	if len(report.Runs) == 0 {
		_, err := fmt.Fprintf(out, "\nNo zero-finding runs in this window.\n")
		return err
	}
	if _, err := fmt.Fprintf(out, "\nPer-run detail\n"); err != nil {
		return err
	}
	for _, d := range report.Runs {
		prev := "none"
		if d.PrevRunID != 0 {
			prev = fmt.Sprintf("run %d (%s, %d finding(s))", d.PrevRunID, shortSHA(d.PrevHeadSHA), d.PrevFindingsCount)
		}
		note := ""
		if d.NoCoverage {
			note = "  [no coverage — no pass ran]"
		}
		if _, err := fmt.Fprintf(out, "  %s#%d run %d  ordinal %d  $%.2f  head %s  prev %s  -> %s (%s)%s\n",
			d.Anvil, d.PRNumber, d.RunID, d.Ordinal, d.CostUSD, shortSHA(d.HeadSHA), prev, d.Cell, d.Substance, note); err != nil {
			return err
		}
	}
	return nil
}

// shortSHA abbreviates a commit for the detail listing, naming an absent one
// rather than printing an empty column that reads as a rendering bug.
func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return "(none)"
	}
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
