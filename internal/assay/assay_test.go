package assay

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
)

// --- test helpers ---------------------------------------------------------

// stubResp is one scripted runner response.
type stubResp struct {
	text string
	err  error
}

// scriptRunner is a deterministic PassRunner: each pass name has an ordered
// list of responses; the final response repeats for any further calls.
type scriptRunner struct {
	mu     sync.Mutex
	script map[string][]stubResp
	idx    map[string]int
}

func newScriptRunner(script map[string][]stubResp) *scriptRunner {
	return &scriptRunner{script: script, idx: map[string]int{}}
}

func (r *scriptRunner) run(_ context.Context, pass, _, _ string) (PassOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
		return PassOutput{}, resp.err
	}
	return PassOutput{Text: resp.text}, nil
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
		"logic":         {{text: "garbage"}, {text: good}},
	}
	cfg := DefaultConfig().WithRunner(newScriptRunner(script).run)

	res, err := Review(context.Background(), testRequest(), openTestDB(t), cfg)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(res.Findings) != 1 {
		t.Errorf("expected retry to recover 1 finding, got %d", len(res.Findings))
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
