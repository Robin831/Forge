package cost

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// Zero-finding run analysis.
//
// This file answers one question, and deliberately only asks it — it changes no
// behaviour and gates nothing. Of the Assay runs that reported NO findings, how
// many were the first review of a PR and how many were the nth clean look at a
// PR whose reviewable content had not moved since the review before it?
//
// The distinction is the whole point. A first review that finds nothing is a
// review that did its job: the only way to learn a diff is clean is to read it.
// An nth review of a head that has not changed since the last clean review of
// the same head is spend on a question already answered, and it is the only cell
// a skip/short-circuit heuristic could ever recover. Sizing that cell BEFORE
// building the heuristic is the point of this module: if it is small, the
// heuristic is not worth having, and saying so plainly is a result.
//
// # The substance axis is head SHA, and that is the only field there is
//
// An assay_runs row records the head commit it reviewed (RunRecord.HeadSHA) and
// nothing else about what it read: no diff hash, no changed-file list, no base
// SHA, no diff byte count. So "did the reviewable content change since the
// previous run of this PR" is answered here by comparing head SHAs, and the
// comparison is reported as a tri-state rather than a bool so a row that cannot
// answer it says so instead of defaulting into a cell:
//
//   - SubstanceUnchanged — the previous run of this PR reviewed the same head
//     commit. This is the confident direction, and it is also exactly what
//     Assay's own re-dispatch gate keys on (LastReviewedSHA), so a skip
//     heuristic built on it would be keyed the same way.
//   - SubstanceChanged — the previous run reviewed a different head commit.
//     This is a PROXY, not a proof: a push that only touched a lockfile moves
//     the head while leaving the reviewable diff identical after the
//     generated-file filter. It is the right default anyway, because it is the
//     direction that never over-claims recoverable spend.
//   - SubstanceUnknown — one of the two rows carries no head SHA. Not inferred
//     from timestamps, cost or anything else: an unrecorded field is reported
//     as a gap, and its spend is kept in every total so the money is never lost
//     to the ambiguity.
//
// One caveat the data cannot close, stated rather than papered over: a run's
// diff is taken against the PR's BASE, and the base is not recorded. Two runs
// of one head SHA whose base moved between them read different diffs while
// looking identical here. That direction over-states the unchanged cell, which
// is why the recommendation this module supports has to be read against the
// SIZE of that cell rather than against its exact membership.
//
// # Ordinals come from DeriveRunOrdinals, not from a second definition
//
// "The nth review of this PR" is defined once, in attribution.go, and derived
// over each PR's full history before the window filter is applied. Re-deriving
// it here would produce a second answer that can disagree with the report the
// finding is quoted alongside. The immediately-preceding run is taken from that
// same full-history ordering, so a zero-finding run whose predecessor sits just
// outside the window is still classified against its real predecessor rather
// than being promoted to "no prior".

// Substance-change verdicts. The tri-state exists because "not recorded" is a
// third answer and must not collapse into either of the other two.
const (
	SubstanceUnchanged = "unchanged"
	SubstanceChanged   = "changed"
	SubstanceUnknown   = "unknown"
)

// The cells a zero-finding run is classified into. Every zero-finding run lands
// in exactly one, and the four sum back to the zero-finding totals.
const (
	// CellFirstOrNoPrior is ordinal 1: the PR's first review. Under
	// full-history ordinals "ordinal 1" and "no prior run in the dataset" are
	// the same condition, which is why they are one cell and not two.
	CellFirstOrNoPrior = "first_or_no_prior"
	// CellRepeatCleanUnchanged is the cell a skip heuristic would target:
	// ordinal > 1, reviewing the same head commit as the run before it.
	CellRepeatCleanUnchanged = "repeat_clean_unchanged"
	// CellRepeatCleanChanged is ordinal > 1 over a head that moved. A review
	// of new content that happened to find nothing is not waste.
	CellRepeatCleanChanged = "repeat_clean_changed"
	// CellRepeatCleanUnknown is ordinal > 1 where a head SHA is missing, so
	// the substance question cannot be answered for that pair.
	CellRepeatCleanUnknown = "repeat_clean_unknown"
)

// SubstanceField names the column the substance axis is derived from, so a
// report says what it compared instead of leaving a reader to assume a diff
// hash existed.
const SubstanceField = "head_sha"

