package smelter

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// filteredEnv returns os.Environ() with the git repo-location vars removed so
// that git subprocesses spawned in tests are not accidentally confined to the
// worktree environment Forge runs in.
func filteredEnv() []string {
	return executil.CleanGitEnv()
}

func openTestDB(t *testing.T) *state.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestBranchForAnvil(t *testing.T) {
	assert.Equal(t, "forge/warden-learn-batch/heimdall", branchForAnvil("heimdall"))
	assert.Equal(t, "forge/warden-learn-batch/my-repo", branchForAnvil("my-repo"))
}

func TestNew_SetsDefaults(t *testing.T) {
	db := openTestDB(t)
	paths := map[string]string{"a": "/a"}
	s := New(db, 5*time.Minute, paths)

	assert.NotNil(t, s.wtMgr)
	assert.Equal(t, 5*time.Minute, s.interval)
	assert.Equal(t, paths, s.anvilPaths)
}

func TestFlush_NoPending_IsNoop(t *testing.T) {
	db := openTestDB(t)
	s := New(db, time.Hour, map[string]string{"anvil-a": "/tmp/a"})

	err := s.Flush(context.Background())
	assert.NoError(t, err)
}

func TestFlush_UnknownAnvil_Skips(t *testing.T) {
	db := openTestDB(t)

	// Insert a rule for an anvil that is not in the config.
	require.NoError(t, db.InsertPendingRule("unknown-anvil", "id: r1\npattern: test", "PR-1"))

	s := New(db, time.Hour, map[string]string{"other-anvil": "/tmp/other"})
	err := s.Flush(context.Background())
	assert.NoError(t, err)

	// Rule should still be pending (not deleted) since anvil was skipped.
	byAnvil, err := db.QueryPendingRulesByAnvil()
	require.NoError(t, err)
	assert.Len(t, byAnvil["unknown-anvil"], 1)
}

func TestFlush_ContextCanceled_ReturnsError(t *testing.T) {
	db := openTestDB(t)
	require.NoError(t, db.InsertPendingRule("anvil-a", "id: r1\npattern: test", "PR-1"))

	s := New(db, time.Hour, map[string]string{"anvil-a": "/tmp/a"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := s.Flush(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestUpdateAnvilPaths(t *testing.T) {
	db := openTestDB(t)
	s := New(db, time.Hour, map[string]string{"a": "/path/a"})

	newPaths := map[string]string{"b": "/path/b", "c": "/path/c"}
	s.UpdateAnvilPaths(newPaths)

	// Verify the paths were updated (read under lock).
	s.mu.RLock()
	defer s.mu.RUnlock()
	assert.Equal(t, newPaths, s.anvilPaths)

	// Verify the original map was copied (not aliased).
	newPaths["d"] = "/path/d"
	assert.NotContains(t, s.anvilPaths, "d")
}

func TestUpdateInterval_ResetsTicker(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 24*time.Hour, map[string]string{"a": "/a"})

	ctx, cancel := context.WithCancel(context.Background())

	// Start the Run loop in the background.
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Update the interval and verify it's stored.
	s.UpdateInterval(4 * time.Hour)

	// Wait deterministically for the Run loop to process the update.
	require.Eventually(t, func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.interval == 4*time.Hour
	}, 2*time.Second, 10*time.Millisecond)

	cancel()
	<-done
}

func TestUpdateInterval_NonBlocking(t *testing.T) {
	db := openTestDB(t)
	s := New(db, time.Hour, map[string]string{})

	// Call UpdateInterval twice without a consumer — should not block.
	s.UpdateInterval(2 * time.Hour)
	s.UpdateInterval(3 * time.Hour)

	// The latest value should be in the channel.
	select {
	case v := <-s.intervalCh:
		assert.Equal(t, 3*time.Hour, v)
	default:
		t.Fatal("expected a value on intervalCh")
	}
}

func TestTimeUntilNextFlush_NoEventLogged(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 8*time.Hour, map[string]string{})

	// No event logged yet — flush immediately (delay == 0).
	assert.Equal(t, time.Duration(0), s.timeUntilNextFlush())
}

func TestTimeUntilNextFlush_RecentCycle(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 8*time.Hour, map[string]string{})

	// Cycle-done just now — next flush is ~8 hours away.
	require.NoError(t, db.LogEvent(state.EventSmelterCycleDone, "cycle complete", "", ""))
	delay := s.timeUntilNextFlush()
	assert.Greater(t, delay, 7*time.Hour, "expected delay close to the full interval")
	assert.LessOrEqual(t, delay, 8*time.Hour)
}

func TestTimeUntilNextFlush_CycleHalfwayThrough(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 8*time.Hour, map[string]string{})

	// Cycle completed 4 hours ago — next flush is ~4 hours away.
	halfway := time.Now().Add(-4 * time.Hour)
	require.NoError(t, db.LogEventAt(state.EventSmelterCycleDone, "cycle complete", "", "", halfway))
	delay := s.timeUntilNextFlush()
	assert.Greater(t, delay, 3*time.Hour+55*time.Minute)
	assert.LessOrEqual(t, delay, 4*time.Hour+5*time.Minute)
}

func TestTimeUntilNextFlush_OldCycle(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 8*time.Hour, map[string]string{})

	// Cycle-done event logged more than the interval ago — flush immediately.
	old := time.Now().Add(-9 * time.Hour)
	require.NoError(t, db.LogEventAt(state.EventSmelterCycleDone, "cycle complete", "", "", old))
	assert.Equal(t, time.Duration(0), s.timeUntilNextFlush())
}

