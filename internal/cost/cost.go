// Package cost tracks token usage and estimated costs from AI CLI output.
//
// Claude self-reports total_cost_usd in its stream-json result event, so its
// recorded cost is exact. Other providers (Copilot, Gemini, OpenAI/Codex)
// usually emit zero or no cost, so this package estimates their spend from
// token counts and a configurable per-model pricing table (see
// DefaultPricingTable). For a finished session the estimate is a fallback only
// — Claude's self-reported figure always wins. The one place the table prices
// Claude itself is in flight: the Assay per-pass cost ceiling adds up each
// turn's usage as it streams, because the provider's own figure arrives only
// with the final result event, after the money is spent. That is why the
// Claude rows must track list price too — a stale row there is not a wrong
// number in a report, it is a pass killed on its first turn.
//
// Default token pricing (Claude Sonnet 4-class, USD per 1M tokens):
//
//	Input:       $3.00
//	Output:      $15.00
//	Cache read:  $0.30
//	Cache write: $3.75
//
// Cache-write rates are the 5-minute-TTL figure (1.25x input). Claude Code
// writes 1-hour entries (2x input), so a cache-write-heavy turn estimates
// about 20% low against the bill. That is the intended failure direction for
// the ceiling — an estimate that reads low delays a stop; one that reads high
// kills a healthy pass — and an operator who wants the 1-hour figure sets
// cache_write_per_m in settings.pricing.
//
// Operators can override any model's rates via settings.pricing and the
// Copilot premium multipliers via settings.copilot_premium_multipliers; the
// daemon pushes those into this package on load and on every hot-reload.
package cost

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/provider"
)

// Model pricing keys used by the default pricing table and the provider
// fallback resolver. Operators reference these keys in settings.pricing.
const (
	ModelClaudeSonnet = "claude-sonnet"
	ModelClaudeHaiku  = "claude-haiku"
	ModelClaudeOpus   = "claude-opus"
	ModelClaudeFable  = "claude-fable"
	ModelGemini       = "gemini"
	ModelOpenAI       = "openai"
)

// Pricing defines per-token costs in USD per million tokens.
type Pricing struct {
	InputPerM      float64 `json:"input_per_m"`
	OutputPerM     float64 `json:"output_per_m"`
	CacheReadPerM  float64 `json:"cache_read_per_m"`
	CacheWritePerM float64 `json:"cache_write_per_m"`
}

// DefaultPricingTable returns a fresh copy of the built-in per-model pricing
// table. These values are the single source of truth for the defaults that
// reproduce Forge's historical hardcoded rates; settings.pricing overrides
// individual entries on top of this table. Callers may freely mutate the
// returned map.
func DefaultPricingTable() map[string]Pricing {
	return map[string]Pricing{
		// Claude Sonnet 4-class (also used as the Copilot fallback, since
		// Copilot runs Claude models under the hood).
		ModelClaudeSonnet: {InputPerM: 3.00, OutputPerM: 15.00, CacheReadPerM: 0.30, CacheWritePerM: 3.75},
		ModelClaudeHaiku:  {InputPerM: 1.00, OutputPerM: 5.00, CacheReadPerM: 0.10, CacheWritePerM: 1.25},
		// Opus 4.5 and later, Opus 5 included. Opus 4.1 and earlier were
		// $15/$75 — three times this — and that row survived here long after
		// every anvil had moved on: a 165K-token cache write on Opus 5 was
		// estimated at $3.13 against a real $1.04 and tripped a $1.50 per-pass
		// ceiling on the first turn. An anvil still pinned to an old Opus
		// overrides this row in settings.pricing.
		ModelClaudeOpus: {InputPerM: 5.00, OutputPerM: 25.00, CacheReadPerM: 0.50, CacheWritePerM: 6.25},
		// Claude Fable 5 / Mythos 5 — twice Opus. Before this row existed a
		// "fable" model matched no family and priced at the Sonnet row, five
		// times under, which is why the ceiling never fired while Assay was
		// running on it and fired at once when it moved to Opus.
		ModelClaudeFable: {InputPerM: 10.00, OutputPerM: 50.00, CacheReadPerM: 1.00, CacheWritePerM: 12.50},
		// Gemini 1.5 Pro-class. Gemini caching pricing differs and is not
		// modelled here.
		ModelGemini: {InputPerM: 3.50, OutputPerM: 10.50},
		// Generic GPT-5.x-class placeholder for all OpenAI/Codex models. The
		// OpenAI provider supports several model IDs with varying prices, so
		// this is an approximate estimate only.
		ModelOpenAI: {InputPerM: 2.50, OutputPerM: 10.00},
	}
}

