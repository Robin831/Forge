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
}

// SettingsConfig holds global operational settings.
type SettingsConfig struct {
	PollInterval  time.Duration `mapstructure:"poll_interval"`
	SmithTimeout  time.Duration `mapstructure:"smith_timeout"`
	MaxTotalSmiths int          `mapstructure:"max_total_smiths"`
	ClaudeFlags   []string      `mapstructure:"claude_flags"`
	// Providers is the ordered list of AI providers to try.
	// Each entry is a Kind string ("claude", "gemini") or "kind:command" pair.
	// When a provider signals a rate limit the next one in the list is tried.
	// Defaults to ["claude", "gemini"] when empty.
	Providers     []string      `mapstructure:"providers"`
}

// NotificationsConfig holds webhook and notification settings.
type NotificationsConfig struct {
	TeamsWebhookURL string `mapstructure:"teams_webhook_url"`
	Enabled         bool   `mapstructure:"enabled"`
	// Events to notify on. Empty = all. Options: pr_created, bead_failed, daily_cost, worker_done.
	Events []string `mapstructure:"events"`
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		Anvils: make(map[string]AnvilConfig),
		Settings: SettingsConfig{
			PollInterval:   5 * time.Minute,
			SmithTimeout:   30 * time.Minute,
			MaxTotalSmiths: 4,
			ClaudeFlags:    []string{},
			// No Providers default here — provider.FromConfig handles empty slice.
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
	v.SetDefault("settings.claude_flags", []string{})

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
	if c.Settings.PollInterval < 10*time.Second {
		errs = append(errs, "settings.poll_interval must be >= 10s")
	}
	if c.Settings.SmithTimeout < 1*time.Minute {
		errs = append(errs, "settings.smith_timeout must be >= 1m")
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
	v.Set("settings.claude_flags", cfg.Settings.ClaudeFlags)

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	return v.WriteConfigAs(path)
}
