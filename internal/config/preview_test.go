package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeConfig writes a forge.yaml into a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forge.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

func TestPreviewSettingsDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "anvils: {}\n"))
	require.NoError(t, err)

	assert.False(t, cfg.Settings.PreviewEnabled, "previews are opt-in")
	assert.Equal(t, DefaultPreviewMaxConcurrent, cfg.Settings.PreviewMaxConcurrent)
	assert.False(t, cfg.Settings.PreviewEvictLRU, "a full box rejects rather than evicting by default")
	assert.Equal(t, DefaultPreviewIdleTimeout, cfg.Settings.PreviewIdleTimeout)
	assert.Equal(t, DefaultPreviewPortRange, cfg.Settings.PreviewPortRange)
	assert.Equal(t, DefaultPreviewBindHost, cfg.Settings.PreviewBindHost)
	assert.Empty(t, cfg.Settings.PreviewPublicHost)
	assert.Empty(t, cfg.Settings.PreviewProxyBase, "host-based routing is off unless a base is configured")
	assert.False(t, cfg.Settings.IsPreviewProxyEnabled())

	lo, hi, err := cfg.Settings.PreviewPortRangeBounds()
	require.NoError(t, err)
	assert.Equal(t, 42000, lo)
	assert.Equal(t, 42999, hi)

	assert.Empty(t, cfg.Validate())
}

func TestPreviewSettingsParse(t *testing.T) {
	cfg, err := Load(writeConfig(t, `settings:
  preview_enabled: true
  preview_max_concurrent: 4
  preview_evict_lru: true
  preview_idle_timeout: 45m
  preview_port_range: "43000-43100"
  preview_bind_host: "0.0.0.0"
  preview_public_host: "devbox.local"
  preview_proxy_base: "Preview.Example.Test."
`))
	require.NoError(t, err)

	assert.True(t, cfg.Settings.PreviewEnabled)
	assert.Equal(t, "preview.example.test", cfg.Settings.ResolvedPreviewProxyBase(),
		"the base is lowercased and loses its trailing root dot")
	assert.True(t, cfg.Settings.IsPreviewProxyEnabled())
	assert.Equal(t, 4, cfg.Settings.PreviewMaxConcurrent)
	assert.True(t, cfg.Settings.PreviewEvictLRU)
	assert.Equal(t, 45*time.Minute, cfg.Settings.PreviewIdleTimeout)
	assert.Equal(t, "0.0.0.0", cfg.Settings.ResolvedPreviewBindHost())
	assert.Equal(t, "devbox.local", cfg.Settings.ResolvedPreviewPublicHost())
	assert.Empty(t, cfg.Validate())

	lo, hi, err := cfg.Settings.PreviewPortRangeBounds()
	require.NoError(t, err)
	assert.Equal(t, 43000, lo)
	assert.Equal(t, 43100, hi)
}

func TestPreviewSettingsResolvers(t *testing.T) {
	var zero SettingsConfig
	assert.Equal(t, DefaultPreviewMaxConcurrent, zero.ResolvedPreviewMaxConcurrent())
	assert.Equal(t, DefaultPreviewBindHost, zero.ResolvedPreviewBindHost())
	// The public host falls back to the bind host so links are never hostless.
	assert.Equal(t, DefaultPreviewBindHost, zero.ResolvedPreviewPublicHost())

	s := SettingsConfig{PreviewMaxConcurrent: 3, PreviewBindHost: "0.0.0.0"}
	assert.Equal(t, 3, s.ResolvedPreviewMaxConcurrent())
	assert.Equal(t, "0.0.0.0", s.ResolvedPreviewPublicHost())
}

