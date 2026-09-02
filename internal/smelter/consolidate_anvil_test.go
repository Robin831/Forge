package smelter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/Robin831/Forge/internal/worktree"
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

// gitAnvilFixture builds a MAIN checkout with one commit. The shape matters:
// an anvil is a main checkout by definition, and a main checkout is exactly
// what Smith's pre-flight refuses to run a session in.
//
// CleanGitEnv rather than os.Environ, because these tests run inside a
// worker worktree that exports GIT_DIR/GIT_WORK_TREE — inherited, `git -C
// <tmp> init` reinitializes THAT repository and the fixture silently is not
// a repository at all, which is the one condition this test exists to set up.
func gitAnvilFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(executil.CleanGitEnv(), "LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %v: %s", args, out)
	}
	run("init", "-b", "main")
	run("config", "user.email", "forge@example.com")
	run("config", "user.name", "Forge Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("anvil\n"), 0o644))
	run("add", "README.md")
	run("commit", "-m", "initial")
	return dir
}

// The bug this guards: Pass 1 spawns one AI session per cluster, and
// smith.SpawnWithOptions refuses any working directory that is inside a git
// repository without being a linked worktree. Handed the anvil path — a main
// checkout — every cluster failed the pre-flight before a provider was
// spawned, so `forge warden consolidate` had never merged a single cluster
// on a real anvil while reporting the pass as run.
//
// It cannot be caught by the other tests in this file: their fixtures are
// bare t.TempDirs, which sit outside any repository and which the pre-flight
// accepts as they stand. Only a fixture that is a real checkout reproduces
// the refusal.
func TestConsolidateAnvil_Pass1RunsInAWorktreeNotTheAnvil(t *testing.T) {
	anvil := gitAnvilFixture(t)
	writeRulesFile(t, anvil, &warden.RulesFile{Rules: overlapOnlyPair()})

	var sessionDirs []string
	consolidator := func(_ context.Context, dir, _ string) ([]byte, error) {
		sessionDirs = append(sessionDirs, dir)
		// The exact question smith asks before it spawns anything.
		if err := worktree.ValidateWorktreeDir(dir); err != nil {
			return nil, err
		}
		body, err := json.Marshal(map[string]string{
			"id":      "merged-log-filename",
			"pattern": "documentation filename",
			"check":   "the documented filename must match the code",
		})
		require.NoError(t, err)
		return body, nil
	}

	res, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath:      anvil,
		AnvilName:      "test",
		Consolidator:   consolidator,
		DedupThreshold: 0.6,
	})
	require.NoError(t, err)
	require.NoError(t, res.FirstError, "no cluster may fail the pre-flight")
	require.Len(t, res.Passes.Consolidated, 1)

	require.NotEmpty(t, sessionDirs)
	for _, dir := range sessionDirs {
		assert.NotEqual(t, anvil, dir, "a session must never run in the main checkout")
	}

	// The merge is applied to the anvil's own rules file, not to whatever
	// the throwaway checkout holds: the merged rule comes back as JSON and
	// the file the caller loaded is the file it writes.
	active, err := warden.LoadRules(anvil)
	require.NoError(t, err)
	require.Len(t, active.Rules, 1)
	assert.Equal(t, "merged-log-filename", active.Rules[0].ID)

	// And nothing is left registered against the anvil.
	list := exec.Command("git", "-C", anvil, "worktree", "list")
	list.Env = append(executil.CleanGitEnv(), "LC_ALL=C")
	out, err := list.CombinedOutput()
	require.NoErrorf(t, err, "git worktree list: %s", out)
	for _, dir := range sessionDirs {
		assert.NotContains(t, string(out), dir)
		assert.NoDirExists(t, dir)
	}
}

// emptyGitAnvilFixture builds a repository with no commits. It is the
// cheapest deterministic way to make the ephemeral worktree impossible to
// create: HEAD resolves to nothing, so there is no commit to detach at.
func emptyGitAnvilFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
	dir := t.TempDir()
	cmd := exec.Command("git", "-C", dir, "init", "-b", "main")
	cmd.Env = append(executil.CleanGitEnv(), "LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git init: %s", out)
	return dir
}

