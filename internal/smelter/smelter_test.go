package smelter

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	require.NoError(t, delRef.Run(), "should be able to delete remote-tracking ref")

	// Assert the remote-tracking ref is now absent — without the pre-push fetch,
	// git push --force-with-lease would treat this as "no lease" and reject the push
	// even though the branch exists on origin.
	checkRef := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+branch)
	checkRef.Dir = localDir
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
	err = s.commitAndPush(ctx, localDir, branch, 1)
	require.NoError(t, err, "commitAndPush should succeed after fetching remote-tracking ref")
}
