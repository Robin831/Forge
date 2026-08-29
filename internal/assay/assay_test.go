package assay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/state"
)

// --- test helpers ---------------------------------------------------------

// stubResp is one scripted runner response.
type stubResp struct {
	text  string
	err   error
	turns int
	cost  float64
	// estCost is costTracker's running estimate for the session — the unit
	// assay.max_cost_per_pass_usd is compared against, and a different quantity
	// from cost, which is the provider's own billed total. It has its own
	// channel here for the same reason it has its own field on PassOutput: a
	// test cannot otherwise tell the two apart on the rendered line. Set it on
	// an err response through PassError.EstCostUSD, which is the carrier a
	// failed session has.
	estCost float64
	// cacheW and cacheR are the session's prompt-cache write/read accounting,
	// which the pass telemetry sums over every session a pass made — unlike
	// turns, which records the final session only.
	cacheW int
	cacheR int
	// opened is what the session reports having read, as the real runner
	// reports it off the provider's tool-use events. On a turn-budget failure
	// it is what scopes the retry's diff; set it on err responses via
	// PassError.OpenedFiles instead, which is the carrier a failed session has.
	opened []string
	// toolCalls is how many tool calls the session made, which a pass sums over
	// its sessions while folding opened into a deduplicated union — two
	// conventions that a scripted runner has to be able to produce separately,
	// or no Review-level test can tell a sum from an assignment. Like opened it
	// has no channel on an err response: set PassError.ToolCalls there.
	toolCalls int
	// tokensIn and tokensOut are the session's plain token counts, summed the
	// same way — they are what the run's usage carries to the cost tables.
	tokensIn  int
	tokensOut int
}

// stubCall records one invocation of the scripted runner, so a test can assert
// how many sessions a pass took and how the retry's request differed from the
// one before it.
type stubCall struct {
	pass   string
	prompt string
	// turnBudget is the --max-turns the session was given, read off the
	// context the way the real runner reads it. Zero when the caller set none.
	turnBudget int
	// openedFiles is what the scripted response says the session read, echoed
	// here so a test can assert what fed the retry's scoping decision.
	openedFiles []string
}

// scriptRunner is a deterministic PassRunner: each pass name has an ordered
// list of responses; the final response repeats for any further calls.
type scriptRunner struct {
	mu     sync.Mutex
	script map[string][]stubResp
	idx    map[string]int
	calls  []stubCall
}

func newScriptRunner(script map[string][]stubResp) *scriptRunner {
	return &scriptRunner{script: script, idx: map[string]int{}}
}

func (r *scriptRunner) run(ctx context.Context, pass, _, prompt string) (PassOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, stubCall{pass: pass, prompt: prompt, turnBudget: turnBudgetFrom(ctx)})
	seq := r.script[pass]
	if len(seq) == 0 {
		// Default: no findings.
		return PassOutput{Text: `{"findings": []}`}, nil
	}
	i := r.idx[pass]
	if i >= len(seq) {
		i = len(seq) - 1
	}
	r.idx[pass]++
	resp := seq[i]
	if resp.err != nil {
		// A failed session's cost rides on the error, exactly as the real
		// runner reports it — the provider bills an error_max_turns session
		// like any other, so resp.cost has no separate channel here.
		return PassOutput{}, resp.err
	}
	return PassOutput{
		Text:                resp.text,
		Turns:               resp.turns,
		CostUSD:             resp.cost,
		EstCostUSD:          resp.estCost,
		TokensIn:            resp.tokensIn,
		TokensOut:           resp.tokensOut,
		CacheCreationTokens: resp.cacheW,
		CacheReadTokens:     resp.cacheR,
		OpenedFiles:         resp.opened,
		ToolCalls:           resp.toolCalls,
	}, nil
}

// callsFor returns the recorded invocations for one pass, in order.
func (r *scriptRunner) callsFor(pass string) []stubCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []stubCall
	for _, c := range r.calls {
		if c.pass == pass {
			out = append(out, c)
		}
	}
	return out
}

// passReport returns the named pass's report from a result, or nil.
func passReport(res *ReviewResult, name string) *PassReport {
	for i := range res.Passes {
		if res.Passes[i].Name == name {
			return &res.Passes[i]
		}
	}
	return nil
}

func findingsJSON(t *testing.T, fs []Finding) string {
	t.Helper()
	b, err := json.Marshal(findingsEnvelope{Findings: fs})
	if err != nil {
		t.Fatalf("marshal findings: %v", err)
	}
	return string(b)
}

func triageJSON(t *testing.T, files []string, notes string) string {
	t.Helper()
	b, err := json.Marshal(triageResult{ReviewFiles: files, Notes: notes})
	if err != nil {
		t.Fatalf("marshal triage: %v", err)
	}
	return string(b)
}

// allPassScript returns a script where triage scopes nothing and every named
// deep pass produces the given JSON (others produce empty findings).
func baseScript(triage string, deep map[string]string) map[string][]stubResp {
	s := map[string][]stubResp{
		passTriage.Name: {{text: triage}},
	}
	for _, p := range deepPasses {
		if out, ok := deep[p.Name]; ok {
			s[p.Name] = []stubResp{{text: out}}
		}
	}
	return s
}

func openTestDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func countFindings(t *testing.T, db *state.DB, anvil string, pr int) int {
	t.Helper()
	var n int
	if err := db.Conn().QueryRow(
		`SELECT COUNT(*) FROM pr_findings WHERE anvil = ? AND pr_number = ?`,
		anvil, pr,
	).Scan(&n); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	return n
}

func testRequest() ReviewRequest {
	return ReviewRequest{
		Anvil:    "demo",
		PRNumber: 7,
		HeadSHA:  "abc123",
		Title:    "Add feature",
		Diff:     "diff --git a/main.go b/main.go\n@@\n+x := 1\n",
	}
}

// --- hashing & canonicalization ------------------------------------------

func TestComputeHashStableAcrossCosmeticBodyChanges(t *testing.T) {
	h1 := computeHash("demo", 7, "main.go:10", "logic", "Off-by-one in the loop bound.")
	h2 := computeHash("demo", 7, "main.go:10", "logic", "  off-by-one   in the LOOP bound.  ")
	if h1 != h2 {
		t.Fatalf("expected identical hashes for cosmetically different bodies; got %s vs %s", h1, h2)
	}
}

func TestComputeHashDiffersByAnchorAndCategory(t *testing.T) {
	base := computeHash("demo", 7, "main.go:10", "logic", "issue")
	if base == computeHash("demo", 7, "main.go:11", "logic", "issue") {
		t.Error("hash should change with anchor")
	}
	if base == computeHash("demo", 7, "main.go:10", "security", "issue") {
		t.Error("hash should change with category")
	}
	if base == computeHash("demo", 7, "main.go:10", "logic", "different") {
		t.Error("hash should change with body")
	}
}

func TestComputeHashDiffersByAnvilAndPR(t *testing.T) {
	base := computeHash("demo", 7, "main.go:10", "logic", "issue")
	if base == computeHash("other-repo", 7, "main.go:10", "logic", "issue") {
		t.Error("hash should change with anvil")
	}
	if base == computeHash("demo", 99, "main.go:10", "logic", "issue") {
		t.Error("hash should change with PR number")
	}
}

