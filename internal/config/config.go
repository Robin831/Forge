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
	"strings"
	"time"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for The Forge.
type Config struct {
	Anvils        map[string]AnvilConfig `mapstructure:"anvils" yaml:"anvils"`
	Settings      SettingsConfig         `mapstructure:"settings" yaml:"settings"`
	Notifications NotificationsConfig    `mapstructure:"notifications" yaml:"notifications,omitempty"`
}

// AnvilConfig defines a registered repository (anvil).
type AnvilConfig struct {
	Path                    string `mapstructure:"path" yaml:"path"`
	MaxSmiths               int    `mapstructure:"max_smiths" yaml:"max_smiths"`
	AutoDispatch            string `mapstructure:"auto_dispatch" yaml:"auto_dispatch"`
	AutoDispatchTag         string `mapstructure:"auto_dispatch_tag" yaml:"auto_dispatch_tag,omitempty"`
	AutoDispatchMinPriority int    `mapstructure:"auto_dispatch_min_priority" yaml:"auto_dispatch_min_priority"`
	// SchematicEnabled controls whether the Schematic pre-worker runs for
	// beads in this anvil. When nil, the global setting is used. Set to
	// a pointer to false to disable per-anvil.
	SchematicEnabled *bool `mapstructure:"schematic_enabled" yaml:"schematic_enabled,omitempty"`
	// GolangciLint controls whether golangci-lint runs as a Temper step
	// for Go projects. When nil (default), golangci-lint runs if the
	// binary is found on PATH. Set to a pointer to false to disable.
	GolangciLint *bool `mapstructure:"golangci_lint" yaml:"golangci_lint,omitempty"`
	// GoRaceDetection enables the Go race detector (-race flag) as a
	// separate temper step for this anvil. When nil, the global setting
	// is used. Default off since -race slows tests and increases memory.
	GoRaceDetection *bool `mapstructure:"go_race_detection" yaml:"go_race_detection,omitempty"`
	// DepcheckEnabled controls whether the depcheck monitor scans this
	// anvil for outdated dependencies. When nil (default), depcheck runs
	// as normal (opt-out). Set to false to skip this anvil entirely.
	DepcheckEnabled *bool `mapstructure:"depcheck_enabled" yaml:"depcheck_enabled,omitempty"`
}

