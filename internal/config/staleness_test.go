package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/Robin831/Forge/internal/state"
)

// TestStalenessSettingsSurviveAYAMLRoundTrip is the one that catches the silent
// failure. SettingsConfig marshals through a hand-written shadow struct, so a
// field added to SettingsConfig but not to the shadow is dropped on every
// config write-back — no error, the setting simply disappears from the file the
// next time Forge rewrites it.
func TestStalenessSettingsSurviveAYAMLRoundTrip(t *testing.T) {
	off := false
	original := SettingsConfig{
		StalenessCheck:      &off,
		StalenessMultiplier: 5,
	}

	data, err := yaml.Marshal(original)
	require.NoError(t, err)

	var got SettingsConfig
	require.NoError(t, yaml.Unmarshal(data, &got))

	require.NotNil(t, got.StalenessCheck, "staleness_check was dropped by the shadow struct")
	assert.False(t, *got.StalenessCheck)
	assert.Equal(t, float64(5), got.StalenessMultiplier, "staleness_multiplier was dropped by the shadow struct")
}

// Defaults must agree with the constant the judgement falls back to, or a
// deployment that never configured the setting is judged against a different
// number from one that wrote the default out explicitly.
func TestStalenessDefaults(t *testing.T) {
	d := Defaults()

	assert.True(t, d.Settings.IsStalenessCheckEnabled(), "the backstop is opt-out, not opt-in")
	assert.Equal(t, float64(state.DefaultStalenessMultiplier), d.Settings.StalenessMultiplier)
}

// The tri-state resolver: unset means on, and only an explicit false turns it
// off. An opt-out that defaulted to off would ship the whole feature dormant.
func TestIsStalenessCheckEnabled(t *testing.T) {
	var unset SettingsConfig
	assert.True(t, unset.IsStalenessCheckEnabled())

	on, off := true, false
	assert.True(t, SettingsConfig{StalenessCheck: &on}.IsStalenessCheckEnabled())
	assert.False(t, SettingsConfig{StalenessCheck: &off}.IsStalenessCheckEnabled())
}

// A multiplier below 1 would report every checker before its own interval had
// even elapsed, so every anvil would be permanently stale. Rejected rather than
// clamped, so a setting that does not mean what its author thought is said out
// loud instead of quietly corrected.
func TestValidateStalenessMultiplier(t *testing.T) {
	cfg := Defaults()

	cfg.Settings.StalenessMultiplier = -1
	assert.NotEmpty(t, cfg.Validate(), "a negative multiplier must be rejected")

	cfg.Settings.StalenessMultiplier = 0.5
	assert.NotEmpty(t, cfg.Validate(), "a multiplier below 1 must be rejected")

	// Zero is the unset zero value and falls back to the default, so it is not
	// an error — this is the trap the repo's other numeric settings document.
	cfg.Settings.StalenessMultiplier = 0
	assert.Empty(t, cfg.Validate())

	cfg.Settings.StalenessMultiplier = 3
	assert.Empty(t, cfg.Validate())
}