func TestNormalizeSeverity(t *testing.T) {
	cases := map[string]Severity{
		"Important":    SeverityImportant,
		"critical":     SeverityImportant,
		"Nit":          SeverityNit,
		"suggestion":   SeverityNit,
		"PreExisting":  SeverityPreExisting,
		"pre-existing": SeverityPreExisting,
		"weird-value":  SeverityNit, // unknown -> Nit
		"":             SeverityNit,
	}
	for in, want := range cases {
		if got := normalizeSeverity(Severity(in)); got != want {
			t.Errorf("normalizeSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- aggregation ----------------------------------------------------------

// The two bodies below are paraphrased from a real Munin Assay run where
// tests-missing and the logic pass independently flagged the same untested
// code path with different category labels. Used by multiple similarity tests
// below to assert dedup behavior on realistic input.
const realParaphraseBodyA = "This change moves IDbContextFactory<CatalogDbContext> resolution out of the constructor and into StampCrossDatabase/StampCrossDatabaseAsync via scopeFactory.CreateScope()/CreateAsyncScope(). The only integration test explicitly documents that it drives Kilde/Variabel edits which map to ForKilde — TryBuildCrossDatabaseStamp returns false for those, so neither stamp method ever runs (allKilder/profileIds path requires a Rule or RuleProfileAssignment trigger). So the exact lines that changed have zero coverage. Add a regression test that saves a Rule or RuleProfileAssignment change through the interceptor-bearing context and asserts the affected KildeGrade.GradeInputsChangedAt rows are stamped."
const realParaphraseBodyB = "The substance of this diff is the new lazy resolution inside StampCrossDatabase and StampCrossDatabaseAsync: create a scope and resolve IDbContextFactory<CatalogDbContext> from it. The DI-cycle break at the constructor level is exercised, but the modified lines themselves never run because the cross-DB stamp only fires on a Rule or RuleProfileAssignment change. Add an integration test that saves a Rule or RuleProfileAssignment change through the interceptor-bearing context and asserts the affected catalog.KildeGrades rows get GradeInputsChangedAt stamped."

func TestDedupeBySimilarityCollapsesRealParaphrases(t *testing.T) {
	anchor := "api/X.cs:204"
	findings := []Finding{
		{Anchor: anchor, Category: "missing-test", Severity: SeverityImportant, Title: "no test", Body: realParaphraseBodyA},
		{Anchor: anchor, Category: "untested-path", Severity: SeverityImportant, Title: "untested", Body: realParaphraseBodyB},
	}

	out := dedupeBySimilarity(findings)

	if len(out) != 1 {
		t.Fatalf("expected paraphrases on the same anchor to collapse to 1; got %d", len(out))
	}
}

func TestDedupeBySimilarityKeepsDistinctConcernsOnSameAnchor(t *testing.T) {
	anchor := "src/cart.go:42"
	findings := []Finding{
		{Anchor: anchor, Category: "logic", Severity: SeverityImportant, Title: "off-by-one",
			Body: "The loop bound iterates one element past the end of the slice, dereferencing memory outside the cart contents."},
		{Anchor: anchor, Category: "security", Severity: SeverityImportant, Title: "nil",
			Body: "There is no guard against a nil customer record, so the audit log entry below dereferences a null pointer when the request is anonymous."},
	}

	out := dedupeBySimilarity(findings)

	if len(out) != 2 {
		t.Errorf("expected distinct concerns on same anchor to survive; got %d", len(out))
	}
}

// Adjacent lines on the same file no longer survive when bodies are
// essentially identical — the calibration data (Munin PRs #3514, #3523)
// showed the model emitting the same observation as separate findings at
// adjacent statement boundaries inside one method. Different FILES still
// survive (see TestDedupeBySimilarityKeepsDifferentFiles below).
func TestDedupeBySimilarityCollapsesAdjacentLinesOnSameFile(t *testing.T) {
	findings := []Finding{
		{Anchor: "a.go:10", Category: "logic", Severity: SeverityImportant, Title: "x", Body: realParaphraseBodyA},
		{Anchor: "a.go:11", Category: "logic", Severity: SeverityImportant, Title: "x", Body: realParaphraseBodyA},
	}

	out := dedupeBySimilarity(findings)

	if len(out) != 1 {
		t.Errorf("expected adjacent same-file paraphrases to collapse; got %d", len(out))
	}
}

func TestDedupeBySimilarityKeepsDifferentFiles(t *testing.T) {
	findings := []Finding{
		{Anchor: "a.go:10", Category: "logic", Severity: SeverityImportant, Title: "x", Body: realParaphraseBodyA},
		{Anchor: "b.go:10", Category: "logic", Severity: SeverityImportant, Title: "x", Body: realParaphraseBodyA},
	}

	out := dedupeBySimilarity(findings)

	if len(out) != 2 {
		t.Errorf("expected different-file findings to survive even when bodies are identical; got %d", len(out))
	}
}

// Real Munin pattern from PR #3514: three paraphrases of the same
// AsyncLocal-restore concern at adjacent lines 60, 61, 62 of
// RecomputeSqlProfiler.cs. Should collapse to one.
func TestDedupeBySimilarityCollapsesPR3514ThreeWayParaphrase(t *testing.T) {
	body := "Scope.Dispose clears AsyncLocal to null instead of restoring the previous scope, breaking nested-scope semantics in the RecomputeSqlProfiler."
	file := "api/Fhi.Metadata.Api/Service/Catalog/Grading/RecomputeSqlProfiler.cs"
	findings := []Finding{
		{Anchor: file + ":60", Category: "concurrency", Severity: SeverityNit, Title: "Scope.Dispose clears AsyncLocal", Body: body},
		{Anchor: file + ":61", Category: "maintainability", Severity: SeverityNit, Title: "Scope.Dispose clears AsyncLocal", Body: body},
		{Anchor: file + ":62", Category: "concurrency", Severity: SeverityNit, Title: "Scope.Dispose clears AsyncLocal", Body: body},
	}

	out := dedupeBySimilarity(findings)

	if len(out) != 1 {
		t.Errorf("expected PR #3514 three-way paraphrase to collapse to 1; got %d", len(out))
	}
}

// Lines further apart than nearAnchorMaxLineDistance on the same file must
// survive — large gaps usually mean genuinely distinct concerns. This guards
// against the model emitting one Nit at the top of a 500-line file and
// another at the bottom and having them silently merged.
func TestDedupeBySimilarityKeepsFarLinesOnSameFile(t *testing.T) {
	findings := []Finding{
		{Anchor: "a.go:10", Category: "logic", Severity: SeverityImportant, Title: "x", Body: realParaphraseBodyA},
		{Anchor: "a.go:" + strconv.Itoa(10+nearAnchorMaxLineDistance+5), Category: "logic", Severity: SeverityImportant, Title: "x", Body: realParaphraseBodyA},
	}

	out := dedupeBySimilarity(findings)

	if len(out) != 2 {
		t.Errorf("expected far-apart same-file findings to survive; got %d", len(out))
	}
}

// Near-anchor regime uses a stricter threshold than exact-anchor — verify
// that a pair barely over the exact-anchor threshold but under the
// near-anchor threshold survives when anchors differ.
func TestDedupeBySimilarityNearAnchorThresholdIsStricter(t *testing.T) {
	// Bodies share enough words to clear similarityDedupeThreshold (0.4) but
	// not enough to clear sameFileNearAnchorThreshold (0.55).
	bodyA := "The recompute path swallows OperationCanceledException and reports a misleading commandtimeout warning during contention windows."
	bodyB := "OperationCanceledException is treated as a contention warning when it should be propagated; the recompute timeout configuration is unrelated."

	// Same anchor — clears 0.4, collapses.
	sameAnchor := []Finding{
		{Anchor: "a.go:10", Category: "x", Severity: SeverityNit, Title: "a", Body: bodyA},
		{Anchor: "a.go:10", Category: "x", Severity: SeverityNit, Title: "b", Body: bodyB},
	}
	if got := dedupeBySimilarity(sameAnchor); len(got) != 1 {
		t.Errorf("same-anchor pair below stricter threshold should still collapse at 0.4; got %d", len(got))
	}

	// Different line, same file — must clear 0.55 to collapse; this pair
	// should survive because the bodies share the framing but disagree on
	// the diagnosis.
	nearAnchor := []Finding{
		{Anchor: "a.go:10", Category: "x", Severity: SeverityNit, Title: "a", Body: bodyA},
		{Anchor: "a.go:14", Category: "x", Severity: SeverityNit, Title: "b", Body: bodyB},
	}
	if got := dedupeBySimilarity(nearAnchor); len(got) != 2 {
		t.Errorf("near-anchor pair under the stricter threshold should survive; got %d", len(got))
	}
}

func TestParseAnchorHandlesRangesAndMethodAnchors(t *testing.T) {
	cases := []struct {
		in       string
		wantFile string
		wantLine int
	}{
		{"src/foo.go:42", "src/foo.go", 42},
		{"src/foo.go:42-58", "src/foo.go", 42},
		{"src/foo.go:MethodName", "src/foo.go", -1},
		{"src/foo.go", "src/foo.go", -1},
		{"  src/foo.go:7  ", "src/foo.go", 7},
		{"", "", -1},
	}
	for _, tc := range cases {
		got := parseAnchor(tc.in)
		if got.file != tc.wantFile || got.line != tc.wantLine {
			t.Errorf("parseAnchor(%q) = {%q, %d}; want {%q, %d}", tc.in, got.file, got.line, tc.wantFile, tc.wantLine)
		}
	}
}

func TestDedupeBySimilarityPrefersHigherSeverity(t *testing.T) {
	anchor := "api/X.cs:204"
	findings := []Finding{
		{Anchor: anchor, Category: "missing-test", Severity: SeverityNit, Title: "nit", Body: realParaphraseBodyA},
		{Anchor: anchor, Category: "untested-path", Severity: SeverityImportant, Title: "imp", Body: realParaphraseBodyB},
	}

	out := dedupeBySimilarity(findings)

	if len(out) != 1 {
		t.Fatalf("expected paraphrases to collapse; got %d", len(out))
	}
	if out[0].Severity != SeverityImportant {
		t.Errorf("expected Important to survive over Nit; got %s", out[0].Severity)
	}
}

func TestSuppressSimilarToExistingDropsParaphraseAtSameAnchor(t *testing.T) {
	anchor := "api/X.cs:172"
	existing := []ExistingFinding{
		{Anchor: anchor, Body: realParaphraseBodyA},
	}
	newFindings := []Finding{
		{Anchor: anchor, Category: "missing-test", Severity: SeverityImportant, Title: "rewording", Body: realParaphraseBodyB},
	}

	out := suppressSimilarToExisting(newFindings, existing)

	if len(out) != 0 {
		t.Errorf("expected reworded paraphrase to be suppressed against existing; got %d", len(out))
	}
}

func TestSuppressSimilarToExistingKeepsDistinctConcernAtSameAnchor(t *testing.T) {
	anchor := "src/cart.go:42"
	existing := []ExistingFinding{
		{Anchor: anchor, Body: "The loop bound iterates one element past the end of the slice, dereferencing memory outside the cart contents."},
	}
	newFindings := []Finding{
		{Anchor: anchor, Category: "security", Severity: SeverityImportant, Title: "nil",
			Body: "There is no guard against a nil customer record, so the audit log entry below dereferences a null pointer when the request is anonymous."},
	}

	out := suppressSimilarToExisting(newFindings, existing)

	if len(out) != 1 {
		t.Errorf("expected distinct concern at same anchor to survive; got %d", len(out))
	}
}

func TestSuppressSimilarToExistingKeepsDifferentAnchor(t *testing.T) {
	existing := []ExistingFinding{
		{Anchor: "api/X.cs:204", Body: realParaphraseBodyA},
	}
	newFindings := []Finding{
		{Anchor: "api/X.cs:172", Category: "missing-test", Severity: SeverityImportant, Title: "at different line", Body: realParaphraseBodyB},
	}

	out := suppressSimilarToExisting(newFindings, existing)

	if len(out) != 1 {
		t.Errorf("expected finding at a different anchor to survive even when body matches; got %d", len(out))
	}
}

func TestSuppressSimilarToExistingDropsParaphraseAtNearbyLine(t *testing.T) {
	file := "api/X.cs"
	existing := []ExistingFinding{
		{Anchor: file + ":172", Body: realParaphraseBodyA},
	}
	newFindings := []Finding{
		{Anchor: file + ":178", Category: "missing-test", Severity: SeverityImportant, Title: "rewording at drift", Body: realParaphraseBodyB},
	}

	out := suppressSimilarToExisting(newFindings, existing)

	if len(out) != 0 {
		t.Errorf("expected reworded paraphrase at nearby line to be suppressed against existing; got %d", len(out))
	}
}

func TestSuppressSimilarToExistingKeepsFarLineOnSameFile(t *testing.T) {
	file := "api/X.cs"
	existing := []ExistingFinding{
		{Anchor: file + ":10", Body: realParaphraseBodyA},
	}
	newFindings := []Finding{
		{Anchor: file + ":" + strconv.Itoa(10+nearAnchorMaxLineDistance+5), Category: "logic", Severity: SeverityImportant, Title: "far-apart same-file", Body: realParaphraseBodyB},
	}

	out := suppressSimilarToExisting(newFindings, existing)

	if len(out) != 1 {
		t.Errorf("expected far-apart same-file finding to survive even when body matches; got %d", len(out))
	}
}

func TestSuppressSimilarToExistingNoOpOnEmptyExisting(t *testing.T) {
	newFindings := []Finding{
		{Anchor: "a.go:1", Category: "logic", Severity: SeverityImportant, Title: "x", Body: realParaphraseBodyA},
	}

	out := suppressSimilarToExisting(newFindings, nil)

	if len(out) != 1 {
		t.Errorf("expected pass-through with nil existing; got %d", len(out))
	}
}

func TestDedupeBySimilaritySkipsTinyBodies(t *testing.T) {
	anchor := "a.go:1"
	findings := []Finding{
		{Anchor: anchor, Category: "logic", Severity: SeverityImportant, Title: "x", Body: "no test"},
		{Anchor: anchor, Category: "tests", Severity: SeverityNit, Title: "y", Body: "no test"},
	}

	out := dedupeBySimilarity(findings)

	if len(out) != 2 {
		t.Errorf("expected very short bodies to skip similarity dedup; got %d", len(out))
	}
}

func TestDedupeByHash(t *testing.T) {
	in := []Finding{
		{Hash: "a"}, {Hash: "b"}, {Hash: "a"}, {Hash: ""}, {Hash: "c"}, {Hash: "b"},
	}
	out := dedupeByHash(in)
	if len(out) != 3 {
		t.Fatalf("expected 3 unique findings, got %d", len(out))
	}
	if out[0].Hash != "a" || out[1].Hash != "b" || out[2].Hash != "c" {
		t.Errorf("dedupe did not preserve first-seen order: %+v", out)
	}
}

func TestCapNitsKeepsImportant(t *testing.T) {
	in := []Finding{
		{Hash: "1", Severity: SeverityNit},
		{Hash: "2", Severity: SeverityImportant},
		{Hash: "3", Severity: SeverityNit},
		{Hash: "4", Severity: SeverityNit},
		{Hash: "5", Severity: SeverityImportant},
	}
	out, dropped := capNits(in, 2)
	if dropped != 1 {
		t.Errorf("expected 1 nit dropped, got %d", dropped)
	}
	var nits, imp int
	for _, f := range out {
		switch f.Severity {
		case SeverityNit:
			nits++
		case SeverityImportant:
			imp++
		}
	}
	if nits != 2 || imp != 2 {
		t.Errorf("expected 2 nits + 2 important, got %d nits %d important", nits, imp)
	}
}

func TestCapNitsZeroMeansUnlimited(t *testing.T) {
	in := []Finding{{Severity: SeverityNit}, {Severity: SeverityNit}}
	out, dropped := capNits(in, 0)
	if dropped != 0 || len(out) != 2 {
		t.Errorf("cap<=0 should keep all nits; got %d kept, %d dropped", len(out), dropped)
	}
}

func TestSuppressPostedNits(t *testing.T) {
	in := []Finding{
		{Hash: "nit-posted", Severity: SeverityNit},
		{Hash: "nit-new", Severity: SeverityNit},
		{Hash: "imp-posted", Severity: SeverityImportant},
	}
	posted := map[string]bool{"nit-posted": true, "imp-posted": true}
	out, dropped := suppressPostedNits(in, posted)
	if dropped != 1 {
		t.Fatalf("expected 1 suppressed nit, got %d", dropped)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 findings retained, got %d", len(out))
	}
	for _, f := range out {
		if f.Hash == "nit-posted" {
			t.Error("posted nit should have been suppressed")
		}
	}
}

// --- parsing --------------------------------------------------------------

func TestParseFindingsVariants(t *testing.T) {
	// Raw object.
	fs, err := parseFindings(`{"findings": [{"file":"a","severity":"Nit","title":"t"}]}`)
	if err != nil || len(fs) != 1 {
		t.Fatalf("raw object: got %d findings, err %v", len(fs), err)
	}
	// Fenced block with surrounding prose.
	fs, err = parseFindings("Here is my review:\n```json\n{\"findings\": []}\n```\nDone.")
	if err != nil || len(fs) != 0 {
		t.Fatalf("fenced empty: got %d findings, err %v", len(fs), err)
	}
	// No JSON at all -> error.
	if _, err := parseFindings("no json here"); err == nil {
		t.Error("expected error for missing JSON")
	}
}

func TestScopeDiffToFiles(t *testing.T) {
	d := "diff --git a/keep.go b/keep.go\n@@\n+a\n" +
		"diff --git a/drop.go b/drop.go\n@@\n+b\n"
	out := scopeDiffToFiles(d, []string{"keep.go"})
	if !contains(out, "keep.go") || contains(out, "drop.go") {
		t.Errorf("scope did not keep only keep.go: %q", out)
	}
	// Empty file list returns the diff unchanged.
	if scopeDiffToFiles(d, nil) != d {
		t.Error("empty file list should return diff unchanged")
	}
	// Nonexistent files -> fall back to full diff.
	if scopeDiffToFiles(d, []string{"nope.go"}) != d {
		t.Error("non-matching files should fall back to full diff")
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// --- Review end-to-end (stubbed) -----------------------------------------

func TestReviewShadowModeWritesFindings(t *testing.T) {
	db := openTestDB(t)
	deep := map[string]string{
		"logic": findingsJSON(t, []Finding{
			{File: "main.go", Anchor: "main.go:1", Category: "logic", Severity: SeverityImportant, Title: "bug", Body: "boom"},
		}),
	}
	runner := newScriptRunner(baseScript(triageJSON(t, nil, ""), deep))
	cfg := DefaultConfig().WithRunner(runner.run) // ShadowMode true by default

	res, err := Review(context.Background(), testRequest(), db, cfg)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !res.ShadowMode {
		t.Error("expected ShadowMode true")
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(res.Findings))
	}
	if res.Findings[0].Hash == "" || res.Findings[0].SourcePass != "logic" {
		t.Errorf("finding not finalized: %+v", res.Findings[0])
	}
	if n := countFindings(t, db, "demo", 7); n != 1 {
		t.Errorf("expected 1 persisted finding, got %d", n)
	}
	// triage + 5 deep passes = 6 pass reports.
	if len(res.Passes) != 1+len(deepPasses) {
		t.Errorf("expected %d pass reports, got %d", 1+len(deepPasses), len(res.Passes))
	}
}

func TestReviewIdempotentPerHeadSHA(t *testing.T) {
	db := openTestDB(t)
	deep := map[string]string{
		"security": findingsJSON(t, []Finding{
			{File: "a.go", Anchor: "a.go:5", Category: "security", Severity: SeverityImportant, Title: "x", Body: "y"},
		}),
	}
	cfg := DefaultConfig().WithRunner(newScriptRunner(baseScript(triageJSON(t, nil, ""), deep)).run)

	for i := 0; i < 3; i++ {
		if _, err := Review(context.Background(), testRequest(), db, cfg); err != nil {
			t.Fatalf("Review iter %d: %v", i, err)
		}
	}
	if n := countFindings(t, db, "demo", 7); n != 1 {
		t.Errorf("expected idempotent single persisted finding, got %d", n)
	}
}

func TestReviewNitCap(t *testing.T) {
	db := openTestDB(t)
	var nits []Finding
	for i := 0; i < 5; i++ {
		nits = append(nits, Finding{
			File: "a.go", Anchor: "a.go:" + string(rune('a'+i)), Category: "conventions",
			Severity: SeverityNit, Title: "nit", Body: "polish",
		})
	}
	deep := map[string]string{"conventions": findingsJSON(t, nits)}
	cfg := DefaultConfig().WithRunner(newScriptRunner(baseScript(triageJSON(t, nil, ""), deep)).run)
	cfg.NitCap = 2

	res, err := Review(context.Background(), testRequest(), db, cfg)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(res.Findings) != 2 {
		t.Errorf("expected 2 findings after nit cap, got %d", len(res.Findings))
	}
	if res.NitsCapped != 3 {
		t.Errorf("expected 3 nits capped, got %d", res.NitsCapped)
	}
}

func TestReviewSuppressesAlreadyPostedNit(t *testing.T) {
	db := openTestDB(t)
	anchor, category, body := "a.go:9", "conventions", "rename this"
	hash := computeHash("demo", 7, anchor, category, body)

	// Pre-record the nit as already posted on a prior review of this PR.
	if err := db.InsertFinding(state.Finding{
		Anvil: "demo", PRNumber: 7, HeadSHA: "old", FindingHash: hash,
		File: "a.go", Anchor: anchor, Category: category, Severity: string(SeverityNit),
		Title: "nit", Body: body, Posted: true,
	}); err != nil {
		t.Fatalf("seed posted finding: %v", err)
	}

	deep := map[string]string{
		"conventions": findingsJSON(t, []Finding{
			{File: "a.go", Anchor: anchor, Category: category, Severity: SeverityNit, Title: "nit", Body: body},
		}),
	}
	cfg := DefaultConfig().WithRunner(newScriptRunner(baseScript(triageJSON(t, nil, ""), deep)).run)

	res, err := Review(context.Background(), testRequest(), db, cfg)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("expected posted nit suppressed, got %d findings", len(res.Findings))
	}
	if res.NitsSuppressed != 1 {
		t.Errorf("expected NitsSuppressed=1, got %d", res.NitsSuppressed)
	}
}

func TestReviewStrictJSONRetryThenError(t *testing.T) {
	// Every deep pass returns invalid JSON on both attempts -> run error.
	script := map[string][]stubResp{
		passTriage.Name: {{text: triageJSON(t, nil, "")}},
	}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: "not json"}, {text: "still not json"}}
	}
	cfg := DefaultConfig().WithRunner(newScriptRunner(script).run)

	if _, err := Review(context.Background(), testRequest(), openTestDB(t), cfg); err == nil {
		t.Fatal("expected run error from unparseable pass output")
	}
}

// TestReviewPartialPassFailure verifies the engine no longer throws away
// findings from healthy passes when a single pass errors. Previously a
// max-turns or rate-limit failure on tests-missing would abort the whole
// review and discard the other four passes' findings; now those findings
// flow through and the pass error is surfaced via ReviewResult.PassErrors.
func TestReviewPartialPassFailure(t *testing.T) {
	good := findingsJSON(t, []Finding{
		{File: "a.go", Anchor: "a.go:1", Category: "logic", Severity: SeverityImportant, Title: "t", Body: "b"},
	})
	script := map[string][]stubResp{
		passTriage.Name: {{text: triageJSON(t, nil, "")}},
	}
	// Every deep pass except tests-missing returns one good finding; the
	// tests-missing pass simulates a turn-budget exhaustion that bubbles
	// up as a runner error on both the first attempt and the retry.
	for _, p := range deepPasses {
		if p.Name == "tests-missing" {
			err := fmt.Errorf("assay pass tests-missing: provider claude/claude-opus-4-8 failed (exit 1, subtype error_max_turns)")
			script[p.Name] = []stubResp{{err: err}, {err: err}}
		} else {
			script[p.Name] = []stubResp{{text: good}}
		}
	}
	cfg := DefaultConfig().WithRunner(newScriptRunner(script).run)

	result, err := Review(context.Background(), testRequest(), openTestDB(t), cfg)
	if err != nil {
		t.Fatalf("expected nil error from partial failure; got %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result from partial failure")
	}
	if len(result.Findings) == 0 {
		t.Errorf("expected findings from non-erroring passes; got 0")
	}
	if len(result.PassErrors) != 1 {
		t.Errorf("expected one PassErrors entry; got %d (%v)", len(result.PassErrors), result.PassErrors)
	}
	if len(result.PassErrors) > 0 && !strings.Contains(result.PassErrors[0], "tests-missing") {
		t.Errorf("expected PassErrors to mention tests-missing; got %q", result.PassErrors[0])
	}

	// The run reports the gap rather than reading as a clean review: a
	// distinct partial status, the pass tally, and the failed pass named
	// with the reason it failed for.
	if result.Status != RunStatusPartial {
		t.Errorf("Status = %q; want %q", result.Status, RunStatusPartial)
	}
	if result.TotalPasses != len(deepPasses) {
		t.Errorf("TotalPasses = %d; want %d", result.TotalPasses, len(deepPasses))
	}
	if result.CompletedPasses != len(deepPasses)-1 {
		t.Errorf("CompletedPasses = %d; want %d", result.CompletedPasses, len(deepPasses)-1)
	}
	if len(result.FailedPasses) != 1 {
		t.Fatalf("expected one FailedPasses entry; got %d (%v)", len(result.FailedPasses), result.FailedPasses)
	}
	if result.FailedPasses[0].Name != "tests-missing" {
		t.Errorf("FailedPasses[0].Name = %q; want tests-missing", result.FailedPasses[0].Name)
	}
	if result.FailedPasses[0].Reason != "error_max_turns" {
		t.Errorf("FailedPasses[0].Reason = %q; want error_max_turns", result.FailedPasses[0].Reason)
	}
	want := fmt.Sprintf("partial: %d of %d passes completed (failed: tests-missing — error_max_turns)",
		len(deepPasses)-1, len(deepPasses))
	if got := result.StatusText(); got != want {
		t.Errorf("StatusText() = %q; want %q", got, want)
	}
}

// TestReviewCompleteStatus pins the healthy case: every deep pass answering
// means a complete status, no failed passes, and no coverage caveat anywhere.
func TestReviewCompleteStatus(t *testing.T) {
	good := findingsJSON(t, []Finding{
		{File: "a.go", Anchor: "a.go:1", Category: "logic", Severity: SeverityNit, Title: "t", Body: "b"},
	})
	script := map[string][]stubResp{passTriage.Name: {{text: triageJSON(t, nil, "")}}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: good}}
	}
	cfg := DefaultConfig().WithRunner(newScriptRunner(script).run)

	result, err := Review(context.Background(), testRequest(), openTestDB(t), cfg)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if result.Status != RunStatusComplete {
		t.Errorf("Status = %q; want %q", result.Status, RunStatusComplete)
	}
	if len(result.FailedPasses) != 0 {
		t.Errorf("expected no FailedPasses; got %v", result.FailedPasses)
	}
	if result.CompletedPasses != len(deepPasses) || result.TotalPasses != len(deepPasses) {
		t.Errorf("pass tally = %d/%d; want %d/%d",
			result.CompletedPasses, result.TotalPasses, len(deepPasses), len(deepPasses))
	}
	if note := PartialCoverageNote(result.FailedPasses); note != "" {
		t.Errorf("a complete run must carry no coverage caveat; got %q", note)
	}
}