func TestTimeUntilNextFlush_ZeroInterval_AlwaysZero(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	// interval=0 means always flush immediately.
	require.NoError(t, db.LogEvent(state.EventSmelterCycleDone, "cycle complete", "", ""))
	assert.Equal(t, time.Duration(0), s.timeUntilNextFlush())
}

func TestFlush_NoPending_LogsCycleDone(t *testing.T) {
	db := openTestDB(t)
	s := New(db, time.Hour, map[string]string{})

	err := s.Flush(context.Background())
	assert.NoError(t, err)

	// A cycle-done event should have been logged even when nothing was pending.
	ran, err := db.HasEventWithin(state.EventSmelterCycleDone, time.Minute)
	require.NoError(t, err)
	assert.True(t, ran, "EventSmelterCycleDone should be logged after a no-op flush")
}

func TestFlush_WorktreeFailure_ContinuesToNextAnvil(t *testing.T) {
	db := openTestDB(t)

	// Insert rules for two anvils, both pointing to non-existent paths.
	require.NoError(t, db.InsertPendingRule("anvil-a", "id: r1\ncategory: test\npattern: p\ncheck: c", "PR-1"))
	require.NoError(t, db.InsertPendingRule("anvil-b", "id: r2\ncategory: test\npattern: p\ncheck: c", "PR-2"))

	nonExistent := filepath.Join(t.TempDir(), "does-not-exist")
	s := New(db, time.Hour, map[string]string{
		"anvil-a": nonExistent,
		"anvil-b": filepath.Join(t.TempDir(), "also-missing"),
	})

	// Flush should log errors but not return an error — each anvil failure
	// is handled individually.
	err := s.Flush(context.Background())
	assert.NoError(t, err)

	// Rules should still be pending since all flushes failed.
	byAnvil, err := db.QueryPendingRulesByAnvil()
	require.NoError(t, err)
	assert.Len(t, byAnvil["anvil-a"], 1)
	assert.Len(t, byAnvil["anvil-b"], 1)

	// EventSmelterCycleDone must NOT be logged when pending rules remain —
	// otherwise a restart after a partial failure would skip the startup flush
	// and postpone retries for still-pending rules.
	cycleDone, err := db.HasEventWithin(state.EventSmelterCycleDone, time.Minute)
	require.NoError(t, err)
	assert.False(t, cycleDone, "EventSmelterCycleDone should not be logged when anvils failed")
}

func TestFlush_MultipleRulesSameAnvil_AllProcessed(t *testing.T) {
	db := openTestDB(t)

	require.NoError(t, db.InsertPendingRule("anvil-a",
		"id: r1\ncategory: style\npattern: p1\ncheck: c1", "PR-1"))
	require.NoError(t, db.InsertPendingRule("anvil-a",
		"id: r2\ncategory: security\npattern: p2\ncheck: c2", "PR-2"))
	require.NoError(t, db.InsertPendingRule("anvil-a",
		"id: r3\ncategory: perf\npattern: p3\ncheck: c3", "PR-3"))

	// Use a non-existent path so flushAnvil fails at worktree creation,
	// but we can verify all rules were queried together.
	_ = New(db, time.Hour, map[string]string{"anvil-a": "/nonexistent"})

	byAnvil, err := db.QueryPendingRulesByAnvil()
	require.NoError(t, err)
	assert.Len(t, byAnvil["anvil-a"], 3, "all 3 rules should be queried for anvil-a")
}

// stubConsolidator returns a runner that emits a fixed merged-rule JSON
// payload for every call. Used by smelter consolidation tests.
func stubConsolidator(t *testing.T, mergedID, pattern, check string) warden.ConsolidationRunner {
	t.Helper()
	return func(_ context.Context, _, _ string) ([]byte, error) {
		body, err := json.Marshal(map[string]string{
			"id":      mergedID,
			"pattern": pattern,
			"check":   check,
		})
		require.NoError(t, err)
		return body, nil
	}
}

// TestRunConsolidation_DisabledByDefault verifies that without explicit
// configuration the consolidation pass is a no-op.
func TestRunConsolidation_DisabledByDefault(t *testing.T) {
	db := openTestDB(t)
	s := New(db, time.Hour, map[string]string{})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Category: "style", Pattern: "shared word", Check: "shared concern"},
		{ID: "r2", Category: "style", Pattern: "shared word", Check: "shared concern"},
	}}

	summary, replaced, err := s.runConsolidation(context.Background(), t.TempDir(), "anvil-a", rf)
	require.NoError(t, err)
	assert.Empty(t, summary)
	assert.Empty(t, replaced)
	assert.Len(t, rf.Rules, 2)
}

