// Package config handles loading and validating Forge configuration from
// forge.yaml files and environment variable overrides.
//
// Config resolution order (first found wins):
//  1. --config flag (explicit path)
//  2. ./forge.yaml (working directory)
//  3. ~/.forge/config.yaml (user home)
//
// Environment variables override file values with the FORGE_ prefix:
//
//	FORGE_SETTINGS_POLL_INTERVAL=60s
//	FORGE_SETTINGS_MAX_TOTAL_SMITHS=4
package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/viper"
)

// Config is the top-level configuration for The Forge.
type Config struct {
	Anvils        map[string]AnvilConfig `mapstructure:"anvils"`
	Settings      SettingsConfig         `mapstructure:"settings"`
	Notifications NotificationsConfig    `mapstructure:"notifications"`
}

// AnvilConfig defines a registered repository (anvil).
type AnvilConfig struct {
	Path                    string `mapstructure:"path"`
	MaxSmiths               int    `mapstructure:"max_smiths"`
	AutoDispatch            string `mapstructure:"auto_dispatch"`
	AutoDispatchTag         string `mapstructure:"auto_dispatch_tag"`
	AutoDispatchMinPriority int    `mapstructure:"auto_dispatch_min_priority"`
	// SchematicEnabled controls whether the Schematic pre-worker runs for
	// beads in this anvil. When nil, the global setting is used. Set to
	// a pointer to false to disable per-anvil.
	SchematicEnabled *bool `mapstructure:"schematic_enabled"`
	// GolangciLint controls whether golangci-lint runs as a Temper step
	// for Go projects. When nil (default), golangci-lint runs if the
	// binary is found on PATH. Set to a pointer to false to disable.
	GolangciLint *bool `mapstructure:"golangci_lint"`
}

// SettingsConfig holds global operational settings.
type SettingsConfig struct {
	PollInterval  time.Duration `mapstructure:"poll_interval"`
	SmithTimeout  time.Duration `mapstructure:"smith_timeout"`
	MaxTotalSmiths int          `mapstructure:"max_total_smiths"`
	MaxReviewAttempts int       `mapstructure:"max_review_attempts"`
	ClaudeFlags   []string      `mapstructure:"claude_flags"`
	// Providers is the ordered list of AI providers to try.
	// Each entry is a Kind string ("claude", "gemini") or "kind:command" pair.
	// When a provider signals a rate limit the next one in the list is tried.
	// Defaults to ["claude", "gemini"] when empty.
	Providers     []string      `mapstructure:"providers"`
	// RateLimitBackoff is how long dispatchBead waits after releasing a bead
	// back to open when all providers are rate limited. During this window the
	// bead slot stays reserved (activeBeads) so the poller does not
	// immediately re-claim it. Defaults to 5 minutes.
	RateLimitBackoff time.Duration `mapstructure:"rate_limit_backoff"`
	// SmithProviders is the ordered list of AI providers used specifically for
	// dispatch pipeline (Smith + Warden + Schematic). When empty, Providers is
	// used as fallback. This lets smiths run a more capable model (e.g.
	// claude/claude-opus-4-6) while lifecycle workers (cifix, reviewfix) use a
	// lighter model. Accepts the same "kind/model" format as Providers.
	SmithProviders []string `mapstructure:"smith_providers"`
	// SchematicEnabled enables the Schematic pre-worker globally. When true,
	// beads that exceed the word threshold or carry the "decompose" tag are
	// analysed before Smith starts. Default: false.
	SchematicEnabled bool `mapstructure:"schematic_enabled"`
	// SchematicWordThreshold is the minimum word count in a bead description
	// to trigger automatic schematic analysis. When this value is zero or
	// unset, the daemon applies an effective default of 100.
	SchematicWordThreshold int `mapstructure:"schematic_word_threshold"`
	// BellowsInterval is how often the Bellows PR monitor polls GitHub for
	// status changes on open PRs. Defaults to 2 minutes.
	BellowsInterval time.Duration `mapstructure:"bellows_interval"`
	// DailyCostLimit is the maximum estimated USD spend per calendar day.
	// When the running total exceeds this value, auto-dispatch is paused until
	// the next calendar day. Zero means no limit (default).
	DailyCostLimit float64 `mapstructure:"daily_cost_limit"`
	// MaxCIFixAttempts is the maximum number of CI fix cycles per PR before
	// the PR is considered exhausted. Default: 5.
	MaxCIFixAttempts int `mapstructure:"max_ci_fix_attempts"`
	// MaxReviewFixAttempts is the maximum number of review fix cycles per PR
	// before the PR is considered exhausted. Default: 5.
	MaxReviewFixAttempts int `mapstructure:"max_review_fix_attempts"`
	// MaxRebaseAttempts is the maximum number of conflict rebase attempts per
	// PR before the PR is considered exhausted. Default: 3.
	MaxRebaseAttempts int `mapstructure:"max_rebase_attempts"`
	// StaleInterval is how long a worker's log file can go without being
	// modified before the worker is marked as stalled. A value of 0 disables
	// stale detection. Defaults to 5 minutes.
	StaleInterval time.Duration `mapstructure:"stale_interval"`
	// DepcheckInterval is how often the dependency checker runs 'go list -m -u all'
	// on Go anvils. A value of 0 disables depcheck. Defaults to 168h (weekly).
	DepcheckInterval time.Duration `mapstructure:"depcheck_interval"`
	// DepcheckTimeout is the maximum time allowed for a single 'go list -m -u all'
	// invocation per anvil. Defaults to 5 minutes.
	DepcheckTimeout time.Duration `mapstructure:"depcheck_timeout"`
	// VulncheckInterval is how often govulncheck runs on registered Go anvils.
	// Set to 0 to disable scheduled scanning. Default: 24h (daily).
	VulncheckInterval time.Duration `mapstructure:"vulncheck_interval"`
	// VulncheckTimeout is the maximum time allowed for a single govulncheck
	// invocation per anvil (govulncheck downloads the vuln DB on first run).
	// Defaults to 10 minutes.
	VulncheckTimeout time.Duration `mapstructure:"vulncheck_timeout"`
}