// TestReviewAllPassesFailed pins the other end of the three-way status call:
// when no deep pass reviewed the head there is nothing to report, so Review
// must return an error and no result. The gate reads DeriveStatus rather than
// counting pass errors, so this covers the case where failedPasses and
// passErrors fall out of step — a run that reviewed nothing coming back as a
// 'partial' result with zero findings would have the caller post a summary
// presenting a review that never happened.
func TestReviewAllPassesFailed(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {{text: triageJSON(t, nil, "")}}}
	for _, p := range deepPasses {
		err := fmt.Errorf("assay pass %s: provider claude/claude-opus-4-8 failed (exit 1, subtype error_max_turns)", p.Name)
		// Two responses: the first attempt and the strict-format retry.
		script[p.Name] = []stubResp{{err: err}, {err: err}}
	}
	cfg := DefaultConfig().WithRunner(newScriptRunner(script).run)

	result, err := Review(context.Background(), testRequest(), openTestDB(t), cfg)
	if err == nil {
		t.Fatal("expected an error when every deep pass failed")
	}
	if result != nil {
		t.Fatalf("expected no result when every deep pass failed; got %+v", result)
	}
	if !strings.Contains(err.Error(), "all assay deep passes failed") {
		t.Errorf("error = %q; want it to name the total failure", err)
	}
	for _, p := range deepPasses {
		if !strings.Contains(err.Error(), p.Name) {
			t.Errorf("error does not name failed pass %q: %v", p.Name, err)
		}
	}
}

