package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoad_StageProvidersLowerCasesKeysAndAnvilNames pins the lower-casing
// loadDottedKeyTablesFromYAML does to stay compatible with the viper decode
// stage_providers used to go through. viper lower-cases every key, so a
// mixed-case anvil name (`Munin:`) is stored as `munin` and a mixed-case stage
// key (`Smith:`) was matched as `smith`. Without the anvil ToLower the
// per-anvil block is dropped silently by the lookup miss; without
// lowerStageKeys the stage keys stop matching ProvidersForStage's lower-case
// lookups. Either way the anvil falls back to the global chain with no error.
func TestLoad_StageProvidersLowerCasesKeysAndAnvilNames(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "forge.yaml")
	content := `
settings:
  stage_providers:
    Smith: [claude]
    Assay.Conventions: [gemini, claude]
anvils:
  Munin:
    path: /some/munin
    stage_providers:
      Warden: [copilot]
      Assay.Logic: [gemini]
  forge:
    path: /some/forge
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))

	cfg, err := Load(cfgPath)
	require.NoError(t, err)

	assert.Equal(t, map[string][]string{
		"smith":             {"claude"},
		"assay.conventions": {"gemini", "claude"},
	}, cfg.Settings.StageProviders, "global stage keys must load lower-cased")

	munin, ok := cfg.Anvils["munin"]
	require.True(t, ok, "viper stores the Munin anvil as munin; anvils = %v", cfg.Anvils)
	assert.Equal(t, "/some/munin", munin.Path)
	assert.Equal(t, map[string][]string{
		"warden":      {"copilot"},
		"assay.logic": {"gemini"},
	}, munin.StageProviders, "per-anvil stage_providers under a mixed-case anvil name must not be dropped")

	assert.Nil(t, cfg.Anvils["forge"].StageProviders)

	assert.Equal(t, []string{"claude"}, cfg.Settings.ProvidersForStage("smith"))
	assert.Equal(t, []string{"copilot"}, ProvidersForStageWithAnvil(cfg.Settings, &munin, "warden"))
	assert.Equal(t, []string{"gemini"}, ProvidersForStageWithAnvil(cfg.Settings, &munin, "assay.logic"))
	assert.Equal(t, []string{"gemini", "claude"}, ProvidersForStageWithAnvil(cfg.Settings, &munin, "assay.conventions"))
}
