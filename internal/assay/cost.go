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
	// limitUSD is the ceiling. Zero (or anything costCeilingUSD normalises to
	// zero) disables the tracker entirely.
	limitUSD float64

	mu            sync.Mutex
	totalUSD      float64
	turns         int
	cacheCreation int
	cacheRead     int
}

// newCostTracker returns a tracker enforcing limitUSD. A non-positive or
// non-finite limit yields a disabled tracker, which accumulates nothing and
// never reports Exceeded.
func newCostTracker(limitUSD float64) *costTracker {
	if limitUSD <= 0 || math.IsNaN(limitUSD) || math.IsInf(limitUSD, 0) {
		limitUSD = 0
	}
	return &costTracker{limitUSD: limitUSD}
}

// enabled reports whether a ceiling is in force. A nil tracker is disabled, so
// callers never have to nil-check before asking.
func (t *costTracker) enabled() bool { return t != nil && t.limitUSD > 0 }

// AddTurnCost records one completed turn: its estimated cost in USD and the
// prompt-cache tokens it wrote and read. It reports whether the session has now
// reached the ceiling.
//
// A cost that is negative, NaN or infinite is recorded as zero rather than
// rejecting the turn: a garbled usage payload must not be able to move the
// running total in either direction — inflating it stops a healthy pass, and
// letting it go negative buys a runaway unlimited budget. The turn itself is
// still counted, since it happened.
func (t *costTracker) AddTurnCost(usd float64, cacheCreation, cacheRead int) bool {
	if !t.enabled() {
		return false
	}
	if usd < 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		usd = 0
	}
	t.mu.Lock()
	t.totalUSD += usd
	t.turns++
	t.cacheCreation += max(cacheCreation, 0)
	t.cacheRead += max(cacheRead, 0)
	exceeded := t.totalUSD >= t.limitUSD
	t.mu.Unlock()
	return exceeded
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
// turns it saw, and the prompt-cache write/read totals. It is what a stopped
// session reports instead of the result event it never got to emit.
func (t *costTracker) Snapshot() (usd float64, turns, cacheCreation, cacheRead int) {
	if t == nil {
		return 0, 0, 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.totalUSD, t.turns, t.cacheCreation, t.cacheRead
}

// turnEnvelope is the per-turn accounting an assistant stream event carries on
// its message envelope. Claude reports a `usage` block per assistant message —
// the tokens that turn's request billed, cache lines included — and names the
// model that served it. It is decoded here rather than in smith.StreamEvent
// because this is the only consumer: the aggregate Result already gets its
// totals from the result event.
type turnEnvelope struct {
	Model string `json:"model"`
	Usage *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// turnUsage extracts one turn's billable usage from a stream event, returning
// ok=false for every event that is not a turn the provider has just billed.
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
func turnUsage(ev smith.StreamEvent) (u cost.Usage, model string, ok bool) {
	if ev.Type != "assistant" || len(ev.Message) == 0 {
		return cost.Usage{}, "", false
	}
	var env turnEnvelope
	if err := json.Unmarshal(ev.Message, &env); err != nil || env.Usage == nil {
		return cost.Usage{}, "", false
	}
	return cost.Usage{
		InputTokens:      env.Usage.InputTokens,
		OutputTokens:     env.Usage.OutputTokens,
		CacheReadTokens:  env.Usage.CacheReadInputTokens,
		CacheWriteTokens: env.Usage.CacheCreationInputTokens,
	}, env.Model, true
}

// turnCostUSD estimates what one turn of a session cost, from the usage the
// provider reported for it and the pricing table for the model that served it.
//
// The estimate is unavoidable: the provider prices a session only in its final
// result event, so a ceiling that must act mid-session has nothing but tokens
// and the configured pricing to work from. The model named on the turn itself
// wins over the configured hint — the hint is often empty ("let the provider
// pick"), and the turn says what it actually picked.
func turnCostUSD(ev smith.StreamEvent, pv provider.Provider) (usd float64, cacheCreation, cacheRead int, ok bool) {
	u, model, ok := turnUsage(ev)
	if !ok {
		return 0, 0, 0, false
	}
	if model == "" {
		model = pv.Model
	}
	u.Calculate(cost.EstimatePricing(pv.Kind, model))
	return u.EstimatedCostUSD, u.CacheWriteTokens, u.CacheReadTokens, true
}

// sessionAnswered reports whether a session reached a genuine terminal success
// — the provider's own "the model finished and this is its answer". It is the
// same test smith applies when it decides a session succeeded despite a
// non-zero exit code, kept here as one predicate so the cost-stop branch and
// that definition cannot drift apart.
//
// A nil result (no result event at all) is not an answer.
func sessionAnswered(res *smith.Result) bool {
	return res != nil && res.ResultSubtype == "success" && !res.IsError
}

// costStopError returns the failure for a session Assay stopped at its spend
// ceiling, or nil when nothing was stopped.
//
// The one case where a crossed ceiling is not a stop is a session that
// answered anyway: the ceiling can only be crossed by a turn the model was
// already mid-answer on, and a completed answer means nothing was cut short.
// Reporting that as a stop would throw away a finished review AND claim an
// intervention that never happened — so it falls through to the ordinary
// success path, where the provider's own final cost is what gets recorded.
//
// The error carries the tracker's accounting rather than the result's, because
// a stopped session never emitted a result event: what the tracker summed on
// the way is the only record of what the provider already billed. A stop is
// not a refund.
func costStopError(pass string, tracker *costTracker, res *smith.Result) *PassError {
	if !tracker.Exceeded() || sessionAnswered(res) {
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