// TestRunConsolidation_MergesClusterAndPopulatesSummary verifies the smelter
// integrates warden.Consolidate correctly and returns the summary metadata
// needed for the commit message.
func TestRunConsolidation_MergesClusterAndPopulatesSummary(t *testing.T) {
	db := openTestDB(t)
	s := New(db, time.Hour, map[string]string{},
		WithConsolidator(stubConsolidator(t, "shared-concern", "shared pattern", "verify shared concern")),
		WithDedupThreshold(func() float64 { return 0.3 }),
	)

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Category: "style", Pattern: "shared word here", Check: "verify shared concern", Source: warden.SourceList{"PR-1"}, Added: "2024-02-01"},
		{ID: "r2", Category: "style", Pattern: "shared word there", Check: "ensure shared concern", Source: warden.SourceList{"PR-2"}, Added: "2024-01-01"},
		// Unrelated rule should be untouched.
		{ID: "r3", Category: "security", Pattern: "sql injection", Check: "use prepared statements"},
	}}

	summary, replaced, err := s.runConsolidation(context.Background(), t.TempDir(), "anvil-a", rf)
	require.NoError(t, err)
	require.Len(t, summary, 1)
	assert.Equal(t, "style", summary[0].Category)
	assert.ElementsMatch(t, []string{"r1", "r2"}, summary[0].ReplacedIDs)
	assert.Equal(t, "shared-concern", summary[0].Merged.ID)
	assert.Equal(t, "2024-02-01", summary[0].Merged.Added, "merged Added should be the newest in cluster")
	assert.NotEmpty(t, summary[0].Merged.MergedAt, "the merge dates the rule's own age")
	assert.Equal(t, warden.SourceList{"PR-1", "PR-2"}, summary[0].Merged.Source)

	assert.Len(t, replaced, 2)

	// Rules file should now contain r3 plus the merged rule.
	ids := make([]string, 0, len(rf.Rules))
	for _, r := range rf.Rules {
		ids = append(ids, r.ID)
	}
	assert.ElementsMatch(t, []string{"r3", "shared-concern"}, ids)
}

// TestRunConsolidation_ZeroThresholdSkipsPass verifies that a zero threshold
// disables consolidation even when a consolidator is wired.
func TestRunConsolidation_ZeroThresholdSkipsPass(t *testing.T) {
	db := openTestDB(t)
	s := New(db, time.Hour, map[string]string{},
		WithConsolidator(stubConsolidator(t, "m", "p", "c")),
		WithDedupThreshold(func() float64 { return 0 }),
	)

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Category: "style", Pattern: "same", Check: "same"},
		{ID: "r2", Category: "style", Pattern: "same", Check: "same"},
	}}

	summary, replaced, err := s.runConsolidation(context.Background(), t.TempDir(), "anvil-a", rf)
	require.NoError(t, err)
	assert.Empty(t, summary)
	assert.Empty(t, replaced)
	assert.Len(t, rf.Rules, 2)
}

// TestArchiveRules_WritesSupersededByCorrectly verifies the smelter archives
// each replaced rule with reason=duplicate and the correct superseded_by ID.
func TestArchiveRules_WritesSupersededByCorrectly(t *testing.T) {
	dir := t.TempDir()
	archived := []warden.Rule{
		{ID: "r1", Category: "style", Pattern: "p1", Check: "c1"},
		{ID: "r2", Category: "style", Pattern: "p2", Check: "c2"},
	}
	summary := []warden.MergeResult{
		{
			Merged:      warden.Rule{ID: "merged-1", Category: "style"},
			ReplacedIDs: []string{"r1", "r2"},
			Category:    "style",
		},
	}

	require.NoError(t, archiveRules(dir, duplicateArchiveEntries(archived, summary, time.Now().UTC()), nil))

	a, err := warden.LoadArchive(warden.ArchivePath(dir))
	require.NoError(t, err)
	require.Len(t, a.Rules, 2)

	for _, ar := range a.Rules {
		assert.Equal(t, warden.ArchiveReasonDuplicate, ar.ArchiveReason)
		assert.Equal(t, "merged-1", ar.SupersededBy)
		assert.False(t, ar.ArchivedAt.IsZero())
	}
}

// TestPersistRulesAndArchive_ArchiveFirstThenRules verifies the happy path:
// when both archive and rules-file writes succeed, both files land on disk.
func TestPersistRulesAndArchive_ArchiveFirstThenRules(t *testing.T) {
	dir := t.TempDir()
	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "merged-1", Category: "style", Pattern: "p", Check: "c"},
	}}
	archived := []warden.Rule{
		{ID: "r1", Category: "style", Pattern: "p1", Check: "c1"},
	}
	summary := []warden.MergeResult{{
		Merged:      warden.Rule{ID: "merged-1", Category: "style"},
		ReplacedIDs: []string{"r1"},
		Category:    "style",
	}}

	require.NoError(t, persistRulesAndArchive(dir, rf,
		duplicateArchiveEntries(archived, summary, time.Now().UTC()), nil))

	rulesData, err := os.ReadFile(filepath.Join(dir, warden.RulesFileName))
	require.NoError(t, err)
	assert.Contains(t, string(rulesData), "merged-1")

	a, err := warden.LoadArchive(warden.ArchivePath(dir))
	require.NoError(t, err)
	require.Len(t, a.Rules, 1)
	assert.Equal(t, "r1", a.Rules[0].ID)
	assert.Equal(t, "merged-1", a.Rules[0].SupersededBy)
}

