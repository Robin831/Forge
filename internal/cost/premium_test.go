package cost

import "testing"

func TestCopilotPremiumMultiplier(t *testing.T) {
	tests := []struct {
		model string
		want  float64
	}{
		{"claude-opus-4.6", 3.0},
		{"claude-opus-4.6-fast", 30.0},
		{"claude-opus-4.5", 3.0},
		{"claude-sonnet-4.6", 1.0},
		{"claude-sonnet-4.5", 1.0},
		{"claude-sonnet-4", 1.0},
		{"claude-haiku-4.5", 0.33},
		{"gpt-5.4", 1.0},
		{"gpt-5.1-codex-max", 1.0},
		{"gpt-5.1-codex-mini", 0.33},
		{"gpt-5-mini", 0.0},
		{"gpt-4.1", 0.0},
		{"gemini-2.5-pro", 1.0},
		{"gemini-3-pro-preview", 1.0},
		// Unknown model defaults to 1.0
		{"unknown-model-x", 1.0},
		{"", 1.0},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := CopilotPremiumMultiplier(tt.model)
			if got != tt.want {
				t.Errorf("CopilotPremiumMultiplier(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

// TestSetCopilotPremiumMultipliersOverride verifies a config override for a
// single model is respected while unlisted models keep their defaults.
func TestSetCopilotPremiumMultipliersOverride(t *testing.T) {
	t.Cleanup(func() { SetCopilotPremiumMultipliers(nil) })

	SetCopilotPremiumMultipliers(map[string]float64{
		"claude-opus-4.6": 5.0,
	})

	if got := CopilotPremiumMultiplier("claude-opus-4.6"); got != 5.0 {
		t.Errorf("CopilotPremiumMultiplier(claude-opus-4.6) after override = %v, want 5.0", got)
	}
	// Unlisted models keep their defaults.
	if got := CopilotPremiumMultiplier("claude-haiku-4.5"); got != 0.33 {
		t.Errorf("CopilotPremiumMultiplier(claude-haiku-4.5) = %v, want default 0.33", got)
	}

	// Resetting restores the default.
	SetCopilotPremiumMultipliers(nil)
	if got := CopilotPremiumMultiplier("claude-opus-4.6"); got != 3.0 {
		t.Errorf("CopilotPremiumMultiplier(claude-opus-4.6) after reset = %v, want default 3.0", got)
	}
}
