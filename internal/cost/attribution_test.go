package cost

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func ts(day, hour int) time.Time {
	return time.Date(2026, 6, day, hour, 0, 0, 0, time.UTC)
}

// run builds a review run with cache accounting.
func run(id int, anvil string, pr int, at time.Time, costUSD float64, cacheWrite, cacheRead int) RunRecord {
	return RunRecord{
		RunID:               id,
		Anvil:               anvil,
		PRNumber:            pr,
		StartedAt:           at,
		CostUSD:             costUSD,
		CacheCreationTokens: cacheWrite,
		CacheReadTokens:     cacheRead,
	}
}

func TestDeriveRunOrdinalsNumbersEachPRIndependently(t *testing.T) {
	// Two PRs interleaved in time, plus a third with a single run.
	runs := []RunRecord{
		run(3, "a", 1, ts(1, 12), 1, 0, 0),
		run(1, "a", 2, ts(1, 10), 1, 0, 0),
		run(4, "a", 2, ts(1, 13), 1, 0, 0),
		run(2, "a", 1, ts(1, 11), 1, 0, 0),
		run(5, "b", 7, ts(1, 14), 1, 0, 0),
	}
	got := map[string]int{}
	for _, o := range DeriveRunOrdinals(runs) {
		got[fmt.Sprintf("%s/%d", o.Run.PRKey(), o.Run.RunID)] = o.Ordinal
	}
	want := map[string]int{
		"a#1/2": 1, "a#1/3": 2,
		"a#2/1": 1, "a#2/4": 2,
		"b#7/5": 1,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("ordinal for %s = %d, want %d", k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d ordinals, want %d", len(got), len(want))
	}
}

// A PR number is only unique inside its repository. Two anvils sharing PR #1
// must not be folded into one PR, or every one of the second anvil's reviews
// is misreported as a repeat.
func TestDeriveRunOrdinalsSeparatesAnvilsSharingAPRNumber(t *testing.T) {
	runs := []RunRecord{
		run(1, "alpha", 1, ts(1, 10), 1, 0, 0),
		run(2, "beta", 1, ts(1, 11), 1, 0, 0),
	}
	for _, o := range DeriveRunOrdinals(runs) {
		if o.Ordinal != 1 {
			t.Errorf("%s: ordinal = %d, want 1", o.Run.PRKey(), o.Ordinal)
		}
	}
}

// Repeatability is the point of the report, so a timestamp tie must resolve the
// same way every time rather than following the storage layer's row order.
func TestDeriveRunOrdinalsBreaksTimestampTiesDeterministically(t *testing.T) {
	same := ts(1, 10)
	forward := []RunRecord{run(7, "a", 1, same, 1, 0, 0), run(4, "a", 1, same, 1, 0, 0)}
	backward := []RunRecord{run(4, "a", 1, same, 1, 0, 0), run(7, "a", 1, same, 1, 0, 0)}

	for _, input := range [][]RunRecord{forward, backward} {
		got := DeriveRunOrdinals(input)
		if len(got) != 2 {
			t.Fatalf("got %d runs, want 2", len(got))
		}
		if got[0].Run.RunID != 4 || got[0].Ordinal != 1 {
			t.Errorf("first = run %d ordinal %d, want run 4 ordinal 1", got[0].Run.RunID, got[0].Ordinal)
		}
		if got[1].Run.RunID != 7 || got[1].Ordinal != 2 {
			t.Errorf("second = run %d ordinal %d, want run 7 ordinal 2", got[1].Run.RunID, got[1].Ordinal)
		}
	}
}

func TestDeriveRunOrdinalsDoesNotReorderCallerSlice(t *testing.T) {
	runs := []RunRecord{
		run(3, "a", 1, ts(1, 12), 1, 0, 0),
		run(1, "a", 1, ts(1, 10), 1, 0, 0),
	}
	DeriveRunOrdinals(runs)
	if runs[0].RunID != 3 || runs[1].RunID != 1 {
		t.Errorf("caller slice was reordered: %d, %d", runs[0].RunID, runs[1].RunID)
	}
}

// Each cache class is priced at its own rate. Collapsing them into a single
// input rate would misattribute the bulk of the traffic, since a write costs
// more than ten times a read.
func TestTokenClassesPriceEachClassAtItsOwnRate(t *testing.T) {
	p := Pricing{InputPerM: 3, OutputPerM: 15, CacheReadPerM: 0.30, CacheWritePerM: 3.75}
	report := BuildReport([]RunRecord{
		run(1, "a", 1, ts(1, 10), 5.00, 1_000_000, 2_000_000),
	}, time.Time{}, time.Time{}, Options{Pricing: p, Now: ts(2, 0)})

	classes := map[string]TokenClassBreakdown{}
	for _, c := range report.ByTokenClass {
		classes[c.Class] = c
	}
	if got := classes[TokenClassCacheCreation].CostUSD; !nearly(got, 3.75) {
		t.Errorf("cache_creation cost = %.4f, want 3.75", got)
	}
	if got := classes[TokenClassCacheRead].CostUSD; !nearly(got, 0.60) {
		t.Errorf("cache_read cost = %.4f, want 0.60", got)
	}
	if got := classes[TokenClassCacheCreation].Basis; got != BasisPriced {
		t.Errorf("cache_creation basis = %q, want %q", got, BasisPriced)
	}
	// The priced cache classes explain part of the recorded spend, never all
	// of it: assay_runs stores no plain input/output token counts.
	if !nearly(report.TotalCostUSD, 5.00) {
		t.Errorf("recorded total = %.4f, want 5.00 (the provider figure, untouched)", report.TotalCostUSD)
	}
}

// Rows written before cache instrumentation carry zero in both counters. They
// must degrade to 'unknown' — never fail, and never be reported as a confident
// zero — while their recorded spend still lands in every total.
func TestRunsWithoutCacheAccountingDegradeToUnknown(t *testing.T) {
	report := BuildReport([]RunRecord{
		run(1, "a", 1, ts(1, 10), 4.00, 0, 0),
		run(2, "a", 1, ts(1, 11), 6.00, 100_000, 500_000),
	}, time.Time{}, time.Time{}, Options{Now: ts(2, 0)})

	var unknown TokenClassBreakdown
	for _, c := range report.ByTokenClass {
		if c.Class == TokenClassUnknown {
			unknown = c
		}
	}
	if unknown.Runs != 1 {
		t.Errorf("unknown runs = %d, want 1", unknown.Runs)
	}
	if !nearly(unknown.CostUSD, 4.00) {
		t.Errorf("unknown cost = %.4f, want 4.00 (the recorded cost of the unattributable run)", unknown.CostUSD)
	}
	if unknown.Basis != BasisRecorded {
		t.Errorf("unknown basis = %q, want %q", unknown.Basis, BasisRecorded)
	}
	if unknown.Tokens != 0 {
		t.Errorf("unknown tokens = %d, want 0 (not knowable)", unknown.Tokens)
	}
	if report.RunsWithoutCacheAccounting != 1 || !nearly(report.CostWithoutCacheAccountingUSD, 4.00) {
		t.Errorf("report coverage = %d runs / $%.2f, want 1 / $4.00",
			report.RunsWithoutCacheAccounting, report.CostWithoutCacheAccountingUSD)
	}
	// The money is never lost to the unknown bucket.
	if !nearly(report.TotalCostUSD, 10.00) {
		t.Errorf("total = %.4f, want 10.00", report.TotalCostUSD)
	}
}

// An all-pre-instrumentation window is the realistic older case: it must
// produce a report, not an error, with every run in the unknown class.
func TestReportOverEntirelyUninstrumentedRows(t *testing.T) {
	report := BuildReport([]RunRecord{
		run(1, "a", 1, ts(1, 10), 3.00, 0, 0),
		run(2, "a", 1, ts(1, 11), 2.00, 0, 0),
	}, time.Time{}, time.Time{}, Options{Now: ts(2, 0)})

	if report.TotalRuns != 2 || !nearly(report.TotalCostUSD, 5.00) {
		t.Fatalf("totals = %d runs / $%.2f, want 2 / $5.00", report.TotalRuns, report.TotalCostUSD)
	}
	if report.RunsWithoutCacheAccounting != 2 {
		t.Errorf("unattributable runs = %d, want 2", report.RunsWithoutCacheAccounting)
	}
	if !nearly(report.RepeatRun.CostUSD, 2.00) {
		t.Errorf("repeat cost = %.2f, want 2.00 — the split works without cache data",
			report.RepeatRun.CostUSD)
	}
	var buf bytes.Buffer
	if err := report.WriteTable(&buf); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	if !strings.Contains(buf.String(), "unknown") {
		t.Error("table does not mention the unknown token class")
	}
}

func TestBuildReportSplitsFirstFromRepeat(t *testing.T) {
	report := BuildReport([]RunRecord{
		run(1, "a", 1, ts(1, 10), 10.00, 100, 200),
		run(2, "a", 1, ts(1, 11), 3.00, 10, 900),
		run(3, "a", 1, ts(1, 12), 2.00, 10, 900),
		run(4, "a", 2, ts(1, 13), 8.00, 100, 100),
	}, time.Time{}, time.Time{}, Options{Now: ts(2, 0)})

	if report.FirstRun.Runs != 2 || !nearly(report.FirstRun.CostUSD, 18.00) {
		t.Errorf("first-run = %d runs / $%.2f, want 2 / $18.00", report.FirstRun.Runs, report.FirstRun.CostUSD)
	}
	if report.RepeatRun.Runs != 2 || !nearly(report.RepeatRun.CostUSD, 5.00) {
		t.Errorf("repeat-run = %d runs / $%.2f, want 2 / $5.00", report.RepeatRun.Runs, report.RepeatRun.CostUSD)
	}
	if report.TotalPRs != 2 {
		t.Errorf("PRs = %d, want 2 (a PR reviewed three times is one PR)", report.TotalPRs)
	}
	if !nearly(report.TotalCostUSD, report.FirstRun.CostUSD+report.RepeatRun.CostUSD) {
		t.Error("total is not the sum of the two halves")
	}
	// Ordinal buckets must agree with the split.
	byLabel := map[string]OrdinalBucket{}
	for _, b := range report.ByOrdinal {
		byLabel[b.Label] = b
	}
	if byLabel["1"].Runs != 2 || byLabel["2"].Runs != 1 || byLabel["3"].Runs != 1 {
		t.Errorf("ordinal buckets = 1:%d 2:%d 3:%d, want 2/1/1",
			byLabel["1"].Runs, byLabel["2"].Runs, byLabel["3"].Runs)
	}
}

// Ordinals derived over the window alone would call this run a first review and
// move its spend into the wrong column. Deriving over full history is the whole
// reason the source returns runs from before the window.
func TestOrdinalsUseHistoryFromBeforeTheWindow(t *testing.T) {
	since, until := ts(5, 0), ts(9, 0)
	report := BuildReport([]RunRecord{
		run(1, "a", 1, ts(1, 10), 10.00, 0, 0), // before the window
		run(2, "a", 1, ts(6, 10), 4.00, 0, 0),  // inside it — the PR's 2nd review
	}, since, until, Options{Now: ts(10, 0)})

	if report.TotalRuns != 1 {
		t.Fatalf("runs in window = %d, want 1", report.TotalRuns)
	}
	if report.FirstRun.Runs != 0 {
		t.Errorf("first-run runs = %d, want 0 — the first review was before the window", report.FirstRun.Runs)
	}
	if report.RepeatRun.Runs != 1 || !nearly(report.RepeatRun.CostUSD, 4.00) {
		t.Errorf("repeat-run = %d runs / $%.2f, want 1 / $4.00", report.RepeatRun.Runs, report.RepeatRun.CostUSD)
	}
	if report.HistoryRunsOutsideWindow != 1 {
		t.Errorf("history runs = %d, want 1", report.HistoryRunsOutsideWindow)
	}
	if !nearly(report.TotalCostUSD, 4.00) {
		t.Errorf("total = %.2f, want 4.00 — the earlier run's spend is not in this window", report.TotalCostUSD)
	}
}

// The upper bound is exclusive so consecutive windows tile without counting a
// boundary run twice.
func TestWindowUpperBoundIsExclusive(t *testing.T) {
	boundary := ts(5, 0)
	runs := []RunRecord{run(1, "a", 1, boundary, 1.00, 0, 0)}

	before := BuildReport(runs, ts(1, 0), boundary, Options{Now: ts(9, 0)})
	after := BuildReport(runs, boundary, ts(9, 0), Options{Now: ts(9, 0)})
	if before.TotalRuns != 0 {
		t.Errorf("run on the upper bound counted in the earlier window (%d runs)", before.TotalRuns)
	}
	if after.TotalRuns != 1 {
		t.Errorf("run on the lower bound missing from the later window (%d runs)", after.TotalRuns)
	}
}

// A skipped run reviewed nothing, so by default it is dropped before ordinals
// are assigned — otherwise a PR's genuine second review is labelled its third.
func TestSkippedRunsAreExcludedFromOrdinalsByDefault(t *testing.T) {
	skipped := run(2, "a", 1, ts(1, 11), 0, 0, 0)
	skipped.SkippedReason = "no_reviewable_changes"
	runs := []RunRecord{
		run(1, "a", 1, ts(1, 10), 10.00, 0, 0),
		skipped,
		run(3, "a", 1, ts(1, 12), 5.00, 0, 0),
	}

	report := BuildReport(runs, time.Time{}, time.Time{}, Options{Now: ts(2, 0)})
	if report.SkippedRunsExcluded != 1 {
		t.Errorf("excluded = %d, want 1", report.SkippedRunsExcluded)
	}
	byLabel := map[string]OrdinalBucket{}
	for _, b := range report.ByOrdinal {
		byLabel[b.Label] = b
	}
	if byLabel["2"].Runs != 1 || byLabel["3"].Runs != 0 {
		t.Errorf("with the skip dropped, the last review is ordinal 2: got 2:%d 3:%d",
			byLabel["2"].Runs, byLabel["3"].Runs)
	}

	kept := BuildReport(runs, time.Time{}, time.Time{}, Options{IncludeSkipped: true, Now: ts(2, 0)})
	if kept.TotalRuns != 3 {
		t.Errorf("--include-skipped runs = %d, want 3", kept.TotalRuns)
	}
	if kept.SkippedRunsExcluded != 0 {
		t.Errorf("--include-skipped excluded = %d, want 0", kept.SkippedRunsExcluded)
	}
}

func TestAnvilFilterRestrictsTheReport(t *testing.T) {
	report := BuildReport([]RunRecord{
		run(1, "alpha", 1, ts(1, 10), 10.00, 0, 0),
		run(2, "beta", 1, ts(1, 11), 7.00, 0, 0),
	}, time.Time{}, time.Time{}, Options{Anvil: "beta", Now: ts(2, 0)})

	if report.TotalRuns != 1 || !nearly(report.TotalCostUSD, 7.00) {
		t.Errorf("filtered report = %d runs / $%.2f, want 1 / $7.00", report.TotalRuns, report.TotalCostUSD)
	}
}

// A repeat review that reports nothing is the spend most likely to have bought
// nothing, so the report counts it rather than leaving it to a follow-up query.
func TestZeroFindingTailIsCounted(t *testing.T) {
	first := run(1, "a", 1, ts(1, 10), 6.00, 0, 0)
	first.FindingsCount = 3
	second := run(2, "a", 1, ts(1, 11), 4.00, 0, 0)
	third := run(3, "a", 1, ts(1, 12), 2.50, 0, 0)

	report := BuildReport([]RunRecord{first, second, third}, time.Time{}, time.Time{}, Options{Now: ts(2, 0)})
	if report.RepeatRun.ZeroFindingRuns != 2 || !nearly(report.RepeatRun.ZeroFindingCostUSD, 6.50) {
		t.Errorf("repeat zero-finding = %d runs / $%.2f, want 2 / $6.50",
			report.RepeatRun.ZeroFindingRuns, report.RepeatRun.ZeroFindingCostUSD)
	}
	if report.FirstRun.ZeroFindingRuns != 0 {
		t.Errorf("first-run zero-finding = %d, want 0", report.FirstRun.ZeroFindingRuns)
	}
}

func TestModelTierSelectsThePricingRow(t *testing.T) {
	runs := []RunRecord{run(1, "a", 1, ts(1, 10), 1.00, 1_000_000, 0)}
	sonnet := BuildReport(runs, time.Time{}, time.Time{}, Options{ModelTier: "sonnet", Now: ts(2, 0)})
	opus := BuildReport(runs, time.Time{}, time.Time{}, Options{ModelTier: "opus", Now: ts(2, 0)})

	if !nearly(sonnet.FirstRun.AttributedCostUSD, PricingForTier("sonnet").CacheWritePerM) {
		t.Errorf("sonnet attributed = %.4f, want the sonnet cache-write rate", sonnet.FirstRun.AttributedCostUSD)
	}
	if sonnet.FirstRun.AttributedCostUSD >= opus.FirstRun.AttributedCostUSD {
		t.Error("opus should price a cache write above sonnet")
	}
}

type stubSource struct {
	runs []RunRecord
	err  error
	// since/until record what the source was asked for.
	since, until time.Time
}

func (s *stubSource) AssayRunHistory(since, until time.Time) ([]RunRecord, error) {
	s.since, s.until = since, until
	return s.runs, s.err
}

func TestReportRepeatCostPassesTheWindowToTheSource(t *testing.T) {
	src := &stubSource{runs: []RunRecord{run(1, "a", 1, ts(5, 10), 2.00, 0, 0)}}
	since, until := ts(5, 0), ts(6, 0)

	report, err := ReportRepeatCost(src, since, until, Options{Now: ts(7, 0)})
	if err != nil {
		t.Fatalf("ReportRepeatCost: %v", err)
	}
	if !src.since.Equal(since) || !src.until.Equal(until) {
		t.Errorf("source was asked for %s..%s, want %s..%s", src.since, src.until, since, until)
	}
	if report.TotalRuns != 1 {
		t.Errorf("runs = %d, want 1", report.TotalRuns)
	}
}

func TestReportRepeatCostSurfacesSourceErrors(t *testing.T) {
	src := &stubSource{err: errors.New("database is locked")}
	if _, err := ReportRepeatCost(src, time.Time{}, time.Time{}, Options{}); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("error %q does not name the cause", err)
	}
	if _, err := ReportRepeatCost(nil, time.Time{}, time.Time{}, Options{}); err == nil {
		t.Fatal("expected an error for a nil source")
	}
}