// TestPersistRulesAndArchive_ArchiveFailureAbortsRulesSave verifies the
// load-bearing ordering invariant: if archive write fails, the active rules
// file must NOT be written. Otherwise the smelter would commit a rules file
// whose superseded entries have no archive record (bead-contract violation).
func TestPersistRulesAndArchive_ArchiveFailureAbortsRulesSave(t *testing.T) {
	dir := t.TempDir()

	// Force archive write to fail by creating a *directory* at the archive
	// file path. os.WriteFile inside Archive.Save will then return EISDIR.
	require.NoError(t, os.MkdirAll(filepath.Dir(warden.ArchivePath(dir)), 0o755))
	require.NoError(t, os.MkdirAll(warden.ArchivePath(dir), 0o755))

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "merged-1", Category: "style", Pattern: "p", Check: "c"},
	}}
	archived := []warden.Rule{
		{ID: "r1", Category: "style", Pattern: "p1", Check: "c1"},
	}
	summary := []warden.MergeResult{{
		Merged:      warden.Rule{ID: "merged-1", Category: "style"},
		ReplacedIDs: []string{"r1"},
		Category:    "style",
	}}

	err := persistRulesAndArchive(dir, rf,
		duplicateArchiveEntries(archived, summary, time.Now().UTC()), nil)
	require.Error(t, err, "archive failure must propagate")
	assert.Contains(t, err.Error(), "archiving rules")

	// Critical: the active rules file must not have been written.
	_, statErr := os.Stat(filepath.Join(dir, warden.RulesFileName))
	assert.True(t, os.IsNotExist(statErr),
		"active rules file must not be saved when archive write fails (got stat err: %v)", statErr)
}

// TestRunStaleness_DisabledByDefault verifies that without an
// archive-after-days option configured the staleness pass is a no-op.
func TestRunStaleness_DisabledByDefault(t *testing.T) {
	db := openTestDB(t)
	s := New(db, time.Hour, map[string]string{})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Category: "style", Pattern: "p", Check: "c", Added: "2020-01-01"},
	}}

	archived, protected := s.runStaleness(t.TempDir(), "anvil-a", rf, nil)
	assert.Empty(t, archived)
	assert.Empty(t, protected)
	assert.Len(t, rf.Rules, 1, "rf.Rules must be untouched when the staleness pass is disabled")
}

// TestRunStaleness_ZeroThresholdSkipsPass verifies that a threshold of zero
// or negative disables the staleness pass even when WithArchiveAfterDays is
// configured.
func TestRunStaleness_ZeroThresholdSkipsPass(t *testing.T) {
	db := openTestDB(t)
	s := New(db, time.Hour, map[string]string{},
		WithArchiveAfterDays(func() int { return 0 }),
	)

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Added: "2020-01-01"},
	}}

	archived, protected := s.runStaleness(t.TempDir(), "anvil-a", rf, nil)
	assert.Empty(t, archived)
	assert.Empty(t, protected)
	assert.Len(t, rf.Rules, 1)
}