var (
	pricingMu     sync.RWMutex
	activePricing = DefaultPricingTable()
)

// SetPricingTable overlays the given per-model overrides on top of the built-in
// defaults and installs the result as the active pricing table. Passing a nil
// or empty map resets the table to the built-in defaults. The overlay design
// means an operator can override a single model's rates without having to
// restate every other model. Safe for concurrent use; called at daemon startup
// and on every config hot-reload.
func SetPricingTable(overrides map[string]Pricing) {
	table := DefaultPricingTable()
	for model, p := range overrides {
		table[model] = p
	}
	pricingMu.Lock()
	activePricing = table
	pricingMu.Unlock()
}

// lookupPricing returns the active rates for a known model key, falling back to
// the Claude Sonnet defaults if the key is somehow absent (it never is under
// SetPricingTable's overlay semantics, but the guard keeps callers total).
func lookupPricing(model string) Pricing {
	pricingMu.RLock()
	defer pricingMu.RUnlock()
	if p, ok := activePricing[model]; ok {
		return p
	}
	return Pricing{InputPerM: 3.00, OutputPerM: 15.00, CacheReadPerM: 0.30, CacheWritePerM: 3.75}
}

// DefaultPricing returns the active Claude Sonnet 4-class pricing.
func DefaultPricing() Pricing { return lookupPricing(ModelClaudeSonnet) }

// CopilotPricing returns the active fallback pricing for GitHub Copilot.
// Copilot runs Claude models under the hood, so the Claude Sonnet rates are a
// reasonable estimate for token-level tracking.
func CopilotPricing() Pricing { return lookupPricing(ModelClaudeSonnet) }

// GeminiPricing returns the active fallback pricing for Gemini.
func GeminiPricing() Pricing { return lookupPricing(ModelGemini) }

// OpenAIPricing returns the active fallback pricing for OpenAI/Codex models.
func OpenAIPricing() Pricing { return lookupPricing(ModelOpenAI) }

// PricingForTier returns the Assay stage cost classification for a given model
// tier. Assay estimates its review cost from the model_tier configured in its
// settings ("haiku", "sonnet", "opus" or "fable"); the tier string is matched
// case-insensitively after trimming surrounding whitespace. Unknown or empty
// tiers fall back to the Claude Sonnet defaults.
func PricingForTier(tier string) Pricing {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "haiku":
		return lookupPricing(ModelClaudeHaiku)
	case "opus":
		return lookupPricing(ModelClaudeOpus)
	case "fable":
		return lookupPricing(ModelClaudeFable)
	case "sonnet":
		return lookupPricing(ModelClaudeSonnet)
	default:
		return lookupPricing(ModelClaudeSonnet)
	}
}

// FallbackPricing returns the estimated pricing for a provider/model pair that
// did not self-report a cost, consulting the configured pricing table. It is
// the single entry point used by smith.go's fallback cost computation for
// Copilot, Gemini, and OpenAI/Codex.
//
// Resolution order: an exact model-key match in the table, then a family
// inference from the model name (e.g. the versioned Copilot id
// "claude-opus-4.6" maps to the claude-opus row), then the provider's default
// key. To make table drift visible, it logs at info level the first time a
// fallback estimate is applied for a given provider/model each calendar day.
func FallbackPricing(kind provider.Kind, model string) Pricing {
	p := resolvePricing(kind, model)
	logFallbackOncePerDay(kind, model, p)
	return p
}