func TestValidateBaselineReportsTheDelta(t *testing.T) {
	report := &CostReport{RepeatRun: RunGroup{Runs: 780, CostUSD: 2326.54}}

	exact := ValidateBaseline(report, BaselineExpectation{RepeatRuns: 780, RepeatCostUSD: 2326.54})
	if !exact.Matches || exact.RunDelta != 0 || !nearly(exact.CostDeltaUSD, 0) {
		t.Errorf("exact reproduction did not match: %+v", exact)
	}

	// Within the default one-cent tolerance: the published figure is rounded
	// to cents, so anything tighter would fail on rounding alone.
	rounded := ValidateBaseline(&CostReport{RepeatRun: RunGroup{Runs: 780, CostUSD: 2326.5449}},
		BaselineExpectation{RepeatRuns: 780, RepeatCostUSD: 2326.54})
	if !rounded.Matches {
		t.Errorf("a sub-cent difference should still reproduce: %+v", rounded)
	}

	off := ValidateBaseline(&CostReport{RepeatRun: RunGroup{Runs: 37, CostUSD: 261.36}},
		BaselineExpectation{RepeatRuns: 780, RepeatCostUSD: 2326.54})
	if off.Matches {
		t.Error("a different dataset must not report as reproduced")
	}
	if off.RunDelta != -743 || !nearly(off.CostDeltaUSD, -2065.18) {
		t.Errorf("delta = %d runs / $%.2f, want -743 / $-2065.18", off.RunDelta, off.CostDeltaUSD)
	}
}

