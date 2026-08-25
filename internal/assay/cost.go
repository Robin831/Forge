package assay

import (
	"encoding/json"
	"fmt"
	"math"
	"sync"

	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/textfmt"
)

// costTracker accumulates one pass session's spend as its turns complete and
// reports when that spend has reached the configured ceiling.
//
// It exists because a session's real cost is known only at the end: the
// provider reports total_cost_usd on its final result event, which is exactly
// the moment the money is already spent. A ceiling that has to stop a runaway
// therefore cannot read that number — it has to add up the per-turn usage the
// provider streams along the way, which is what this does.
//
// The accounting it keeps is not only the ceiling's input. A session stopped
// mid-flight never emits a result event, so smith.Result reports zero cost,
// zero turns and no cache accounting for it — while the provider bills every
// turn it did serve. What this tracker summed is the only accounting an
// aborted pass has, and it is what the PassError carries out (see
// newSmithRunner): a stop is not a refund.
//
// It is written from the provider's stdout reader goroutine and read from the
// pass goroutine, so every field is guarded.
type costTracker struct {
	// limitUSD is the ceiling. Zero — which is what newCostTracker normalises
	// every unusable limit to — disables the tracker entirely.
	limitUSD float64

	mu            sync.Mutex
	totalUSD      float64
	turns         int
	cacheCreation int
	cacheRead     int
	// seen holds the message ids already billed. Claude emits one assistant
	// stream event per content block of a single API message — thinking, text
	// and tool_use arrive as three events sharing one message id and one
	// identical usage block — so billing per event charges a tool-using turn
	// two or three times over. See AddTurnCost.
	seen map[string]struct{}
}

// newCostTracker returns a tracker enforcing limitUSD. This is the one place
// an unusable ceiling — unset, negative, NaN, Inf — is normalised to zero,
// which yields a disabled tracker that accumulates nothing and never reports
// Exceeded. One normalisation means no call site, and no comparison inside the
// tracker, has to decide what a NaN limit means.
func newCostTracker(limitUSD float64) *costTracker {
	if limitUSD <= 0 || math.IsNaN(limitUSD) || math.IsInf(limitUSD, 0) {
		limitUSD = 0
	}
	return &costTracker{limitUSD: limitUSD}
}

// enabled reports whether a ceiling is in force. A nil tracker is disabled, so
// callers never have to nil-check before asking.
func (t *costTracker) enabled() bool { return t != nil && t.limitUSD > 0 }

// AddTurnCost records one API message: its estimated cost in USD and the
// prompt-cache tokens it wrote and read. It reports whether the session has now
// reached the ceiling.
//
// id is the provider's message id, and a message is billed once however many
// stream events carry it. Claude splits one API turn across an assistant event
// per content block — a thinking + text + tool_use turn is three events, each
// repeating the same message id and the same usage block, cache write included
// — so counting per event bills the turn three times, reaches the ceiling at a
// third of the configured spend, and kills a healthy pass with an inflated
// figure in the stop message. Repeats are therefore ignored rather than added:
// they are the same message, not new spend. The tally still reports whether the
// ceiling is reached, since a duplicate arriving after the crossing does not
// un-cross it.
//
// An empty id is billed every time, because there is nothing to deduplicate on
// — a backend that streams usage without message ids gets the historical
// per-event accounting.
//
// A cost that is negative, NaN or infinite is recorded as zero rather than
// rejecting the turn: a garbled usage payload must not be able to move the
// running total in either direction — inflating it stops a healthy pass, and
// letting it go negative buys a runaway unlimited budget. The turn itself is
// still counted, since it happened.
func (t *costTracker) AddTurnCost(id string, usd float64, cacheCreation, cacheRead int) bool {
	if !t.enabled() {
		return false
	}
	if usd < 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		usd = 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if id != "" {
		if _, dup := t.seen[id]; dup {
			return t.totalUSD >= t.limitUSD
		}
		if t.seen == nil {
			t.seen = make(map[string]struct{})
		}
		t.seen[id] = struct{}{}
	}
	t.totalUSD += usd
	t.turns++
	t.cacheCreation += max(cacheCreation, 0)
	t.cacheRead += max(cacheRead, 0)
	return t.totalUSD >= t.limitUSD
}

