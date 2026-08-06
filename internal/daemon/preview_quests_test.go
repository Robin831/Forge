package daemon

import (
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/stretchr/testify/require"
)

// TestPreviewQuestAnvils_Gating covers the set QuestGiver's preview run path is
// handed: the per-anvil opt-in plus the preview gates it depends on.
func TestPreviewQuestAnvils_Gating(t *testing.T) {
	cfg := &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"quests":   {Path: "/tmp/quests", PreviewQuests: true},
			"plain":    {Path: "/tmp/plain"},
			"optedOut": {Path: "/tmp/optedout", PreviewQuests: true, PreviewEnabled: boolPtr(false)},
			"pathless": {Path: "", PreviewQuests: true},
		},
		Settings: config.SettingsConfig{PreviewEnabled: true},
	}
	require.Equal(t, map[string]string{"quests": "/tmp/quests"}, previewQuestAnvils(cfg))

	// The global gate wins, and a nil config answers rather than panics.
	cfg.Settings.PreviewEnabled = false
	require.Empty(t, previewQuestAnvils(cfg))
	require.Empty(t, previewQuestAnvils(nil))
}

// TestShaMatches pins how a PR head (often abbreviated) is matched against the
// full SHA a preview checkout reports.
func TestShaMatches(t *testing.T) {
	const full = "abcdef1234567890abcdef1234567890abcdef12"

	require.True(t, shaMatches(full, full))
	require.True(t, shaMatches(full, "abcdef1"), "a 7-char abbreviation matches by prefix")
	require.True(t, shaMatches(full, "ABCDEF1234"), "matching is case-insensitive")
	require.True(t, shaMatches(full, "  abcdef1  "), "surrounding whitespace is ignored")

	require.False(t, shaMatches(full, "abcdef"), "a fragment shorter than 7 must match exactly")
	require.False(t, shaMatches(full, "beefbeef"))
	require.False(t, shaMatches(full, ""))
	require.False(t, shaMatches("", full))
}
