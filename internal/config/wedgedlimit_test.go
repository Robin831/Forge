package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestResolvedWedgedLimitPolls pins the tri-state 0 has to carry here: it is
// the field's zero value, so an unset setting and an explicit `0` are the same
// number by the time either reaches this method — reading that as "warn on the
// first saturated poll" would make every deployment that never configured the
// key noisy, so it takes the default and a NEGATIVE value is the off switch.
func TestResolvedWedgedLimitPolls(t *testing.T) {
	tests := []struct {
		name    string
		set     int
		want    int
		enabled bool
	}{
		{"unset takes the default", 0, DefaultWedgedLimitPolls, true},
		{"an explicit value is used as-is", 7, 7, true},
		{"one poll is a legal, tight threshold", 1, 1, true},
		{"a negative value disables the check", -1, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, enabled := SettingsConfig{WedgedLimitPolls: tt.set}.ResolvedWedgedLimitPolls()
			assert.Equal(t, tt.enabled, enabled)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestDefaultConfigSetsWedgedLimitPolls keeps the shipped default and the
// resolver's fallback from drifting apart: a default written into the config
// and a different one applied when the key is absent would mean two thresholds
// depending on which path a deployment took.
func TestDefaultConfigSetsWedgedLimitPolls(t *testing.T) {
	cfg := Defaults()
	assert.Equal(t, DefaultWedgedLimitPolls, cfg.Settings.WedgedLimitPolls)
	got, enabled := cfg.Settings.ResolvedWedgedLimitPolls()
	assert.True(t, enabled)
	assert.Equal(t, DefaultWedgedLimitPolls, got)
}