// EstimatePricing returns the active pricing for a provider/model pair without
// FallbackPricing's once-a-day log line. It is for in-flight estimates — a
// running session's spend, priced turn by turn from streamed token counts
// before the provider has reported anything — where the log FallbackPricing
// emits would be wrong twice over: it is not a fallback (the provider will
// report its own cost when the session ends, and that is what gets recorded),
// and it would fire on every healthy run rather than on the drift it exists to
// make visible.
func EstimatePricing(kind provider.Kind, model string) Pricing {
	return resolvePricing(kind, model)
}

// resolvePricing looks up the active pricing for a provider/model pair without
// any logging side effect.
func resolvePricing(kind provider.Kind, model string) Pricing {
	pricingMu.RLock()
	defer pricingMu.RUnlock()

	if model != "" {
		if p, ok := activePricing[model]; ok {
			return p
		}
		// Family inference from the model name so versioned provider model
		// ids (e.g. "claude-opus-4.6") resolve to the right row.
		lower := strings.ToLower(model)
		switch {
		// Fable/Mythos first: it shares no substring with the other families
		// today, but it is the row a miss would misprice by the most.
		case strings.Contains(lower, "fable"), strings.Contains(lower, "mythos"):
			if p, ok := activePricing[ModelClaudeFable]; ok {
				return p
			}
		case strings.Contains(lower, "opus"):
			if p, ok := activePricing[ModelClaudeOpus]; ok {
				return p
			}
		case strings.Contains(lower, "haiku"):
			if p, ok := activePricing[ModelClaudeHaiku]; ok {
				return p
			}
		case strings.Contains(lower, "sonnet"):
			if p, ok := activePricing[ModelClaudeSonnet]; ok {
				return p
			}
		}
	}

	if p, ok := activePricing[defaultModelKeyForKind(kind)]; ok {
		return p
	}
	return Pricing{InputPerM: 3.00, OutputPerM: 15.00, CacheReadPerM: 0.30, CacheWritePerM: 3.75}
}

// defaultModelKeyForKind returns the default pricing key for a provider kind
// when the specific model is unknown. Copilot maps to the Claude Sonnet key
// because it runs Claude models under the hood.
func defaultModelKeyForKind(kind provider.Kind) string {
	switch kind {
	case provider.Gemini:
		return ModelGemini
	case provider.OpenAI:
		return ModelOpenAI
	default: // Claude, Copilot
		return ModelClaudeSonnet
	}
}

var (
	fallbackLogMu  sync.Mutex
	fallbackLogged = map[string]string{} // "kind/model" -> YYYY-MM-DD last logged
)

// logFallbackOncePerDay emits an info log the first time a fallback pricing
// estimate is applied for a given provider/model on the current calendar day,
// suppressing repeats so a busy day produces at most one line per model.
func logFallbackOncePerDay(kind provider.Kind, model string, p Pricing) {
	key := string(kind) + "/" + model
	if !firstFallbackToday(key, Today()) {
		return
	}
	slog.Info("cost: applying fallback pricing estimate (provider did not self-report cost); adjust settings.pricing if this drifts from real billing",
		"provider", string(kind),
		"model", model,
		"input_per_m", p.InputPerM,
		"output_per_m", p.OutputPerM,
	)
}

// firstFallbackToday records that a fallback estimate was logged for key on the
// given date and reports whether this is the first time for that date. It is
// the testable core of the once-per-day-per-model guard.
func firstFallbackToday(key, date string) bool {
	fallbackLogMu.Lock()
	defer fallbackLogMu.Unlock()
	if fallbackLogged[key] == date {
		return false
	}
	fallbackLogged[key] = date
	return true
}

