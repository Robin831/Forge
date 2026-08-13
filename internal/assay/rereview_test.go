package assay

// Tests for the repeat-review tightening: cumulative per-PR budgets, cross-run
// far-anchor suppression, prior-findings prompt injection, the incremental
// framing, summary upsert, and the resolved-hash posting guard.

import (
	"context"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/state"
)

// --- cumulative budgets ----------------------------------------------------

func TestCapNitsBudgetZeroDropsAllNits(t *testing.T) {
	in := []Finding{
		{Hash: "1", Severity: SeverityNit},
		{Hash: "2", Severity: SeverityImportant},
		{Hash: "3", Severity: SeverityNit},
	}
	out, dropped := capNitsBudget(in, 0)
	if dropped != 2 {
		t.Errorf("budget 0 should drop every nit, dropped %d", dropped)
	}
	if len(out) != 1 || out[0].Severity != SeverityImportant {
		t.Errorf("budget 0 should keep only non-nits, got %+v", out)
	}
}

func TestCapNitsBudgetNegativeMeansUnlimited(t *testing.T) {
	in := []Finding{{Severity: SeverityNit}, {Severity: SeverityNit}}
	out, dropped := capNitsBudget(in, -1)
	if dropped != 0 || len(out) != 2 {
		t.Errorf("budget < 0 should keep all nits; got %d kept, %d dropped", len(out), dropped)
	}
}

func TestCapTotalFindingsDropsLowestSeverityFromEnd(t *testing.T) {
	in := []Finding{
		{Hash: "imp1", Severity: SeverityImportant},
		{Hash: "nit1", Severity: SeverityNit},
		{Hash: "pre1", Severity: SeverityPreExisting},
		{Hash: "imp2", Severity: SeverityImportant},
		{Hash: "nit2", Severity: SeverityNit},
	}
	out, dropped := capTotalFindings(in, 3)
	if dropped != 2 {
		t.Fatalf("expected 2 dropped, got %d", dropped)
	}
	var hashes []string
	for _, f := range out {
		hashes = append(hashes, f.Hash)
	}
	// Both nits go before the PreExisting does; Importants always survive at
	// this budget; original order is preserved.
	want := []string{"imp1", "pre1", "imp2"}
	if strings.Join(hashes, ",") != strings.Join(want, ",") {
		t.Errorf("expected %v, got %v", want, hashes)
	}
}

func TestCapTotalFindingsCapsImportantWhenBudgetDemands(t *testing.T) {
	in := []Finding{
		{Hash: "imp1", Severity: SeverityImportant},
		{Hash: "imp2", Severity: SeverityImportant},
		{Hash: "imp3", Severity: SeverityImportant},
	}
	out, dropped := capTotalFindings(in, 1)
	if dropped != 2 || len(out) != 1 || out[0].Hash != "imp1" {
		t.Errorf("expected only imp1 to survive budget 1, got %+v (dropped %d)", out, dropped)
	}
}

func TestCapTotalFindingsNegativeMeansUnlimited(t *testing.T) {
	in := []Finding{{Severity: SeverityImportant}, {Severity: SeverityNit}}
	out, dropped := capTotalFindings(in, -1)
	if dropped != 0 || len(out) != 2 {
		t.Errorf("budget < 0 should keep everything; got %d kept, %d dropped", len(out), dropped)
	}
}