// zeroFindingCellSpec fixes the cell order for every renderer, so two runs of
// the report print their rows in the same sequence.
var zeroFindingCellSpec = []string{
	CellFirstOrNoPrior,
	CellRepeatCleanUnchanged,
	CellRepeatCleanChanged,
	CellRepeatCleanUnknown,
}

// CompareSubstance answers whether a run reviewed different content from the
// run that preceded it on the same PR, using the only field the row records.
//
// It is the Go form of the tri-state the analysis needs: a missing head SHA on
// either side is SubstanceUnknown, never a guess. Comparison is on the trimmed,
// lower-cased value, because a SHA written by two paths can differ in case
// without naming a different commit.
func CompareSubstance(prev, run RunRecord) string {
	a := strings.ToLower(strings.TrimSpace(prev.HeadSHA))
	b := strings.ToLower(strings.TrimSpace(run.HeadSHA))
	if a == "" || b == "" {
		return SubstanceUnknown
	}
	if a == b {
		return SubstanceUnchanged
	}
	return SubstanceChanged
}

// ZeroFindingRun is one zero-finding run with everything the classification
// rested on, so a cell count can be audited back to the rows behind it rather
// than taken on trust.
type ZeroFindingRun struct {
	RunID     int       `json:"run_id"`
	Anvil     string    `json:"anvil"`
	PRNumber  int       `json:"pr_number"`
	Ordinal   int       `json:"ordinal"`
	StartedAt time.Time `json:"started_at"`
	CostUSD   float64   `json:"cost_usd"`
	HeadSHA   string    `json:"head_sha,omitempty"`
	Cell      string    `json:"cell"`
	Substance string    `json:"substance"`
	// NoCoverage marks a run whose zero findings mean no pass ever ran (see
	// RunRecord.NoCoverage). It is an annotation, not a cell: such a run is
	// still classified by ordinal and substance and its spend still counts,
	// but it is not evidence that a review found nothing.
	NoCoverage bool `json:"no_coverage"`

	// PrevRunID is 0 when this is the PR's first review. The rest of the
	// Prev* fields are only meaningful when it is not.
	PrevRunID   int    `json:"prev_run_id,omitempty"`
	PrevHeadSHA string `json:"prev_head_sha,omitempty"`
	// PrevFindingsCount is what the preceding run reported. PrevClean is the
	// sub-column the plan calls for: a skip heuristic keys on "the last review
	// of this head was clean", not merely on "this is a repeat", so the two
	// have to be countable apart.
	PrevFindingsCount int  `json:"prev_findings_count"`
	PrevClean         bool `json:"prev_clean"`
	// PrevInWindow records whether the preceding run fell inside the reported
	// window. A false here is the evidence that the predecessor was read from
	// history rather than invented.
	PrevInWindow bool `json:"prev_in_window"`
}

// ZeroFindingCell is one cell of the classification: its runs, its spend, and
// the prev-clean sub-split within it.
type ZeroFindingCell struct {
	Label   string  `json:"label"`
	Runs    int     `json:"runs"`
	PRs     int     `json:"prs"`
	CostUSD float64 `json:"cost_usd"`

	// PrevClean* / PrevWithFindings* split the cell by what the PRECEDING run
	// reported. They are zero for CellFirstOrNoPrior, which has no
	// predecessor, and otherwise sum back to Runs / CostUSD.
	PrevCleanRuns           int     `json:"prev_clean_runs"`
	PrevCleanCostUSD        float64 `json:"prev_clean_cost_usd"`
	PrevWithFindingsRuns    int     `json:"prev_with_findings_runs"`
	PrevWithFindingsCostUSD float64 `json:"prev_with_findings_cost_usd"`

	// NoCoverage* is how much of this cell is runs that never reviewed
	// anything. Subtracting it gives the cell as a count of actual clean
	// reviews.
	NoCoverageRuns    int     `json:"no_coverage_runs"`
	NoCoverageCostUSD float64 `json:"no_coverage_cost_usd"`

	prs map[string]struct{}
}

