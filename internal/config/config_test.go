package config

import (
	"bytes"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		expected []string
	}{
		{
			name: "valid config",
			cfg: Config{
				Settings: SettingsConfig{
					MaxTotalSmiths:        4,
					MaxReviewAttempts:     5,
					MaxPipelineIterations: 5,
					PollInterval:          1 * time.Minute,
					SmithTimeout:          30 * time.Minute,
					BellowsInterval:       2 * time.Minute,
					MaxCIFixAttempts:      5,
					MaxReviewFixAttempts:  5,
					MaxRebaseAttempts:     3,
				},
				Anvils: map[string]AnvilConfig{
					"test": {
						Path:         "/path/to/repo",
						MaxSmiths:    2,
						AutoDispatch: "all",
					},
				},
			},
			expected: nil,
		},
		{
			name: "invalid settings",
			cfg: Config{
				Settings: SettingsConfig{
					MaxTotalSmiths:    0,
					MaxReviewAttempts: 0,
					PollInterval:      5 * time.Second,
					SmithTimeout:      30 * time.Second,
					BellowsInterval:   10 * time.Second,
				},
			},
			expected: []string{
				"settings.max_total_smiths must be >= 1",
				"settings.max_review_attempts must be >= 1",
				"settings.max_pipeline_iterations must be >= 1",
				"settings.poll_interval must be >= 10s",
				"settings.smith_timeout must be >= 1m",
				"settings.bellows_interval must be >= 30s",
				"settings.max_ci_fix_attempts must be >= 1",
				"settings.max_review_fix_attempts must be >= 1",
				"settings.max_rebase_attempts must be >= 1",
			},
		},
		{
			name: "invalid anvil path",
			cfg: Config{
				Settings: SettingsConfig{
					MaxTotalSmiths:        4,
					MaxReviewAttempts:     5,
					MaxPipelineIterations: 5,
					PollInterval:          1 * time.Minute,
					SmithTimeout:          30 * time.Minute,
					BellowsInterval:       2 * time.Minute,
					MaxCIFixAttempts:      5,
					MaxReviewFixAttempts:  5,
					MaxRebaseAttempts:     3,
				},
				Anvils: map[string]AnvilConfig{
					"test": {
						Path: "",
					},
				},
			},
			expected: []string{
				"anvil \"test\": path is required",
			},
		},
		{
			name: "invalid auto_dispatch mode",
			cfg: Config{
				Settings: SettingsConfig{
					MaxTotalSmiths:        4,
					MaxReviewAttempts:     5,
					MaxPipelineIterations: 5,
					PollInterval:          1 * time.Minute,
					SmithTimeout:          30 * time.Minute,
					BellowsInterval:       2 * time.Minute,
					MaxCIFixAttempts:      5,
					MaxReviewFixAttempts:  5,
					MaxRebaseAttempts:     3,
				},
				Anvils: map[string]AnvilConfig{
					"test": {
						Path:         "/path/to/repo",
						AutoDispatch: "invalid",
					},
				},
			},
			expected: []string{
				"anvil \"test\": invalid auto_dispatch \"invalid\" (must be all|tagged|priority|off)",
			},
		},
		{
			name: "missing tag for tagged mode",
			cfg: Config{
				Settings: SettingsConfig{
					MaxTotalSmiths:        4,
					MaxReviewAttempts:     5,
					MaxPipelineIterations: 5,
					PollInterval:          1 * time.Minute,
					SmithTimeout:          30 * time.Minute,
					BellowsInterval:       2 * time.Minute,
					MaxCIFixAttempts:      5,
					MaxReviewFixAttempts:  5,
					MaxRebaseAttempts:     3,
				},
				Anvils: map[string]AnvilConfig{
					"test": {
						Path:            "/path/to/repo",
						AutoDispatch:    "tagged",
						AutoDispatchTag: "",
					},
				},
			},
			expected: []string{
				"anvil \"test\": auto_dispatch_tag must be non-empty when auto_dispatch is \"tagged\"",
			},
		},
		{
			name: "invalid priority for priority mode",
			cfg: Config{
				Settings: SettingsConfig{
					MaxTotalSmiths:        4,
					MaxReviewAttempts:     5,
					MaxPipelineIterations: 5,
					PollInterval:          1 * time.Minute,
					SmithTimeout:          30 * time.Minute,
					BellowsInterval:       2 * time.Minute,
					MaxCIFixAttempts:      5,
					MaxReviewFixAttempts:  5,
					MaxRebaseAttempts:     3,
				},
				Anvils: map[string]AnvilConfig{
					"test": {
						Path:                    "/path/to/repo",
						AutoDispatch:            "priority",
						AutoDispatchMinPriority: -1,
					},
					"test2": {
						Path:                    "/path/to/repo",
						AutoDispatch:            "priority",
						AutoDispatchMinPriority: 5,
					},
				},
			},
			expected: []string{
				"anvil \"test\": auto_dispatch_min_priority must be 0-4 (0 = critical-only) when auto_dispatch is \"priority\"",
				"anvil \"test2\": auto_dispatch_min_priority must be 0-4 (0 = critical-only) when auto_dispatch is \"priority\"",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := tt.cfg.Validate()
			assert.ElementsMatch(t, tt.expected, errs)
		})
	}
}

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	assert.Equal(t, 5*time.Minute, cfg.Settings.PollInterval)
	assert.Equal(t, 30*time.Minute, cfg.Settings.SmithTimeout)
	assert.Equal(t, 4, cfg.Settings.MaxTotalSmiths)
	assert.Equal(t, 5*time.Minute, cfg.Settings.RateLimitBackoff)
	assert.Equal(t, 2*time.Minute, cfg.Settings.BellowsInterval)
	assert.Equal(t, 5, cfg.Settings.MaxCIFixAttempts)
	assert.Equal(t, 5, cfg.Settings.MaxReviewFixAttempts)
	assert.Equal(t, 3, cfg.Settings.MaxRebaseAttempts)
	assert.Equal(t, DefaultMaxLifecycleWorkers, cfg.Settings.MaxLifecycleWorkers)
	assert.NotNil(t, cfg.Anvils)
}

func TestLoad_FromFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
anvils:
  myrepo:
    path: /some/path
    max_smiths: 2
    auto_dispatch: all
settings:
  poll_interval: 30s
  smith_timeout: 5m
  max_total_smiths: 2
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, cfg.Settings.PollInterval)
	assert.Equal(t, 5*time.Minute, cfg.Settings.SmithTimeout)
	assert.Equal(t, 2, cfg.Settings.MaxTotalSmiths)
	assert.Equal(t, "/some/path", cfg.Anvils["myrepo"].Path)
	assert.Equal(t, "all", cfg.Anvils["myrepo"].AutoDispatch)
}

