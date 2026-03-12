package cost

// CopilotPremiumMultiplier returns the premium request multiplier for a given
// Copilot model name. Models not in the lookup table default to 1.0x.
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
func CopilotPremiumMultiplier(model string) float64 {
	if m, ok := premiumMultipliers[model]; ok {
		return m
	}
	return 1.0
}

var premiumMultipliers = map[string]float64{
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
