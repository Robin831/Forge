package cost

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// Repeat-review cost attribution.
//
// This file answers one question repeatably: of what Assay spent over a window,
// how much went on reviewing a PR for the FIRST time and how much on reviewing
// it again — and how much of that spend is prompt-cache traffic, split into the
// two classes that are priced differently (cache writes and cache reads).
//
// It sits beside the pricing table it prices with rather than in a package of
// its own: the rates a report quotes and the rates the daemon estimates a
// running session against must be the same numbers, and two copies of a pricing
// table drift the moment one of them is edited.
//
// # Two cost axes, deliberately not summed together
//
// A report carries two different kinds of dollar figure and never adds them:
//
//   - RECORDED spend (CostUSD everywhere) is assay_runs.cost_usd — what the
//     provider reported for the run. It is the authoritative total, and it is
//     the figure the first/repeat split is computed from, because that is the
//     methodology the original baseline used. Nothing here re-derives it.
//
//   - PRICED attribution (the cache_creation / cache_read token classes) is
//     tokens x that class's own rate. It explains the cache COMPONENT of the
//     recorded spend and is a strict subset of it: assay_runs persists cache
//     tokens but not plain input/output tokens, so the two priced classes can
//     never add up to the recorded total. Reading AttributedCostUSD as "total
//     spend" would silently under-report every run.
//
// # Ordinals are derived over a PR's whole history
//
// A run's ordinal is its position among that PR's reviews (1 = first, n>1 =
// repeat), and it is derived over every run the PR has ever had — not over the
// window being reported. A window-local ordinal is wrong in the one direction
// that flatters the answer: a PR first reviewed the day before the window opens
// would have its second review counted as a first, moving repeat spend into the
// first-run column. Feed BuildReport the full history (state's
// AssayRunHistoryForWindow returns exactly that) and it filters to the window
// only after the ordinals are assigned.
//
// # Cache accounting can be absent, and absent is not zero
//
// assay_runs.cache_creation_tokens / cache_read_tokens are zero on three
// different kinds of row that cannot be told apart: one written before the
// columns existed, one behind a backend that reports no cache accounting, and a
// run that genuinely shared nothing. A row with both at zero is therefore
// classified TokenClassUnknown rather than as a run with no cache traffic — its
// recorded cost is still counted in every total, so the money is never lost,
// but it is reported as unattributable instead of as a confident zero. That is
// the graceful degradation older rows need: the report never fails on them, it
// says how much of the window it could not attribute.
//
// # Every pass is priced at the model that ran it
//
// A run is not one model: each pass resolves its own provider chain, and a
// pass that was rate limited moves down it. Pricing a run's tokens at one
// model's rates therefore misprices every mixed run — an Opus 5 cache write
// costs two and a half times a Sonnet 5 one. So the priced figures here are
// built pass by pass: each pass's tokens at the rates of the model recorded on
// its own row (RatesForModel, the resolver the in-flight cost ceiling uses
// too). Where a row cannot say, the resolution falls back one level at a time
// and says which level answered (ModelSource*): the pass's own model, then the
// run-level model, then the report's fallback model. A row written before
// per-pass tokens were recorded carries no pass tokens at all, and its run
// totals are priced as one unit at the run-level model on the same terms.

// Token class names. A run's cache accounting is either present, in which case
// its tokens land in one or both of the two priced classes, or absent, in which
// case its recorded cost lands in the unknown class.
const (
	TokenClassCacheCreation = "cache_creation"
	TokenClassCacheRead     = "cache_read"
	TokenClassUnknown       = "unknown"
)

// Cost bases. A breakdown says which of the two axes its dollar figure came
// from, so a consumer can never add a priced estimate to a recorded total by
// accident.
const (
	// BasisPriced: cost = tokens x this class's rate from the pricing table.
	BasisPriced = "priced"
	// BasisRecorded: cost = the provider-reported cost_usd of the rows
	// involved. Used for the unknown class, whose tokens are not knowable.
	BasisRecorded = "recorded"
)

// Model sources: which level of a row named the model a unit was priced at.
const (
	// ModelSourcePass: the pass row recorded the model it ran on.
	ModelSourcePass = "pass"
	// ModelSourceRun: the pass named none (or the row carries no pass
	// tokens), and the run-level model answered.
	ModelSourceRun = "run"
	// ModelSourceFallback: neither the pass nor the run named a model this
	// package can price, so the report's fallback model was used.
	ModelSourceFallback = "fallback"
)

// DefaultFallbackModel is the model a unit is priced at when neither its pass
// row nor its run names one that resolves: the current Sonnet, which is what
// Assay runs on when nothing is configured.
const DefaultFallbackModel = ModelClaudeSonnet5

// GroupFirstRun / GroupRepeatRun are the two halves of the split this report
// exists for.
const (
	GroupFirstRun  = "first_run"
	GroupRepeatRun = "repeat_run"
)

// RunRecord is one Assay review run as the attribution report reads it: a
// projection of an assay_runs row carrying only the columns the report needs.
//
// It is defined here rather than taken as state.AssayRun so this package stays
// free of a state import — cost is deliberately a leaf that the persistence
// layer's own importers can depend on. Callers project their rows into it (see
// the adapter in cmd/forge).
type RunRecord struct {
	RunID         int       `json:"run_id"`
	Anvil         string    `json:"anvil"`
	PRNumber      int       `json:"pr_number"`
	HeadSHA       string    `json:"head_sha,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	CostUSD       float64   `json:"cost_usd"`
	FindingsCount int       `json:"findings_count"`
	// SkippedReason is non-empty on a run that dispatched no passes (nothing
	// reviewable in the diff). Such a run reviewed no code and cost nothing,
	// so by default it is excluded before ordinals are assigned — see
	// Options.IncludeSkipped.
	SkippedReason string `json:"skipped_reason,omitempty"`
	ShadowMode    bool   `json:"shadow_mode,omitempty"`
	// Status is the run's coverage outcome as persisted ("complete",
	// "partial", "failed", or empty on rows written before the column
	// existed), and Error is the cause a run that died carries. Neither
	// affects the first-vs-repeat split — a failure is not a refund, so its
	// spend counts exactly like any other run's — but together they are the
	// only way to tell a run that read the diff and found nothing from one
	// that reported zero findings because no pass ever ran. See NoCoverage.
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
	// CacheCreationTokens / CacheReadTokens are the run's prompt-cache
	// accounting summed over its sessions. Both zero means "not knowable",
	// not "no cache traffic" — see the package note above.
	CacheCreationTokens int `json:"cache_creation_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens"`
	// Model is the run-level model: the one model the run's passes ran on,
	// where a source can establish it. It is what a pass that names no model
	// of its own — and a row with no per-pass tokens — is priced at. Empty
	// when the source cannot say (pre-attribution rows, or a run whose passes
	// ran on different models).
	Model string `json:"model,omitempty"`
	// Passes is the run's per-pass attribution: which model each pass ran on
	// and what it was billed for. Nil on rows written before it was recorded.
	Passes []PassRecord `json:"passes,omitempty"`
}

// PassRecord is one pass of a run as the per-model pricing reads it.
type PassRecord struct {
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"`
	// Model is the model the pass ran on; empty when the row does not say.
	Model               string  `json:"model,omitempty"`
	CostUSD             float64 `json:"cost_usd"`
	InputTokens         int     `json:"input_tokens"`
	OutputTokens        int     `json:"output_tokens"`
	CacheCreationTokens int     `json:"cache_creation_tokens"`
	CacheReadTokens     int     `json:"cache_read_tokens"`
}

