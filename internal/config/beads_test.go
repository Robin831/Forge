package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_BeadsParent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "forge.yaml")
	content := `
anvils:
  heimdall:
    path: /some/path
    beads:
      default_parent: Heimdall-6tyq
      parents:
        depcheck: Heimdall-deps
  devinfra:
    path: /other/path
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o644))
	cfg, err := Load(cfgPath)
	require.NoError(t, err)

	assert.Equal(t, "Heimdall-deps", cfg.BeadParent("heimdall", BeadKindDepcheck), "a per-kind parent wins")
	assert.Equal(t, "Heimdall-6tyq", cfg.BeadParent("heimdall", BeadKindWicket), "other kinds fall back to default_parent")
	assert.Empty(t, cfg.BeadParent("devinfra", BeadKindDepcheck), "no beads config means no parent")
	assert.Empty(t, cfg.BeadParent("unknown", BeadKindDepcheck))
	var nilCfg *Config
	assert.Empty(t, nilCfg.BeadParent("heimdall", BeadKindDepcheck))
}

func TestValidate_BeadsParentsUnknownKind(t *testing.T) {
	cfg := Defaults()
	cfg.Anvils["heimdall"] = AnvilConfig{Path: "/p", Beads: BeadsConfig{Parents: map[string]string{"depchek": "Heimdall-6tyq"}}}
	errs := strings.Join(cfg.Validate(), "\n")
	assert.Contains(t, errs, `beads.parents has unknown kind "depchek"`)

	cfg.Anvils["heimdall"] = AnvilConfig{Path: "/p", Beads: BeadsConfig{Parents: map[string]string{BeadKindDepcheck: "Heimdall-6tyq"}}}
	assert.NotContains(t, strings.Join(cfg.Validate(), "\n"), "beads.parents")
}

func TestSave_RoundTrip_BeadsConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "forge.yaml")
	original := Defaults()
	original.Anvils["munin"] = AnvilConfig{Path: "/p", Beads: BeadsConfig{
		DefaultParent: "Fhi.Metadata-eeuey",
		Parents:       map[string]string{BeadKindFollowup: "Fhi.Metadata-abc"},
	}}
	require.NoError(t, Save(&original, cfgPath))

	loaded, err := Load(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, original.Anvils["munin"].Beads, loaded.Anvils["munin"].Beads)
}
