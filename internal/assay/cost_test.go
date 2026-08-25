package assay

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
)

func TestCostTrackerDisabled(t *testing.T) {
	for _, limit := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		tr := newCostTracker(limit)
		if tr.enabled() {
			t.Errorf("limit %v: enabled = true; want a disabled tracker", limit)
		}
		if tr.AddTurnCost(1000, 10, 10) {
			t.Errorf("limit %v: AddTurnCost reported the ceiling reached on a disabled tracker", limit)
		}
		if tr.Exceeded() {
			t.Errorf("limit %v: Exceeded = true on a disabled tracker", limit)
		}
		if got := tr.TotalUSD(); got != 0 {
			t.Errorf("limit %v: TotalUSD = %v; want 0 — a disabled tracker accumulates nothing", limit, got)
		}
	}
	// A nil tracker is the same as a disabled one, so no call site has to
	// nil-check before asking.
	var nilTracker *costTracker
	if nilTracker.enabled() || nilTracker.Exceeded() || nilTracker.TotalUSD() != 0 || nilTracker.LimitUSD() != 0 {
		t.Error("a nil tracker must read as disabled and empty")
	}
}

func TestCostTrackerAccumulatesAcrossTurns(t *testing.T) {
	tr := newCostTracker(1.00)
	if tr.AddTurnCost(0.30, 100, 0) {
		t.Error("ceiling reported reached after $0.30 of a $1.00 budget")
	}
	if tr.AddTurnCost(0.30, 0, 200) {
		t.Error("ceiling reported reached after $0.60 of a $1.00 budget")
	}
	if !tr.AddTurnCost(0.40, 50, 300) {
		t.Error("ceiling not reported reached at exactly the $1.00 budget")
	}
	if !tr.Exceeded() {
		t.Error("Exceeded = false at exactly the limit; the boundary spends the budget")
	}
	usd, turns, cc, cr := tr.Snapshot()
	if math.Abs(usd-1.00) > 1e-9 {
		t.Errorf("total = %v; want 1.00", usd)
	}
	// Turns and cache tokens ride along because a stopped session emits no
	// result event: this snapshot is the only accounting it has.
	if turns != 3 {
		t.Errorf("turns = %d; want 3", turns)
	}
	if cc != 150 || cr != 500 {
		t.Errorf("cache accounting = {w:%d r:%d}; want {150 500}", cc, cr)
	}
}

func TestCostTrackerIgnoresUnusableTurnCosts(t *testing.T) {
	tr := newCostTracker(1.00)
	for _, usd := range []float64{-5, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if tr.AddTurnCost(usd, -1, -1) {
			t.Errorf("turn cost %v tripped the ceiling", usd)
		}
	}
	usd, turns, cc, cr := tr.Snapshot()
	if usd != 0 {
		t.Errorf("total = %v; want 0 — an unusable per-turn cost must not move the total in either direction", usd)
	}
	if turns != 4 {
		t.Errorf("turns = %d; want 4 — the turns happened even if their cost was unreadable", turns)
	}
	if cc != 0 || cr != 0 {
		t.Errorf("cache accounting = {w:%d r:%d}; want {0 0} for negative token counts", cc, cr)
	}
}

// assistantEvent builds the stream event Claude emits for one answered turn.
func assistantEvent(t *testing.T, model string, in, out, cacheW, cacheR int) smith.StreamEvent {
	t.Helper()
	msg := map[string]any{
		"model": model,
		"usage": map[string]any{
			"input_tokens":                in,
			"output_tokens":               out,
			"cache_creation_input_tokens": cacheW,
			"cache_read_input_tokens":     cacheR,
		},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshalling assistant message: %v", err)
	}
	return smith.StreamEvent{Type: "assistant", Message: raw}
}

func TestTurnCostUSDPricesAnsweredTurns(t *testing.T) {
	pv := provider.Provider{Kind: provider.Claude}
	ev := assistantEvent(t, "claude-sonnet", 1_000_000, 0, 0, 0)
	usd, cc, cr, ok := turnCostUSD(ev, pv)
	if !ok {
		t.Fatal("an assistant event carrying usage must yield a turn cost")
	}
	// 1M input tokens at the claude-sonnet row ($3.00/M).
	if math.Abs(usd-3.00) > 1e-9 {
		t.Errorf("usd = %v; want 3.00 (1M input tokens at the sonnet rate)", usd)
	}
	if cc != 0 || cr != 0 {
		t.Errorf("cache accounting = {w:%d r:%d}; want {0 0}", cc, cr)
	}

	// The model named on the turn wins over the configured hint: the hint is
	// routinely empty ("let the provider pick") and the turn says what it did.
	ev = assistantEvent(t, "claude-opus-4-8", 1_000_000, 0, 500, 900)
	usd, cc, cr, _ = turnCostUSD(ev, pv)
	// 1M input at $15.00/M + 500 cache-write at $18.75/M + 900 cache-read at
	// $1.50/M — the cache lines are priced too, since they are billed too.
	want := 15.00 + 500*18.75/1e6 + 900*1.50/1e6
	if math.Abs(usd-want) > 1e-9 {
		t.Errorf("usd = %v; want %v (opus rate inferred from the turn's own model)", usd, want)
	}
	if cc != 500 || cr != 900 {
		t.Errorf("cache accounting = {w:%d r:%d}; want {500 900}", cc, cr)
	}
}