// SettingsConfig holds global operational settings.
type SettingsConfig struct {
	PollInterval  time.Duration `mapstructure:"poll_interval" yaml:"poll_interval"`
	SmithTimeout  time.Duration `mapstructure:"smith_timeout" yaml:"smith_timeout"`
	MaxTotalSmiths int          `mapstructure:"max_total_smiths" yaml:"max_total_smiths"`
	MaxReviewAttempts int       `mapstructure:"max_review_attempts" yaml:"max_review_attempts"`
	// MaxPipelineIterations is the maximum number of Smith-Warden cycles
	// in the initial pipeline loop before declaring failure. This controls
	// how many times Smith can be asked to revise its implementation based
	// on Temper or Warden feedback during a single bead run. Default: 5.
	MaxPipelineIterations int   `mapstructure:"max_pipeline_iterations" yaml:"max_pipeline_iterations"`
	ClaudeFlags   []string      `mapstructure:"claude_flags" yaml:"claude_flags"`
	// Providers is the ordered list of AI providers to try.
	// Each entry is a Kind string ("claude", "gemini") or "kind:command" pair.
	// When a provider signals a rate limit the next one in the list is tried.
	// Defaults to ["claude", "gemini"] when empty.
	Providers     []string      `mapstructure:"providers" yaml:"providers,omitempty"`
	// RateLimitBackoff is how long dispatchBead waits after releasing a bead
	// back to open when all providers are rate limited. During this window the
	// bead slot stays reserved (activeBeads) so the poller does not
	// immediately re-claim it. Defaults to 5 minutes.
	RateLimitBackoff time.Duration `mapstructure:"rate_limit_backoff" yaml:"rate_limit_backoff"`
	// SmithProviders is the ordered list of AI providers used specifically for
	// dispatch pipeline (Smith + Warden + Schematic). When empty, Providers is
	// used as fallback. This lets smiths run a more capable model (e.g.
	// claude/claude-opus-4-6) while lifecycle workers (cifix, reviewfix) use a
	// lighter model. Accepts the same "kind/model" format as Providers.
	SmithProviders []string `mapstructure:"smith_providers" yaml:"smith_providers,omitempty"`
	// SchematicEnabled enables the Schematic pre-worker globally. When true,
	// beads that exceed the word threshold or carry the "decompose" tag are
	// analysed before Smith starts. Default: false.
	SchematicEnabled bool `mapstructure:"schematic_enabled" yaml:"schematic_enabled"`
	// SchematicWordThreshold is the minimum word count in a bead description
	// to trigger automatic schematic analysis. When this value is zero or
	// unset, the daemon applies an effective default of 100.
	SchematicWordThreshold int `mapstructure:"schematic_word_threshold" yaml:"schematic_word_threshold,omitempty"`
	// BellowsInterval is how often the Bellows PR monitor polls GitHub for
	// status changes on open PRs. Defaults to 2 minutes.
	BellowsInterval time.Duration `mapstructure:"bellows_interval" yaml:"bellows_interval"`
	// DailyCostLimit is the maximum estimated USD spend per calendar day.
	// When the running total exceeds this value, auto-dispatch is paused until
	// the next calendar day. Zero means no limit (default).
	DailyCostLimit float64 `mapstructure:"daily_cost_limit" yaml:"daily_cost_limit,omitempty"`
	// MaxCIFixAttempts is the maximum number of CI fix cycles per PR before
	// the PR is considered exhausted. Default: 5.
	MaxCIFixAttempts int `mapstructure:"max_ci_fix_attempts" yaml:"max_ci_fix_attempts"`
	// MaxReviewFixAttempts is the maximum number of review fix cycles per PR
	// before the PR is considered exhausted. Default: 5.
	MaxReviewFixAttempts int `mapstructure:"max_review_fix_attempts" yaml:"max_review_fix_attempts"`
	// MaxRebaseAttempts is the maximum number of conflict rebase attempts per
	// PR before the PR is considered exhausted. Default: 3.
	MaxRebaseAttempts int `mapstructure:"max_rebase_attempts" yaml:"max_rebase_attempts"`
	// MergeStrategy controls how PRs are merged from the Hearth TUI.
	// Valid values: "squash" (default), "merge", "rebase".
	MergeStrategy string `mapstructure:"merge_strategy" yaml:"merge_strategy,omitempty"`
	// StaleInterval is how long a worker's log file can go without being
	// modified before the worker is marked as stalled. A value of 0 disables
	// stale detection. Defaults to 5 minutes.
	StaleInterval time.Duration `mapstructure:"stale_interval" yaml:"stale_interval"`
	// DepcheckInterval is how often the dependency checker runs 'go list -m -u all'
	// on Go anvils. A value of 0 disables depcheck. Defaults to 168h (weekly).
	DepcheckInterval time.Duration `mapstructure:"depcheck_interval" yaml:"depcheck_interval,omitempty"`
	// DepcheckTimeout is the maximum time allowed for a single 'go list -m -u all'
	// invocation per anvil. Defaults to 5 minutes.
	DepcheckTimeout time.Duration `mapstructure:"depcheck_timeout" yaml:"depcheck_timeout,omitempty"`
	// VulncheckInterval is how often govulncheck runs on registered Go anvils.
	// Set to 0 to disable scheduled scanning. Default: 24h (daily).
	VulncheckInterval time.Duration `mapstructure:"vulncheck_interval" yaml:"vulncheck_interval,omitempty"`
	// VulncheckTimeout is the maximum time allowed for a single govulncheck
	// invocation per anvil (govulncheck downloads the vuln DB on first run).
	// Defaults to 10 minutes.
	VulncheckTimeout time.Duration `mapstructure:"vulncheck_timeout" yaml:"vulncheck_timeout,omitempty"`
	// VulncheckEnabled controls whether vulnerability scanning is active.
	// When false, scheduled scanning and "forge scan" are disabled regardless
	// of VulncheckInterval. Default: true.
	VulncheckEnabled *bool `mapstructure:"vulncheck_enabled" yaml:"vulncheck_enabled,omitempty"`
	// GoRaceDetection enables the Go race detector (-race flag) as a
	// separate temper step globally. Per-anvil settings override this.
	// Default: false.
	GoRaceDetection bool `mapstructure:"go_race_detection" yaml:"go_race_detection"`
	// AutoLearnRules enables automatic learning of Warden review rules from
	// Copilot comments when a PR is merged. Bellows will fetch Copilot review
	// comments, distill them into rules via Claude, and save them to the
	// anvil's .forge/warden-rules.yaml. Default: false.
	AutoLearnRules bool `mapstructure:"auto_learn_rules" yaml:"auto_learn_rules"`
	// CopilotDailyRequestLimit is the maximum number of weighted Copilot
	// premium requests per calendar day. When the running total exceeds this
	// value, the Copilot provider is skipped in the fallback chain (other
	// providers are unaffected). Zero means no limit (default).
	// Premium requests are weighted by model multiplier (e.g. opus 4.6 = 3x).
	CopilotDailyRequestLimit int `mapstructure:"copilot_daily_request_limit" yaml:"copilot_daily_request_limit,omitempty"`
	// CrucibleEnabled enables automatic Crucible orchestration for parent beads
	// that have children (blocks other beads). When true, the daemon detects
	// parent beads during polling and dispatches them through the Crucible
	// instead of the normal pipeline. Default: false.
	CrucibleEnabled bool `mapstructure:"crucible_enabled" yaml:"crucible_enabled"`
	// AutoMergeCrucibleChildren controls whether child PRs targeting a Crucible
	// feature branch are automatically merged (squash) after the pipeline
	// succeeds. Default: true.
	AutoMergeCrucibleChildren *bool `mapstructure:"auto_merge_crucible_children" yaml:"auto_merge_crucible_children,omitempty"`
}

