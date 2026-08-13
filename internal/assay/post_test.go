package assay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Robin831/Forge/internal/state"
)

// --- gh exec stub ---------------------------------------------------------

// recordedCall captures one ghExec invocation.
type recordedCall struct {
	args []string
}

// stubGh is a deterministic ghExec. It records every call and returns either a
// scripted stdout or an error. failPaths makes inline POSTs for a given file
// path fail (to exercise per-comment continue-on-failure). existingComments,
// when set, is the JSON array the summary-upsert list call returns (default:
// no comments, so the summary is created).
type stubGh struct {
	mu               sync.Mutex
	calls            []recordedCall
	nextID           int64
	failPaths        map[string]bool
	existingComments []byte
}

func newStubGh() *stubGh {
	return &stubGh{nextID: 1000, failPaths: map[string]bool{}}
}

func (s *stubGh) exec(_ context.Context, _ string, args ...string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, recordedCall{args: args})

	// Inline comment POST? Find the path field to decide success/failure.
	isInline := false
	isCommentList := hasArg(args, "--paginate")
	path := ""
	for _, a := range args {
		if strings.HasPrefix(a, "path=") {
			path = strings.TrimPrefix(a, "path=")
		}
		if strings.Contains(a, "/pulls/") && strings.HasSuffix(a, "/comments") {
			isInline = true
		}
	}
	if isInline {
		if s.failPaths[path] {
			return nil, fmt.Errorf("simulated gh failure for %s", path)
		}
		s.nextID++
		return []byte(fmt.Sprintf(`{"id": %d}`, s.nextID)), nil
	}
	if isCommentList {
		if s.existingComments != nil {
			return s.existingComments, nil
		}
		return []byte(`[]`), nil
	}
	return []byte(`{}`), nil
}

func (s *stubGh) inlineCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		for _, a := range c.args {
			if strings.Contains(a, "/pulls/") && strings.HasSuffix(a, "/comments") {
				n++
			}
		}
	}
	return n
}

// summaryPosted reports whether a summary comment was created or edited: a
// POST to the PR's issue-comments endpoint or a PATCH to an issue comment.
func (s *stubGh) summaryPosted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if s.isSummaryWrite(c.args) {
			return true
		}
	}
	return false
}

// summaryBody returns the body= payload of the summary create/edit call, or ""
// when none happened.
func (s *stubGh) summaryBody() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if !s.isSummaryWrite(c.args) {
			continue
		}
		for _, a := range c.args {
			if strings.HasPrefix(a, "body=") {
				return strings.TrimPrefix(a, "body=")
			}
		}
	}
	return ""
}

func (s *stubGh) isSummaryWrite(args []string) bool {
	method := ""
	endpoint := ""
	for i, a := range args {
		if a == "--method" && i+1 < len(args) {
			method = args[i+1]
		}
		if strings.Contains(a, "/issues/") {
			endpoint = a
		}
	}
	if endpoint == "" {
		return false
	}
	if method == "POST" && strings.HasSuffix(endpoint, "/comments") {
		return true
	}
	return method == "PATCH" && strings.Contains(endpoint, "/issues/comments/")
}

// --- thread resolver stub -------------------------------------------------

type stubResolver struct {
	threadID    string // returned by ThreadIDByBodyHeader
	lookupCalls int
	resolved    []string
	lookedFor   []string
}

func (r *stubResolver) ThreadIDByBodyHeader(_ context.Context, _ string, _ int, header string) (string, error) {
	r.lookupCalls++
	r.lookedFor = append(r.lookedFor, header)
	return r.threadID, nil
}

func (r *stubResolver) ResolveThread(_ context.Context, _ string, threadID string) error {
	r.resolved = append(r.resolved, threadID)
	return nil
}

// --- helpers --------------------------------------------------------------

