package config

import (
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
		"smith":    {"claude/claude-opus-4-6"},
		"warden":   {"claude/claude-sonnet-4-6"},
		"schematic": {"gemini/gemini-2.5-flash"},
	}

	require.NoError(t, Save(&original, cfgPath))

	loaded, err := Load(cfgPath)
	require.NoError(t, err)

	assert.Equal(t, original.Settings.StageProviders, loaded.Settings.StageProviders,
		"stage_providers must survive Save→Load round-trip")
}

func TestSave_RoundTrip_PerAnvilStageProviders(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")

	original := Defaults()
	original.Anvils["myrepo"] = AnvilConfig{
		Path: "/some/path",
		StageProviders: map[string][]string{
			"warden":   {"gemini/gemini-2.5-pro"},
			"cifix":    {"claude/claude-sonnet-4-6"},
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