// HasTokens reports whether the pass row carries any token accounting. A pass
// row written before tokens were recorded carries none, and is not priced on
// its own.
func (p PassRecord) HasTokens() bool {
	return p.InputTokens > 0 || p.OutputTokens > 0 || p.CacheCreationTokens > 0 || p.CacheReadTokens > 0
}

// PRKey identifies the PR a run belongs to. PR numbers are per-repository, so
// the anvil is part of the key: without it, PR #12 in two anvils would be one
// PR with twice the reviews and every ordinal after the first would be wrong.
func (r RunRecord) PRKey() string {
	return r.Anvil + "#" + strconv.Itoa(r.PRNumber)
}

// Skipped reports whether the run dispatched no passes.
func (r RunRecord) Skipped() bool { return strings.TrimSpace(r.SkippedReason) != "" }

// runStatusFailed is the persisted coverage outcome for a run no deep pass
// reviewed the head on. It is spelled out here rather than imported from
// internal/assay because cost is deliberately a leaf package; the value is
// pinned by TestNoCoverageMatchesPersistedFailedStatus.
const runStatusFailed = "failed"

// NoCoverage reports whether this run's zero findings mean "no pass ever ran"
// rather than "a review read the diff and found nothing".
//
// The distinction only matters to an analysis of the zero-finding tail, and
// there it matters completely: a rate-limited run that died at triage reports
// findings_count = 0 exactly as a clean review does, so counting it as a clean
// review inflates the very cell a skip heuristic would claim to recover. Its
// spend is still counted everywhere — a failure is not a refund — it is only
// annotated so a reader is not told a review happened that did not.
//
// Two shapes qualify. A persisted "failed" status is the direct signal. A row
// written before that column existed carries no status at all, so the fallback
// is an error with no recorded spend: a run that was billed for something did
// reach the model, whatever it then failed at, while one that was billed for
// nothing and carries an error never got that far. The fallback is deliberately
// narrow — a partly-successful older run reporting an error and real spend is
// NOT claimed as no-coverage, because over-claiming here would shrink the
// zero-finding population on a guess.
func (r RunRecord) NoCoverage() bool {
	status := strings.ToLower(strings.TrimSpace(r.Status))
	if status == runStatusFailed {
		return true
	}
	return status == "" && strings.TrimSpace(r.Error) != "" && r.CostUSD == 0
}

// HasCacheAccounting reports whether this row carries usable prompt-cache
// token counts. Both counters at zero is the ambiguous case and counts as no
// accounting.
func (r RunRecord) HasCacheAccounting() bool {
	return r.CacheCreationTokens > 0 || r.CacheReadTokens > 0
}

// OrdinalRun pairs a run with its position in its PR's review history.
type OrdinalRun struct {
	Run RunRecord `json:"run"`
	// Ordinal is 1 for a PR's first review and n>1 for the nth. It is only
	// meaningful relative to the set DeriveRunOrdinals was given, which is
	// why callers pass a PR's whole history rather than a window of it.
	Ordinal int `json:"ordinal"`
}

// IsRepeat reports whether this run re-reviewed a PR that had been reviewed
// before.
func (o OrdinalRun) IsRepeat() bool { return o.Ordinal > 1 }

// DeriveRunOrdinals groups runs by PR, orders each PR's runs oldest first and
// assigns each an ordinal starting at 1. It is pure, does not reorder the
// caller's slice, and returns its result in the same (PR, time) order it
// derives in so a consumer can walk a PR's reviews in sequence.
//
// Ties on StartedAt break on RunID, so two runs stamped in the same
// millisecond always come back in the same order: an ordinal that depends on
// the storage layer's row order is not repeatable, and repeatability is the
// entire point of the report.
//
// Exported because it is the shared definition of "the nth review of this PR" —
// anything else asking that question (a zero-finding-tail investigation, a
// trigger gate) reads it from here rather than re-deriving a second answer.
func DeriveRunOrdinals(runs []RunRecord) []OrdinalRun {
	if len(runs) == 0 {
		return nil
	}

	sorted := make([]RunRecord, len(runs))
	copy(sorted, runs)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if ka, kb := a.PRKey(), b.PRKey(); ka != kb {
			return ka < kb
		}
		if !a.StartedAt.Equal(b.StartedAt) {
			return a.StartedAt.Before(b.StartedAt)
		}
		return a.RunID < b.RunID
	})

	out := make([]OrdinalRun, 0, len(sorted))
	var (
		currentKey string
		n          int
	)
	for i, r := range sorted {
		key := r.PRKey()
		if i == 0 || key != currentKey {
			currentKey = key
			n = 0
		}
		n++
		out = append(out, OrdinalRun{Run: r, Ordinal: n})
	}
	return out
}

// TokenClassBreakdown is one token class's contribution to a report or a group.
type TokenClassBreakdown struct {
	Class string `json:"class"`
	// Runs is how many runs contributed to this class: for a priced class,
	// the runs carrying a non-zero count of that token; for the unknown
	// class, the runs with no cache accounting at all.
	Runs   int   `json:"runs"`
	Tokens int64 `json:"tokens"`
	// CostUSD is priced from Tokens for the cache classes — each pass's
	// tokens at its own model's rate — and is the recorded provider cost for
	// the unknown class. Basis says which.
	CostUSD float64 `json:"cost_usd"`
	Basis   string  `json:"basis"`
	// RatePerM is the EFFECTIVE per-million-token rate: CostUSD over Tokens.
	// It is one model's rate only when every unit in the class ran on one
	// model; across models it is the blend. Zero for a class that was not
	// priced from tokens.
	RatePerM float64 `json:"rate_per_m,omitempty"`
}