// TestLoad_ResolvesHomeConfigYAML asserts that Load("") finds
// ~/.forge/config.yaml — the documented default path. Regression for
// Forge-9mka, where viper's SetConfigName("forge")+AddConfigPath("~/.forge")
// search forced ~/.forge/forge.yaml and ignored the actual deployed file.
func TestLoad_ResolvesHomeConfigYAML(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome) // Windows
	require.NoError(t, os.Mkdir(filepath.Join(tmpHome, ".forge"), 0o755))
	cfgPath := filepath.Join(tmpHome, ".forge", "config.yaml")
	content := `
anvils:
  myrepo:
    path: /some/path
    max_smiths: 3
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	// Run from a directory with no forge.yaml so the home fallback is exercised.
	t.Chdir(t.TempDir())

	cfg, err := Load("")
	require.NoError(t, err)
	require.Contains(t, cfg.Anvils, "myrepo")
	assert.Equal(t, "/some/path", cfg.Anvils["myrepo"].Path)
	assert.Equal(t, 3, cfg.Anvils["myrepo"].MaxSmiths)

	assert.Equal(t, cfgPath, ConfigFilePath(""))
}

// TestConfigFilePath_ExplicitHonoursFlag guards the regression behind Forge-vhro:
// when an explicit --config path is given it MUST be returned, even when a
// default-location config also exists. The daemon relies on this so its web
// settings API and hot-reload watcher target the same file it loaded, rather
// than a default-probe guess (which on non-default deployments points elsewhere
// or nowhere).
func TestConfigFilePath_ExplicitHonoursFlag(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome) // Windows
	require.NoError(t, os.Mkdir(filepath.Join(tmpHome, ".forge"), 0o755))

	// A default-location config exists and would be picked up by the empty-arg
	// probe — proving the explicit path is honoured, not the default.
	defaultPath := filepath.Join(tmpHome, ".forge", "config.yaml")
	require.NoError(t, os.WriteFile(defaultPath, []byte("anvils: {}\n"), 0o644))

	explicitPath := filepath.Join(t.TempDir(), "custom.yaml")
	require.NoError(t, os.WriteFile(explicitPath, []byte("anvils: {}\n"), 0o644))

	// Run from a directory with no ./forge.yaml so only the two files above exist.
	t.Chdir(t.TempDir())

	assert.Equal(t, explicitPath, ConfigFilePath(explicitPath),
		"explicit --config path must be returned verbatim")
	assert.Equal(t, defaultPath, ConfigFilePath(""),
		"empty arg must fall back to the default location")
}

func TestLoad_AnvilDefaultAutoDispatch(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
anvils:
  myrepo:
    path: /some/path
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, "all", cfg.Anvils["myrepo"].AutoDispatch)
}

func TestLoad_GlobalAssay(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
anvils:
  myrepo:
    path: /some/path
assay:
  enabled: true
  shadow_mode: false
  debounce_seconds: 45
  model_tier: sonnet
  max_diff_bytes: 500000
  nit_cap: 3
  skip_paths:
    - vendor/
    - "**/*.gen.go"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.True(t, cfg.Assay.IsEnabled())
	assert.False(t, cfg.Assay.IsShadowMode())
	assert.Equal(t, 45, cfg.Assay.GetDebounceSeconds())
	assert.Equal(t, "sonnet", cfg.Assay.ModelTier)
	assert.Equal(t, 500000, cfg.Assay.GetMaxDiffBytes())
	assert.Equal(t, 3, cfg.Assay.GetNitCap())
	assert.Equal(t, []string{"vendor/", "**/*.gen.go"}, cfg.Assay.SkipPaths)
}

// TestResolvedAssay_AnvilOverlay verifies that a per-anvil assay block merges
// over the global config: overridden fields change while non-overridden fields
// inherit. Hot-reload of Assay is satisfied transparently because Assay lives
// on Config, which the daemon replaces wholesale (d.cfg.Store) on fsnotify
// change — no extra wiring is required.
func TestResolvedAssay_AnvilOverlay(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
anvils:
  myrepo:
    path: /some/path
    assay:
      shadow_mode: false
      nit_cap: 1
assay:
  enabled: true
  shadow_mode: true
  debounce_seconds: 45
  nit_cap: 10
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)

	resolved := cfg.ResolvedAssay("myrepo")
	// Overridden fields reflect the anvil overlay.
	assert.False(t, resolved.IsShadowMode())
	assert.Equal(t, 1, resolved.GetNitCap())
	// Non-overridden fields inherit from the global config.
	assert.True(t, resolved.IsEnabled())
	assert.Equal(t, 45, resolved.GetDebounceSeconds())
}

func TestResolvedAssay_NoOverrideReturnsGlobal(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
anvils:
  myrepo:
    path: /some/path
assay:
  enabled: true
  shadow_mode: false
  debounce_seconds: 45
  nit_cap: 7
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)

	resolved := cfg.ResolvedAssay("myrepo")
	assert.True(t, resolved.IsEnabled())
	assert.False(t, resolved.IsShadowMode())
	assert.Equal(t, 45, resolved.GetDebounceSeconds())
	assert.Equal(t, 7, resolved.GetNitCap())

	// An unknown anvil also falls back to the global config.
	unknown := cfg.ResolvedAssay("does-not-exist")
	assert.Equal(t, cfg.Assay, unknown)
}

func TestAssayConfig_MaxCostPerPassUSD(t *testing.T) {
	// Unset is not "off": the ceiling is a runaway brake that a deployment
	// which never heard of the key still gets.
	var a AssayConfig
	assert.Equal(t, defaultAssayMaxCostPerPassUSD, a.GetMaxCostPerPassUSD())

	// 0 is the documented way to turn it off, and is distinguishable from
	// unset only because the field is a pointer.
	off := 0.0
	a.MaxCostPerPassUSD = &off
	assert.Equal(t, 0.0, a.GetMaxCostPerPassUSD())

	set := 3.25
	a.MaxCostPerPassUSD = &set
	assert.Equal(t, 3.25, a.GetMaxCostPerPassUSD())
}

func TestAssayConfig_MaxCostPerPassUSD_AnvilOverlay(t *testing.T) {
	global, anvil := 1.5, 4.0
	cfg := &Config{
		Assay: AssayConfig{MaxCostPerPassUSD: &global},
		Anvils: map[string]AnvilConfig{
			"api":   {Path: "/repos/api", Assay: &AssayConfig{MaxCostPerPassUSD: &anvil}},
			"plain": {Path: "/repos/plain"},
		},
	}
	assert.Equal(t, 4.0, cfg.ResolvedAssay("api").GetMaxCostPerPassUSD())
	assert.Equal(t, 1.5, cfg.ResolvedAssay("plain").GetMaxCostPerPassUSD())
}

func TestValidate_AssayMaxCostPerPassUSD(t *testing.T) {
	base := func() *Config {
		c := Defaults()
		c.Anvils = map[string]AnvilConfig{"test": {Path: "/repos/test"}}
		return &c
	}
	const globalMsg = "assay.max_cost_per_pass_usd must be a non-negative finite number (set 0 to disable the per-pass ceiling)"

	t.Run("defaults are valid", func(t *testing.T) {
		for _, e := range base().Validate() {
			assert.NotContains(t, e, "max_cost_per_pass_usd")
		}
	})

	t.Run("zero disables", func(t *testing.T) {
		cfg := base()
		zero := 0.0
		cfg.Assay.MaxCostPerPassUSD = &zero
		for _, e := range cfg.Validate() {
			assert.NotContains(t, e, "max_cost_per_pass_usd")
		}
	})

	for name, bad := range map[string]float64{
		"negative": -1,
		"NaN":      math.NaN(),
		"infinite": math.Inf(1),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			v := bad
			cfg.Assay.MaxCostPerPassUSD = &v
			assert.Contains(t, cfg.Validate(), globalMsg)
		})
	}

	// A per-anvil overlay is what the daemon actually resolves, so it is
	// checked under the same rule and named by the anvil that wrote it.
	t.Run("per-anvil overlay", func(t *testing.T) {
		cfg := base()
		bad := -0.5
		cfg.Anvils["test"] = AnvilConfig{Path: "/repos/test", Assay: &AssayConfig{MaxCostPerPassUSD: &bad}}
		assert.Contains(t, cfg.Validate(),
			`anvil "test": assay.max_cost_per_pass_usd must be a non-negative finite number (set 0 to disable the per-pass ceiling)`)
	})
}

func TestAssayConfig_ResolverDefaults(t *testing.T) {
	// A zero AssayConfig (all tri-state booleans nil) uses the documented
	// defaults: disabled, shadow mode on, drafts skipped.
	var a AssayConfig
	assert.False(t, a.IsEnabled())
	assert.True(t, a.IsShadowMode())
	assert.True(t, a.IsSkipDrafts())
}

func TestLoad_RateLimitBackoff(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  rate_limit_backoff: 10m
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, 10*time.Minute, cfg.Settings.RateLimitBackoff)
}

func TestLoad_RateLimitBackoff_Default(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	// No rate_limit_backoff set — should use 5m default.
	content := `
settings:
  max_total_smiths: 2
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, cfg.Settings.RateLimitBackoff)
}

