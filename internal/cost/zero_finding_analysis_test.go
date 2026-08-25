package cost

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func zfBase() time.Time { return time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC) }

// zfRun builds a run row for the analysis tests. Minutes keep the ordering
// explicit at every call site.
func zfRun(id, pr, minute int, sha string, findings int, costUSD float64) RunRecord {
	return RunRecord{
		RunID:         id,
		Anvil:         "forge",
		PRNumber:      pr,
		HeadSHA:       sha,
		StartedAt:     zfBase().Add(time.Duration(minute) * time.Minute),
		CostUSD:       costUSD,
		FindingsCount: findings,
	}
}

// cellOf finds a cell by label, failing rather than returning a zero value: a
// missing cell is a bug in the spec list, not an empty result.
func cellOf(t *testing.T, r *ZeroFindingReport, label string) ZeroFindingCell {
	t.Helper()
	for _, c := range r.Cells {
		if c.Label == label {
			return c
		}
	}
	t.Fatalf("no cell %q in report", label)
	return ZeroFindingCell{}
}

func TestCompareSubstance(t *testing.T) {
	cases := []struct {
		name       string
		prev, next string
		want       string
	}{
		{"identical", "abc123", "abc123", SubstanceUnchanged},
		{"case insensitive", "ABC123", "abc123", SubstanceUnchanged},
		{"whitespace trimmed", " abc123 ", "abc123", SubstanceUnchanged},
		{"different", "abc123", "def456", SubstanceChanged},
		{"prev missing", "", "abc123", SubstanceUnknown},
		{"next missing", "abc123", "", SubstanceUnknown},
		{"both missing", "", "", SubstanceUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CompareSubstance(RunRecord{HeadSHA: tc.prev}, RunRecord{HeadSHA: tc.next})
			if got != tc.want {
				t.Fatalf("CompareSubstance(%q, %q) = %q, want %q", tc.prev, tc.next, got, tc.want)
			}
		})
	}
}

// A first review that finds nothing is not waste and must never land in the
// recoverable cell — the distinction the whole analysis exists to draw.
func TestZeroFindingFirstRunIsNotRecoverable(t *testing.T) {
	runs := []RunRecord{zfRun(1, 10, 0, "aaa", 0, 3.00)}

	r := BuildZeroFindingReport(runs, time.Time{}, time.Time{}, Options{Now: zfBase()})

	if r.ZeroFindingRuns != 1 {
		t.Fatalf("zero-finding runs = %d, want 1", r.ZeroFindingRuns)
	}
	first := cellOf(t, r, CellFirstOrNoPrior)
	if first.Runs != 1 || first.CostUSD != 3.00 {
		t.Fatalf("first cell = %d run(s), $%.2f; want 1, $3.00", first.Runs, first.CostUSD)
	}
	if r.RecoverableRuns != 0 || r.RecoverableCostUSD != 0 {
		t.Fatalf("recoverable = %d run(s), $%.2f; want 0", r.RecoverableRuns, r.RecoverableCostUSD)
	}
	// The predecessor sub-columns are meaningless for a run with no
	// predecessor and must stay empty rather than counting it as prev-clean.
	if first.PrevCleanRuns != 0 || first.PrevWithFindingsRuns != 0 {
		t.Fatalf("first cell carries prev sub-counts: clean=%d withFindings=%d", first.PrevCleanRuns, first.PrevWithFindingsRuns)
	}
}

func TestZeroFindingRepeatUnchangedIsRecoverable(t *testing.T) {
	runs := []RunRecord{
		zfRun(1, 10, 0, "aaa", 0, 3.00),
		zfRun(2, 10, 10, "aaa", 0, 2.50),
	}

	r := BuildZeroFindingReport(runs, time.Time{}, time.Time{}, Options{Now: zfBase()})

	unchanged := cellOf(t, r, CellRepeatCleanUnchanged)
	if unchanged.Runs != 1 || unchanged.CostUSD != 2.50 {
		t.Fatalf("unchanged cell = %d run(s), $%.2f; want 1, $2.50", unchanged.Runs, unchanged.CostUSD)
	}
	// The predecessor was itself clean — the sub-column a skip heuristic would
	// actually key on.
	if unchanged.PrevCleanRuns != 1 || unchanged.PrevWithFindingsRuns != 0 {
		t.Fatalf("prev split = clean %d / withFindings %d; want 1 / 0", unchanged.PrevCleanRuns, unchanged.PrevWithFindingsRuns)
	}
	if r.RecoverableCostUSD != 2.50 {
		t.Fatalf("recoverable = $%.2f, want $2.50", r.RecoverableCostUSD)
	}
	if got := r.RecoverableShareOfZeroFind; got < 0.45 || got > 0.46 {
		t.Fatalf("recoverable share of zero-finding spend = %.4f, want ~0.4545", got)
	}
}

