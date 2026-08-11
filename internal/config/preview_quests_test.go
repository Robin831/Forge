package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPreviewQuestsDefaultsToOff pins the opt-in: an anvil that says nothing
// about preview_quests runs no quests against its previews, even with previews
// otherwise fully enabled.
func TestPreviewQuestsDefaultsToOff(t *testing.T) {
	cfg, err := Load(writeConfig(t, `anvils:
  quiet:
    path: /tmp/quiet
settings:
  preview_enabled: true
`))
	require.NoError(t, err)

	assert.False(t, cfg.Anvils["quiet"].PreviewQuests)
	assert.False(t, cfg.IsPreviewQuestsEnabledForAnvil("quiet"))
	assert.False(t, cfg.AnvilSettingsMap()["quiet"].PreviewQuests)
	assert.Empty(t, cfg.Validate())
}

// TestIsPreviewQuestsEnabledForAnvil covers the resolution the QuestGiver entry
// point relies on, including the preview gates it folds in.
func TestIsPreviewQuestsEnabledForAnvil(t *testing.T) {
	optedOut := false
	base := func() *Config {
		cfg := Defaults()
		cfg.Settings.PreviewEnabled = true
		cfg.Anvils = map[string]AnvilConfig{
			"quests":  {Path: "/tmp/quests", PreviewQuests: true},
			"plain":   {Path: "/tmp/plain"},
			"noquest": {Path: "/tmp/noquest", PreviewQuests: false},
			"optout":  {Path: "/tmp/optout", PreviewQuests: true, PreviewEnabled: &optedOut},
		}
		return &cfg
	}

	cfg := base()
	assert.True(t, cfg.IsPreviewQuestsEnabledForAnvil("quests"))
	assert.False(t, cfg.IsPreviewQuestsEnabledForAnvil("plain"))
	assert.False(t, cfg.IsPreviewQuestsEnabledForAnvil("noquest"))
	assert.False(t, cfg.IsPreviewQuestsEnabledForAnvil("optout"),
		"an anvil that cannot run previews has nothing to run quests against")
	assert.False(t, cfg.IsPreviewQuestsEnabledForAnvil("unknown-anvil"))

	// The global gate wins over any per-anvil opt-in, which is what a
	// hot-reloaded config turning previews off must mean.
	off := base()
	off.Settings.PreviewEnabled = false
	assert.False(t, off.IsPreviewQuestsEnabledForAnvil("quests"))

	// A nil config must answer rather than panic.
	var nilCfg *Config
	assert.False(t, nilCfg.IsPreviewQuestsEnabledForAnvil("quests"))
}

// TestPreviewQuestsValidation rejects an opt-in that can never do anything: no
// previews for the anvil means no preview to run quests against.
func TestPreviewQuestsValidation(t *testing.T) {
	globalOff := Defaults()
	globalOff.Settings.PreviewEnabled = false
	globalOff.Anvils["api"] = AnvilConfig{Path: "/tmp/api", PreviewQuests: true}
	assert.Contains(t, globalOff.Validate(),
		`anvil "api": preview_quests requires settings.preview_enabled: true`)

	optedOut := false
	anvilOff := Defaults()
	anvilOff.Settings.PreviewEnabled = true
	anvilOff.Anvils["api"] = AnvilConfig{Path: "/tmp/api", PreviewQuests: true, PreviewEnabled: &optedOut}
	assert.Contains(t, anvilOff.Validate(),
		`anvil "api": preview_quests requires preview_enabled for this anvil (it is set to false)`)

	ok := Defaults()
	ok.Settings.PreviewEnabled = true
	ok.Anvils["api"] = AnvilConfig{Path: "/tmp/api", PreviewQuests: true}
	assert.Empty(t, ok.Validate())

	// The zero value stays valid with previews off, so existing configs are
	// untouched by the new key.
	zero := Defaults()
	zero.Anvils["api"] = AnvilConfig{Path: "/tmp/api"}
	assert.Empty(t, zero.Validate())
}

// TestPreviewQuestsRoundTrip keeps the key readable and writable through the
// config API's load → save → load path.
func TestPreviewQuestsRoundTrip(t *testing.T) {
	cfg, err := Load(writeConfig(t, `anvils:
  api:
    path: /tmp/api
    preview_quests: true
settings:
  preview_enabled: true
`))
	require.NoError(t, err)
	require.True(t, cfg.Anvils["api"].PreviewQuests)
	assert.True(t, cfg.AnvilSettingsMap()["api"].PreviewQuests)
	assert.Empty(t, cfg.Validate())

	path := filepath.Join(t.TempDir(), "forge.yaml")
	require.NoError(t, Save(cfg, path))
	reloaded, err := Load(path)
	require.NoError(t, err)
	assert.True(t, reloaded.Anvils["api"].PreviewQuests)
	assert.True(t, reloaded.IsPreviewQuestsEnabledForAnvil("api"))
}

func TestAssayMaxTurnsPerPass(t *testing.T) {
	var a AssayConfig
	if got := a.GetMaxTurnsPerPass(); got != 0 {
		t.Fatalf("unset should be 0 (engine default), got %d", got)
	}
	v := 24
	a.MaxTurnsPerPass = &v
	if got := a.GetMaxTurnsPerPass(); got != 24 {
		t.Fatalf("want 24, got %d", got)
	}
}