// secondOverlapPair is a second cluster, disjoint in vocabulary from
// overlapOnlyPair's, so a file holding both produces two clusters and a
// consolidator can fail one while the other merges.
func secondOverlapPair() []warden.Rule {
	short := "the handle must be deleted only when the caller still owns it, " +
		"checking the generation counter before the close"
	long := short + ", so that a recycled descriptor belonging to another " +
		"session is never closed and no unrelated request loses its socket"
	return []warden.Rule{
		{ID: "handle-verbose", Category: "style", Pattern: "handle ownership", Check: long, Source: warden.SourceList{"manual"}, Added: "2026-05-01"},
		{ID: "handle-terse", Category: "style", Pattern: "handle ownership", Check: short, Source: warden.SourceList{"manual"}, Added: "2026-05-02"},
	}
}

// The design decision this change exists for, stated as a test: when the
// ephemeral worktree cannot be created there is NO fallback to AnvilPath —
// that is the arrangement that failed every cluster while reporting the pass
// as run — the reason lands in FirstError (the only Pass 1 diagnostic the
// CLI prints), and Passes 2 and 3, which spawn no session, still run.
//
// Nothing else in this file reaches the branch: every other fixture is
// either a bare t.TempDir (which WithEphemeralWorktree passes straight
// through) or a healthy checkout. A regression that reinstated the fallback,
// or swallowed the error, would leave the whole suite green.
func TestConsolidateAnvil_Pass1WorktreeFailureSkipsPass1AndKeepsLaterPasses(t *testing.T) {
	anvil := emptyGitAnvilFixture(t)
	rules := append(overlapOnlyPair(),
		warden.Rule{ID: "ancient", Category: "documentation", Pattern: "p", Check: "c", Source: warden.SourceList{"manual"}, Added: "2020-01-01"})
	writeRulesFile(t, anvil, &warden.RulesFile{Rules: rules})

	var sessionDirs []string
	consolidator := func(_ context.Context, dir, _ string) ([]byte, error) {
		sessionDirs = append(sessionDirs, dir)
		return nil, nil
	}

	res, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath:        anvil,
		AnvilName:        "test",
		Consolidator:     consolidator,
		DedupThreshold:   0.6,
		ArchiveAfterDays: 30,
		Now:              time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err, "a worktree failure is not fatal to the run")
	require.Error(t, res.FirstError, "the reason Pass 1 did not run must reach the caller")
	assert.Contains(t, res.FirstError.Error(), "pass 1 worktree for test")

	assert.Empty(t, sessionDirs, "no session may run once the worktree could not be created")
	assert.Empty(t, res.Passes.Consolidated)

	// A pass that never ran has established nothing about the file, so it
	// must not read as one that found nothing to merge: no cluster was
	// attempted, no cluster errored, and Pass1Complete is what keeps the
	// caller from reporting steady state off those two zeros.
	assert.True(t, res.Pass1Skipped)
	assert.Zero(t, res.ClustersAttempted)
	assert.Empty(t, res.ClusterErrors)
	assert.False(t, res.Pass1Complete())

	// Passes 2 and 3 need no session and are unaffected.
	require.Len(t, res.Passes.Archived, 1)
	assert.Equal(t, "ancient", res.Passes.Archived[0].ID)

	// The pair Pass 1 would have merged is still there, unmerged.
	active, err := warden.LoadRules(anvil)
	require.NoError(t, err)
	ids := make([]string, 0, len(active.Rules))
	for _, r := range active.Rules {
		ids = append(ids, r.ID)
	}
	assert.ElementsMatch(t, []string{"verbose", "terse"}, ids)
}

