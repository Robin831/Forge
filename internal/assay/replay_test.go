package assay

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file is the acceptance evidence for the prompt ordering: two
// consecutive runs over the same pull request — the second one a push later —
// driven through the real prompt builders, against a provider stub that
// emulates a prefix cache. What it measures is the thing no unit test of
// stablePrefix can: not that two runs *could* share bytes, but that the second
// run is actually served them.
//
// --- what the emulation is, and is not ------------------------------------
//
// cacheBlockBytes and cacheBytesPerToken stand in for a provider's block
// granularity and tokenizer. Neither number is a claim about any provider's
// real ones: the assertions are all "at least the stable region, minus one
// block of slack", so a different block size or a tokenizer that packs more
// bytes per token moves the numbers without moving the conclusions.
//
// Deliberately not modelled: cache entry TTLs (a real prefix expires, so a
// run hours later legitimately re-writes it), the minimum prefix length below
// which a provider caches nothing, and pricing. All three change what a hit is
// WORTH; none of them changes where the hit boundary falls, which is the only
// thing this file is a guard for.
const (
	// cacheBlockBytes is the granularity a matched prefix is rounded down to,
	// standing in for a provider's cache block. It is what keeps the
	// assertions off the exact byte count — a shared region that ends one byte
	// earlier than expected is not a regression, and a test that failed on it
	// would be rewritten rather than believed.
	cacheBlockBytes = 1024
	// cacheBytesPerToken converts prompt bytes to the token counts a provider
	// reports. English prose and diffs both sit near four.
	cacheBytesPerToken = 4
)

func cacheTokens(nbytes int) int { return nbytes / cacheBytesPerToken }

// cacheSession is one provider session as the emulated cache saw it.
type cacheSession struct {
	pass     string
	prompt   string
	creation int // tokens written into the cache
	read     int // tokens served from a prefix already there
}

// promptCache emulates a provider-side prompt cache across sessions AND across
// runs. It is the whole point of the harness: the engine's own tests inject a
// runner that returns fixed cache numbers, which proves the plumbing carries
// them but cannot prove any prefix was ever reused. Here the numbers are
// derived from the prompts the real builders produced, so an ordering change
// that puts run-varying bytes back above the diff shows up as a collapsed read.
type promptCache struct {
	mu       sync.Mutex
	stored   []string
	sessions []cacheSession
}

// send prices one session's prompt against everything cached so far and stores
// it. The longest prefix shared with any cached prompt is served as a read
// (rounded down to a block); the rest is written.
func (c *promptCache) send(pass, prompt string) (creation, read int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	best := 0
	for _, stored := range c.stored {
		if n := len(commonPrefix([]string{stored, prompt})); n > best {
			best = n
		}
	}
	hit := best - best%cacheBlockBytes
	read = cacheTokens(hit)
	creation = cacheTokens(len(prompt) - hit)
	c.stored = append(c.stored, prompt)
	c.sessions = append(c.sessions, cacheSession{pass: pass, prompt: prompt, creation: creation, read: read})
	return creation, read
}

// runner is the PassRunner the two runs share. Every pass answers with empty
// findings unless findings is set, which is how the "run 1 reported something"
// case gives run 2 a genuinely different already-reported list.
//
// The first-output callback is invoked AFTER the prompt is priced and stored,
// which is what makes the stagger observable here: the primer's prefix is in
// the cache at the moment the other four are released, exactly as it is when a
// real provider has started answering.
func (c *promptCache) runner(findings string) PassRunner {
	return func(ctx context.Context, pass, _, prompt string) (PassOutput, error) {
		creation, read := c.send(pass, prompt)
		if fn := firstOutputFn(ctx); fn != nil {
			fn()
		}
		out := PassOutput{CacheCreationTokens: creation, CacheReadTokens: read}
		if pass == passTriage.Name {
			out.Text = `{"review_files": [], "notes": ""}`
			return out, nil
		}
		out.Text = `{"findings": []}`
		if findings != "" {
			out.Text = findings
		}
		return out, nil
	}
}

