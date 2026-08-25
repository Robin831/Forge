package assay

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/smith"
)

// prefixFixture is a review request with every optional shared section
// populated, so the prompt head under test is the full one rather than the
// degenerate empty case.
func prefixFixture() (ReviewRequest, string, string) {
	var d strings.Builder
	d.WriteString("diff --git a/internal/pay/charge.go b/internal/pay/charge.go\n")
	d.WriteString("--- a/internal/pay/charge.go\n+++ b/internal/pay/charge.go\n")
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&d, "@@ -%d,3 +%d,4 @@\n+\tif amount%d < 0 { return errNegative }\n", i, i, i)
	}
	req := ReviewRequest{
		Anvil:        "munin",
		PRNumber:     347,
		Title:        "Reject negative charge amounts",
		Description:  "Guard the charge path against negative amounts reaching the ledger.",
		RepoGuidance: "Every new endpoint needs an integration test. Prefer table tests.",
		Incremental:  true,
		BaselineSHA:  "cafebabecafebabe",
		PriorFindings: []PriorFinding{
			{Anchor: "internal/pay/charge.go:12", Severity: "Nit", Title: "name the constant"},
			{Anchor: "internal/pay/ledger.go:88", Severity: "Important", Title: "unchecked error", Resolved: true},
		},
		ElidedFiles: []string{"client/package-lock.json", "web/yarn.lock"},
	}
	return req, d.String(), "Focus on the ledger boundary; the retry loop is new."
}

// commonPrefix returns the longest byte prefix shared by every string.
func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		n := min(len(p), len(s))
		i := 0
		for i < n && p[i] == s[i] {
			i++
		}
		p = p[:i]
	}
	return p
}

// TestDeepPassPromptsShareCachePrefix is the regression guard for the inverted
// prompt order. A prompt cache matches from the first byte, so a single
// pass-specific string moved above the shared head — a pass name, an index, a
// per-pass heading — costs the run its shared prefix and puts it back to paying
// a full-price cache write of the whole diff once per pass.
func TestDeepPassPromptsShareCachePrefix(t *testing.T) {
	req, unifiedDiff, notes := prefixFixture()

	prompts := make([]string, 0, len(deepPasses))
	shortest := -1
	for _, p := range deepPasses {
		got, err := buildPassPrompt(p, req, unifiedDiff, notes)
		if err != nil {
			t.Fatalf("buildPassPrompt(%s): %v", p.Name, err)
		}
		prompts = append(prompts, got)
		if shortest < 0 || len(got) < shortest {
			shortest = len(got)
		}
	}

	// The shared head is not merely "some" common prefix: every prompt must
	// start with exactly the bytes writeSharedPromptHead produces.
	var head strings.Builder
	writeSharedPromptHead(&head, req, unifiedDiff, notes)
	for i, p := range deepPasses {
		if !strings.HasPrefix(prompts[i], head.String()) {
			t.Fatalf("pass %s does not open with the shared head; prompt starts:\n%.400s", p.Name, prompts[i])
		}
	}

	lcp := commonPrefix(prompts)
	if len(lcp) < len(head.String()) {
		t.Fatalf("common prefix (%d bytes) is shorter than the shared head (%d bytes)", len(lcp), len(head.String()))
	}

	// Everything the head is supposed to carry has to be inside the shared
	// prefix, the diff included — the diff is the bulk of the tokens and the
	// whole reason the ordering was inverted.
	for _, want := range []string{
		sharedPromptPreamble,
		"Repository Review Guidance",
		"integration test",
		"Change Context",
		"Reject negative charge amounts",
		"Incremental Review",
		"cafebabecafe",
		"Already-Reported Findings",
		"unchecked error",
		"Triage Notes",
		"the retry loop is new",
		unifiedDiff,
	} {
		if !strings.Contains(lcp, want) {
			t.Errorf("shared prefix is missing %q", want)
		}
	}

	// No pass may identify itself above the divergence point.
	for _, p := range deepPasses {
		if strings.Contains(lcp, p.Name) {
			t.Errorf("pass name %q leaked into the shared prefix", p.Name)
		}
	}

	if got := float64(len(lcp)) / float64(shortest); got < 0.8 {
		t.Errorf("shared prefix covers only %.1f%% of the shortest prompt (%d/%d bytes); want >= 80%%",
			got*100, len(lcp), shortest)
	}
}