func newTestDB(t *testing.T) *state.DB {
	t.Helper()
	dir, err := os.MkdirTemp("", "assay-post-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func readFindingRow(t *testing.T, db *state.DB, hash string) (posted bool, commentID int64, misses int, resolved bool) {
	t.Helper()
	var p, m int
	var resolvedAt *string
	err := db.Conn().QueryRow(
		`SELECT posted, gh_comment_id, consecutive_misses, resolved_at
		   FROM pr_findings WHERE finding_hash = ?`, hash,
	).Scan(&p, &commentID, &m, &resolvedAt)
	if err != nil {
		t.Fatalf("reading finding %s: %v", hash, err)
	}
	return p == 1, commentID, m, resolvedAt != nil
}

func newPoster(db *state.DB, gh ghExec, resolver ThreadResolver) *Poster {
	return &Poster{gh: gh, db: db, resolver: resolver, logf: func(string, ...any) {}}
}

// --- buildSummaryBody -----------------------------------------------------

func TestBuildSummaryBody(t *testing.T) {
	findings := []Finding{
		{Severity: SeverityImportant},
		{Severity: SeverityImportant},
		{Severity: SeverityNit},
		{Severity: SeverityPreExisting},
	}
	got := buildSummaryBody("2 important, 1 nit, 1 pre-existing.", nil, findings)

	if !strings.HasPrefix(got, "2 important, 1 nit, 1 pre-existing.") {
		t.Errorf("summary body should start with the summary line, got:\n%s", got)
	}
	for _, want := range []string{
		"| Severity | Count |",
		"| Important | 2 |",
		"| Nit | 1 |",
		"| PreExisting | 1 |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary body missing %q, got:\n%s", want, got)
		}
	}
}

func TestBuildSummaryBodyNamesFailedPassesAboveFindings(t *testing.T) {
	findings := []Finding{{Severity: SeverityNit}}
	failed := []PassFailure{
		{Name: "logic", Reason: "error_max_turns"},
		{Name: "repo-specific", Reason: "error_max_turns"},
	}
	got := buildSummaryBody("Assay (AI review): 0 important, 1 nit", failed, findings)

	for _, want := range []string{"Partial coverage", "logic, repo-specific", "error_max_turns"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary body missing %q, got:\n%s", want, got)
		}
	}
	// The caveat must precede the severity table: a reader who meets it after
	// the findings has already read them as a full review.
	if strings.Index(got, "Partial coverage") > strings.Index(got, "| Severity | Count |") {
		t.Errorf("coverage caveat must come before the severity table, got:\n%s", got)
	}
}

func TestBuildSummaryBodyNoCaveatOnFullRun(t *testing.T) {
	got := buildSummaryBody("Assay (AI review): no issues found.", nil, nil)
	if strings.Contains(got, "Partial coverage") {
		t.Errorf("a full run must not claim partial coverage, got:\n%s", got)
	}
}

func TestBuildSummaryBodyOmitsZeroRows(t *testing.T) {
	got := buildSummaryBody("", nil, []Finding{{Severity: SeverityNit}})
	if strings.Contains(got, "Important") || strings.Contains(got, "PreExisting") {
		t.Errorf("zero-count severities should be omitted, got:\n%s", got)
	}
	if !strings.Contains(got, "| Nit | 1 |") {
		t.Errorf("expected the Nit row, got:\n%s", got)
	}
}

// --- buildInlineBody / marker --------------------------------------------

func TestBuildInlineBodyOpensWithMarker(t *testing.T) {
	f := Finding{
		Hash:     "deadbeef",
		Severity: SeverityImportant,
		Title:    "nil deref",
		Body:     "x may be nil here.",
		Evidence: "x.Foo()",
	}
	got := buildInlineBody(f)

	wantMarker := "<!-- assay-hash: deadbeef -->"
	if !strings.HasPrefix(got, wantMarker) {
		t.Errorf("inline body must OPEN with the hash marker %q, got:\n%s", wantMarker, got)
	}
	if !strings.Contains(got, "**[Important] nil deref**") {
		t.Errorf("inline body missing severity-tagged title, got:\n%s", got)
	}
	if !strings.Contains(got, "x may be nil here.") || !strings.Contains(got, "x.Foo()") {
		t.Errorf("inline body missing body/evidence, got:\n%s", got)
	}
}