// ZeroFindingReport is the whole answer: the zero-finding population, its
// classification into cells, and the one headline number the parent bead's
// decision turns on.
type ZeroFindingReport struct {
	Since       *time.Time `json:"since,omitempty"`
	Until       *time.Time `json:"until,omitempty"`
	GeneratedAt time.Time  `json:"generated_at"`
	Anvil       string     `json:"anvil,omitempty"`
	// IncludeSkipped echoes the option for the same reason CostReport does:
	// two reports over one window that disagree on it are not comparable and
	// the difference is otherwise invisible.
	IncludeSkipped bool `json:"include_skipped"`

	// SubstanceFieldUsed names the column the substance axis came from, and
	// SubstanceFieldAvailable is false when NO run in the population carries
	// it — the "record the gap explicitly rather than inferring" case, in
	// which the changed/unchanged split is unavailable and every repeat lands
	// in CellRepeatCleanUnknown.
	SubstanceFieldUsed      string `json:"substance_field_used"`
	SubstanceFieldAvailable bool   `json:"substance_field_available"`
	// RunsMissingHeadSHA counts zero-finding runs in the window whose own head
	// SHA is absent, so a large unknown cell can be told apart from a small one.
	RunsMissingHeadSHA int `json:"runs_missing_head_sha"`

	// Window context: every run, not just the zero-finding ones. The headline
	// share is quoted against both denominators because "38% of clean-run
	// spend" and "6% of all spend" are very different arguments for building
	// something.
	TotalRuns    int     `json:"total_runs"`
	TotalCostUSD float64 `json:"total_cost_usd"`

	ZeroFindingRuns    int     `json:"zero_finding_runs"`
	ZeroFindingPRs     int     `json:"zero_finding_prs"`
	ZeroFindingCostUSD float64 `json:"zero_finding_cost_usd"`

	Cells []ZeroFindingCell `json:"cells"`

	// The headline: the cell a skip/short-circuit could address, and its share
	// of each denominator as a fraction in [0,1].
	RecoverableRuns              int     `json:"recoverable_runs"`
	RecoverableCostUSD           float64 `json:"recoverable_cost_usd"`
	RecoverableShareOfZeroFind   float64 `json:"recoverable_share_of_zero_finding_spend"`
	RecoverableShareOfTotalSpend float64 `json:"recoverable_share_of_total_spend"`

	// NoCoverage* counts the zero-finding population that reported zero
	// findings without reviewing anything (a failed run). Recoverable*Reviewed
	// is the headline with those removed — the number a build/don't-build
	// decision should actually be taken against, since a heuristic that skips
	// a re-review cannot recover spend on a run that never reviewed.
	NoCoverageRuns             int     `json:"no_coverage_runs"`
	NoCoverageCostUSD          float64 `json:"no_coverage_cost_usd"`
	RecoverableRunsReviewed    int     `json:"recoverable_runs_reviewed"`
	RecoverableCostReviewedUSD float64 `json:"recoverable_cost_reviewed_usd"`

	SkippedRunsExcluded      int `json:"skipped_runs_excluded"`
	HistoryRunsOutsideWindow int `json:"history_runs_outside_window"`

	// Runs is the per-run detail behind the cells, ordered by (anvil, PR,
	// time). Present so a cell count is auditable to the row.
	Runs []ZeroFindingRun `json:"runs"`
}

// ReportZeroFindings fetches a window's runs and classifies its zero-finding
// tail. It shares RunSource with ReportRepeatCost, so both reports read the
// same rows through the same query.
func ReportZeroFindings(src RunSource, since, until time.Time, opts Options) (*ZeroFindingReport, error) {
	if src == nil {
		return nil, fmt.Errorf("zero-finding analysis: nil run source")
	}
	runs, err := src.AssayRunHistory(since, until)
	if err != nil {
		return nil, fmt.Errorf("zero-finding analysis: loading assay runs: %w", err)
	}
	return BuildZeroFindingReport(runs, since, until, opts), nil
}