func TestZeroFindingRepeatChangedIsNotRecoverable(t *testing.T) {
	runs := []RunRecord{
		zfRun(1, 10, 0, "aaa", 0, 3.00),
		zfRun(2, 10, 10, "bbb", 0, 2.50),
	}

	r := BuildZeroFindingReport(runs, time.Time{}, time.Time{}, Options{Now: zfBase()})

	changed := cellOf(t, r, CellRepeatCleanChanged)
	if changed.Runs != 1 || changed.CostUSD != 2.50 {
		t.Fatalf("changed cell = %d run(s), $%.2f; want 1, $2.50", changed.Runs, changed.CostUSD)
	}
	if r.RecoverableRuns != 0 {
		t.Fatalf("recoverable = %d run(s), want 0", r.RecoverableRuns)
	}
}

// The None path: a repeat whose head SHA is unrecorded is reported as a gap,
// never inferred into changed or unchanged.
func TestZeroFindingMissingHeadSHAIsUnknownNotInferred(t *testing.T) {
	runs := []RunRecord{
		zfRun(1, 10, 0, "", 0, 3.00),
		zfRun(2, 10, 10, "", 0, 2.50),
	}

	r := BuildZeroFindingReport(runs, time.Time{}, time.Time{}, Options{Now: zfBase()})

	unknown := cellOf(t, r, CellRepeatCleanUnknown)
	if unknown.Runs != 1 || unknown.CostUSD != 2.50 {
		t.Fatalf("unknown cell = %d run(s), $%.2f; want 1, $2.50", unknown.Runs, unknown.CostUSD)
	}
	if r.RecoverableRuns != 0 {
		t.Fatalf("recoverable = %d run(s), want 0 — an unrecorded field must not be read as 'unchanged'", r.RecoverableRuns)
	}
	if r.SubstanceFieldAvailable {
		t.Fatal("SubstanceFieldAvailable = true, want false when no run records a head SHA")
	}
	if r.RunsMissingHeadSHA != 2 {
		t.Fatalf("RunsMissingHeadSHA = %d, want 2", r.RunsMissingHeadSHA)
	}

	var buf bytes.Buffer
	if err := r.WriteTable(&buf); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	if !strings.Contains(buf.String(), "UNAVAILABLE") {
		t.Fatalf("table does not name the missing substance field:\n%s", buf.String())
	}
}

// A repeat whose predecessor reported findings is still a repeat — classified
// by ordinal and substance — but the prev-clean sub-column has to tell it apart
// from one following a clean review, since that is what a heuristic keys on.
func TestZeroFindingRepeatAfterFindingsSplitsBySubColumn(t *testing.T) {
	runs := []RunRecord{
		zfRun(1, 10, 0, "aaa", 3, 4.00),  // found something
		zfRun(2, 10, 10, "aaa", 0, 2.00), // clean re-review of the same head
	}

	r := BuildZeroFindingReport(runs, time.Time{}, time.Time{}, Options{Now: zfBase()})

	if r.ZeroFindingRuns != 1 {
		t.Fatalf("zero-finding runs = %d, want 1 (the run with findings is not one)", r.ZeroFindingRuns)
	}
	unchanged := cellOf(t, r, CellRepeatCleanUnchanged)
	if unchanged.Runs != 1 {
		t.Fatalf("unchanged cell = %d run(s), want 1", unchanged.Runs)
	}
	if unchanged.PrevCleanRuns != 0 || unchanged.PrevWithFindingsRuns != 1 {
		t.Fatalf("prev split = clean %d / withFindings %d; want 0 / 1", unchanged.PrevCleanRuns, unchanged.PrevWithFindingsRuns)
	}
	if unchanged.PrevWithFindingsCostUSD != 2.00 {
		t.Fatalf("prev-with-findings cost = $%.2f, want $2.00", unchanged.PrevWithFindingsCostUSD)
	}
}