// TestReviewCumulativeNitBudget: nit_cap is a per-PR budget, not per-run. Two
// open Nits from an earlier review leave a NitCap of 3 with room for exactly
// one more.
func TestReviewCumulativeNitBudget(t *testing.T) {
	db := openTestDB(t)
	for i, hash := range []string{"prior-nit-1", "prior-nit-2"} {
		if err := db.InsertFinding(state.Finding{
			Anvil: "demo", PRNumber: 7, HeadSHA: "old", FindingHash: hash,
			File: "z.go", Anchor: "z.go:" + string(rune('1'+i)),
			Category: "conventions", Severity: string(SeverityNit),
			Title: "prior nit", Body: "polish",
		}); err != nil {
			t.Fatalf("seed prior nit: %v", err)
		}
	}

	var nits []Finding
	for _, a := range []string{"a.go:1", "a.go:2", "a.go:3"} {
		nits = append(nits, Finding{
			File: "a.go", Anchor: a, Category: "conventions",
			Severity: SeverityNit, Title: "nit", Body: "tidy",
		})
	}
	deep := map[string]string{"conventions": findingsJSON(t, nits)}
	cfg := DefaultConfig().WithRunner(newScriptRunner(baseScript(triageJSON(t, nil, ""), deep)).run)
	cfg.NitCap = 3

	res, err := Review(context.Background(), testRequest(), db, cfg)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(res.Findings) != 1 {
		t.Errorf("expected 1 nit within the cumulative budget, got %d", len(res.Findings))
	}
	if res.NitsCapped != 2 {
		t.Errorf("expected 2 nits capped, got %d", res.NitsCapped)
	}
}

// TestReviewCumulativeTotalBudget: the total findings cap counts what earlier
// reviews already contributed, Important findings included.
func TestReviewCumulativeTotalBudget(t *testing.T) {
	db := openTestDB(t)
	if err := db.InsertFinding(state.Finding{
		Anvil: "demo", PRNumber: 7, HeadSHA: "old", FindingHash: "prior-imp",
		File: "z.go", Anchor: "z.go:1", Category: "logic",
		Severity: string(SeverityImportant), Title: "prior", Body: "boom",
	}); err != nil {
		t.Fatalf("seed prior finding: %v", err)
	}

	var imps []Finding
	for _, a := range []string{"a.go:1", "a.go:2", "a.go:3"} {
		imps = append(imps, Finding{
			File: "a.go", Anchor: a, Category: "logic",
			Severity: SeverityImportant, Title: "imp", Body: "crash",
		})
	}
	deep := map[string]string{"logic": findingsJSON(t, imps)}
	cfg := DefaultConfig().WithRunner(newScriptRunner(baseScript(triageJSON(t, nil, ""), deep)).run)
	cfg.MaxFindingsPerPR = 3

	res, err := Review(context.Background(), testRequest(), db, cfg)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(res.Findings) != 2 {
		t.Errorf("expected 2 findings within the cumulative total budget, got %d", len(res.Findings))
	}
	if res.TotalCapped != 1 {
		t.Errorf("expected TotalCapped=1, got %d", res.TotalCapped)
	}
}

// --- cross-run far-anchor suppression --------------------------------------

const farDriftBodyA = "The cache key omits the tenant identifier so cached responses leak across tenants when requests interleave."
const farDriftBodyB = "Cache key omits the tenant identifier, so cached responses leak across tenants whenever requests interleave."

func TestSuppressSimilarToExistingDropsFarDriftSameCategory(t *testing.T) {
	existing := []ExistingFinding{
		{Anchor: "api/cache.go:12", Body: farDriftBodyA, Category: "security"},
	}
	newFindings := []Finding{
		{Anchor: "api/cache.go:180", Category: "security", Severity: SeverityImportant,
			Title: "drifted rewording", Body: farDriftBodyB},
	}

	out := suppressSimilarToExisting(newFindings, existing)

	if len(out) != 0 {
		t.Errorf("expected far-drifted same-category paraphrase to be suppressed; got %d", len(out))
	}
}

func TestSuppressSimilarToExistingKeepsFarDriftDifferentCategory(t *testing.T) {
	existing := []ExistingFinding{
		{Anchor: "api/cache.go:12", Body: farDriftBodyA, Category: "security"},
	}
	newFindings := []Finding{
		{Anchor: "api/cache.go:180", Category: "logic", Severity: SeverityImportant,
			Title: "different pass, far line", Body: farDriftBodyB},
	}

	out := suppressSimilarToExisting(newFindings, existing)

	if len(out) != 1 {
		t.Errorf("expected far pair with different categories to survive; got %d", len(out))
	}
}

