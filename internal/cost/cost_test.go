package cost

import (
	"testing"

	"github.com/Robin831/Forge/internal/provider"
)

// TestRatesForModel pins the one model -> rates resolver the per-model report
// and the in-flight ceiling share: full ids, versioned ids and the CLI's bare
// aliases all reach their own row, and a name nothing matches reports false
// rather than a guess.
func TestRatesForModel(t *testing.T) {
	t.Cleanup(func() { SetPricingTable(nil) })
	SetPricingTable(nil)

	opus5 := Pricing{InputPerM: 5.00, OutputPerM: 25.00, CacheReadPerM: 0.50, CacheWritePerM: 6.25}
	opus55 := Pricing{InputPerM: 4.00, OutputPerM: 20.00, CacheReadPerM: 0.20, CacheWritePerM: 5.00}
	sonnet5 := Pricing{InputPerM: 2.00, OutputPerM: 10.00, CacheReadPerM: 0.20, CacheWritePerM: 2.50}
	sonnet4 := Pricing{InputPerM: 3.00, OutputPerM: 15.00, CacheReadPerM: 0.30, CacheWritePerM: 3.75}
	haiku45 := Pricing{InputPerM: 1.00, OutputPerM: 5.00, CacheReadPerM: 0.10, CacheWritePerM: 1.25}

	for _, tt := range []struct {
		model   string
		want    Pricing
		wantKey string
	}{
		{"claude-opus-5", opus5, ModelClaudeOpus},
		{"claude-opus-5-5", opus55, ModelClaudeOpus55},
		{"claude-opus-5.5", opus55, ModelClaudeOpus55}, // Copilot's dotted form
		{"claude-opus-5-5-20261001", opus55, ModelClaudeOpus55},
		// A date after the major version is not a minor version.
		{"claude-opus-5-20260901", opus5, ModelClaudeOpus},
		{"opus", opus5, ModelClaudeOpus},
		{" Opus ", opus5, ModelClaudeOpus},
		{"claude-opus-4-8", opus5, ModelClaudeOpus},
		{"claude-sonnet-5", sonnet5, ModelClaudeSonnet5},
		{"sonnet", sonnet5, ModelClaudeSonnet5},
		{"claude-sonnet-5-20260901", sonnet5, ModelClaudeSonnet5},
		{"claude-sonnet-4-6", sonnet4, ModelClaudeSonnet},
		{"claude-sonnet-4.5", sonnet4, ModelClaudeSonnet},
		// Legacy ids carry the version before the family and a DATE after
		// it; the date is not a Sonnet version.
		{"claude-3-7-sonnet-20250219", sonnet4, ModelClaudeSonnet},
		{"claude-3-5-sonnet-20241022", sonnet4, ModelClaudeSonnet},
		{"sonnet-20250219", sonnet4, ModelClaudeSonnet},
		{"claude-sonnet", sonnet4, ModelClaudeSonnet},
		{"claude-haiku-4-5-20251001", haiku45, ModelClaudeHaiku},
		{"claude-haiku-4-5", haiku45, ModelClaudeHaiku},
		{"haiku", haiku45, ModelClaudeHaiku},
		{"claude-fable-5", Pricing{10.00, 50.00, 1.00, 12.50}, ModelClaudeFable},
	} {
		got, key, ok := RatesForModel(tt.model)
		if !ok || got != tt.want || key != tt.wantKey {
			t.Errorf("RatesForModel(%q) = %+v, %q, %v; want %+v, %q, true", tt.model, got, key, ok, tt.want, tt.wantKey)
		}
	}
	for _, model := range []string{"", "  ", "gpt-5", "unknown"} {
		if _, key, ok := RatesForModel(model); ok {
			t.Errorf("RatesForModel(%q) resolved to %q, want no match", model, key)
		}
	}

	// An operator's settings.pricing entry for an exact id wins over the
	// family row it would otherwise infer.
	SetPricingTable(map[string]Pricing{"claude-opus-5": {InputPerM: 1, OutputPerM: 2, CacheReadPerM: 3, CacheWritePerM: 4}})
	if got, key, _ := RatesForModel("claude-opus-5"); got.CacheWritePerM != 4 || key != "claude-opus-5" {
		t.Errorf("exact override = %+v under %q, want the override row", got, key)
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
		{"haiku", lookupPricing(ModelClaudeHaiku), Pricing{1.00, 5.00, 0.10, 1.25}},
		{"opus", lookupPricing(ModelClaudeOpus), Pricing{5.00, 25.00, 0.50, 6.25}},
		{"fable", lookupPricing(ModelClaudeFable), Pricing{10.00, 50.00, 1.00, 12.50}},
		{"sonnet 4", lookupPricing(ModelClaudeSonnet), Pricing{3.00, 15.00, 0.30, 3.75}},
		{"sonnet 5", lookupPricing(ModelClaudeSonnet5), Pricing{2.00, 10.00, 0.20, 2.50}},
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
		"claude-opus-5-5":  20.00,
		"claude-opus-4-8":  25.00,
		"claude-fable-5":   50.00,
		"claude-mythos-5":  50.00,
		"claude-sonnet-5":  10.00,
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
