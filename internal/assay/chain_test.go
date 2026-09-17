package assay

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
)

// chainCall is one session a chainRunner was asked for: the pass, the chain
// entry the engine handed it, and the turn budget it was given.
type chainCall struct {
	pass       string
	provider   string
	turnBudget int
}

// chainRunner is a PassRunner stub that answers per (pass, provider kind). A
// session with no scripted response answers like a clean pass on whatever
// provider it was handed, reporting that entry's model.
type chainRunner struct {
	mu     sync.Mutex
	calls  []chainCall
	script map[string][]func(pv provider.Provider) (PassOutput, error)
	idx    map[string]int
}

func newChainRunner() *chainRunner {
	return &chainRunner{
		script: map[string][]func(provider.Provider) (PassOutput, error){},
		idx:    map[string]int{},
	}
}

func chainKey(pass string, kind provider.Kind) string { return pass + "@" + string(kind) }

// on scripts the successive sessions of pass on the given provider kind; the
// last response repeats once the script runs out.
func (r *chainRunner) on(pass string, kind provider.Kind, resp ...func(provider.Provider) (PassOutput, error)) {
	r.script[chainKey(pass, kind)] = resp
}

func (r *chainRunner) run(ctx context.Context, pass, _, _ string) (PassOutput, error) {
	pv, ok := PassProviderFrom(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !ok {
		return PassOutput{}, errors.New("chain walk handed the runner no provider")
	}
	r.calls = append(r.calls, chainCall{pass: pass, provider: string(pv.Kind), turnBudget: turnBudgetFrom(ctx)})
	key := chainKey(pass, pv.Kind)
	seq := r.script[key]
	if len(seq) == 0 {
		return answer(pv), nil
	}
	i := min(r.idx[key], len(seq)-1)
	r.idx[key]++
	return seq[i](pv)
}

// count is how many sessions pass was given on the provider kind; an empty
// pass counts every pass.
func (r *chainRunner) count(pass string, kind provider.Kind) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if (pass == "" || c.pass == pass) && c.provider == string(kind) {
			n++
		}
	}
	return n
}

// answer is a clean session: the pass's JSON contract, a cost, the model.
func answer(pv provider.Provider) PassOutput {
	return PassOutput{
		Text:     `{"findings": [], "review_files": [], "notes": ""}`,
		CostUSD:  0.10,
		TokensIn: 100,
		Turns:    3,
		Model:    pv.Model,
	}
}

func failWith(pass, reason string) func(provider.Provider) (PassOutput, error) {
	return func(pv provider.Provider) (PassOutput, error) {
		perr := newPassError(pass, reason, "provider "+pv.Label()+" "+reason, nil)
		perr.Model = pv.Model
		return PassOutput{}, perr
	}
}

// twoProviderConfig gives EVERY pass the chain [claude/model-a, gemini/model-b],
// so "the other passes never called gemini" is a claim about passes that could
// have, not about passes whose chains never named it.
func twoProviderConfig(r *chainRunner) Config {
	c := DefaultConfig()
	c.AnvilStageProviders = map[string][]string{
		"assay": {"claude/model-a", "gemini/model-b"},
	}
	return c.WithRunner(r.run)
}

