package assay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
)

// turnOf is one billed message's usage as the tracker takes it: a priced cost
// and the prompt-cache halves behind it. The tests drive the tracker with the
// same shape turnCostUSD hands it.
func turnOf(usd float64, cacheCreation, cacheRead int) cost.Usage {
	return cost.Usage{EstimatedCostUSD: usd, CacheWriteTokens: cacheCreation, CacheReadTokens: cacheRead}
}

func TestCostTrackerDisabled(t *testing.T) {
	for _, limit := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		tr := newCostTracker(limit)
		if tr.enabled() {
			t.Errorf("limit %v: enabled = true; want a disabled tracker", limit)
		}
		if tr.AddTurnCost("msg_1", turnOf(1000, 10, 10)) {
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
	if tr.AddTurnCost("msg_1", turnOf(0.30, 100, 0)) {
		t.Error("ceiling reported reached after $0.30 of a $1.00 budget")
	}
	if tr.AddTurnCost("msg_2", turnOf(0.30, 0, 200)) {
		t.Error("ceiling reported reached after $0.60 of a $1.00 budget")
	}
	if !tr.AddTurnCost("msg_3", turnOf(0.40, 50, 300)) {
		t.Error("ceiling not reported reached at exactly the $1.00 budget")
	}
	if !tr.Exceeded() {
		t.Error("Exceeded = false at exactly the limit; the boundary spends the budget")
	}
	snap, turns := tr.Snapshot()
	usd, cc, cr := snap.EstimatedCostUSD, snap.CacheWriteTokens, snap.CacheReadTokens
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
	for i, usd := range []float64{-5, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if tr.AddTurnCost(fmt.Sprintf("msg_%d", i), turnOf(usd, -1, -1)) {
			t.Errorf("turn cost %v tripped the ceiling", usd)
		}
	}
	snap, turns := tr.Snapshot()
	usd, cc, cr := snap.EstimatedCostUSD, snap.CacheWriteTokens, snap.CacheReadTokens
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

// assistantEvent builds the stream event Claude emits for one content block of
// an answered turn. Real transcripts repeat the id and the usage block across
// every block of one message; see TestCostTrackerBillsOneMessageOnce.
func assistantEvent(t *testing.T, id, model string, in, out, cacheW, cacheR int) smith.StreamEvent {
	t.Helper()
	msg := map[string]any{
		"id":    id,
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
	ev := assistantEvent(t, "msg_a", "claude-sonnet", 1_000_000, 0, 0, 0)
	id, u, ok := turnCostUSD(ev, pv)
	if !ok {
		t.Fatal("an assistant event carrying usage must yield a turn cost")
	}
	usd, cc, cr := u.EstimatedCostUSD, u.CacheWriteTokens, u.CacheReadTokens
	// The id rides out so the tracker can bill the message once however many
	// content-block events carry it.
	if id != "msg_a" {
		t.Errorf("id = %q; want msg_a", id)
	}
	// The input and output counts ride out with it: a session stopped at the
	// ceiling reports these instead of the result event it never emitted.
	if u.InputTokens != 1_000_000 {
		t.Errorf("input tokens = %d; want 1000000", u.InputTokens)
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
	ev = assistantEvent(t, "msg_b", "claude-opus-4-8", 1_000_000, 0, 500, 900)
	_, u, _ = turnCostUSD(ev, pv)
	usd, cc, cr = u.EstimatedCostUSD, u.CacheWriteTokens, u.CacheReadTokens
	// 1M input at $5.00/M + 500 cache-write at $6.25/M + 900 cache-read at
	// $0.50/M — the cache lines are priced too, since they are billed too.
	want := 5.00 + 500*6.25/1e6 + 900*0.50/1e6
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
			if _, u, ok := turnCostUSD(tc.ev, pv); ok || !u.IsZero() {
				t.Errorf("turnCostUSD = (%+v, ok=%v); want (the zero usage, false)", u, ok)
			}
		})
	}
}

func TestFromAssayConfigCarriesCostCeiling(t *testing.T) {
	// Unset: the on-disk default applies, so a deployment that never heard of
	// the key still gets a ceiling.
	var ac config.AssayConfig
	if got := FromAssayConfig(ac).MaxCostPerPassUSD; got != ac.GetMaxCostPerPassUSD() {
		t.Errorf("unset ceiling = %v; want the config default %v", got, ac.GetMaxCostPerPassUSD())
	}

	// Explicitly off. This is why the field is read unconditionally rather than
	// behind a "> 0" guard: 0 is the only way to turn a default-on ceiling off.
	off := 0.0
	ac.MaxCostPerPassUSD = &off
	if tr := newCostTracker(FromAssayConfig(ac).MaxCostPerPassUSD); tr.enabled() {
		t.Error("an explicit 0 ceiling must reach the tracker as disabled")
	}

	set := 2.75
	ac.MaxCostPerPassUSD = &set
	if got := FromAssayConfig(ac).MaxCostPerPassUSD; got != 2.75 {
		t.Errorf("ceiling = %v; want 2.75", got)
	}
	if got := newCostTracker(FromAssayConfig(ac).MaxCostPerPassUSD).LimitUSD(); got != 2.75 {
		t.Errorf("tracker limit = %v; want 2.75", got)
	}
}

// TestCostStopExceptionUsesSmithAnswered pins the predicate the cost-stop
// exception reads: smith's own definition of a session that answered, not a
// copy of it in this package.
func TestCostStopExceptionUsesSmithAnswered(t *testing.T) {
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
			if got := tc.res.Answered(); got != tc.want {
				t.Errorf("Answered = %v; want %v", got, tc.want)
			}
			// A crossed ceiling is a stop for exactly the sessions that did not
			// answer, which is the whole of the exception.
			tr := newCostTracker(1.00)
			tr.AddTurnCost("msg_1", turnOf(2.00, 0, 0))
			perr := costStopError("logic", tr, tc.res)
			if (perr == nil) != tc.want {
				t.Errorf("costStopError = %v for Answered=%v; a stop is reported only for a session that did not answer", perr, tc.want)
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
	tr.AddTurnCost("msg_1", turnOf(0.20, 0, 0))
	if perr := costStopError("logic", tr, &smith.Result{ExitCode: -1}); perr != nil {
		t.Errorf("costStopError under the ceiling = %v; want nil", perr)
	}

	// Crossed, and the session died with it: a stop, named as one.
	tr.AddTurnCost("msg_2", turnOf(1.31, 900, 41500))
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

// TestCostStopErrorCarriesEveryTokenColumn is the regression for a stopped pass
// recording dollars and cache tokens beside input=0/output=0. Those counters
// reach daily_costs, provider_daily_costs and bead_costs together, so a row
// with real money and empty token columns is internally inconsistent in exactly
// the tables this accounting exists to make trustworthy. Every counter
// therefore comes out of the one tracker snapshot.
func TestCostStopErrorCarriesEveryTokenColumn(t *testing.T) {
	pv := provider.Provider{Kind: provider.Claude}
	tr := newCostTracker(1.00)
	for _, ev := range []smith.StreamEvent{
		assistantEvent(t, "msg_1", "claude-sonnet", 400_000, 7, 900, 41500),
		assistantEvent(t, "msg_2", "claude-sonnet", 400_000, 5, 0, 20000),
	} {
		id, u, ok := turnCostUSD(ev, pv)
		if !ok {
			t.Fatalf("event %s carries no usage", id)
		}
		tr.AddTurnCost(id, u)
	}
	perr := costStopError("logic", tr, &smith.Result{ExitCode: -1})
	if perr == nil {
		t.Fatal("costStopError = nil for a session killed past its ceiling")
	}
	if perr.TokensIn != 800_000 || perr.TokensOut != 12 {
		t.Errorf("token telemetry = {in:%d out:%d}; want {800000 12} — the same snapshot the dollars came from",
			perr.TokensIn, perr.TokensOut)
	}
	if perr.CacheCreationTokens != 900 || perr.CacheReadTokens != 61500 {
		t.Errorf("cache telemetry = {w:%d r:%d}; want {900 61500}", perr.CacheCreationTokens, perr.CacheReadTokens)
	}
	if perr.CostUSD <= 0 {
		t.Fatalf("CostUSD = %v; want the spend that crossed the ceiling", perr.CostUSD)
	}
	// And it survives the projection every cost sink reads it through, so the
	// row that lands is the whole session, not its dollars alone.
	u, turns, _, est := passErrorTelemetry(perr)
	if u.InputTokens != 800_000 || u.OutputTokens != 12 || u.CacheWriteTokens != 900 || u.CacheReadTokens != 61500 || turns != 2 {
		t.Errorf("passErrorTelemetry = {%+v turns:%d}; want the tracker's whole accounting", u, turns)
	}
	if math.Abs(u.EstimatedCostUSD-perr.CostUSD) > 1e-9 {
		t.Errorf("usage cost %v disagrees with the error's %v", u.EstimatedCostUSD, perr.CostUSD)
	}
	// The estimate rides out beside the money. A stopped session has no
	// provider figure at all, so this is the one dollar number it can be read
	// against its own ceiling by.
	if math.Abs(est-perr.CostUSD) > 1e-9 {
		t.Errorf("estimate %v disagrees with the error's cost %v; a stopped session reports one snapshot under both names", est, perr.CostUSD)
	}
}

// maxCostErr is the error the runner produces for a session stopped at its
// spend ceiling. It is built by the production path so the scripted stub and
// the real runner cannot report the stop differently.
func maxCostErr(pass string, turns int, usd float64) *PassError {
	tr := newCostTracker(1.50)
	for i := 0; i < turns; i++ {
		tr.AddTurnCost(fmt.Sprintf("msg_%d", i), turnOf(usd/float64(turns), 0, 0))
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
	// Not "skipped" either: a cost stop was never eligible for the turn-budget
	// re-run, and reporting it as a dropped retry would send an operator
	// looking for a retry policy that had nothing to do with this pass.
	if rep.RetrySkipped {
		t.Error("RetrySkipped = true; a cost stop is not an eligible retry that was dropped")
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

// realToolUseTurn returns the three stream events Claude actually emitted for
// one tool-using turn, read from testdata/claude_tool_use_turn.jsonl — a
// verbatim `claude --output-format stream-json` transcript excerpt with only
// the content bodies elided (the message envelope, ids and usage blocks are
// untouched). Keeping a real fixture is what makes a CLI format change visible
// here rather than as a ceiling that silently fires at a third of its value.
func realToolUseTurn(t *testing.T) []smith.StreamEvent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "claude_tool_use_turn.jsonl"))
	if err != nil {
		t.Fatalf("reading transcript fixture: %v", err)
	}
	var evs []smith.StreamEvent
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var ev smith.StreamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decoding transcript line: %v", err)
		}
		evs = append(evs, ev)
	}
	return evs
}

// TestCostTrackerBillsOneMessageOnce is the regression for the double-count:
// Claude emits an assistant event per content block, each repeating the same
// message id and the same usage block, so billing per event charges a
// tool-using turn two or three times over and reaches the ceiling at a
// fraction of the configured spend.
func TestCostTrackerBillsOneMessageOnce(t *testing.T) {
	pv := provider.Provider{Kind: provider.Claude}
	evs := realToolUseTurn(t)
	if len(evs) != 3 {
		t.Fatalf("fixture has %d events; want the 3 content blocks of one message", len(evs))
	}

	// The fixture is one message: same id, same usage, three events.
	var firstID string
	for i, ev := range evs {
		id, u, ok := turnCostUSD(ev, pv)
		if !ok {
			t.Fatalf("event %d: carries no usage", i)
		}
		usd, cc, cr := u.EstimatedCostUSD, u.CacheWriteTokens, u.CacheReadTokens
		if i == 0 {
			firstID = id
		} else if id != firstID {
			t.Fatalf("event %d: id %q; want all three blocks to share %q", i, id, firstID)
		}
		if cc != 41895 || cr != 19553 {
			t.Errorf("event %d: cache accounting = {w:%d r:%d}; want the same {41895 19553} on every block", i, cc, cr)
		}
		if usd <= 0 {
			t.Errorf("event %d: usd = %v; want a priced turn", i, usd)
		}
	}

	// Billed through the tracker, the three events cost what one message costs.
	tr := newCostTracker(1000)
	for _, ev := range evs {
		id, u, _ := turnCostUSD(ev, pv)
		tr.AddTurnCost(id, u)
	}
	snap, turns := tr.Snapshot()
	usd, cc, cr := snap.EstimatedCostUSD, snap.CacheWriteTokens, snap.CacheReadTokens
	if turns != 1 {
		t.Errorf("turns = %d; want 1 — three content blocks are one billed message (the provider's own num_turns counts it once)", turns)
	}
	if cc != 41895 || cr != 19553 {
		t.Errorf("cache accounting = {w:%d r:%d}; want {41895 19553} counted once, not tripled", cc, cr)
	}
	_, oneTurn, _ := turnCostUSD(evs[0], pv)
	one := oneTurn.EstimatedCostUSD
	if math.Abs(usd-one) > 1e-9 {
		t.Errorf("total = %v; want %v — one message's cost, not three", usd, one)
	}

	// A second, distinct message is new spend and is added.
	next := assistantEvent(t, "msg_second", "claude-fable-5", 1_000_000, 0, 0, 0)
	nid, nu, _ := turnCostUSD(next, pv)
	tr.AddTurnCost(nid, nu)
	if _, turns := tr.Snapshot(); turns != 2 {
		t.Errorf("turns = %d after a second message id; want 2", turns)
	}

	// A repeat that arrives after the ceiling was already reached still reports
	// it as reached — a duplicate does not un-cross the line.
	tight := newCostTracker(0.01)
	if !tight.AddTurnCost("msg_x", turnOf(5, 0, 0)) {
		t.Fatal("a $5 turn against a $0.01 ceiling must report it reached")
	}
	if !tight.AddTurnCost("msg_x", turnOf(5, 0, 0)) {
		t.Error("a repeat of an already-billed message must still report the ceiling reached")
	}
	if got := tight.TotalUSD(); got != 5 {
		t.Errorf("total = %v; want 5 — the repeat must not be added again", got)
	}

	// An empty id cannot be deduplicated, so it keeps the historical per-event
	// accounting rather than collapsing every unlabelled turn into one.
	anon := newCostTracker(1000)
	anon.AddTurnCost("", turnOf(0.25, 0, 0))
	anon.AddTurnCost("", turnOf(0.25, 0, 0))
	if got, turns := anon.Snapshot(); got.EstimatedCostUSD != 0.50 || turns != 2 {
		t.Errorf("unlabelled turns = {usd:%v turns:%d}; want {0.5 2}", got.EstimatedCostUSD, turns)
	}
}

// TestCostStopCallbackStopsAtTheCrossing exercises the callback the pass
// session actually installs — the thing that turns "the tracker says exceeded"
// into a killed process. Every other test of the ceiling drives the pieces
// directly or hands the scripted runner a pre-built PassError, so without this
// the feature itself is unexecuted.
func TestCostStopCallbackStopsAtTheCrossing(t *testing.T) {
	pv := provider.Provider{Kind: provider.Claude, Model: "claude-sonnet"}

	t.Run("bills turns and stops once at the crossing", func(t *testing.T) {
		tr := newCostTracker(3.00)
		var stops int
		cb := costStopCallback(tr, pv, nil, func() { stops++ })

		// $3.00/M at the sonnet input rate: half a million tokens a turn.
		cb(assistantEvent(t, "msg_1", "claude-sonnet", 500_000, 0, 0, 0))
		if stops != 0 {
			t.Fatalf("stopped after $1.50 of a $3.00 budget (stops=%d)", stops)
		}
		cb(assistantEvent(t, "msg_2", "claude-sonnet", 500_000, 0, 0, 0))
		if stops != 1 {
			t.Fatalf("stops = %d at exactly the $3.00 budget; want 1", stops)
		}
		// Events already buffered in the reader keep arriving after the kill;
		// stopping twice says nothing the first stop did not.
		cb(assistantEvent(t, "msg_3", "claude-sonnet", 500_000, 0, 0, 0))
		cb(assistantEvent(t, "msg_3", "claude-sonnet", 500_000, 0, 0, 0))
		if stops != 1 {
			t.Errorf("stops = %d after further events; want 1", stops)
		}
	})

	t.Run("non-turn events are not billed", func(t *testing.T) {
		tr := newCostTracker(0.01)
		var stops int
		cb := costStopCallback(tr, pv, nil, func() { stops++ })
		cb(smith.StreamEvent{Type: "system", Subtype: "init"})
		cb(smith.StreamEvent{Type: "user"})
		cb(smith.StreamEvent{Type: "result", TotalCostUSD: 99})
		cb(smith.StreamEvent{Type: "assistant", Message: json.RawMessage(`{"content":[]}`)})
		if stops != 0 {
			t.Errorf("stops = %d; want 0 — only a billed message may trip the ceiling", stops)
		}
		if snap, turns := tr.Snapshot(); snap.EstimatedCostUSD != 0 || turns != 0 {
			t.Errorf("tracker = {usd:%v turns:%d}; want {0 0}", snap.EstimatedCostUSD, turns)
		}
	})

	t.Run("a disabled tracker never stops", func(t *testing.T) {
		var stops int
		cb := costStopCallback(newCostTracker(0), pv, nil, func() { stops++ })
		for i := 0; i < 5; i++ {
			cb(assistantEvent(t, fmt.Sprintf("msg_%d", i), "claude-opus-4-8", 5_000_000, 0, 0, 0))
		}
		if stops != 0 {
			t.Errorf("stops = %d with no ceiling configured; want 0 — an unconfigured anvil runs the session it always did", stops)
		}
	})

	t.Run("a nil stop is never called", func(t *testing.T) {
		// The stagger installs the callback on sessions with no ceiling in
		// force, so the tracker half must tolerate having nothing to cancel.
		cb := costStopCallback(newCostTracker(0.01), pv, nil, nil)
		cb(assistantEvent(t, "msg_1", "claude-sonnet", 500_000, 0, 0, 0))
	})

	t.Run("the stagger signal still fires when both share the hook", func(t *testing.T) {
		// The guard that installs this callback became a disjunction when the
		// ceiling joined the stagger on OnStreamEvent; the release must still
		// happen exactly once, and never on Claude's opening init event, which
		// is emitted before the model request is made at all.
		tr := newCostTracker(3.00)
		var signals, stops int
		cb := costStopCallback(tr, pv, func() { signals++ }, func() { stops++ })

		cb(smith.StreamEvent{Type: "system", Subtype: "init"})
		if signals != 0 {
			t.Fatal("the fan-out was released on the init event, restoring the simultaneous-miss race the stagger exists to break")
		}
		cb(assistantEvent(t, "msg_1", "claude-sonnet", 100_000, 0, 0, 0))
		if signals != 1 {
			t.Fatalf("signals = %d after the first model output; want 1", signals)
		}
		cb(assistantEvent(t, "msg_2", "claude-sonnet", 100_000, 0, 0, 0))
		if signals != 1 {
			t.Errorf("signals = %d; want 1 — the barrier releases once", signals)
		}
		if stops != 0 {
			t.Errorf("stops = %d well under the ceiling; want 0", stops)
		}
	})

	t.Run("the stagger signal fires with the ceiling disabled", func(t *testing.T) {
		// The configuration of an anvil that set max_cost_per_pass_usd: 0.
		// The release must happen BEFORE the callback's cost guard returns —
		// hoisting `!tracker.enabled()` above the signal block (the natural
		// simplification, since everything else here is cost work) would leave
		// those anvils' passes waiting out primerWaitDefault and then fanning
		// out simultaneously: the all-miss cache write the stagger exists to
		// prevent, and nothing would fail, so only this test holds it.
		var signals, stops int
		cb := costStopCallback(newCostTracker(0), pv, func() { signals++ }, func() { stops++ })

		cb(smith.StreamEvent{Type: "system", Subtype: "init"})
		if signals != 0 {
			t.Fatal("the fan-out was released on the init event, restoring the simultaneous-miss race the stagger exists to break")
		}
		cb(assistantEvent(t, "msg_1", "claude-opus-4-8", 5_000_000, 0, 0, 0))
		if signals != 1 {
			t.Fatalf("signals = %d after the first model output with the ceiling off; want 1", signals)
		}
		cb(assistantEvent(t, "msg_2", "claude-opus-4-8", 5_000_000, 0, 0, 0))
		if signals != 1 {
			t.Errorf("signals = %d; want 1 — the barrier releases once", signals)
		}
		if stops != 0 {
			t.Errorf("stops = %d with no ceiling configured; want 0", stops)
		}
	})

	t.Run("a real tool-using turn is billed once through the callback", func(t *testing.T) {
		// One message, three content-block events: the ceiling must be reached
		// on the session's real spend, not on three times it.
		tr := newCostTracker(1000)
		cb := costStopCallback(tr, provider.Provider{Kind: provider.Claude}, nil, func() {})
		for _, ev := range realToolUseTurn(t) {
			cb(ev)
		}
		if snap, turns := tr.Snapshot(); turns != 1 || snap.CacheWriteTokens != 41895 || snap.CacheReadTokens != 19553 {
			t.Errorf("tracker = {turns:%d w:%d r:%d}; want {1 41895 19553}", turns, snap.CacheWriteTokens, snap.CacheReadTokens)
		}
	})
}

// TestSessionOutcomeAsksTheCeilingFirst pins the branch order a stopped session
// depends on. A session killed mid-stream reports its death in whatever shape
// the kill produced — and the rate-limit flag read off a truncated stream is
// one of those shapes, which classified first would send a deliberate stop
// round the retry path that buys the identical runaway again.
func TestSessionOutcomeAsksTheCeilingFirst(t *testing.T) {
	pv := provider.Provider{Kind: provider.Claude}
	stopped := func() *costTracker {
		tr := newCostTracker(1.50)
		tr.AddTurnCost("msg_1", turnOf(1.60, 900, 41500))
		return tr
	}

	// A truncated stream that also set RateLimited is still a cost stop.
	_, err := sessionOutcome("logic", stopped(), nil, &smith.Result{ExitCode: -1, RateLimited: true}, pv)
	var perr *PassError
	if !errors.As(err, &perr) || perr.Reason != ReasonMaxCost {
		t.Fatalf("outcome = %v; want a %s PassError", err, ReasonMaxCost)
	}
	// It carries the tracker's accounting, not the killed session's zeros.
	if math.Abs(perr.CostUSD-1.60) > 1e-9 || perr.Turns != 1 || perr.CacheCreationTokens != 900 || perr.CacheReadTokens != 41500 {
		t.Errorf("telemetry = {cost:%v turns:%d w:%d r:%d}; want the tracker's {1.60 1 900 41500}",
			perr.CostUSD, perr.Turns, perr.CacheCreationTokens, perr.CacheReadTokens)
	}

	// A subtype the provider did report is likewise not allowed to relabel a stop.
	_, err = sessionOutcome("logic", stopped(), nil, &smith.Result{ExitCode: 1, IsError: true, ResultSubtype: ReasonMaxTurns}, pv)
	if !errors.As(err, &perr) || perr.Reason != ReasonMaxCost {
		t.Errorf("outcome = %v; want %s to win over the provider's own subtype", err, ReasonMaxCost)
	}

	// With no ceiling crossed the historical classification is untouched.
	intact := newCostTracker(1.50)
	_, err = sessionOutcome("logic", intact, nil, &smith.Result{ExitCode: 1, RateLimited: true, NumTurns: 4, CostUSD: 0.12}, pv)
	if !errors.As(err, &perr) || perr.Reason != ReasonRateLimited {
		t.Fatalf("outcome = %v; want %s", err, ReasonRateLimited)
	}
	if perr.Turns != 4 || math.Abs(perr.CostUSD-0.12) > 1e-9 {
		t.Errorf("telemetry = {turns:%d cost:%v}; want the result's {4 0.12} — a failure is not a refund", perr.Turns, perr.CostUSD)
	}
	_, err = sessionOutcome("logic", intact, nil, &smith.Result{ExitCode: 1, IsError: true, ResultSubtype: ReasonMaxTurns, NumTurns: 30}, pv)
	if !errors.As(err, &perr) || perr.Reason != ReasonMaxTurns {
		t.Errorf("outcome = %v; want %s", err, ReasonMaxTurns)
	}
	_, err = sessionOutcome("logic", intact, nil, &smith.Result{ExitCode: 2, IsError: true}, pv)
	if !errors.As(err, &perr) || perr.Reason != ReasonProviderFailed {
		t.Errorf("outcome = %v; want %s when the provider named no subtype", err, ReasonProviderFailed)
	}

	// The success path, including a session that answered on the very turn that
	// crossed the ceiling: nothing was cut short, so it keeps its answer.
	out, err := sessionOutcome("logic", stopped(), nil, &smith.Result{ResultSubtype: "success", FullOutput: "{}", NumTurns: 7, CostUSD: 1.60}, pv)
	if err != nil {
		t.Fatalf("outcome for a session that answered = %v; want the answer", err)
	}
	if out.Text != "{}" || out.Turns != 7 {
		t.Errorf("output = %+v; want the session's own text and turns", out)
	}
}

// TestEstimatedCostReachesPassTelemetry is the end-to-end half of the same
// argument: the ceiling's unit has to survive the whole fold from a session's
// stream to the rendered log field, or it is still unrecoverable from the log.
//
// Two things are pinned. First, a pass that answered reports BOTH figures and
// they disagree — that gap is the structural one docs/assay-turn-budget.md
// measures, and a line carrying only the billed number sizes the ceiling that
// much too permissively. Second, the
// pass the ceiling actually stopped reports its estimate at all: that session
// emits no result event, so it is absent by construction from any reproduction
// built off preserved session logs, which is the exact gap this closes.
func TestEstimatedCostReachesPassTelemetry(t *testing.T) {
	script := map[string][]stubResp{
		passTriage.Name: {{text: triageJSON(t, nil, ""), turns: 3, cost: 0.42, estCost: 0.26}},
	}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: findingsJSON(t, nil), turns: 6, cost: 0.80, estCost: 0.49}}
	}
	stopped := maxCostErr("security", 13, 1.57)
	script["security"] = []stubResp{{err: stopped}}

	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(newScriptRunner(script).run))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}

	triage := passReport(res, passTriage.Name)
	if triage == nil {
		t.Fatal("no triage pass report")
	}
	if math.Abs(triage.EstCostUSD-0.26) > 1e-9 {
		t.Errorf("triage EstCostUSD = %v; want the tracker's 0.26", triage.EstCostUSD)
	}
	if triage.EstCostUSD == triage.CostUSD {
		t.Error("triage reports one figure under both names; the billed total and the ceiling's unit are different quantities")
	}

	sec := passReport(res, "security")
	if sec == nil {
		t.Fatal("no security pass report")
	}
	if math.Abs(sec.EstCostUSD-stopped.EstCostUSD) > 1e-9 {
		t.Errorf("stopped pass EstCostUSD = %v; want the tracker snapshot %v — a killed session has no other dollar figure",
			sec.EstCostUSD, stopped.EstCostUSD)
	}

	line := res.PassTelemetryText()
	for _, want := range []string{
		"cost_usd=0.4200 cost_est=0.2600",
		fmt.Sprintf("term=%s cost_usd=%.4f cost_est=%.4f", ReasonMaxCost, stopped.CostUSD, stopped.EstCostUSD),
	} {
		if !strings.Contains(line, want) {
			t.Errorf("PassTelemetryText = %q; want it to carry %q", line, want)
		}
	}
}