// since returns the sessions recorded after mark — i.e. one run's worth.
func (c *promptCache) since(mark int) []cacheSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cacheSession(nil), c.sessions[mark:]...)
}

func (c *promptCache) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sessions)
}

// totals folds a run's sessions the way the engine folds its passes.
func totals(sessions []cacheSession) (creation, read int) {
	for _, s := range sessions {
		creation += s.creation
		read += s.read
	}
	return creation, read
}

// replayDiff builds a diff big enough that the cache accounting is dominated by
// it rather than by the framing — which is the real shape: the diff is the bulk
// of every Assay prompt, and it is what a lost prefix makes the run pay for
// again per pass.
func replayDiff(file string, hunks int) string {
	var d strings.Builder
	fmt.Fprintf(&d, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n", file, file, file, file)
	for i := range hunks {
		fmt.Fprintf(&d, "@@ -%d,3 +%d,4 @@\n+\tif amount%d < 0 { return errNegative }\n", i, i, i)
	}
	return d.String()
}

// replayGuidance is a REVIEW.md of a size a real one reaches. It is padded
// deliberately: the block rounding means an assertion can only claim the stable
// region minus one block, so a stable region of two blocks would be asserting
// almost nothing. This puts it at several, and the floor with it.
func replayGuidance() string {
	var b strings.Builder
	b.WriteString("Every new endpoint needs an integration test. Prefer table tests.\n\n")
	for i := range 40 {
		fmt.Fprintf(&b, "- Rule %d: money values cross the ledger boundary as minor units; "+
			"flag any float that reaches a balance, a rate or a fee.\n", i)
	}
	return b.String()
}

// replayRequest is one review of PR 347 of anvil munin: same PR, same
// checkout, same repository guidance. head/baseline/diff are what a push moves.
func replayRequest(head, baseline, unifiedDiff string, prior []PriorFinding) ReviewRequest {
	return ReviewRequest{
		Anvil:         "munin",
		PRNumber:      347,
		HeadSHA:       head,
		Title:         "Reject negative charge amounts",
		Description:   "Guard the charge path against negative amounts reaching the ledger.",
		RepoGuidance:  replayGuidance(),
		Diff:          unifiedDiff,
		Incremental:   baseline != "",
		BaselineSHA:   baseline,
		PriorFindings: prior,
	}
}

func replayConfig(c *promptCache, findings string) Config {
	cfg := DefaultConfig().WithRunner(c.runner(findings))
	cfg.primerWaitOverride = 5 * time.Second
	return cfg
}

// TestConsecutiveRunsReadTheStablePrefixFromCache is the acceptance test for
// the ordering work: two runs over one PR, the second a push later, and the
// second run's very first session — which has nothing of its own in the cache
// yet — must be served the whole stable region as a READ rather than paying to
// write it again.
//
// The first session is the one that matters. Later sessions in a run read the
// prefix their own run's primer just wrote, so "every pass reported a read"
// would pass just as well with no cross-run sharing at all. Only the opening
// session of run 2 can distinguish the two, and it is the one this asserts a
// floor on.
//
// The boundary is asserted from both sides. The read must cover the stable
// region (nothing that varies per run may have crept above it, which would
// truncate the hit) and must NOT cover the whole prompt (the incremental
// banner names a new baseline and the diff is new — a run that "read" those
// would mean the emulator, not the ordering, was doing the work).
func TestConsecutiveRunsReadTheStablePrefixFromCache(t *testing.T) {
	cache := &promptCache{}

	// Run 1: the PR's first review, at head cafe0001. Nothing is cached, so
	// the opening session pays to write everything.
	req1 := replayRequest("cafe0001", "", replayDiff("internal/pay/charge.go", 200), nil)
	mark1 := cache.mark()
	res1, err := Review(context.Background(), req1, nil, replayConfig(cache, ""))
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	run1 := cache.since(mark1)
	if len(run1) == 0 {
		t.Fatal("run 1 made no provider sessions")
	}
	if run1[0].read != 0 {
		t.Errorf("run 1 opened with a cache read of %d tokens; the cache was empty", run1[0].read)
	}

	// Run 2: one push later. Same PR, same checkout, same already-reported
	// list — a new head, an incremental scope against run 1's head, and the
	// pushed delta as the diff.
	req2 := replayRequest("cafe0002", "cafe0001", replayDiff("internal/pay/charge.go", 40), nil)
	mark2 := cache.mark()
	res2, err := Review(context.Background(), req2, nil, replayConfig(cache, ""))
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	run2 := cache.since(mark2)
	if len(run2) == 0 {
		t.Fatal("run 2 made no provider sessions")
	}

	stable := stablePrefix(req2)
	if len(stable) <= cacheBlockBytes {
		t.Fatalf("stable prefix is only %d bytes; the fixture cannot demonstrate a block-granular hit", len(stable))
	}
	// One block of slack: the emulator rounds a match down to a block boundary,
	// so the floor is the stable region minus at most one block.
	wantRead := cacheTokens(len(stable) - cacheBlockBytes)

	opening := run2[0]
	if opening.read < wantRead {
		t.Errorf("run 2's first session (%s) read %d cached tokens; want at least %d (the %d-byte stable prefix)",
			opening.pass, opening.read, wantRead, len(stable))
	}
	if opening.creation == 0 {
		t.Error("run 2's first session wrote nothing; the new baseline and the pushed diff must be a miss")
	}
	if got := cacheTokens(len(opening.prompt)); opening.read >= got {
		t.Errorf("run 2's first session read %d of %d prompt tokens; the diff below the stable prefix must not be a hit",
			opening.read, got)
	}

	// The headline number: the run reports a non-zero read, and it is the fold
	// of its sessions — the same accounting the daemon persists on assay_runs.
	if res2.CacheReadTokens <= 0 {
		t.Fatalf("run 2 reported %d cache-read tokens; the shared prefix bought nothing", res2.CacheReadTokens)
	}
	wantCreation, wantReadTotal := totals(run2)
	if res2.CacheCreationTokens != wantCreation || res2.CacheReadTokens != wantReadTotal {
		t.Errorf("run 2 totals = {w:%d r:%d}; sessions summed to {w:%d r:%d}",
			res2.CacheCreationTokens, res2.CacheReadTokens, wantCreation, wantReadTotal)
	}
	if res1.CacheReadTokens < 0 || res1.CacheCreationTokens <= 0 {
		t.Errorf("run 1 totals = {w:%d r:%d}; want a non-zero write", res1.CacheCreationTokens, res1.CacheReadTokens)
	}

	// The measured numbers, logged rather than only asserted: this test is the
	// bead's acceptance evidence, and "PASS" alone does not say what the
	// sharing was worth.
	run1Creation, run1Read := totals(run1)
	t.Logf("run 1: cache_w=%d cache_r=%d over %d sessions", run1Creation, run1Read, len(run1))
	t.Logf("run 2: cache_w=%d cache_r=%d over %d sessions (opening session cache_r=%d, stable prefix %d bytes ≈ %d tokens)",
		res2.CacheCreationTokens, res2.CacheReadTokens, len(run2), opening.read, len(stable), cacheTokens(len(stable)))

	// Every session in run 2 is served the shared opening, not just the first:
	// the primer reads across runs, the other four read what it wrote.
	for _, s := range run2 {
		if s.read < wantRead {
			t.Errorf("run 2 pass %s read %d cached tokens; want at least %d", s.pass, s.read, wantRead)
		}
	}

	// What the sharing was actually worth, measured without the confound of
	// run 1's larger diff: the identical second run against an EMPTY cache.
	// The difference between the two is the write the warm run did not have to
	// pay for, and it must be at least the stable region.
	cold := &promptCache{}
	coldRes, err := Review(context.Background(), req2, nil, replayConfig(cold, ""))
	if err != nil {
		t.Fatalf("cold replay of run 2: %v", err)
	}
	saved := coldRes.CacheCreationTokens - res2.CacheCreationTokens
	t.Logf("same run cold: cache_w=%d; warm: cache_w=%d; saved %d write tokens",
		coldRes.CacheCreationTokens, res2.CacheCreationTokens, saved)
	if saved < wantRead {
		t.Errorf("the warm run saved %d write tokens over the identical cold run; want at least %d "+
			"(the stable prefix it should not have re-written)", saved, wantRead)
	}
}

// TestRedundantCacheWriteTokensIsSumMinusLargest pins the fold behind the
// headline metric. One write of the shared prefix is the intended cost, so the
// redundancy is everything beyond the largest single write — get that
// subtraction wrong and the number the ordering is judged by is wrong in the
// flattering direction.
func TestRedundantCacheWriteTokensIsSumMinusLargest(t *testing.T) {
	shared := &ReviewResult{Passes: []PassReport{
		{Name: "triage", CacheCreationTokens: 41200},
		{Name: "logic", CacheCreationTokens: 900, CacheReadTokens: 41200, Primer: true},
		{Name: "security", CacheCreationTokens: 850, CacheReadTokens: 41500},
		{Name: "conventions", CacheCreationTokens: 800, CacheReadTokens: 41500},
	}}
	if got, want := shared.RedundantCacheWriteTokens(), 2550; got != want {
		t.Errorf("shared run redundancy = %d; want %d", got, want)
	}

	// The regression shape: nothing shared, so every pass wrote the prefix and
	// all but one of those writes was redundant.
	unshared := &ReviewResult{Passes: []PassReport{
		{Name: "triage", CacheCreationTokens: 41200},
		{Name: "logic", CacheCreationTokens: 41500},
		{Name: "security", CacheCreationTokens: 41500},
	}}
	if got, want := unshared.RedundantCacheWriteTokens(), 82700; got != want {
		t.Errorf("unshared run redundancy = %d; want %d", got, want)
	}

	// A backend that reports no cache accounting reports no redundancy, rather
	// than a negative number or a fabricated one.
	quiet := &ReviewResult{Passes: []PassReport{{Name: "logic"}, {Name: "security"}}}
	if got := quiet.RedundantCacheWriteTokens(); got != 0 {
		t.Errorf("run without cache accounting reports redundancy %d; want 0", got)
	}
	if got := (&ReviewResult{}).RedundantCacheWriteTokens(); got != 0 {
		t.Errorf("run with no passes reports redundancy %d; want 0", got)
	}
}

// TestConsecutiveRunsShareTheHeadAboveTheReportedFindings covers the realistic
// second run: run 1 found something, so run 2's already-reported list is
// genuinely different and the cross-run hit CANNOT reach the end of the stable
// region.
//
// What it must still reach is everything above that list — the preamble, the
// repository guidance and the change context — which is why the list is
// ordered last inside stablePrefix. Moving it above the change context (or
// above the guidance) would silently shorten every repeat run's hit to nearly
// nothing, and no other test in this package would notice.
func TestConsecutiveRunsShareTheHeadAboveTheReportedFindings(t *testing.T) {
	cache := &promptCache{}
	unifiedDiff := replayDiff("internal/pay/charge.go", 200)

	found := `{"findings": [{"file": "internal/pay/charge.go", "anchor": "internal/pay/charge.go:12", ` +
		`"category": "logic", "severity": "Important", "title": "negative amount reaches the ledger", ` +
		`"body": "The guard runs after the ledger write, so a negative amount is already recorded."}]}`

	req1 := replayRequest("cafe0001", "", unifiedDiff, nil)
	if _, err := Review(context.Background(), req1, nil, replayConfig(cache, found)); err != nil {
		t.Fatalf("run 1: %v", err)
	}

	// Run 2 carries what run 1 reported, as the daemon's run would: the
	// already-reported list is now non-empty where run 1's was empty.
	prior := []PriorFinding{{
		Anchor:   "internal/pay/charge.go:12",
		Severity: "Important",
		Title:    "negative amount reaches the ledger",
	}}
	req2 := replayRequest("cafe0002", "cafe0001", replayDiff("internal/pay/charge.go", 40), prior)
	mark := cache.mark()
	res2, err := Review(context.Background(), req2, nil, replayConfig(cache, ""))
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	run2 := cache.since(mark)

	// The floor is the region above the already-reported list, which is what
	// stablePrefix renders before priorFindingsSection.
	aboveList := sharedPromptPreamble + repoGuidanceSection(req2) + contextSection(req2)
	if len(aboveList) <= cacheBlockBytes {
		t.Fatalf("the region above the reported list is only %d bytes; too small to assert a block-granular hit", len(aboveList))
	}
	wantRead := cacheTokens(len(aboveList) - cacheBlockBytes)

	if run2[0].read < wantRead {
		t.Errorf("run 2's first session read %d cached tokens; want at least %d (the %d bytes above the already-reported list)",
			run2[0].read, wantRead, len(aboveList))
	}
	if res2.CacheReadTokens <= 0 {
		t.Fatalf("run 2 reported no cache read at all after run 1 recorded a finding")
	}
	// And the new list really did cut the hit short — otherwise the fixture is
	// not exercising the case it claims to.
	full := stablePrefix(req2)
	if run2[0].read >= cacheTokens(len(full)) {
		t.Errorf("run 2's first session read %d tokens, covering the whole %d-byte stable prefix; "+
			"the already-reported list changed and cannot have been a hit", run2[0].read, len(full))
	}
}

// TestRunErrorCarriesCacheTokens: a run that dies still wrote prefixes, and the
// provider billed them. The failure path records cost for exactly this reason,
// and cache accounting recorded as zero there would read back as a run that
// shared everything — the opposite of what happened.
func TestRunErrorCarriesCacheTokens(t *testing.T) {
	cache := &promptCache{}
	cfg := DefaultConfig()
	cfg.primerWaitOverride = 20 * time.Millisecond
	inner := cache.runner("")
	cfg = cfg.WithRunner(func(ctx context.Context, pass, tier, prompt string) (PassOutput, error) {
		out, _ := inner(ctx, pass, tier, prompt)
		if pass == passTriage.Name {
			return out, nil
		}
		// Every deep pass writes its prefix and then fails, which is what a
		// max-turns session does: the prompt was sent and billed, no findings
		// came back.
		perr := newPassError(pass, ReasonMaxTurns, "error_max_turns", nil)
		perr.CacheCreationTokens = out.CacheCreationTokens
		perr.CacheReadTokens = out.CacheReadTokens
		return PassOutput{}, perr
	})

	req := replayRequest("cafe0001", "", replayDiff("internal/pay/charge.go", 200), nil)
	_, err := Review(context.Background(), req, nil, cfg)
	if err == nil {
		t.Fatal("expected the run to fail with every deep pass failing")
	}
	creation, read := RunCacheTokens(err)
	if creation <= 0 {
		t.Errorf("failed run carried %d cache-creation tokens; its sessions were billed for the prefixes they wrote", creation)
	}
	if read <= 0 {
		t.Errorf("failed run carried %d cache-read tokens; its deep passes read the primer's prefix", read)
	}
	// A foreign error carries nothing rather than a fabricated number.
	if c, r := RunCacheTokens(fmt.Errorf("something else")); c != 0 || r != 0 {
		t.Errorf("RunCacheTokens(foreign error) = (%d, %d); want (0, 0)", c, r)
	}
}