// TestTriagePromptSharesHeadWithDeepPasses records the secondary saving: when
// triage does not narrow the file set, scopeDiffToFiles returns the filtered
// diff unchanged and triage's prompt shares its whole head — diff included —
// with the deep passes. It is a consequence of building both heads from one
// function, not a guarantee: the moment triage narrows anything the two diffs
// differ, which is exactly why the fan-out is primed from a deep pass.
func TestTriagePromptSharesHeadWithDeepPasses(t *testing.T) {
	req, unifiedDiff, _ := prefixFixture()

	scoped := scopeDiffToFiles(unifiedDiff, nil)
	if scoped != unifiedDiff {
		t.Fatalf("expected an empty file list to leave the diff unchanged")
	}

	triagePrompt, err := buildTriagePrompt(req, unifiedDiff)
	if err != nil {
		t.Fatalf("buildTriagePrompt: %v", err)
	}
	// No triage notes: on the run where triage has not answered yet there are
	// none, which is the case that shares.
	deepPrompt, err := buildPassPrompt(deepPasses[0], req, scoped, "")
	if err != nil {
		t.Fatalf("buildPassPrompt: %v", err)
	}

	lcp := commonPrefix([]string{triagePrompt, deepPrompt})
	if !strings.Contains(lcp, unifiedDiff) {
		t.Errorf("triage and deep-pass prompts do not share the diff; shared prefix is %d bytes", len(lcp))
	}
}

// --- staggered fan-out ----------------------------------------------------

// stagger stubs a PassRunner that reports every session start on a channel and
// holds the primer pass at two separate points: primerSignal gates its first
// token (the first-output callback) and primerReturn gates its return.
//
// The two gates are what make the barrier's release condition observable. With
// one gate the callback and the primer's own unconditional defer primed.open()
// fire in the same instant, so a wiring regression — a runner that ignored the
// callback, a gate only ever opened by the defer — would still let the other
// four passes start and every assertion would still pass, while the fan-out had
// silently degraded from "wait for the primer's first token" to "wait for the
// whole primer session".
type stagger struct {
	t             *testing.T
	started       chan string
	primerSignal  chan struct{}
	primerReturn  chan struct{}
	primerHasSeam chan bool
	cache         map[string][2]int
	mu            sync.Mutex
}

func newStagger(t *testing.T) *stagger {
	return &stagger{
		t:             t,
		started:       make(chan string, 16),
		primerSignal:  make(chan struct{}),
		primerReturn:  make(chan struct{}),
		primerHasSeam: make(chan bool, 1),
		cache:         map[string][2]int{},
	}
}

func (s *stagger) run(ctx context.Context, pass, _, _ string) (PassOutput, error) {
	s.started <- pass
	if pass == passTriage.Name {
		return PassOutput{Text: `{"review_files": [], "notes": ""}`}, nil
	}
	if pass == deepPasses[primerPass].Name {
		fn := firstOutputFn(ctx)
		s.primerHasSeam <- fn != nil
		<-s.primerSignal
		if fn != nil {
			fn()
		}
		// Still "streaming": the session has emitted its first token but has
		// not finished, so anything that starts now was released by the
		// callback and not by this pass returning.
		<-s.primerReturn
	}
	s.mu.Lock()
	tok := s.cache[pass]
	s.mu.Unlock()
	return PassOutput{Text: `{"findings": []}`, CacheCreationTokens: tok[0], CacheReadTokens: tok[1]}, nil
}

// releasePrimer lets the primer emit its first token and then finish, for the
// tests whose subject is not the barrier itself.
func (s *stagger) releasePrimer() {
	close(s.primerSignal)
	close(s.primerReturn)
}

// awaitStart returns the next pass to start, failing the test on timeout.
func (s *stagger) awaitStart() string {
	s.t.Helper()
	select {
	case p := <-s.started:
		return p
	case <-time.After(5 * time.Second):
		s.t.Fatal("timed out waiting for a pass to start")
		return ""
	}
}