func TestReviewRetrySucceedsOnSecondAttempt(t *testing.T) {
	good := findingsJSON(t, []Finding{
		{File: "a.go", Anchor: "a.go:1", Category: "logic", Severity: SeverityImportant, Title: "t", Body: "b"},
	})
	script := map[string][]stubResp{
		passTriage.Name: {{text: triageJSON(t, nil, "")}},
		"logic": {
			{text: "garbage", turns: 9, cost: 0.02, cacheW: 41500},
			{text: good, turns: 4, cost: 0.03, cacheW: 700, cacheR: 41500},
		},
	}
	cfg := DefaultConfig().WithRunner(newScriptRunner(script).run)

	res, err := Review(context.Background(), testRequest(), openTestDB(t), cfg)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(res.Findings) != 1 {
		t.Errorf("expected retry to recover 1 finding, got %d", len(res.Findings))
	}

	rep := passReport(res, "logic")
	if rep == nil {
		t.Fatal("no PassReport for logic")
	}
	// The recorded turn count must be the session whose output was kept — that
	// is the number comparable to the --max-turns budget a single session gets.
	// Keeping the discarded session's 9, or summing the two into 13, would both
	// make the budget look tighter than it was.
	if rep.Turns != 4 {
		t.Errorf("Turns = %d; want 4 (the strict-JSON session whose output was kept)", rep.Turns)
	}
	// Cost, unlike turns, is cumulative: the discarded session was still billed.
	if math.Abs(rep.CostUSD-0.05) > 1e-9 {
		t.Errorf("CostUSD = %v; want 0.05 (both sessions)", rep.CostUSD)
	}
	// Cache accounting is cumulative for the same reason cost is — the
	// unparseable session paid to write the prefix before it answered badly.
	if rep.CacheCreationTokens != 42200 || rep.CacheReadTokens != 41500 {
		t.Errorf("logic cache telemetry = {w:%d r:%d}; want {42200 41500} (both sessions summed)",
			rep.CacheCreationTokens, rep.CacheReadTokens)
	}
	// A strict-JSON re-prompt is not a turn-budget attempt.
	if rep.Retried || rep.Attempts != 1 {
		t.Errorf("telemetry = {Retried:%v Attempts:%d}; want {false 1}", rep.Retried, rep.Attempts)
	}
}