// TestRunStaleness_MovesOldRulesAndUpdatesRulesFile verifies the staleness
// pass partitions rules correctly, mutates rf.Rules in place so Pass 3 only
// operates on actives, and returns ArchivedRule entries with reason=stale.
func TestRunStaleness_MovesOldRulesAndUpdatesRulesFile(t *testing.T) {
	db := openTestDB(t)
	s := New(db, time.Hour, map[string]string{},
		WithArchiveAfterDays(func() int { return 180 }),
	)

	fresh := warden.Rule{ID: "fresh", Category: "style", Pattern: "p1", Check: "c1",
		Added: time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02")}
	stale := warden.Rule{ID: "stale", Category: "style", Pattern: "p2", Check: "c2",
		Added: "2020-01-01"}
	rf := &warden.RulesFile{Rules: []warden.Rule{fresh, stale}}

	archived, protected := s.runStaleness(t.TempDir(), "anvil-a", rf, nil)
	assert.Empty(t, protected, "nothing in the archive points at either rule")
	require.Len(t, archived, 1)
	assert.Equal(t, "stale", archived[0].ID)
	assert.Equal(t, warden.ArchiveReasonStale, archived[0].ArchiveReason)
	assert.False(t, archived[0].LastSeen.IsZero(), "LastSeen should be set on archived stale entries")

	// rf.Rules should now contain only the fresh rule so Pass 3 sees only actives.
	require.Len(t, rf.Rules, 1)
	assert.Equal(t, "fresh", rf.Rules[0].ID)
}

// TestArchiveRules_WritesStaleEntries verifies that archiveRules persists
// Pass 2 stale entries into the archive store alongside Pass 1 duplicates,
// preserving their pre-built reason and LastSeen values.
func TestArchiveRules_WritesStaleEntries(t *testing.T) {
	dir := t.TempDir()
	staleAt := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	staleArchived := []warden.ArchivedRule{
		{
			Rule:          warden.Rule{ID: "old", Category: "style", Pattern: "p", Check: "c", Added: "2020-01-01"},
			LastSeen:      staleAt,
			ArchivedAt:    staleAt,
			ArchiveReason: warden.ArchiveReasonStale,
		},
	}

	require.NoError(t, archiveRules(dir, nil, staleArchived))

	a, err := warden.LoadArchive(warden.ArchivePath(dir))
	require.NoError(t, err)
	require.Len(t, a.Rules, 1)
	assert.Equal(t, "old", a.Rules[0].ID)
	assert.Equal(t, warden.ArchiveReasonStale, a.Rules[0].ArchiveReason)
	assert.Equal(t, "", a.Rules[0].SupersededBy)
	assert.Equal(t, staleAt, a.Rules[0].LastSeen)
}

// TestArchiveRules_CombinesPass1AndPass2Entries verifies that a single
// archive write captures both duplicates (Pass 1) and stale entries (Pass 2).
func TestArchiveRules_CombinesPass1AndPass2Entries(t *testing.T) {
	dir := t.TempDir()

	dupArchived := []warden.Rule{
		{ID: "dup1", Category: "style", Pattern: "p1", Check: "c1"},
	}
	summary := []warden.MergeResult{{
		Merged:      warden.Rule{ID: "merged-1", Category: "style"},
		ReplacedIDs: []string{"dup1"},
		Category:    "style",
	}}
	staleArchived := []warden.ArchivedRule{
		{
			Rule:          warden.Rule{ID: "stale1", Category: "style", Added: "2020-01-01"},
			LastSeen:      time.Now().UTC(),
			ArchivedAt:    time.Now().UTC(),
			ArchiveReason: warden.ArchiveReasonStale,
		},
	}

	require.NoError(t, archiveRules(dir, duplicateArchiveEntries(dupArchived, summary, time.Now().UTC()), staleArchived))

	a, err := warden.LoadArchive(warden.ArchivePath(dir))
	require.NoError(t, err)
	require.Len(t, a.Rules, 2)

	byID := make(map[string]warden.ArchivedRule, len(a.Rules))
	for _, r := range a.Rules {
		byID[r.ID] = r
	}
	require.Contains(t, byID, "dup1")
	assert.Equal(t, warden.ArchiveReasonDuplicate, byID["dup1"].ArchiveReason)
	assert.Equal(t, "merged-1", byID["dup1"].SupersededBy)

	require.Contains(t, byID, "stale1")
	assert.Equal(t, warden.ArchiveReasonStale, byID["stale1"].ArchiveReason)
	assert.Equal(t, "", byID["stale1"].SupersededBy)
}

// TestPersistRulesAndArchive_StaleOnlyWritesArchive verifies the load-bearing
// invariant for Pass 2: when only stale rules need archiving (no Pass 1
// activity), both the archive file and the active rules file are written.
func TestPersistRulesAndArchive_StaleOnlyWritesArchive(t *testing.T) {
	dir := t.TempDir()
	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "kept", Category: "style", Pattern: "p", Check: "c"},
	}}
	staleArchived := []warden.ArchivedRule{
		{
			Rule:          warden.Rule{ID: "old", Category: "style", Added: "2020-01-01"},
			LastSeen:      time.Now().UTC(),
			ArchivedAt:    time.Now().UTC(),
			ArchiveReason: warden.ArchiveReasonStale,
		},
	}

	require.NoError(t, persistRulesAndArchive(dir, rf, nil, staleArchived))

	rulesData, err := os.ReadFile(filepath.Join(dir, warden.RulesFileName))
	require.NoError(t, err)
	assert.Contains(t, string(rulesData), "kept")

	a, err := warden.LoadArchive(warden.ArchivePath(dir))
	require.NoError(t, err)
	require.Len(t, a.Rules, 1)
	assert.Equal(t, "old", a.Rules[0].ID)
	assert.Equal(t, warden.ArchiveReasonStale, a.Rules[0].ArchiveReason)
}

// TestPersistRulesAndArchive_NoArchiveSkipsArchiveStep verifies that when
// there are no consolidated rules to archive, the archive file is not
// created — only the active rules file is written.
func TestPersistRulesAndArchive_NoArchiveSkipsArchiveStep(t *testing.T) {
	dir := t.TempDir()
	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Category: "style", Pattern: "p", Check: "c"},
	}}

	require.NoError(t, persistRulesAndArchive(dir, rf, nil, nil))

	_, err := os.Stat(filepath.Join(dir, warden.RulesFileName))
	assert.NoError(t, err, "rules file should be saved")

	_, err = os.Stat(warden.ArchivePath(dir))
	assert.True(t, os.IsNotExist(err), "archive file should not be created when nothing to archive")
}