// TestEstimatedCostSumsOverAPassesSessions holds the estimate to CostUSD's fold
// rather than Turns' final-session one. The two dollar fields on a telemetry
// segment are only comparable if they count the same sessions, and a pass that
// took a strict-JSON re-prompt made two — both of which the provider billed and
// both of which the tracker watched.
func TestEstimatedCostSumsOverAPassesSessions(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {{text: triageJSON(t, nil, "")}}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: findingsJSON(t, nil)}}
	}
	// First session answers unparseably, so the pass re-prompts: two sessions,
	// one pass.
	script["logic"] = []stubResp{
		{text: "not json", cost: 0.30, estCost: 0.18},
		{text: findingsJSON(t, nil), cost: 0.20, estCost: 0.11},
	}

	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(newScriptRunner(script).run))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	logic := passReport(res, "logic")
	if logic == nil {
		t.Fatal("no logic pass report")
	}
	if math.Abs(logic.EstCostUSD-0.29) > 1e-9 {
		t.Errorf("EstCostUSD = %v; want 0.29 — both sessions the pass made", logic.EstCostUSD)
	}
	if math.Abs(logic.CostUSD-0.50) > 1e-9 {
		t.Fatalf("CostUSD = %v; want 0.50 — the fold the estimate has to match", logic.CostUSD)
	}
}