// PR numbers are per-repository, so two anvils' PR #10 must not share a
// predecessor — the same argument PRKey exists for.
func TestZeroFindingKeepsAnvilsApart(t *testing.T) {
	a := zfRun(1, 10, 0, "aaa", 0, 1.00)
	b := zfRun(2, 10, 10, "aaa", 0, 1.00)
	b.Anvil = "other"

	r := BuildZeroFindingReport([]RunRecord{a, b}, time.Time{}, time.Time{}, Options{Now: zfBase()})

	first := cellOf(t, r, CellFirstOrNoPrior)
	if first.Runs != 2 {
		t.Fatalf("first cell = %d run(s), want 2 — each anvil's PR #10 is its own PR", first.Runs)
	}
	if r.RecoverableRuns != 0 {
		t.Fatalf("recoverable = %d, want 0", r.RecoverableRuns)
	}
}

// The predecessor is taken from full history, so a repeat whose previous run
// sits before the window is not promoted to "first" — the same failure mode the
// ordinal derivation guards against, on the substance axis.
func TestZeroFindingPredecessorReadFromOutsideWindow(t *testing.T) {
	runs := []RunRecord{
		zfRun(1, 10, 0, "aaa", 0, 3.00),
		zfRun(2, 10, 120, "aaa", 0, 2.00),
	}
	since := zfBase().Add(time.Hour)

	r := BuildZeroFindingReport(runs, since, time.Time{}, Options{Now: zfBase()})

	if r.ZeroFindingRuns != 1 {
		t.Fatalf("zero-finding runs = %d, want 1 (only the in-window run)", r.ZeroFindingRuns)
	}
	if r.HistoryRunsOutsideWindow != 1 {
		t.Fatalf("history runs outside window = %d, want 1", r.HistoryRunsOutsideWindow)
	}
	unchanged := cellOf(t, r, CellRepeatCleanUnchanged)
	if unchanged.Runs != 1 {
		t.Fatalf("unchanged cell = %d run(s), want 1 — the predecessor outside the window still counts", unchanged.Runs)
	}
	if len(r.Runs) != 1 || r.Runs[0].PrevInWindow {
		t.Fatalf("expected the one detail row to record its predecessor as out-of-window: %+v", r.Runs)
	}
	if r.Runs[0].Ordinal != 2 {
		t.Fatalf("ordinal = %d, want 2", r.Runs[0].Ordinal)
	}
}

// A skipped run reviewed nothing, so counting one would push a PR's genuine
// second review to ordinal 3 — the definition the attribution report and the
// per-PR run cap already share.
func TestZeroFindingExcludesSkippedRunsByDefault(t *testing.T) {
	skipped := zfRun(2, 10, 10, "bbb", 0, 0)
	skipped.SkippedReason = "no reviewable changes"
	runs := []RunRecord{
		zfRun(1, 10, 0, "aaa", 0, 3.00),
		skipped,
		zfRun(3, 10, 20, "ccc", 0, 2.00),
	}

	r := BuildZeroFindingReport(runs, time.Time{}, time.Time{}, Options{Now: zfBase()})

	if r.ZeroFindingRuns != 2 {
		t.Fatalf("zero-finding runs = %d, want 2", r.ZeroFindingRuns)
	}
	if r.SkippedRunsExcluded != 1 {
		t.Fatalf("skipped runs excluded = %d, want 1", r.SkippedRunsExcluded)
	}
	// Without the exclusion the third run would be ordinal 3 and its
	// predecessor the skipped row, whose head SHA it never reviewed.
	last := r.Runs[len(r.Runs)-1]
	if last.Ordinal != 2 || last.PrevRunID != 1 {
		t.Fatalf("last run ordinal=%d prev=%d; want ordinal 2 after run 1", last.Ordinal, last.PrevRunID)
	}
}