// BuildZeroFindingReport is the pure core.
//
// The order of operations is the same one BuildReport uses and is not
// interchangeable: filter (skipped, anvil) -> derive ordinals over what
// survives -> walk each PR's runs in sequence -> classify the zero-finding ones
// that fall in the window. Classifying before ordinals are derived would leave
// every run looking like a first; restricting to the window before walking
// would sever a run from the predecessor its cell depends on.
func BuildZeroFindingReport(runs []RunRecord, since, until time.Time, opts Options) *ZeroFindingReport {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	report := &ZeroFindingReport{
		Since:              optionalTime(since),
		Until:              optionalTime(until),
		GeneratedAt:        now,
		Anvil:              opts.Anvil,
		IncludeSkipped:     opts.IncludeSkipped,
		SubstanceFieldUsed: SubstanceField,
	}

	eligible, skippedInWindow := eligibleRuns(runs, since, until, opts)
	report.SkippedRunsExcluded = skippedInWindow

	ordered := DeriveRunOrdinals(eligible)

	cells := make([]ZeroFindingCell, len(zeroFindingCellSpec))
	byLabel := make(map[string]*ZeroFindingCell, len(zeroFindingCellSpec))
	for i, label := range zeroFindingCellSpec {
		cells[i] = ZeroFindingCell{Label: label, prs: map[string]struct{}{}}
		byLabel[label] = &cells[i]
	}

	zeroPRs := map[string]struct{}{}
	var (
		prev    RunRecord
		prevKey string
		haveAny bool
	)

	for _, o := range ordered {
		run := o.Run
		key := run.PRKey()
		inWin := inWindow(run.StartedAt, since, until)

		if inWin {
			report.TotalRuns++
			report.TotalCostUSD += run.CostUSD
		} else {
			report.HistoryRunsOutsideWindow++
		}
		if strings.TrimSpace(run.HeadSHA) != "" {
			haveAny = true
		}

		// A new PR resets the predecessor: the run before this one in the
		// ordering belongs to a different PR and says nothing about this one.
		if key != prevKey {
			prev = RunRecord{}
			prevKey = key
		}

		if inWin && run.FindingsCount == 0 {
			detail := classifyZeroFindingRun(o, prev, since, until)
			if strings.TrimSpace(run.HeadSHA) == "" {
				report.RunsMissingHeadSHA++
			}
			cell := byLabel[detail.Cell]
			cell.Runs++
			cell.CostUSD += run.CostUSD
			cell.prs[key] = struct{}{}
			if detail.NoCoverage {
				cell.NoCoverageRuns++
				cell.NoCoverageCostUSD += run.CostUSD
				report.NoCoverageRuns++
				report.NoCoverageCostUSD += run.CostUSD
			}
			if detail.PrevRunID != 0 {
				if detail.PrevClean {
					cell.PrevCleanRuns++
					cell.PrevCleanCostUSD += run.CostUSD
				} else {
					cell.PrevWithFindingsRuns++
					cell.PrevWithFindingsCostUSD += run.CostUSD
				}
			}

			zeroPRs[key] = struct{}{}
			report.ZeroFindingRuns++
			report.ZeroFindingCostUSD += run.CostUSD
			report.Runs = append(report.Runs, detail)
		}

		prev = run
	}

	for i := range cells {
		cells[i].PRs = len(cells[i].prs)
		cells[i].prs = nil
	}
	report.Cells = cells
	report.ZeroFindingPRs = len(zeroPRs)
	report.SubstanceFieldAvailable = haveAny

	recoverable := byLabel[CellRepeatCleanUnchanged]
	report.RecoverableRuns = recoverable.Runs
	report.RecoverableCostUSD = recoverable.CostUSD
	report.RecoverableShareOfZeroFind = share(recoverable.CostUSD, report.ZeroFindingCostUSD)
	report.RecoverableShareOfTotalSpend = share(recoverable.CostUSD, report.TotalCostUSD)
	report.RecoverableRunsReviewed = recoverable.Runs - recoverable.NoCoverageRuns
	report.RecoverableCostReviewedUSD = recoverable.CostUSD - recoverable.NoCoverageCostUSD

	return report
}

// classifyZeroFindingRun places one zero-finding run in its cell. prev is the
// immediately preceding run of the same PR in full-history order, or the zero
// RunRecord when there is none.
func classifyZeroFindingRun(o OrdinalRun, prev RunRecord, since, until time.Time) ZeroFindingRun {
	run := o.Run
	detail := ZeroFindingRun{
		NoCoverage: run.NoCoverage(),

		RunID:     run.RunID,
		Anvil:     run.Anvil,
		PRNumber:  run.PRNumber,
		Ordinal:   o.Ordinal,
		StartedAt: run.StartedAt,
		CostUSD:   run.CostUSD,
		HeadSHA:   run.HeadSHA,
	}

	// No predecessor is the same condition as ordinal 1 here, since ordinals
	// are derived over full history. Both are checked so a caller handing in a
	// window-local set gets the conservative answer rather than a repeat cell
	// built against a predecessor that was never read.
	if !o.IsRepeat() || prev.RunID == 0 {
		detail.Cell = CellFirstOrNoPrior
		detail.Substance = SubstanceUnknown
		return detail
	}

	detail.PrevRunID = prev.RunID
	detail.PrevHeadSHA = prev.HeadSHA
	detail.PrevFindingsCount = prev.FindingsCount
	detail.PrevClean = prev.FindingsCount == 0
	detail.PrevInWindow = inWindow(prev.StartedAt, since, until)
	detail.Substance = CompareSubstance(prev, run)

	switch detail.Substance {
	case SubstanceUnchanged:
		detail.Cell = CellRepeatCleanUnchanged
	case SubstanceChanged:
		detail.Cell = CellRepeatCleanChanged
	default:
		detail.Cell = CellRepeatCleanUnknown
	}
	return detail
}