// maxTurnsErr is the error the provider layer produces for a session that
// spent its turn budget without answering. It carries a cost because the real
// one does: the result event reports total_cost_usd on error subtypes too, and
// a session that burned the whole budget is the most expensive kind there is.
func maxTurnsErr(pass string, turns int, cost float64) *PassError {
	return &PassError{
		Pass:    pass,
		Reason:  ReasonMaxTurns,
		Message: fmt.Sprintf("assay pass %s: provider claude/claude-opus-4-8 failed (exit 1, subtype error_max_turns)", pass),
		Turns:   turns,
		CostUSD: cost,
	}
}

// maxTurnsErrCached is maxTurnsErr carrying the failed session's prompt-cache
// accounting too. A failure is not a refund for the cache write any more than
// it is for the tokens: the provider wrote the prefix before the session ran
// out of turns and billed for it, so the numbers a retried pass reports have to
// include it or the redundancy metric under-reports exactly the runs that cost
// the most.
func maxTurnsErrCached(pass string, turns int, cost float64, cacheW, cacheR int) *PassError {
	e := maxTurnsErr(pass, turns, cost)
	e.CacheCreationTokens = cacheW
	e.CacheReadTokens = cacheR
	return e
}

// TestReviewRetriesMaxTurnsPassInFreshSession pins the recovery case: a pass
// that burns its turn budget exploring is re-run once, and a retry that answers
// is an ordinary success — it must not leave a residue in PassErrors or
// FailedPasses, which is what would turn a fully covered run into a "partial"
// one and put a coverage caveat on the PR.
//
// The re-run is a MODIFIED request, not the same one again: it opens with the
// original's bytes (so it still reads the prompt prefix out of the provider's
// cache) and ends with the appended "answer now" instruction that is the whole
// reason to expect a different outcome from it.
func TestReviewRetriesMaxTurnsPassInFreshSession(t *testing.T) {
	good := findingsJSON(t, []Finding{
		{File: "a.go", Anchor: "a.go:1", Category: "logic", Severity: SeverityImportant, Title: "t", Body: "b"},
	})
	script := map[string][]stubResp{passTriage.Name: {{text: triageJSON(t, nil, ""), turns: 3, cost: 0.01}}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: findingsJSON(t, nil), turns: 4, cost: 0.02}}
	}
	script["logic"] = []stubResp{
		{err: maxTurnsErrCached("logic", 12, 0.25, 41500, 0)},
		{text: good, turns: 7, cost: 0.05, cacheW: 800, cacheR: 41500},
	}

	runner := newScriptRunner(script)
	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(runner.run))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(res.PassErrors) != 0 {
		t.Errorf("a pass that succeeded on retry must not count as a pass error; got %v", res.PassErrors)
	}
	if len(res.FailedPasses) != 0 {
		t.Errorf("expected no failed passes; got %v", res.FailedPasses)
	}
	if res.Status != RunStatusComplete {
		t.Errorf("Status = %q; want %q", res.Status, RunStatusComplete)
	}
	if res.CompletedPasses != len(deepPasses) {
		t.Errorf("CompletedPasses = %d; want %d", res.CompletedPasses, len(deepPasses))
	}
	if len(res.Findings) != 1 {
		t.Errorf("expected the retry's finding to survive; got %d findings", len(res.Findings))
	}

	// Two sessions, and the second is not the first again: a byte-identical
	// re-send gives the model exactly as much reason to wander as the session
	// that just wandered, which is what made a partial run cost more than a
	// complete one.
	calls := runner.callsFor("logic")
	if len(calls) != 2 {
		t.Fatalf("expected 2 logic sessions (attempt + retry); got %d", len(calls))
	}
	if calls[0].prompt == calls[1].prompt {
		t.Error("retry re-sent a byte-identical prompt; the re-run must modify its inputs")
	}
	if !strings.Contains(calls[1].prompt, answerNowInstruction) {
		t.Error("retry prompt is missing the answer-now instruction")
	}
	// The modification is a suffix. Everything above it is unchanged, so the
	// retry still reads the shared prefix out of the cache rather than paying
	// to write it a second time.
	if !strings.HasPrefix(calls[1].prompt, calls[0].prompt) {
		t.Error("retry prompt does not open with the original's bytes; the cached prefix is lost")
	}

	rep := passReport(res, "logic")
	if rep == nil {
		t.Fatal("no PassReport for logic")
	}
	if !rep.Retried {
		t.Error("Retried = false; want true")
	}
	if rep.Attempts != 2 {
		t.Errorf("Attempts = %d; want 2", rep.Attempts)
	}
	if rep.TerminationReason != "" {
		t.Errorf("TerminationReason = %q; want empty for a pass that answered", rep.TerminationReason)
	}
	if rep.Turns != 7 {
		t.Errorf("Turns = %d; want 7 (the session the pass recorded)", rep.Turns)
	}
	// Cost is cumulative across sessions, and the exhausted session counts.
	// The provider bills an error_max_turns run — by definition one that spent
	// its whole turn budget — like any other, so charging the run only for the
	// retry that answered would under-report a retried pass by roughly a full
	// session, and with it the daily spend Review's total feeds.
	if math.Abs(rep.CostUSD-0.30) > 1e-9 {
		t.Errorf("logic CostUSD = %v; want 0.30 (0.25 exhausted session + 0.05 retry)", rep.CostUSD)
	}
	// triage 0.01 + logic 0.30 + four other deep passes at 0.02.
	if math.Abs(res.CostUSD-0.39) > 1e-9 {
		t.Errorf("ReviewResult.CostUSD = %v; want 0.39", res.CostUSD)
	}
	// Cache accounting follows cost, not turns: the exhausted session wrote the
	// whole prefix and was billed for it, so both sessions' numbers are summed.
	// Recording the retry's alone would report a 800-token write for a pass
	// that really wrote 42300 — and the redundancy metric (sum minus max) is
	// computed from exactly these numbers, so the under-report lands on the
	// most expensive runs there are.
	if rep.CacheCreationTokens != 42300 || rep.CacheReadTokens != 41500 {
		t.Errorf("logic cache telemetry = {w:%d r:%d}; want {42300 41500} (both sessions summed)",
			rep.CacheCreationTokens, rep.CacheReadTokens)
	}
	if got := res.PassTelemetryText(); !strings.Contains(got, "pass=logic turns=7 term=success retry=1") {
		t.Errorf("PassTelemetryText() = %q; want it to report the logic retry", got)
	}
}