// TestReviewSuppressesResurrectionOfResolvedFinding: a finding whose thread was
// auto-resolved must suppress a reworded regeneration of itself — resolved
// findings used to be excluded from cross-run dedup, so they reappeared as
// brand-new comments.
func TestReviewSuppressesResurrectionOfResolvedFinding(t *testing.T) {
	db := openTestDB(t)
	if err := db.InsertFinding(state.Finding{
		Anvil: "demo", PRNumber: 7, HeadSHA: "old", FindingHash: "resolved-1",
		File: "api/cache.go", Anchor: "api/cache.go:12", Category: "security",
		Severity: string(SeverityImportant), Title: "cache", Body: farDriftBodyA,
		Posted: true,
	}); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	if err := db.MarkResolved("resolved-1"); err != nil {
		t.Fatalf("resolve finding: %v", err)
	}

	deep := map[string]string{"security": findingsJSON(t, []Finding{
		{File: "api/cache.go", Anchor: "api/cache.go:14", Category: "security",
			Severity: SeverityImportant, Title: "reworded", Body: farDriftBodyB},
	})}
	cfg := DefaultConfig().WithRunner(newScriptRunner(baseScript(triageJSON(t, nil, ""), deep)).run)

	res, err := Review(context.Background(), testRequest(), db, cfg)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("expected resolved finding's rewording to be suppressed, got %d findings", len(res.Findings))
	}
}

// --- prompt injection ------------------------------------------------------

func TestReviewInjectsPriorFindingsIntoPrompts(t *testing.T) {
	db := openTestDB(t)
	if err := db.InsertFinding(state.Finding{
		Anvil: "demo", PRNumber: 7, HeadSHA: "old", FindingHash: "prior-1",
		File: "z.go", Anchor: "z.go:42", Category: "logic",
		Severity: string(SeverityImportant), Title: "off-by-one in pager", Body: "boom",
	}); err != nil {
		t.Fatalf("seed finding: %v", err)
	}

	runner := newScriptRunner(baseScript(triageJSON(t, nil, ""), nil))
	cfg := DefaultConfig().WithRunner(runner.run)
	if _, err := Review(context.Background(), testRequest(), db, cfg); err != nil {
		t.Fatalf("Review: %v", err)
	}

	for _, pass := range []string{passTriage.Name, "logic"} {
		calls := runner.callsFor(pass)
		if len(calls) == 0 {
			t.Fatalf("pass %s never ran", pass)
		}
		prompt := calls[0].prompt
		if !strings.Contains(prompt, "Already-Reported Findings") {
			t.Errorf("pass %s prompt missing already-reported section", pass)
		}
		if !strings.Contains(prompt, "off-by-one in pager") || !strings.Contains(prompt, "z.go:42") {
			t.Errorf("pass %s prompt missing the prior finding's title/anchor", pass)
		}
	}
}

func TestPriorFindingsSectionMarksResolvedAndTruncates(t *testing.T) {
	req := ReviewRequest{PriorFindings: []PriorFinding{
		{Anchor: "a.go:1", Severity: "Important", Title: "open one"},
		{Anchor: "b.go:2", Severity: "Nit", Title: "closed one", Resolved: true},
	}}
	got := priorFindingsSection(req)
	if !strings.Contains(got, "open one") || !strings.Contains(got, "closed one") ||
		!strings.Contains(got, "`b.go:2` (resolved)") {
		t.Errorf("section missing entries or resolved tag:\n%s", got)
	}

	var many []PriorFinding
	for i := 0; i < maxPriorFindingsListed+7; i++ {
		many = append(many, PriorFinding{Anchor: "a.go:1", Severity: "Nit", Title: "x"})
	}
	got = priorFindingsSection(ReviewRequest{PriorFindings: many})
	if !strings.Contains(got, "and 7 more") {
		t.Errorf("section should note the 7 unlisted findings:\n%s", got)
	}
}