// TestReviewHoldsFanOutUntilPrimerAnswers asserts the scheduling half of the
// shared-prefix change: the primer pass runs alone until the provider starts
// answering it (which is when the prefix lands in the cache), and only then are
// the other four released. Launched together they would all miss the cache and
// all pay to write the identical prefix — the state PR #5261 was observed in,
// with all five passes starting in the same millisecond.
//
// The release is pinned to the primer's FIRST TOKEN specifically: the stub
// holds the primer's session open after signalling, so the four passes start
// while it is still running. "Released when the primer returns" — the
// degradation a broken first-output wiring falls back to, which serialises the
// run behind a whole session — fails here rather than passing quietly.
func TestReviewHoldsFanOutUntilPrimerAnswers(t *testing.T) {
	s := newStagger(t)
	cfg := DefaultConfig()
	cfg.primerWaitOverride = 30 * time.Second // never the thing that releases them here
	cfg = cfg.WithRunner(s.run)

	done := make(chan *ReviewResult, 1)
	go func() {
		res, err := Review(context.Background(), ReviewRequest{Anvil: "munin", PRNumber: 1, Diff: "diff --git a/x b/x\n+x\n"}, nil, cfg)
		if err != nil {
			t.Errorf("Review: %v", err)
			done <- nil
			return
		}
		done <- res
	}()

	if got := s.awaitStart(); got != passTriage.Name {
		t.Fatalf("first session was %q, want triage", got)
	}
	if got := s.awaitStart(); got != deepPasses[primerPass].Name {
		t.Fatalf("first deep pass was %q, want the primer %q", got, deepPasses[primerPass].Name)
	}
	select {
	case has := <-s.primerHasSeam:
		if !has {
			t.Error("primer session ran without the first-output callback; the barrier has nothing to open it")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the primer to report its context")
	}

	// Nothing else may start while the primer is still working towards its
	// first token.
	select {
	case p := <-s.started:
		t.Fatalf("pass %q started before the primer answered", p)
	case <-time.After(150 * time.Millisecond):
	}

	// Let the primer emit its first token — and nothing else. Its session is
	// still open (it blocks on primerReturn), so the four passes below can only
	// have been released by the first-output callback, never by the primer's
	// unconditional open-on-return.
	close(s.primerSignal)

	rest := map[string]bool{}
	for range len(deepPasses) - 1 {
		rest[s.awaitStart()] = true
	}
	for i, p := range deepPasses {
		if i == primerPass {
			continue
		}
		if !rest[p.Name] {
			t.Errorf("pass %q never ran after the primer answered", p.Name)
		}
	}

	close(s.primerReturn)

	res := <-done
	if res == nil {
		t.Fatal("Review returned no result")
	}
	if res.Status != RunStatusComplete {
		t.Fatalf("status = %q, want complete", res.Status)
	}
	primer := passReport(res, deepPasses[primerPass].Name)
	if primer == nil || !primer.Primer {
		t.Errorf("primer pass %q not marked as the primer in its report", deepPasses[primerPass].Name)
	}
	for i, p := range deepPasses {
		if i == primerPass {
			continue
		}
		if r := passReport(res, p.Name); r != nil && r.Primer {
			t.Errorf("pass %q marked as primer; only one pass may be", p.Name)
		}
	}
}

// TestReviewReleasesFanOutWhenPrimerNeverSignals covers the backstop. A
// provider that streams no structured events (or a primer wedged before its
// first token) has no signal to give, and the run must lose the cache saving
// rather than the four remaining passes.
func TestReviewReleasesFanOutWhenPrimerNeverSignals(t *testing.T) {
	s := newStagger(t)
	cfg := DefaultConfig()
	cfg.primerWaitOverride = 20 * time.Millisecond
	cfg = cfg.WithRunner(s.run)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := Review(context.Background(), ReviewRequest{Anvil: "munin", PRNumber: 2, Diff: "diff --git a/x b/x\n+x\n"}, nil, cfg); err != nil {
			t.Errorf("Review: %v", err)
		}
	}()

	// Drain the primer's seam report so it does not block, then wait for the
	// other four to start while the primer is still held.
	go func() { <-s.primerHasSeam }()

	seen := map[string]bool{}
	for range len(deepPasses) + 1 { // triage + every deep pass
		seen[s.awaitStart()] = true
	}
	for _, p := range deepPasses {
		if !seen[p.Name] {
			t.Errorf("pass %q did not start within the fallback wait", p.Name)
		}
	}

	s.releasePrimer()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Review did not finish")
	}
}