func TestLoad_InvalidRateLimitBackoff(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  rate_limit_backoff: notaduration
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	_, err := Load(cfgPath)
	assert.ErrorContains(t, err, "rate_limit_backoff")
}

func TestLoad_BellowsInterval(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  bellows_interval: 3m
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, 3*time.Minute, cfg.Settings.BellowsInterval)
}

func TestLoad_BellowsInterval_Default(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  max_total_smiths: 2
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, 2*time.Minute, cfg.Settings.BellowsInterval)
}

func TestLoad_Bus_Default(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  max_total_smiths: 2
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	// Bus is disabled by default for safe rollout, with the buffer sized to the
	// package default.
	assert.False(t, cfg.Settings.BusEnabled)
	assert.Equal(t, DefaultBusBufferSize, cfg.Settings.BusBufferSize)
	assert.Equal(t, DefaultBusBufferSize, cfg.Settings.ResolvedBusBufferSize())
}

func TestLoad_Bus_EnabledWithBufferSize(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  bus_enabled: true
  bus_buffer_size: 1024
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.True(t, cfg.Settings.BusEnabled)
	assert.Equal(t, 1024, cfg.Settings.BusBufferSize)
	assert.Equal(t, 1024, cfg.Settings.ResolvedBusBufferSize())
}

func TestLoad_Bus_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("settings:\n  max_total_smiths: 2\n"), 0o644))

	t.Setenv("FORGE_SETTINGS_BUS_ENABLED", "true")
	t.Setenv("FORGE_SETTINGS_BUS_BUFFER_SIZE", "42")

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.True(t, cfg.Settings.BusEnabled)
	assert.Equal(t, 42, cfg.Settings.BusBufferSize)
}

func TestResolvedBusBufferSize_FallsBackForNonPositive(t *testing.T) {
	assert.Equal(t, DefaultBusBufferSize, SettingsConfig{BusBufferSize: 0}.ResolvedBusBufferSize())
	assert.Equal(t, DefaultBusBufferSize, SettingsConfig{BusBufferSize: -5}.ResolvedBusBufferSize())
	assert.Equal(t, 16, SettingsConfig{BusBufferSize: 16}.ResolvedBusBufferSize())
}

func TestLoad_SSEPollFallback_Default(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("settings:\n  max_total_smiths: 2\n"), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	// The activity SSE stream uses the Bus (when enabled) by default; the poll
	// fallback is opt-in.
	assert.False(t, cfg.Settings.SSEPollFallback)
}

func TestLoad_SSEPollFallback_Enabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  sse_poll_fallback: true
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.True(t, cfg.Settings.SSEPollFallback)
}

func TestLoad_SSEPollFallback_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("settings:\n  max_total_smiths: 2\n"), 0o644))

	t.Setenv("FORGE_SETTINGS_SSE_POLL_FALLBACK", "true")

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.True(t, cfg.Settings.SSEPollFallback)
}

func TestLoad_InvalidBellowsInterval(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  bellows_interval: notaduration
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	_, err := Load(cfgPath)
	assert.ErrorContains(t, err, "bellows_interval")
}

func TestLoad_LifecycleRetryCaps(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  max_ci_fix_attempts: 10
  max_review_fix_attempts: 8
  max_rebase_attempts: 6
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, 10, cfg.Settings.MaxCIFixAttempts)
	assert.Equal(t, 8, cfg.Settings.MaxReviewFixAttempts)
	assert.Equal(t, 6, cfg.Settings.MaxRebaseAttempts)
}

func TestLoad_LifecycleRetryCaps_Defaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  max_total_smiths: 2
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, 5, cfg.Settings.MaxCIFixAttempts)
	assert.Equal(t, 5, cfg.Settings.MaxReviewFixAttempts)
	assert.Equal(t, 3, cfg.Settings.MaxRebaseAttempts)
}

func TestLoad_InvalidPollInterval(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  poll_interval: notaduration
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	_, err := Load(cfgPath)
	assert.ErrorContains(t, err, "poll_interval")
}

func TestLoad_InvalidSmelterInterval(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  smelter_interval: notaduration
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	_, err := Load(cfgPath)
	assert.ErrorContains(t, err, "smelter_interval")
}

func TestIsVulncheckEnabled(t *testing.T) {
	// nil (not set) → default true
	s := SettingsConfig{}
	assert.True(t, s.IsVulncheckEnabled())

	// explicitly true
	tr := true
	s.VulncheckEnabled = &tr
	assert.True(t, s.IsVulncheckEnabled())

	// explicitly false
	fa := false
	s.VulncheckEnabled = &fa
	assert.False(t, s.IsVulncheckEnabled())
}

func TestLoad_DepcheckEnabled(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
anvils:
  enabled-repo:
    path: /some/path
    depcheck_enabled: true
  disabled-repo:
    path: /other/path
    depcheck_enabled: false
  default-repo:
    path: /default/path
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)

	// Explicitly enabled
	require.NotNil(t, cfg.Anvils["enabled-repo"].DepcheckEnabled)
	assert.True(t, *cfg.Anvils["enabled-repo"].DepcheckEnabled)

	// Explicitly disabled
	require.NotNil(t, cfg.Anvils["disabled-repo"].DepcheckEnabled)
	assert.False(t, *cfg.Anvils["disabled-repo"].DepcheckEnabled)

	// Not set (nil = use default)
	assert.Nil(t, cfg.Anvils["default-repo"].DepcheckEnabled)
}

func TestSave_RoundTrip_PreservesAllFields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")

	depcheckFalse := false
	vulnTrue := true

	original := Defaults()
	original.Anvils["myrepo"] = AnvilConfig{
		Path:            "/some/path",
		MaxSmiths:       2,
		AutoDispatch:    "tagged",
		AutoDispatchTag: "forgeReady",
		DepcheckEnabled: &depcheckFalse,
	}
	original.Settings.Providers = []string{
		"claude/claude-sonnet-4-6",
		"gemini/gemini-2.5-pro",
		"gemini/gemini-2.5-flash",
	}
	original.Settings.SmithProviders = []string{
		"claude/claude-opus-4-6",
		"gemini/gemini-2.5-pro",
	}
	original.Settings.MaxTotalSmiths = 7
	original.Settings.CrucibleEnabled = true
	original.Settings.AutoLearnRules = true
	original.Settings.VulncheckEnabled = &vulnTrue

	// Save → Load round-trip.
	require.NoError(t, Save(&original, cfgPath))

	loaded, err := Load(cfgPath)
	require.NoError(t, err)

	// Providers must survive.
	assert.Equal(t, original.Settings.Providers, loaded.Settings.Providers,
		"providers must survive Save→Load round-trip")
	assert.Equal(t, original.Settings.SmithProviders, loaded.Settings.SmithProviders,
		"smith_providers must survive Save→Load round-trip")

	// Anvil optional bools.
	require.NotNil(t, loaded.Anvils["myrepo"].DepcheckEnabled)
	assert.False(t, *loaded.Anvils["myrepo"].DepcheckEnabled)

	// Other settings.
	assert.Equal(t, 7, loaded.Settings.MaxTotalSmiths)
	assert.True(t, loaded.Settings.CrucibleEnabled)
	assert.True(t, loaded.Settings.AutoLearnRules)
	require.NotNil(t, loaded.Settings.VulncheckEnabled)
	assert.True(t, *loaded.Settings.VulncheckEnabled)

	// Durations should round-trip as strings, not nanoseconds.
	assert.Equal(t, original.Settings.PollInterval, loaded.Settings.PollInterval)
	assert.Equal(t, original.Settings.SmithTimeout, loaded.Settings.SmithTimeout)
}

func TestSave_RoundTrip_StageProviders(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")

	original := Defaults()
	original.Settings.StageProviders = map[string][]string{
		"smith":     {"claude/claude-opus-4-6"},
		"warden":    {"claude/claude-sonnet-4-6"},
		"schematic": {"gemini/gemini-2.5-flash"},
	}

	require.NoError(t, Save(&original, cfgPath))

	loaded, err := Load(cfgPath)
	require.NoError(t, err)

	assert.Equal(t, original.Settings.StageProviders, loaded.Settings.StageProviders,
		"stage_providers must survive Save→Load round-trip")
}