func TestIncrementalSectionNamesBaseline(t *testing.T) {
	if got := incrementalSection(ReviewRequest{}); got != "" {
		t.Errorf("non-incremental request should render nothing, got %q", got)
	}
	got := incrementalSection(ReviewRequest{Incremental: true, BaselineSHA: "0123456789abcdef"})
	if !strings.Contains(got, "Incremental Review") || !strings.Contains(got, "0123456789ab") {
		t.Errorf("incremental section missing framing or short baseline:\n%s", got)
	}
}

func TestBuildPassPromptCarriesIncrementalFraming(t *testing.T) {
	req := testRequest()
	req.Incremental = true
	req.BaselineSHA = "cafebabe"
	req.PriorFindings = []PriorFinding{{Anchor: "a.go:1", Severity: "Nit", Title: "old nit"}}

	prompt, err := buildPassPrompt(deepPasses[0], req, req.Diff, "")
	if err != nil {
		t.Fatalf("buildPassPrompt: %v", err)
	}
	for _, want := range []string{"Incremental Review", "cafebabe", "Already-Reported Findings", "old nit"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// --- summary upsert --------------------------------------------------------

func TestPostSummaryCreateThenUpdate(t *testing.T) {
	gh := newStubGh()
	p := newPoster(nil, gh.exec, nil)
	req := PostRequest{PRNumber: 7, WorktreePath: "/wt", SummaryLine: "Assay (AI review)"}

	res, err := p.Post(context.Background(), Config{}, req)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if !res.SummaryPosted || res.SummaryUpdated {
		t.Fatalf("first post should create, got %+v", res)
	}
	if !strings.Contains(gh.summaryBody(), summaryMarker) {
		t.Error("summary body must open with the upsert marker")
	}

	// Second run: the PR now carries a marked summary comment; expect a PATCH.
	gh2 := newStubGh()
	gh2.existingComments = []byte(`[{"id": 9001, "body": "` + summaryMarker + ` old body"}]`)
	p2 := newPoster(nil, gh2.exec, nil)
	res2, err := p2.Post(context.Background(), Config{}, req)
	if err != nil {
		t.Fatalf("Post (update): %v", err)
	}
	if !res2.SummaryPosted || !res2.SummaryUpdated {
		t.Fatalf("second post should edit in place, got %+v", res2)
	}
	edited := false
	for _, c := range gh2.calls {
		for _, a := range c.args {
			if strings.Contains(a, "/issues/comments/9001") {
				edited = true
			}
		}
	}
	if !edited {
		t.Error("expected PATCH against the existing summary comment id")
	}
}

// --- resolved-hash posting guard -------------------------------------------

func TestPostSkipsResolvedFinding(t *testing.T) {
	db := newTestDB(t)
	gh := newStubGh()
	p := newPoster(db, gh.exec, nil)

	f := Finding{File: "a.go", Anchor: "a.go:3", Hash: "res1", Severity: SeverityImportant, Title: "was resolved"}
	mustInsert(t, db, "anvil", 5, f)
	mustMarkPosted(t, db, "res1", 111)
	if err := db.MarkResolved("res1"); err != nil {
		t.Fatalf("MarkResolved: %v", err)
	}

	res, err := p.Post(context.Background(), Config{}, PostRequest{
		Anvil: "anvil", PRNumber: 5, HeadSHA: "sha5", WorktreePath: "/wt",
		SummaryLine: "s", Findings: []Finding{f},
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if res.Posted != 0 {
		t.Errorf("resolved finding must not be re-posted, got Posted=%d", res.Posted)
	}
	if gh.inlineCalls() != 0 {
		t.Errorf("expected no inline gh call for a resolved finding, got %d", gh.inlineCalls())
	}
}