// TestCommitAndPush_FreshWorktreeWithExistingRemoteBranch verifies that
// commitAndPush succeeds when the batch branch already exists on origin but
// the local worktree has no remote-tracking ref (fresh creation path). The
// pre-push fetch must populate refs/remotes/origin/<branch> so that
// --force-with-lease can verify the lease correctly.
func TestCommitAndPush_FreshWorktreeWithExistingRemoteBranch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping git integration test in short mode")
	}

	ctx := context.Background()

	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = filteredEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// --- Set up a bare "origin" repo with an initial commit on main ---
	originDir := t.TempDir()
	runGit(originDir, "init", "--bare", "--initial-branch=main")

	// Seed main via a temporary clone.
	seedDir := t.TempDir()
	runGit(seedDir, "clone", originDir, ".")
	runGit(seedDir, "config", "user.email", "test@example.com")
	runGit(seedDir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "README"), []byte("test\n"), 0o644))
	runGit(seedDir, "add", "README")
	runGit(seedDir, "commit", "-m", "init")
	runGit(seedDir, "push", "origin", "main")

	// Push the batch branch to origin (simulating a prior smelter run).
	branch := branchForAnvil("test-anvil")
	runGit(seedDir, "checkout", "-b", branch)
	require.NoError(t, os.MkdirAll(filepath.Join(seedDir, ".forge"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(seedDir, warden.RulesFileName),
		[]byte("rules: []\n"), 0o644))
	runGit(seedDir, "add", warden.RulesFileName)
	runGit(seedDir, "commit", "-m", "initial rules")
	runGit(seedDir, "push", "origin", branch)

	// --- Fresh local worktree: cloned from origin, batch branch created
	// locally without fetching it, so there is no remote-tracking ref. ---
	localDir := t.TempDir()
	runGit(localDir, "clone", originDir, ".")
	runGit(localDir, "config", "user.email", "test@example.com")
	runGit(localDir, "config", "user.name", "Test")
	// Create the local branch without setting upstream tracking.
	runGit(localDir, "checkout", "-b", branch)

	// The clone above fetches all remote branches, so refs/remotes/origin/<branch>
	// already exists. Explicitly delete it to simulate a fresh worktree where only
	// the local branch was created (e.g. via git worktree add) without fetching.
	// This is the exact condition that caused --force-with-lease to reject the push.
	delRef := exec.Command("git", "update-ref", "-d", "refs/remotes/origin/"+branch)
	delRef.Dir = localDir
	delRef.Env = filteredEnv()
	require.NoError(t, delRef.Run(), "should be able to delete remote-tracking ref")

	// Assert the remote-tracking ref is now absent — without the pre-push fetch,
	// git push --force-with-lease would treat this as "no lease" and reject the push
	// even though the branch exists on origin.
	checkRef := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+branch)
	checkRef.Dir = localDir
	checkRef.Env = filteredEnv()
	err := checkRef.Run()
	require.Error(t, err, "remote-tracking ref should be absent before commitAndPush")

	// Write the updated rules file so commitAndPush can stage and commit it.
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, ".forge"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(localDir, warden.RulesFileName),
		[]byte("rules:\n  - id: r1\n    category: style\n    pattern: foo\n    check: bar\n"),
		0o644))

	db := openTestDB(t)
	s := New(db, time.Hour, map[string]string{"test-anvil": localDir})

	// commitAndPush must succeed: the fetch populates the remote-tracking ref
	// so --force-with-lease can verify the lease and allow the push.
	err = s.commitAndPush(ctx, localDir, branch, PassResults{Added: []string{"r1"}})
	require.NoError(t, err, "commitAndPush should succeed after fetching remote-tracking ref")
}

// TestRunStaleness_ProtectsASupersessionTerminusFromTheAnvilsArchive is the
// guard end to end through the flush path: the index comes from the archive
// file on disk beside the rules file, not from anything the caller passes, so
// this is the one case that exercises the read.
func TestRunStaleness_ProtectsASupersessionTerminusFromTheAnvilsArchive(t *testing.T) {
	db := openTestDB(t)
	wt := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(wt, ".forge"), 0o755))

	// Two rules were merged into "terminus" and archived; "plain" was archived
	// for staleness and so names no successor.
	archive := &warden.Archive{Rules: []warden.ArchivedRule{
		{Rule: warden.Rule{ID: "member-a"}, SupersededBy: "terminus", ArchiveReason: warden.ArchiveReasonDuplicate},
		{Rule: warden.Rule{ID: "member-b"}, SupersededBy: "terminus", ArchiveReason: warden.ArchiveReasonDuplicate},
		{Rule: warden.Rule{ID: "long-gone"}, ArchiveReason: warden.ArchiveReasonStale},
	}}
	require.NoError(t, archive.Save(warden.ArchivePath(wt)))

	old := func(id string) warden.Rule {
		return warden.Rule{ID: id, Category: "style", Pattern: "p", Check: "c", Added: "2020-01-01"}
	}
	newRules := func() *warden.RulesFile {
		return &warden.RulesFile{Rules: []warden.Rule{old("terminus"), old("plain")}}
	}

	s := New(db, time.Hour, map[string]string{},
		WithArchiveAfterDays(func() int { return 180 }),
	)
	rf := newRules()
	archived, protected := s.runStaleness(wt, "anvil-a", rf, nil)

	require.Len(t, archived, 1)
	assert.Equal(t, "plain", archived[0].ID)
	require.Len(t, protected, 1)
	assert.Equal(t, "terminus", protected[0].ID)
	assert.Equal(t, []string{"terminus"}, ruleIDs(rf.Rules),
		"a protected terminus stays on the active file")

	// The override archives it, and the protected list is then empty — which
	// is what makes the summary line disappear rather than repeat every flush.
	forced := New(db, time.Hour, map[string]string{},
		WithArchiveAfterDays(func() int { return 180 }),
		WithAllowArchiveTerminus(func() bool { return true }),
	)
	rf = newRules()
	archived, protected = forced.runStaleness(wt, "anvil-a", rf, nil)
	assert.Equal(t, []string{"terminus", "plain"}, archivedRuleIDs(archived))
	assert.Empty(t, protected)
	assert.Empty(t, rf.Rules)
}

// TestRunStaleness_MissingArchiveStillSweeps is the compatibility floor: an
// anvil that has never archived anything has no archive file, and the sweep
// must behave exactly as it did before the guard existed rather than declining
// to run without evidence.
func TestRunStaleness_MissingArchiveStillSweeps(t *testing.T) {
	db := openTestDB(t)
	s := New(db, time.Hour, map[string]string{},
		WithArchiveAfterDays(func() int { return 180 }),
	)

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "old", Category: "style", Pattern: "p", Check: "c", Added: "2020-01-01"},
	}}
	archived, protected := s.runStaleness(t.TempDir(), "anvil-a", rf, nil)

	require.Len(t, archived, 1)
	assert.Equal(t, "old", archived[0].ID)
	assert.Empty(t, protected)
}