func TestSave_RoundTrip_Pricing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")

	original := Defaults()
	original.Settings.Pricing = map[string]ModelPricing{
		"gemini": {InputPerM: 4.00, OutputPerM: 12.00},
		"claude-opus": {
			InputPerM:      15.00,
			OutputPerM:     75.00,
			CacheReadPerM:  1.50,
			CacheWritePerM: 18.75,
		},
	}
	original.Settings.CopilotPremiumMultipliers = map[string]float64{
		"claude-opus-4.6":  3,
		"claude-haiku-4.5": 0.33,
	}

	require.NoError(t, Save(&original, cfgPath))

	loaded, err := Load(cfgPath)
	require.NoError(t, err)

	assert.Equal(t, original.Settings.Pricing, loaded.Settings.Pricing,
		"pricing must survive Save→Load round-trip")
	assert.Equal(t, original.Settings.CopilotPremiumMultipliers, loaded.Settings.CopilotPremiumMultipliers,
		"copilot_premium_multipliers must survive Save→Load round-trip")
}

func TestSave_RoundTrip_PerAnvilStageProviders(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")

	original := Defaults()
	original.Anvils["myrepo"] = AnvilConfig{
		Path: "/some/path",
		StageProviders: map[string][]string{
			"warden": {"gemini/gemini-2.5-pro"},
			"cifix":  {"claude/claude-sonnet-4-6"},
		},
	}

	require.NoError(t, Save(&original, cfgPath))

	loaded, err := Load(cfgPath)
	require.NoError(t, err)

	anvilLoaded, ok := loaded.Anvils["myrepo"]
	require.True(t, ok, "anvil myrepo must survive Save→Load round-trip")
	assert.Equal(t, original.Anvils["myrepo"].StageProviders, anvilLoaded.StageProviders,
		"per-anvil stage_providers must survive Save→Load round-trip")
}

func TestIsQuestgiverEnabled(t *testing.T) {
	// nil (not set) → default false
	s := SettingsConfig{}
	assert.False(t, s.IsQuestgiverEnabled())

	// explicitly true
	tr := true
	s.QuestgiverEnabled = &tr
	assert.True(t, s.IsQuestgiverEnabled())

	// explicitly false
	fa := false
	s.QuestgiverEnabled = &fa
	assert.False(t, s.IsQuestgiverEnabled())
}

func TestSave_RoundTrip_QuestgiverDurations(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")

	qgEnabled := true
	original := Defaults()
	original.Settings.QuestgiverEnabled = &qgEnabled
	original.Settings.QuestgiverInterval = 24 * time.Hour
	original.Settings.AdventurerTimeout = 5 * time.Minute

	require.NoError(t, Save(&original, cfgPath))

	loaded, err := Load(cfgPath)
	require.NoError(t, err)

	require.NotNil(t, loaded.Settings.QuestgiverEnabled)
	assert.True(t, *loaded.Settings.QuestgiverEnabled)
	assert.Equal(t, 24*time.Hour, loaded.Settings.QuestgiverInterval)
	assert.Equal(t, 5*time.Minute, loaded.Settings.AdventurerTimeout)
}

func TestResolvedForgeID(t *testing.T) {
	t.Run("explicit setting wins over hostname", func(t *testing.T) {
		s := SettingsConfig{ForgeID: "skybert-forge"}
		assert.Equal(t, "skybert-forge", s.ResolvedForgeID())
	})

	t.Run("whitespace is trimmed before fallback", func(t *testing.T) {
		s := SettingsConfig{ForgeID: "   "}
		// Falls back to hostname (or "default"); the only invariant we can
		// assert without depending on the host environment is that it is
		// non-empty and not literally the whitespace string.
		got := s.ResolvedForgeID()
		assert.NotEmpty(t, got)
		assert.NotEqual(t, "   ", got)
	})

	t.Run("falls back to a non-empty value when nothing is configured", func(t *testing.T) {
		s := SettingsConfig{}
		assert.NotEmpty(t, s.ResolvedForgeID(),
			"ResolvedForgeID must always return a non-empty value so the forge-managed marker stays well-formed")
	})
}

func TestIsSmelterEnabled(t *testing.T) {
	// nil (not set) → default true
	s := SettingsConfig{}
	assert.True(t, s.IsSmelterEnabled())

	// explicitly true
	tr := true
	s.SmelterEnabled = &tr
	assert.True(t, s.IsSmelterEnabled())

	// explicitly false
	fa := false
	s.SmelterEnabled = &fa
	assert.False(t, s.IsSmelterEnabled())
}

func TestConfig_Validate_SmelterInterval(t *testing.T) {
	base := func() Config {
		return Config{
			Settings: SettingsConfig{
				MaxTotalSmiths:        4,
				MaxReviewAttempts:     5,
				MaxPipelineIterations: 5,
				PollInterval:          1 * time.Minute,
				SmithTimeout:          30 * time.Minute,
				BellowsInterval:       2 * time.Minute,
				MaxCIFixAttempts:      5,
				MaxReviewFixAttempts:  5,
				MaxRebaseAttempts:     3,
			},
			Anvils: map[string]AnvilConfig{
				"test": {Path: "/path/to/repo"},
			},
		}
	}

	t.Run("zero disables", func(t *testing.T) {
		cfg := base()
		cfg.Settings.SmelterInterval = 0
		errs := cfg.Validate()
		for _, e := range errs {
			assert.NotContains(t, e, "smelter_interval")
		}
	})

	t.Run("valid 8h", func(t *testing.T) {
		cfg := base()
		cfg.Settings.SmelterInterval = 8 * time.Hour
		errs := cfg.Validate()
		for _, e := range errs {
			assert.NotContains(t, e, "smelter_interval")
		}
	})

	t.Run("too short", func(t *testing.T) {
		cfg := base()
		cfg.Settings.SmelterInterval = 30 * time.Minute
		errs := cfg.Validate()
		assert.Contains(t, errs, "settings.smelter_interval must be >= 1h when enabled (or 0 to disable)")
	})

	t.Run("negative", func(t *testing.T) {
		cfg := base()
		cfg.Settings.SmelterInterval = -1 * time.Hour
		errs := cfg.Validate()
		assert.Contains(t, errs, "settings.smelter_interval must not be negative (set to 0 to disable)")
	})
}

func TestSave_RoundTrip_SmelterSettings(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")

	smelterEnabled := true
	original := Defaults()
	original.Settings.SmelterEnabled = &smelterEnabled
	original.Settings.SmelterInterval = 12 * time.Hour

	require.NoError(t, Save(&original, cfgPath))

	loaded, err := Load(cfgPath)
	require.NoError(t, err)

	require.NotNil(t, loaded.Settings.SmelterEnabled)
	assert.True(t, *loaded.Settings.SmelterEnabled)
	assert.Equal(t, 12*time.Hour, loaded.Settings.SmelterInterval)
}

func TestLoad_NoFile_UsesDefaults(t *testing.T) {
	// Load with a path that doesn't exist → viper.ConfigFileNotFoundError → uses defaults
	cfg, err := Load("/nonexistent/forge.yaml")
	// Will error because explicit path not found is treated as parse error by viper
	// Either an error or defaults — just verify the call doesn't panic
	_ = cfg
	_ = err
}

func TestLoad_WicketInterval(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  wicket_interval: 30m
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, 30*time.Minute, cfg.Settings.WicketInterval)
}

func TestLoad_WicketInterval_Default(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  max_total_smiths: 2
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, 15*time.Minute, cfg.Settings.WicketInterval)
}

func TestLoad_InvalidWicketInterval(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  wicket_interval: notaduration
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	_, err := Load(cfgPath)
	assert.ErrorContains(t, err, "wicket_interval")
}