// RunGroup is the aggregate for one half of the first-vs-repeat split.
type RunGroup struct {
	Label string `json:"label"`
	Runs  int    `json:"runs"`
	PRs   int    `json:"prs"`
	// CostUSD is recorded spend — the provider's own figure, summed. This is
	// the number the baseline methodology compares on.
	CostUSD             float64 `json:"cost_usd"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	// AttributedCostUSD is the two cache classes priced from tokens. It is a
	// SUBSET of CostUSD (assay_runs persists no plain input/output token
	// counts), never a second total.
	AttributedCostUSD float64 `json:"attributed_cost_usd"`
	// RunsWithoutCacheAccounting / CostWithoutAccountingUSD are the part of
	// this group no token class could be derived for.
	RunsWithoutCacheAccounting int     `json:"runs_without_cache_accounting"`
	CostWithoutAccountingUSD   float64 `json:"cost_without_accounting_usd"`
	// Findings and the zero-finding tail: a repeat review that reports
	// nothing is the spend most likely to have bought nothing, so the report
	// counts it rather than leaving it to a follow-up query.
	Findings           int                   `json:"findings"`
	ZeroFindingRuns    int                   `json:"zero_finding_runs"`
	ZeroFindingCostUSD float64               `json:"zero_finding_cost_usd"`
	ByTokenClass       []TokenClassBreakdown `json:"by_token_class"`
}

// OrdinalBucket is spend at one review depth: how much the nth review of a PR
// costs, which is what says whether repeat spend is a long tail or a handful of
// PRs reviewed twenty times.
type OrdinalBucket struct {
	Label string `json:"label"`
	// MinOrdinal / MaxOrdinal bound the bucket; MaxOrdinal 0 means unbounded
	// (the trailing "n+" bucket).
	MinOrdinal          int     `json:"min_ordinal"`
	MaxOrdinal          int     `json:"max_ordinal,omitempty"`
	Runs                int     `json:"runs"`
	CostUSD             float64 `json:"cost_usd"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
}

// ModelRate is one pricing row a report applied, named by its key.
type ModelRate struct {
	Model string  `json:"model"`
	Rates Pricing `json:"rates"`
}

// ModelBreakdown is the spend priced at one model's rates. Rows are keyed by
// the pricing row, so two ids billed identically (claude-opus-4-8 and
// claude-opus-5) are one row.
type ModelBreakdown struct {
	Model string  `json:"model"`
	Rates Pricing `json:"rates"`
	// Runs is how many distinct runs contributed a unit priced here; a mixed
	// run counts under every model it used.
	Runs int `json:"runs"`
	// Passes is how many pass rows were priced here, and RunLevelUnits how
	// many runs with no per-pass tokens were priced here whole.
	Passes        int `json:"passes"`
	RunLevelUnits int `json:"run_level_units"`
	// FallbackUnits is how many of those units reached this model only
	// because nothing on the row named a model that resolves.
	FallbackUnits        int     `json:"fallback_units"`
	InputTokens          int64   `json:"input_tokens"`
	OutputTokens         int64   `json:"output_tokens"`
	CacheCreationTokens  int64   `json:"cache_creation_tokens"`
	CacheReadTokens      int64   `json:"cache_read_tokens"`
	InputCostUSD         float64 `json:"input_cost_usd"`
	OutputCostUSD        float64 `json:"output_cost_usd"`
	CacheCreationCostUSD float64 `json:"cache_creation_cost_usd"`
	CacheReadCostUSD     float64 `json:"cache_read_cost_usd"`
	// PricedCostUSD is the four classes above summed. RecordedCostUSD is the
	// provider's own figure for the same units — per pass where the row
	// carries one, the run's where it does not — and, as everywhere in this
	// report, the two are never added together.
	PricedCostUSD   float64 `json:"priced_cost_usd"`
	RecordedCostUSD float64 `json:"recorded_cost_usd"`
}

