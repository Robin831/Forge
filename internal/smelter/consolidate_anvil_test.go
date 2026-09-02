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

// overlapOnlyPair is two rules the overlap criterion clusters and Jaccard
// does not: the short rule's whole vocabulary sits inside the long one's, so
// containment is 1.00 while Jaccard is |short| / |long| — the exact shape
// the second criterion was added for, and the one a Jaccard-only pass cannot
// see at any usable threshold.
func overlapOnlyPair() []warden.Rule {
	long := "the documented log filename must match the filename the code actually produces, " +
		"including the rotation suffix and the directory the writer resolves at startup, " +
		"because a stale document sends an operator to a path that holds nothing"
	short := "the documented log filename must match the filename the code produces"
	return []warden.Rule{
		{ID: "verbose", Category: "style", Pattern: "documentation filename", Check: long, Source: warden.SourceList{"manual"}, Added: "2026-05-01"},
		{ID: "terse", Category: "style", Pattern: "documentation filename", Check: short, Source: warden.SourceList{"manual"}, Added: "2026-05-02"},
	}
}

// The `forge warden consolidate` path is the CLI's only route to the overlap
// criterion, and it fails silently if the wiring regresses: dropping Overlap
// from the DedupParams literal restores the measured "clusters nothing at
// 0.6" behaviour with every other test in this file still green, because the
// flush tests build their params through Smelter.dedupParams and never reach
// this function.
func TestConsolidateAnvil_OverlapThresholdDefaultsWhenUnset(t *testing.T) {
	dir := t.TempDir()
	writeRulesFile(t, dir, &warden.RulesFile{Rules: overlapOnlyPair()})

	res, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath:      dir,
		AnvilName:      "test",
		Consolidator:   stubConsolidator(t, "merged-log-filename", "documentation filename", "the documented filename must match the code"),
		DedupThreshold: 0.6,
		// OverlapThreshold left at 0 — it must resolve to the shipped
		// default rather than to "criterion disabled".
	})
	require.NoError(t, err)
	require.NoError(t, res.FirstError)
	require.Len(t, res.Passes.Consolidated, 1, "the pair must cluster on containment when overlap falls back to its default")
	assert.ElementsMatch(t, []string{"verbose", "terse"}, res.Passes.Consolidated[0].ReplacedIDs)

	active, err := warden.LoadRules(dir)
	require.NoError(t, err)
	require.Len(t, active.Rules, 1)
	assert.Equal(t, "merged-log-filename", active.Rules[0].ID)
}

// The mirror case: a negative overlap threshold disables the criterion and
// leaves Jaccard alone, which on this pair merges nothing. Without it the
// test above would pass just as well against a pass that ignored the field.
func TestConsolidateAnvil_NegativeOverlapThresholdDisablesTheCriterion(t *testing.T) {
	dir := t.TempDir()
	writeRulesFile(t, dir, &warden.RulesFile{Rules: overlapOnlyPair()})

	res, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath:        dir,
		AnvilName:        "test",
		Consolidator:     stubConsolidator(t, "merged-log-filename", "p", "c"),
		DedupThreshold:   0.6,
		OverlapThreshold: -1,
	})
	require.NoError(t, err)
	assert.Empty(t, res.Passes.Consolidated, "overlap disabled leaves the pair to Jaccard, which cannot score it")

	active, err := warden.LoadRules(dir)
	require.NoError(t, err)
	assert.Len(t, active.Rules, 2)
}

// contradictoryPair is two rules from one source PR prescribing opposite
// lock scopes for the same call — the shape DetectContradictions reports and
// never resolves.
func contradictoryPair() []warden.Rule {
	return []warden.Rule{
		{
			ID: "invoke-cancel-under-lock", Category: "concurrency",
			Pattern: "cancellation callback invoked from the registry",
			Check:   "Invoke the cancellation callback under the lock so the registry cannot be mutated between the lookup and the call",
			Source:  warden.SourceList{"copilot:PR#682"}, Added: "2026-05-01",
			// Paths pre-set so the backfill pass has nothing to do: this
			// test is about a run whose ONLY finding is a contradiction.
			Paths: []string{"**/*.go"},
		},
		{
			ID: "unlock-before-callback", Category: "concurrency",
			Pattern: "cancellation callback invoked from the registry",
			Check:   "Release the lock before invoking the cancellation callback so a callback that re-enters the registry cannot deadlock",
			Source:  warden.SourceList{"copilot:PR#682"}, Added: "2026-05-02",
			Paths: []string{"**/*.go"},
		},
	}
}