func chainPassReport(t *testing.T, res *ReviewResult, name string) PassReport {
	t.Helper()
	for _, p := range res.Passes {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no pass report for %s in %+v", name, res.Passes)
	return PassReport{}
}

// The security pass's first provider is rate limited: that pass, and only that
// pass, moves down its chain. It finishes on the second provider, the other
// passes never call the second provider, and the pass row names where it ran.
func TestRateLimitFailsOverOnlyThatPass(t *testing.T) {
	r := newChainRunner()
	r.on("security", provider.Claude, failWith("security", ReasonRateLimited))

	res, err := Review(context.Background(), testRequest(), openTestDB(t), twoProviderConfig(r))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Status != RunStatusComplete {
		t.Fatalf("status = %s (%v); want complete — the failover recovered the pass", res.Status, res.PassErrors)
	}

	if n := r.count("security", provider.Claude); n != 1 {
		t.Errorf("security sessions on claude = %d, want 1", n)
	}
	if n := r.count("security", provider.Gemini); n != 1 {
		t.Errorf("security sessions on gemini = %d, want 1", n)
	}
	if n := r.count("", provider.Gemini); n != 1 {
		t.Errorf("gemini sessions across the run = %d, want 1 (security's alone): %+v", n, r.calls)
	}

	sec := chainPassReport(t, res, "security")
	if sec.Provider != "gemini" || sec.Model != "model-b" || !sec.FailedOver || sec.TerminationReason != "" {
		t.Errorf("security report = %+v; want gemini/model-b, failed over, answered", sec)
	}
	for _, name := range PassNames() {
		if name == "security" {
			continue
		}
		p := chainPassReport(t, res, name)
		if p.Provider != "claude" || p.Model != "model-a" || p.FailedOver {
			t.Errorf("%s report = %+v; want claude/model-a on its head", name, p)
		}
	}

	line := res.PassTelemetryText()
	if !strings.Contains(line, "pass=security turns=3 term=success cost_usd=0.1000 model=model-b provider=gemini") {
		t.Errorf("telemetry %q does not name the failover", line)
	}
	if strings.Count(line, "provider=") != 1 {
		t.Errorf("telemetry %q: provider= must appear on the failed-over pass alone", line)
	}
	if strings.Count(line, "model=") != len(res.Passes) {
		t.Errorf("telemetry %q: every pass must carry model=", line)
	}

	// The spend is split by where it ran and still sums to the run.
	var claude, gemini float64
	for _, pu := range res.UsageByProvider {
		switch pu.Provider {
		case "claude":
			claude = pu.Usage.EstimatedCostUSD
		case "gemini":
			gemini = pu.Usage.EstimatedCostUSD
		}
	}
	if gemini < 0.099 || gemini > 0.101 {
		t.Errorf("gemini usage = %v, want the security pass's 0.10", gemini)
	}
	if d := claude + gemini - res.Usage.EstimatedCostUSD; d > 1e-9 || d < -1e-9 {
		t.Errorf("by-provider usage %v + %v does not sum to the run's %v", claude, gemini, res.Usage.EstimatedCostUSD)
	}
}

// An auth failure never fails over: the credential is broken, and a pass that
// quietly ran on the next provider would hide it. The pass fails once, on its
// head, and the run reports the gap.
func TestAuthFailureDoesNotFailOver(t *testing.T) {
	r := newChainRunner()
	r.on("security", provider.Claude, failWith("security", ReasonAuthFailed))

	res, err := Review(context.Background(), testRequest(), openTestDB(t), twoProviderConfig(r))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if n := r.count("security", provider.Claude); n != 1 {
		t.Errorf("security sessions on claude = %d, want exactly 1 — no retry loop", n)
	}
	if n := r.count("", provider.Gemini); n != 0 {
		t.Errorf("gemini sessions = %d, want 0: %+v", n, r.calls)
	}
	if res.Status != RunStatusPartial {
		t.Fatalf("status = %s; want partial", res.Status)
	}
	if len(res.FailedPasses) != 1 || res.FailedPasses[0] != (PassFailure{Name: "security", Reason: ReasonAuthFailed}) {
		t.Errorf("failed passes = %+v; want security — auth_failed", res.FailedPasses)
	}
	if sec := chainPassReport(t, res, "security"); sec.Provider != "claude" || sec.FailedOver {
		t.Errorf("security report = %+v; want it left on claude", sec)
	}
}

// Triage is a hard gate, so an auth failure there fails the run — still without
// touching the next provider.
func TestTriageAuthFailureDoesNotFailOver(t *testing.T) {
	r := newChainRunner()
	r.on(passTriage.Name, provider.Claude, failWith(passTriage.Name, ReasonAuthFailed))

	_, err := Review(context.Background(), testRequest(), openTestDB(t), twoProviderConfig(r))
	if err == nil {
		t.Fatal("Review succeeded; want the triage auth failure surfaced")
	}
	if n := r.count("", provider.Gemini); n != 0 {
		t.Errorf("gemini sessions = %d, want 0", n)
	}
	if n := r.count(passTriage.Name, provider.Claude); n != 1 {
		t.Errorf("triage sessions on claude = %d, want 1", n)
	}
}

// A rate-limited triage head falls back like a deep pass does, and the run
// goes on.
func TestTriageRateLimitFailsOver(t *testing.T) {
	r := newChainRunner()
	r.on(passTriage.Name, provider.Claude, failWith(passTriage.Name, ReasonRateLimited))

	res, err := Review(context.Background(), testRequest(), openTestDB(t), twoProviderConfig(r))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	tr := chainPassReport(t, res, passTriage.Name)
	if tr.Provider != "gemini" || tr.Model != "model-b" || !tr.FailedOver {
		t.Errorf("triage report = %+v; want gemini/model-b, failed over", tr)
	}
	if n := r.count("", provider.Gemini); n != 1 {
		t.Errorf("gemini sessions = %d, want triage's 1", n)
	}
}

// The in-provider recoveries run to completion before a failover is considered:
// a pass that exhausts its turn budget is re-run on the SAME provider, and only
// when that retry is rate limited does it move on.
func TestTurnBudgetRetryStaysOnProviderBeforeFailover(t *testing.T) {
	r := newChainRunner()
	r.on("logic", provider.Claude,
		failWith("logic", ReasonMaxTurns),
		failWith("logic", ReasonRateLimited),
	)

	res, err := Review(context.Background(), testRequest(), openTestDB(t), twoProviderConfig(r))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	var logic []chainCall
	for _, c := range r.calls {
		if c.pass == "logic" {
			logic = append(logic, c)
		}
	}
	if len(logic) != 3 || logic[0].provider != "claude" || logic[1].provider != "claude" || logic[2].provider != "gemini" {
		t.Fatalf("logic sessions = %+v; want claude, claude (the turn-budget retry), then gemini", logic)
	}
	if logic[1].turnBudget >= logic[0].turnBudget {
		t.Errorf("retry budget %d not reduced from %d — the second claude session was not the turn-budget retry",
			logic[1].turnBudget, logic[0].turnBudget)
	}
	p := chainPassReport(t, res, "logic")
	if p.Provider != "gemini" || !p.FailedOver || p.TerminationReason != "" {
		t.Errorf("logic report = %+v; want answered on gemini after failing over", p)
	}
	// The retry on claude is still telemetry once the pass leaves claude: the
	// failover is not a retry, and it must not erase the one that happened.
	if p.Attempts != 2 || !p.Retried || p.RetrySkipped {
		t.Errorf("logic retry telemetry = attempts %d, retried %v, skipped %v; want 2, true, false",
			p.Attempts, p.Retried, p.RetrySkipped)
	}
	if line := res.PassTelemetryText(); !strings.Contains(line, "pass=logic turns=3 term=success retry=1") {
		t.Errorf("telemetry %q does not carry logic's retry=1 past the failover", line)
	}
}

// The primer is the one pass the rest of the fan-out waits on, and its chain
// walk runs inside that wait. A rate-limited primer head emits no model output,
// so the release must come from the FALLBACK session's first token — the
// first-output callback has to survive the walk re-wrapping the context — and
// not from the primer's whole chain returning or from primerWait running out.
func TestRateLimitedPrimerFailoverReleasesFanOutOnFallbackOutput(t *testing.T) {
	primer := deepPasses[primerPass].Name
	othersStarted := make(chan struct{})
	var startOnce sync.Once
	var mu sync.Mutex
	var releasedBeforePrimerReturned bool
	primerReturned := false

	runner := func(ctx context.Context, pass, _, _ string) (PassOutput, error) {
		pv, ok := PassProviderFrom(ctx)
		if !ok {
			return PassOutput{}, errors.New("no provider handed to the runner")
		}
		switch {
		case pass == primer && pv.Kind == provider.Claude:
			// Rate limited before any model output: no signal.
			return failWith(pass, ReasonRateLimited)(pv)
		case pass == primer:
			fn := firstOutputFn(ctx)
			if fn == nil {
				return PassOutput{}, errors.New("fallback primer session lost the first-output callback")
			}
			fn()
			// Hold the session open until the others start, so only the
			// signal — not the primer returning — can have released them.
			select {
			case <-othersStarted:
			case <-time.After(5 * time.Second):
			}
			mu.Lock()
			primerReturned = true
			mu.Unlock()
			return answer(pv), nil
		case pass != passTriage.Name:
			mu.Lock()
			if !primerReturned {
				releasedBeforePrimerReturned = true
			}
			mu.Unlock()
			startOnce.Do(func() { close(othersStarted) })
		}
		return answer(pv), nil
	}

	cfg := DefaultConfig()
	cfg.AnvilStageProviders = map[string][]string{"assay": {"claude/model-a", "gemini/model-b"}}
	cfg.primerWaitOverride = 30 * time.Second // never the thing that releases them here
	cfg = cfg.WithRunner(runner)

	start := time.Now()
	res, err := Review(context.Background(), testRequest(), openTestDB(t), cfg)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("review took %v; the fan-out waited on a timeout rather than the fallback's first output", elapsed)
	}
	mu.Lock()
	released := releasedBeforePrimerReturned
	mu.Unlock()
	if !released {
		t.Error("the other passes started only after the primer returned; the fallback's first output did not open the barrier")
	}
	p := chainPassReport(t, res, primer)
	if p.Provider != "gemini" || p.Model != "model-b" || !p.FailedOver || !p.Primer {
		t.Errorf("primer report = %+v; want the primer, failed over to gemini/model-b", p)
	}
	if res.Status != RunStatusComplete {
		t.Errorf("status = %s (%v); want complete", res.Status, res.PassErrors)
	}
}