// Usage tracks token usage for a single Claude invocation.
type Usage struct {
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
}

// Add merges another Usage into this one.
func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
	u.EstimatedCostUSD += other.EstimatedCostUSD
}

// IsZero reports whether the usage carries nothing worth persisting.
//
// Cache tokens count toward "did this session use anything": a session served
// almost entirely from a prompt cache reports large cache reads next to
// negligible input, and treating that as empty would drop the very sessions the
// cache columns exist to make visible.
func (u Usage) IsZero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 &&
		u.EstimatedCostUSD == 0
}

// Sink is the persistence side of cost recording: the three tables one
// completed provider session lands in — the daily aggregate, the per-provider
// daily aggregate and the bead's cumulative row. *state.DB implements it.
//
// It is an interface here rather than a concrete dependency because this
// package is imported by state's own importers; taking the three methods keeps
// the fan-out in one place without inverting that import.
type Sink interface {
	AddDailyCost(date string, input, output, cacheRead, cacheWrite int, cost float64) error
	AddProviderDailyCost(date, prov string, input, output, cacheRead, cacheWrite int, cost float64) error
	AddBeadCost(beadID, anvil string, input, output, cacheRead, cacheWrite int, cost float64) error
}

// Record persists one completed provider session's usage into all three cost
// tables. It is the single fan-out every stage that spawns a session goes
// through — Smith, Schematic, Warden, Assay and the quench/burnish/rebase fix
// workers — so no stage can record two of the three tables, or pass a literal
// zero where the provider reported real cache accounting.
//
// A zero usage writes nothing (a rate-limited spawn is not a completion), and a
// nil sink is a no-op so callers without a DB need no guard of their own.
// Callers holding a typed nil pointer (a nil *state.DB) must still check it
// themselves — a typed nil in an interface is not a nil interface.
//
// Every write is attempted even when an earlier one fails: the tables are
// independent, and a failed daily write is no reason to lose the bead's row.
// The errors come back joined and named by table, since cost accounting is
// best-effort but a silently broken cost table is exactly the failure this
// accounting exists to make visible.
func Record(sink Sink, providerName, beadID, anvil string, u Usage) error {
	if sink == nil || u.IsZero() {
		return nil
	}
	today := Today()
	var errs []error
	if err := sink.AddDailyCost(today, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.EstimatedCostUSD); err != nil {
		errs = append(errs, fmt.Errorf("daily_costs: %w", err))
	}
	if err := sink.AddProviderDailyCost(today, providerName, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.EstimatedCostUSD); err != nil {
		errs = append(errs, fmt.Errorf("provider_daily_costs: %w", err))
	}
	if err := sink.AddBeadCost(beadID, anvil, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.EstimatedCostUSD); err != nil {
		errs = append(errs, fmt.Errorf("bead_costs: %w", err))
	}
	return errors.Join(errs...)
}

// Calculate computes the estimated cost based on pricing.
func (u *Usage) Calculate(p Pricing) {
	u.EstimatedCostUSD = float64(u.InputTokens)*p.InputPerM/1_000_000 +
		float64(u.OutputTokens)*p.OutputPerM/1_000_000 +
		float64(u.CacheReadTokens)*p.CacheReadPerM/1_000_000 +
		float64(u.CacheWriteTokens)*p.CacheWritePerM/1_000_000
}

// BeadCost stores cumulative cost data for a specific bead.
type BeadCost struct {
	BeadID    string    `json:"bead_id"`
	Anvil     string    `json:"anvil"`
	Usage     Usage     `json:"usage"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DailyCost stores aggregated cost data for a specific day.
type DailyCost struct {
	Date  string  `json:"date"` // YYYY-MM-DD
	Usage Usage   `json:"usage"`
	Limit float64 `json:"limit,omitempty"` // 0 = no limit
}

// Today returns today's date string in YYYY-MM-DD format.
func Today() string {
	return time.Now().Format("2006-01-02")
}