// Contradictions ride out on the CLI path too, and they must not make the
// run look like it changed something: nothing is merged or dropped for a
// pair, so the rules file is left byte-identical and HasChanges stays false.
func TestConsolidateAnvil_ContradictionsSurfaceWithoutChangingRules(t *testing.T) {
	dir := t.TempDir()
	writeRulesFile(t, dir, &warden.RulesFile{Rules: contradictoryPair()})
	before, err := os.ReadFile(warden.RulesPath(dir))
	require.NoError(t, err)

	var events []string
	res, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath: dir,
		AnvilName: "test",
		EventLogger: func(name, msg string) {
			events = append(events, name+":"+msg)
		},
	})
	require.NoError(t, err)

	require.Len(t, res.Passes.Contradictions, 1)
	c := res.Passes.Contradictions[0]
	assert.Equal(t, "invoke-cancel-under-lock", c.A.ID)
	assert.Equal(t, "unlock-before-callback", c.B.ID)
	assert.Equal(t, warden.ContradictionLockScope, c.Kind)

	assert.False(t, res.Passes.HasChanges(), "a contradiction is reported, not resolved — there is nothing to persist")

	after, err := os.ReadFile(warden.RulesPath(dir))
	require.NoError(t, err)
	assert.Equal(t, string(before), string(after), "the rules file must be untouched")

	// Every other pass in ConsolidateAnvil reports its outcome through the
	// event logger; before the shared reporter this one logged and nothing
	// else, so the CLI path emitted no event where the daemon path did.
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "smelter_flushed:")
	assert.Contains(t, events[0], "1 contradictory rule pair")
}

// TestConsolidateAnvil_FileCapPersistsWithItsOwnReason drives the CLI path's
// eviction end to end. Both entry points now share applyFileCap, so this also
// pins that the evicted rules reach the archive under over-cap and are never
// folded into the staleness pass's count.
func TestConsolidateAnvil_FileCapPersistsWithItsOwnReason(t *testing.T) {
	dir := t.TempDir()
	writeRulesFile(t, dir, &warden.RulesFile{
		Rules: []warden.Rule{
			{ID: "ancient", Category: "style", Pattern: "p0", Check: "c0", Source: warden.SourceList{"manual"}, Added: "2020-01-01"},
			{ID: "broad", Category: "style", Pattern: "p1", Check: "c1", Source: warden.SourceList{"manual"}, Added: "2026-05-01", Paths: []string{"**/*"}},
			{ID: "narrow", Category: "style", Pattern: "p2", Check: "c2", Source: warden.SourceList{"manual"}, Added: "2026-05-01", Paths: []string{"internal/warden/filter.go"}},
		},
	})

	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	res, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath:        dir,
		AnvilName:        "test",
		ArchiveAfterDays: 30,
		MaxRulesInFile:   1,
		Now:              now,
	})
	require.NoError(t, err)
	require.True(t, res.Passes.HasChanges())

	// The staleness sweep took "ancient"; the ceiling then took the broader of
	// the two survivors. Two entries, two different reasons.
	reasons := map[string]string{}
	for _, a := range res.Passes.Archived {
		reasons[a.ID] = a.ArchiveReason
	}
	assert.Equal(t, map[string]string{
		"ancient": warden.ArchiveReasonStale,
		"broad":   warden.ArchiveReasonOverCap,
	}, reasons)

	active, err := warden.LoadRules(dir)
	require.NoError(t, err)
	require.Len(t, active.Rules, 1)
	assert.Equal(t, "narrow", active.Rules[0].ID)

	archive, err := warden.LoadArchive(warden.ArchivePath(dir))
	require.NoError(t, err)
	assert.Len(t, archive.Rules, 2)

	// And the rendered aggregates keep the two apart.
	subject := buildCommitSubject(res.Passes)
	assert.Contains(t, subject, "archive 1 stale rule(s)")
	assert.Contains(t, subject, "evict 1 over-cap rule(s)")
}

// A ceiling of zero (unset) or negative is the disable, on dedup_threshold's
// rule that 0 is the field's zero value and cannot mean "off" by itself.
func TestConsolidateAnvil_FileCapDisabled(t *testing.T) {
	for _, max := range []int{0, -1} {
		dir := t.TempDir()
		writeRulesFile(t, dir, &warden.RulesFile{
			Rules: []warden.Rule{
				{ID: "a", Category: "style", Pattern: "p1", Check: "c1", Source: warden.SourceList{"manual"}, Added: "2026-05-01"},
				{ID: "b", Category: "style", Pattern: "p2", Check: "c2", Source: warden.SourceList{"manual"}, Added: "2026-05-01"},
			},
		})
		res, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
			AnvilPath:      dir,
			AnvilName:      "test",
			MaxRulesInFile: max,
			Now:            time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
		})
		require.NoError(t, err)
		assert.False(t, res.Passes.HasChanges(), "max=%d", max)
		assert.Equal(t, 2, res.FinalActive, "max=%d", max)
	}
}