// share renders a fraction, answering 0 rather than NaN for a zero total.
func share(part, total float64) float64 {
	if total == 0 {
		return 0
	}
	return part / total
}

// ZeroFindingBaseline is the published figure a zero-finding report is checked
// against — the parent bead's "208 runs / $429.95".
type ZeroFindingBaseline struct {
	Runs    int
	CostUSD float64
	// TolUSD is the accepted absolute dollar difference; zero means the
	// default cent, since a published figure is quoted to cents.
	TolUSD float64
}

// ZeroFindingBaselineCheck is the reconciliation between a report and that
// figure.
type ZeroFindingBaselineCheck struct {
	Expected     ZeroFindingBaseline `json:"expected"`
	ActualRuns   int                 `json:"actual_runs"`
	ActualCost   float64             `json:"actual_cost_usd"`
	RunDelta     int                 `json:"run_delta"`
	CostDeltaUSD float64             `json:"cost_delta_usd"`
	Matches      bool                `json:"matches"`
}

// ValidateZeroFindingBaseline reconciles a report's zero-finding totals against
// a published figure and reports the delta rather than deciding what it means.
// It is the mechanical form of the instruction this analysis was given: if the
// filtered set does not total the published runs and dollars, record the
// discrepancy instead of adjusting the filter until it does.
func ValidateZeroFindingBaseline(r *ZeroFindingReport, exp ZeroFindingBaseline) ZeroFindingBaselineCheck {
	tol := exp.TolUSD
	if tol <= 0 {
		tol = defaultBaselineTolUSD
	}
	check := ZeroFindingBaselineCheck{Expected: exp}
	if r != nil {
		check.ActualRuns = r.ZeroFindingRuns
		check.ActualCost = r.ZeroFindingCostUSD
	}
	check.RunDelta = check.ActualRuns - exp.Runs
	check.CostDeltaUSD = check.ActualCost - exp.CostUSD
	check.Matches = check.RunDelta == 0 && absFloat(check.CostDeltaUSD) <= tol
	return check
}

