package cost

import (
	"testing"

	"github.com/Robin831/Forge/internal/provider"
)

func TestPricingForTier(t *testing.T) {
	tests := []struct {
		tier       string
		wantInput  float64
		wantOutput float64
	}{
		{"haiku", 1.00, 5.00},
		{"HAIKU", 1.00, 5.00},
		{"  sonnet  ", 3.00, 15.00},
		{"opus", 15.00, 75.00},
		{"Opus", 15.00, 75.00},
	}
	for _, tt := range tests {
		got := PricingForTier(tt.tier)
		if got.InputPerM != tt.wantInput {
			t.Errorf("PricingForTier(%q).InputPerM = %v, want %v", tt.tier, got.InputPerM, tt.wantInput)
		}
		if got.OutputPerM != tt.wantOutput {
			t.Errorf("PricingForTier(%q).OutputPerM = %v, want %v", tt.tier, got.OutputPerM, tt.wantOutput)
		}
	}

	// Unknown and empty tiers fall back to DefaultPricing.
	for _, tier := range []string{"", "gpt-5", "unknown"} {
		if got := PricingForTier(tier); got != DefaultPricing() {
			t.Errorf("PricingForTier(%q) = %+v, want DefaultPricing() %+v", tier, got, DefaultPricing())
		}
	}
}

// TestDefaultsMatchPreviousConstants pins the built-in defaults to the exact
// values Forge shipped before pricing became configurable, so an accidental
// edit to the table is caught.
func TestDefaultsMatchPreviousConstants(t *testing.T) {
	// Ensure a clean default table regardless of test ordering.
	SetPricingTable(nil)

	cases := []struct {
		name string
		got  Pricing
		want Pricing
	}{
		{"DefaultPricing", DefaultPricing(), Pricing{3.00, 15.00, 0.30, 3.75}},
		{"CopilotPricing", CopilotPricing(), Pricing{3.00, 15.00, 0.30, 3.75}},
		{"GeminiPricing", GeminiPricing(), Pricing{3.50, 10.50, 0.00, 0.00}},
		{"OpenAIPricing", OpenAIPricing(), Pricing{2.50, 10.00, 0.00, 0.00}},
		{"tier haiku", PricingForTier("haiku"), Pricing{1.00, 5.00, 0.10, 1.25}},
		{"tier opus", PricingForTier("opus"), Pricing{15.00, 75.00, 1.50, 18.75}},
		{"tier sonnet", PricingForTier("sonnet"), Pricing{3.00, 15.00, 0.30, 3.75}},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %+v, want %+v", tc.name, tc.got, tc.want)
		}
	}
}

// TestSetPricingTableOverride verifies a config override for a single model is
// respected and that unlisted models retain their defaults (overlay semantics).
func TestSetPricingTableOverride(t *testing.T) {
	t.Cleanup(func() { SetPricingTable(nil) })

	SetPricingTable(map[string]Pricing{
		ModelGemini: {InputPerM: 99, OutputPerM: 199},
	})

	if got := GeminiPricing(); got.InputPerM != 99 || got.OutputPerM != 199 {
		t.Errorf("GeminiPricing() after override = %+v, want input 99 output 199", got)
	}
	// The fallback path used by smith.go must see the override too.
	if got := FallbackPricing(provider.Gemini, ""); got.InputPerM != 99 {
		t.Errorf("FallbackPricing(gemini) = %+v, want input 99", got)
	}
	// Unlisted models keep their defaults.
	if got := OpenAIPricing(); got != (Pricing{2.50, 10.00, 0.00, 0.00}) {
		t.Errorf("OpenAIPricing() after gemini override = %+v, want default", got)
	}

	// Resetting restores the default.
	SetPricingTable(nil)
	if got := GeminiPricing(); got.InputPerM != 3.50 {
		t.Errorf("GeminiPricing() after reset = %+v, want default input 3.50", got)
	}
}

// TestFallbackPricingFamilyInference checks that a versioned provider model id
// resolves to the right family row.
func TestFallbackPricingFamilyInference(t *testing.T) {
	t.Cleanup(func() { SetPricingTable(nil) })
	SetPricingTable(nil)

	// A Copilot opus model should price at the opus row (3x sonnet output).
	if got := FallbackPricing(provider.Copilot, "claude-opus-4.6"); got.OutputPerM != 75.00 {
		t.Errorf("FallbackPricing(copilot, claude-opus-4.6).OutputPerM = %v, want 75.00", got.OutputPerM)
	}
	// An unknown Copilot model falls back to the Claude Sonnet default.
	if got := FallbackPricing(provider.Copilot, ""); got.OutputPerM != 15.00 {
		t.Errorf("FallbackPricing(copilot, \"\").OutputPerM = %v, want 15.00", got.OutputPerM)
	}
}

// TestFirstFallbackTodayGuard verifies the once-per-day-per-model log guard
// fires exactly once for a given key+date and again on a new date.
func TestFirstFallbackTodayGuard(t *testing.T) {
	fallbackLogMu.Lock()
	fallbackLogged = map[string]string{}
	fallbackLogMu.Unlock()

	const key = "gemini/gemini-2.5-pro"
	if !firstFallbackToday(key, "2026-07-14") {
		t.Fatal("first call for a date should return true")
	}
	if firstFallbackToday(key, "2026-07-14") {
		t.Error("second call for the same date should return false")
	}
	if firstFallbackToday(key, "2026-07-14") {
		t.Error("third call for the same date should still return false")
	}
	if !firstFallbackToday(key, "2026-07-15") {
		t.Error("a new date should return true again")
	}
	// A different model on an already-seen date is independent.
	if !firstFallbackToday("openai/gpt-5.1", "2026-07-14") {
		t.Error("a different model key should log independently")
	}
}
