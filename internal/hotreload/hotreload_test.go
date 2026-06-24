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

// TestApplyChanges_AssayConfig verifies that an assay-config edit (e.g. bumping
// daily_cost_limit_usd via the ConfigMap) is detected so reload() stores the new
// config instead of early-returning on "no changes". Without this, assay edits
// silently never hot-applied and required a pod restart.
func TestApplyChanges_AssayConfig(t *testing.T) {
	mk := func(limit float64) *config.Config {
		l := limit
		return &config.Config{Assay: config.AssayConfig{DailyCostLimitUSD: &l}}
	}
	if ch := applyChanges(mk(50), mk(150)); len(ch) == 0 {
		t.Error("assay daily_cost_limit change must be detected so reload() stores the new config")
	}
	if ch := applyChanges(mk(50), mk(50)); len(ch) != 0 {
		t.Errorf("identical assay config must not register a change, got %v", ch)
	}
}

// TestApplyChanges_PerAnvilAssayConfig verifies a per-anvil assay overlay edit
// is detected too.
func TestApplyChanges_PerAnvilAssayConfig(t *testing.T) {
	mk := func(shadow bool) *config.Config {
		s := shadow
		return &config.Config{Anvils: map[string]config.AnvilConfig{
			"munin": {Assay: &config.AssayConfig{ShadowMode: &s}},
		}}
	}
	if ch := applyChanges(mk(true), mk(false)); len(ch) == 0 {
		t.Error("per-anvil assay overlay change must be detected")
	}
}