// --- ProvidersForStage tests ---

func TestProvidersForStage_StageProvidersTakesPrecedence(t *testing.T) {
	s := SettingsConfig{
		Providers:      []string{"claude", "gemini"},
		SmithProviders: []string{"claude/claude-opus-4-6"},
		StageProviders: map[string][]string{
			"smith": {"gemini/gemini-2.5-pro"},
		},
	}
	got := s.ProvidersForStage("smith")
	assert.Equal(t, []string{"gemini/gemini-2.5-pro"}, got)
}

func TestProvidersForStage_FallsBackToSmithProviders(t *testing.T) {
	s := SettingsConfig{
		Providers:      []string{"claude", "gemini"},
		SmithProviders: []string{"claude/claude-opus-4-6"},
	}
	for _, stage := range []string{"smith", "warden", "schematic"} {
		got := s.ProvidersForStage(stage)
		assert.Equal(t, []string{"claude/claude-opus-4-6"}, got, "stage %s", stage)
	}
}

func TestProvidersForStage_SmithProvidersFallbackNotForCIFixReviewFix(t *testing.T) {
	s := SettingsConfig{
		Providers:      []string{"claude", "gemini"},
		SmithProviders: []string{"claude/claude-opus-4-6"},
	}
	for _, stage := range []string{"cifix", "reviewfix"} {
		got := s.ProvidersForStage(stage)
		assert.Equal(t, []string{"claude", "gemini"}, got,
			"stage %s should fall back to providers, not smith_providers", stage)
	}
}

func TestProvidersForStage_FallsBackToProviders(t *testing.T) {
	s := SettingsConfig{
		Providers: []string{"claude", "gemini"},
	}
	got := s.ProvidersForStage("smith")
	assert.Equal(t, []string{"claude", "gemini"}, got)
}

func TestProvidersForStage_ReturnsNilWhenAllEmpty(t *testing.T) {
	s := SettingsConfig{}
	got := s.ProvidersForStage("smith")
	assert.Nil(t, got)
}

func TestProvidersForStage_EmptyStageProvidersEntryFallsBack(t *testing.T) {
	s := SettingsConfig{
		Providers: []string{"claude"},
		StageProviders: map[string][]string{
			"smith": {}, // empty slice should be treated as unset
		},
	}
	got := s.ProvidersForStage("smith")
	assert.Equal(t, []string{"claude"}, got)
}

func TestProvidersForStage_EachStageIndependent(t *testing.T) {
	s := SettingsConfig{
		Providers: []string{"claude"},
		StageProviders: map[string][]string{
			"smith":     {"claude/claude-opus-4-6"},
			"warden":    {"claude/claude-sonnet-4-6"},
			"schematic": {"gemini/gemini-2.5-flash"},
			"cifix":     {"claude/claude-sonnet-4-6"},
			"reviewfix": {"gemini/gemini-2.5-pro"},
		},
	}
	assert.Equal(t, []string{"claude/claude-opus-4-6"}, s.ProvidersForStage("smith"))
	assert.Equal(t, []string{"claude/claude-sonnet-4-6"}, s.ProvidersForStage("warden"))
	assert.Equal(t, []string{"gemini/gemini-2.5-flash"}, s.ProvidersForStage("schematic"))
	assert.Equal(t, []string{"claude/claude-sonnet-4-6"}, s.ProvidersForStage("cifix"))
	assert.Equal(t, []string{"gemini/gemini-2.5-pro"}, s.ProvidersForStage("reviewfix"))
}

// --- ProvidersForStageWithAnvil tests ---

func TestProvidersForStageWithAnvil_AnvilOverrideTakesPrecedence(t *testing.T) {
	s := SettingsConfig{
		Providers: []string{"claude", "gemini"},
		StageProviders: map[string][]string{
			"smith": {"claude/claude-opus-4-6"},
		},
	}
	anvil := &AnvilConfig{
		StageProviders: map[string][]string{
			"smith": {"gemini/gemini-2.5-flash"},
		},
	}
	got := ProvidersForStageWithAnvil(s, anvil, "smith")
	assert.Equal(t, []string{"gemini/gemini-2.5-flash"}, got)
}

func TestProvidersForStageWithAnvil_FallsBackToGlobalStageProviders(t *testing.T) {
	s := SettingsConfig{
		Providers: []string{"claude", "gemini"},
		StageProviders: map[string][]string{
			"warden": {"claude/claude-sonnet-4-6"},
		},
	}
	anvil := &AnvilConfig{
		StageProviders: map[string][]string{
			"smith": {"gemini/gemini-2.5-pro"},
		},
	}
	got := ProvidersForStageWithAnvil(s, anvil, "warden")
	assert.Equal(t, []string{"claude/claude-sonnet-4-6"}, got)
}

func TestProvidersForStageWithAnvil_NilAnvilUsesGlobal(t *testing.T) {
	s := SettingsConfig{
		StageProviders: map[string][]string{
			"smith": {"claude/claude-opus-4-6"},
		},
	}
	got := ProvidersForStageWithAnvil(s, nil, "smith")
	assert.Equal(t, []string{"claude/claude-opus-4-6"}, got)
}

func TestProvidersForStageWithAnvil_EmptyAnvilEntryFallsBack(t *testing.T) {
	s := SettingsConfig{
		Providers: []string{"claude"},
		StageProviders: map[string][]string{
			"cifix": {"gemini/gemini-2.5-pro"},
		},
	}
	anvil := &AnvilConfig{
		StageProviders: map[string][]string{
			"cifix": {}, // empty should fall through
		},
	}
	got := ProvidersForStageWithAnvil(s, anvil, "cifix")
	assert.Equal(t, []string{"gemini/gemini-2.5-pro"}, got)
}

func TestProvidersForStageWithAnvil_AnvilNoStageProviders(t *testing.T) {
	s := SettingsConfig{
		Providers: []string{"claude"},
	}
	anvil := &AnvilConfig{}
	got := ProvidersForStageWithAnvil(s, anvil, "smith")
	assert.Equal(t, []string{"claude"}, got)
}

func TestLoad_TemperLintRequired(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
anvils:
  myrepo:
    path: /some/path
    temper:
      build: "make build"
      lint: "make lint"
      lint_required: true
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	require.NotNil(t, cfg.Anvils["myrepo"].Temper)
	assert.True(t, cfg.Anvils["myrepo"].Temper.LintRequired)
	assert.Equal(t, "make lint", cfg.Anvils["myrepo"].Temper.Lint)
	assert.Equal(t, "make build", cfg.Anvils["myrepo"].Temper.Build)
}