func TestParsePortRange(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		lo, hi  int
		wantErr string
	}{
		{name: "valid", raw: "42000-42999", lo: 42000, hi: 42999},
		{name: "whitespace tolerated", raw: " 42000 - 42999 ", lo: 42000, hi: 42999},
		{name: "missing separator", raw: "42000", wantErr: `must be in the form "min-max"`},
		{name: "non-numeric lower bound", raw: "abc-42999", wantErr: "invalid lower bound"},
		{name: "non-numeric upper bound", raw: "42000-abc", wantErr: "invalid upper bound"},
		{name: "privileged lower bound", raw: "80-8080", wantErr: "must stay within 1024-65535"},
		{name: "upper bound out of range", raw: "42000-70000", wantErr: "must stay within 1024-65535"},
		{name: "inverted", raw: "42999-42000", wantErr: "lower bound must be less than upper bound"},
		{name: "equal bounds", raw: "42000-42000", wantErr: "lower bound must be less than upper bound"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lo, hi, err := ParsePortRange(tc.raw)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.lo, lo)
			assert.Equal(t, tc.hi, hi)
		})
	}
}

func TestNormalizePreviewProxyBase(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "empty means off", raw: ""},
		{name: "whitespace only means off", raw: "   "},
		{name: "plain name", raw: "preview.example.test", want: "preview.example.test"},
		{name: "lowercased", raw: "Preview.Example.TEST", want: "preview.example.test"},
		{name: "padded", raw: "  preview.example.test  ", want: "preview.example.test"},
		{name: "trailing root dot dropped", raw: "preview.example.test.", want: "preview.example.test"},
		{name: "single label allowed", raw: "localtest", want: "localtest"},
		{name: "hyphens and digits", raw: "pr-1.dev-box.test", want: "pr-1.dev-box.test"},

		{name: "leading dot", raw: ".foo.test", wantErr: "must not start with a dot"},
		{name: "scheme", raw: "https://preview.test", wantErr: "without a scheme"},
		{name: "port", raw: "preview.test:8080", wantErr: "without a port"},
		{name: "path", raw: "preview.test/previews", wantErr: "without a path"},
		{name: "empty label", raw: "a..b", wantErr: "has an empty label"},
		{name: "leading hyphen in a label", raw: "-bad.test", wantErr: "must not start or end with a hyphen"},
		{name: "trailing hyphen in a label", raw: "bad-.test", wantErr: "must not start or end with a hyphen"},
		{name: "underscore", raw: "bad_label.test", wantErr: "not allowed in a DNS name"},
		{name: "over-long label", raw: strings.Repeat("a", 64) + ".test", wantErr: "is longer than 63 characters"},
		{
			name:    "over-long name",
			raw:     strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 63)+".", 5), "."),
			wantErr: "is longer than 253 characters",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePreviewProxyBase(tc.raw)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)

			// The resolver is the same normalization, and an invalid base
			// resolves to "" (feature off) rather than to a broken host.
			s := SettingsConfig{PreviewProxyBase: tc.raw}
			assert.Equal(t, tc.want, s.ResolvedPreviewProxyBase())
			assert.Equal(t, tc.want != "", s.IsPreviewProxyEnabled())
		})
	}

	// An invalid base never leaks onto the request path: Validate rejects it at
	// load time and the resolver reports the feature as off.
	bad := SettingsConfig{PreviewProxyBase: ".foo.test"}
	assert.Empty(t, bad.ResolvedPreviewProxyBase())
	assert.False(t, bad.IsPreviewProxyEnabled())
}