// WriteJSON emits the machine-readable form.
func (r *ZeroFindingReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteCSV emits the cells and the per-run detail in a tidy (long) shape, one
// row per fact, so a figure quoted from the table can be found again by name
// rather than by column position.
func (r *ZeroFindingReport) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	header := []string{"section", "key", "runs", "prs", "cost_usd", "detail"}
	if err := cw.Write(header); err != nil {
		return err
	}
	row := func(section, key string, runs, prs int, cost float64, detail string) error {
		return cw.Write([]string{
			section, key,
			strconv.Itoa(runs), strconv.Itoa(prs),
			strconv.FormatFloat(cost, 'f', 4, 64),
			detail,
		})
	}

	if err := row("window", "all_runs", r.TotalRuns, 0, r.TotalCostUSD, ""); err != nil {
		return err
	}
	if err := row("window", "zero_finding_runs", r.ZeroFindingRuns, r.ZeroFindingPRs, r.ZeroFindingCostUSD, ""); err != nil {
		return err
	}
	for _, c := range r.Cells {
		if err := row("cell", c.Label, c.Runs, c.PRs, c.CostUSD, ""); err != nil {
			return err
		}
		if err := row("cell_prev_clean", c.Label, c.PrevCleanRuns, 0, c.PrevCleanCostUSD, ""); err != nil {
			return err
		}
		if err := row("cell_prev_with_findings", c.Label, c.PrevWithFindingsRuns, 0, c.PrevWithFindingsCostUSD, ""); err != nil {
			return err
		}
		if err := row("cell_no_coverage", c.Label, c.NoCoverageRuns, 0, c.NoCoverageCostUSD, ""); err != nil {
			return err
		}
	}
	for _, d := range r.Runs {
		key := fmt.Sprintf("%s#%d/run-%d", d.Anvil, d.PRNumber, d.RunID)
		detail := fmt.Sprintf("ordinal=%d cell=%s substance=%s prev_run=%d prev_findings=%d no_coverage=%t",
			d.Ordinal, d.Cell, d.Substance, d.PrevRunID, d.PrevFindingsCount, d.NoCoverage)
		if err := row("run", key, 1, 0, d.CostUSD, detail); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteTable emits the human-readable report.
//
// The notes below the table are part of it, not decoration: the substance axis
// is a head-SHA proxy with a stated blind spot, and a cell count read without
// that caveat is a stronger claim than the data supports.
func (r *ZeroFindingReport) WriteTable(w io.Writer) error {
	fmt.Fprintf(w, "Assay zero-finding run analysis\n")
	fmt.Fprintf(w, "  window            %s\n", formatWindow(r.Since, r.Until))
	if r.Anvil != "" {
		fmt.Fprintf(w, "  anvil             %s\n", r.Anvil)
	}
	fmt.Fprintf(w, "  substance axis    %s (%s)\n", r.SubstanceFieldUsed, substanceAvailabilityText(r.SubstanceFieldAvailable))
	fmt.Fprintf(w, "  runs in window    %d ($%.2f recorded)\n", r.TotalRuns, r.TotalCostUSD)
	fmt.Fprintf(w, "  zero-finding      %d run(s) over %d PR(s) ($%.2f, %s of window spend)\n",
		r.ZeroFindingRuns, r.ZeroFindingPRs, r.ZeroFindingCostUSD,
		percentOf(r.ZeroFindingCostUSD, r.TotalCostUSD))
	if r.SkippedRunsExcluded > 0 {
		fmt.Fprintf(w, "  excluded          %d skipped run(s) — no passes dispatched (--include-skipped to keep)\n", r.SkippedRunsExcluded)
	}
	if r.HistoryRunsOutsideWindow > 0 {
		fmt.Fprintf(w, "  history read      %d earlier run(s), for ordinals and predecessors\n", r.HistoryRunsOutsideWindow)
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CELL\tRUNS\tPRS\tCOST $\t% RUNS\t% ZERO-FIND $\t% WINDOW $\tPREV CLEAN\tPREV HAD FINDINGS\tNO COVERAGE")
	for _, c := range r.Cells {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%.2f\t%s\t%s\t%s\t%d ($%.2f)\t%d ($%.2f)\t%d ($%.2f)\n",
			c.Label, c.Runs, c.PRs, c.CostUSD,
			percentOfInt(c.Runs, r.ZeroFindingRuns),
			percentOf(c.CostUSD, r.ZeroFindingCostUSD),
			percentOf(c.CostUSD, r.TotalCostUSD),
			c.PrevCleanRuns, c.PrevCleanCostUSD,
			c.PrevWithFindingsRuns, c.PrevWithFindingsCostUSD,
			c.NoCoverageRuns, c.NoCoverageCostUSD)
	}
	fmt.Fprintf(tw, "TOTAL\t%d\t%d\t%.2f\t%s\t%s\t%s\t\t\t%d ($%.2f)\n",
		r.ZeroFindingRuns, r.ZeroFindingPRs, r.ZeroFindingCostUSD,
		percentOfInt(r.ZeroFindingRuns, r.ZeroFindingRuns),
		percentOf(r.ZeroFindingCostUSD, r.ZeroFindingCostUSD),
		percentOf(r.ZeroFindingCostUSD, r.TotalCostUSD),
		r.NoCoverageRuns, r.NoCoverageCostUSD)
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "Headline: repeat clean reviews of UNCHANGED substance are %d run(s), $%.2f — %s of\n",
		r.RecoverableRuns, r.RecoverableCostUSD,
		percentOf(r.RecoverableCostUSD, r.ZeroFindingCostUSD))
	fmt.Fprintf(w, "  zero-finding spend and %s of all Assay spend in the window. That is the ONLY cell a\n",
		percentOf(r.RecoverableCostUSD, r.TotalCostUSD))
	fmt.Fprintln(w, "  skip/short-circuit heuristic could recover; every other cell reviewed content that had")
	fmt.Fprintln(w, "  not been reviewed before.")
	if r.RecoverableRuns != r.RecoverableRunsReviewed {
		fmt.Fprintf(w, "  Excluding runs that never reviewed anything, that cell is %d run(s), $%.2f.\n",
			r.RecoverableRunsReviewed, r.RecoverableCostReviewedUSD)
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Notes:")
	fmt.Fprintf(w, "  - The substance axis is %s and nothing else: assay_runs records no diff hash, no\n", SubstanceField)
	fmt.Fprintln(w, "    changed-file set, no diff size and no base SHA. 'unchanged' means the previous run of")
	fmt.Fprintln(w, "    the same PR reviewed the same head commit — the same key Assay's own re-dispatch gate")
	fmt.Fprintln(w, "    (LastReviewedSHA) uses.")
	fmt.Fprintln(w, "  - 'changed' is a proxy, not a proof: a push touching only a generated file moves the head")
	fmt.Fprintln(w, "    while leaving the reviewable diff identical. It errs towards NOT claiming recoverable")
	fmt.Fprintln(w, "    spend, which is the safe direction for a build/don't-build decision.")
	fmt.Fprintln(w, "  - The base branch a diff is taken against is not recorded, so two runs over one head SHA")
	fmt.Fprintln(w, "    whose base moved between them are counted 'unchanged' here. That over-states the")
	fmt.Fprintln(w, "    recoverable cell rather than under-stating it.")
	if r.RunsMissingHeadSHA > 0 {
		fmt.Fprintf(w, "  - %d zero-finding run(s) carry no head SHA of their own; their repeats land in\n", r.RunsMissingHeadSHA)
		fmt.Fprintf(w, "    %s rather than being assigned a substance verdict.\n", CellRepeatCleanUnknown)
	}
	if !r.SubstanceFieldAvailable {
		fmt.Fprintln(w, "  - NO run in this population records a head SHA, so the changed/unchanged split is")
		fmt.Fprintln(w, "    UNAVAILABLE: every repeat is reported as unknown. Nothing here infers substance from")
		fmt.Fprintln(w, "    timestamps or cost, and the two-cell first-vs-repeat reading is all the data supports.")
	}
	if r.NoCoverageRuns > 0 {
		fmt.Fprintf(w, "  - NO COVERAGE counts the %d run(s) ($%.2f) in this population that reported zero findings\n",
			r.NoCoverageRuns, r.NoCoverageCostUSD)
		fmt.Fprintln(w, "    because no pass ever ran (a failed run), not because a review read the diff and found")
		fmt.Fprintln(w, "    nothing. Their spend still counts — a failure is not a refund — but a skip heuristic")
		fmt.Fprintln(w, "    could not recover it: there was no review to skip.")
	}
	fmt.Fprintln(w, "  - PREV CLEAN / PREV HAD FINDINGS split each repeat cell by what the PRECEDING run")
	fmt.Fprintln(w, "    reported, since a skip heuristic would key on 'the last review of this head was clean'")
	fmt.Fprintln(w, "    rather than on 'this is a repeat'.")
	fmt.Fprintln(w, "  - Ordinals and predecessors are derived over each PR's full review history, then")
	fmt.Fprintln(w, "    restricted to the window (cost.DeriveRunOrdinals — the same definition forge cost assay")
	fmt.Fprintln(w, "    uses).")
	return nil
}

// substanceAvailabilityText names the state of the substance axis in the header,
// so a report whose population records no head SHA says so at the top rather
// than only in a note under the table.
func substanceAvailabilityText(available bool) string {
	if available {
		return "recorded"
	}
	return "NOT RECORDED — changed/unchanged split unavailable"
}

// percentOfInt renders a count share, answering "-" rather than NaN for a zero
// total. It is the integer twin of percentOf rather than a caller-side float
// conversion, so the two shares in one row are rendered by the same rule.
func percentOfInt(part, total int) string {
	if total == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", float64(part)/float64(total)*100)
}

// SortRuns orders the per-run detail by (anvil, PR, started_at, run id), the
// same total order DeriveRunOrdinals uses. BuildZeroFindingReport already emits
// them in that order; this exists for a caller assembling details from more
// than one report.
func SortRuns(runs []ZeroFindingRun) {
	sort.SliceStable(runs, func(i, j int) bool {
		a, b := runs[i], runs[j]
		if a.Anvil != b.Anvil {
			return a.Anvil < b.Anvil
		}
		if a.PRNumber != b.PRNumber {
			return a.PRNumber < b.PRNumber
		}
		if !a.StartedAt.Equal(b.StartedAt) {
			return a.StartedAt.Before(b.StartedAt)
		}
		return a.RunID < b.RunID
	})
}