// TestReviewDoesNotRetryMaxTurnsFromStrictJSONSession bounds the retry from the
// other side. A max-turns failure earns a fresh session only when it was the
// attempt's first: here the base prompt answered (unparseably) and the strict
// -JSON re-prompt is what ran out of turns, so the attempt has already made two
// full-price sessions and a re-run could buy two more — four for one pass,
// double what the documented "re-run once" bound implies.
func TestReviewDoesNotRetryMaxTurnsFromStrictJSONSession(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {{text: triageJSON(t, nil, ""), turns: 2}}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: findingsJSON(t, nil), turns: 4}}
	}
	script["logic"] = []stubResp{
		{text: "not json", turns: 3, cost: 0.02, cacheW: 41500},
		{err: maxTurnsErrCached("logic", 12, 0.25, 600, 41500)},
	}

	runner := newScriptRunner(script)
	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(runner.run))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if n := len(runner.callsFor("logic")); n != 2 {
		t.Fatalf("expected exactly 2 logic sessions (base prompt + strict re-prompt, no turn-budget retry); got %d", n)
	}
	if len(res.FailedPasses) != 1 || res.FailedPasses[0].Name != "logic" || res.FailedPasses[0].Reason != ReasonMaxTurns {
		t.Fatalf("FailedPasses = %v; want one {logic error_max_turns} entry", res.FailedPasses)
	}
	rep := passReport(res, "logic")
	if rep == nil {
		t.Fatal("no PassReport for logic")
	}
	if rep.Retried || rep.Attempts != 1 {
		t.Errorf("telemetry = {Retried:%v Attempts:%d}; want {false 1} — no turn-budget attempt was retried", rep.Retried, rep.Attempts)
	}
	// Attempts stays at 1 while CostUSD covers both sessions: the two fields
	// measure different things and this is the case that separates them.
	if math.Abs(rep.CostUSD-0.27) > 1e-9 {
		t.Errorf("logic CostUSD = %v; want 0.27 (both sessions of the single attempt)", rep.CostUSD)
	}
	// The failing re-prompt's own cache accounting rides on its PassError and
	// is summed in like a successful session's: this is the error branch of the
	// same accumulation, and the one an `=` in place of a `+=` would silently
	// truncate to the failure's numbers alone.
	if rep.CacheCreationTokens != 42100 || rep.CacheReadTokens != 41500 {
		t.Errorf("logic cache telemetry = {w:%d r:%d}; want {42100 41500} (both sessions of the single attempt)",
			rep.CacheCreationTokens, rep.CacheReadTokens)
	}
}

// TestReviewDoesNotRetryTriageMaxTurns pins the triage side of the retry
// boundary, which runTriage's doc comment declares but nothing enforced. Triage
// is a hard gate — its failure aborts the run rather than costing one pass's
// coverage — so there is no partial outcome for a retry to salvage, and routing
// it through the deep passes' retry path would only spend a second session
// before failing the run anyway.
func TestReviewDoesNotRetryTriageMaxTurns(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {{err: maxTurnsErr(passTriage.Name, 12, 0.25)}}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: findingsJSON(t, nil)}}
	}

	runner := newScriptRunner(script)
	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(runner.run))
	if err == nil {
		t.Fatalf("Review succeeded; want a run error when triage fails (result %+v)", res)
	}
	if n := len(runner.callsFor(passTriage.Name)); n != 1 {
		t.Errorf("expected exactly 1 triage session (no turn-budget retry); got %d", n)
	}
	// The gate is closed before any deep pass runs, so nothing downstream is
	// billed for a run that never had a scoped diff.
	for _, p := range deepPasses {
		if n := len(runner.callsFor(p.Name)); n != 0 {
			t.Errorf("deep pass %s ran %d session(s); want 0 after a triage failure", p.Name, n)
		}
	}
	// The one session that did run is billed, and with no result to carry the
	// total it leaves on the error instead. An error_max_turns triage burned
	// its whole budget, so this is the expensive kind of failure to lose.
	if got := RunCost(err); math.Abs(got-0.25) > 1e-9 {
		t.Errorf("RunCost(err) = %v; want 0.25 (the failed triage session)", got)
	}
}

// TestReviewCountsTriageStrictJSONRetryCost pins triage's strict-JSON re-prompt
// — the branch where the first reply is unparseable and a second session with
// the stricter reminder answers. The pass keeps *both* sessions' cost (the
// provider billed the unparseable one) but only the recorded session's turn
// count, since a sum would say nothing about how close either session came to
// the --max-turns budget. The deep passes have the same contract and their own
// test; without this one, dropping `turns = out2.Turns` would silently report
// the discarded session's count.
func TestReviewCountsTriageStrictJSONRetryCost(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {
		{text: "not json at all", turns: 9, cost: 0.04, cacheW: 30000},
		{text: triageJSON(t, nil, ""), turns: 2, cost: 0.01, cacheW: 500, cacheR: 30000},
	}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: findingsJSON(t, nil), turns: 4, cost: 0.02}}
	}

	runner := newScriptRunner(script)
	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(runner.run))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if n := len(runner.callsFor(passTriage.Name)); n != 2 {
		t.Fatalf("expected 2 triage sessions (base prompt + strict re-prompt); got %d", n)
	}
	rep := passReport(res, passTriage.Name)
	if rep == nil {
		t.Fatal("no PassReport for triage")
	}
	if rep.Turns != 2 {
		t.Errorf("triage Turns = %d; want 2 (the session whose output was recorded, not the discarded one's 9)", rep.Turns)
	}
	// The strict re-prompt is a second session inside the single attempt, not a
	// second attempt — Attempts stays 1 while cost covers both.
	if rep.Attempts != 1 || rep.Retried {
		t.Errorf("triage telemetry = {Attempts:%d Retried:%v}; want {1 false}", rep.Attempts, rep.Retried)
	}
	if math.Abs(rep.CostUSD-0.05) > 1e-9 {
		t.Errorf("triage CostUSD = %v; want 0.05 (0.04 unparseable session + 0.01 re-prompt)", rep.CostUSD)
	}
	// Triage's cache accounting is summed over both sessions, like its cost.
	if rep.CacheCreationTokens != 30500 || rep.CacheReadTokens != 30000 {
		t.Errorf("triage cache telemetry = {w:%d r:%d}; want {30500 30000} (both sessions summed)",
			rep.CacheCreationTokens, rep.CacheReadTokens)
	}
	// triage 0.05 + five deep passes at 0.02.
	if math.Abs(res.CostUSD-0.15) > 1e-9 {
		t.Errorf("ReviewResult.CostUSD = %v; want 0.15", res.CostUSD)
	}
}

// TestReviewCarriesCostWhenTriageStrictRetryFails is the error half of the same
// branch: the re-prompt session fails outright, so runTriage pairs the first
// session's cost with the failed one's and Review carries the sum out on the
// RunError rather than dropping it with the nil result.
func TestReviewCarriesCostWhenTriageStrictRetryFails(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {
		{text: "not json at all", turns: 9, cost: 0.04},
		{err: maxTurnsErr(passTriage.Name, 12, 0.25)},
	}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: findingsJSON(t, nil), cost: 0.02}}
	}

	runner := newScriptRunner(script)
	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(runner.run))
	if err == nil {
		t.Fatalf("Review succeeded; want a run error when triage's retry fails (result %+v)", res)
	}
	if n := len(runner.callsFor(passTriage.Name)); n != 2 {
		t.Errorf("expected 2 triage sessions (base prompt + strict re-prompt); got %d", n)
	}
	if got := RunCost(err); math.Abs(got-0.29) > 1e-9 {
		t.Errorf("RunCost(err) = %v; want 0.29 (0.04 unparseable session + 0.25 failed re-prompt)", got)
	}
}

