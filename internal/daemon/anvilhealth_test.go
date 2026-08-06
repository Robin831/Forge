package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/anvilhealth"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/worktree"
)

// scriptedRunner is an anvilhealth.Runner whose dolt_conflicts reply can be
// swapped between polls so a test can walk the healthy → wedged → healthy state
// machine. Non-conflict queries (divergence) are answered from a fixed script.
type scriptedRunner struct {
	mu        sync.Mutex
	conflicts string
	err       error
	calls     int32
}

func (s *scriptedRunner) run(_ context.Context, _ string, query string) ([]byte, error) {
	if strings.Contains(query, "dolt_conflicts") {
		atomic.AddInt32(&s.calls, 1)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.err != nil {
			return nil, s.err
		}
		return []byte(s.conflicts), nil
	}
	switch {
	case strings.Contains(query, "active_branch() AS branch"):
		return []byte(`[{"branch":"beads-sync"}]`), nil
	case strings.Contains(query, "FROM dolt_branches"):
		return []byte(`[{"remote":"origin","branch":"beads-sync"}]`), nil
	case strings.Contains(query, "dolt_log('remotes/origin/beads-sync..beads-sync')"):
		return []byte(`[{"n":1}]`), nil
	case strings.Contains(query, "dolt_log('beads-sync..remotes/origin/beads-sync')"):
		return []byte(`[{"n":10}]`), nil
	}
	return nil, errors.New("unexpected query: " + query)
}

func (s *scriptedRunner) set(conflicts string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conflicts, s.err = conflicts, err
}

// newHealthTestDaemon builds a Daemon wired to a temp state.db, a scripted
// conflict probe, and a log buffer so log assertions are possible.
func newHealthTestDaemon(t *testing.T, runner *scriptedRunner) (*Daemon, *state.DB, *bytes.Buffer) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "forge-anvil-health-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	logs := &bytes.Buffer{}
	d := &Daemon{
		db:          db,
		logger:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		anvilHealth: &anvilhealth.Checker{Run: runner.run},
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{"munin": {Path: tmpDir}},
	})
	return d, db, logs
}

func wedgedEventCount(t *testing.T, db *state.DB, kind state.EventType) int {
	t.Helper()
	events, err := db.RecentEvents(100)
	require.NoError(t, err)
	n := 0
	for _, e := range events {
		if e.Type == kind {
			n++
		}
	}
	return n
}