// PassBreakdown is one priced unit: a pass, or a whole run whose row carries no
// per-pass tokens (Pass empty).
type PassBreakdown struct {
	RunID    int    `json:"run_id"`
	Anvil    string `json:"anvil"`
	PRNumber int    `json:"pr_number"`
	Ordinal  int    `json:"ordinal"`
	Pass     string `json:"pass,omitempty"`
	Provider string `json:"provider,omitempty"`
	// RecordedModel is the model the pass row named, verbatim; Model is the
	// pricing row the unit was priced at, and ModelSource the level that
	// supplied it.
	RecordedModel       string  `json:"recorded_model,omitempty"`
	Model               string  `json:"model"`
	ModelSource         string  `json:"model_source"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	PricedCostUSD       float64 `json:"priced_cost_usd"`
	RecordedCostUSD     float64 `json:"recorded_cost_usd"`
}

// Options tunes a report. The zero value is the baseline methodology: skipped
// runs excluded, every anvil included, each pass priced at its own model.
type Options struct {
	// IncludeSkipped keeps runs that dispatched no passes. Off by default,
	// which matches the definition of "a run" the per-PR run cap already uses
	// (state.CountAssayRuns excludes a row carrying a skipped_reason): a run
	// that reviewed nothing is not a review, and counting it would push a
	// PR's genuine second review to ordinal 3.
	IncludeSkipped bool
	// Anvil, when set, restricts the report to one anvil. The filter is
	// applied BEFORE ordinals are derived, which is safe because a PR belongs
	// to exactly one anvil, so no PR's history is ever split by it.
	Anvil string
	// FallbackModel is the model a unit is priced at when neither its pass
	// nor its run names one that resolves (a model id or alias: "opus",
	// "claude-sonnet-5", …). Empty, or a name that does not resolve, means
	// DefaultFallbackModel. It never overrides a model a row does name.
	FallbackModel string
	// ByModel / ByPass populate the report's per-model and per-pass
	// breakdowns. They are opt-in because the per-pass list grows with every
	// run in the window.
	ByModel bool
	ByPass  bool
	// Now stamps the report. Zero means time.Now(); tests set it so output
	// is byte-stable.
	Now time.Time
}

// fallbackRates resolves the fallback model to its pricing row.
func (o Options) fallbackRates() (Pricing, string) {
	if p, key, ok := RatesForModel(o.FallbackModel); ok {
		return p, key
	}
	p, key, _ := RatesForModel(DefaultFallbackModel)
	return p, key
}

// pricedUnit is one thing priced at one model's rates: a pass, or a run whose
// row carries no per-pass tokens.
type pricedUnit struct {
	pass          string
	provider      string
	recordedModel string
	key           string
	source        string
	rates         Pricing
	input         int64
	output        int64
	cacheCreation int64
	cacheRead     int64
	recorded      float64
}

func (u pricedUnit) inputCost() float64  { return float64(u.input) * u.rates.InputPerM / 1_000_000 }
func (u pricedUnit) outputCost() float64 { return float64(u.output) * u.rates.OutputPerM / 1_000_000 }
func (u pricedUnit) cacheCreationCost() float64 {
	return float64(u.cacheCreation) * u.rates.CacheWritePerM / 1_000_000
}
func (u pricedUnit) cacheReadCost() float64 {
	return float64(u.cacheRead) * u.rates.CacheReadPerM / 1_000_000
}
func (u pricedUnit) pricedCost() float64 {
	return u.inputCost() + u.outputCost() + u.cacheCreationCost() + u.cacheReadCost()
}

// resolveUnitModel walks the fallback order for one unit: the pass's own model,
// then the run-level model, then the report's fallback. A name that does not
// resolve to a pricing row is passed over rather than priced at a guess.
func resolveUnitModel(passModel, runModel string, fallback Pricing, fallbackKey string) (Pricing, string, string) {
	if p, key, ok := RatesForModel(passModel); ok {
		return p, key, ModelSourcePass
	}
	if p, key, ok := RatesForModel(runModel); ok {
		return p, key, ModelSourceRun
	}
	return fallback, fallbackKey, ModelSourceFallback
}

// priceRun splits a run into priced units. A row whose passes carry tokens is
// priced pass by pass; one whose passes carry none (or that has no pass rows)
// is priced whole, from its run-level cache tokens and at its run-level model.
func priceRun(r RunRecord, fallback Pricing, fallbackKey string) []pricedUnit {
	perPass := false
	for _, p := range r.Passes {
		if p.HasTokens() {
			perPass = true
			break
		}
	}
	if !perPass {
		rates, key, source := resolveUnitModel("", r.Model, fallback, fallbackKey)
		return []pricedUnit{{
			recordedModel: r.Model,
			key:           key,
			source:        source,
			rates:         rates,
			cacheCreation: int64(r.CacheCreationTokens),
			cacheRead:     int64(r.CacheReadTokens),
			recorded:      r.CostUSD,
		}}
	}
	units := make([]pricedUnit, 0, len(r.Passes))
	for _, p := range r.Passes {
		if !p.HasTokens() && p.CostUSD == 0 {
			continue
		}
		rates, key, source := resolveUnitModel(p.Model, r.Model, fallback, fallbackKey)
		units = append(units, pricedUnit{
			pass:          p.Name,
			provider:      p.Provider,
			recordedModel: p.Model,
			key:           key,
			source:        source,
			rates:         rates,
			input:         int64(p.InputTokens),
			output:        int64(p.OutputTokens),
			cacheCreation: int64(p.CacheCreationTokens),
			cacheRead:     int64(p.CacheReadTokens),
			recorded:      p.CostUSD,
		})
	}
	return units
}

// CostReport is the whole answer: totals, the first-vs-repeat split, the
// token-class attribution and the per-ordinal tail, over one window.
type CostReport struct {
	// Since / Until are pointers so an open bound marshals as absent rather
	// than as the zero time: a JSON report reading "until": "0001-01-01" says
	// the window ended before the data began, which is the opposite of what
	// an unbounded report means. time.Time is a struct, so `omitempty` alone
	// would never drop it.
	Since       *time.Time `json:"since,omitempty"`
	Until       *time.Time `json:"until,omitempty"`
	GeneratedAt time.Time  `json:"generated_at"`
	Anvil       string     `json:"anvil,omitempty"`
	// IncludeSkipped echoes the option, because two reports over one window
	// that disagree on it are not comparable and the difference is otherwise
	// invisible in the output.
	IncludeSkipped bool `json:"include_skipped"`
	// FallbackModel is the pricing row a unit naming no resolvable model was
	// priced at, and ModelRates every row the report applied — one per model
	// the window's passes actually ran on, not one rate for the whole report.
	FallbackModel string      `json:"fallback_model"`
	ModelRates    []ModelRate `json:"model_rates"`

	TotalRuns int `json:"total_runs"`
	TotalPRs  int `json:"total_prs"`
	// TotalCostUSD is recorded spend over the window: the sum the baseline
	// methodology reports, and always FirstRun.CostUSD + RepeatRun.CostUSD.
	TotalCostUSD float64 `json:"total_cost_usd"`
	// SkippedRunsExcluded is how many rows the skipped filter dropped, so a
	// default report says what it left out instead of quietly shrinking.
	SkippedRunsExcluded int `json:"skipped_runs_excluded"`
	// HistoryRunsOutsideWindow is how many earlier runs were read purely to
	// place the window's runs at the right ordinal. It is the evidence that
	// ordinals were derived over full history and not over the window.
	HistoryRunsOutsideWindow int `json:"history_runs_outside_window"`

	FirstRun  RunGroup `json:"first_run"`
	RepeatRun RunGroup `json:"repeat_run"`

	ByTokenClass []TokenClassBreakdown `json:"by_token_class"`
	ByOrdinal    []OrdinalBucket       `json:"by_ordinal"`

	RunsWithoutCacheAccounting    int     `json:"runs_without_cache_accounting"`
	CostWithoutCacheAccountingUSD float64 `json:"cost_without_cache_accounting_usd"`

	// PricedCostUSD is every unit's tokens at its own model's rates. It
	// covers plain input/output only for pass rows that record them, so like
	// every priced figure here it is not the recorded total.
	PricedCostUSD float64 `json:"priced_cost_usd"`
	// FallbackPricedUnits counts the units priced at FallbackModel because
	// nothing on the row named a model — the part of the pricing that is an
	// assumption rather than a record.
	FallbackPricedUnits int `json:"fallback_priced_units"`
	// ByModel sums to PricedCostUSD; ByPass lists every unit. Populated only
	// when Options.ByModel / Options.ByPass ask for them.
	ByModel []ModelBreakdown `json:"by_model,omitempty"`
	ByPass  []PassBreakdown  `json:"by_pass,omitempty"`
}

// RunSource supplies the run rows a report is built from. The implementation
// must return every run of every PR that has a run in [since, until) — the
// PR's full history, not the window — so ordinals can be derived correctly;
// state.DB.AssayRunHistoryForWindow does exactly that.
type RunSource interface {
	AssayRunHistory(since, until time.Time) ([]RunRecord, error)
}

// ReportRepeatCost is the top-level entry point: fetch the runs for a window
// and build the report over them. A zero since/until is an open bound on that
// side.
func ReportRepeatCost(src RunSource, since, until time.Time, opts Options) (*CostReport, error) {
	if src == nil {
		return nil, fmt.Errorf("cost attribution: nil run source")
	}
	runs, err := src.AssayRunHistory(since, until)
	if err != nil {
		return nil, fmt.Errorf("cost attribution: loading assay runs: %w", err)
	}
	return BuildReport(runs, since, until, opts), nil
}

// ordinalBucketSpec defines the per-ordinal tail. Ordinals 1-5 are reported
// exactly and everything deeper folds into one bucket: past the fifth review of
// one PR the interesting number is "how much is down here", not which rung.
var ordinalBucketSpec = []struct {
	label    string
	min, max int
}{
	{"1", 1, 1},
	{"2", 2, 2},
	{"3", 3, 3},
	{"4", 4, 4},
	{"5", 5, 5},
	{"6+", 6, 0},
}

// BuildReport is the pure core: given runs (a PR's whole history, not just the
// window), the window and the options, produce the report.
//
// The order of operations is the methodology and is not interchangeable:
// filter (skipped, anvil) -> derive ordinals over what survives -> restrict to
// the window -> aggregate. Deriving before filtering would let a skipped run
// occupy ordinal 1; restricting before deriving would relabel every repeat run
// whose first review predates the window.
func BuildReport(runs []RunRecord, since, until time.Time, opts Options) *CostReport {
	fallback, fallbackKey := opts.fallbackRates()
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	report := &CostReport{
		Since:          optionalTime(since),
		Until:          optionalTime(until),
		GeneratedAt:    now,
		Anvil:          opts.Anvil,
		IncludeSkipped: opts.IncludeSkipped,
		FallbackModel:  fallbackKey,
		FirstRun:       RunGroup{Label: GroupFirstRun},
		RepeatRun:      RunGroup{Label: GroupRepeatRun},
	}

	eligible, skippedInWindow := eligibleRuns(runs, since, until, opts)
	report.SkippedRunsExcluded = skippedInWindow

	ordered := DeriveRunOrdinals(eligible)

	// Accumulators. Token-class sums are kept per group and folded into the
	// report total afterwards, so the two can never disagree.
	var (
		firstAcc, repeatAcc groupAccumulator
		buckets             = make([]OrdinalBucket, len(ordinalBucketSpec))
		models              = modelAccumulator{}
	)
	for i, spec := range ordinalBucketSpec {
		buckets[i] = OrdinalBucket{Label: spec.label, MinOrdinal: spec.min, MaxOrdinal: spec.max}
	}

	for _, o := range ordered {
		if !inWindow(o.Run.StartedAt, since, until) {
			report.HistoryRunsOutsideWindow++
			continue
		}
		units := priceRun(o.Run, fallback, fallbackKey)
		if o.IsRepeat() {
			repeatAcc.add(o.Run, units)
		} else {
			firstAcc.add(o.Run, units)
		}
		models.add(o, units, opts.ByPass)
		for i, spec := range ordinalBucketSpec {
			if o.Ordinal < spec.min || (spec.max > 0 && o.Ordinal > spec.max) {
				continue
			}
			buckets[i].Runs++
			buckets[i].CostUSD += o.Run.CostUSD
			buckets[i].CacheCreationTokens += int64(o.Run.CacheCreationTokens)
			buckets[i].CacheReadTokens += int64(o.Run.CacheReadTokens)
			break
		}
	}

	report.FirstRun = firstAcc.group(GroupFirstRun)
	report.RepeatRun = repeatAcc.group(GroupRepeatRun)
	report.ByOrdinal = buckets

	report.TotalRuns = report.FirstRun.Runs + report.RepeatRun.Runs
	// A PR appears in FirstRun only if its first review fell in the window,
	// so PR counts cannot simply be added; the union is counted once here.
	report.TotalPRs = countPRs(firstAcc.prs, repeatAcc.prs)
	report.TotalCostUSD = report.FirstRun.CostUSD + report.RepeatRun.CostUSD
	report.RunsWithoutCacheAccounting = report.FirstRun.RunsWithoutCacheAccounting + report.RepeatRun.RunsWithoutCacheAccounting
	report.CostWithoutCacheAccountingUSD = report.FirstRun.CostWithoutAccountingUSD + report.RepeatRun.CostWithoutAccountingUSD

	var total groupAccumulator
	total.merge(firstAcc)
	total.merge(repeatAcc)
	report.ByTokenClass = total.tokenClasses()

	report.ModelRates = models.rates()
	report.PricedCostUSD = models.pricedTotal()
	report.FallbackPricedUnits = models.fallbackUnits
	if opts.ByModel {
		report.ByModel = models.breakdown()
	}
	if opts.ByPass {
		report.ByPass = models.passes
	}
	return report
}

// eligibleRuns applies the two filters that decide which rows a report in this
// package is built from — the anvil restriction and the skipped-run exclusion —
// and returns how many of the rows it dropped fell inside the window, which is
// the figure a report prints so a default run says what it left out.
//
// It is one function rather than one copy per report because both filters are
// part of the METHODOLOGY: a zero-finding analysis that counted skipped runs
// while the attribution report excluded them would assign the same run two
// different ordinals and the two reports could not be quoted side by side.
// Filtering happens before ordinals are derived for the same reason it does in
// BuildReport: a skipped run occupying ordinal 1 pushes a PR's genuine first
// review to ordinal 2.
func eligibleRuns(runs []RunRecord, since, until time.Time, opts Options) ([]RunRecord, int) {
	eligible := make([]RunRecord, 0, len(runs))
	skippedInWindow := 0
	for _, r := range runs {
		if opts.Anvil != "" && r.Anvil != opts.Anvil {
			continue
		}
		if r.Skipped() && !opts.IncludeSkipped {
			if inWindow(r.StartedAt, since, until) {
				skippedInWindow++
			}
			continue
		}
		eligible = append(eligible, r)
	}
	return eligible, skippedInWindow
}

// inWindow reports whether t falls in [since, until). A zero bound is open on
// that side. The upper bound is exclusive so consecutive windows tile without
// double-counting the run on the boundary.
func inWindow(t, since, until time.Time) bool {
	if !since.IsZero() && t.Before(since) {
		return false
	}
	if !until.IsZero() && !t.Before(until) {
		return false
	}
	return true
}

// groupAccumulator sums one group's runs.
type groupAccumulator struct {
	runs            int
	cost            float64
	cacheCreation   int64
	cacheRead       int64
	creationCost    float64
	readCost        float64
	creationRuns    int
	readRuns        int
	findings        int
	zeroFindingRuns int
	zeroFindingCost float64
	unaccountedRuns int
	unaccountedCost float64
	prs             map[string]struct{}
}

// add folds one run in. units are the run priced per model (priceRun); only
// their cache classes feed a group, whose priced figure is cache-only.
func (a *groupAccumulator) add(r RunRecord, units []pricedUnit) {
	if a.prs == nil {
		a.prs = map[string]struct{}{}
	}
	a.runs++
	a.cost += r.CostUSD
	a.findings += r.FindingsCount
	a.prs[r.PRKey()] = struct{}{}
	if r.FindingsCount == 0 {
		a.zeroFindingRuns++
		a.zeroFindingCost += r.CostUSD
	}
	// Token counts and their cost are taken from the SAME priced units, never
	// the counts from the run row and the cost from the passes: a row whose
	// pass sums disagree with its run totals would otherwise report an
	// effective rate that is neither model's, and a row carrying cache tokens
	// on its passes alone would land in 'unknown' while the by-model breakdown
	// still priced them — breaking the by-model-sums-to-attributed invariant.
	var creation, read int64
	for _, u := range units {
		creation += u.cacheCreation
		read += u.cacheRead
	}
	if creation == 0 && read == 0 {
		a.unaccountedRuns++
		a.unaccountedCost += r.CostUSD
		return
	}
	for _, u := range units {
		a.creationCost += u.cacheCreationCost()
		a.readCost += u.cacheReadCost()
	}
	if creation > 0 {
		a.cacheCreation += creation
		a.creationRuns++
	}
	if read > 0 {
		a.cacheRead += read
		a.readRuns++
	}
}

// merge folds another accumulator in, for the report-wide token classes.
func (a *groupAccumulator) merge(b groupAccumulator) {
	if a.prs == nil {
		a.prs = map[string]struct{}{}
	}
	a.runs += b.runs
	a.cost += b.cost
	a.cacheCreation += b.cacheCreation
	a.cacheRead += b.cacheRead
	a.creationCost += b.creationCost
	a.readCost += b.readCost
	a.creationRuns += b.creationRuns
	a.readRuns += b.readRuns
	a.findings += b.findings
	a.zeroFindingRuns += b.zeroFindingRuns
	a.zeroFindingCost += b.zeroFindingCost
	a.unaccountedRuns += b.unaccountedRuns
	a.unaccountedCost += b.unaccountedCost
	for k := range b.prs {
		a.prs[k] = struct{}{}
	}
}

// tokenClasses reports the accumulated classes. The two cache classes are
// priced from tokens at their own rates — a cache write and a cache read differ
// by more than a factor of ten, so collapsing them into one input rate would
// misattribute the bulk of the traffic — and each unit at its own model's
// rates, so the class cost is already a sum over models by the time it gets
// here. The unknown class reports the recorded cost of the rows no class could
// be derived for.
func (a *groupAccumulator) tokenClasses() []TokenClassBreakdown {
	return []TokenClassBreakdown{
		{
			Class:    TokenClassCacheCreation,
			Runs:     a.creationRuns,
			Tokens:   a.cacheCreation,
			CostUSD:  a.creationCost,
			Basis:    BasisPriced,
			RatePerM: effectiveRate(a.creationCost, a.cacheCreation),
		},
		{
			Class:    TokenClassCacheRead,
			Runs:     a.readRuns,
			Tokens:   a.cacheRead,
			CostUSD:  a.readCost,
			Basis:    BasisPriced,
			RatePerM: effectiveRate(a.readCost, a.cacheRead),
		},
		{
			Class:   TokenClassUnknown,
			Runs:    a.unaccountedRuns,
			Tokens:  0,
			CostUSD: a.unaccountedCost,
			Basis:   BasisRecorded,
		},
	}
}

// effectiveRate is cost per million tokens, zero for no tokens.
func effectiveRate(costUSD float64, tokens int64) float64 {
	if tokens == 0 {
		return 0
	}
	return costUSD / float64(tokens) * 1_000_000
}

// modelAccumulator sums priced units by pricing row, and keeps the per-unit
// rows when they were asked for.
type modelAccumulator struct {
	byKey         map[string]*ModelBreakdown
	runsByKey     map[string]map[int]struct{}
	fallbackUnits int
	passes        []PassBreakdown
}

func (m *modelAccumulator) add(o OrdinalRun, units []pricedUnit, keepPasses bool) {
	if m.byKey == nil {
		m.byKey = map[string]*ModelBreakdown{}
		m.runsByKey = map[string]map[int]struct{}{}
	}
	for _, u := range units {
		b, ok := m.byKey[u.key]
		if !ok {
			b = &ModelBreakdown{Model: u.key, Rates: u.rates}
			m.byKey[u.key] = b
			m.runsByKey[u.key] = map[int]struct{}{}
		}
		m.runsByKey[u.key][o.Run.RunID] = struct{}{}
		b.Runs = len(m.runsByKey[u.key])
		if u.pass == "" {
			b.RunLevelUnits++
		} else {
			b.Passes++
		}
		if u.source == ModelSourceFallback {
			b.FallbackUnits++
			m.fallbackUnits++
		}
		b.InputTokens += u.input
		b.OutputTokens += u.output
		b.CacheCreationTokens += u.cacheCreation
		b.CacheReadTokens += u.cacheRead
		b.InputCostUSD += u.inputCost()
		b.OutputCostUSD += u.outputCost()
		b.CacheCreationCostUSD += u.cacheCreationCost()
		b.CacheReadCostUSD += u.cacheReadCost()
		b.PricedCostUSD += u.pricedCost()
		b.RecordedCostUSD += u.recorded

		if keepPasses {
			m.passes = append(m.passes, PassBreakdown{
				RunID:               o.Run.RunID,
				Anvil:               o.Run.Anvil,
				PRNumber:            o.Run.PRNumber,
				Ordinal:             o.Ordinal,
				Pass:                u.pass,
				Provider:            u.provider,
				RecordedModel:       u.recordedModel,
				Model:               u.key,
				ModelSource:         u.source,
				InputTokens:         u.input,
				OutputTokens:        u.output,
				CacheCreationTokens: u.cacheCreation,
				CacheReadTokens:     u.cacheRead,
				PricedCostUSD:       u.pricedCost(),
				RecordedCostUSD:     u.recorded,
			})
		}
	}
}

// breakdown returns the per-model rows, costliest first, ties on the key so
// the order is repeatable.
func (m *modelAccumulator) breakdown() []ModelBreakdown {
	out := make([]ModelBreakdown, 0, len(m.byKey))
	for _, b := range m.byKey {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PricedCostUSD != out[j].PricedCostUSD {
			return out[i].PricedCostUSD > out[j].PricedCostUSD
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// rates lists every pricing row applied, by key.
func (m *modelAccumulator) rates() []ModelRate {
	out := make([]ModelRate, 0, len(m.byKey))
	for key, b := range m.byKey {
		out = append(out, ModelRate{Model: key, Rates: b.Rates})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// pricedTotal sums the per-model priced cost over the keys in sorted order.
// The order is deliberate: map iteration is random and float addition is not
// associative, so an unordered sum could differ in the last cent between two
// runs over the same data.
func (m *modelAccumulator) pricedTotal() float64 {
	keys := make([]string, 0, len(m.byKey))
	for k := range m.byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var total float64
	for _, k := range keys {
		total += m.byKey[k].PricedCostUSD
	}
	return total
}

func (a *groupAccumulator) group(label string) RunGroup {
	classes := a.tokenClasses()
	g := RunGroup{
		Label:                      label,
		Runs:                       a.runs,
		PRs:                        len(a.prs),
		CostUSD:                    a.cost,
		CacheCreationTokens:        a.cacheCreation,
		CacheReadTokens:            a.cacheRead,
		RunsWithoutCacheAccounting: a.unaccountedRuns,
		CostWithoutAccountingUSD:   a.unaccountedCost,
		Findings:                   a.findings,
		ZeroFindingRuns:            a.zeroFindingRuns,
		ZeroFindingCostUSD:         a.zeroFindingCost,
		ByTokenClass:               classes,
	}
	for _, c := range classes {
		if c.Basis == BasisPriced {
			g.AttributedCostUSD += c.CostUSD
		}
	}
	return g
}

// countPRs counts the union of the PR key sets, so a PR whose first and repeat
// reviews both fall in the window is one PR and not two.
func countPRs(sets ...map[string]struct{}) int {
	union := map[string]struct{}{}
	for _, s := range sets {
		for k := range s {
			union[k] = struct{}{}
		}
	}
	return len(union)
}

// BaselineExpectation is the published figure a report is checked against.
type BaselineExpectation struct {
	RepeatRuns    int
	RepeatCostUSD float64
	// TolUSD is the accepted absolute dollar difference. Zero means the
	// default of one cent, since the published figure is rounded to cents.
	TolUSD float64
}

// BaselineCheck is the reconciliation between a report and a published figure.
type BaselineCheck struct {
	Expected            BaselineExpectation `json:"expected"`
	ActualRepeatRuns    int                 `json:"actual_repeat_runs"`
	ActualRepeatCostUSD float64             `json:"actual_repeat_cost_usd"`
	RunDelta            int                 `json:"run_delta"`
	CostDeltaUSD        float64             `json:"cost_delta_usd"`
	Matches             bool                `json:"matches"`
}

// defaultBaselineTolUSD is a cent: the published baseline is quoted to cents,
// so anything tighter would fail on rounding alone.
const defaultBaselineTolUSD = 0.01

// ValidateBaseline reconciles a report's repeat-run figures against a published
// baseline. It reports the delta rather than deciding what to do about one:
// a mismatch can mean the methodology drifted OR that the data behind the
// published figure is not the data in this database, and only the operator
// knows which. See docs/assay-cost-attribution.md for the reconciliation of the
// $2,326.54 / 780-repeat-run figure this was built against.
func ValidateBaseline(r *CostReport, exp BaselineExpectation) BaselineCheck {
	tol := exp.TolUSD
	if tol <= 0 {
		tol = defaultBaselineTolUSD
	}
	check := BaselineCheck{Expected: exp}
	if r != nil {
		check.ActualRepeatRuns = r.RepeatRun.Runs
		check.ActualRepeatCostUSD = r.RepeatRun.CostUSD
	}
	check.RunDelta = check.ActualRepeatRuns - exp.RepeatRuns
	check.CostDeltaUSD = check.ActualRepeatCostUSD - exp.RepeatCostUSD
	check.Matches = check.RunDelta == 0 && absFloat(check.CostDeltaUSD) <= tol
	return check
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// WriteJSON emits the machine-readable form.
func (r *CostReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteCSV emits the same numbers as WriteTable in a tidy (long) shape:
// section, key, runs, tokens, cost_usd, basis. One row per fact keeps the file
// stable as sections are added, and keeps every figure the table shows
// machine-readable under a name rather than a column position.
func (r *CostReport) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"section", "key", "runs", "tokens", "cost_usd", "basis"}); err != nil {
		return err
	}
	row := func(section, key string, runs int, tokens int64, cost float64, basis string) error {
		return cw.Write([]string{
			section, key,
			strconv.Itoa(runs),
			strconv.FormatInt(tokens, 10),
			strconv.FormatFloat(cost, 'f', 4, 64),
			basis,
		})
	}

	if err := row("total", "all_runs", r.TotalRuns, r.FirstRun.CacheCreationTokens+r.RepeatRun.CacheCreationTokens+r.FirstRun.CacheReadTokens+r.RepeatRun.CacheReadTokens, r.TotalCostUSD, BasisRecorded); err != nil {
		return err
	}
	for _, g := range []RunGroup{r.FirstRun, r.RepeatRun} {
		if err := row("split", g.Label, g.Runs, g.CacheCreationTokens+g.CacheReadTokens, g.CostUSD, BasisRecorded); err != nil {
			return err
		}
		if err := row("split_zero_findings", g.Label, g.ZeroFindingRuns, 0, g.ZeroFindingCostUSD, BasisRecorded); err != nil {
			return err
		}
		for _, c := range g.ByTokenClass {
			if err := row("split_token_class", g.Label+"/"+c.Class, c.Runs, c.Tokens, c.CostUSD, c.Basis); err != nil {
				return err
			}
		}
	}
	for _, c := range r.ByTokenClass {
		if err := row("token_class", c.Class, c.Runs, c.Tokens, c.CostUSD, c.Basis); err != nil {
			return err
		}
	}
	for _, b := range r.ByOrdinal {
		if err := row("ordinal", b.Label, b.Runs, b.CacheCreationTokens+b.CacheReadTokens, b.CostUSD, BasisRecorded); err != nil {
			return err
		}
	}
	if err := row("total", "priced", r.TotalRuns, 0, r.PricedCostUSD, BasisPriced); err != nil {
		return err
	}
	for _, m := range r.ByModel {
		tokens := m.InputTokens + m.OutputTokens + m.CacheCreationTokens + m.CacheReadTokens
		if err := row("model", m.Model, m.Runs, tokens, m.PricedCostUSD, BasisPriced); err != nil {
			return err
		}
		if err := row("model_recorded", m.Model, m.Runs, tokens, m.RecordedCostUSD, BasisRecorded); err != nil {
			return err
		}
	}
	for _, p := range r.ByPass {
		pass := p.Pass
		if pass == "" {
			pass = "(run)"
		}
		key := fmt.Sprintf("%s#%d/%d/%s@%s(%s)", p.Anvil, p.PRNumber, p.RunID, pass, p.Model, p.ModelSource)
		tokens := p.InputTokens + p.OutputTokens + p.CacheCreationTokens + p.CacheReadTokens
		if err := row("pass", key, 1, tokens, p.PricedCostUSD, BasisPriced); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// WriteTable emits the human-readable form: the window, the totals, the
// first-vs-repeat split with run counts, the per-token-class rows and the
// per-ordinal tail.
//
// The caveats are printed with the numbers rather than left to documentation.
// A priced cache total read as "the spend" under-reports every run, and a
// window whose rows predate cache instrumentation reports zero tokens that mean
// "unknown" — both are mistakes made at a glance, so the glance has to carry
// the correction.
func (r *CostReport) WriteTable(w io.Writer) error {
	fmt.Fprintf(w, "Assay repeat-review cost attribution\n")
	fmt.Fprintf(w, "  window            %s\n", formatWindow(r.Since, r.Until))
	if r.Anvil != "" {
		fmt.Fprintf(w, "  anvil             %s\n", r.Anvil)
	}
	fmt.Fprintf(w, "  pricing           per pass, at the rates of the model that ran it (fallback %s)\n", r.FallbackModel)
	for i, m := range r.ModelRates {
		label := ""
		if i == 0 {
			label = "rates"
		}
		fmt.Fprintf(w, "  %-17s %s — input $%.2f/M, output $%.2f/M, cache write $%.2f/M, cache read $%.2f/M\n",
			label, m.Model, m.Rates.InputPerM, m.Rates.OutputPerM, m.Rates.CacheWritePerM, m.Rates.CacheReadPerM)
	}
	fmt.Fprintf(w, "  runs              %d over %d PRs ($%.2f recorded)\n", r.TotalRuns, r.TotalPRs, r.TotalCostUSD)
	if r.SkippedRunsExcluded > 0 {
		fmt.Fprintf(w, "  excluded          %d skipped run(s) — no passes dispatched (--include-skipped to keep)\n", r.SkippedRunsExcluded)
	}
	if r.HistoryRunsOutsideWindow > 0 {
		fmt.Fprintf(w, "  history read      %d earlier run(s), to place window runs at the right ordinal\n", r.HistoryRunsOutsideWindow)
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "GROUP\tRUNS\tPRS\tRECORDED $\tSHARE\tCACHE-W TOK\tCACHE-R TOK\tPRICED CACHE $\tZERO-FIND RUNS\tZERO-FIND $")
	for _, g := range []RunGroup{r.FirstRun, r.RepeatRun} {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%.2f\t%s\t%d\t%d\t%.2f\t%d\t%.2f\n",
			g.Label, g.Runs, g.PRs, g.CostUSD, percentOf(g.CostUSD, r.TotalCostUSD),
			g.CacheCreationTokens, g.CacheReadTokens, g.AttributedCostUSD,
			g.ZeroFindingRuns, g.ZeroFindingCostUSD)
	}
	fmt.Fprintf(tw, "TOTAL\t%d\t%d\t%.2f\t%s\t%d\t%d\t%.2f\t%d\t%.2f\n",
		r.TotalRuns, r.TotalPRs, r.TotalCostUSD, percentOf(r.TotalCostUSD, r.TotalCostUSD),
		r.FirstRun.CacheCreationTokens+r.RepeatRun.CacheCreationTokens,
		r.FirstRun.CacheReadTokens+r.RepeatRun.CacheReadTokens,
		r.FirstRun.AttributedCostUSD+r.RepeatRun.AttributedCostUSD,
		r.FirstRun.ZeroFindingRuns+r.RepeatRun.ZeroFindingRuns,
		r.FirstRun.ZeroFindingCostUSD+r.RepeatRun.ZeroFindingCostUSD)
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)

	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TOKEN CLASS\tRUNS\tTOKENS\tEFF RATE $/M\tCOST $\tBASIS")
	for _, c := range r.ByTokenClass {
		rate := "-"
		if c.RatePerM > 0 {
			rate = fmt.Sprintf("%.2f", c.RatePerM)
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%.2f\t%s\n", c.Class, c.Runs, c.Tokens, rate, c.CostUSD, c.Basis)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(w)

	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ORDINAL\tRUNS\tRECORDED $\tSHARE\tCACHE-W TOK\tCACHE-R TOK")
	for _, b := range r.ByOrdinal {
		fmt.Fprintf(tw, "%s\t%d\t%.2f\t%s\t%d\t%d\n", b.Label, b.Runs, b.CostUSD,
			percentOf(b.CostUSD, r.TotalCostUSD), b.CacheCreationTokens, b.CacheReadTokens)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if r.ByModel != nil {
		fmt.Fprintln(w)
		if err := r.writeByModelTable(w); err != nil {
			return err
		}
	}
	if r.ByPass != nil {
		fmt.Fprintln(w)
		if err := r.writeByPassTable(w); err != nil {
			return err
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Notes:")
	fmt.Fprintln(w, "  - RECORDED $ is the provider's own cost_usd. PRICED CACHE $ is cache tokens x their")
	fmt.Fprintln(w, "    own rates and is a SUBSET of it: it leaves out input and output tokens, which only")
	fmt.Fprintln(w, "    rows with per-pass token counts record (older rows carry cache tokens alone), so the")
	fmt.Fprintln(w, "    two cache classes can never sum to the recorded total.")
	if r.RunsWithoutCacheAccounting > 0 {
		fmt.Fprintf(w, "  - %d of %d run(s) ($%.2f, %s of recorded spend) carry no cache accounting and are\n",
			r.RunsWithoutCacheAccounting, r.TotalRuns, r.CostWithoutCacheAccountingUSD,
			percentOf(r.CostWithoutCacheAccountingUSD, r.TotalCostUSD))
		fmt.Fprintln(w, "    reported as token class 'unknown'. Zero cache tokens on a row means 'not knowable'")
		fmt.Fprintln(w, "    (pre-instrumentation row, or a backend that reports none), not 'no cache traffic'.")
	}
	fmt.Fprintln(w, "  - Each pass is priced at the model recorded on its own row; a pass naming none takes the")
	fmt.Fprintln(w, "    run's model, and a row naming neither takes the fallback. Rows predating per-pass")
	fmt.Fprintln(w, "    tokens are priced whole, from their run-level cache tokens.")
	if r.FallbackPricedUnits > 0 {
		fmt.Fprintf(w, "  - %d unit(s) named no model that resolves and were priced at the fallback %s.\n",
			r.FallbackPricedUnits, r.FallbackModel)
	}
	fmt.Fprintln(w, "  - Ordinals are derived over each PR's full review history, then restricted to the window.")
	return nil
}

// writeByModelTable renders the per-model breakdown with a TOTAL row, which is
// PricedCostUSD by construction.
func (r *CostReport) writeByModelTable(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tRUNS\tPASSES\tRUN-LEVEL\tINPUT TOK\tOUTPUT TOK\tCACHE-W TOK\tCACHE-R TOK\tPRICED $\tSHARE\tRECORDED $")
	var total ModelBreakdown
	for _, m := range r.ByModel {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%.2f\t%s\t%.2f\n",
			m.Model, m.Runs, m.Passes, m.RunLevelUnits, m.InputTokens, m.OutputTokens,
			m.CacheCreationTokens, m.CacheReadTokens, m.PricedCostUSD,
			percentOf(m.PricedCostUSD, r.PricedCostUSD), m.RecordedCostUSD)
		total.Passes += m.Passes
		total.RunLevelUnits += m.RunLevelUnits
		total.InputTokens += m.InputTokens
		total.OutputTokens += m.OutputTokens
		total.CacheCreationTokens += m.CacheCreationTokens
		total.CacheReadTokens += m.CacheReadTokens
		total.RecordedCostUSD += m.RecordedCostUSD
	}
	// RUNS is not summed: a mixed run counts under every model it used.
	fmt.Fprintf(tw, "TOTAL\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%.2f\t%s\t%.2f\n",
		r.TotalRuns, total.Passes, total.RunLevelUnits, total.InputTokens, total.OutputTokens,
		total.CacheCreationTokens, total.CacheReadTokens, r.PricedCostUSD,
		percentOf(r.PricedCostUSD, r.PricedCostUSD), total.RecordedCostUSD)
	return tw.Flush()
}

// writeByPassTable renders every priced unit. A run priced whole reads "(run)"
// in the PASS column, and the SOURCE column says which level named the model.
func (r *CostReport) writeByPassTable(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tPR\tORDINAL\tPASS\tPROVIDER\tMODEL\tSOURCE\tINPUT TOK\tOUTPUT TOK\tCACHE-W TOK\tCACHE-R TOK\tPRICED $\tRECORDED $")
	for _, p := range r.ByPass {
		pass := p.Pass
		if pass == "" {
			pass = "(run)"
		}
		provider := p.Provider
		if provider == "" {
			provider = "-"
		}
		fmt.Fprintf(tw, "%d\t%s#%d\t%d\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%.4f\t%.4f\n",
			p.RunID, p.Anvil, p.PRNumber, p.Ordinal, pass, provider, p.Model, p.ModelSource,
			p.InputTokens, p.OutputTokens, p.CacheCreationTokens, p.CacheReadTokens,
			p.PricedCostUSD, p.RecordedCostUSD)
	}
	return tw.Flush()
}

// optionalTime turns a zero time into a nil pointer, which is how an open
// window bound is represented once it leaves BuildReport.
func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// formatWindow renders the reported interval, naming an open bound rather than
// printing a zero time.
func formatWindow(since, until *time.Time) string {
	s, u := "(open)", "(open)"
	if since != nil {
		s = since.UTC().Format(time.RFC3339)
	}
	if until != nil {
		u = until.UTC().Format(time.RFC3339)
	}
	return s + " .. " + u
}

// percentOf renders a share, answering "-" rather than NaN for a zero total.
func percentOf(part, total float64) string {
	if total == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", part/total*100)
}