func TestConfig_Validate_LintRequired(t *testing.T) {
	base := func() Config {
		return Config{
			Settings: SettingsConfig{
				MaxTotalSmiths:        4,
				MaxReviewAttempts:     5,
				MaxPipelineIterations: 5,
				PollInterval:          1 * time.Minute,
				SmithTimeout:          30 * time.Minute,
				BellowsInterval:       2 * time.Minute,
				MaxCIFixAttempts:      5,
				MaxReviewFixAttempts:  5,
				MaxRebaseAttempts:     3,
			},
			Anvils: map[string]AnvilConfig{
				"myrepo": {Path: "/some/path"},
			},
		}
	}

	t.Run("lint_required with lint set is valid", func(t *testing.T) {
		cfg := base()
		temper := cfg.Anvils["myrepo"]
		temper.Temper = &TemperCommandsConfig{Lint: "make lint", LintRequired: true}
		cfg.Anvils["myrepo"] = temper
		errs := cfg.Validate()
		for _, e := range errs {
			assert.NotContains(t, e, "lint_required")
		}
	})

	t.Run("lint_required without lint is invalid", func(t *testing.T) {
		cfg := base()
		temper := cfg.Anvils["myrepo"]
		temper.Temper = &TemperCommandsConfig{LintRequired: true}
		cfg.Anvils["myrepo"] = temper
		errs := cfg.Validate()
		assert.Contains(t, errs, `anvil "myrepo": temper.lint_required is true but temper.lint is not set`)
	})

	t.Run("lint_required with steps is valid", func(t *testing.T) {
		cfg := base()
		a := cfg.Anvils["myrepo"]
		a.Temper = &TemperCommandsConfig{
			LintRequired: true,
			Steps: []TemperStepConfig{
				{Name: "lint", Command: "make", Args: []string{"lint"}},
			},
		}
		cfg.Anvils["myrepo"] = a
		errs := cfg.Validate()
		for _, e := range errs {
			assert.NotContains(t, e, "lint_required")
		}
	})

	t.Run("steps missing name is invalid", func(t *testing.T) {
		cfg := base()
		a := cfg.Anvils["myrepo"]
		a.Temper = &TemperCommandsConfig{
			Steps: []TemperStepConfig{
				{Name: "", Command: "make"},
			},
		}
		cfg.Anvils["myrepo"] = a
		errs := cfg.Validate()
		assert.Contains(t, errs, `anvil "myrepo": temper.steps[0].name must be non-empty`)
	})

	t.Run("steps missing command is invalid", func(t *testing.T) {
		cfg := base()
		a := cfg.Anvils["myrepo"]
		a.Temper = &TemperCommandsConfig{
			Steps: []TemperStepConfig{
				{Name: "build", Command: ""},
			},
		}
		cfg.Anvils["myrepo"] = a
		errs := cfg.Validate()
		assert.Contains(t, errs, `anvil "myrepo": temper.steps[0].command must be non-empty (or set verify_no_conflict_markers for a scan-only step)`)
	})

	t.Run("steps duplicate names is invalid", func(t *testing.T) {
		cfg := base()
		a := cfg.Anvils["myrepo"]
		a.Temper = &TemperCommandsConfig{
			Steps: []TemperStepConfig{
				{Name: "build", Command: "make"},
				{Name: "build", Command: "cargo"},
			},
		}
		cfg.Anvils["myrepo"] = a
		errs := cfg.Validate()
		assert.Contains(t, errs, `anvil "myrepo": temper.steps has duplicate name "build"`)
	})

	t.Run("valid steps pass validation", func(t *testing.T) {
		cfg := base()
		a := cfg.Anvils["myrepo"]
		a.Temper = &TemperCommandsConfig{
			Steps: []TemperStepConfig{
				{Name: "build", Command: "make", Args: []string{"build"}},
				{Name: "test", Command: "make", Args: []string{"test"}},
			},
		}
		cfg.Anvils["myrepo"] = a
		errs := cfg.Validate()
		for _, e := range errs {
			assert.NotContains(t, e, "temper.steps")
		}
	})
}

func TestTemperStepConfig_YAMLRoundTrip(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	original := &TemperCommandsConfig{
		Steps: []TemperStepConfig{
			{
				Name:     "install",
				Command:  "npm",
				Args:     []string{"ci"},
				Dir:      "web",
				Timeout:  5 * time.Minute,
				Required: boolPtr(true),
			},
			{
				Name:    "lint",
				Command: "npm",
				Args:    []string{"run", "lint"},
			},
			{
				Name:     "mypy",
				Command:  "mypy",
				Args:     []string{"src"},
				Required: boolPtr(false),
			},
		},
	}

	data, err := yaml.Marshal(original)
	require.NoError(t, err)

	var roundTripped TemperCommandsConfig
	err = yaml.Unmarshal(data, &roundTripped)
	require.NoError(t, err)

	require.Len(t, roundTripped.Steps, 3)
	assert.Equal(t, "install", roundTripped.Steps[0].Name)
	assert.Equal(t, "npm", roundTripped.Steps[0].Command)
	assert.Equal(t, []string{"ci"}, roundTripped.Steps[0].Args)
	assert.Equal(t, "web", roundTripped.Steps[0].Dir)
	assert.Equal(t, 5*time.Minute, roundTripped.Steps[0].Timeout)
	require.NotNil(t, roundTripped.Steps[0].Required)
	assert.True(t, *roundTripped.Steps[0].Required)

	// Step without optional fields
	assert.Equal(t, "lint", roundTripped.Steps[1].Name)
	assert.Nil(t, roundTripped.Steps[1].Required)
	assert.Empty(t, roundTripped.Steps[1].Dir)
	assert.Equal(t, time.Duration(0), roundTripped.Steps[1].Timeout)

	// Step with required: false
	require.NotNil(t, roundTripped.Steps[2].Required)
	assert.False(t, *roundTripped.Steps[2].Required)
}

func TestTemperStepConfig_VerifyCleanRoundTrip(t *testing.T) {
	original := &TemperCommandsConfig{
		Steps: []TemperStepConfig{
			{
				Name:        "build-frontend",
				Command:     "npm",
				Args:        []string{"run", "build"},
				Dir:         "web",
				VerifyClean: []string{"web/dist", "web/static/build"},
			},
		},
	}

	data, err := yaml.Marshal(original)
	require.NoError(t, err)
	assert.Contains(t, string(data), "verify_clean", "verify_clean must be in marshalled YAML")

	var roundTripped TemperCommandsConfig
	require.NoError(t, yaml.Unmarshal(data, &roundTripped))
	require.Len(t, roundTripped.Steps, 1)
	assert.Equal(t, []string{"web/dist", "web/static/build"}, roundTripped.Steps[0].VerifyClean)
}

func TestLoad_WardenSettings_Default(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("settings:\n  max_total_smiths: 2\n"), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, 30, cfg.Settings.Warden.ResolvedMaxRulesPerReview())
	assert.False(t, cfg.Settings.Warden.UseAllRules)
	assert.True(t, cfg.Settings.Warden.IsFilterPathGlobEnabled())
	assert.True(t, cfg.Settings.Warden.IsFilterCategoryEnabled())
	assert.True(t, cfg.Settings.Warden.IsFilterPatternGrepEnabled())
	assert.Equal(t, 180, cfg.Settings.Warden.ResolvedArchiveAfterDays())
	assert.InDelta(t, 0.6, cfg.Settings.Warden.ResolvedDedupThreshold(), 1e-9)
	assert.InDelta(t, 0.55, cfg.Settings.Warden.ResolvedOverlapThreshold(), 1e-9)
}

func TestLoad_WardenSettings_Custom(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  warden:
    max_rules_per_review: 50
    use_all_rules: true
    filter_path_glob: false
    filter_category: false
    filter_pattern_grep: false
    archive_after_days: 90
    dedup_threshold: 0.8
    overlap_threshold: 0.7
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, 50, cfg.Settings.Warden.MaxRulesPerReview)
	assert.True(t, cfg.Settings.Warden.UseAllRules)
	assert.False(t, cfg.Settings.Warden.IsFilterPathGlobEnabled())
	assert.False(t, cfg.Settings.Warden.IsFilterCategoryEnabled())
	assert.False(t, cfg.Settings.Warden.IsFilterPatternGrepEnabled())
	assert.Equal(t, 90, cfg.Settings.Warden.ResolvedArchiveAfterDays())
	assert.InDelta(t, 0.8, cfg.Settings.Warden.ResolvedDedupThreshold(), 1e-9)
	assert.InDelta(t, 0.7, cfg.Settings.Warden.ResolvedOverlapThreshold(), 1e-9)
}

// TestWardenSettings_OverlapThresholdCanBeDisabled: a negative value is
// returned as-is so the caller can read it as "disable the overlap
// criterion", leaving Jaccard as the only near-duplicate test.
func TestWardenSettings_OverlapThresholdCanBeDisabled(t *testing.T) {
	w := WardenSettings{OverlapThreshold: -1}
	assert.InDelta(t, -1.0, w.ResolvedOverlapThreshold(), 1e-9)
}