// The strict-JSON re-prompt is an in-provider recovery too.
func TestJSONRepromptStaysOnProviderBeforeFailover(t *testing.T) {
	r := newChainRunner()
	r.on("conventions", provider.Claude,
		func(pv provider.Provider) (PassOutput, error) {
			return PassOutput{Text: "not json", Model: pv.Model}, nil
		},
		failWith("conventions", ReasonRateLimited),
	)

	res, err := Review(context.Background(), testRequest(), openTestDB(t), twoProviderConfig(r))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if n := r.count("conventions", provider.Claude); n != 2 {
		t.Errorf("conventions sessions on claude = %d, want 2 (answer, then the re-prompt)", n)
	}
	if n := r.count("conventions", provider.Gemini); n != 1 {
		t.Errorf("conventions sessions on gemini = %d, want 1", n)
	}
	if p := chainPassReport(t, res, "conventions"); p.Provider != "gemini" || !p.FailedOver {
		t.Errorf("conventions report = %+v; want it to finish on gemini", p)
	}
}

// Every entry rate limited: the pass reports the rate limit it ended on, once
// per entry, and does not loop back to the head.
func TestExhaustedChainReportsRateLimit(t *testing.T) {
	r := newChainRunner()
	r.on("security", provider.Claude, failWith("security", ReasonRateLimited))
	r.on("security", provider.Gemini, failWith("security", ReasonRateLimited))

	res, err := Review(context.Background(), testRequest(), openTestDB(t), twoProviderConfig(r))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if n := r.count("security", provider.Claude) + r.count("security", provider.Gemini); n != 2 {
		t.Errorf("security sessions = %d, want 2 (one per entry)", n)
	}
	if len(res.FailedPasses) != 1 || res.FailedPasses[0].Reason != ReasonRateLimited {
		t.Errorf("failed passes = %+v; want security — rate_limited", res.FailedPasses)
	}
	if p := chainPassReport(t, res, "security"); p.Provider != "gemini" || !p.FailedOver {
		t.Errorf("security report = %+v; want it to name the last entry tried", p)
	}
}

