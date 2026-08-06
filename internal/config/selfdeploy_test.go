package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelfDeployConfig_Defaults(t *testing.T) {
	var s SelfDeployConfig // zero value

	assert.Equal(t, "forge", s.ResolvedUnitName())
	assert.Equal(t, "main", s.ResolvedBranch())
	assert.Equal(t, "./cmd/forge", s.ResolvedBuildTarget())
	assert.Equal(t, "systemctl", s.ResolvedRestartCommand())
	assert.Equal(t, DefaultSelfDeployMaxDrainWait, s.ResolvedMaxDrainWait())
	// RestartArgs default to nil (a plain `systemctl restart <unit>`).
	assert.Nil(t, s.RestartArgs)
}

func TestSelfDeployConfig_RestartCommandOverride(t *testing.T) {
	// sudo-elevated restart for an unprivileged daemon user.
	s := SelfDeployConfig{RestartCommand: "sudo", RestartArgs: []string{"systemctl"}}
	assert.Equal(t, "sudo", s.ResolvedRestartCommand())
	assert.Equal(t, []string{"systemctl"}, s.RestartArgs)
}

func TestSelfDeployConfig_MaxDrainWaitOverride(t *testing.T) {
	assert.Equal(t, DefaultSelfDeployMaxDrainWait, SelfDeployConfig{MaxDrainWait: 0}.ResolvedMaxDrainWait(),
		"unset falls back to the default")
	assert.Equal(t, DefaultSelfDeployMaxDrainWait, SelfDeployConfig{MaxDrainWait: -1}.ResolvedMaxDrainWait(),
		"a negative value is rejected by Validate and falls back here")
	assert.Equal(t, 5*time.Minute, SelfDeployConfig{MaxDrainWait: 5 * time.Minute}.ResolvedMaxDrainWait())
}

// TestSelfDeployConfig_DrainTimeoutFallback pins the deprecated drain_timeout
// key: it still bounds the wait for configs written before the rename, but
// max_drain_wait wins when both are set.
func TestSelfDeployConfig_DrainTimeoutFallback(t *testing.T) {
	assert.Equal(t, DefaultSelfDeployMaxDrainWait, SelfDeployConfig{DrainTimeout: 0}.ResolvedMaxDrainWait())
	assert.Equal(t, 5*time.Minute, SelfDeployConfig{DrainTimeout: 5 * time.Minute}.ResolvedMaxDrainWait())
	assert.Equal(t, 5*time.Minute, SelfDeployConfig{DrainTimeout: 5 * time.Minute}.ResolvedDrainTimeout(),
		"the deprecated accessor resolves to the same value")
	assert.Equal(t, time.Minute, SelfDeployConfig{
		MaxDrainWait: time.Minute,
		DrainTimeout: time.Hour,
	}.ResolvedMaxDrainWait(), "max_drain_wait wins over the deprecated key")
}

// TestSelfDeployConfig_MaxDrainWaitParsedFromYAML verifies the new key is wired
// through the loader as a duration (not silently dropped).
func TestSelfDeployConfig_MaxDrainWaitParsedFromYAML(t *testing.T) {
	c, err := Load(writeConfig(t, `anvils:
  forge:
    path: /tmp/forge
self_deploy:
  enabled: true
  anvil: forge
  max_drain_wait: 45m
`))
	require.NoError(t, err)
	assert.Equal(t, 45*time.Minute, c.SelfDeploy.MaxDrainWait)
	assert.Equal(t, 45*time.Minute, c.SelfDeploy.ResolvedMaxDrainWait())
}

func TestSelfDeployConfig_Validate(t *testing.T) {
	// Enabled but no anvil -> error.
	c := &Config{SelfDeploy: SelfDeployConfig{Enabled: true}}
	assert.Contains(t, c.Validate(), "self_deploy.anvil is required when self_deploy.enabled is true")

	// Enabled with an anvil that is not configured -> error.
	c = &Config{SelfDeploy: SelfDeployConfig{Enabled: true, Anvil: "forge"}}
	assert.NotEmpty(t, c.Validate())

	// Enabled with a matching anvil -> no self_deploy errors.
	c = &Config{
		Anvils:     map[string]AnvilConfig{"forge": {Path: "/tmp/forge"}},
		SelfDeploy: SelfDeployConfig{Enabled: true, Anvil: "forge"},
	}
	for _, e := range c.Validate() {
		assert.NotContains(t, e, "self_deploy")
	}

	// Negative max drain wait -> error even when disabled.
	c = &Config{SelfDeploy: SelfDeployConfig{MaxDrainWait: -time.Second}}
	assert.Contains(t, c.Validate(), "self_deploy.max_drain_wait must not be negative (omit or set to 0 to use the default)")

	// The deprecated key is validated the same way.
	c = &Config{SelfDeploy: SelfDeployConfig{DrainTimeout: -time.Second}}
	assert.Contains(t, c.Validate(), "self_deploy.drain_timeout must not be negative (omit or set to 0 to use the default)")
}