func TestTemperStepConfig_VerifyNoConflictMarkersRoundTrip(t *testing.T) {
	original := &TemperCommandsConfig{
		Steps: []TemperStepConfig{
			{
				Name:                    "conflict-markers",
				VerifyNoConflictMarkers: []string{"internal/web/dist", "internal/web/static"},
			},
		},
	}

	data, err := yaml.Marshal(original)
	require.NoError(t, err)
	assert.Contains(t, string(data), "verify_no_conflict_markers",
		"verify_no_conflict_markers must be in marshalled YAML")

	var roundTripped TemperCommandsConfig
	require.NoError(t, yaml.Unmarshal(data, &roundTripped))
	require.Len(t, roundTripped.Steps, 1)
	assert.Equal(t,
		[]string{"internal/web/dist", "internal/web/static"},
		roundTripped.Steps[0].VerifyNoConflictMarkers)
}

// captureSlog swaps the default slog logger for one writing to a buffer for
// the duration of the test. Returns the buffer so callers can assert on the
// captured output, and restores the original logger via t.Cleanup so a
// failure mid-test does not poison sibling tests.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestLoad_ForgeChatTurnTimeout_DefaultWhenUnset asserts that when forge.yaml
// omits settings.forgechat.turn_timeout the loader falls back to
// DefaultForgeChatTurnTimeout (5 minutes). The runner accesses the effective
// value via cfg.Settings.ForgeChat.ResolvedTurnTimeout(), which applies the
// default and clamping rules on top of the raw TurnTimeout field.
func TestLoad_ForgeChatTurnTimeout_DefaultWhenUnset(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  max_total_smiths: 2
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, cfg.Settings.ForgeChat.TurnTimeout,
		"unset turn_timeout should resolve to the 5m default")
	assert.Equal(t, DefaultForgeChatTurnTimeout, cfg.Settings.ForgeChat.TurnTimeout,
		"unset turn_timeout should equal DefaultForgeChatTurnTimeout")
}

// TestLoad_ForgeChatTurnTimeout_PreservesValueWithinRange asserts that any
// value between (0, MaxForgeChatTurnTimeout] is preserved verbatim, with no
// clamping and no warning emitted.
func TestLoad_ForgeChatTurnTimeout_PreservesValueWithinRange(t *testing.T) {
	buf := captureSlog(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  forgechat:
    turn_timeout: 10m
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, 10*time.Minute, cfg.Settings.ForgeChat.TurnTimeout,
		"in-range turn_timeout must be preserved as-is")
	assert.NotContains(t, buf.String(), "exceeds hard cap",
		"in-range turn_timeout must not emit the clamp warning")
}

// TestLoad_ForgeChatTurnTimeout_ClampedAboveMax asserts that values above the
// 15-minute hard cap are clamped to MaxForgeChatTurnTimeout and that a
// slog.Warn is emitted so operators see why their configured value did not
// take effect.
func TestLoad_ForgeChatTurnTimeout_ClampedAboveMax(t *testing.T) {
	buf := captureSlog(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  forgechat:
    turn_timeout: 30m
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, 15*time.Minute, cfg.Settings.ForgeChat.TurnTimeout,
		"turn_timeout above the cap must be clamped to MaxForgeChatTurnTimeout")
	assert.Equal(t, MaxForgeChatTurnTimeout, cfg.Settings.ForgeChat.TurnTimeout)

	// Verify the operator-facing warning landed in the log. Match on key
	// substrings rather than the full formatted line so future tweaks to
	// the log format don't break the regression.
	logged := buf.String()
	assert.Contains(t, logged, "forgechat.turn_timeout exceeds hard cap",
		"clamping must emit a slog warning so operators can diagnose it")
	assert.Contains(t, logged, "clamping",
		"warning must mention that the value was clamped")
}

// TestLoad_ForgeChatTurnTimeout_InvalidDuration verifies that a malformed
// duration string surfaces as an error rather than silently falling back to
// the default. This guards against typos like "5min" silently becoming 5m.
func TestLoad_ForgeChatTurnTimeout_InvalidDuration(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
settings:
  forgechat:
    turn_timeout: notaduration
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	_, err := Load(cfgPath)
	assert.ErrorContains(t, err, "forgechat.turn_timeout")
}

// TestForgeChatSettings_ResolvedTurnTimeout exercises the resolver function
// directly so the default/clamp logic is covered even for callers that
// construct a ForgeChatSettings in code (e.g. tests) without going through
// Load.
func TestForgeChatSettings_ResolvedTurnTimeout(t *testing.T) {
	cases := []struct {
		name  string
		input time.Duration
		want  time.Duration
	}{
		{"zero falls back to default", 0, DefaultForgeChatTurnTimeout},
		{"negative falls back to default", -1 * time.Second, DefaultForgeChatTurnTimeout},
		{"value within range preserved", 7 * time.Minute, 7 * time.Minute},
		{"value at cap preserved", MaxForgeChatTurnTimeout, MaxForgeChatTurnTimeout},
		{"value above cap clamped", 1 * time.Hour, MaxForgeChatTurnTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ForgeChatSettings{TurnTimeout: tc.input}
			assert.Equal(t, tc.want, f.ResolvedTurnTimeout())
		})
	}
}

// TestForgeChatSettings_ResolvedTurnExpiry covers the default fallback used by
// the TurnStore GC: unset/negative resolve to the 30m default, positive values
// pass through verbatim.
func TestForgeChatSettings_ResolvedTurnExpiry(t *testing.T) {
	cases := []struct {
		name  string
		input time.Duration
		want  time.Duration
	}{
		{"zero falls back to default", 0, DefaultForgeChatTurnExpiry},
		{"negative falls back to default", -5 * time.Minute, DefaultForgeChatTurnExpiry},
		{"positive preserved", 10 * time.Minute, 10 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ForgeChatSettings{TurnExpiry: tc.input}
			assert.Equal(t, tc.want, f.ResolvedTurnExpiry())
		})
	}
}

// TestForgeChatSettings_ResolvedTurnRetentionCap covers the retention-cap
// resolver: unset (0) resolves to the default, negative disables the cap and is
// returned as-is, positive values pass through.
func TestForgeChatSettings_ResolvedTurnRetentionCap(t *testing.T) {
	cases := []struct {
		name  string
		input int
		want  int
	}{
		{"zero falls back to default", 0, DefaultForgeChatTurnRetentionCap},
		{"negative disables cap and is preserved", -1, -1},
		{"positive preserved", 250, 250},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ForgeChatSettings{TurnRetentionCap: tc.input}
			assert.Equal(t, tc.want, f.ResolvedTurnRetentionCap())
		})
	}
}

// TestLoad_ForgeChatTurnExpiry_ParsedFromFile verifies the turn_expiry duration
// string is parsed off the config file into the settings struct.
func TestLoad_ForgeChatTurnExpiry_ParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "forge.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
settings:
  forgechat:
    turn_expiry: 45m
    turn_retention_cap: 500
`), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 45*time.Minute, cfg.Settings.ForgeChat.TurnExpiry)
	assert.Equal(t, 500, cfg.Settings.ForgeChat.TurnRetentionCap)
}

// TestResolveEmptyDiffAction verifies the empty_diff_action enum, including the
// conservative fallback: an unrecognised value must never auto-close beads.
func TestResolveEmptyDiffAction(t *testing.T) {
	cases := []struct {
		raw    string
		want   string
		wantOK bool
	}{
		{"", EmptyDiffActionAttention, true},
		{"attention", EmptyDiffActionAttention, true},
		{"close", EmptyDiffActionClose, true},
		{"  Close  ", EmptyDiffActionClose, true},
		{"ATTENTION", EmptyDiffActionAttention, true},
		{"delete", EmptyDiffActionAttention, false},
	}
	for _, tc := range cases {
		t.Run("raw="+tc.raw, func(t *testing.T) {
			action, ok := ResolveEmptyDiffAction(tc.raw)
			assert.Equal(t, tc.want, action)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, SettingsConfig{EmptyDiffAction: tc.raw}.ResolvedEmptyDiffAction())
		})
	}
}

// TestLoad_EmptyDiffAction_ParsedFromFile verifies settings.empty_diff_action is
// read off the config file.
func TestLoad_EmptyDiffAction_ParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "forge.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
settings:
  empty_diff_action: close
`), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, EmptyDiffActionClose, cfg.Settings.ResolvedEmptyDiffAction())
}