func TestPreviewValidation(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Config)
		expected string
	}{
		{
			name:     "negative max concurrent",
			mutate:   func(c *Config) { c.Settings.PreviewMaxConcurrent = -1 },
			expected: "settings.preview_max_concurrent must not be negative (omit or set to 0 to use the default)",
		},
		{
			name:     "negative idle timeout",
			mutate:   func(c *Config) { c.Settings.PreviewIdleTimeout = -time.Minute },
			expected: "settings.preview_idle_timeout must not be negative (set to 0 to disable the idle reaper)",
		},
		{
			name:     "idle timeout below the minimum",
			mutate:   func(c *Config) { c.Settings.PreviewIdleTimeout = 10 * time.Second },
			expected: "settings.preview_idle_timeout must be >= 1m0s when enabled (or 0 to disable)",
		},
		{
			name:     "malformed port range",
			mutate:   func(c *Config) { c.Settings.PreviewPortRange = "42000" },
			expected: `settings.preview_port_range: port range "42000" must be in the form "min-max" (e.g. "42000-42999")`,
		},
		{
			name:     "proxy base with a leading dot",
			mutate:   func(c *Config) { c.Settings.PreviewProxyBase = ".foo.test" },
			expected: `settings.preview_proxy_base: ".foo.test" must not start with a dot`,
		},
		{
			name:   "proxy base with an over-long label",
			mutate: func(c *Config) { c.Settings.PreviewProxyBase = strings.Repeat("a", 64) + ".test" },
			expected: fmt.Sprintf(`settings.preview_proxy_base: %q: label %q is longer than 63 characters`,
				strings.Repeat("a", 64)+".test", strings.Repeat("a", 64)),
		},
		{
			name:     "proxy base with a port",
			mutate:   func(c *Config) { c.Settings.PreviewProxyBase = "preview.test:8080" },
			expected: `settings.preview_proxy_base: "preview.test:8080" must be a bare DNS name without a port`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			tc.mutate(&cfg)
			assert.Contains(t, cfg.Validate(), tc.expected)
		})
	}

	t.Run("zero idle timeout disables the reaper without error", func(t *testing.T) {
		cfg := Defaults()
		cfg.Settings.PreviewIdleTimeout = 0
		assert.Empty(t, cfg.Validate())
	})

	t.Run("a valid proxy base validates", func(t *testing.T) {
		for _, base := range []string{"", "preview.example.test", "Preview.Example.Test.", "localtest"} {
			cfg := Defaults()
			cfg.Settings.PreviewProxyBase = base
			assert.Empty(t, cfg.Validate(), "preview_proxy_base %q must be accepted", base)
		}
	})
}

func TestPreviewEnabledPerAnvilTriState(t *testing.T) {
	cfg, err := Load(writeConfig(t, `anvils:
  optin:
    path: /tmp/optin
    preview_enabled: true
  optout:
    path: /tmp/optout
    preview_enabled: false
  inherit:
    path: /tmp/inherit
settings:
  preview_enabled: true
`))
	require.NoError(t, err)

	require.NotNil(t, cfg.Anvils["optin"].PreviewEnabled)
	assert.True(t, *cfg.Anvils["optin"].PreviewEnabled)
	require.NotNil(t, cfg.Anvils["optout"].PreviewEnabled)
	assert.False(t, *cfg.Anvils["optout"].PreviewEnabled)
	assert.Nil(t, cfg.Anvils["inherit"].PreviewEnabled, "no override means inherit the global setting")

	assert.True(t, cfg.IsPreviewEnabledForAnvil("optin"))
	assert.False(t, cfg.IsPreviewEnabledForAnvil("optout"))
	assert.True(t, cfg.IsPreviewEnabledForAnvil("inherit"))
	assert.True(t, cfg.IsPreviewEnabledForAnvil("unknown-anvil"), "an unknown anvil inherits the global setting")

	// The global gate wins over any per-anvil opt-in.
	cfg.Settings.PreviewEnabled = false
	assert.False(t, cfg.IsPreviewEnabledForAnvil("optin"))
	assert.False(t, cfg.IsPreviewEnabledForAnvil("inherit"))
}

func TestAnvilSettingsMapCarriesPreviewTriState(t *testing.T) {
	enabled := true
	cfg := Defaults()
	cfg.Anvils["set"] = AnvilConfig{Path: "/tmp/set", PreviewEnabled: &enabled}
	cfg.Anvils["unset"] = AnvilConfig{Path: "/tmp/unset"}

	settings := cfg.AnvilSettingsMap()
	require.NotNil(t, settings["set"].PreviewEnabled)
	assert.True(t, *settings["set"].PreviewEnabled)
	assert.Nil(t, settings["unset"].PreviewEnabled)
	// Deep copy: mutating the projection must not reach back into the config.
	*settings["set"].PreviewEnabled = false
	assert.True(t, *cfg.Anvils["set"].PreviewEnabled)
}