func TestTurnCostUSDIgnoresNonTurnEvents(t *testing.T) {
	pv := provider.Provider{Kind: provider.Claude, Model: "claude-sonnet"}
	cases := []struct {
		name string
		ev   smith.StreamEvent
	}{
		// The result event carries the whole session's total_cost_usd, which is
		// the sum of the turns already counted — billing it again would double
		// every session.
		{"result", smith.StreamEvent{Type: "result", TotalCostUSD: 4.20}},
		{"system init", smith.StreamEvent{Type: "system", Subtype: "init"}},
		// Gemini-style deltas carry no usage, so their sessions are never
		// stopped on cost rather than being stopped on a guess.
		{"gemini delta", smith.StreamEvent{Type: "message", Role: "assistant", Content: "hi"}},
		{"assistant without usage", smith.StreamEvent{Type: "assistant", Message: json.RawMessage(`{"content":[]}`)}},
		{"assistant with unparseable message", smith.StreamEvent{Type: "assistant", Message: json.RawMessage(`not json`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if usd, _, _, ok := turnCostUSD(tc.ev, pv); ok || usd != 0 {
				t.Errorf("turnCostUSD = (%v, ok=%v); want (0, false)", usd, ok)
			}
		})
	}
}

func TestConfigCostCeilingNormalises(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want float64
	}{
		{0, 0}, {-2, 0}, {math.NaN(), 0}, {math.Inf(1), 0}, {1.5, 1.5},
	} {
		cfg := DefaultConfig()
		cfg.MaxCostPerPassUSD = tc.in
		if got := cfg.costCeilingUSD(); got != tc.want {
			t.Errorf("costCeilingUSD(%v) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func TestFromAssayConfigCarriesCostCeiling(t *testing.T) {
	// Unset: the on-disk default applies, so a deployment that never heard of
	// the key still gets a ceiling.
	var ac config.AssayConfig
	if got := FromAssayConfig(ac).costCeilingUSD(); got != ac.GetMaxCostPerPassUSD() {
		t.Errorf("unset ceiling = %v; want the config default %v", got, ac.GetMaxCostPerPassUSD())
	}

	// Explicitly off. This is why the field is read unconditionally rather than
	// behind a "> 0" guard: 0 is the only way to turn a default-on ceiling off.
	off := 0.0
	ac.MaxCostPerPassUSD = &off
	if got := FromAssayConfig(ac).costCeilingUSD(); got != 0 {
		t.Errorf("explicit 0 ceiling = %v; want 0 (disabled)", got)
	}

	set := 2.75
	ac.MaxCostPerPassUSD = &set
	if got := FromAssayConfig(ac).costCeilingUSD(); got != 2.75 {
		t.Errorf("ceiling = %v; want 2.75", got)
	}
}

func TestSessionAnswered(t *testing.T) {
	cases := []struct {
		name string
		res  *smith.Result
		want bool
	}{
		{"nil", nil, false},
		{"success", &smith.Result{ResultSubtype: "success"}, true},
		// Claude exits 2 on its own rate-limit code even after recovering; the
		// subtype, not the exit code, is what says the model answered.
		{"success despite exit code", &smith.Result{ResultSubtype: "success", ExitCode: 2}, true},
		{"success but aborted", &smith.Result{ResultSubtype: "success", IsError: true}, false},
		{"max turns", &smith.Result{ResultSubtype: ReasonMaxTurns}, false},
		{"killed mid-session", &smith.Result{ExitCode: -1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionAnswered(tc.res); got != tc.want {
				t.Errorf("sessionAnswered = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestCostStopError(t *testing.T) {
	// Nothing to report when no ceiling is in force, however the session ended.
	if perr := costStopError("logic", newCostTracker(0), &smith.Result{ExitCode: -1}); perr != nil {
		t.Errorf("costStopError with no ceiling = %v; want nil", perr)
	}
	// Nothing to report when the budget is intact.
	tr := newCostTracker(1.50)
	tr.AddTurnCost(0.20, 0, 0)
	if perr := costStopError("logic", tr, &smith.Result{ExitCode: -1}); perr != nil {
		t.Errorf("costStopError under the ceiling = %v; want nil", perr)
	}

	// Crossed, and the session died with it: a stop, named as one.
	tr.AddTurnCost(1.31, 900, 41500)
	perr := costStopError("logic", tr, &smith.Result{ExitCode: -1})
	if perr == nil {
		t.Fatal("costStopError = nil for a session killed past its ceiling")
	}
	if perr.Pass != "logic" || perr.Reason != ReasonMaxCost {
		t.Errorf("PassError = {%s %s}; want {logic %s}", perr.Pass, perr.Reason, ReasonMaxCost)
	}
	// The message has to say which knob to move and by how much, or the
	// operator is left reading a bare label.
	for _, want := range []string{"2 turns", "$1.51", "$1.50", "max_cost_per_pass_usd"} {
		if !strings.Contains(perr.Error(), want) {
			t.Errorf("message %q does not contain %q", perr.Error(), want)
		}
	}
	// The killed session emitted no result event, so the tracker's accounting
	// is the only record of what it already cost.
	if math.Abs(perr.CostUSD-1.51) > 1e-9 || perr.Turns != 2 {
		t.Errorf("telemetry = {cost:%v turns:%d}; want {1.51 2}", perr.CostUSD, perr.Turns)
	}
	if perr.CacheCreationTokens != 900 || perr.CacheReadTokens != 41500 {
		t.Errorf("cache telemetry = {w:%d r:%d}; want {900 41500}", perr.CacheCreationTokens, perr.CacheReadTokens)
	}

	// Crossed on the very turn the model answered on: nothing was cut short,
	// so it is not a stop and the pass keeps the answer it paid for.
	if perr := costStopError("logic", tr, &smith.Result{ResultSubtype: "success"}); perr != nil {
		t.Errorf("costStopError for a session that answered = %v; want nil — nothing was stopped", perr)
	}
}

// maxCostErr is the error the runner produces for a session stopped at its
// spend ceiling. It is built by the production path so the scripted stub and
// the real runner cannot report the stop differently.
func maxCostErr(pass string, turns int, usd float64) *PassError {
	tr := newCostTracker(1.50)
	for i := 0; i < turns; i++ {
		tr.AddTurnCost(usd/float64(turns), 0, 0)
	}
	return costStopError(pass, tr, &smith.Result{ExitCode: -1})
}

// TestReviewReportsCostStopAsPartialNotSuccess is the bead-32 honesty check for
// the new stop: a pass Forge cut short must reach every reporting surface as a
// named failure, never as a quietly short review.
func TestReviewReportsCostStopAsPartialNotSuccess(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {{text: triageJSON(t, nil, ""), turns: 3, cost: 0.01}}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: findingsJSON(t, nil), turns: 4, cost: 0.02}}
	}
	script["logic"] = []stubResp{{err: maxCostErr("logic", 9, 1.51)}}

	runner := newScriptRunner(script)
	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(runner.run))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if res.Status != RunStatusPartial {
		t.Errorf("Status = %q; want %q — four of five passes reviewed the head", res.Status, RunStatusPartial)
	}
	if len(res.FailedPasses) != 1 || res.FailedPasses[0].Name != "logic" || res.FailedPasses[0].Reason != ReasonMaxCost {
		t.Fatalf("FailedPasses = %+v; want one {logic %s}", res.FailedPasses, ReasonMaxCost)
	}
	// The stop reason is a label, so it survives the sanitiser that guards the
	// public PR comment.
	if got := sanitizeReason(ReasonMaxCost); got != ReasonMaxCost {
		t.Errorf("sanitizeReason(%q) = %q; the stop reason must survive as itself", ReasonMaxCost, got)
	}

	// One session, not two: a cost stop is not retried, or the retry buys the
	// identical runaway again at full price.
	if calls := runner.callsFor("logic"); len(calls) != 1 {
		t.Errorf("logic sessions = %d; want 1 — a cost stop must not be retried", len(calls))
	}
	rep := passReport(res, "logic")
	if rep == nil {
		t.Fatal("no PassReport for logic")
	}
	if rep.TerminationReason != ReasonMaxCost {
		t.Errorf("TerminationReason = %q; want %q", rep.TerminationReason, ReasonMaxCost)
	}
	if rep.Retried || rep.Attempts != 1 {
		t.Errorf("telemetry = {Retried:%v Attempts:%d}; want {false 1}", rep.Retried, rep.Attempts)
	}
	// A stop is not a refund: what the session burned before it was cut off is
	// still billed, and still counted.
	if math.Abs(rep.CostUSD-1.51) > 1e-9 {
		t.Errorf("CostUSD = %v; want 1.51 — the stopped session was still billed", rep.CostUSD)
	}

	// The three renderings an operator actually reads.
	if got := res.StatusText(); !strings.Contains(got, ReasonMaxCost) || !strings.Contains(got, "logic") {
		t.Errorf("StatusText = %q; want it to name logic and %s", got, ReasonMaxCost)
	}
	if got := res.PassTelemetryText(); !strings.Contains(got, "term="+ReasonMaxCost) {
		t.Errorf("PassTelemetryText = %q; want term=%s", got, ReasonMaxCost)
	}
	if got := PartialCoverageNote(res.FailedPasses); !strings.Contains(got, ReasonMaxCost) {
		t.Errorf("PartialCoverageNote = %q; want it to name %s", got, ReasonMaxCost)
	}
}
