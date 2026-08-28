package hotreload

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

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

// previewAnvilConfig builds a config whose single anvil carries the three
// per-anvil preview keys, with previews enabled globally.
func previewAnvilConfig(enabled *bool, auto string, quests bool) *config.Config {
	return &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"munin": {Path: "/tmp/munin", PreviewEnabled: enabled, PreviewAuto: auto, PreviewQuests: quests},
		},
		Settings: config.SettingsConfig{PreviewEnabled: true},
	}
}

func boolPtr(b bool) *bool { return &b }

// TestApplyChanges_PerAnvilPreviewKeys is the bead's first acceptance criterion:
// the three per-anvil preview tri-states are resolved per request, so detecting
// them here is all it takes for the edit to apply without a restart.
func TestApplyChanges_PerAnvilPreviewKeys(t *testing.T) {
	cases := []struct {
		name string
		old  *config.Config
		new  *config.Config
		want string
	}{
		{
			name: "opt in from opted out",
			old:  previewAnvilConfig(boolPtr(false), "", false),
			new:  previewAnvilConfig(boolPtr(true), "", false),
			want: "anvil munin preview_enabled: false → true",
		},
		{
			name: "opt out from inherited",
			old:  previewAnvilConfig(nil, "", false),
			new:  previewAnvilConfig(boolPtr(false), "", false),
			want: "anvil munin preview_enabled: unset → false",
		},
		{
			name: "clearing an override back to inherit",
			old:  previewAnvilConfig(boolPtr(true), "", false),
			new:  previewAnvilConfig(nil, "", false),
			want: "anvil munin preview_enabled: true → unset",
		},
		{
			name: "automatic previews turned on",
			old:  previewAnvilConfig(nil, "", false),
			new:  previewAnvilConfig(nil, config.PreviewAutoReadyToMerge, false),
			want: `anvil munin preview_auto: "" → "ready_to_merge"`,
		},
		{
			name: "preview quests opted into",
			old:  previewAnvilConfig(nil, "", false),
			new:  previewAnvilConfig(nil, "", true),
			want: "anvil munin preview_quests: false → true",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changes := applyChanges(tc.old, tc.new)
			if !slices.Contains(changes, tc.want) {
				t.Errorf("expected %q among changes, got %v", tc.want, changes)
			}
			if back := applyChanges(tc.new, tc.new); len(back) != 0 {
				t.Errorf("identical config must not register a change, got %v", back)
			}
		})
	}
}

// TestRestartRequiredChanges covers the second acceptance criterion: a global
// preview setting the daemon reads once, at startup, is named in a warning
// rather than silently ignored.
func TestRestartRequiredChanges(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.SettingsConfig)
		want   string
	}{
		{"global gate", func(s *config.SettingsConfig) { s.PreviewEnabled = false }, "settings.preview_enabled: true → false"},
		{"port range", func(s *config.SettingsConfig) { s.PreviewPortRange = "31000-31999" }, "settings.preview_port_range: 24000-24999 → 31000-31999"},
		{"bind host", func(s *config.SettingsConfig) { s.PreviewBindHost = "0.0.0.0" }, "settings.preview_bind_host: 127.0.0.1 → 0.0.0.0"},
		{"public host", func(s *config.SettingsConfig) { s.PreviewPublicHost = "box.local" }, "settings.preview_public_host: 127.0.0.1 → box.local"},
		{"max concurrent", func(s *config.SettingsConfig) { s.PreviewMaxConcurrent = 5 }, "settings.preview_max_concurrent: 2 → 5"},
		{"evict lru", func(s *config.SettingsConfig) { s.PreviewEvictLRU = true }, "settings.preview_evict_lru: false → true"},
		{"idle timeout", func(s *config.SettingsConfig) { s.PreviewIdleTimeout = time.Hour }, "settings.preview_idle_timeout: 30m0s → 1h0m0s"},
	}
	base := config.SettingsConfig{
		PreviewEnabled:     true,
		PreviewPortRange:   config.DefaultPreviewPortRange,
		PreviewBindHost:    config.DefaultPreviewBindHost,
		PreviewIdleTimeout: config.DefaultPreviewIdleTimeout,
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := &config.Config{Settings: base}
			changed := base
			tc.mutate(&changed)
			new := &config.Config{Settings: changed}

			got := restartRequiredChanges(old, new)
			if !slices.Contains(got, tc.want) {
				t.Errorf("expected %q among restart-required keys, got %v", tc.want, got)
			}
			// The same edit must not be mistaken for something applied live.
			if ch := applyChanges(old, new); len(ch) != 0 {
				t.Errorf("restart-only key must not report as hot-reloaded, got %v", ch)
			}
			if same := restartRequiredChanges(old, old); len(same) != 0 {
				t.Errorf("identical config must not require a restart, got %v", same)
			}
		})
	}
}

// TestRestartRequiredChanges_PerAnvilKeysAreNotRestartOnly guards the boundary
// from the other side: the keys this bead made hot-reloadable must never turn up
// in the restart warning, or the operator is told to restart for nothing.
func TestRestartRequiredChanges_PerAnvilKeysAreNotRestartOnly(t *testing.T) {
	old := previewAnvilConfig(boolPtr(false), "", false)
	new := previewAnvilConfig(boolPtr(true), config.PreviewAutoReadyToMerge, true)

	if got := restartRequiredChanges(old, new); len(got) != 0 {
		t.Errorf("per-anvil preview keys are hot-reloadable, got restart-required %v", got)
	}
}