// TestSessionOutcomeCarriesTheTrackersEstimate pins the one seam at which the
// ceiling's unit enters the telemetry at all: PassOutput.EstCostUSD and
// PassError.EstCostUSD are sourced from costTracker and from nothing else, on
// every branch sessionOutcome can take.
//
// It has to be a direct test, because the end-to-end ones structurally cannot
// reach this line: they drive Review through a scripted runner that sets
// EstCostUSD itself and constructs no tracker, so they pin the fold and the
// rendering ABOVE this assignment and say nothing about it. Replacing both
// tracker.TotalUSD() reads with a literal 0 left the whole package green —
// while every real pass would render no cost_est= at all, since the field is
// omitted at zero, leaving assay.max_cost_per_pass_usd unsizeable from the
// daemon log. That is the regression the field exists to prevent.
func TestSessionOutcomeCarriesTheTrackersEstimate(t *testing.T) {
	pv := provider.Provider{Kind: provider.Claude}
	// Well under the $1.50 ceiling, so no branch below is a stop: the estimate
	// has to arrive on the ordinary paths' own merits rather than as
	// costStopError's snapshot, which is the one path already covered.
	tracked := func() *costTracker {
		tr := newCostTracker(1.50)
		tr.AddTurnCost("msg_1", turnOf(0.25, 900, 41500))
		tr.AddTurnCost("msg_2", turnOf(0.15, 0, 0))
		return tr
	}
	const wantEst = 0.40

	t.Run("a session that answered", func(t *testing.T) {
		tr := tracked()
		out, err := sessionOutcome("logic", tr, nil, &smith.Result{
			ResultSubtype: "success", FullOutput: "{}", NumTurns: 7, CostUSD: 0.65,
		}, pv)
		if err != nil {
			t.Fatalf("sessionOutcome: %v", err)
		}
		if math.Abs(out.EstCostUSD-tr.TotalUSD()) > 1e-9 || math.Abs(out.EstCostUSD-wantEst) > 1e-9 {
			t.Errorf("EstCostUSD = %v; want the tracker's %v", out.EstCostUSD, wantEst)
		}
		// The whole point of the second field: it is a different quantity from
		// the provider's billed total, not a second copy of it.
		if math.Abs(out.EstCostUSD-out.CostUSD) < 1e-9 {
			t.Errorf("EstCostUSD == CostUSD (%v); the estimate came from the result event rather than the tracker, and only the tracker's figure is the ceiling's unit", out.CostUSD)
		}
	})

	// The failure branches matter more, not less: a session that died is one an
	// operator most wants to read against the ceiling, and a failure is not a
	// refund for what the tracker already watched the provider bill.
	for _, tc := range []struct {
		name   string
		res    *smith.Result
		reason string
	}{
		{"rate limited", &smith.Result{ExitCode: 1, RateLimited: true, NumTurns: 4, CostUSD: 0.12}, ReasonRateLimited},
		{"provider failure", &smith.Result{ExitCode: 1, IsError: true, ResultSubtype: ReasonMaxTurns, NumTurns: 30, CostUSD: 0.90}, ReasonMaxTurns},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := tracked()
			_, err := sessionOutcome("logic", tr, nil, tc.res, pv)
			var perr *PassError
			if !errors.As(err, &perr) || perr.Reason != tc.reason {
				t.Fatalf("outcome = %v; want a %s PassError", err, tc.reason)
			}
			if math.Abs(perr.EstCostUSD-tr.TotalUSD()) > 1e-9 || math.Abs(perr.EstCostUSD-wantEst) > 1e-9 {
				t.Errorf("EstCostUSD = %v; want the tracker's %v", perr.EstCostUSD, wantEst)
			}
			if math.Abs(perr.EstCostUSD-perr.CostUSD) < 1e-9 {
				t.Errorf("EstCostUSD == CostUSD (%v); the failed session's billed figure is not the tracker's estimate", perr.CostUSD)
			}
		})
	}
}