// TestPassReportCarriesCacheTelemetry asserts the per-pass cache accounting
// reaches PassReport and the rendered telemetry line — the metric the whole
// change is measured by ("sum minus max of cache_creation per run") has to be
// computable from a log line, not only from a bill.
func TestPassReportCarriesCacheTelemetry(t *testing.T) {
	s := newStagger(t)
	s.cache[deepPasses[primerPass].Name] = [2]int{41500, 0}
	s.cache[deepPasses[1].Name] = [2]int{900, 41500}
	cfg := DefaultConfig().WithRunner(s.run)
	cfg.primerWaitOverride = 20 * time.Millisecond

	go func() { <-s.primerHasSeam; s.releasePrimer() }()
	go func() {
		for range s.started {
		}
	}()

	res, err := Review(context.Background(), ReviewRequest{Anvil: "munin", PRNumber: 3, Diff: "diff --git a/x b/x\n+x\n"}, nil, cfg)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	primer := passReport(res, deepPasses[primerPass].Name)
	if primer == nil || primer.CacheCreationTokens != 41500 || primer.CacheReadTokens != 0 {
		t.Fatalf("primer cache telemetry = %+v, want w=41500 r=0", primer)
	}
	reader := passReport(res, deepPasses[1].Name)
	if reader == nil || reader.CacheCreationTokens != 900 || reader.CacheReadTokens != 41500 {
		t.Fatalf("second pass cache telemetry = %+v, want w=900 r=41500", reader)
	}

	line := res.PassTelemetryText()
	for _, want := range []string{"cache_w=41500 cache_r=0", "cache_w=900 cache_r=41500", "primer=1"} {
		if !strings.Contains(line, want) {
			t.Errorf("telemetry line missing %q:\n%s", want, line)
		}
	}
	// A pass whose provider reported no cache accounting renders as it always
	// did, with no zero-valued noise.
	if strings.Contains(line, "cache_w=0 cache_r=0") {
		t.Errorf("telemetry line renders empty cache accounting:\n%s", line)
	}
}

// TestIsModelOutputReleasesOnlyOnModelOutput pins the barrier's release
// condition — the one decision the real smith runner makes on every streamed
// event, and the one the assay tests otherwise never reach (they inject a stub
// runner and call the gate function directly).
//
// The `system`/init event is the case the whole switch exists for: Claude emits
// it before the model request is made at all, so releasing on it would put all
// five passes back into the simultaneous-miss race. Adding an event type here
// without meaning to — or renaming one out from under it — would restore that
// silently, since the only symptom is a cache_w number in a log line.
func TestIsModelOutputReleasesOnlyOnModelOutput(t *testing.T) {
	cases := []struct {
		name string
		ev   smith.StreamEvent
		want bool
	}{
		{"claude init", smith.StreamEvent{Type: "system", Subtype: "init"}, false},
		{"claude assistant", smith.StreamEvent{Type: "assistant"}, true},
		{"claude tool result", smith.StreamEvent{Type: "user"}, false},
		{"result", smith.StreamEvent{Type: "result", Subtype: "success"}, true},
		{"rate limit", smith.StreamEvent{Type: "rate_limit_event"}, false},
		// Gemini deltas: the reader accumulates role "assistant" and ignores
		// role "user", which is the provider echoing the prompt back before the
		// model has been asked anything.
		{"gemini assistant delta", smith.StreamEvent{Type: "message", Role: "assistant", Content: "th"}, true},
		{"gemini prompt echo", smith.StreamEvent{Type: "message", Role: "user", Content: "review this"}, false},
		{"unlabelled delta", smith.StreamEvent{Type: "message", Content: "th"}, true},
		{"unknown type", smith.StreamEvent{Type: "tool_use"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isModelOutput(tc.ev); got != tc.want {
				t.Errorf("isModelOutput(%+v) = %v; want %v", tc.ev, got, tc.want)
			}
		})
	}
}