// NotificationsConfig holds webhook and notification settings.
type NotificationsConfig struct {
	TeamsWebhookURL string `mapstructure:"teams_webhook_url"`
	Enabled         bool   `mapstructure:"enabled"`
	// Events to notify on. Empty = all. Options: pr_created, bead_failed, daily_cost, worker_done, bead_decomposed.
	Events []string `mapstructure:"events"`
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		Anvils: make(map[string]AnvilConfig),
		Settings: SettingsConfig{
			PollInterval:         5 * time.Minute,
			SmithTimeout:         30 * time.Minute,
			MaxTotalSmiths:       4,
			MaxReviewAttempts:    2,
			ClaudeFlags:          []string{},
			// No Providers default here — provider.FromConfig handles empty slice.
			RateLimitBackoff:     5 * time.Minute,
			BellowsInterval:      2 * time.Minute,
			MaxCIFixAttempts:     5,
			MaxReviewFixAttempts: 5,
			MaxRebaseAttempts:    3,
			StaleInterval:        5 * time.Minute,
			DepcheckInterval:     168 * time.Hour, // weekly
			DepcheckTimeout:      5 * time.Minute,
			VulncheckInterval:    24 * time.Hour,
		VulncheckTimeout:     10 * time.Minute,
		},
	}
}

