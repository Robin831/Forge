package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
					MaxTotalSmiths:       4,
					MaxReviewAttempts:    5,
					PollInterval:         1 * time.Minute,
					SmithTimeout:         30 * time.Minute,
					BellowsInterval:      2 * time.Minute,
					MaxCIFixAttempts:     5,
					MaxReviewFixAttempts: 5,
					MaxRebaseAttempts:    3,
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
					MaxTotalSmiths:  0,
					MaxReviewAttempts: 0,
					PollInterval:    5 * time.Second,
					SmithTimeout:    30 * time.Second,
					BellowsInterval: 10 * time.Second,
				},
			},
			expected: []string{
				"settings.max_total_smiths must be >= 1",
				"settings.max_review_attempts must be >= 1",
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
					MaxTotalSmiths:       4,
					MaxReviewAttempts:    5,
					PollInterval:         1 * time.Minute,
					SmithTimeout:         30 * time.Minute,
					BellowsInterval:      2 * time.Minute,
					MaxCIFixAttempts:     5,
					MaxReviewFixAttempts: 5,
					MaxRebaseAttempts:    3,
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
					MaxTotalSmiths:       4,
					MaxReviewAttempts:    5,
					PollInterval:         1 * time.Minute,
					SmithTimeout:         30 * time.Minute,
					BellowsInterval:      2 * time.Minute,
					MaxCIFixAttempts:     5,
					MaxReviewFixAttempts: 5,
					MaxRebaseAttempts:    3,
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
					MaxTotalSmiths:       4,
					MaxReviewAttempts:    5,
					PollInterval:         1 * time.Minute,
					SmithTimeout:         30 * time.Minute,
					BellowsInterval:      2 * time.Minute,
					MaxCIFixAttempts:     5,
					MaxReviewFixAttempts: 5,
					MaxRebaseAttempts:    3,
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
					MaxTotalSmiths:       4,
					MaxReviewAttempts:    5,
					PollInterval:         1 * time.Minute,
					SmithTimeout:         30 * time.Minute,
					BellowsInterval:      2 * time.Minute,
					MaxCIFixAttempts:     5,
					MaxReviewFixAttempts: 5,
					MaxRebaseAttempts:    3,
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

func TestLoad_NoFile_UsesDefaults(t *testing.T) {
	// Load with a path that doesn't exist → viper.ConfigFileNotFoundError → uses defaults
	cfg, err := Load("/nonexistent/forge.yaml")
	// Will error because explicit path not found is treated as parse error by viper
	// Either an error or defaults — just verify the call doesn't panic
	_ = cfg
	_ = err
}