func TestSave_RoundTrip_PreviewSettings(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "forge.yaml")

	original := Defaults()
	original.Settings.PreviewEnabled = true
	original.Settings.PreviewMaxConcurrent = 3
	original.Settings.PreviewEvictLRU = true
	original.Settings.PreviewIdleTimeout = 90 * time.Minute
	original.Settings.PreviewPortRange = "43000-43999"
	original.Settings.PreviewBindHost = "0.0.0.0"
	original.Settings.PreviewPublicHost = "devbox.local"
	original.Settings.PreviewProxyBase = "preview.example.test"

	require.NoError(t, Save(&original, cfgPath))

	loaded, err := Load(cfgPath)
	require.NoError(t, err)
	assert.True(t, loaded.Settings.PreviewEnabled)
	assert.Equal(t, 3, loaded.Settings.PreviewMaxConcurrent)
	assert.True(t, loaded.Settings.PreviewEvictLRU)
	assert.Equal(t, 90*time.Minute, loaded.Settings.PreviewIdleTimeout)
	assert.Equal(t, "43000-43999", loaded.Settings.PreviewPortRange)
	assert.Equal(t, "0.0.0.0", loaded.Settings.PreviewBindHost)
	assert.Equal(t, "devbox.local", loaded.Settings.PreviewPublicHost)
	assert.Equal(t, "preview.example.test", loaded.Settings.PreviewProxyBase)
}

// TestPreviewProxyBaseEnvOverride pins the FORGE_ override convention for the
// new key: an operator can point a box at a different base without editing the
// file the daemon hot-reloads.
func TestPreviewProxyBaseEnvOverride(t *testing.T) {
	t.Setenv("FORGE_SETTINGS_PREVIEW_PROXY_BASE", "env.example.test")

	cfg, err := Load(writeConfig(t, `settings:
  preview_enabled: true
  preview_proxy_base: "file.example.test"
`))
	require.NoError(t, err)
	assert.Equal(t, "env.example.test", cfg.Settings.ResolvedPreviewProxyBase(),
		"the environment overrides the configured base")
}

func TestSave_RoundTrip_PreviewIdleTimeoutZero(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "forge.yaml")

	original := Defaults()
	original.Settings.PreviewIdleTimeout = 0

	require.NoError(t, Save(&original, cfgPath))

	loaded, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Zero(t, loaded.Settings.PreviewIdleTimeout, "an explicit 0 must survive a save/load round trip")
}

// TestPreviewAutoDefaultsToOff pins the opt-in: an anvil that says nothing
// about preview_auto starts no previews on its own, even with previews
// otherwise fully enabled.
func TestPreviewAutoDefaultsToOff(t *testing.T) {
	cfg, err := Load(writeConfig(t, `anvils:
  quiet:
    path: /tmp/quiet
settings:
  preview_enabled: true
`))
	require.NoError(t, err)

	assert.Empty(t, cfg.Anvils["quiet"].PreviewAuto)
	assert.Equal(t, PreviewAutoOff, cfg.PreviewAutoForAnvil("quiet"))
	assert.False(t, cfg.IsPreviewAutoReadyToMerge("quiet"))
	assert.Empty(t, cfg.Validate())
}

