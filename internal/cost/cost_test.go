package cost

import "testing"

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
