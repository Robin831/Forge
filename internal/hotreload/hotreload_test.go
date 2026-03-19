package hotreload

import (
	"slices"
	"testing"

	"github.com/Robin831/Forge/internal/config"
)

func TestApplyChanges_CopilotCombinedSmithWarden(t *testing.T) {
	old := &config.Config{Settings: config.SettingsConfig{CopilotCombinedSmithWarden: false}}
	new := &config.Config{Settings: config.SettingsConfig{CopilotCombinedSmithWarden: true}}

	changes := applyChanges(old, new)

	if !slices.Contains(changes, "copilot_combined_smith_warden: false → true") {
		t.Errorf("expected copilot_combined_smith_warden change, got %v", changes)
	}
}

func TestApplyChanges_CopilotWardenSampleRate(t *testing.T) {
	old := &config.Config{Settings: config.SettingsConfig{CopilotWardenSampleRate: 0.1}}
	new := &config.Config{Settings: config.SettingsConfig{CopilotWardenSampleRate: 0.25}}

	changes := applyChanges(old, new)

	if !slices.Contains(changes, "copilot_warden_sample_rate: 0.1 → 0.25") {
		t.Errorf("expected copilot_warden_sample_rate change, got %v", changes)
	}
}

func TestApplyChanges_CopilotSettingsUnchanged(t *testing.T) {
	cfg := &config.Config{Settings: config.SettingsConfig{
		CopilotCombinedSmithWarden: true,
		CopilotWardenSampleRate:    0.1,
	}}
	// Same config — no changes expected.
	changes := applyChanges(cfg, cfg)

	for _, c := range changes {
		if c == "copilot_combined_smith_warden: true → true" || c == "copilot_warden_sample_rate: 0.1 → 0.1" {
			t.Errorf("unexpected change reported for unchanged copilot setting: %s", c)
		}
	}
}