func TestMarkerForDeterministic(t *testing.T) {
	if markerFor("abc123") != markerFor("abc123") {
		t.Fatal("markerFor must be deterministic for the same hash")
	}
	if markerFor("abc123") == markerFor("def456") {
		t.Fatal("markerFor must differ for different hashes")
	}
}

// --- parseLineSpec --------------------------------------------------------

func TestParseLineSpec(t *testing.T) {
	cases := []struct {
		anchor             string
		wantStart, wantEnd int
		wantOK             bool
	}{
		{"main.go:42", 42, 42, true},
		{"pkg/file.go:10-20", 10, 20, true},
		{"pkg/file.go:20-10", 10, 20, true}, // normalized
		{"main.go", 0, 0, false},
		{"main.go:", 0, 0, false},
		{"main.go:abc", 0, 0, false},
	}
	for _, c := range cases {
		s, e, ok := parseLineSpec(c.anchor)
		if ok != c.wantOK || s != c.wantStart || e != c.wantEnd {
			t.Errorf("parseLineSpec(%q) = (%d,%d,%v), want (%d,%d,%v)",
				c.anchor, s, e, ok, c.wantStart, c.wantEnd, c.wantOK)
		}
	}
}

// --- Post: payload construction (single vs multi-line) --------------------

func TestPostInlinePayloadLineSpec(t *testing.T) {
	gh := newStubGh()
	p := newPoster(nil, gh.exec, nil)
	req := PostRequest{PRNumber: 7, HeadSHA: "sha7", WorktreePath: "/wt"}

	if _, err := p.postInline(context.Background(), req, Finding{File: "a.go", Anchor: "a.go:5", Hash: "h1"}); err != nil {
		t.Fatalf("single-line postInline: %v", err)
	}
	if _, err := p.postInline(context.Background(), req, Finding{File: "b.go", Anchor: "b.go:5-9", Hash: "h2"}); err != nil {
		t.Fatalf("multi-line postInline: %v", err)
	}

	single := gh.calls[0].args
	if !hasArg(single, "line=5") || hasArg(single, "start_line=5") {
		t.Errorf("single-line call should carry line=5 and no start_line, got %v", single)
	}
	if !hasArg(single, "commit_id=sha7") || !hasArg(single, "path=a.go") {
		t.Errorf("single-line call missing commit_id/path, got %v", single)
	}

	multi := gh.calls[1].args
	if !hasArg(multi, "start_line=5") || !hasArg(multi, "line=9") {
		t.Errorf("multi-line call should carry start_line=5 and line=9, got %v", multi)
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// --- Post: shadow mode short-circuit -------------------------------------

// TestPostSummarisesPartialCoverageWithoutFindings covers the case the caveat
// exists for: a run whose passes half-failed and found nothing. Staying silent
// there would leave the PR looking reviewed and clean.
func TestPostSummarisesPartialCoverageWithoutFindings(t *testing.T) {
	gh := newStubGh()
	p := newPoster(nil, gh.exec, nil)
	req := PostRequest{
		PRNumber:     7,
		FailedPasses: []PassFailure{{Name: "logic", Reason: "error_max_turns"}},
	}
	res, err := p.Post(context.Background(), Config{}, req)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if !res.SummaryPosted {
		t.Fatal("expected a summary review naming the passes that did not run")
	}
	body := gh.summaryBody()
	for _, want := range []string{"Partial coverage", "logic", "error_max_turns"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary review missing %q, got:\n%s", want, body)
		}
	}
}

func TestPostShadowModeNoOp(t *testing.T) {
	gh := newStubGh()
	p := newPoster(nil, gh.exec, nil)
	cfg := Config{ShadowMode: true}
	req := PostRequest{
		PRNumber:    1,
		SummaryLine: "should not post",
		Findings:    []Finding{{File: "a.go", Anchor: "a.go:1", Hash: "h1"}},
	}
	res, err := p.Post(context.Background(), cfg, req)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(gh.calls) != 0 {
		t.Errorf("shadow mode must not call gh, got %d calls", len(gh.calls))
	}
	if res.SummaryPosted || res.Posted != 0 {
		t.Errorf("shadow mode result should be zero, got %+v", res)
	}
}

// --- Post: per-comment continue-on-failure -------------------------------

func TestPostContinuesOnFailure(t *testing.T) {
	db := newTestDB(t)
	gh := newStubGh()
	gh.failPaths["bad.go"] = true
	p := newPoster(db, gh.exec, nil)

	findings := []Finding{
		{File: "good.go", Anchor: "good.go:1", Hash: "ok1", Severity: SeverityImportant, Title: "a"},
		{File: "bad.go", Anchor: "bad.go:2", Hash: "fail1", Severity: SeverityImportant, Title: "b"},
		{File: "good2.go", Anchor: "good2.go:3", Hash: "ok2", Severity: SeverityNit, Title: "c"},
	}
	// Persist rows (as Review would) so posted state is observable.
	for _, f := range findings {
		mustInsert(t, db, "anvil", 5, f)
	}

	cfg := Config{ShadowMode: false}
	req := PostRequest{Anvil: "anvil", PRNumber: 5, HeadSHA: "sha5", Findings: findings, SummaryLine: "s"}
	res, err := p.Post(context.Background(), cfg, req)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	if res.Posted != 2 || res.Failed != 1 {
		t.Errorf("expected 2 posted / 1 failed, got %+v", res)
	}
	if gh.inlineCalls() != 3 {
		t.Errorf("all 3 inline comments should be attempted, got %d", gh.inlineCalls())
	}
	if !gh.summaryPosted() {
		t.Error("summary review should have been posted")
	}

	// Failed finding stays posted=0 for retry; successful ones get an ID.
	if posted, _, _, _ := readFindingRow(t, db, "fail1"); posted {
		t.Error("failed finding should remain posted=0")
	}
	if posted, id, _, _ := readFindingRow(t, db, "ok1"); !posted || id == 0 {
		t.Errorf("ok1 should be posted with a comment id, got posted=%v id=%d", posted, id)
	}
}

// --- Post: consecutive-miss resolution ------------------------------------

func TestPostResolvesOnSecondMiss(t *testing.T) {
	db := newTestDB(t)
	gh := newStubGh()
	resolver := &stubResolver{threadID: "THREAD_1"}
	p := newPoster(db, gh.exec, resolver)

	// A finding posted on an earlier head, currently at 1 miss.
	mustInsert(t, db, "anvil", 9, Finding{
		File: "gone.go", Anchor: "gone.go:1", Hash: "stale", Severity: SeverityImportant, Title: "x",
	})
	mustMarkPosted(t, db, "stale", 111)
	if err := db.IncrementConsecutiveMiss("stale"); err != nil {
		t.Fatal(err)
	}

	cfg := Config{ShadowMode: false}
	// Review found nothing this round → the stale finding is missed again (2nd).
	req := PostRequest{Anvil: "anvil", PRNumber: 9, HeadSHA: "sha9", Findings: nil}
	res, err := p.Post(context.Background(), cfg, req)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	if res.Resolved != 1 {
		t.Errorf("expected 1 resolution at the 2nd miss, got %d", res.Resolved)
	}
	if len(resolver.resolved) != 1 || resolver.resolved[0] != "THREAD_1" {
		t.Errorf("ResolveThread should have been called with THREAD_1, got %v", resolver.resolved)
	}
	if _, _, misses, resolved := readFindingRow(t, db, "stale"); !resolved || misses != 2 {
		t.Errorf("stale finding should be resolved with misses=2, got misses=%d resolved=%v", misses, resolved)
	}
	// The lookup header must be the finding's hash marker.
	if len(resolver.lookedFor) != 1 || resolver.lookedFor[0] != markerFor("stale") {
		t.Errorf("thread lookup header should be the hash marker, got %v", resolver.lookedFor)
	}
}

func TestPostDoesNotResolveOnFirstMiss(t *testing.T) {
	db := newTestDB(t)
	gh := newStubGh()
	resolver := &stubResolver{threadID: "THREAD_1"}
	p := newPoster(db, gh.exec, resolver)

	mustInsert(t, db, "anvil", 3, Finding{
		File: "gone.go", Anchor: "gone.go:1", Hash: "fresh", Severity: SeverityImportant, Title: "x",
	})
	mustMarkPosted(t, db, "fresh", 222) // misses reset to 0

	cfg := Config{ShadowMode: false}
	req := PostRequest{Anvil: "anvil", PRNumber: 3, HeadSHA: "sha3", Findings: nil}
	res, err := p.Post(context.Background(), cfg, req)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	if res.Resolved != 0 {
		t.Errorf("first miss should not resolve, got Resolved=%d", res.Resolved)
	}
	if len(resolver.resolved) != 0 {
		t.Errorf("ResolveThread should not be called on first miss, got %v", resolver.resolved)
	}
	if _, _, misses, resolved := readFindingRow(t, db, "fresh"); resolved || misses != 1 {
		t.Errorf("expected misses=1 and unresolved, got misses=%d resolved=%v", misses, resolved)
	}
}

func TestPostResetsMissesOnRedetection(t *testing.T) {
	db := newTestDB(t)
	gh := newStubGh()
	p := newPoster(db, gh.exec, nil)

	f := Finding{File: "x.go", Anchor: "x.go:1", Hash: "redetect", Severity: SeverityNit, Title: "x"}
	mustInsert(t, db, "anvil", 4, f)
	mustMarkPosted(t, db, "redetect", 333)
	if err := db.IncrementConsecutiveMiss("redetect"); err != nil { // now at 1
		t.Fatal(err)
	}

	cfg := Config{ShadowMode: false}
	// Re-detected this round → misses reset, not re-posted.
	req := PostRequest{Anvil: "anvil", PRNumber: 4, HeadSHA: "sha4", Findings: []Finding{f}}
	res, err := p.Post(context.Background(), cfg, req)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if res.Posted != 0 {
		t.Errorf("already-posted finding should not be re-posted, got Posted=%d", res.Posted)
	}
	if gh.inlineCalls() != 0 {
		t.Errorf("no inline POST expected for an already-posted finding, got %d", gh.inlineCalls())
	}
	if _, _, misses, _ := readFindingRow(t, db, "redetect"); misses != 0 {
		t.Errorf("misses should reset to 0 on re-detection, got %d", misses)
	}
}

// --- shared insert helpers ------------------------------------------------

func mustInsert(t *testing.T, db *state.DB, anvil string, pr int, f Finding) {
	t.Helper()
	if err := db.InsertFinding(state.Finding{
		Anvil:       anvil,
		PRNumber:    pr,
		HeadSHA:     "sha0",
		FindingHash: f.Hash,
		File:        f.File,
		Anchor:      f.Anchor,
		Severity:    string(f.Severity),
		Category:    f.Category,
		Title:       f.Title,
		Body:        f.Body,
	}); err != nil {
		t.Fatalf("InsertFinding %s: %v", f.Hash, err)
	}
}

func mustMarkPosted(t *testing.T, db *state.DB, hash string, id int64) {
	t.Helper()
	if err := db.MarkFindingPosted(hash, id); err != nil {
		t.Fatalf("MarkFindingPosted %s: %v", hash, err)
	}
}

// --- diff-aware inline routing (Forge-teco) -------------------------------

func TestBuildDiffIndex(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/client/app/Foo.tsx b/client/app/Foo.tsx",
		"--- a/client/app/Foo.tsx",
		"+++ b/client/app/Foo.tsx",
		"@@ -1,2 +18,3 @@ func",
		" ctx18",
		"+added19",
		"+added20",
		"diff --git a/gone.txt b/gone.txt",
		"--- a/gone.txt",
		"+++ /dev/null",
		"@@ -1,1 +0,0 @@",
		"-removed",
	}, "\n")

	idx := buildDiffIndex(diff)
	foo := idx["client/app/Foo.tsx"]
	if foo == nil {
		t.Fatalf("expected Foo.tsx in index, got %v", idx)
	}
	for _, ln := range []int{18, 19, 20} {
		if !foo[ln] {
			t.Errorf("expected line %d anchorable in Foo.tsx", ln)
		}
	}
	if foo[1] || foo[21] {
		t.Errorf("lines 1/21 must not be anchorable: %v", foo)
	}
	if _, ok := idx["gone.txt"]; ok {
		t.Errorf("deleted file (/dev/null) must not be in index")
	}
}