// Exceeded reports whether the accumulated spend has reached the ceiling. The
// comparison is >=: a session that has spent exactly its budget has no budget
// left, and a ceiling that only fires strictly past its value is off by one
// turn at precisely the point it matters.
func (t *costTracker) Exceeded() bool {
	if !t.enabled() {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.totalUSD >= t.limitUSD
}

// TotalUSD returns the spend accumulated so far.
func (t *costTracker) TotalUSD() float64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.totalUSD
}

// LimitUSD returns the ceiling in force, or 0 when there is none.
func (t *costTracker) LimitUSD() float64 {
	if t == nil {
		return 0
	}
	return t.limitUSD
}

// Snapshot returns everything the tracker counted: the spend, the number of
// distinct messages it billed, and the prompt-cache write/read totals. It is
// what a stopped session reports instead of the result event it never got to
// emit.
func (t *costTracker) Snapshot() (usd float64, turns, cacheCreation, cacheRead int) {
	if t == nil {
		return 0, 0, 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.totalUSD, t.turns, t.cacheCreation, t.cacheRead
}

// turnEnvelope is the per-turn accounting an assistant stream event carries on
// its message envelope: the message's own id, the model that served it, and a
// `usage` block covering the tokens that message's request billed on the input
// side — prompt, cache write and cache read.
//
// The output side is effectively invisible here. Claude stamps the usage block
// when the message starts, so `output_tokens` reads as a handful of tokens
// (2 or 3 in real transcripts) no matter how much the model then went on to
// write. The estimate this feeds is therefore an input/cache estimate, which is
// the right shape for the thing it guards — a runaway pass spends by re-reading
// a large prompt every turn, not by writing prose — but it does mean the
// ceiling reads slightly low, never high.
//
// The id is what makes a message billable once: see costTracker.AddTurnCost.
//
// It is decoded here rather than in smith.StreamEvent because this is the only
// consumer: the aggregate Result already gets its totals from the result event.
type turnEnvelope struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	Usage *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// turnUsage extracts one message's billable usage from a stream event,
// returning ok=false for every event that is not a message the provider has
// just billed. The message id comes back with it so the same message arriving
// as several content-block events is billed once (costTracker.AddTurnCost).
//
// Only assistant events qualify. The result event carries the whole session's
// total_cost_usd, which is the sum of the turns already counted here — adding
// it would double every session — and it arrives when the session is over
// anyway, which is too late for a ceiling to act on.
//
// A backend that streams no per-turn usage (Gemini's deltas, a plain-text
// provider) yields nothing here and its sessions are never stopped on cost.
// That is the intended failure direction: the ceiling is a brake on a runaway
// and must not become a source of pass failures on backends whose spend it
// cannot see.
func turnUsage(ev smith.StreamEvent) (u cost.Usage, id, model string, ok bool) {
	if ev.Type != "assistant" || len(ev.Message) == 0 {
		return cost.Usage{}, "", "", false
	}
	var env turnEnvelope
	if err := json.Unmarshal(ev.Message, &env); err != nil || env.Usage == nil {
		return cost.Usage{}, "", "", false
	}
	return cost.Usage{
		InputTokens:      env.Usage.InputTokens,
		OutputTokens:     env.Usage.OutputTokens,
		CacheReadTokens:  env.Usage.CacheReadInputTokens,
		CacheWriteTokens: env.Usage.CacheCreationInputTokens,
	}, env.ID, env.Model, true
}

// turnCostUSD estimates what one message of a session cost, from the usage the
// provider reported for it and the pricing table for the model that served it.
// It returns the message id alongside, which is what the tracker bills against.
//
// The estimate is unavoidable: the provider prices a session only in its final
// result event, so a ceiling that must act mid-session has nothing but tokens
// and the configured pricing to work from. The model named on the turn itself
// wins over the configured hint — the hint is often empty ("let the provider
// pick"), and the turn says what it actually picked.
func turnCostUSD(ev smith.StreamEvent, pv provider.Provider) (id string, usd float64, cacheCreation, cacheRead int, ok bool) {
	u, id, model, ok := turnUsage(ev)
	if !ok {
		return "", 0, 0, 0, false
	}
	if model == "" {
		model = pv.Model
	}
	u.Calculate(cost.EstimatePricing(pv.Kind, model))
	return id, u.EstimatedCostUSD, u.CacheWriteTokens, u.CacheReadTokens, true
}

// costStopCallback builds the stream-event callback a pass session runs on the
// provider's reader goroutine. It does the two independent jobs that share that
// one hook:
//
//   - releasing the staggered fan-out, when signal is non-nil, at the primer's
//     first model-output event (never at Claude's opening init event, which is
//     emitted before the model request is even made); and
//   - billing each message against the tracker and calling stop the moment the
//     session has spent its budget.
//
// The two are deliberately in one function rather than composed at the call
// site: they are the sole readers of a single OnStreamEvent hook, and the guard
// that decides whether to install it at all has to be the disjunction of both.
//
// Both callbacks fire at most once: signal at the first model output, stop at
// the crossing (every event after it reports the ceiling as still reached, and
// stopping a session twice says nothing the first stop did not). A nil stop —
// a session with no ceiling in force, installed only for the stagger — and a
// disabled tracker are both simply never billed, which is what lets an
// unconfigured anvil run exactly the session it always did.
//
// The callback does no waiting: cancelling is what ends the process, and the
// pass goroutine's Wait() is what reaps it.
func costStopCallback(tracker *costTracker, pv provider.Provider, signal func(), stop func()) func(smith.StreamEvent) {
	var signalOnce, stopOnce sync.Once
	return func(ev smith.StreamEvent) {
		if signal != nil && isModelOutput(ev) {
			signalOnce.Do(signal)
		}
		if !tracker.enabled() || stop == nil {
			return
		}
		if id, usd, cc, cr, ok := turnCostUSD(ev, pv); ok && tracker.AddTurnCost(id, usd, cc, cr) {
			stopOnce.Do(stop)
		}
	}
}

// costStopError returns the failure for a session Assay stopped at its spend
// ceiling, or nil when nothing was stopped.
//
// The one case where a crossed ceiling is not a stop is a session that
// answered anyway (smith.Result.Answered, the same predicate smith itself uses
// to recognise a genuine terminal success, so the exception here and that
// definition cannot drift apart): the ceiling can only be crossed by a turn the
// model was already mid-answer on, and a completed answer means nothing was cut
// short. Reporting that as a stop would throw away a finished review AND claim
// an intervention that never happened — so it falls through to the ordinary
// success path, where the provider's own final cost is what gets recorded.
//
// The error carries the tracker's accounting rather than the result's, because
// a stopped session never emitted a result event: what the tracker summed on
// the way is the only record of what the provider already billed. A stop is
// not a refund.
func costStopError(pass string, tracker *costTracker, res *smith.Result) *PassError {
	if !tracker.Exceeded() || res.Answered() {
		return nil
	}
	usd, turns, cacheCreation, cacheRead := tracker.Snapshot()
	perr := newPassError(pass, ReasonMaxCost,
		fmt.Sprintf("stopped after %s: estimated session cost $%.2f reached the $%.2f per-pass ceiling (assay.max_cost_per_pass_usd)",
			textfmt.Count(turns, "turn"), usd, tracker.LimitUSD()), nil)
	perr.Turns = turns
	perr.CostUSD = usd
	perr.CacheCreationTokens = cacheCreation
	perr.CacheReadTokens = cacheRead
	return perr
}