// TestCheckAnvilHealth_StateMachine walks the full lifecycle: a healthy anvil is
// silent, a wedge raises exactly one entry with one WARN and one event, repeat
// polls update in place, a failed probe leaves the state untouched, and
// resolution clears the entry with no operator action.
func TestCheckAnvilHealth_StateMachine(t *testing.T) {
	runner := &scriptedRunner{conflicts: `[]`}
	d, db, logs := newHealthTestDaemon(t, runner)
	cfg := d.cfg.Load()

	t.Run("healthy anvil: no entry, no noise", func(t *testing.T) {
		logs.Reset()
		d.checkAnvilHealth(context.Background(), cfg)

		rows, err := db.WedgedAnvils()
		require.NoError(t, err)
		assert.Empty(t, rows)
		assert.NotContains(t, logs.String(), "level=WARN")
		assert.Zero(t, wedgedEventCount(t, db, state.EventAnvilWedged))
		assert.Zero(t, wedgedEventCount(t, db, state.EventAnvilRecovered))
	})

	t.Run("wedge raises one entry with a WARN naming tables, count and divergence", func(t *testing.T) {
		logs.Reset()
		runner.set(`[{"conflict_table":"issues","conflict_count":3}]`, nil)
		d.checkAnvilHealth(context.Background(), cfg)

		rows, err := db.WedgedAnvils()
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "munin", rows[0].Anvil)
		assert.Equal(t, "issues (3)", rows[0].ConflictTables)
		assert.Equal(t, 3, rows[0].ConflictCount)
		assert.Equal(t, 1, rows[0].Ahead)
		assert.Equal(t, 10, rows[0].Behind)
		assert.True(t, rows[0].DivergenceKnown)

		out := logs.String()
		assert.Contains(t, out, "level=WARN")
		assert.Contains(t, out, "anvil=munin")
		assert.Contains(t, out, "issues (3)")
		assert.Contains(t, out, "ahead 1 / behind 10")
		assert.Equal(t, 1, wedgedEventCount(t, db, state.EventAnvilWedged))

		// It is also surfaced in needs-attention as an anvil-kind entry.
		items, err := db.NeedsAttentionBeads(5, 5, 3)
		require.NoError(t, err)
		var anvilItems int
		for _, it := range items {
			if it.Kind == state.AttentionKindAnvil {
				anvilItems++
				assert.Contains(t, it.Reason, "issues (3)")
				assert.Contains(t, it.Reason, "ahead 1 / behind 10")
			}
		}
		assert.Equal(t, 1, anvilItems)
	})

	t.Run("repeat poll updates in place without duplicating", func(t *testing.T) {
		logs.Reset()
		runner.set(`[{"conflict_table":"issues","conflict_count":5}]`, nil)
		d.checkAnvilHealth(context.Background(), cfg)

		rows, err := db.WedgedAnvils()
		require.NoError(t, err)
		require.Len(t, rows, 1, "a persisting wedge must update, not duplicate")
		assert.Equal(t, 5, rows[0].ConflictCount)
		// One event per wedge, not one per poll.
		assert.Equal(t, 1, wedgedEventCount(t, db, state.EventAnvilWedged))
		// The WARN is rate-limited, so the immediate follow-up poll is quiet.
		assert.NotContains(t, logs.String(), "level=WARN")
	})

	t.Run("failed probe leaves the existing flag untouched", func(t *testing.T) {
		logs.Reset()
		runner.set("", errors.New("dial tcp 127.0.0.1:3306: connection refused"))
		d.checkAnvilHealth(context.Background(), cfg)

		rows, err := db.WedgedAnvils()
		require.NoError(t, err)
		require.Len(t, rows, 1, "an inconclusive probe must never clear a real wedge")
		assert.Equal(t, 5, rows[0].ConflictCount, "the last known detail must be preserved")
		assert.Zero(t, wedgedEventCount(t, db, state.EventAnvilRecovered))
		assert.Contains(t, logs.String(), "inconclusive")
	})

	t.Run("resolution clears the entry automatically", func(t *testing.T) {
		logs.Reset()
		runner.set(`[]`, nil)
		d.checkAnvilHealth(context.Background(), cfg)

		rows, err := db.WedgedAnvils()
		require.NoError(t, err)
		assert.Empty(t, rows)
		assert.Equal(t, 1, wedgedEventCount(t, db, state.EventAnvilRecovered))
		assert.Contains(t, logs.String(), "anvil recovered")

		items, err := db.NeedsAttentionBeads(5, 5, 3)
		require.NoError(t, err)
		for _, it := range items {
			assert.NotEqual(t, state.AttentionKindAnvil, it.Kind)
		}
	})

	t.Run("still-healthy poll after recovery is silent", func(t *testing.T) {
		logs.Reset()
		d.checkAnvilHealth(context.Background(), cfg)
		assert.Equal(t, 1, wedgedEventCount(t, db, state.EventAnvilRecovered),
			"recovery must be logged exactly once")
		assert.NotContains(t, logs.String(), "anvil recovered")
	})
}