// Load reads the configuration from the given file path, or auto-discovers
// forge.yaml from the working directory or ~/.forge/config.yaml.
// Environment variables with the FORGE_ prefix override file values.
func Load(configFile string) (*Config, error) {
	v := viper.New()

	// Set defaults
	v.SetDefault("settings.poll_interval", "5m")
	v.SetDefault("settings.smith_timeout", "30m")
	v.SetDefault("settings.max_total_smiths", 4)
	v.SetDefault("settings.max_review_attempts", 2)
	v.SetDefault("settings.claude_flags", []string{})
	v.SetDefault("settings.rate_limit_backoff", "5m")
	v.SetDefault("settings.bellows_interval", "2m")
	v.SetDefault("settings.max_ci_fix_attempts", 5)
	v.SetDefault("settings.max_review_fix_attempts", 5)
	v.SetDefault("settings.max_rebase_attempts", 3)
	v.SetDefault("settings.stale_interval", "5m")
	v.SetDefault("settings.depcheck_interval", "168h")
	v.SetDefault("settings.depcheck_timeout", "5m")
	v.SetDefault("settings.vulncheck_interval", "24h")
	v.SetDefault("settings.vulncheck_timeout", "10m")

	// Environment variable support: FORGE_SETTINGS_POLL_INTERVAL etc.
	v.SetEnvPrefix("FORGE")
	v.AutomaticEnv()

	// Config file resolution
	if configFile != "" {
		v.SetConfigFile(configFile)
	} else {
		v.SetConfigName("forge")
		v.SetConfigType("yaml")

		// 1. Working directory
		v.AddConfigPath(".")

		// 2. ~/.forge/
		if home, err := os.UserHomeDir(); err == nil {
			v.AddConfigPath(filepath.Join(home, ".forge"))
		}
	}

	// Read config (file not found is OK — we'll use defaults + env)
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			// File exists but can't be parsed
			return nil, fmt.Errorf("reading config: %w", err)
		}
	}

	cfg := Defaults()
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling config: %w", err)
	}

	// Set per-anvil defaults and parse durations
	for name, anvil := range cfg.Anvils {
		if anvil.AutoDispatch == "" {
			anvil.AutoDispatch = "all"
		}
		cfg.Anvils[name] = anvil
	}

	// Parse durations from string values (viper returns strings from YAML)
	if raw := v.GetString("settings.poll_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid poll_interval %q: %w", raw, err)
		}
		cfg.Settings.PollInterval = d
	}
	if raw := v.GetString("settings.smith_timeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid smith_timeout %q: %w", raw, err)
		}
		cfg.Settings.SmithTimeout = d
	}
	if raw := v.GetString("settings.rate_limit_backoff"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid rate_limit_backoff %q: %w", raw, err)
		}
		cfg.Settings.RateLimitBackoff = d
	}
	if raw := v.GetString("settings.bellows_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid bellows_interval %q: %w", raw, err)
		}
		cfg.Settings.BellowsInterval = d
	}
	if raw := v.GetString("settings.stale_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid stale_interval %q: %w", raw, err)
		}
		cfg.Settings.StaleInterval = d
	}
	if raw := v.GetString("settings.depcheck_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid depcheck_interval %q: %w", raw, err)
		}
		cfg.Settings.DepcheckInterval = d
	}
	if raw := v.GetString("settings.depcheck_timeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid depcheck_timeout %q: %w", raw, err)
		}
		cfg.Settings.DepcheckTimeout = d
	}
	if raw := v.GetString("settings.vulncheck_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid vulncheck_interval %q: %w", raw, err)
		}
		cfg.Settings.VulncheckInterval = d
	}
	if raw := v.GetString("settings.vulncheck_timeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid vulncheck_timeout %q: %w", raw, err)
		}
		cfg.Settings.VulncheckTimeout = d
	}

	return &cfg, nil
}

// ConfigFilePath returns the path of the config file that was loaded,
// or empty string if no file was found.
func ConfigFilePath(configFile string) string {
	v := viper.New()

	if configFile != "" {
		v.SetConfigFile(configFile)
	} else {
		v.SetConfigName("forge")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		if home, err := os.UserHomeDir(); err == nil {
			v.AddConfigPath(filepath.Join(home, ".forge"))
		}
	}

	if err := v.ReadInConfig(); err != nil {
		return ""
	}
	return v.ConfigFileUsed()
}