// TestRunTriageSumsCacheTokensWhenStrictRetryFails is the last of the four
// accumulation branches, and the only one Review cannot show: a triage failure
// aborts the run, and RunError carries the spend but not the cache accounting,
// so the numbers are asserted on runTriage directly. Both sessions wrote the
// prefix — the unparseable one and the one that ran out of turns — and both
// were billed for it.
func TestRunTriageSumsCacheTokensWhenStrictRetryFails(t *testing.T) {
	runner := newScriptRunner(map[string][]stubResp{passTriage.Name: {
		{text: "not json at all", turns: 9, cost: 0.04, cacheW: 30000},
		{err: maxTurnsErrCached(passTriage.Name, 12, 0.25, 700, 30000)},
	}})

	run, err := runTriage(context.Background(), runner.run, DefaultConfig(), testRequest(), "diff --git a/x b/x\n+x\n")
	if err == nil {
		t.Fatalf("runTriage succeeded; want the re-prompt's failure (run %+v)", run)
	}
	if run.usage.CacheWriteTokens != 30700 || run.usage.CacheReadTokens != 30000 {
		t.Errorf("triage cache telemetry = {w:%d r:%d}; want {30700 30000} (both sessions summed)",
			run.usage.CacheWriteTokens, run.usage.CacheReadTokens)
	}
	if math.Abs(run.usage.EstimatedCostUSD-0.29) > 1e-9 {
		t.Errorf("triage cost = %v; want 0.29 (both sessions)", run.usage.EstimatedCostUSD)
	}
}

// TestReviewCarriesCostWhenEveryDeepPassFails covers the other run-level
// failure: every deep pass errors, so Review returns no result at all. Six
// billed sessions are the most expensive way a run can end, and the daemon's
// error path has nothing but the error to read the total from.
func TestReviewCarriesCostWhenEveryDeepPassFails(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {{text: triageJSON(t, nil, ""), turns: 2, cost: 0.01}}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{err: &PassError{
			Pass:    p.Name,
			Reason:  ReasonRateLimited,
			Message: "assay pass " + p.Name + ": provider claude/claude-opus-4-8 rate limited",
			CostUSD: 0.02,
		}}}
	}

	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(newScriptRunner(script).run))
	if err == nil {
		t.Fatalf("Review succeeded; want a run error when every deep pass fails (result %+v)", res)
	}
	// triage 0.01 + five failed deep passes at 0.02.
	if got := RunCost(err); math.Abs(got-0.11) > 1e-9 {
		t.Errorf("RunCost(err) = %v; want 0.11 (triage plus every failed deep pass)", got)
	}
	// The wrapped cause is still what a caller logs or matches on.
	if !strings.Contains(err.Error(), "all assay deep passes failed") {
		t.Errorf("err = %q; want the underlying message verbatim", err.Error())
	}
}

// TestRunCostIgnoresForeignErrors keeps the accessor from fabricating a number.
// An error raised before any session ran, or one from a caller this package did
// not build, reports 0 — an undercount is the safe direction for a value that
// feeds a spend limit.
func TestRunCostIgnoresForeignErrors(t *testing.T) {
	if got := RunCost(nil); got != 0 {
		t.Errorf("RunCost(nil) = %v; want 0", got)
	}
	if got := RunCost(errors.New("something else")); got != 0 {
		t.Errorf("RunCost(foreign) = %v; want 0", got)
	}
	// A RunError wrapped further up still reports its cost.
	wrapped := fmt.Errorf("assay: %w", &RunError{Usage: cost.Usage{EstimatedCostUSD: 0.42}, Err: errors.New("boom")})
	if got := RunCost(wrapped); math.Abs(got-0.42) > 1e-9 {
		t.Errorf("RunCost(wrapped) = %v; want 0.42", got)
	}
}

// TestReviewMaxTurnsPassFailsTwice pins the bounded half: the retry happens
// exactly once, and a pass that exhausts its budget twice is reported once,
// with the reason it ended on, rather than once per attempt.
//
// It also carries the cost invariant on the path where nothing is recovered —
// a pass that burns its budget twice produced no findings at all, but the run
// paid for both sessions and the total must say so. That is the same scenario,
// so it lives in this one function rather than a second copy of the script.
func TestReviewMaxTurnsPassFailsTwice(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {{text: triageJSON(t, nil, ""), turns: 2, cost: 0.01}}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: findingsJSON(t, nil), turns: 4, cost: 0.02}}
	}
	script["logic"] = []stubResp{{err: maxTurnsErr("logic", 12, 0.25)}, {err: maxTurnsErr("logic", 11, 0.30)}}

	runner := newScriptRunner(script)
	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(runner.run))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if n := len(runner.callsFor("logic")); n != 2 {
		t.Fatalf("expected exactly 2 logic sessions (one retry, never a third); got %d", n)
	}
	if len(res.FailedPasses) != 1 {
		t.Fatalf("expected the failed pass listed exactly once; got %v", res.FailedPasses)
	}
	if res.FailedPasses[0].Name != "logic" || res.FailedPasses[0].Reason != ReasonMaxTurns {
		t.Errorf("FailedPasses[0] = %+v; want {logic error_max_turns}", res.FailedPasses[0])
	}
	if len(res.PassErrors) != 1 {
		t.Errorf("expected one PassErrors entry; got %d (%v)", len(res.PassErrors), res.PassErrors)
	}
	if res.Status != RunStatusPartial {
		t.Errorf("Status = %q; want %q", res.Status, RunStatusPartial)
	}
	// The same failure reaches both surfaces from the one structure.
	if got := res.StatusText(); !strings.Contains(got, "logic — error_max_turns") {
		t.Errorf("StatusText() = %q; want it to name logic — error_max_turns", got)
	}
	if got := PartialCoverageNote(res.FailedPasses); !strings.Contains(got, "logic (error_max_turns)") {
		t.Errorf("PartialCoverageNote() = %q; want it to name logic (error_max_turns)", got)
	}

	rep := passReport(res, "logic")
	if rep == nil {
		t.Fatal("no PassReport for logic")
	}
	if !rep.Retried || rep.Attempts != 2 {
		t.Errorf("telemetry = {Retried:%v Attempts:%d}; want {true 2}", rep.Retried, rep.Attempts)
	}
	if rep.TerminationReason != ReasonMaxTurns {
		t.Errorf("TerminationReason = %q; want %q", rep.TerminationReason, ReasonMaxTurns)
	}
	if rep.Turns != 11 {
		t.Errorf("Turns = %d; want 11 (the final attempt's session)", rep.Turns)
	}
	if got := res.PassTelemetryText(); !strings.Contains(got, "pass=logic turns=11 term=error_max_turns retry=1") {
		t.Errorf("PassTelemetryText() = %q; want the failed logic pass with its turn count", got)
	}
	if math.Abs(rep.CostUSD-0.55) > 1e-9 {
		t.Errorf("logic CostUSD = %v; want 0.55 (both exhausted sessions)", rep.CostUSD)
	}
	// triage 0.01 + logic 0.55 + four other deep passes at 0.02.
	if math.Abs(res.CostUSD-0.64) > 1e-9 {
		t.Errorf("ReviewResult.CostUSD = %v; want 0.64", res.CostUSD)
	}
}

// TestReviewDoesNotRetryNonMaxTurnsFailure keeps the retry narrow. A rate
// limit or a spawn failure says nothing about turn budgets and would fail the
// same way a second time, so re-running it only spends money and time.
func TestReviewDoesNotRetryNonMaxTurnsFailure(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {{text: triageJSON(t, nil, ""), turns: 2}}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: findingsJSON(t, nil), turns: 4}}
	}
	script["security"] = []stubResp{{err: &PassError{
		Pass:    "security",
		Reason:  ReasonRateLimited,
		Message: "assay pass security: provider claude/claude-opus-4-8 rate limited",
	}}}

	runner := newScriptRunner(script)
	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(runner.run))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if n := len(runner.callsFor("security")); n != 1 {
		t.Fatalf("expected a single security session (no retry); got %d", n)
	}
	if len(res.FailedPasses) != 1 || res.FailedPasses[0].Reason != ReasonRateLimited {
		t.Fatalf("FailedPasses = %v; want one rate_limited entry", res.FailedPasses)
	}
	rep := passReport(res, "security")
	if rep == nil {
		t.Fatal("no PassReport for security")
	}
	if rep.Retried || rep.Attempts != 1 {
		t.Errorf("telemetry = {Retried:%v Attempts:%d}; want {false 1}", rep.Retried, rep.Attempts)
	}
	if rep.TerminationReason != ReasonRateLimited {
		t.Errorf("TerminationReason = %q; want %q", rep.TerminationReason, ReasonRateLimited)
	}
}