// TestEstimatedCostSumsAcrossAMaxTurnsRetry is the sum-over-sessions fold at
// the site that spends the most: the turn-budget retry, which runs a whole
// extra session rather than a re-prompt. A fold that degrades to assignment
// here under-reports the ceiling's own unit on exactly the passes that cost
// twice — and the two dollar fields on one telemetry segment are comparable
// only while both count the same sessions, so CostUSD's fold is asserted
// beside it.
func TestEstimatedCostSumsAcrossAMaxTurnsRetry(t *testing.T) {
	script := map[string][]stubResp{passTriage.Name: {{text: triageJSON(t, nil, "")}}}
	for _, p := range deepPasses {
		script[p.Name] = []stubResp{{text: findingsJSON(t, nil)}}
	}
	// Session one burns the budget without answering — the failure that earns a
	// fresh session — and the retry answers. Both were billed, both watched.
	burned := maxTurnsErr("logic", assayMaxTurns, 0.90)
	burned.EstCostUSD = 0.55
	script["logic"] = []stubResp{
		{err: burned},
		{text: findingsJSON(t, nil), cost: 0.20, estCost: 0.11},
	}

	res, err := Review(context.Background(), testRequest(), openTestDB(t), DefaultConfig().WithRunner(newScriptRunner(script).run))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	logic := passReport(res, "logic")
	if logic == nil {
		t.Fatal("no logic pass report")
	}
	if !logic.Retried || logic.Attempts != 2 {
		t.Fatalf("telemetry = {Retried:%v Attempts:%d}; want the retry to have run", logic.Retried, logic.Attempts)
	}
	if math.Abs(logic.EstCostUSD-0.66) > 1e-9 {
		t.Errorf("EstCostUSD = %v; want 0.66 — both sessions the retry loop made", logic.EstCostUSD)
	}
	if math.Abs(logic.CostUSD-1.10) > 1e-9 {
		t.Errorf("CostUSD = %v; want 1.10 — the fold the estimate has to match", logic.CostUSD)
	}
}

