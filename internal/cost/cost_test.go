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
		{"opus", 5.00, 25.00},
		{"Opus", 5.00, 25.00},
		{"fable", 10.00, 50.00},
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
		{"tier opus", PricingForTier("opus"), Pricing{5.00, 25.00, 0.50, 6.25}},
		{"tier fable", PricingForTier("fable"), Pricing{10.00, 50.00, 1.00, 12.50}},
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

	// A Copilot opus model should price at the opus row.
	if got := FallbackPricing(provider.Copilot, "claude-opus-4.6"); got.OutputPerM != 25.00 {
		t.Errorf("FallbackPricing(copilot, claude-opus-4.6).OutputPerM = %v, want 25.00", got.OutputPerM)
	}
	// The Claude CLI's own model ids resolve the same way, and a Fable or
	// Mythos id must reach the fable row rather than fall through to Sonnet.
	for model, wantOut := range map[string]float64{
		"claude-opus-5":    25.00,
		"claude-opus-4-8":  25.00,
		"claude-fable-5":   50.00,
		"claude-mythos-5":  50.00,
		"claude-sonnet-5":  15.00,
		"claude-haiku-4-5": 5.00,
	} {
		if got := EstimatePricing(provider.Claude, model); got.OutputPerM != wantOut {
			t.Errorf("EstimatePricing(claude, %s).OutputPerM = %v, want %v", model, got.OutputPerM, wantOut)
		}
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

// TestOpus5FirstTurnEstimateIsBelowDefaultCeiling is the regression for the
// stale opus row. It is the exact usage block off the first turn of an Assay
// triage pass on Opus 5 — the whole prompt written to the cache in one go —
// which the Opus 4.1-era row ($18.75/M cache write) priced at $3.13 and so
// stopped against a $1.50 per-pass ceiling before the pass had read anything.
// At list price the turn is about $1.04.
func TestOpus5FirstTurnEstimateIsBelowDefaultCeiling(t *testing.T) {
	t.Cleanup(func() { SetPricingTable(nil) })
	SetPricingTable(nil)

	u := Usage{InputTokens: 2, CacheReadTokens: 19224, CacheWriteTokens: 165521}
	u.Calculate(EstimatePricing(provider.Claude, "claude-opus-5"))
	if u.EstimatedCostUSD < 1.00 || u.EstimatedCostUSD > 1.10 {
		t.Errorf("Opus 5 first-turn estimate = $%.4f, want about $1.04 (list price)", u.EstimatedCostUSD)
	}
	if u.EstimatedCostUSD >= 1.50 {
		t.Errorf("Opus 5 first-turn estimate $%.2f would trip the $1.50 default per-pass ceiling", u.EstimatedCostUSD)
	}
}