// TestRunStaleness_InactivityHalfKeepsAnEmittedRule is the two-signal
// predicate reaching the flush path: the thresholds resolve from the option
// closures, and a rule the review selection stamped last week survives a
// sweep its Added date alone would have taken.
func TestRunStaleness_InactivityHalfKeepsAnEmittedRule(t *testing.T) {
	db := openTestDB(t)
	s := New(db, time.Hour, map[string]string{},
		WithArchiveAfterDays(func() int { return 90 }),
		WithInactiveAfterDays(func() int { return 30 }),
	)

	now := time.Now().UTC()
	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "used", Category: "style", Pattern: "p", Check: "c",
			Added:       now.AddDate(0, 0, -400).Format("2006-01-02"),
			LastEmitted: now.AddDate(0, 0, -7).Format("2006-01-02")},
		{ID: "silent", Category: "style", Pattern: "p", Check: "c",
			Added:       now.AddDate(0, 0, -400).Format("2006-01-02"),
			LastEmitted: now.AddDate(0, 0, -200).Format("2006-01-02")},
	}}
	archived, protected := s.runStaleness(t.TempDir(), "anvil-a", rf, nil)

	assert.Equal(t, []string{"silent"}, archivedRuleIDs(archived))
	assert.Empty(t, protected)
	assert.Equal(t, []string{"used"}, ruleIDs(rf.Rules))
}

func archivedRuleIDs(rules []warden.ArchivedRule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.ID)
	}
	return out
}

// terminusEventMessages returns the smelter_flushed rows announcing a rule the
// terminus guard held, oldest first, so a test can assert on what the line
// SAYS and not only on how many of them there are.
func terminusEventMessages(t *testing.T, db *state.DB) []string {
	t.Helper()
	events, err := db.RecentEvents(200)
	require.NoError(t, err)
	var msgs []string
	for _, e := range events {
		if strings.Contains(e.Message, "supersession terminus rule") {
			msgs = append(msgs, e.Message)
		}
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs
}

// countTerminusEvents counts the smelter_flushed rows announcing a rule the
// terminus guard held, which is the surface the announcer suppresses.
func countTerminusEvents(t *testing.T, db *state.DB) int {
	t.Helper()
	events, err := db.RecentEvents(200)
	require.NoError(t, err)
	n := 0
	for _, e := range events {
		if strings.Contains(e.Message, "supersession terminus rule") {
			n++
		}
	}
	return n
}

// A protected terminus is a condition only a human can clear: the sweep leaves
// the rule untouched, so the next flush finds it in exactly the same state.
// Unsuppressed, one held rule produces one log line and one feed row per flush
// cycle forever, burying the rows that report actual changes — the same failure
// contradictionAnnouncer exists to prevent, on the same code path.
func TestRunStaleness_ProtectedTerminusAnnouncedOnceButAlwaysReported(t *testing.T) {
	db := openTestDB(t)
	wt := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(wt, ".forge"), 0o755))
	archive := &warden.Archive{Rules: []warden.ArchivedRule{
		{Rule: warden.Rule{ID: "member-a"}, SupersededBy: "terminus", ArchiveReason: warden.ArchiveReasonDuplicate},
	}}
	require.NoError(t, archive.Save(warden.ArchivePath(wt)))

	s := New(db, time.Hour, map[string]string{},
		WithArchiveAfterDays(func() int { return 180 }),
	)
	newRules := func() *warden.RulesFile {
		return &warden.RulesFile{Rules: []warden.Rule{
			{ID: "terminus", Category: "style", Pattern: "p", Check: "c", Added: "2020-01-01"},
		}}
	}

	_, protected := s.runStaleness(wt, "anvil-a", newRules(), nil)
	require.Equal(t, []string{"terminus"}, ruleIDs(protected))
	require.Equal(t, 1, countTerminusEvents(t, db), "the held rule is announced on the flush that finds it")

	_, protected = s.runStaleness(wt, "anvil-a", newRules(), nil)
	assert.Equal(t, []string{"terminus"}, ruleIDs(protected),
		"the full set is still returned: the commit and PR bodies describe the file, not the delta")
	assert.Equal(t, 1, countTerminusEvents(t, db),
		"a rule already announced does not produce a second feed row")

	// A different anvil is a different condition for an operator to act on,
	// so it is announced in its own right.
	_, protected = s.runStaleness(wt, "anvil-b", newRules(), nil)
	require.Len(t, protected, 1)
	assert.Equal(t, 2, countTerminusEvents(t, db))
}