// durationString returns the duration string, or omits zero values.
func durationString(d time.Duration) string {
	return d.String()
}

// MarshalYAML serialises SettingsConfig with time.Duration fields as
// human-readable strings (e.g. "30s", "5m0s") instead of nanosecond ints.
func (s SettingsConfig) MarshalYAML() (interface{}, error) {
	// Shadow struct with durations replaced by strings.
	type shadow struct {
		PollInterval             string   `yaml:"poll_interval"`
		SmithTimeout             string   `yaml:"smith_timeout"`
		MaxTotalSmiths           int      `yaml:"max_total_smiths"`
		MaxReviewAttempts        int      `yaml:"max_review_attempts"`
		MaxPipelineIterations    int      `yaml:"max_pipeline_iterations"`
		ClaudeFlags              []string `yaml:"claude_flags"`
		Providers                []string `yaml:"providers,omitempty"`
		RateLimitBackoff         string   `yaml:"rate_limit_backoff"`
		SmithProviders           []string `yaml:"smith_providers,omitempty"`
		SchematicEnabled         bool     `yaml:"schematic_enabled"`
		SchematicWordThreshold   int      `yaml:"schematic_word_threshold,omitempty"`
		BellowsInterval          string   `yaml:"bellows_interval"`
		DailyCostLimit           float64  `yaml:"daily_cost_limit,omitempty"`
		MaxCIFixAttempts         int      `yaml:"max_ci_fix_attempts"`
		MaxReviewFixAttempts     int      `yaml:"max_review_fix_attempts"`
		MaxRebaseAttempts        int      `yaml:"max_rebase_attempts"`
		MergeStrategy            string   `yaml:"merge_strategy,omitempty"`
		StaleInterval            string   `yaml:"stale_interval"`
		DepcheckInterval         string   `yaml:"depcheck_interval,omitempty"`
		DepcheckTimeout          string   `yaml:"depcheck_timeout,omitempty"`
		VulncheckInterval        string   `yaml:"vulncheck_interval,omitempty"`
		VulncheckTimeout         string   `yaml:"vulncheck_timeout,omitempty"`
		VulncheckEnabled         *bool    `yaml:"vulncheck_enabled,omitempty"`
		GoRaceDetection          bool     `yaml:"go_race_detection"`
		AutoLearnRules           bool     `yaml:"auto_learn_rules"`
		CopilotDailyRequestLimit int      `yaml:"copilot_daily_request_limit,omitempty"`
		CrucibleEnabled          bool     `yaml:"crucible_enabled"`
		AutoMergeCrucibleChildren *bool   `yaml:"auto_merge_crucible_children,omitempty"`
	}

	sh := shadow{
		PollInterval:              durationString(s.PollInterval),
		SmithTimeout:              durationString(s.SmithTimeout),
		MaxTotalSmiths:            s.MaxTotalSmiths,
		MaxReviewAttempts:         s.MaxReviewAttempts,
		MaxPipelineIterations:     s.MaxPipelineIterations,
		ClaudeFlags:               s.ClaudeFlags,
		Providers:                 s.Providers,
		RateLimitBackoff:          durationString(s.RateLimitBackoff),
		SmithProviders:            s.SmithProviders,
		SchematicEnabled:          s.SchematicEnabled,
		SchematicWordThreshold:    s.SchematicWordThreshold,
		BellowsInterval:           durationString(s.BellowsInterval),
		DailyCostLimit:            s.DailyCostLimit,
		MaxCIFixAttempts:          s.MaxCIFixAttempts,
		MaxReviewFixAttempts:      s.MaxReviewFixAttempts,
		MaxRebaseAttempts:         s.MaxRebaseAttempts,
		MergeStrategy:             s.MergeStrategy,
		StaleInterval:             durationString(s.StaleInterval),
		VulncheckEnabled:          s.VulncheckEnabled,
		GoRaceDetection:           s.GoRaceDetection,
		AutoLearnRules:            s.AutoLearnRules,
		CopilotDailyRequestLimit:  s.CopilotDailyRequestLimit,
		CrucibleEnabled:           s.CrucibleEnabled,
		AutoMergeCrucibleChildren: s.AutoMergeCrucibleChildren,
	}

	// Only include non-zero optional durations.
	if s.DepcheckInterval > 0 {
		sh.DepcheckInterval = durationString(s.DepcheckInterval)
	}
	if s.DepcheckTimeout > 0 {
		sh.DepcheckTimeout = durationString(s.DepcheckTimeout)
	}
	if s.VulncheckInterval > 0 {
		sh.VulncheckInterval = durationString(s.VulncheckInterval)
	}
	if s.VulncheckTimeout > 0 {
		sh.VulncheckTimeout = durationString(s.VulncheckTimeout)
	}

	return sh, nil
}