// The pre-existing contract the FirstError field carries: a cluster the
// runner failed is reported, and the remaining clusters still merge. No test
// made the consolidator fail at all, so dropping the assignment (or the
// `continue` behind it) changed nothing anybody checked.
func TestConsolidateAnvil_FirstClusterErrorReportedAndOthersStillMerge(t *testing.T) {
	dir := t.TempDir()
	writeRulesFile(t, dir, &warden.RulesFile{Rules: append(overlapOnlyPair(), secondOverlapPair()...)})

	res, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath:      dir,
		AnvilName:      "test",
		Consolidator:   failingClusterConsolidator(t, "documentation filename"),
		DedupThreshold: 0.6,
	})
	require.NoError(t, err)
	require.Error(t, res.FirstError)
	assert.Contains(t, res.FirstError.Error(), "cluster is on fire")

	require.Len(t, res.Passes.Consolidated, 1, "the cluster that did not fail must still merge")
	assert.ElementsMatch(t, []string{"handle-verbose", "handle-terse"}, res.Passes.Consolidated[0].ReplacedIDs)

	// The denominator a summary needs: one cluster merged out of the two the
	// pass found, and the one that did not is carried out rather than only
	// logged.
	assert.Equal(t, 2, res.ClustersAttempted)
	require.Len(t, res.ClusterErrors, 1)
	assert.Contains(t, res.ClusterErrors[0].Error(), "cluster is on fire")
	assert.False(t, res.Pass1Complete())
	assert.False(t, res.Pass1Skipped, "the pass ran; one cluster inside it failed")

	active, err := warden.LoadRules(dir)
	require.NoError(t, err)
	ids := make([]string, 0, len(active.Rules))
	for _, r := range active.Rules {
		ids = append(ids, r.ID)
	}
	assert.ElementsMatch(t, []string{"verbose", "terse", "merged-handle"}, ids)
}

// A teardown failure after a completed Pass 1 is the other error
// WithEphemeralWorktree returns, and it is NOT the reason a cluster failed to
// merge: it must stay out of FirstError while a cluster error is there to
// claim it. Reported the other way round — which is what a single untyped
// worktree error produced — the operator reads `Warning: first consolidation
// cluster error: ... removing /tmp/...` for a pass whose merges were kept and
// persisted, and the one error worth reading is gone.
func TestConsolidateAnvil_ClusterErrorOutranksWorktreeCleanupFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("cleanup cannot be made to fail as root")
	}
	anvil := gitAnvilFixture(t)
	writeRulesFile(t, anvil, &warden.RulesFile{Rules: append(overlapOnlyPair(), secondOverlapPair()...)})

	inner := failingClusterConsolidator(t, "documentation filename")
	var sealed string
	consolidator := func(ctx context.Context, dir, prompt string) ([]byte, error) {
		sealed = sealWorktreeParent(t, dir)
		return inner(ctx, dir, prompt)
	}

	res, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath:      anvil,
		AnvilName:      "test",
		Consolidator:   consolidator,
		DedupThreshold: 0.6,
	})
	require.NoError(t, err)
	require.Error(t, res.FirstError)
	assert.Contains(t, res.FirstError.Error(), "cluster is on fire")
	assert.NotContains(t, res.FirstError.Error(), "worktree",
		"a temp directory that outlived a pass that RAN must not displace the cluster error")

	// Positive evidence that there were two errors to order in the first
	// place: the teardown would have deleted the sealed directory. Without
	// it the assertion above is vacuously true of a run in which cleanup
	// simply succeeded, and the precedence rule it guards would be free to
	// regress unnoticed.
	require.NotEmpty(t, sealed, "the consolidator must have run and sealed the checkout's parent")
	require.DirExists(t, sealed,
		"the ephemeral checkout's teardown must have failed, or this test orders one error against none")

	// The pass ran, so its merge is kept and persisted.
	require.Len(t, res.Passes.Consolidated, 1)
	active, err := warden.LoadRules(anvil)
	require.NoError(t, err)
	ids := make([]string, 0, len(active.Rules))
	for _, r := range active.Rules {
		ids = append(ids, r.ID)
	}
	assert.ElementsMatch(t, []string{"verbose", "terse", "merged-handle"}, ids)
}