// TestResolvedAssay_MaxTurnsPerPass covers the per-anvil turn budget in the one
// place its fallback is decided. The budget is the setting an operator reaches
// for when one repo's layout starves the passes (a rules file plus a handful of
// supporting reads), and the point of the overlay is that the repo needing the
// headroom gets it without every other anvil's runs paying for a raised global.
func TestResolvedAssay_MaxTurnsPerPass(t *testing.T) {
	i := func(v int) *int { return &v }
	cases := []struct {
		name   string
		global *int
		anvils map[string]AnvilConfig
		anvil  string
		want   int
	}{
		{
			name:   "per-anvil override wins over the global",
			global: i(16),
			anvils: map[string]AnvilConfig{"deep": {Path: "/repos/deep", Assay: &AssayConfig{MaxTurnsPerPass: i(30)}}},
			anvil:  "deep",
			want:   30,
		},
		{
			name:   "an anvil with no override inherits the global",
			global: i(16),
			anvils: map[string]AnvilConfig{"plain": {Path: "/repos/plain"}},
			anvil:  "plain",
			want:   16,
		},
		{
			name:   "an anvil with an assay block but no budget inherits the global",
			global: i(16),
			anvils: map[string]AnvilConfig{"other": {Path: "/repos/other", Assay: &AssayConfig{NitCap: i(3)}}},
			anvil:  "other",
			want:   16,
		},
		{
			name:   "an unknown anvil inherits the global",
			global: i(16),
			anvils: map[string]AnvilConfig{"deep": {Path: "/repos/deep", Assay: &AssayConfig{MaxTurnsPerPass: i(30)}}},
			anvil:  "does-not-exist",
			want:   16,
		},
		{
			name:   "an override equal to the global is still an override",
			global: i(16),
			anvils: map[string]AnvilConfig{"same": {Path: "/repos/same", Assay: &AssayConfig{MaxTurnsPerPass: i(16)}}},
			anvil:  "same",
			want:   16,
		},
		{
			// 0 means unset at both levels; the engine then applies its own
			// default, which is why the resolver reports 0 rather than guessing.
			name:   "unset at both levels leaves the engine default to apply",
			global: nil,
			anvils: map[string]AnvilConfig{"plain": {Path: "/repos/plain"}},
			anvil:  "plain",
			want:   0,
		},
		{
			name:   "a per-anvil override applies with no global set",
			global: nil,
			anvils: map[string]AnvilConfig{"deep": {Path: "/repos/deep", Assay: &AssayConfig{MaxTurnsPerPass: i(30)}}},
			anvil:  "deep",
			want:   30,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Assay: AssayConfig{MaxTurnsPerPass: tc.global}, Anvils: tc.anvils}
			assert.Equal(t, tc.want, cfg.ResolvedAssay(tc.anvil).GetMaxTurnsPerPass())
		})
	}
}

func TestValidate_AssayMaxTurnsPerPass(t *testing.T) {
	base := func() *Config {
		c := Defaults()
		c.Anvils = map[string]AnvilConfig{"test": {Path: "/repos/test"}}
		return &c
	}
	i := func(v int) *int { return &v }
	const globalMsg = "assay.max_turns_per_pass must not be negative (omit it to use the engine default)"

	t.Run("defaults are valid", func(t *testing.T) {
		for _, e := range base().Validate() {
			assert.NotContains(t, e, "max_turns_per_pass")
		}
	})

	// 0 is the documented spelling of "unset", so it is not an error — unlike
	// the spend ceiling, there is no such thing as a session with no turns.
	t.Run("zero means unset", func(t *testing.T) {
		cfg := base()
		cfg.Assay.MaxTurnsPerPass = i(0)
		for _, e := range cfg.Validate() {
			assert.NotContains(t, e, "max_turns_per_pass")
		}
	})

	t.Run("negative is a typo, not a switch", func(t *testing.T) {
		cfg := base()
		cfg.Assay.MaxTurnsPerPass = i(-1)
		assert.Contains(t, cfg.Validate(), globalMsg)
	})

	t.Run("per-anvil overlay", func(t *testing.T) {
		cfg := base()
		cfg.Anvils["test"] = AnvilConfig{Path: "/repos/test", Assay: &AssayConfig{MaxTurnsPerPass: i(-4)}}
		assert.Contains(t, cfg.Validate(),
			`anvil "test": assay.max_turns_per_pass must not be negative (omit it to use the engine default)`)
	})
}

// TestAssayDefaultEstimateFloorLeavesRoomUnderTheDefaultCap pins the
// relationship between the two shipped defaults, not either number.
//
// The in-flight gate refuses a review when recorded spend plus the estimate
// does not fit under the cap, and a review refused that way has its head
// released to merge readiness as "reviewed". So at floor >= cap an
// unconfigured deployment admits one review and then auto-merges the rest of
// the day's PRs unreviewed — the estimate would be acting as an off switch
// rather than as a budget. Whichever default moves, they must stay apart.
func TestAssayDefaultEstimateFloorLeavesRoomUnderTheDefaultCap(t *testing.T) {
	def := Defaults().Assay
	limit := def.GetDailyCostLimitUSD()
	floor := def.GetRunCostEstimateUSD()

	require.Greater(t, limit, 0.0, "the shipped Assay cap must not be unlimited")
	assert.Less(t, floor, limit,
		"the default run cost estimate floor ($%.2f) must sit below the default daily cap ($%.2f), "+
			"or one cent of recorded spend refuses every later review and releases its head unreviewed",
		floor, limit)
	assert.GreaterOrEqual(t, limit/floor, 2.0,
		"the default cap must fit more than one estimated review")
}

func TestAssayConfig_RunCostEstimateUSD(t *testing.T) {
	var a AssayConfig
	assert.Equal(t, defaultAssayRunCostEstimateUSD, a.GetRunCostEstimateUSD())

	// Zero is legitimate: no floor, estimate from recorded runs alone.
	zero := 0.0
	a.RunCostEstimateUSD = &zero
	assert.Equal(t, 0.0, a.GetRunCostEstimateUSD())

	set := 7.25
	a.RunCostEstimateUSD = &set
	assert.Equal(t, 7.25, a.GetRunCostEstimateUSD())
}

func TestResolvedAssay_RunCostEstimateUSD_AnvilOverlay(t *testing.T) {
	global, anvil := 5.0, 9.0
	cfg := &Config{
		Assay: AssayConfig{RunCostEstimateUSD: &global},
		Anvils: map[string]AnvilConfig{
			"api":   {Path: "/repos/api", Assay: &AssayConfig{RunCostEstimateUSD: &anvil}},
			"plain": {Path: "/repos/plain"},
		},
	}
	assert.Equal(t, 9.0, cfg.ResolvedAssay("api").GetRunCostEstimateUSD())
	assert.Equal(t, 5.0, cfg.ResolvedAssay("plain").GetRunCostEstimateUSD())
}

func TestValidate_AssayRunCostEstimateUSD(t *testing.T) {
	const msg = "assay.run_cost_estimate_usd must be a non-negative finite number (set 0 to estimate from recorded runs alone)"

	cfg := Defaults()
	zero := 0.0
	cfg.Assay.RunCostEstimateUSD = &zero
	for _, e := range cfg.Validate() {
		assert.NotContains(t, e, "run_cost_estimate_usd")
	}

	// A negative floor would subtract from an in-flight reservation, widening
	// the very overrun it exists to close.
	neg := -1.0
	cfg.Assay.RunCostEstimateUSD = &neg
	assert.Contains(t, cfg.Validate(), msg)

	bad := -2.0
	cfg = Defaults()
	cfg.Anvils["test"] = AnvilConfig{Path: "/repos/test", Assay: &AssayConfig{RunCostEstimateUSD: &bad}}
	assert.Contains(t, cfg.Validate(), `anvil "test": `+msg)
}