// The suppression is over WHETHER the line is emitted, never over what it
// counts. The protected set grows one rule at a time — a rule enters it the
// day its inactivity window expires — so on every flush after the first the
// newly held rules are a strict subset of what the sweep is holding. Counted
// from that subset, the log line and the feed row report a smaller sweep than
// the commit body and the PR body of the very same run, which list the full
// set from PassResults.ProtectedTermini.
func TestRunStaleness_AnnouncedLineCountsTheWholeProtectedSet(t *testing.T) {
	db := openTestDB(t)
	wt := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(wt, ".forge"), 0o755))
	archive := &warden.Archive{Rules: []warden.ArchivedRule{
		{Rule: warden.Rule{ID: "member-a"}, SupersededBy: "terminus-a", ArchiveReason: warden.ArchiveReasonDuplicate},
		{Rule: warden.Rule{ID: "member-b"}, SupersededBy: "terminus-b", ArchiveReason: warden.ArchiveReasonDuplicate},
	}}
	require.NoError(t, archive.Save(warden.ArchivePath(wt)))

	s := New(db, time.Hour, map[string]string{},
		WithArchiveAfterDays(func() int { return 180 }),
	)
	rules := func(ids ...string) *warden.RulesFile {
		rf := &warden.RulesFile{}
		for _, id := range ids {
			rf.Rules = append(rf.Rules,
				warden.Rule{ID: id, Category: "style", Pattern: "p", Check: "c", Added: "2020-01-01"})
		}
		return rf
	}

	_, protected := s.runStaleness(wt, "anvil-a", rules("terminus-a"), nil)
	require.Equal(t, []string{"terminus-a"}, ruleIDs(protected))

	// terminus-b has now aged into protection. It is the only news, but the
	// sweep is holding two.
	_, protected = s.runStaleness(wt, "anvil-a", rules("terminus-a", "terminus-b"), nil)
	require.Equal(t, []string{"terminus-a", "terminus-b"}, ruleIDs(protected))

	msgs := terminusEventMessages(t, db)
	require.Len(t, msgs, 2, "the second flush is announced: it holds a rule nobody has been told about")
	assert.Contains(t, msgs[0], "Kept 1 supersession terminus rule for anvil-a:")
	assert.NotContains(t, msgs[0], "newly held",
		"where every protected rule is new the total already says so")
	assert.Contains(t, msgs[1], "Kept 2 supersession terminus rules for anvil-a (1 newly held: terminus-b):",
		"the count is the sweep's, and the delta the suppression is about is named inside it")
}

// The index the guard reads must include the supersessions THIS run has
// decided on and not yet written. A merged rule carries its members' usage
// stamps — empty when they were never emitted, which is the population of old
// near-duplicates Pass 1 exists to fold — so once its own MergedAt has aged
// past the threshold it is aged AND inactive with nothing having used it.
// Reading the archive alone, Pass 2 archives it while every rule it stands in
// for is already archived, retiring the whole chain with nothing left on the
// active file to say what went. The fixture below is such a rule: an ancient
// anchor, no usage, and a supersession the archive on disk knows nothing of.
func TestRunStaleness_DoesNotRetireAChainThisRunJustCreated(t *testing.T) {
	db := openTestDB(t)
	wt := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(wt, ".forge"), 0o755))

	s := New(db, time.Hour, map[string]string{},
		WithArchiveAfterDays(func() int { return 180 }),
	)
	// Pass 1 merged two never-emitted rules into "merged" on an earlier run
	// and its own merge stamp has since aged past the threshold; the archive
	// on disk still knows nothing about the supersession.
	pending := []warden.MergeResult{{
		Merged:      warden.Rule{ID: "merged"},
		ReplacedIDs: []string{"member-a", "member-b"},
	}}
	// The shape applyClusters actually emits: Added inherited from the newest
	// member, MergedAt stamped at the fold. Both are past the threshold, so
	// the rule is aged whichever anchor IsStale reads — without MergedAt the
	// fixture would model a rule production can no longer produce, and the
	// second half below would be asserting on the Added fallback rather than
	// on the anchor a merged rule really carries.
	merged := warden.Rule{ID: "merged", Category: "style", Pattern: "p", Check: "c",
		Added:    time.Now().UTC().AddDate(0, 0, -500).Format("2006-01-02"),
		MergedAt: time.Now().UTC().AddDate(0, 0, -400).Format("2006-01-02")}

	rf := &warden.RulesFile{Rules: []warden.Rule{merged}}
	archived, protected := s.runStaleness(wt, "anvil-a", rf, pending)
	assert.Empty(t, archived, "the merged rule holds a chain this run created")
	assert.Equal(t, []string{"merged"}, ruleIDs(protected))
	assert.Equal(t, []string{"merged"}, ruleIDs(rf.Rules))

	// Without the pending supersessions it is swept even though every rule it
	// stands in for is already archived, which is the defect the seeding
	// closes. Its own merge stamp is what makes it eligible at all: MergedAt
	// no longer shields a chain whose fold has aged, so the guard is the only
	// thing holding it.
	rf = &warden.RulesFile{Rules: []warden.Rule{merged}}
	archived, protected = s.runStaleness(wt, "anvil-b", rf, nil)
	assert.Equal(t, []string{"merged"}, archivedRuleIDs(archived))
	assert.Empty(t, protected)
}

// A merge whose replaced rule shares the merged rule's ID says nothing about
// anything standing behind it — the same degenerate record
// BuildSupersededByIndex drops from the archive — so the pending half must be
// filtered on the same rules rather than protecting a rule on the strength of
// its own retirement.
func TestSupersessionIndex_DropsSelfReferentialAndEmptyPendingEntries(t *testing.T) {
	idx := supersessionIndex(t.TempDir(), "anvil-a", []warden.MergeResult{
		{Merged: warden.Rule{ID: "self"}, ReplacedIDs: []string{"self"}},
		{Merged: warden.Rule{ID: ""}, ReplacedIDs: []string{"orphan"}},
		{Merged: warden.Rule{ID: "real"}, ReplacedIDs: []string{"member"}},
	})
	assert.Equal(t, map[string][]string{"real": {"member"}}, idx)
}