// TestReportIgnored covers the three outcomes of a reload: a named restart-only
// key, an untracked edit that moved nothing hot-reloadable (the generic line),
// and a reload that applied everything it saw (silence).
func TestReportIgnored(t *testing.T) {
	preview := func(enabled bool) *config.Config {
		return &config.Config{Settings: config.SettingsConfig{PreviewEnabled: enabled}}
	}
	smiths := func(n int) *config.Config {
		return &config.Config{Settings: config.SettingsConfig{MaxTotalSmiths: n}}
	}
	cases := []struct {
		name       string
		old        *config.Config
		new        *config.Config
		applied    int
		wantWarn   bool
		wantSubstr string
	}{
		{
			name:       "named restart-only key",
			old:        preview(false),
			new:        preview(true),
			wantWarn:   true,
			wantSubstr: "settings.preview_enabled: false → true",
		},
		{
			// A restart-only key this package does not know by name still has to
			// produce a signal, even though it cannot be named.
			name:       "untracked key changed alone",
			old:        &config.Config{Settings: config.SettingsConfig{MergeStrategy: "squash"}},
			new:        &config.Config{Settings: config.SettingsConfig{MergeStrategy: "merge"}},
			wantWarn:   true,
			wantSubstr: "daemon restart is required",
		},
		{
			name:     "hot-reloadable change applied",
			old:      smiths(2),
			new:      smiths(4),
			applied:  1,
			wantWarn: false,
		},
		{
			name:     "nothing changed at all",
			old:      preview(true),
			new:      preview(true),
			wantWarn: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			w := NewWatcher("forge.yaml", tc.old, logger)

			w.reportIgnored(tc.old, tc.new, tc.applied)

			out := buf.String()
			if got := strings.Contains(out, "level=WARN"); got != tc.wantWarn {
				t.Fatalf("warn logged = %v, want %v (log: %q)", got, tc.wantWarn, out)
			}
			if tc.wantSubstr != "" && !strings.Contains(out, tc.wantSubstr) {
				t.Errorf("expected warning to name %q, got %q", tc.wantSubstr, out)
			}
		})
	}
}

// The three warden knobs the smelter resolves through a closure at flush
// time. A setting absent from applyChanges is never swapped in, so the
// closure keeps reading the old value — silently: nothing errors and nothing
// logs, the edit just does nothing. That is the bug these entries fix, so
// deleting one must fail a test rather than pass a green suite.
func TestApplyChanges_WardenSmelterThresholds(t *testing.T) {
	cases := []struct {
		name string
		old  config.WardenSettings
		new  config.WardenSettings
		want string
	}{
		{
			name: "dedup_threshold",
			old:  config.WardenSettings{DedupThreshold: 0.6},
			new:  config.WardenSettings{DedupThreshold: 0.45},
			want: "warden.dedup_threshold: 0.60 → 0.45",
		},
		{
			name: "dedup_threshold disabled",
			old:  config.WardenSettings{DedupThreshold: 0.6},
			new:  config.WardenSettings{DedupThreshold: -1},
			want: "warden.dedup_threshold: 0.60 → -1.00",
		},
		{
			name: "overlap_threshold",
			old:  config.WardenSettings{OverlapThreshold: 0.55},
			new:  config.WardenSettings{OverlapThreshold: 0.7},
			want: "warden.overlap_threshold: 0.55 → 0.70",
		},
		{
			name: "archive_after_days",
			old:  config.WardenSettings{ArchiveAfterDays: 180},
			new:  config.WardenSettings{ArchiveAfterDays: 30},
			want: "warden.archive_after_days: 180 → 30",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := &config.Config{Settings: config.SettingsConfig{Warden: tc.old}}
			new := &config.Config{Settings: config.SettingsConfig{Warden: tc.new}}

			changes := applyChanges(old, new)

			if !slices.Contains(changes, tc.want) {
				t.Errorf("expected %q, got %v", tc.want, changes)
			}
		})
	}
}

// The comparison is on the RESOLVED values, so two spellings of one
// effective setting must not report a change: unset and an explicit default
// are the same number by the time the smelter's closure sees them, and a
// reload line claiming otherwise is a line an operator has to disprove.
func TestApplyChanges_WardenSmelterThresholdsUnchangedWhenResolvedEqual(t *testing.T) {
	old := &config.Config{Settings: config.SettingsConfig{Warden: config.WardenSettings{}}}
	new := &config.Config{Settings: config.SettingsConfig{Warden: config.WardenSettings{
		DedupThreshold:   config.DefaultWardenDedupThreshold,
		OverlapThreshold: config.DefaultWardenOverlapThreshold,
		ArchiveAfterDays: config.DefaultWardenArchiveAfterDays,
	}}}

	for _, c := range applyChanges(old, new) {
		if strings.HasPrefix(c, "warden.") {
			t.Errorf("unexpected change reported for a warden knob whose resolved value is unchanged: %s", c)
		}
	}
}
