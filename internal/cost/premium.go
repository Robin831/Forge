package cost

import "sync"

// CopilotPremiumMultiplier returns the premium request multiplier for a given
// Copilot model name. Models not in the active table default to 1.0x. The table
// is configurable via settings.copilot_premium_multipliers; the daemon installs
// overrides on load and on every hot-reload.
func CopilotPremiumMultiplier(model string) float64 {
	premiumMu.RLock()
	defer premiumMu.RUnlock()
	if m, ok := activePremium[model]; ok {
		return m
	}
	return 1.0
}

// DefaultCopilotPremiumMultipliers returns a fresh copy of the built-in Copilot
// premium-request multiplier table. These are the defaults that reproduce
// Forge's historical hardcoded values; settings.copilot_premium_multipliers
// overrides individual entries on top of this table. Callers may freely mutate
// the returned map.
//
// Reference (2026 pricing):
//
//	claude-opus-4.6:        3x
//	claude-opus-4.6-fast:  30x
//	claude-opus-4.5:        3x
//	claude-sonnet-4.6/4.5/4: 1x
//	claude-haiku-4.5:       0.33x
//	gpt-5.4/5.3-codex/5.2-codex/5.2/5.1-codex-max/5.1-codex/5.1: 1x
//	gpt-5.1-codex-mini:     0.33x
//	gpt-5-mini/gpt-4.1:     0x (free)
//	gemini-3-pro-preview/gemini-2.5-pro: 1x
func DefaultCopilotPremiumMultipliers() map[string]float64 {
	return map[string]float64{
		// Claude models
		"claude-opus-4.6":      3.0,
		"claude-opus-4.6-fast": 30.0,
		"claude-opus-4.5":      3.0,
		"claude-sonnet-4.6":    1.0,
		"claude-sonnet-4.5":    1.0,
		"claude-sonnet-4":      1.0,
		"claude-haiku-4.5":     0.33,

		// GPT models
		"gpt-5.4":            1.0,
		"gpt-5.3-codex":      1.0,
		"gpt-5.2-codex":      1.0,
		"gpt-5.2":            1.0,
		"gpt-5.1-codex-max":  1.0,
		"gpt-5.1-codex":      1.0,
		"gpt-5.1":            1.0,
		"gpt-5.1-codex-mini": 0.33,
		"gpt-5-mini":         0.0,
		"gpt-4.1":            0.0,

		// Gemini models
		"gemini-3-pro-preview": 1.0,
		"gemini-2.5-pro":       1.0,
	}
}

var (
	premiumMu     sync.RWMutex
	activePremium = DefaultCopilotPremiumMultipliers()
)

// SetCopilotPremiumMultipliers overlays the given overrides on top of the
// built-in defaults and installs the result as the active multiplier table.
// Passing a nil or empty map resets the table to the built-in defaults. Safe
// for concurrent use; called at daemon startup and on every config hot-reload.
func SetCopilotPremiumMultipliers(overrides map[string]float64) {
	table := DefaultCopilotPremiumMultipliers()
	for model, m := range overrides {
		table[model] = m
	}
	premiumMu.Lock()
	activePremium = table
	premiumMu.Unlock()
}