// TestCheckAnvilHealth_Disabled verifies the config kill switch: no probe runs
// and no flag is touched when anvil_health_check is false.
func TestCheckAnvilHealth_Disabled(t *testing.T) {
	runner := &scriptedRunner{conflicts: `[{"conflict_table":"issues","conflict_count":3}]`}
	d, db, _ := newHealthTestDaemon(t, runner)

	disabled := false
	cfg := d.cfg.Load()
	cfg.Settings.AnvilHealthCheck = &disabled
	d.cfg.Store(cfg)

	d.checkAnvilHealth(context.Background(), cfg)
	assert.Zero(t, atomic.LoadInt32(&runner.calls), "the probe must not run when disabled")
	rows, err := db.WedgedAnvils()
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// TestCheckAnvilHealth_SkipsAnvilsWithoutPath guards against probing a
// misconfigured anvil (which would fail on every poll and log forever).
func TestCheckAnvilHealth_SkipsAnvilsWithoutPath(t *testing.T) {
	runner := &scriptedRunner{conflicts: `[]`}
	d, _, _ := newHealthTestDaemon(t, runner)
	d.cfg.Store(&config.Config{Anvils: map[string]config.AnvilConfig{"munin": {Path: ""}}})

	d.checkAnvilHealth(context.Background(), d.cfg.Load())
	assert.Zero(t, atomic.LoadInt32(&runner.calls))
}

// TestWedgedAnvilSet_AndDispatchReason verifies the dispatch gate's inputs: the
// wedged set is keyed by anvil and the rendered reason explains the block.
func TestWedgedAnvilSet_AndDispatchReason(t *testing.T) {
	runner := &scriptedRunner{conflicts: `[]`}
	d, db, _ := newHealthTestDaemon(t, runner)

	assert.Empty(t, d.wedgedAnvilSet(), "a healthy forge blocks nothing")
	assert.Empty(t, d.wedgedAnvilError("munin"))

	_, _, err := db.MarkAnvilWedged(state.AnvilHealth{
		Anvil:          "munin",
		ConflictTables: "issues (3)",
		ConflictCount:  3,
		Detail:         "Beads database is mid-merge with unresolved conflicts",
	})
	require.NoError(t, err)

	set := d.wedgedAnvilSet()
	require.Contains(t, set, "munin")
	assert.NotContains(t, set, "hugin")

	reason := d.wedgedAnvilError("munin")
	assert.Contains(t, reason, `anvil "munin" is wedged`)
	assert.Contains(t, reason, "mid-merge")
	assert.Empty(t, d.wedgedAnvilError("hugin"))
	assert.Empty(t, d.wedgedAnvilError(""))

	// The gate is opt-out along with the check itself.
	disabled := false
	cfg := d.cfg.Load()
	cfg.Settings.AnvilHealthCheck = &disabled
	d.cfg.Store(cfg)
	assert.Empty(t, d.wedgedAnvilSet(), "disabling the check must also disable the gate")
}

// TestPollAndDispatch_HealthCheckOnlyOnFullPoll pins the placement of the check:
// a wedge is a minutes-to-hours condition, so the probe belongs on the full poll
// and must not be paid for on every fast cycle.
func TestPollAndDispatch_HealthCheckOnlyOnFullPoll(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-health-poll-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	// A stub bd on PATH so the poller itself succeeds without a real beads setup.
	// The conflict probe never reaches it — it goes through the injected runner.
	bdScript := filepath.Join(tmpDir, "bd")
	bdContent := "#!/bin/sh\necho '[]'\nexit 0\n"
	if runtime.GOOS == "windows" {
		bdScript = filepath.Join(tmpDir, "bd.bat")
		bdContent = "@echo off\necho []\nexit /b 0\n"
	}
	require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))
	oldPath := os.Getenv("PATH")
	require.NoError(t, os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath))
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })

	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	runner := &scriptedRunner{conflicts: `[]`}
	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		anvilHealth:   &anvilhealth.Checker{Run: runner.run},
	}
	d.cfg.Store(&config.Config{
		Settings: config.SettingsConfig{
			MaxTotalSmiths: 1,
			PollInterval:   time.Second,
			// A positive crucible poll interval is what makes a fast poll stay
			// fast (otherwise every poll is promoted to a full one).
			CruciblePollInterval: time.Minute,
		},
		Anvils: map[string]config.AnvilConfig{"munin": {Path: tmpDir, AutoDispatch: "off"}},
	})

	d.pollAndDispatch(context.Background(), false)
	assert.Zero(t, atomic.LoadInt32(&runner.calls), "the fast poll must not run the conflict probe")

	d.pollAndDispatch(context.Background(), true)
	assert.Equal(t, int32(1), atomic.LoadInt32(&runner.calls), "the full poll must run the probe exactly once per anvil")
}

// TestShouldWarnWedged verifies the WARN rate limiter: the first call for an
// anvil is due, an immediate follow-up is not, and a recovery resets it so the
// next wedge warns again immediately.
func TestShouldWarnWedged(t *testing.T) {
	d := &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	assert.True(t, d.shouldWarnWedged("munin"))
	assert.False(t, d.shouldWarnWedged("munin"))
	assert.True(t, d.shouldWarnWedged("hugin"), "the limiter is per anvil")

	d.wedgedWarned.Delete("munin")
	assert.True(t, d.shouldWarnWedged("munin"))
}