// IsVulncheckEnabled returns true unless vulncheck_enabled is explicitly false.
func (s SettingsConfig) IsVulncheckEnabled() bool {
	if s.VulncheckEnabled == nil {
		return true
	}
	return *s.VulncheckEnabled
}

// IsAutoMergeCrucibleChildren returns true unless auto_merge_crucible_children
// is explicitly false. Defaults to true.
func (s SettingsConfig) IsAutoMergeCrucibleChildren() bool {
	if s.AutoMergeCrucibleChildren == nil {
		return true
	}
	return *s.AutoMergeCrucibleChildren
}

// NotificationsConfig holds webhook and notification settings.
type NotificationsConfig struct {
	TeamsWebhookURL string `mapstructure:"teams_webhook_url" yaml:"teams_webhook_url,omitempty"`
	Enabled         bool   `mapstructure:"enabled" yaml:"enabled"`
	// Events to notify on. Empty = all. Options: pr_created, bead_failed, daily_cost, worker_done, bead_decomposed, release_published, pr_ready_to_merge.
	Events []string `mapstructure:"events" yaml:"events,omitempty"`
	// ReleaseWebhookURLs is a list of generic JSON webhook URLs that receive
	// a release_published payload when 'forge notify release' is called.
	// These receive a simple JSON object (not a Teams Adaptive Card) suitable
	// for custom dashboards or other receivers.
	ReleaseWebhookURLs []string `mapstructure:"release_webhook_urls" yaml:"release_webhook_urls,omitempty"`
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		Anvils: make(map[string]AnvilConfig),
		Settings: SettingsConfig{
			PollInterval:         5 * time.Minute,
			SmithTimeout:         30 * time.Minute,
			MaxTotalSmiths:         4,
			MaxReviewAttempts:      2,
			MaxPipelineIterations:  5,
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
	v.SetDefault("settings.max_pipeline_iterations", 5)
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
	v.SetDefault("settings.vulncheck_enabled", true)

	// Environment variable support: FORGE_SETTINGS_POLL_INTERVAL etc.
	// SetEnvKeyReplacer maps dotted config keys (settings.auto_learn_rules) to
	// underscore env vars (FORGE_SETTINGS_AUTO_LEARN_RULES).
	v.SetEnvPrefix("FORGE")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
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
	if c.Settings.MaxPipelineIterations < 1 {
		errs = append(errs, "settings.max_pipeline_iterations must be >= 1")
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

	if c.Settings.CopilotDailyRequestLimit < 0 {
		errs = append(errs, "settings.copilot_daily_request_limit must be >= 0 (0 = no limit)")
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
// It uses yaml.Marshal with yaml struct tags so that every config field
// is persisted automatically — no new field can be silently dropped.
func Save(cfg *Config, path string) error {
	// Ensure directory exists.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}

	return os.WriteFile(path, data, 0o644)
}
