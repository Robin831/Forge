package smelter

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	assert.Equal(t, "2024-01-01", summary[0].Merged.Added, "merged Added should be the oldest in cluster")
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

	require.NoError(t, archiveRules(dir, archived, summary, nil))

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

	require.NoError(t, persistRulesAndArchive(dir, rf, archived, summary, nil))

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

	err := persistRulesAndArchive(dir, rf, archived, summary, nil)
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

	archived := s.runStaleness("anvil-a", rf)
	assert.Empty(t, archived)
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

	archived := s.runStaleness("anvil-a", rf)
	assert.Empty(t, archived)
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

	archived := s.runStaleness("anvil-a", rf)
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

	require.NoError(t, archiveRules(dir, nil, nil, staleArchived))

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

	require.NoError(t, archiveRules(dir, dupArchived, summary, staleArchived))

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

	require.NoError(t, persistRulesAndArchive(dir, rf, nil, nil, staleArchived))

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

	require.NoError(t, persistRulesAndArchive(dir, rf, nil, nil, nil))

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