// TestPreviewAutoForAnvil covers the resolution the ready-to-merge handler
// relies on, including the two gates it folds in: previews must be enabled
// globally and the anvil must not have opted out.
func TestPreviewAutoForAnvil(t *testing.T) {
	optedOut := false
	base := func() *Config {
		cfg := Defaults()
		cfg.Settings.PreviewEnabled = true
		cfg.Anvils = map[string]AnvilConfig{
			"auto":     {Path: "/tmp/auto", PreviewAuto: PreviewAutoReadyToMerge},
			"messy":    {Path: "/tmp/messy", PreviewAuto: "  Ready_To_Merge  "},
			"explicit": {Path: "/tmp/explicit", PreviewAuto: PreviewAutoOff},
			"inherit":  {Path: "/tmp/inherit"},
			"optout":   {Path: "/tmp/optout", PreviewAuto: PreviewAutoReadyToMerge, PreviewEnabled: &optedOut},
			"bogus":    {Path: "/tmp/bogus", PreviewAuto: "on-every-poll"},
		}
		return &cfg
	}

	cfg := base()
	assert.Equal(t, PreviewAutoReadyToMerge, cfg.PreviewAutoForAnvil("auto"))
	assert.Equal(t, PreviewAutoReadyToMerge, cfg.PreviewAutoForAnvil("messy"), "case and padding are normalized")
	assert.Equal(t, PreviewAutoOff, cfg.PreviewAutoForAnvil("explicit"))
	assert.Equal(t, PreviewAutoOff, cfg.PreviewAutoForAnvil("inherit"))
	assert.Equal(t, PreviewAutoOff, cfg.PreviewAutoForAnvil("optout"),
		"an anvil that cannot run previews cannot auto-start them either")
	assert.Equal(t, PreviewAutoOff, cfg.PreviewAutoForAnvil("bogus"),
		"an unrecognized mode must not be read as an opt-in")
	assert.Equal(t, PreviewAutoOff, cfg.PreviewAutoForAnvil("unknown-anvil"))
	assert.True(t, cfg.IsPreviewAutoReadyToMerge("auto"))

	// The global gate wins over any per-anvil opt-in.
	off := base()
	off.Settings.PreviewEnabled = false
	assert.Equal(t, PreviewAutoOff, off.PreviewAutoForAnvil("auto"))
	assert.False(t, off.IsPreviewAutoReadyToMerge("auto"))

	// A nil config must answer rather than panic — hot reload can hand a
	// handler a config before one is loaded.
	var nilCfg *Config
	assert.Equal(t, PreviewAutoOff, nilCfg.PreviewAutoForAnvil("auto"))
	assert.False(t, nilCfg.IsPreviewAutoReadyToMerge("auto"))
}

// TestPreviewAutoValidation rejects a misspelled mode at load time, so the
// silent-off fallback in PreviewAutoForAnvil is a safety net rather than the
// only thing standing between a typo and "previews never start".
func TestPreviewAutoValidation(t *testing.T) {
	cfg := Defaults()
	cfg.Anvils["bad"] = AnvilConfig{Path: "/tmp/bad", PreviewAuto: "readytomerge"}
	assert.Contains(t, cfg.Validate(),
		`anvil "bad": invalid preview_auto "readytomerge" (must be off|ready_to_merge)`)

	for _, ok := range []string{"", PreviewAutoOff, PreviewAutoReadyToMerge, "READY_TO_MERGE"} {
		valid := Defaults()
		valid.Anvils["good"] = AnvilConfig{Path: "/tmp/good", PreviewAuto: ok}
		assert.Empty(t, valid.Validate(), "preview_auto %q must be accepted", ok)
	}
}

// TestPreviewAutoRoundTrip keeps the key readable and writable through the
// config API's load → save → load path.
func TestPreviewAutoRoundTrip(t *testing.T) {
	cfg, err := Load(writeConfig(t, `anvils:
  api:
    path: /tmp/api
    preview_auto: ready_to_merge
settings:
  preview_enabled: true
`))
	require.NoError(t, err)
	require.Equal(t, PreviewAutoReadyToMerge, cfg.Anvils["api"].PreviewAuto)
	assert.Equal(t, PreviewAutoReadyToMerge, cfg.AnvilSettingsMap()["api"].PreviewAuto)

	path := filepath.Join(t.TempDir(), "forge.yaml")
	require.NoError(t, Save(cfg, path))
	reloaded, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, PreviewAutoReadyToMerge, reloaded.Anvils["api"].PreviewAuto)
	assert.True(t, reloaded.IsPreviewAutoReadyToMerge("api"))
}
