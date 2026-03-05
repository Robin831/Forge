package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
					MaxTotalSmiths: 4,
					PollInterval:   1 * time.Minute,
					SmithTimeout:   30 * time.Minute,
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
					MaxTotalSmiths: 0,
					PollInterval:   5 * time.Second,
					SmithTimeout:   30 * time.Second,
				},
			},
			expected: []string{
				"settings.max_total_smiths must be >= 1",
				"settings.poll_interval must be >= 10s",
				"settings.smith_timeout must be >= 1m",
			},
		},
		{
			name: "invalid anvil path",
			cfg: Config{
				Settings: SettingsConfig{
					MaxTotalSmiths: 4,
					PollInterval:   1 * time.Minute,
					SmithTimeout:   30 * time.Minute,
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
					MaxTotalSmiths: 4,
					PollInterval:   1 * time.Minute,
					SmithTimeout:   30 * time.Minute,
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
					MaxTotalSmiths: 4,
					PollInterval:   1 * time.Minute,
					SmithTimeout:   30 * time.Minute,
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
					MaxTotalSmiths: 4,
					PollInterval:   1 * time.Minute,
					SmithTimeout:   30 * time.Minute,
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
				"anvil \"test\": auto_dispatch_min_priority must be 1-4 when auto_dispatch is \"priority\" (0 would only dispatch critical beads; set explicitly if intentional)",
				"anvil \"test2\": auto_dispatch_min_priority must be 1-4 when auto_dispatch is \"priority\" (0 would only dispatch critical beads; set explicitly if intentional)",
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
