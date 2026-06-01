package assay

import (
	"context"
	"encoding/json"
	"path/filepath"
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