// Validate checks the config for logical errors.
func (c *Config) Validate() []string {
	var errs []string

	if c.Settings.MaxTotalSmiths < 1 {
		errs = append(errs, "settings.max_total_smiths must be >= 1")
	}
	if c.Settings.MaxReviewAttempts < 1 {
		errs = append(errs, "settings.max_review_attempts must be >= 1")
	}
	if c.Settings.PollInterval < 10*time.Second {
		errs = append(errs, "settings.poll_interval must be >= 10s")
	}
	if c.Settings.SmithTimeout < 1*time.Minute {
		errs = append(errs, "settings.smith_timeout must be >= 1m")
	}
	if c.Settings.BellowsInterval < 30*time.Second {
		errs = append(errs, "settings.bellows_interval must be >= 30s")
	}
	if c.Settings.DailyCostLimit < 0 || math.IsNaN(c.Settings.DailyCostLimit) || math.IsInf(c.Settings.DailyCostLimit, 0) {
		errs = append(errs, "settings.daily_cost_limit must be a non-negative finite number")
	}
	if c.Settings.StaleInterval < 0 {
		errs = append(errs, "settings.stale_interval must not be negative (set to 0 to disable)")
	} else if c.Settings.StaleInterval > 0 && c.Settings.StaleInterval < 30*time.Second {
		errs = append(errs, "settings.stale_interval must be >= 30s when enabled (or 0 to disable)")
	}
	if c.Settings.MaxCIFixAttempts < 1 {
		errs = append(errs, "settings.max_ci_fix_attempts must be >= 1")
	}
	if c.Settings.MaxReviewFixAttempts < 1 {
		errs = append(errs, "settings.max_review_fix_attempts must be >= 1")
	}
	if c.Settings.MaxRebaseAttempts < 1 {
		errs = append(errs, "settings.max_rebase_attempts must be >= 1")
	}

	if c.Settings.DepcheckInterval < 0 {
		errs = append(errs, "settings.depcheck_interval must not be negative (set to 0 to disable)")
	} else if c.Settings.DepcheckInterval > 0 && c.Settings.DepcheckInterval < 1*time.Hour {
		errs = append(errs, "settings.depcheck_interval must be >= 1h when enabled (or 0 to disable)")
	}
	if c.Settings.DepcheckTimeout < 0 {
		errs = append(errs, "settings.depcheck_timeout must not be negative")
	}

	for name, anvil := range c.Anvils {
		if anvil.Path == "" {
			errs = append(errs, fmt.Sprintf("anvil %q: path is required", name))
		}
		if anvil.MaxSmiths < 0 {
			errs = append(errs, fmt.Sprintf("anvil %q: max_smiths must be >= 0", name))
		}

		switch anvil.AutoDispatch {
		case "all", "tagged", "priority", "off", "":
			// valid
		default:
			errs = append(errs, fmt.Sprintf("anvil %q: invalid auto_dispatch %q (must be all|tagged|priority|off)", name, anvil.AutoDispatch))
		}

		if anvil.AutoDispatch == "tagged" && anvil.AutoDispatchTag == "" {
			errs = append(errs, fmt.Sprintf("anvil %q: auto_dispatch_tag must be non-empty when auto_dispatch is \"tagged\"", name))
		}
		if anvil.AutoDispatch == "priority" && (anvil.AutoDispatchMinPriority < 0 || anvil.AutoDispatchMinPriority > 4) {
			errs = append(errs, fmt.Sprintf("anvil %q: auto_dispatch_min_priority must be 0-4 (0 = critical-only) when auto_dispatch is \"priority\"", name))
		}
	}

	return errs
}

// Save writes the config to the specified file path in YAML format.
func Save(cfg *Config, path string) error {
	v := viper.New()

	// Set all values from our config struct
	for name, anvil := range cfg.Anvils {
		v.Set("anvils."+name+".path", anvil.Path)
		v.Set("anvils."+name+".max_smiths", anvil.MaxSmiths)
		v.Set("anvils."+name+".auto_dispatch", anvil.AutoDispatch)
		v.Set("anvils."+name+".auto_dispatch_tag", anvil.AutoDispatchTag)
		v.Set("anvils."+name+".auto_dispatch_min_priority", anvil.AutoDispatchMinPriority)
	}

	v.Set("settings.poll_interval", cfg.Settings.PollInterval.String())
	v.Set("settings.smith_timeout", cfg.Settings.SmithTimeout.String())
	v.Set("settings.max_total_smiths", cfg.Settings.MaxTotalSmiths)
	v.Set("settings.max_review_attempts", cfg.Settings.MaxReviewAttempts)
	v.Set("settings.claude_flags", cfg.Settings.ClaudeFlags)
	v.Set("settings.rate_limit_backoff", cfg.Settings.RateLimitBackoff.String())
	v.Set("settings.bellows_interval", cfg.Settings.BellowsInterval.String())
	v.Set("settings.stale_interval", cfg.Settings.StaleInterval.String())
	v.Set("settings.max_ci_fix_attempts", cfg.Settings.MaxCIFixAttempts)
	v.Set("settings.max_review_fix_attempts", cfg.Settings.MaxReviewFixAttempts)
	v.Set("settings.max_rebase_attempts", cfg.Settings.MaxRebaseAttempts)

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	return v.WriteConfigAs(path)
}