func summaryBody(t *testing.T, gh *stubGh) string {
	t.Helper()
	return gh.summaryBody()
}

func TestPostRoutesOutOfDiffFindingsToSummary(t *testing.T) {
	db := newTestDB(t)
	gh := newStubGh()
	p := newPoster(db, gh.exec, nil)

	diff := strings.Join([]string{
		"diff --git a/client/app/Foo.tsx b/client/app/Foo.tsx",
		"--- a/client/app/Foo.tsx",
		"+++ b/client/app/Foo.tsx",
		"@@ -1,1 +18,3 @@ func",
		" ctx18",
		"+added19",
		"+added20",
	}, "\n")

	findings := []Finding{
		{File: "client/app/Foo.tsx", Anchor: "client/app/Foo.tsx:19", Hash: "in1", Severity: SeverityImportant, Title: "on a changed line"},
		{File: "client/app/Bar.tsx", Anchor: "client/app/Bar.tsx:5", Hash: "out1", Severity: SeverityNit, Title: "orphaned file note", Body: "should be deleted"},
		{File: "client/app/Foo.tsx", Anchor: "client/app/Foo.tsx:1", Hash: "out2", Severity: SeverityNit, Title: "file-level note"},
	}

	res, err := p.Post(context.Background(), Config{}, PostRequest{
		Anvil: "munin", PRNumber: 4219, HeadSHA: "head", WorktreePath: "/wt",
		SummaryLine: "Assay (AI review)", Findings: findings, Diff: diff,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	if res.Posted != 1 {
		t.Errorf("expected 1 inline posted, got %d", res.Posted)
	}
	if res.OutOfDiff != 2 {
		t.Errorf("expected 2 out-of-diff, got %d", res.OutOfDiff)
	}
	if res.Failed != 0 {
		t.Errorf("expected 0 failed (out-of-diff are not attempted), got %d", res.Failed)
	}
	if gh.inlineCalls() != 1 {
		t.Errorf("expected exactly 1 inline gh call, got %d", gh.inlineCalls())
	}

	body := summaryBody(t, gh)
	if !strings.Contains(body, "Findings outside the diff") {
		t.Errorf("summary missing out-of-diff section:\n%s", body)
	}
	if !strings.Contains(body, "orphaned file note") || !strings.Contains(body, "file-level note") {
		t.Errorf("summary missing out-of-diff finding details:\n%s", body)
	}
	if strings.Contains(body, "on a changed line") {
		t.Errorf("in-diff finding should be inline, not in the out-of-diff section:\n%s", body)
	}
}