// TestTriageEstimatedCostSumsOverItsReprompt covers the remaining two write
// sites of the same fold, which are triage's re-prompt branches. They are
// tested against runTriage directly rather than through Review because the
// error branch is unobservable from there: a triage failure aborts the run, and
// RunError carries the token usage but no estimate, so the only place the
// second session's estimate can be seen is the triageRun itself.
func TestTriageEstimatedCostSumsOverItsReprompt(t *testing.T) {
	req := testRequest()

	t.Run("the re-prompt answered", func(t *testing.T) {
		r := newScriptRunner(map[string][]stubResp{passTriage.Name: {
			{text: "not json", cost: 0.30, estCost: 0.18},
			{text: triageJSON(t, nil, ""), cost: 0.20, estCost: 0.11},
		}})
		run, err := runTriage(context.Background(), r.run, DefaultConfig(), req, req.Diff)
		if err != nil {
			t.Fatalf("runTriage: %v", err)
		}
		if math.Abs(run.estCostUSD-0.29) > 1e-9 {
			t.Errorf("estCostUSD = %v; want 0.29 — both sessions triage made", run.estCostUSD)
		}
		if math.Abs(run.usage.EstimatedCostUSD-0.50) > 1e-9 {
			t.Errorf("usage cost = %v; want 0.50 — the fold the estimate has to match", run.usage.EstimatedCostUSD)
		}
	})

	t.Run("the re-prompt failed", func(t *testing.T) {
		// The first session answered unparseably and was billed for it; the
		// re-prompt then died. Reporting only the second would drop spend the
		// provider made either way — a failure is not a refund.
		died := maxTurnsErr(passTriage.Name, assayMaxTurns, 0.20)
		died.EstCostUSD = 0.11
		r := newScriptRunner(map[string][]stubResp{passTriage.Name: {
			{text: "not json", cost: 0.30, estCost: 0.18},
			{err: died},
		}})
		run, err := runTriage(context.Background(), r.run, DefaultConfig(), req, req.Diff)
		if err == nil {
			t.Fatal("runTriage returned no error; want the re-prompt's failure")
		}
		if math.Abs(run.estCostUSD-0.29) > 1e-9 {
			t.Errorf("estCostUSD = %v; want 0.29 — both sessions triage made", run.estCostUSD)
		}
		if math.Abs(run.usage.EstimatedCostUSD-0.50) > 1e-9 {
			t.Errorf("usage cost = %v; want 0.50 — the fold the estimate has to match", run.usage.EstimatedCostUSD)
		}
	})
}