// Every zero-finding run lands in exactly one cell, and the cells sum back to
// the totals: an analysis whose parts do not add up cannot support a decision.
func TestZeroFindingCellsSumToTotals(t *testing.T) {
	runs := []RunRecord{
		zfRun(1, 10, 0, "aaa", 0, 3.00),  // first
		zfRun(2, 10, 10, "aaa", 0, 2.50), // repeat unchanged
		zfRun(3, 10, 20, "bbb", 0, 2.00), // repeat changed
		zfRun(4, 11, 30, "ccc", 2, 5.00), // has findings — excluded
		zfRun(5, 11, 40, "ccc", 0, 1.25), // repeat unchanged
		zfRun(6, 12, 50, "", 0, 0.75),    // first, no SHA
		zfRun(7, 12, 60, "ddd", 0, 0.50), // repeat, prev SHA missing -> unknown
	}

	r := BuildZeroFindingReport(runs, time.Time{}, time.Time{}, Options{Now: zfBase()})

	var (
		sumRuns int
		sumCost float64
	)
	for _, c := range r.Cells {
		sumRuns += c.Runs
		sumCost += c.CostUSD
		if c.Label != CellFirstOrNoPrior && c.PrevCleanRuns+c.PrevWithFindingsRuns != c.Runs {
			t.Fatalf("cell %s: prev sub-columns (%d + %d) do not sum to %d runs",
				c.Label, c.PrevCleanRuns, c.PrevWithFindingsRuns, c.Runs)
		}
	}
	if sumRuns != r.ZeroFindingRuns || sumRuns != 6 {
		t.Fatalf("cells sum to %d run(s), report says %d, want 6", sumRuns, r.ZeroFindingRuns)
	}
	if diff := sumCost - r.ZeroFindingCostUSD; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cells sum to $%.4f, report says $%.4f", sumCost, r.ZeroFindingCostUSD)
	}
	if len(r.Runs) != sumRuns {
		t.Fatalf("%d detail row(s) for %d classified run(s)", len(r.Runs), sumRuns)
	}
	// The whole-window denominator counts every run, including the one with
	// findings, so the "% of all spend" share is not quoted against the clean
	// subset by accident.
	if r.TotalRuns != 7 {
		t.Fatalf("total runs = %d, want 7", r.TotalRuns)
	}
	if r.RecoverableRuns != 2 {
		t.Fatalf("recoverable = %d run(s), want 2", r.RecoverableRuns)
	}
	if got := r.RecoverableShareOfTotalSpend; got <= 0 || got >= 1 {
		t.Fatalf("recoverable share of total spend = %.4f, want a fraction in (0,1)", got)
	}
}

func TestZeroFindingEmptyPopulation(t *testing.T) {
	r := BuildZeroFindingReport(nil, time.Time{}, time.Time{}, Options{Now: zfBase()})

	if r.ZeroFindingRuns != 0 || r.RecoverableCostUSD != 0 {
		t.Fatalf("expected an empty report, got %d run(s) / $%.2f", r.ZeroFindingRuns, r.RecoverableCostUSD)
	}
	if r.RecoverableShareOfZeroFind != 0 || r.RecoverableShareOfTotalSpend != 0 {
		t.Fatal("shares over a zero denominator must be 0, not NaN")
	}
	if len(r.Cells) != len(zeroFindingCellSpec) {
		t.Fatalf("cells = %d, want %d — the cell list is fixed so two reports print alike", len(r.Cells), len(zeroFindingCellSpec))
	}
	var buf bytes.Buffer
	if err := r.WriteTable(&buf); err != nil {
		t.Fatalf("WriteTable on an empty report: %v", err)
	}
}

func TestValidateZeroFindingBaselineReportsDeltaWithoutBending(t *testing.T) {
	runs := []RunRecord{
		zfRun(1, 10, 0, "aaa", 0, 3.00),
		zfRun(2, 10, 10, "aaa", 0, 2.50),
	}
	r := BuildZeroFindingReport(runs, time.Time{}, time.Time{}, Options{Now: zfBase()})

	check := ValidateZeroFindingBaseline(r, ZeroFindingBaseline{Runs: 208, CostUSD: 429.95})
	if check.Matches {
		t.Fatal("expected the published baseline not to reproduce over this fixture")
	}
	if check.RunDelta != 2-208 {
		t.Fatalf("run delta = %d, want %d", check.RunDelta, 2-208)
	}
	if check.ActualRuns != 2 || check.ActualCost != 5.50 {
		t.Fatalf("actuals = %d run(s) / $%.2f; the check must report the data, not the expectation", check.ActualRuns, check.ActualCost)
	}

	exact := ValidateZeroFindingBaseline(r, ZeroFindingBaseline{Runs: 2, CostUSD: 5.50})
	if !exact.Matches {
		t.Fatalf("expected an exact baseline to reproduce, got delta %+d / %+.4f", exact.RunDelta, exact.CostDeltaUSD)
	}
}