// The three renderings are three views of one computation, so the machine
// formats must carry the same figures the human table shows.
func TestJSONAndCSVCarryTheSameFiguresAsTheTable(t *testing.T) {
	report := BuildReport([]RunRecord{
		run(1, "a", 1, ts(1, 10), 10.00, 500_000, 100_000),
		run(2, "a", 1, ts(1, 11), 4.50, 20_000, 900_000),
		run(3, "a", 2, ts(1, 12), 6.25, 0, 0),
	}, time.Time{}, time.Time{}, Options{Now: ts(2, 0)})

	var jsonBuf bytes.Buffer
	if err := report.WriteJSON(&jsonBuf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var decoded CostReport
	if err := json.Unmarshal(jsonBuf.Bytes(), &decoded); err != nil {
		t.Fatalf("decoding JSON report: %v", err)
	}
	if decoded.TotalRuns != report.TotalRuns || !nearly(decoded.TotalCostUSD, report.TotalCostUSD) {
		t.Errorf("JSON totals = %d / $%.2f, want %d / $%.2f",
			decoded.TotalRuns, decoded.TotalCostUSD, report.TotalRuns, report.TotalCostUSD)
	}
	if !nearly(decoded.RepeatRun.CostUSD, report.RepeatRun.CostUSD) {
		t.Errorf("JSON repeat cost = %.2f, want %.2f", decoded.RepeatRun.CostUSD, report.RepeatRun.CostUSD)
	}

	var csvBuf bytes.Buffer
	if err := report.WriteCSV(&csvBuf); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	rows, err := csv.NewReader(&csvBuf).ReadAll()
	if err != nil {
		t.Fatalf("parsing CSV report: %v", err)
	}
	if len(rows) < 2 {
		t.Fatal("CSV report has no data rows")
	}
	find := func(section, key string) []string {
		for _, r := range rows[1:] {
			if r[0] == section && r[1] == key {
				return r
			}
		}
		t.Fatalf("CSV has no %s/%s row", section, key)
		return nil
	}
	repeat := find("split", GroupRepeatRun)
	cost, err := strconv.ParseFloat(repeat[4], 64)
	if err != nil {
		t.Fatalf("parsing CSV repeat cost: %v", err)
	}
	if !nearly(cost, report.RepeatRun.CostUSD) {
		t.Errorf("CSV repeat cost = %.4f, want %.4f", cost, report.RepeatRun.CostUSD)
	}
	if runs, _ := strconv.Atoi(repeat[2]); runs != report.RepeatRun.Runs {
		t.Errorf("CSV repeat runs = %d, want %d", runs, report.RepeatRun.Runs)
	}
	unknown := find("token_class", TokenClassUnknown)
	if unknown[5] != BasisRecorded {
		t.Errorf("CSV unknown basis = %q, want %q", unknown[5], BasisRecorded)
	}

	var table bytes.Buffer
	if err := report.WriteTable(&table); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	out := table.String()
	for _, want := range []string{GroupFirstRun, GroupRepeatRun, TokenClassCacheCreation, TokenClassCacheRead} {
		if !strings.Contains(out, want) {
			t.Errorf("table is missing %q", want)
		}
	}
	if !strings.Contains(out, fmt.Sprintf("%.2f", report.RepeatRun.CostUSD)) {
		t.Errorf("table does not show the repeat-run total $%.2f", report.RepeatRun.CostUSD)
	}
	// The caveat travels with the numbers: a priced cache figure read as the
	// total under-reports every run.
	if !strings.Contains(out, "SUBSET") {
		t.Error("table omits the recorded-vs-priced caveat")
	}
}

func TestEmptyWindowRendersRatherThanFailing(t *testing.T) {
	report := BuildReport(nil, ts(1, 0), ts(2, 0), Options{Now: ts(3, 0)})
	if report.TotalRuns != 0 || report.TotalCostUSD != 0 {
		t.Errorf("empty report = %d runs / $%.2f, want 0 / $0", report.TotalRuns, report.TotalCostUSD)
	}
	for _, w := range []func(b *bytes.Buffer) error{
		func(b *bytes.Buffer) error { return report.WriteTable(b) },
		func(b *bytes.Buffer) error { return report.WriteJSON(b) },
		func(b *bytes.Buffer) error { return report.WriteCSV(b) },
	} {
		var buf bytes.Buffer
		if err := w(&buf); err != nil {
			t.Errorf("rendering an empty report failed: %v", err)
		}
		if buf.Len() == 0 {
			t.Error("rendering an empty report produced nothing")
		}
	}
}

func nearly(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 0.0001
}

// An open bound must marshal as absent, not as the zero time: a JSON report
// reading "until": "0001-01-01" says the window closed before the data began,
// which is the opposite of what an unbounded report means.
func TestOpenWindowBoundsAreAbsentFromJSON(t *testing.T) {
	report := BuildReport(nil, time.Time{}, time.Time{}, Options{Now: ts(1, 0)})
	var buf bytes.Buffer
	if err := report.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if _, ok := raw["since"]; ok {
		t.Errorf("open lower bound serialized as %v, want absent", raw["since"])
	}
	if _, ok := raw["until"]; ok {
		t.Errorf("open upper bound serialized as %v, want absent", raw["until"])
	}

	bounded := BuildReport(nil, ts(1, 0), ts(2, 0), Options{Now: ts(3, 0)})
	var bbuf bytes.Buffer
	if err := bounded.WriteJSON(&bbuf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if err := json.Unmarshal(bbuf.Bytes(), &raw); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if _, ok := raw["since"]; !ok {
		t.Error("a real lower bound is missing from the JSON report")
	}
	// The human table names an open bound rather than printing a zero time.
	var table bytes.Buffer
	if err := report.WriteTable(&table); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	if !strings.Contains(table.String(), "(open) .. (open)") {
		t.Error("table does not name the open window bounds")
	}
}