// TestReviewRecordsPerPassTurnTelemetry pins the tuning data itself: every
// pass of a healthy run — triage included — carries the turns it used and how
// it terminated, on the result and in the rendered log field.
func TestReviewRecordsPerPassTurnTelemetry(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {{text: triageJSON(t, nil, ""), turns: 3}}}
	for i, p := range deepPasses {
		script[p.Name] = []stubResp{{text: findingsJSON(t, nil), turns: 5 + i}}
	}
	runner := newScriptRunner(script)
	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(runner.run))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(res.Passes) != 1+len(deepPasses) {
		t.Fatalf("expected %d pass reports, got %d", 1+len(deepPasses), len(res.Passes))
	}
	if rep := passReport(res, passTriage.Name); rep == nil || rep.Turns != 3 {
		t.Errorf("triage report = %+v; want Turns 3", rep)
	}
	telemetry := res.PassTelemetryText()
	for i, p := range deepPasses {
		rep := passReport(res, p.Name)
		if rep == nil {
			t.Fatalf("no PassReport for %s", p.Name)
		}
		if rep.Turns != 5+i {
			t.Errorf("%s Turns = %d; want %d", p.Name, rep.Turns, 5+i)
		}
		if rep.TerminationReason != "" || rep.Retried || rep.Attempts != 1 {
			t.Errorf("%s telemetry = %+v; want a clean single-attempt pass", p.Name, *rep)
		}
		want := fmt.Sprintf("pass=%s turns=%d term=success", p.Name, 5+i)
		if !strings.Contains(telemetry, want) {
			t.Errorf("PassTelemetryText() = %q; want it to contain %q", telemetry, want)
		}
	}
	if strings.Contains(telemetry, "retry=") {
		t.Errorf("PassTelemetryText() = %q; a run with no retries must not report one", telemetry)
	}
}

func TestReviewNilDBSkipsPersistence(t *testing.T) {
	deep := map[string]string{
		"logic": findingsJSON(t, []Finding{
			{File: "a.go", Anchor: "a.go:1", Category: "logic", Severity: SeverityNit, Title: "t", Body: "b"},
		}),
	}
	cfg := DefaultConfig().WithRunner(newScriptRunner(baseScript(triageJSON(t, nil, ""), deep)).run)
	res, err := Review(context.Background(), testRequest(), nil, cfg)
	if err != nil {
		t.Fatalf("Review with nil db: %v", err)
	}
	if len(res.Findings) != 1 {
		t.Errorf("expected 1 finding, got %d", len(res.Findings))
	}
}

// --- config ---------------------------------------------------------------

func TestFromAssayConfigDefaultsAndOverrides(t *testing.T) {
	// Empty config -> defaults (shadow on, no model hints).
	cfg := FromAssayConfig(config.AssayConfig{})
	if !cfg.ShadowMode {
		t.Error("expected shadow mode default true")
	}

	shadow := false
	nit := 4
	ac := config.AssayConfig{
		ShadowMode:  &shadow,
		NitCap:      &nit,
		ModelTier:   "fast",
		TriageModel: "model-a",
		ReviewModel: "model-b",
		SkipPaths:   []string{"vendor/**"},
	}
	cfg = FromAssayConfig(ac)
	if cfg.ShadowMode {
		t.Error("expected shadow mode false override")
	}
	if cfg.NitCap != 4 {
		t.Errorf("nit cap = %d, want 4", cfg.NitCap)
	}
	if cfg.ModelTier != "fast" || cfg.TriageModel != "model-a" || cfg.ReviewModel != "model-b" {
		t.Errorf("model fields not propagated: %+v", cfg)
	}
	if len(cfg.SkipPaths) != 1 {
		t.Errorf("skip paths not propagated: %+v", cfg.SkipPaths)
	}
}

func TestProviderForUsesConfiguredModelHintsOnly(t *testing.T) {
	cfg := Config{
		TriageProvider: "claude",
		ReviewProvider: "claude",
		TriageModel:    "triage-model",
		ReviewModel:    "review-model",
	}
	if pv := cfg.providerFor(tierTriage); pv.Model != "triage-model" {
		t.Errorf("triage model = %q, want triage-model", pv.Model)
	}
	if pv := cfg.providerFor(tierReview); pv.Model != "review-model" {
		t.Errorf("review model = %q, want review-model", pv.Model)
	}
	// With no hints, Model must stay empty (provider default) — never a
	// hard-coded identifier.
	empty := Config{}
	if pv := empty.providerFor(tierReview); pv.Model != "" {
		t.Errorf("expected empty model with no hints, got %q", pv.Model)
	}
}

// TestReviewUsageSumsEveryPass pins the run-level accounting the daemon hands
// to the cost tables: a run's usage is every session of every pass added up —
// triage included — with the provider's cache halves kept apart from the plain
// token counts. Recording a run's dollars while dropping its cache columns is
// the failure this exists to catch, so every pass reports a distinct set of
// numbers and none of them can be missing from the total by coincidence.
func TestReviewUsageSumsEveryPass(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {
		{text: triageJSON(t, nil, ""), turns: 2, cost: 0.01, tokensIn: 100, tokensOut: 10, cacheW: 30000, cacheR: 0},
	}}
	for i, p := range deepPasses {
		script[p.Name] = []stubResp{{
			text:      `{"findings": []}`,
			turns:     3,
			cost:      0.02,
			tokensIn:  200 + i,
			tokensOut: 20 + i,
			cacheW:    500 + i,
			cacheR:    30000 + i,
		}}
	}

	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(newScriptRunner(script).run))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	want := cost.Usage{InputTokens: 100, OutputTokens: 10, CacheWriteTokens: 30000, EstimatedCostUSD: 0.01}
	for i := range deepPasses {
		want.Add(cost.Usage{
			InputTokens:      200 + i,
			OutputTokens:     20 + i,
			CacheWriteTokens: 500 + i,
			CacheReadTokens:  30000 + i,
			EstimatedCostUSD: 0.02,
		})
	}
	if res.Usage.InputTokens != want.InputTokens || res.Usage.OutputTokens != want.OutputTokens ||
		res.Usage.CacheWriteTokens != want.CacheWriteTokens || res.Usage.CacheReadTokens != want.CacheReadTokens {
		t.Errorf("result usage tokens = %+v; want %+v", res.Usage, want)
	}
	if math.Abs(res.Usage.EstimatedCostUSD-want.EstimatedCostUSD) > 1e-9 {
		t.Errorf("result usage cost = %v; want %v", res.Usage.EstimatedCostUSD, want.EstimatedCostUSD)
	}
	// CostUSD is the same number under its historical name — nothing that
	// renders it had to change.
	if math.Abs(res.CostUSD-res.Usage.EstimatedCostUSD) > 1e-9 {
		t.Errorf("CostUSD = %v; want the usage's cost %v", res.CostUSD, res.Usage.EstimatedCostUSD)
	}
}

// TestRunUsageCarriesFailedRunAccounting is RunCost's twin on the error path: a
// run that dies still paid for the sessions it made, cache writes included, and
// the daemon has nothing but the error to read them from.
func TestRunUsageCarriesFailedRunAccounting(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {
		{text: triageJSON(t, nil, ""), turns: 2, cost: 0.01, tokensIn: 100, tokensOut: 10, cacheW: 30000},
	}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{err: &PassError{
			Pass:                p.Name,
			Reason:              ReasonRateLimited,
			Message:             "assay pass " + p.Name + ": provider claude rate limited",
			CostUSD:             0.02,
			TokensIn:            50,
			TokensOut:           5,
			CacheCreationTokens: 400,
		}}}
	}

	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(newScriptRunner(script).run))
	if err == nil {
		t.Fatalf("Review succeeded; want a run error when every deep pass fails (result %+v)", res)
	}
	got := RunUsage(err)
	if got.InputTokens != 100+5*50 || got.OutputTokens != 10+5*5 || got.CacheWriteTokens != 30000+5*400 {
		t.Errorf("RunUsage(err) = %+v; want triage plus every failed deep pass", got)
	}
	if math.Abs(got.EstimatedCostUSD-RunCost(err)) > 1e-9 {
		t.Errorf("RunUsage cost %v disagrees with RunCost %v", got.EstimatedCostUSD, RunCost(err))
	}
	// A foreign error fabricates nothing.
	foreign := RunUsage(errors.New("something else"))
	if !foreign.IsZero() {
		t.Error("RunUsage(foreign) should be zero")
	}
}