func TestZeroFindingRendersJSONAndCSV(t *testing.T) {
	runs := []RunRecord{
		zfRun(1, 10, 0, "aaa", 0, 3.00),
		zfRun(2, 10, 10, "aaa", 0, 2.50),
	}
	r := BuildZeroFindingReport(runs, time.Time{}, time.Time{}, Options{Now: zfBase()})

	var js bytes.Buffer
	if err := r.WriteJSON(&js); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	for _, want := range []string{CellRepeatCleanUnchanged, "recoverable_cost_usd", "substance_field_used"} {
		if !strings.Contains(js.String(), want) {
			t.Fatalf("JSON missing %q:\n%s", want, js.String())
		}
	}

	var csvOut bytes.Buffer
	if err := r.WriteCSV(&csvOut); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(csvOut.String()), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "section,key,") {
		t.Fatalf("unexpected CSV shape:\n%s", csvOut.String())
	}
}

// A run that died before any pass ran reports findings_count = 0 exactly as a
// clean review does. Counting it as a clean re-review would inflate the one
// cell the whole analysis exists to size, so it is annotated apart.
func TestZeroFindingNoCoverageRunsAreAnnotatedNotCounted(t *testing.T) {
	first := zfRun(1, 10, 0, "aaa", 0, 0)
	first.Status = "failed"
	first.Error = "assay pass triage: provider claude rate limited"
	second := zfRun(2, 10, 10, "aaa", 0, 0)
	second.Status = "failed"
	second.Error = "assay pass triage: provider claude rate limited"

	r := BuildZeroFindingReport([]RunRecord{first, second}, time.Time{}, time.Time{}, Options{Now: zfBase()})

	// Still classified and still counted: the population must reconcile.
	unchanged := cellOf(t, r, CellRepeatCleanUnchanged)
	if unchanged.Runs != 1 {
		t.Fatalf("unchanged cell = %d run(s), want 1 — a failed run is still classified", unchanged.Runs)
	}
	if unchanged.NoCoverageRuns != 1 {
		t.Fatalf("cell no-coverage = %d, want 1", unchanged.NoCoverageRuns)
	}
	if r.NoCoverageRuns != 2 {
		t.Fatalf("report no-coverage = %d run(s), want 2", r.NoCoverageRuns)
	}
	// ...but the headline a decision is taken against nets them out.
	if r.RecoverableRunsReviewed != 0 || r.RecoverableCostReviewedUSD != 0 {
		t.Fatalf("recoverable-reviewed = %d run(s) / $%.2f; want 0 — no review happened to skip",
			r.RecoverableRunsReviewed, r.RecoverableCostReviewedUSD)
	}

	var buf bytes.Buffer
	if err := r.WriteTable(&buf); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	if !strings.Contains(buf.String(), "NO COVERAGE") {
		t.Fatalf("table does not report the no-coverage split:\n%s", buf.String())
	}
}

func TestNoCoverageClassification(t *testing.T) {
	cases := []struct {
		name   string
		record RunRecord
		want   bool
	}{
		{"persisted failed status", RunRecord{Status: "failed", Error: "died"}, true},
		{"failed status mixed case", RunRecord{Status: "Failed"}, true},
		{"complete run", RunRecord{Status: "complete"}, false},
		// A partial run read part of the head; its zero findings are a real
		// (if incomplete) verdict, so it is not no-coverage.
		{"partial run with an error", RunRecord{Status: "partial", Error: "one pass died", CostUSD: 5}, false},
		// Pre-instrumentation rows carry no status. Error plus no spend never
		// reached the model; error plus spend did, whatever it failed at.
		{"legacy row, error and no spend", RunRecord{Error: "provider failed", CostUSD: 0}, true},
		{"legacy row, error but billed", RunRecord{Error: "one pass hit max turns", CostUSD: 2.50}, false},
		{"legacy row, no error", RunRecord{CostUSD: 3.00}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.record.NoCoverage(); got != tc.want {
				t.Fatalf("NoCoverage() = %t, want %t", got, tc.want)
			}
		})
	}
}

// The failed-status literal is duplicated here rather than imported (cost is a
// leaf package), so it is pinned against the value assay actually persists.
func TestNoCoverageMatchesPersistedFailedStatus(t *testing.T) {
	// internal/assay.RunStatusFailed and internal/state.AssayStatusFailed are
	// both "failed"; if either moves, this catches the drift here rather than
	// in a report that silently stops annotating anything.
	if runStatusFailed != "failed" {
		t.Fatalf("runStatusFailed = %q, want %q", runStatusFailed, "failed")
	}
}
