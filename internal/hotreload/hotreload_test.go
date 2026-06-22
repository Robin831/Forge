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

func TestConfigEventIsRelevant(t *testing.T) {
	const base = "forge.yaml"
	cases := []struct {
		name      string
		eventName string
		want      bool
	}{
		{"direct edit of config file", "/etc/forge/forge.yaml", true},
		{"editor write-to-temp rename target", "/home/u/.forge/forge.yaml", true},
		// Kubernetes ConfigMap atomic update: kubelet renames ..data, never
		// touching forge.yaml. This is the production path and MUST trigger.
		{"configmap ..data symlink swap", "/etc/forge/..data", true},
		// The new timestamped dir is created too, but ..data is the marker we
		// key on; the timestamped dir alone should not trigger.
		{"configmap timestamped dir (ignored)", "/etc/forge/..2026_06_19_12_17_24.513680161", false},
		{"unrelated file in dir", "/etc/forge/bootstrap-anvils.sh", false},
		{"unrelated dotfile", "/etc/forge/.swp", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := configEventIsRelevant(tc.eventName, base); got != tc.want {
				t.Errorf("configEventIsRelevant(%q, %q) = %v, want %v", tc.eventName, base, got, tc.want)
			}
		})
	}
}
