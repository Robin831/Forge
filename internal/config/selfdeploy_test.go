package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSelfDeployConfig_Defaults(t *testing.T) {
	var s SelfDeployConfig // zero value

	assert.Equal(t, "forge", s.ResolvedUnitName())
	assert.Equal(t, "main", s.ResolvedBranch())
	assert.Equal(t, "./cmd/forge", s.ResolvedBuildTarget())
	assert.Equal(t, "systemctl", s.ResolvedRestartCommand())
	assert.Equal(t, DefaultSelfDeployDrainTimeout, s.ResolvedDrainTimeout())
	// RestartArgs default to nil (a plain `systemctl restart <unit>`).
	assert.Nil(t, s.RestartArgs)
}

func TestSelfDeployConfig_RestartCommandOverride(t *testing.T) {
	// sudo-elevated restart for an unprivileged daemon user.
	s := SelfDeployConfig{RestartCommand: "sudo", RestartArgs: []string{"systemctl"}}
	assert.Equal(t, "sudo", s.ResolvedRestartCommand())
	assert.Equal(t, []string{"systemctl"}, s.RestartArgs)
}

func TestSelfDeployConfig_DrainTimeoutOverride(t *testing.T) {
	assert.Equal(t, DefaultSelfDeployDrainTimeout, SelfDeployConfig{DrainTimeout: 0}.ResolvedDrainTimeout())
	assert.Equal(t, DefaultSelfDeployDrainTimeout, SelfDeployConfig{DrainTimeout: -1}.ResolvedDrainTimeout())
	assert.Equal(t, 5*time.Minute, SelfDeployConfig{DrainTimeout: 5 * time.Minute}.ResolvedDrainTimeout())
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

	// Negative drain timeout -> error even when disabled.
	c = &Config{SelfDeploy: SelfDeployConfig{DrainTimeout: -time.Second}}
	assert.Contains(t, c.Validate(), "self_deploy.drain_timeout must not be negative (omit or set to 0 to use the default)")
}