// The production runner spawns the chain entry the walk handed it, not its
// chain's head, and reports an auth rejection as auth_failed.
func TestSmithRunnerSpawnsTheHandedProvider(t *testing.T) {
	var got provider.Provider
	orig := spawnPassSession
	spawnPassSession = func(_ context.Context, _, _, _ string, pv provider.Provider, _ []string, _ smith.SpawnOptions) (*smith.Process, error) {
		got = pv
		return nil, errors.New("stub spawn")
	}
	t.Cleanup(func() { spawnPassSession = orig })

	req := testRequest()
	req.WorkDir = t.TempDir()
	cfg := DefaultConfig()
	cfg.AnvilStageProviders = map[string][]string{"assay": {"claude/model-a", "gemini/model-b"}}
	run := newSmithRunner(cfg, req)

	fallback := provider.Provider{Kind: provider.Gemini, Model: "model-b"}
	_, err := run(withPassProvider(context.Background(), fallback), "logic", tierReview, "prompt")
	if got.Kind != provider.Gemini || got.Model != "model-b" {
		t.Errorf("spawned %s, want the handed gemini/model-b", got.Label())
	}
	var pe *PassError
	if !errors.As(err, &pe) || pe.Model != "model-b" {
		t.Errorf("spawn failure = %v; want a PassError naming model-b", err)
	}
}

func TestSessionOutcomeClassifiesAuthFailure(t *testing.T) {
	pv := provider.Provider{Kind: provider.Claude, Model: "model-a"}
	_, err := sessionOutcome("logic", newCostTracker(0), &turnCounter{}, &smith.Result{AuthFailed: true, IsError: true, ExitCode: 2}, pv)
	var pe *PassError
	if !errors.As(err, &pe) || pe.Reason != ReasonAuthFailed || pe.Model != "model-a" {
		t.Fatalf("outcome = %v; want an auth_failed PassError on model-a", err)
	}
	if failsOver(classifyPassError("logic", err)) {
		t.Error("an auth failure must not fail over")
	}

	out, err := sessionOutcome("logic", newCostTracker(0), &turnCounter{},
		&smith.Result{ResultSubtype: "success", Output: `{"findings":[]}`, Model: "claude-opus-5"}, pv)
	if err != nil || out.Model != "claude-opus-5" {
		t.Errorf("answered outcome = %+v, %v; want the in-band model", out, err)
	}
}