// The other half of that precedence: with no cluster error to report, the
// leaked checkout is worth surfacing rather than living in the log alone —
// and it says cleanup, not that Pass 1 never ran.
func TestConsolidateAnvil_CleanupFailureReportedWhenNoClusterFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("cleanup cannot be made to fail as root")
	}
	anvil := gitAnvilFixture(t)
	writeRulesFile(t, anvil, &warden.RulesFile{Rules: overlapOnlyPair()})

	inner := stubConsolidator(t, "merged-log-filename", "documentation filename", "the documented filename must match the code")
	var sealed string
	consolidator := func(ctx context.Context, dir, prompt string) ([]byte, error) {
		sealed = sealWorktreeParent(t, dir)
		return inner(ctx, dir, prompt)
	}

	res, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath:      anvil,
		AnvilName:      "test",
		Consolidator:   consolidator,
		DedupThreshold: 0.6,
	})
	require.NoError(t, err)
	require.Error(t, res.FirstError)
	assert.Contains(t, res.FirstError.Error(), "pass 1 worktree cleanup for test")
	require.NotEmpty(t, sealed, "the consolidator must have run and sealed the checkout's parent")
	require.DirExists(t, sealed, "the teardown this test is about must actually have failed")

	require.Len(t, res.Passes.Consolidated, 1, "the pass completed; only its checkout outlived it")
	active, err := warden.LoadRules(anvil)
	require.NoError(t, err)
	require.Len(t, active.Rules, 1)
	assert.Equal(t, "merged-log-filename", active.Rules[0].ID)
}

// failingClusterConsolidator fails the cluster whose prompt names marker and
// merges every other one, so a run can hold one failing and one successful
// cluster.
func failingClusterConsolidator(t *testing.T, marker string) warden.ConsolidationRunner {
	t.Helper()
	return func(_ context.Context, _, prompt string) ([]byte, error) {
		if strings.Contains(prompt, marker) {
			return nil, errors.New("cluster is on fire")
		}
		body, err := json.Marshal(map[string]string{
			"id":      "merged-handle",
			"pattern": "handle ownership",
			"check":   "delete the handle only when you still own it",
		})
		require.NoError(t, err)
		return body, nil
	}
}

// sealWorktreeParent drops the write bit on the temp directory holding the
// ephemeral checkout, which is what makes its teardown fail: nothing can be
// unlinked from a directory that cannot be written. It is the portable stand-in
// for the Windows file lock that teardown is really guarding against, and the
// test unseals and removes the directory afterwards — it lives under
// os.MkdirTemp rather than t.TempDir, so nothing else would.
//
// It returns the directory it sealed, so a caller can prove the seal did its
// work: a teardown that succeeded would have deleted that directory. And it
// FAILS rather than returning quietly when its precondition does not hold.
// The layout it depends on — the checkout sitting one directory below an
// os.MkdirTemp parent — is worktree.CreateEphemeral's private detail, so a
// silent early return would leave the tests that force a cleanup failure
// asserting things about an error that was never produced, green either way.
func sealWorktreeParent(t *testing.T, worktreeDir string) string {
	t.Helper()
	parent := filepath.Dir(worktreeDir)
	require.NotEqual(t, worktreeDir, parent,
		"the ephemeral checkout must sit BELOW the directory the seal blocks")
	require.DirExists(t, parent)
	// The seal chmods and then deletes this directory, so pin it to the
	// os.MkdirTemp root before touching it: a layout change that moved the
	// checkout into the anvil must fail here, not delete part of a fixture.
	tempRoot, err := filepath.EvalSymlinks(os.TempDir())
	require.NoError(t, err)
	sealed, err := filepath.EvalSymlinks(parent)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(sealed, tempRoot+string(filepath.Separator)),
		"expected the ephemeral checkout's parent under %s, got %s", tempRoot, sealed)

	t.Cleanup(func() {
		_ = os.Chmod(parent, 0o700)
		_ = os.RemoveAll(parent)
	})
	require.NoError(t, os.Chmod(parent, 0o500))
	return parent
}
