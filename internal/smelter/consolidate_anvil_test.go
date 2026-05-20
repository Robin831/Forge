package smelter

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeRulesFile renders a RulesFile to .forge/warden-rules.yaml under anvilPath.
func writeRulesFile(t *testing.T, anvilPath string, rf *warden.RulesFile) {
	t.Helper()
	require.NoError(t, warden.SaveRules(anvilPath, rf))
}

func TestConsolidateAnvil_NoChanges_LeavesFilesUntouched(t *testing.T) {
	dir := t.TempDir()
	writeRulesFile(t, dir, &warden.RulesFile{
		Rules: []warden.Rule{
			{ID: "r1", Category: "style", Pattern: "p1", Check: "c1", Source: warden.SourceList{"manual"}, Added: "2026-05-01"},
		},
	})
	rulesPath := warden.RulesPath(dir)
	before, err := os.ReadFile(rulesPath)
	require.NoError(t, err)

	res, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath: dir,
		AnvilName: "test",
		// Pass 1 disabled (no consolidator), Pass 2 disabled (no threshold),
		// Pass 3 yields nothing because the only rule has no copilot:PR#N source.
	})
	require.NoError(t, err)
	assert.False(t, res.Passes.HasChanges())
	assert.Equal(t, 1, res.InitialCount)
	assert.Equal(t, 1, res.FinalActive)

	// Archive file should not be created when no pass produced changes.
	_, statErr := os.Stat(warden.ArchivePath(dir))
	assert.True(t, os.IsNotExist(statErr), "archive must not be created when nothing changed")

	// Active rules file must be byte-identical to the input.
	after, err := os.ReadFile(rulesPath)
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "active file must not be rewritten when no pass produced changes")
}

func TestConsolidateAnvil_StalenessPersistsArchive(t *testing.T) {
	dir := t.TempDir()
	writeRulesFile(t, dir, &warden.RulesFile{
		Rules: []warden.Rule{
			{ID: "ancient", Category: "style", Pattern: "p", Check: "c", Source: warden.SourceList{"manual"}, Added: "2020-01-01"},
			{ID: "fresh", Category: "style", Pattern: "p2", Check: "c2", Source: warden.SourceList{"manual"}, Added: "2026-05-01"},
		},
	})

	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	res, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath:        dir,
		AnvilName:        "test",
		ArchiveAfterDays: 30,
		Now:              now,
	})
	require.NoError(t, err)
	require.True(t, res.Passes.HasChanges())
	assert.Len(t, res.Passes.Archived, 1)
	assert.Equal(t, "ancient", res.Passes.Archived[0].ID)
	assert.Equal(t, 1, res.FinalActive)
	assert.Equal(t, 1, res.ArchiveCount)

	// Active file no longer contains the stale rule.
	active, err := warden.LoadRules(dir)
	require.NoError(t, err)
	require.Len(t, active.Rules, 1)
	assert.Equal(t, "fresh", active.Rules[0].ID)

	// Archive file contains the stale rule with reason="stale".
	archive, err := warden.LoadArchive(warden.ArchivePath(dir))
	require.NoError(t, err)
	require.Len(t, archive.Rules, 1)
	assert.Equal(t, "ancient", archive.Rules[0].ID)
	assert.Equal(t, warden.ArchiveReasonStale, archive.Rules[0].ArchiveReason)
}

func TestConsolidateAnvil_RoundTripPreservesContent(t *testing.T) {
	// Verifies the bead's contract: archive→active (restore) → archive
	// (consolidate) preserves the embedded Rule's content (ID, Category,
	// Pattern, Check, Source, Added, Paths) byte-for-byte.
	dir := t.TempDir()

	// Start: a single rule sits in the archive, active is empty.
	original := warden.Rule{
		ID:       "round-trip-rule",
		Category: "style",
		Pattern:  "a long pattern about something",
		Check:    "verify the thing",
		Source:   warden.SourceList{"manual", "copilot:PR#42"},
		Added:    "2020-01-01",
		Paths:    []string{"**/*.go"},
	}
	archive := &warden.Archive{}
	archive.Add(original, warden.ArchiveReasonStale, "")
	require.NoError(t, archive.Save(warden.ArchivePath(dir)))
	writeRulesFile(t, dir, &warden.RulesFile{})

	// Simulate restore: pull the rule out of the archive into active.
	loadedArchive, err := warden.LoadArchive(warden.ArchivePath(dir))
	require.NoError(t, err)
	restored, ok := loadedArchive.Remove(original.ID)
	require.True(t, ok)
	rf, err := warden.LoadRules(dir)
	require.NoError(t, err)
	require.True(t, rf.AddRule(restored.Rule))
	require.NoError(t, warden.SaveRules(dir, rf))
	require.NoError(t, loadedArchive.Save(warden.ArchivePath(dir)))

	// Confirm the active file now holds the restored rule with identical content.
	afterRestore, err := warden.LoadRules(dir)
	require.NoError(t, err)
	require.Len(t, afterRestore.Rules, 1)
	assert.Equal(t, original, afterRestore.Rules[0],
		"restored rule must be byte-equivalent to the original Rule content")

	// Re-archive via consolidate (using a far-future "now" so the stale pass picks it up).
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	res, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath:        dir,
		AnvilName:        "test",
		ArchiveAfterDays: 30,
		Now:              now,
	})
	require.NoError(t, err)
	require.Len(t, res.Passes.Archived, 1)

	// The archive must contain a rule whose embedded Rule equals the original.
	finalArchive, err := warden.LoadArchive(warden.ArchivePath(dir))
	require.NoError(t, err)
	require.Len(t, finalArchive.Rules, 1)
	assert.Equal(t, original, finalArchive.Rules[0].Rule,
		"archive→active→archive must preserve the embedded Rule content")
}

func TestConsolidateAnvil_LoadError(t *testing.T) {
	// Pointing AnvilPath at a non-existent directory should NOT error —
	// LoadRules returns empty for missing files. The run becomes a no-op.
	dir := filepath.Join(t.TempDir(), "missing")
	res, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath: dir,
		AnvilName: "missing",
	})
	require.NoError(t, err)
	assert.False(t, res.Passes.HasChanges())
	assert.Equal(t, 0, res.InitialCount)
}

func TestConsolidateAnvil_EventLoggerInvoked(t *testing.T) {
	dir := t.TempDir()
	writeRulesFile(t, dir, &warden.RulesFile{
		Rules: []warden.Rule{
			{ID: "ancient", Category: "style", Pattern: "p", Check: "c", Source: warden.SourceList{"manual"}, Added: "2020-01-01"},
		},
	})

	var events []string
	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	_, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath:        dir,
		AnvilName:        "test",
		ArchiveAfterDays: 30,
		Now:              now,
		EventLogger: func(name, msg string) {
			events = append(events, name+":"+msg)
		},
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "smelter_flushed:")
	assert.Contains(t, events[0], "Archived 1 stale rule(s)")
}
