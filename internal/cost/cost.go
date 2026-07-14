// Package cost tracks token usage and estimated costs from AI CLI output.
//
// Claude self-reports total_cost_usd in its stream-json result event, so its
// cost is exact. Other providers (Copilot, Gemini, OpenAI/Codex) usually emit
// zero or no cost, so this package estimates their spend from token counts and
// a configurable per-model pricing table (see DefaultPricingTable). The
// estimate is a fallback only — Claude's self-reported figure always wins.
//
// Default token pricing (Claude Sonnet 4-class, USD per 1M tokens):
//
//	Input:       $3.00
//	Output:      $15.00
//	Cache read:  $0.30
//	Cache write: $3.75
//
// Operators can override any model's rates via settings.pricing and the
// Copilot premium multipliers via settings.copilot_premium_multipliers; the
// daemon pushes those into this package on load and on every hot-reload.
package cost

import (
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
		ModelClaudeOpus:   {InputPerM: 15.00, OutputPerM: 75.00, CacheReadPerM: 1.50, CacheWritePerM: 18.75},
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
// settings ("haiku", "sonnet", or "opus"); the tier string is matched
// case-insensitively after trimming surrounding whitespace. Unknown or empty
// tiers fall back to the Claude Sonnet defaults.
func PricingForTier(tier string) Pricing {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "haiku":
		return lookupPricing(ModelClaudeHaiku)
	case "opus":
		return lookupPricing(ModelClaudeOpus)
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
