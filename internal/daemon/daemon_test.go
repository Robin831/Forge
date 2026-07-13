package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/crucible"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/lifecycle"
	"github.com/Robin831/Forge/internal/pipeline"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/schematic"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/Robin831/Forge/internal/vcs/github"
	"github.com/Robin831/Forge/internal/wicket"
	"github.com/Robin831/Forge/internal/worktree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleIPC_RunBead_Errors(t *testing.T) {
	// Setup a temporary forge directory
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Mock config
	cfg := &config.Config{
		Settings: config.SettingsConfig{
			MaxTotalSmiths: 1,
			PollInterval:   10 * time.Second,
		},
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {
				Path:         tmpDir,
				MaxSmiths:    1,
				AutoDispatch: "off",
			},
		},
	}

	// Create daemon with temporary DB
	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		reqTracker:    *ipc.NewRequestTracker("test-"),
	}
	d.cfg.Store(cfg)

	t.Run("invalid payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{
			Type:    "run_bead",
			Payload: []byte("invalid"),
		})
		assert.Equal(t, "error", resp.Type)

		var msg map[string]string
		err := json.Unmarshal(resp.Payload, &msg)
		assert.NoError(t, err)
		assert.Contains(t, msg["message"], "invalid run_bead payload")
	})

	t.Run("bead not found", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.RunBeadPayload{
			BeadID: "NON-EXISTENT",
		})
		resp := d.handleIPC(ipc.Command{
			Type:    "run_bead",
			Payload: payload,
		})
		assert.Equal(t, "error", resp.Type)

		var msg map[string]string
		err := json.Unmarshal(resp.Payload, &msg)
		assert.NoError(t, err)
		assert.Contains(t, msg["message"], "not found or not ready")
	})
}

func TestHandleIPC_RunBead_Success(t *testing.T) {
	// Setup a temporary forge directory
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create a fake bd script (cross-platform)
	var bdScript string
	var bdContent string
	if runtime.GOOS == "windows" {
		bdScript = filepath.Join(tmpDir, "bd.bat")
		bdContent = `@echo off
if "%1"=="ready" (
    echo [{"id": "TEST-1", "title": "Test Bead", "status": "ready", "priority": 1, "tags": ["test"]}]
    exit /b 0
)
if "%1"=="update" (
    echo {"id": "TEST-1", "status": "in_progress"}
    exit /b 0
)
exit /b 1
`
	} else {
		bdScript = filepath.Join(tmpDir, "bd")
		bdContent = `#!/bin/sh
if [ "$1" = "ready" ]; then
    echo '[{"id": "TEST-1", "title": "Test Bead", "status": "ready", "priority": 1, "tags": ["test"]}]'
    exit 0
fi
if [ "$1" = "update" ]; then
    echo '{"id": "TEST-1", "status": "in_progress"}'
    exit 0
fi
exit 1
`
	}
	err = os.WriteFile(bdScript, []byte(bdContent), 0o755)
	require.NoError(t, err)

	// Add tmpDir to PATH
	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
	defer os.Setenv("PATH", oldPath)

	// Mock config
	cfg := &config.Config{
		Settings: config.SettingsConfig{
			MaxTotalSmiths: 1,
			PollInterval:   10 * time.Second,
		},
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {
				Path:         tmpDir,
				MaxSmiths:    1,
				AutoDispatch: "off",
			},
		},
	}

	// Create daemon with temporary DB
	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
		reqTracker:    *ipc.NewRequestTracker("test-"),
	}
	d.cfg.Store(cfg)

	t.Run("successful dispatch via poll fallback", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.RunBeadPayload{
			BeadID: "TEST-1",
			Anvil:  "test-anvil",
		})
		resp := d.handleIPC(ipc.Command{
			Type:    "run_bead",
			Payload: payload,
		})
		assert.Equal(t, "ok", resp.Type)

		var msg map[string]string
		err := json.Unmarshal(resp.Payload, &msg)
		assert.NoError(t, err)
		assert.Equal(t, "dispatched", msg["message"])

		// Verify it's in activeBeads
		_, inFlight := d.activeBeads.Load("TEST-1")
		assert.True(t, inFlight)
	})

	// Wait for the background goroutine from the previous subtest to finish so
	// its DB worker record (status=pending) is transitioned to a terminal state
	// before the next capacity check runs.
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// ok
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for background goroutines to finish")
	}

	t.Run("set clarification: invalid payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{
			Type:    "set_clarification",
			Payload: []byte("invalid"),
		})
		assert.Equal(t, "error", resp.Type)
	})

	t.Run("set clarification: missing fields", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.ClarificationPayload{BeadID: "X"})
		resp := d.handleIPC(ipc.Command{
			Type:    "set_clarification",
			Payload: payload,
		})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id and anvil are required")
	})

	t.Run("set clarification: empty reason", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.ClarificationPayload{BeadID: "X", Anvil: "a"})
		resp := d.handleIPC(ipc.Command{
			Type:    "set_clarification",
			Payload: payload,
		})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "reason is required")
	})

	t.Run("set and clear clarification", func(t *testing.T) {
		// Set
		payload, _ := json.Marshal(ipc.ClarificationPayload{
			BeadID: "TEST-CLAR",
			Anvil:  "test-anvil",
			Reason: "which auth library?",
		})
		resp := d.handleIPC(ipc.Command{
			Type:    "set_clarification",
			Payload: payload,
		})
		assert.Equal(t, "ok", resp.Type)

		// Verify in DB
		r, err := db.GetRetry("TEST-CLAR", "test-anvil")
		require.NoError(t, err)
		assert.True(t, r.ClarificationNeeded)

		// isBeadClarificationNeeded should return true
		needed, err := d.isBeadClarificationNeeded("TEST-CLAR", "test-anvil")
		require.NoError(t, err)
		assert.True(t, needed)

		// Clear
		payload, _ = json.Marshal(ipc.ClarificationPayload{
			BeadID: "TEST-CLAR",
			Anvil:  "test-anvil",
		})
		resp = d.handleIPC(ipc.Command{
			Type:    "clear_clarification",
			Payload: payload,
		})
		assert.Equal(t, "ok", resp.Type)

		// Verify cleared
		needed, err = d.isBeadClarificationNeeded("TEST-CLAR", "test-anvil")
		require.NoError(t, err)
		assert.False(t, needed)
	})

	t.Run("isBeadClarificationNeeded returns false for unknown bead", func(t *testing.T) {
		needed, err := d.isBeadClarificationNeeded("UNKNOWN", "test-anvil")
		require.NoError(t, err)
		assert.False(t, needed)
	})

	t.Run("retry_bead: invalid payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{
			Type:    "retry_bead",
			Payload: []byte("invalid"),
		})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "invalid retry_bead payload")
	})

	t.Run("retry_bead: missing fields", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.RetryBeadPayload{BeadID: "X"})
		resp := d.handleIPC(ipc.Command{
			Type:    "retry_bead",
			Payload: payload,
		})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id and anvil are required")
	})

	t.Run("retry_bead: resets circuit breaker", func(t *testing.T) {
		// Trip the circuit breaker by incrementing past the threshold.
		const beadID = "CB-BEAD"
		const anvil = "test-anvil"
		_, broke, err := db.IncrementDispatchFailures(beadID, anvil, 1, "test failure")
		require.NoError(t, err)
		require.True(t, broke, "expected circuit breaker to trip")

		// Verify needs_human is set.
		r, err := db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		require.True(t, r.NeedsHuman)

		// Reset via IPC — DB reset is synchronous, bd shelling is async.
		payload, _ := json.Marshal(ipc.RetryBeadPayload{BeadID: beadID, Anvil: anvil})
		resp := d.handleIPC(ipc.Command{
			Type:    "retry_bead",
			Payload: payload,
		})
		assert.Equal(t, "queued", resp.Type)
		assert.NotEmpty(t, resp.RequestID, "queued retry_bead response should include a request_id")

		// Verify circuit breaker is cleared (DB reset is synchronous).
		r, err = db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		assert.False(t, r.NeedsHuman)
		assert.Equal(t, 0, r.DispatchFailures)
	})

	t.Run("retry_bead: resets warden rejection with dispatch failures", func(t *testing.T) {
		// Regression test: warden rejects changes, which sets needs_human=true
		// and may also have DispatchFailures>0 from prior attempts. The old code
		// path called ResetDispatchFailures (which only clears circuit-breaker
		// records) and silently matched 0 rows, leaving needs_human set.
		const beadID = "WR-BEAD"
		const anvil = "test-anvil"
		err := db.UpsertRetry(&state.RetryRecord{
			BeadID:           beadID,
			Anvil:            anvil,
			DispatchFailures: 2,
			NeedsHuman:       true,
			LastError:        "warden rejected: code quality issues found",
		})
		require.NoError(t, err)

		r, err := db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		require.True(t, r.NeedsHuman)
		require.Equal(t, 2, r.DispatchFailures)

		// Reset via IPC — must clear both needs_human and dispatch_failures.
		payload, _ := json.Marshal(ipc.RetryBeadPayload{BeadID: beadID, Anvil: anvil})
		resp := d.handleIPC(ipc.Command{
			Type:    "retry_bead",
			Payload: payload,
		})
		assert.Equal(t, "queued", resp.Type)

		r, err = db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		assert.False(t, r.NeedsHuman, "needs_human should be cleared after reset")
		assert.Equal(t, 0, r.DispatchFailures, "dispatch_failures should be cleared after reset")
		assert.Empty(t, r.LastError, "last_error should be cleared after reset")
	})

	t.Run("retry_bead: clears needs_human for pipeline-exhausted beads", func(t *testing.T) {
		// Set needs_human via exhausted retries (no circuit breaker prefix).
		// When a user clicks Retry on such a bead, needs_human should be
		// cleared so the bead is eligible for re-dispatch.
		const beadID = "NH-BEAD"
		const anvil = "test-anvil"
		err := db.UpsertRetry(&state.RetryRecord{
			BeadID:     beadID,
			Anvil:      anvil,
			NeedsHuman: true,
			LastError:  "exhausted retries",
		})
		require.NoError(t, err)

		r, err := db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		require.True(t, r.NeedsHuman)

		payload, _ := json.Marshal(ipc.RetryBeadPayload{BeadID: beadID, Anvil: anvil})
		resp := d.handleIPC(ipc.Command{
			Type:    "retry_bead",
			Payload: payload,
		})
		assert.Equal(t, "queued", resp.Type)

		r, err = db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		assert.False(t, r.NeedsHuman, "needs_human should be cleared after retry")
		assert.Equal(t, 0, r.RetryCount, "retry_count should be reset")
		assert.Empty(t, r.LastError, "last_error should be cleared")
	})

	t.Run("successful dispatch via cache", func(t *testing.T) {
		// Wait for the goroutine from the previous subtest to finish so its
		// deferred activeBeads.Delete cannot race with the Store below.
		done := make(chan struct{})
		go func() { d.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for goroutines from previous subtest to finish")
		}

		// Clear activeBeads
		d.activeBeads.Delete("TEST-1")

		// Pre-populate cache
		d.lastBeadsMu.Lock()
		d.lastBeads = []poller.Bead{
			{ID: "TEST-1", Anvil: "test-anvil", Title: "Test Bead", Priority: 1},
		}
		d.lastBeadsMu.Unlock()

		payload, _ := json.Marshal(ipc.RunBeadPayload{
			BeadID: "TEST-1",
			Anvil:  "test-anvil",
		})
		resp := d.handleIPC(ipc.Command{
			Type:    "run_bead",
			Payload: payload,
		})
		assert.Equal(t, "ok", resp.Type)

		var msg map[string]string
		err := json.Unmarshal(resp.Payload, &msg)
		assert.NoError(t, err)
		assert.Equal(t, "dispatched", msg["message"])

		// Verify it's in activeBeads
		_, inFlight := d.activeBeads.Load("TEST-1")
		assert.True(t, inFlight)
	})
}

// TestPollAndDispatch_CostLimit verifies that pollAndDispatch skips dispatch when
// today's cost meets or exceeds the configured limit, and that the cost_limit_hit
// event is logged only once per day across multiple poll cycles.
func TestPollAndDispatch_CostLimit(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-costlimit-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create a fake bd script that returns one ready bead so we can verify
	// dispatch is actually skipped (not just a no-op because no beads exist).
	var bdScript string
	var bdContent string
	if runtime.GOOS == "windows" {
		bdScript = filepath.Join(tmpDir, "bd.bat")
		bdContent = `@echo off
if "%1"=="ready" (
    echo [{"id": "COST-1", "title": "Cost Test Bead", "status": "ready", "priority": 1, "tags": []}]
    exit /b 0
)
exit /b 0
`
	} else {
		bdScript = filepath.Join(tmpDir, "bd")
		bdContent = `#!/bin/sh
if [ "$1" = "ready" ]; then
    echo '[{"id": "COST-1", "title": "Cost Test Bead", "status": "ready", "priority": 1, "tags": []}]'
    exit 0
fi
exit 0
`
	}
	require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
	defer os.Setenv("PATH", oldPath)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	// Seed today's cost above the limit.
	today := time.Now().Format("2006-01-02")
	require.NoError(t, db.AddDailyCost(today, 0, 0, 0, 0, 15.00))

	cfg := &config.Config{
		Settings: config.SettingsConfig{
			MaxTotalSmiths: 4,
			PollInterval:   10 * time.Second,
			DailyCostLimit: 10.00, // limit is $10, cost is $15
		},
		Anvils: map[string]config.AnvilConfig{
			"dummy": {Path: tmpDir, AutoDispatch: "all"},
		},
	}

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
	}
	d.cfg.Store(cfg)
	d.costLimitLoggedDate.Store("")

	countCostLimitEvents := func() int {
		events, err := db.RecentEvents(50)
		require.NoError(t, err)
		n := 0
		for _, e := range events {
			if e.Type == state.EventCostLimitHit {
				n++
			}
		}
		return n
	}

	// First poll: the fake bd script returns a ready bead but dispatch should be
	// skipped because today's cost exceeds the limit.
	d.pollAndDispatch(context.Background(), true)
	assert.GreaterOrEqual(t, len(d.lastBeads), 1, "poll should surface the ready bead")
	// No worker should have been dispatched.
	_, inFlight := d.activeBeads.Load("COST-1")
	assert.False(t, inFlight, "bead should NOT be dispatched when cost limit is exceeded")
	assert.Equal(t, 1, countCostLimitEvents(), "cost_limit_hit event should be logged once")

	// Second poll: event must NOT be logged again (same calendar day).
	d.pollAndDispatch(context.Background(), true)
	assert.Equal(t, 1, countCostLimitEvents(), "cost_limit_hit must not be logged again on same day")

	// Simulate a daemon restart: reset the in-memory guard but keep the DB event.
	// The DB-backed deduplication must prevent the notification from firing again.
	d.costLimitLoggedDate.Store("")
	d.pollAndDispatch(context.Background(), true)
	assert.Equal(t, 1, countCostLimitEvents(), "cost_limit_hit must not be logged after simulated restart when already notified today")
}

func TestPollAndDispatch_ManualPause(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-pause-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Fake bd returning one ready bead so we can verify dispatch is actually
	// skipped while paused (not a no-op because no beads exist).
	var bdScript, bdContent string
	if runtime.GOOS == "windows" {
		bdScript = filepath.Join(tmpDir, "bd.bat")
		bdContent = `@echo off
if "%1"=="ready" (
    echo [{"id": "PAUSE-1", "title": "Pause Test Bead", "status": "ready", "priority": 1, "tags": []}]
    exit /b 0
)
exit /b 0
`
	} else {
		bdScript = filepath.Join(tmpDir, "bd")
		bdContent = `#!/bin/sh
if [ "$1" = "ready" ]; then
    echo '[{"id": "PAUSE-1", "title": "Pause Test Bead", "status": "ready", "priority": 1, "tags": []}]'
    exit 0
fi
exit 0
`
	}
	require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
	defer os.Setenv("PATH", oldPath)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	cfg := &config.Config{
		Settings: config.SettingsConfig{
			MaxTotalSmiths: 4,
			PollInterval:   10 * time.Second,
		},
		Anvils: map[string]config.AnvilConfig{
			"dummy": {Path: tmpDir, AutoDispatch: "all"},
		},
	}

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
	}
	d.cfg.Store(cfg)
	d.runCtx = context.Background()

	// Pause via IPC: state flag flips and an event is logged.
	resp := d.handleIPC(ipc.Command{Type: "pause_dispatch"})
	assert.Equal(t, "ok", resp.Type)
	assert.True(t, d.dispatchPaused.Load(), "dispatch should be paused after pause_dispatch")

	countEvents := func(et state.EventType) int {
		events, err := db.RecentEvents(50)
		require.NoError(t, err)
		n := 0
		for _, e := range events {
			if e.Type == et {
				n++
			}
		}
		return n
	}
	assert.Equal(t, 1, countEvents(state.EventDispatchPaused), "pause should log one event")

	// Pausing again is idempotent — no duplicate event.
	resp = d.handleIPC(ipc.Command{Type: "pause_dispatch"})
	assert.Equal(t, "ok", resp.Type)
	assert.Equal(t, 1, countEvents(state.EventDispatchPaused), "re-pausing must not log again")

	// Poll while paused: the ready bead is surfaced but NOT dispatched.
	d.pollAndDispatch(context.Background(), true)
	assert.GreaterOrEqual(t, len(d.lastBeads), 1, "poll should still surface the ready bead while paused")
	_, inFlight := d.activeBeads.Load("PAUSE-1")
	assert.False(t, inFlight, "bead should NOT be dispatched while paused")

	// Resume triggers a background poll. Swap to an empty-anvils config first so
	// that async poll finds no beads to dispatch — keeps the test deterministic
	// and avoids a real worktree dispatch racing with teardown.
	d.cfg.Store(&config.Config{Settings: cfg.Settings})

	// Resume: state flag clears and a resume event is logged.
	resp = d.handleIPC(ipc.Command{Type: "resume_dispatch"})
	assert.Equal(t, "ok", resp.Type)
	assert.False(t, d.dispatchPaused.Load(), "dispatch should resume after resume_dispatch")
	assert.Equal(t, 1, countEvents(state.EventDispatchResumed), "resume should log one event")

	// Resuming again is idempotent — no duplicate event.
	resp = d.handleIPC(ipc.Command{Type: "resume_dispatch"})
	assert.Equal(t, "ok", resp.Type)
	assert.Equal(t, 1, countEvents(state.EventDispatchResumed), "re-resuming must not log again")
}

// TestDispatchPause_PersistAndRestore verifies that a manual dispatch pause is
// persisted to state.db and restored on daemon startup, and that resume clears
// both the in-memory atomic and the persisted flag.
func TestDispatchPause_PersistAndRestore(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	defer db.Close()

	newDaemon := func() *Daemon {
		d := &Daemon{
			db:     db,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		d.cfg.Store(&config.Config{})
		d.runCtx = context.Background()
		// Pre-set pollRunning so the async pollAndDispatch goroutine spawned
		// by resume_dispatch no-ops immediately (the CompareAndSwap guard
		// returns false), avoiding races with test teardown.
		d.pollRunning.Store(true)
		return d
	}

	// Pause via IPC persists the flag and a timestamp.
	d := newDaemon()
	resp := d.handleIPC(ipc.Command{Type: "pause_dispatch"})
	require.Equal(t, "ok", resp.Type)

	paused, ok, err := db.GetSetting(state.SettingDispatchPaused)
	require.NoError(t, err)
	require.True(t, ok, "dispatch_paused should be persisted")
	assert.Equal(t, "1", paused)
	at, ok, err := db.GetSetting(state.SettingDispatchPausedAt)
	require.NoError(t, err)
	require.True(t, ok, "dispatch_paused_at should be persisted")
	assert.NotEmpty(t, at, "paused-at timestamp should be recorded")

	// Simulate a restart: a fresh daemon over the same db restores the pause.
	d2 := newDaemon()
	assert.False(t, d2.dispatchPaused.Load(), "fresh daemon starts unpaused before restore")
	d2.restoreDispatchPause()
	assert.True(t, d2.dispatchPaused.Load(), "pause should be restored from state.db on startup")
	since, sok := d2.pausedSince.Load().(time.Time)
	assert.True(t, sok && !since.IsZero(), "pausedSince should be restored")

	// Resume clears both the atomic and the persisted flag.
	resp = d2.handleIPC(ipc.Command{Type: "resume_dispatch"})
	require.Equal(t, "ok", resp.Type)
	assert.False(t, d2.dispatchPaused.Load(), "resume clears the atomic")
	paused, _, err = db.GetSetting(state.SettingDispatchPaused)
	require.NoError(t, err)
	assert.Equal(t, "0", paused, "resume clears the persisted flag")

	// A subsequent restart must NOT restore a pause.
	d3 := newDaemon()
	d3.restoreDispatchPause()
	assert.False(t, d3.dispatchPaused.Load(), "resumed state must not restore a pause")
}

func TestHandleIPC_RetryBead(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
		reqTracker:    *ipc.NewRequestTracker("test-"),
	}
	d.cfg.Store(&config.Config{})

	t.Run("invalid payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "retry_bead", Payload: []byte("invalid")})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "invalid retry_bead payload")
	})

	t.Run("missing fields", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.RetryBeadPayload{BeadID: "X"})
		resp := d.handleIPC(ipc.Command{Type: "retry_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id and anvil are required")
	})

	t.Run("success", func(t *testing.T) {
		// Trip the circuit breaker so retry_bead has something to reset.
		_, broke, err := db.IncrementDispatchFailures("BD-RETRY", "anvil-1", 1, "test failure")
		require.NoError(t, err)
		require.True(t, broke, "expected circuit breaker to trip")

		payload, _ := json.Marshal(ipc.RetryBeadPayload{BeadID: "BD-RETRY", Anvil: "anvil-1"})
		resp := d.handleIPC(ipc.Command{Type: "retry_bead", Payload: payload})
		assert.Equal(t, "queued", resp.Type)

		r, err := db.GetRetry("BD-RETRY", "anvil-1")
		require.NoError(t, err)
		require.NotNil(t, r)
		assert.False(t, r.NeedsHuman)
		assert.Equal(t, 0, r.DispatchFailures)
	})
}

// TestHandleIPC_RetryBead_ExhaustedPR verifies the full retry flow for an
// exhausted PR (PRID > 0). This covers the scenario where a user clicks Retry
// on "Rebase exhausted (3/3)" in the Needs Attention panel: DB fix counts are
// reset, the lifecycle manager's in-memory state is cleared, and the bellows
// snapshot cache is purged so that the next poll re-detects the conflict and
// dispatches a fresh rebase worker.
func TestHandleIPC_RetryBead_ExhaustedPR(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Create lifecycle manager with a noop handler.
	lm := lifecycle.New(db, logger, func(_ context.Context, _ lifecycle.ActionRequest) {})

	// Create bellows monitor (won't actually run, just needs to exist for reset).
	bm := bellows.New(db, nil, time.Minute, map[string]string{"test-anvil": tmpDir}, nil, nil, nil, nil)

	d := &Daemon{
		db:             db,
		logger:         logger,
		worktreeMgr:    worktree.NewManager(),
		promptBuilder:  prompt.NewBuilder(),
		lifecycleMgr:   lm,
		bellowsMonitor: bm,
		runCtx:         context.Background(),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {Path: tmpDir},
		},
	})

	// Insert a PR that has exhausted its rebase attempts (3/3).
	pr := &state.PR{
		Number:    42,
		Anvil:     "test-anvil",
		BeadID:    "BD-REBASE",
		Branch:    "forge/BD-REBASE",
		Status:    state.PRNeedsFix,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	require.NotZero(t, pr.ID, "InsertPR should set the ID")
	// InsertPR doesn't set rebase_count/ci_passing/is_conflicting, so update them.
	require.NoError(t, db.UpdatePRLifecycle(pr.ID, 0, 0, 3, true))
	require.NoError(t, db.UpdatePRMergeability(pr.ID, true, true, false, false, false, true))

	// Verify the PR shows up as exhausted.
	exhausted, err := db.ExhaustedPRs(
		state.DefaultMaxCIFixAttempts,
		state.DefaultMaxReviewFixAttempts,
		state.DefaultMaxRebaseAttempts,
	)
	require.NoError(t, err)
	require.Len(t, exhausted, 1, "PR should be exhausted before retry")

	// Populate lifecycle state for this PR (simulates what Load() does at startup).
	// Fire 3 conflict events to reach exhaustion (maxRebase=3).
	for i := 0; i < state.DefaultMaxRebaseAttempts; i++ {
		lm.HandleEvent(context.Background(), bellows.PREvent{
			PRNumber:  42,
			BeadID:    "BD-REBASE",
			Anvil:     "test-anvil",
			Branch:    "forge/BD-REBASE",
			EventType: bellows.EventPRConflicting,
		})
	}
	st := lm.GetState("test-anvil", 42)
	require.NotNil(t, st)
	require.Equal(t, state.DefaultMaxRebaseAttempts, st.RebaseCount, "setup: lifecycle should be exhausted")

	// Reset bellows snapshot cache so this retry starts with no prior snapshot state.
	// (In production this cache would be populated by prior checkAll polls.)
	bm.ResetPRState("test-anvil", 42)

	// --- Execute retry via IPC ---
	payload, _ := json.Marshal(ipc.RetryBeadPayload{
		BeadID: "BD-REBASE",
		Anvil:  "test-anvil",
		PRID:   pr.ID,
	})
	resp := d.handleIPC(ipc.Command{Type: "retry_bead", Payload: payload})
	assert.Equal(t, "ok", resp.Type)

	var msg map[string]string
	require.NoError(t, json.Unmarshal(resp.Payload, &msg))
	assert.Equal(t, "PR fix counts reset, status set to open", msg["message"])

	// Verify DB: fix counts reset, status back to open.
	pr2, err := db.GetPRByID(pr.ID)
	require.NoError(t, err)
	require.NotNil(t, pr2)
	assert.Equal(t, state.PROpen, pr2.Status, "status should be reset to open")
	assert.Equal(t, 0, pr2.RebaseCount, "rebase_count should be 0")
	assert.Equal(t, 0, pr2.CIFixCount, "ci_fix_count should be 0")
	assert.Equal(t, 0, pr2.ReviewFixCount, "review_fix_count should be 0")
	assert.False(t, pr2.IsConflicting, "is_conflicting should be cleared")

	// Verify PR no longer appears as exhausted.
	exhausted, err = db.ExhaustedPRs(
		state.DefaultMaxCIFixAttempts,
		state.DefaultMaxReviewFixAttempts,
		state.DefaultMaxRebaseAttempts,
	)
	require.NoError(t, err)
	assert.Empty(t, exhausted, "PR should no longer be exhausted after retry")

	// Verify lifecycle in-memory state was reset.
	st = lm.GetState("test-anvil", 42)
	require.NotNil(t, st)
	assert.Equal(t, 0, st.RebaseCount, "lifecycle RebaseCount should be 0")
	assert.False(t, st.Conflicting, "lifecycle Conflicting should be false")
	assert.True(t, st.CIPassing, "lifecycle CIPassing should be true")

	// Verify that a new EventPRConflicting dispatches a fresh rebase after reset.
	// We need a new lifecycle manager with a tracking handler since lm was
	// created with a noop handler.
	var dispatched []lifecycle.ActionRequest
	lm2 := lifecycle.New(db, logger, func(_ context.Context, req lifecycle.ActionRequest) {
		dispatched = append(dispatched, req)
	})
	// Load from DB — the reset PR should have rebase_count=0 in the DB.
	require.NoError(t, lm2.Load(context.Background()))
	st2 := lm2.GetState("test-anvil", 42)
	require.NotNil(t, st2, "lifecycle should load the reset PR from DB")
	assert.Equal(t, 0, st2.RebaseCount, "loaded lifecycle state should have RebaseCount=0")

	// Send a conflict event — should dispatch ActionRebase (not exhausted).
	lm2.HandleEvent(context.Background(), bellows.PREvent{
		PRNumber:  42,
		BeadID:    "BD-REBASE",
		Anvil:     "test-anvil",
		Branch:    "forge/BD-REBASE",
		EventType: bellows.EventPRConflicting,
	})
	require.Len(t, dispatched, 1, "conflict event after retry should dispatch")
	assert.Equal(t, lifecycle.ActionRebase, dispatched[0].Action)

	// Verify retry event was logged.
	events, err := db.RecentEvents(50)
	require.NoError(t, err)
	found := false
	for _, e := range events {
		if e.Type == state.EventRetryReset && e.BeadID == "BD-REBASE" {
			found = true
			break
		}
	}
	assert.True(t, found, "EventRetryReset should be logged for the bead")
}

// TestHandleIPC_RetryBead_NonBeadPR verifies that retry works for exhausted PRs
// that have no associated bead (e.g. warden-learn PRs). The retry should succeed
// even when bead_id is empty in the payload, as long as PRID > 0.
func TestHandleIPC_RetryBead_NonBeadPR(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	lm := lifecycle.New(db, logger, func(_ context.Context, _ lifecycle.ActionRequest) {})
	bm := bellows.New(db, nil, time.Minute, map[string]string{"test-anvil": tmpDir}, nil, nil, nil, nil)

	d := &Daemon{
		db:             db,
		logger:         logger,
		worktreeMgr:    worktree.NewManager(),
		promptBuilder:  prompt.NewBuilder(),
		lifecycleMgr:   lm,
		bellowsMonitor: bm,
		runCtx:         context.Background(),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {Path: tmpDir},
		},
	})

	// Insert a PR with no bead_id (simulates a warden-learn or other non-bead PR).
	pr := &state.PR{
		Number:    99,
		Anvil:     "test-anvil",
		BeadID:    "", // no bead
		Branch:    "warden-learn/forge",
		Status:    state.PRNeedsFix,
		CreatedAt: time.Now(),
	}
	require.NoError(t, db.InsertPR(pr))
	require.NotZero(t, pr.ID, "InsertPR should set the ID")
	require.NoError(t, db.UpdatePRLifecycle(pr.ID, 0, 0, 3, true))
	require.NoError(t, db.UpdatePRMergeability(pr.ID, true, true, false, false, false, true))

	// Retry with only PRID set — no BeadID.
	payload, _ := json.Marshal(ipc.RetryBeadPayload{
		BeadID: "", // intentionally empty
		Anvil:  "test-anvil",
		PRID:   pr.ID,
	})
	resp := d.handleIPC(ipc.Command{Type: "retry_bead", Payload: payload})
	assert.Equal(t, "ok", resp.Type, "retry should succeed for non-bead PR")

	var msg map[string]string
	require.NoError(t, json.Unmarshal(resp.Payload, &msg))
	assert.Equal(t, "PR fix counts reset, status set to open", msg["message"])

	// Verify fix counts were reset.
	pr2, err := db.GetPRByID(pr.ID)
	require.NoError(t, err)
	require.NotNil(t, pr2)
	assert.Equal(t, state.PROpen, pr2.Status)
	assert.Equal(t, 0, pr2.RebaseCount)
}

// TestResolveAnvilConfig verifies the case-insensitive anvil lookup helper.
// Users typing the anvil name on the CLI may not match the configured key
// exactly (e.g. "Munin" vs "munin"); the helper resolves these to the
// canonical config key so DB queries and crucibleStatuses keys line up.
func TestResolveAnvilConfig(t *testing.T) {
	d := &Daemon{}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"munin":   {Path: "/tmp/munin"},
			"Heimdall": {Path: "/tmp/heimdall"},
		},
	})

	t.Run("exact match wins", func(t *testing.T) {
		name, cfg, ok := d.resolveAnvilConfig("munin")
		require.True(t, ok)
		assert.Equal(t, "munin", name)
		assert.Equal(t, "/tmp/munin", cfg.Path)
	})

	t.Run("case-insensitive match", func(t *testing.T) {
		name, cfg, ok := d.resolveAnvilConfig("Munin")
		require.True(t, ok)
		assert.Equal(t, "munin", name, "should canonicalise to lowercase configured key")
		assert.Equal(t, "/tmp/munin", cfg.Path)
	})

	t.Run("case-insensitive match preserves configured case", func(t *testing.T) {
		name, _, ok := d.resolveAnvilConfig("heimdall")
		require.True(t, ok)
		assert.Equal(t, "Heimdall", name, "should canonicalise to the configured (mixed-case) key")
	})

	t.Run("unknown anvil returns false", func(t *testing.T) {
		_, _, ok := d.resolveAnvilConfig("unknown")
		assert.False(t, ok)
	})

	t.Run("empty name returns false", func(t *testing.T) {
		_, _, ok := d.resolveAnvilConfig("")
		assert.False(t, ok)
	})
}

// TestHandleIPC_RetryBead_ClearsCrucibleStatusOnSuccess verifies that the
// retry_bead handler clears the in-memory crucibleStatuses entry after the
// bd update succeeds, so a paused crucible re-enters the loop without a
// daemon restart. Uses a mixed-case anvil name on the payload to also
// exercise the case-insensitive lookup so the DB retry record (stored
// under the canonical configured key) is reset, the crucibleStatuses key
// matches, and the bd update is dispatched against the canonical path.
func TestHandleIPC_RetryBead_ClearsCrucibleStatusOnSuccess(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Fake bd script that records its arguments and exits 0 on `update`.
	bdLog := filepath.Join(tmpDir, "bd-args.log")
	var bdScript, bdContent string
	if runtime.GOOS == "windows" {
		bdScript = filepath.Join(tmpDir, "bd.bat")
		bdContent = "@echo off\r\necho %*>>\"" + bdLog + "\"\r\nif \"%1\"==\"update\" (\r\n  echo {\"id\":\"%2\",\"status\":\"open\"}\r\n  exit /b 0\r\n)\r\nexit /b 1\r\n"
	} else {
		bdScript = filepath.Join(tmpDir, "bd")
		bdContent = "#!/bin/sh\necho \"$@\" >> \"" + bdLog + "\"\nif [ \"$1\" = \"update\" ]; then\n  echo '{\"id\":\"'\"$2\"'\",\"status\":\"open\"}'\n  exit 0\nfi\nexit 1\n"
	}
	require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
	defer os.Setenv("PATH", oldPath)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
		reqTracker:    *ipc.NewRequestTracker("test-"),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"munin": {Path: tmpDir, AutoDispatchTag: "forgeReady"},
		},
	})

	const beadID = "BD-CRUCIBLE"
	_, broke, err := db.IncrementDispatchFailures(beadID, "munin", 1, "test failure")
	require.NoError(t, err)
	require.True(t, broke)

	// Seed the in-memory paused crucible status — this is what `forge queue
	// retry` previously failed to clear, leaving the daemon stuck.
	d.crucibleStatuses.Store("munin/"+beadID, crucible.Status{Phase: "paused"})

	// Mixed-case anvil name on the payload — exercises both the
	// case-insensitive lookup and the canonical key threading.
	payload, _ := json.Marshal(ipc.RetryBeadPayload{BeadID: beadID, Anvil: "Munin"})
	resp := d.handleIPC(ipc.Command{Type: "retry_bead", Payload: payload})
	assert.Equal(t, "queued", resp.Type)

	// The DB ResetRetry runs synchronously before the goroutine starts —
	// confirms the case-insensitive canonicalisation matched the configured
	// "munin" key. Without canonicalisation, ResetRetry would have used
	// "Munin" and matched zero rows.
	r, err := db.GetRetry(beadID, "munin")
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.False(t, r.NeedsHuman, "needs_human should be cleared synchronously")
	assert.Equal(t, 0, r.DispatchFailures)

	// Wait for the async goroutine to finish (it calls completeAsync at the
	// end, which removes the request from the tracker).
	require.Eventually(t, func() bool {
		return d.reqTracker.Pending() == 0
	}, 5*time.Second, 10*time.Millisecond, "async goroutine never completed")

	// crucibleStatuses entry must have been deleted on bd success — keyed by
	// the canonical "munin", not the user-typed "Munin".
	_, stillPresent := d.crucibleStatuses.Load("munin/" + beadID)
	assert.False(t, stillPresent, "crucibleStatuses entry should be cleared after retry_bead succeeds")

	// And the bd update must have re-applied the auto_dispatch_tag.
	logBytes, err := os.ReadFile(bdLog)
	require.NoError(t, err)
	logged := string(logBytes)
	assert.Contains(t, logged, "update "+beadID, "bd update should target the bead")
	assert.Contains(t, logged, "--add-label=forgeReady",
		"bd update should re-apply the auto_dispatch_tag that releaseBeadClaim strips")
}

// TestHandleIPC_RetryBead_PreservesCrucibleStatusOnBdFailure verifies that
// when the bd update fails (e.g. anvil missing from config), the in-memory
// crucibleStatuses entry is left alone so the UI doesn't drop paused state
// on a transient shell error.
func TestHandleIPC_RetryBead_PreservesCrucibleStatusOnBdFailure(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
		reqTracker:    *ipc.NewRequestTracker("test-"),
	}
	// Empty anvil config — the goroutine will skip the bd update entirely.
	d.cfg.Store(&config.Config{})

	const beadID = "BD-NOBD"
	const anvil = "missing-anvil"
	_, broke, err := db.IncrementDispatchFailures(beadID, anvil, 1, "test failure")
	require.NoError(t, err)
	require.True(t, broke)

	d.crucibleStatuses.Store(anvil+"/"+beadID, crucible.Status{Phase: "paused"})

	payload, _ := json.Marshal(ipc.RetryBeadPayload{BeadID: beadID, Anvil: anvil})
	resp := d.handleIPC(ipc.Command{Type: "retry_bead", Payload: payload})
	assert.Equal(t, "queued", resp.Type)

	require.Eventually(t, func() bool {
		return d.reqTracker.Pending() == 0
	}, 5*time.Second, 10*time.Millisecond)

	_, stillPresent := d.crucibleStatuses.Load(anvil + "/" + beadID)
	assert.True(t, stillPresent, "crucibleStatuses entry should be preserved when bd update did not succeed")
}

// TestHandleIPC_CrucibleAction_Resume_RestoresAutoDispatchTag verifies that
// the crucible_action resume path re-applies the anvil's auto_dispatch_tag.
// releaseBeadClaim only strips the tag on circuit-breaker escalation, but the
// resume path is also exercised after escalations (and the add-label is
// idempotent), so it must run unconditionally — otherwise an escalated bead
// returns to status=open but stays invisible to bd ready on tagged-dispatch
// anvils.
func TestHandleIPC_CrucibleAction_Resume_RestoresAutoDispatchTag(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	bdLog := filepath.Join(tmpDir, "bd-args.log")
	var bdScript, bdContent string
	if runtime.GOOS == "windows" {
		bdScript = filepath.Join(tmpDir, "bd.bat")
		bdContent = "@echo off\r\necho %*>>\"" + bdLog + "\"\r\nif \"%1\"==\"update\" (\r\n  echo {\"id\":\"%2\",\"status\":\"open\"}\r\n  exit /b 0\r\n)\r\nexit /b 1\r\n"
	} else {
		bdScript = filepath.Join(tmpDir, "bd")
		bdContent = "#!/bin/sh\necho \"$@\" >> \"" + bdLog + "\"\nif [ \"$1\" = \"update\" ]; then\n  echo '{\"id\":\"'\"$2\"'\",\"status\":\"open\"}'\n  exit 0\nfi\nexit 1\n"
	}
	require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
	defer os.Setenv("PATH", oldPath)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
		reqTracker:    *ipc.NewRequestTracker("test-"),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"munin": {Path: tmpDir, AutoDispatchTag: "forgeReady"},
		},
	})

	const parentID = "BD-PARENT"
	d.crucibleStatuses.Store("munin/"+parentID, crucible.Status{Phase: "paused"})

	// Resume via Hearth using a mixed-case anvil name to also exercise
	// the canonicalisation path.
	payload, _ := json.Marshal(ipc.CrucibleActionPayload{
		ParentID: parentID,
		Anvil:    "Munin",
		Action:   "resume",
	})
	resp := d.handleIPC(ipc.Command{Type: "crucible_action", Payload: payload})
	assert.Equal(t, "ok", resp.Type)

	logBytes, err := os.ReadFile(bdLog)
	require.NoError(t, err)
	logged := string(logBytes)
	assert.Contains(t, logged, "update "+parentID)
	assert.Contains(t, logged, "--add-label=forgeReady",
		"crucible resume must re-apply the auto_dispatch_tag")

	// The paused entry should be cleared after a successful resume.
	_, stillPresent := d.crucibleStatuses.Load("munin/" + parentID)
	assert.False(t, stillPresent)
}

func TestHandleIPC_DismissBead(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
	}
	d.cfg.Store(&config.Config{})

	t.Run("invalid payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "dismiss_bead", Payload: []byte("invalid")})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "invalid dismiss_bead payload")
	})

	t.Run("missing fields", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.DismissBeadPayload{BeadID: "X"})
		resp := d.handleIPC(ipc.Command{Type: "dismiss_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id and anvil are required")
	})

	t.Run("success", func(t *testing.T) {
		require.NoError(t, db.UpsertRetry(&state.RetryRecord{
			BeadID:     "BD-DISMISS",
			Anvil:      "anvil-1",
			NeedsHuman: true,
		}))

		payload, _ := json.Marshal(ipc.DismissBeadPayload{BeadID: "BD-DISMISS", Anvil: "anvil-1"})
		resp := d.handleIPC(ipc.Command{Type: "dismiss_bead", Payload: payload})
		assert.Equal(t, "ok", resp.Type)

		// Record should be gone.
		r, err := db.GetRetry("BD-DISMISS", "anvil-1")
		require.NoError(t, err)
		assert.Nil(t, r)
	})
}

func TestHandleIPC_ClearBead(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
	}
	d.cfg.Store(&config.Config{})

	t.Run("invalid payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "clear_bead", Payload: []byte("invalid")})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "invalid clear_bead payload")
	})

	t.Run("missing fields", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.ClearBeadPayload{BeadID: "X"})
		resp := d.handleIPC(ipc.Command{Type: "clear_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id and anvil are required")
	})

	t.Run("success clears flags but preserves row", func(t *testing.T) {
		require.NoError(t, db.UpsertRetry(&state.RetryRecord{
			BeadID:              "BD-CLEAR",
			Anvil:               "anvil-1",
			NeedsHuman:          true,
			DispatchFailures:    3,
			RecoveryFailures:    1,
			ClarificationNeeded: true,
			RetryCount:          5,
			LastError:           "boom",
		}))

		payload, _ := json.Marshal(ipc.ClearBeadPayload{BeadID: "BD-CLEAR", Anvil: "anvil-1"})
		resp := d.handleIPC(ipc.Command{Type: "clear_bead", Payload: payload})
		assert.Equal(t, "ok", resp.Type)

		r, err := db.GetRetry("BD-CLEAR", "anvil-1")
		require.NoError(t, err)
		require.NotNil(t, r)
		assert.False(t, r.NeedsHuman)
		assert.Equal(t, 0, r.DispatchFailures)
		assert.Equal(t, 0, r.RecoveryFailures)
		assert.Empty(t, r.LastError)
		// Untouched fields.
		assert.True(t, r.ClarificationNeeded)
		assert.Equal(t, 5, r.RetryCount)

		// retry_cleared event should be logged.
		events, err := db.RecentEvents(50)
		require.NoError(t, err)
		var found bool
		for _, e := range events {
			if e.Type == state.EventRetryCleared && e.BeadID == "BD-CLEAR" && e.Anvil == "anvil-1" {
				found = true
				break
			}
		}
		assert.True(t, found, "expected retry_cleared event for BD-CLEAR")
	})

	t.Run("idempotent on missing row", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.ClearBeadPayload{BeadID: "BD-NONE", Anvil: "anvil-1"})
		resp := d.handleIPC(ipc.Command{Type: "clear_bead", Payload: payload})
		assert.Equal(t, "ok", resp.Type)
	})
}

func TestResolveGoRaceDetection(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-race-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	forgeDir := filepath.Join(tmpDir, ".forge")
	require.NoError(t, os.MkdirAll(forgeDir, 0o755))
	temperYAMLPath := filepath.Join(forgeDir, "temper.yaml")

	makeTrue := func() *bool { b := true; return &b }
	makeFalse := func() *bool { b := false; return &b }

	newDaemon := func(globalRace bool) *Daemon {
		d := &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		d.cfg.Store(&config.Config{
			Settings: config.SettingsConfig{GoRaceDetection: globalRace},
		})
		return d
	}

	t.Run("global config used when no overrides", func(t *testing.T) {
		os.Remove(temperYAMLPath)
		assert.True(t, newDaemon(true).resolveGoRaceDetection(config.AnvilConfig{Path: tmpDir}))
		assert.False(t, newDaemon(false).resolveGoRaceDetection(config.AnvilConfig{Path: tmpDir}))
	})

	t.Run("per-anvil config overrides global", func(t *testing.T) {
		os.Remove(temperYAMLPath)
		// global=false, per-anvil=true → true
		assert.True(t, newDaemon(false).resolveGoRaceDetection(config.AnvilConfig{Path: tmpDir, GoRaceDetection: makeTrue()}))
		// global=true, per-anvil=false → false
		assert.False(t, newDaemon(true).resolveGoRaceDetection(config.AnvilConfig{Path: tmpDir, GoRaceDetection: makeFalse()}))
	})

	t.Run("temper.yaml overrides global and per-anvil config", func(t *testing.T) {
		require.NoError(t, os.WriteFile(temperYAMLPath, []byte("go_race_detection: true\n"), 0o644))
		// global=false, per-anvil=false, temper.yaml=true → true
		assert.True(t, newDaemon(false).resolveGoRaceDetection(config.AnvilConfig{Path: tmpDir, GoRaceDetection: makeFalse()}))
	})

	t.Run("temper.yaml false overrides per-anvil true", func(t *testing.T) {
		require.NoError(t, os.WriteFile(temperYAMLPath, []byte("go_race_detection: false\n"), 0o644))
		// global=true, per-anvil=true, temper.yaml=false → false
		assert.False(t, newDaemon(true).resolveGoRaceDetection(config.AnvilConfig{Path: tmpDir, GoRaceDetection: makeTrue()}))
	})

	t.Run("missing temper.yaml falls back to per-anvil config", func(t *testing.T) {
		os.Remove(temperYAMLPath)
		// global=false, per-anvil=true, no temper.yaml → true
		assert.True(t, newDaemon(false).resolveGoRaceDetection(config.AnvilConfig{Path: tmpDir, GoRaceDetection: makeTrue()}))
	})
}

func TestHandleIPC_ViewLogs(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
	}
	d.cfg.Store(&config.Config{})

	t.Run("invalid payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "view_logs", Payload: []byte("invalid")})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "invalid view_logs payload")
	})

	t.Run("missing bead_id", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.ViewLogsPayload{})
		resp := d.handleIPC(ipc.Command{Type: "view_logs", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id is required")
	})

	t.Run("no log found", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.ViewLogsPayload{BeadID: "BD-NO-LOG"})
		resp := d.handleIPC(ipc.Command{Type: "view_logs", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "no worker logs found")
	})

	t.Run("success", func(t *testing.T) {
		// Write a small log file.
		logFile := filepath.Join(tmpDir, "smith.log")
		require.NoError(t, os.WriteFile(logFile, []byte(`line1
line2
line3
`), 0o644))

		// Insert a worker record pointing to the log.
		require.NoError(t, db.InsertWorker(&state.Worker{
			ID:        "w-view-logs",
			BeadID:    "BD-VIEWLOGS",
			Anvil:     "anvil-1",
			Status:    state.WorkerDone,
			LogPath:   logFile,
			StartedAt: time.Now(),
		}))

		payload, _ := json.Marshal(ipc.ViewLogsPayload{BeadID: "BD-VIEWLOGS"})
		resp := d.handleIPC(ipc.Command{Type: "view_logs", Payload: payload})
		assert.Equal(t, "ok", resp.Type)

		var vr ipc.ViewLogsResponse
		require.NoError(t, json.Unmarshal(resp.Payload, &vr))
		assert.Equal(t, logFile, vr.LogPath)
		assert.Contains(t, vr.LastLines, "line1")
		assert.Contains(t, vr.LastLines, "line3")
	})
}

func TestHandleIPC_TagBead(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
		reqTracker:    *ipc.NewRequestTracker("test-"),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {
				Path:            tmpDir,
				AutoDispatchTag: "forge-ready",
			},
			"no-path-anvil": {
				Path:            "",
				AutoDispatchTag: "forge-ready",
			},
			"no-tag-anvil": {
				Path:            tmpDir,
				AutoDispatchTag: "",
			},
		},
	})

	t.Run("invalid JSON payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "tag_bead", Payload: []byte("invalid")})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "invalid tag_bead payload")
	})

	t.Run("missing bead_id", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.TagBeadPayload{Anvil: "test-anvil"})
		resp := d.handleIPC(ipc.Command{Type: "tag_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id and anvil are required")
	})

	t.Run("missing anvil", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.TagBeadPayload{BeadID: "BEAD-1"})
		resp := d.handleIPC(ipc.Command{Type: "tag_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id and anvil are required")
	})

	t.Run("unknown anvil", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.TagBeadPayload{BeadID: "BEAD-1", Anvil: "unknown-anvil"})
		resp := d.handleIPC(ipc.Command{Type: "tag_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "not found")
	})

	t.Run("anvil with empty path", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.TagBeadPayload{BeadID: "BEAD-1", Anvil: "no-path-anvil"})
		resp := d.handleIPC(ipc.Command{Type: "tag_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "no path configured")
	})

	t.Run("anvil with no auto_dispatch_tag", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.TagBeadPayload{BeadID: "BEAD-1", Anvil: "no-tag-anvil"})
		resp := d.handleIPC(ipc.Command{Type: "tag_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "no auto_dispatch_tag configured")
	})

	t.Run("success with fake bd script", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			bdScript := filepath.Join(tmpDir, "bd.bat")
			bdContent := "@echo off\r\nif \"%1\"==\"update\" (\r\n    echo {\"id\": \"%2\", \"status\": \"ok\"}\r\n    exit /b 0\r\n)\r\nexit /b 1\r\n"
			require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))
		} else {
			bdScript := filepath.Join(tmpDir, "bd")
			bdContent := "#!/bin/sh\nif [ \"$1\" = \"update\" ]; then\n    echo '{\"id\": \"'\"$2\"'\", \"status\": \"ok\"}'\n    exit 0\nfi\nexit 1\n"
			require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))
		}
		oldPath := os.Getenv("PATH")
		os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
		defer os.Setenv("PATH", oldPath)

		payload, _ := json.Marshal(ipc.TagBeadPayload{BeadID: "BEAD-1", Anvil: "test-anvil"})
		resp := d.handleIPC(ipc.Command{Type: "tag_bead", Payload: payload})
		assert.Equal(t, "queued", resp.Type)
	})
}

func TestHandleIPC_CloseBead(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
		reqTracker:    *ipc.NewRequestTracker("test-"),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {
				Path:            tmpDir,
				AutoDispatchTag: "forge-ready",
			},
			"no-path-anvil": {
				Path:            "",
				AutoDispatchTag: "forge-ready",
			},
		},
	})

	t.Run("invalid JSON payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "close_bead", Payload: []byte("invalid")})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "invalid close_bead payload")
	})

	t.Run("missing bead_id", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.CloseBeadPayload{Anvil: "test-anvil"})
		resp := d.handleIPC(ipc.Command{Type: "close_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id and anvil are required")
	})

	t.Run("missing anvil", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.CloseBeadPayload{BeadID: "BEAD-1"})
		resp := d.handleIPC(ipc.Command{Type: "close_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id and anvil are required")
	})

	t.Run("unknown anvil", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.CloseBeadPayload{BeadID: "BEAD-1", Anvil: "unknown-anvil"})
		resp := d.handleIPC(ipc.Command{Type: "close_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "not found")
	})

	t.Run("anvil with empty path", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.CloseBeadPayload{BeadID: "BEAD-1", Anvil: "no-path-anvil"})
		resp := d.handleIPC(ipc.Command{Type: "close_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "no path configured")
	})
}

func TestHandleIPC_UpdateLabel(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
		reqTracker:    *ipc.NewRequestTracker("test-"),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {
				Path: tmpDir,
			},
			"no-path-anvil": {
				Path: "",
			},
		},
	})

	t.Run("invalid JSON payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "update_label", Payload: []byte("invalid")})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "invalid update_label payload")
	})

	t.Run("missing bead_id", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.UpdateLabelPayload{Anvil: "test-anvil", Label: "ready", Action: "add"})
		resp := d.handleIPC(ipc.Command{Type: "update_label", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id, anvil, and label are required")
	})

	t.Run("missing anvil", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.UpdateLabelPayload{BeadID: "BEAD-1", Label: "ready", Action: "add"})
		resp := d.handleIPC(ipc.Command{Type: "update_label", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id, anvil, and label are required")
	})

	t.Run("missing label", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.UpdateLabelPayload{BeadID: "BEAD-1", Anvil: "test-anvil", Action: "add"})
		resp := d.handleIPC(ipc.Command{Type: "update_label", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id, anvil, and label are required")
	})

	t.Run("invalid action", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.UpdateLabelPayload{BeadID: "BEAD-1", Anvil: "test-anvil", Label: "ready", Action: "toggle"})
		resp := d.handleIPC(ipc.Command{Type: "update_label", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "invalid action")
	})

	t.Run("missing action", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.UpdateLabelPayload{BeadID: "BEAD-1", Anvil: "test-anvil", Label: "ready"})
		resp := d.handleIPC(ipc.Command{Type: "update_label", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "invalid action")
	})

	t.Run("unknown anvil", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.UpdateLabelPayload{BeadID: "BEAD-1", Anvil: "unknown-anvil", Label: "ready", Action: "add"})
		resp := d.handleIPC(ipc.Command{Type: "update_label", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "not found")
	})

	t.Run("anvil with empty path", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.UpdateLabelPayload{BeadID: "BEAD-1", Anvil: "no-path-anvil", Label: "ready", Action: "add"})
		resp := d.handleIPC(ipc.Command{Type: "update_label", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "no path configured")
	})

	// The queued subtests below dispatch a goroutine that execs `bd`. Install a
	// fake `bd` on PATH so those goroutines don't pick up a real bead binary
	// from the host (which would mutate the test tmpDir as a fake repo).
	if runtime.GOOS == "windows" {
		bdScript := filepath.Join(tmpDir, "bd.bat")
		bdContent := "@echo off\r\nexit /b 0\r\n"
		require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))
	} else {
		bdScript := filepath.Join(tmpDir, "bd")
		bdContent := "#!/bin/sh\nexit 0\n"
		require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))
	}
	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
	defer os.Setenv("PATH", oldPath)

	t.Run("queues add with valid payload", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.UpdateLabelPayload{BeadID: "BEAD-1", Anvil: "test-anvil", Label: "ready", Action: "add"})
		resp := d.handleIPC(ipc.Command{Type: "update_label", Payload: payload})
		assert.Equal(t, "queued", resp.Type)
	})

	t.Run("queues remove with valid payload", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.UpdateLabelPayload{BeadID: "BEAD-1", Anvil: "test-anvil", Label: "ready", Action: "remove"})
		resp := d.handleIPC(ipc.Command{Type: "update_label", Payload: payload})
		assert.Equal(t, "queued", resp.Type)
	})
}

// TestApplyDecomposedOutcome verifies the retry/circuit-breaker behavior of
// applyDecomposedOutcome:
//
//	(a) when SubBeads > 0 the retry record is cleared and no dispatch failure is recorded.
//	(b) when SubBeads is empty the retry record is preserved/incremented.
func TestApplyDecomposedOutcome(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-decomposed-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// Default beadShower/parentCloser to no-ops so maybeCloseDecomposedParent
	// doesn't panic when called indirectly by applyDecomposedOutcome.
	d.beadShower = func(anvilPath, beadID string) ([]byte, string, error) {
		return []byte(`{"dependents":[]}`), "", nil
	}
	d.parentCloser = func(anvilPath, beadID, reason string) error {
		return nil
	}
	d.cfg.Store(&config.Config{})

	const anvil = "test-anvil"

	t.Run("with sub-beads: clears retry record, no dispatch failure", func(t *testing.T) {
		const beadID = "DECOMP-WITH-CHILDREN"

		// Pre-seed a prior dispatch failure to confirm it gets cleared.
		_, _, err := db.IncrementDispatchFailures(beadID, anvil, 10, "prior failure")
		require.NoError(t, err)

		r, err := db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		require.NotNil(t, r)
		require.Equal(t, 1, r.DispatchFailures, "setup: dispatch failures should be 1")

		// Call with non-empty SubBeads: should clear the retry record.
		sr := &schematic.Result{
			Action:   schematic.ActionDecompose,
			SubBeads: []schematic.SubBead{{ID: "child-1", Title: "Child task"}},
		}
		d.applyDecomposedOutcome(poller.Bead{ID: beadID, Anvil: anvil}, config.AnvilConfig{}, sr)

		// Retry record should be gone (ClearRetry deleted it).
		r, err = db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		assert.Nil(t, r, "retry record should be cleared when sub-beads were created")
	})

	t.Run("no sub-beads: preserves and increments retry record", func(t *testing.T) {
		const beadID = "DECOMP-NO-CHILDREN"

		// No prior retry record.
		r, err := db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		assert.Nil(t, r, "setup: no retry record should exist yet")

		// Call with empty SubBeads: should record a dispatch failure.
		sr := &schematic.Result{
			Action:   schematic.ActionDecompose,
			SubBeads: nil,
			Reason:   "bead too ambiguous",
		}
		d.applyDecomposedOutcome(poller.Bead{ID: beadID, Anvil: anvil}, config.AnvilConfig{}, sr)

		// A dispatch failure should now be recorded.
		r, err = db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		require.NotNil(t, r, "retry record should exist after empty decomposition")
		assert.Equal(t, 1, r.DispatchFailures, "dispatch failures should be incremented")
		assert.Contains(t, r.LastError, "decomposition produced no child beads")
		assert.Contains(t, r.LastError, "bead too ambiguous", "failure reason should include schematic reason")
	})

	t.Run("no sub-beads: nil schematic result uses default reason", func(t *testing.T) {
		const beadID = "DECOMP-NIL-RESULT"

		d.applyDecomposedOutcome(poller.Bead{ID: beadID, Anvil: anvil}, config.AnvilConfig{}, nil)

		r, err := db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		require.NotNil(t, r)
		assert.Equal(t, 1, r.DispatchFailures)
		assert.Equal(t, "decomposition produced no child beads", r.LastError)
	})

	t.Run("tagged auto_dispatch: copies tag to children when parent has it", func(t *testing.T) {
		const beadID = "DECOMP-TAGGED-PARENT"

		// Track which children received the label.
		var mu sync.Mutex
		labeled := map[string]string{} // childID -> tag

		d.labelAdder = func(anvilPath, childID, tag string) error {
			mu.Lock()
			defer mu.Unlock()
			labeled[childID] = tag
			return nil
		}
		defer func() { d.labelAdder = nil }()

		sr := &schematic.Result{
			Action: schematic.ActionDecompose,
			SubBeads: []schematic.SubBead{
				{ID: "child-a", Title: "Child A"},
				{ID: "child-b", Title: "Child B"},
			},
		}
		parentBead := poller.Bead{
			ID:     beadID,
			Anvil:  anvil,
			Labels: []string{"forgeReady"},
		}
		anvilCfg := config.AnvilConfig{
			AutoDispatch:    "tagged",
			AutoDispatchTag: "forgeReady",
		}

		d.applyDecomposedOutcome(parentBead, anvilCfg, sr)

		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, "forgeReady", labeled["child-a"], "child-a should receive the forgeReady tag")
		assert.Equal(t, "forgeReady", labeled["child-b"], "child-b should receive the forgeReady tag")

		// Retry record should be cleared (successful decomposition).
		r, err := db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		assert.Nil(t, r, "retry record should be cleared after successful decomposition")
	})

	t.Run("tagged auto_dispatch: skips tagging when parent lacks the tag", func(t *testing.T) {
		const beadID = "DECOMP-TAGGED-NO-PARENT-TAG"

		called := false
		d.labelAdder = func(anvilPath, childID, tag string) error {
			called = true
			return nil
		}
		defer func() { d.labelAdder = nil }()

		sr := &schematic.Result{
			Action:   schematic.ActionDecompose,
			SubBeads: []schematic.SubBead{{ID: "child-c", Title: "Child C"}},
		}
		parentBead := poller.Bead{
			ID:     beadID,
			Anvil:  anvil,
			Labels: []string{"someOtherLabel"},
		}
		anvilCfg := config.AnvilConfig{
			AutoDispatch:    "tagged",
			AutoDispatchTag: "forgeReady",
		}

		d.applyDecomposedOutcome(parentBead, anvilCfg, sr)

		assert.False(t, called, "labelAdder should not be called when parent lacks the dispatch tag")
	})

	t.Run("non-tagged auto_dispatch: skips tagging entirely", func(t *testing.T) {
		const beadID = "DECOMP-ALL-DISPATCH"

		called := false
		d.labelAdder = func(anvilPath, childID, tag string) error {
			called = true
			return nil
		}
		defer func() { d.labelAdder = nil }()

		sr := &schematic.Result{
			Action:   schematic.ActionDecompose,
			SubBeads: []schematic.SubBead{{ID: "child-d", Title: "Child D"}},
		}
		parentBead := poller.Bead{
			ID:     beadID,
			Anvil:  anvil,
			Labels: []string{"forgeReady"},
		}
		anvilCfg := config.AnvilConfig{
			AutoDispatch: "all",
		}

		d.applyDecomposedOutcome(parentBead, anvilCfg, sr)

		assert.False(t, called, "labelAdder should not be called for non-tagged auto_dispatch mode")
	})
}
func TestMaybeCloseDecomposedParent(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-maybe-close-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	const anvil = "test-anvil"
	anvilCfg := config.AnvilConfig{Path: tmpDir}

	t.Run("no dependents: auto-closes parent", func(t *testing.T) {
		const beadID = "PARENT-NO-DEPS"

		closeCalled := false
		closeBeadID := ""
		closeReason := ""

		d := &Daemon{
			db:     db,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		d.beadShower = func(anvilPath, id string) ([]byte, string, error) {
			return []byte(`{"id":"` + id + `","dependents":[]}`), "", nil
		}
		d.parentCloser = func(anvilPath, id, reason string) error {
			closeCalled = true
			closeBeadID = id
			closeReason = reason
			return nil
		}

		d.maybeCloseDecomposedParent(poller.Bead{ID: beadID, Anvil: anvil}, anvilCfg, 3)

		assert.True(t, closeCalled, "parentCloser should be called when no dependents")
		assert.Equal(t, beadID, closeBeadID)
		assert.Contains(t, closeReason, "3 children")

		// Verify event was logged.
		events, err := db.RecentEvents(10)
		require.NoError(t, err, "RecentEvents should succeed")
		found := false
		for _, ev := range events {
			if ev.Type == state.EventBeadAutoClosed && ev.BeadID == beadID {
				found = true
				assert.Contains(t, ev.Message, "3 children")
				break
			}
		}
		assert.True(t, found, "EventBeadAutoClosed event should be logged")
	})

	t.Run("has dependents: keeps parent open", func(t *testing.T) {
		const beadID = "PARENT-HAS-DEPS"

		closeCalled := false

		d := &Daemon{
			db:     db,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		d.beadShower = func(anvilPath, id string) ([]byte, string, error) {
			resp := `{"id":"` + id + `","dependents":[{"id":"OTHER-BEAD","dependency_type":"depends_on"}]}`
			return []byte(resp), "", nil
		}
		d.parentCloser = func(anvilPath, id, reason string) error {
			closeCalled = true
			return nil
		}

		d.maybeCloseDecomposedParent(poller.Bead{ID: beadID, Anvil: anvil}, anvilCfg, 2)

		assert.False(t, closeCalled, "parentCloser should NOT be called when parent has dependents")
	})

	t.Run("bd show fails: leaves parent open", func(t *testing.T) {
		const beadID = "PARENT-SHOW-FAIL"

		closeCalled := false

		d := &Daemon{
			db:     db,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		d.beadShower = func(anvilPath, id string) ([]byte, string, error) {
			return nil, "connection refused", assert.AnError
		}
		d.parentCloser = func(anvilPath, id, reason string) error {
			closeCalled = true
			return nil
		}

		d.maybeCloseDecomposedParent(poller.Bead{ID: beadID, Anvil: anvil}, anvilCfg, 1)

		assert.False(t, closeCalled, "parentCloser should NOT be called when bd show fails")
	})

	t.Run("invalid JSON: leaves parent open", func(t *testing.T) {
		const beadID = "PARENT-BAD-JSON"

		closeCalled := false

		d := &Daemon{
			db:     db,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		d.beadShower = func(anvilPath, id string) ([]byte, string, error) {
			return []byte(`not valid json`), "", nil
		}
		d.parentCloser = func(anvilPath, id, reason string) error {
			closeCalled = true
			return nil
		}

		d.maybeCloseDecomposedParent(poller.Bead{ID: beadID, Anvil: anvil}, anvilCfg, 1)

		assert.False(t, closeCalled, "parentCloser should NOT be called when JSON parsing fails")
	})

	t.Run("bd close fails: logs warning but does not panic", func(t *testing.T) {
		const beadID = "PARENT-CLOSE-FAIL"

		d := &Daemon{
			db:     db,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		d.beadShower = func(anvilPath, id string) ([]byte, string, error) {
			return []byte(`{"id":"` + id + `","dependents":[]}`), "", nil
		}
		d.parentCloser = func(anvilPath, id, reason string) error {
			return assert.AnError
		}

		// Should not panic.
		d.maybeCloseDecomposedParent(poller.Bead{ID: beadID, Anvil: anvil}, anvilCfg, 2)

		// No EventBeadAutoClosed event should be logged for this bead.
		events, err := db.RecentEvents(50)
		require.NoError(t, err, "RecentEvents should succeed")
		for _, ev := range events {
			if ev.Type == state.EventBeadAutoClosed && ev.BeadID == beadID {
				t.Fatal("EventBeadAutoClosed should NOT be logged when bd close fails")
			}
		}
	})

	t.Run("wrapped array response: unwraps and parses correctly", func(t *testing.T) {
		const beadID = "PARENT-WRAPPED"

		closeCalled := false

		d := &Daemon{
			db:     db,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		d.beadShower = func(anvilPath, id string) ([]byte, string, error) {
			// bd show --json sometimes returns [{...}]
			return []byte(`[{"id":"` + id + `","dependents":[]}]`), "", nil
		}
		d.parentCloser = func(anvilPath, id, reason string) error {
			closeCalled = true
			return nil
		}

		d.maybeCloseDecomposedParent(poller.Bead{ID: beadID, Anvil: anvil}, anvilCfg, 1)

		assert.True(t, closeCalled, "parentCloser should be called after unwrapping array response")
	})
}

func TestHandleIPC_StopBead(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-stop-bead-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
		reqTracker:    *ipc.NewRequestTracker("test-"),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {Path: tmpDir, AutoDispatchTag: "forge-ready"},
		},
	})

	t.Run("invalid JSON payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "stop_bead", Payload: []byte("invalid")})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		require.NoError(t, json.Unmarshal(resp.Payload, &msg))
		assert.Contains(t, msg["message"], "invalid stop_bead payload")
	})

	t.Run("missing bead_id", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.StopBeadPayload{Anvil: "test-anvil", Reason: "test"})
		resp := d.handleIPC(ipc.Command{Type: "stop_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		require.NoError(t, json.Unmarshal(resp.Payload, &msg))
		assert.Contains(t, msg["message"], "bead_id and anvil are required")
	})

	t.Run("missing anvil", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.StopBeadPayload{BeadID: "BEAD-1", Reason: "test"})
		resp := d.handleIPC(ipc.Command{Type: "stop_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		require.NoError(t, json.Unmarshal(resp.Payload, &msg))
		assert.Contains(t, msg["message"], "bead_id and anvil are required")
	})

	t.Run("unknown anvil", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.StopBeadPayload{BeadID: "BEAD-1", Anvil: "nonexistent", Reason: "test"})
		resp := d.handleIPC(ipc.Command{Type: "stop_bead", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		require.NoError(t, json.Unmarshal(resp.Payload, &msg))
		assert.Contains(t, msg["message"], "not found")
	})

	t.Run("sets clarification_needed and frees active slot even when bd release fails", func(t *testing.T) {
		const beadID = "BEAD-STOP-1"
		const anvil = "test-anvil"

		// Pre-populate activeBeads so we can verify ordering via DB read.
		d.activeBeads.Store(beadID, struct{}{})

		payload, _ := json.Marshal(ipc.StopBeadPayload{
			BeadID: beadID,
			Anvil:  anvil,
			Reason: "manually stopped by user",
		})
		resp := d.handleIPC(ipc.Command{Type: "stop_bead", Payload: payload})
		// bd shelling is now async — the synchronous response is "queued".
		assert.Equal(t, "queued", resp.Type)
		assert.NotEmpty(t, resp.RequestID, "queued stop_bead response should include a request_id")

		// Verify clarification_needed was persisted in DB.
		retry, err := db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		require.NotNil(t, retry, "retry record should exist after stop")
		assert.True(t, retry.ClarificationNeeded, "clarification_needed should be set")

		// Verify active slot was freed.
		_, stillActive := d.activeBeads.Load(beadID)
		assert.False(t, stillActive, "bead should be removed from activeBeads")
	})

	t.Run("reason sanitization strips control characters", func(t *testing.T) {
		const beadID = "BEAD-STOP-2"
		const anvil = "test-anvil"

		maliciousReason := "stop\x1b[31mRED\x1b[0m\x00\x07"
		payload, _ := json.Marshal(ipc.StopBeadPayload{
			BeadID: beadID,
			Anvil:  anvil,
			Reason: maliciousReason,
		})
		resp := d.handleIPC(ipc.Command{Type: "stop_bead", Payload: payload})
		// Response may be "error" due to bd not being available, but
		// clarification was already set with the sanitized reason.

		_ = resp // Response varies by bd availability.

		// Confirm the stored reason does not contain control chars.
		retry, err := db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		require.NotNil(t, retry)
		for _, r := range retry.LastError {
			if r < 32 && r != '\n' {
				t.Errorf("stored reason contains control character %q in: %q", r, retry.LastError)
			}
		}
	})
}
func TestHandleIPC_CrucibleAction(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-crucible-action-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {Path: tmpDir, AutoDispatchTag: "forge-ready"},
		},
	})

	t.Run("invalid JSON payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "crucible_action", Payload: []byte("invalid")})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		require.NoError(t, json.Unmarshal(resp.Payload, &msg))
		assert.Contains(t, msg["message"], "invalid crucible_action payload")
	})

	t.Run("missing parent_id", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.CrucibleActionPayload{Anvil: "test-anvil", Action: "resume"})
		resp := d.handleIPC(ipc.Command{Type: "crucible_action", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		require.NoError(t, json.Unmarshal(resp.Payload, &msg))
		assert.Contains(t, msg["message"], "parent_id and anvil are required")
	})

	t.Run("missing anvil", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.CrucibleActionPayload{ParentID: "Forge-epic1", Action: "resume"})
		resp := d.handleIPC(ipc.Command{Type: "crucible_action", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		require.NoError(t, json.Unmarshal(resp.Payload, &msg))
		assert.Contains(t, msg["message"], "parent_id and anvil are required")
	})

	t.Run("unknown anvil", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.CrucibleActionPayload{ParentID: "Forge-epic1", Anvil: "nonexistent", Action: "resume"})
		resp := d.handleIPC(ipc.Command{Type: "crucible_action", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		require.NoError(t, json.Unmarshal(resp.Payload, &msg))
		assert.Contains(t, msg["message"], "not found")
	})

	t.Run("unknown action", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.CrucibleActionPayload{ParentID: "Forge-epic1", Anvil: "test-anvil", Action: "bogus"})
		resp := d.handleIPC(ipc.Command{Type: "crucible_action", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		require.NoError(t, json.Unmarshal(resp.Payload, &msg))
		assert.Contains(t, msg["message"], "unknown crucible action")
	})

	t.Run("stop removes status map entry with composite key", func(t *testing.T) {
		const parentID = "Forge-epic2"
		const anvil = "test-anvil"
		compositeKey := anvil + "/" + parentID

		// Pre-populate status map under composite key
		d.crucibleStatuses.Store(compositeKey, struct{}{})
		_, loaded := d.crucibleStatuses.Load(compositeKey)
		require.True(t, loaded, "precondition: status should be present")

		payload, _ := json.Marshal(ipc.CrucibleActionPayload{ParentID: parentID, Anvil: anvil, Action: "stop"})
		resp := d.handleIPC(ipc.Command{Type: "crucible_action", Payload: payload})
		// bd is not available in test env so stop may fail; regardless, if it
		// succeeded the status map entry must be deleted.
		if resp.Type == "ok" {
			_, still := d.crucibleStatuses.Load(compositeKey)
			assert.False(t, still, "crucible status should be removed from map after stop")
		}
	})

	t.Run("resume error does not remove status map entry", func(t *testing.T) {
		const parentID = "Forge-epic3"
		const anvil = "test-anvil"
		compositeKey := anvil + "/" + parentID

		// Pre-populate status map
		d.crucibleStatuses.Store(compositeKey, struct{}{})

		payload, _ := json.Marshal(ipc.CrucibleActionPayload{ParentID: parentID, Anvil: anvil, Action: "resume"})
		resp := d.handleIPC(ipc.Command{Type: "crucible_action", Payload: payload})
		// bd is not available in test env so resume will return an error
		// (either from bd update or from ResetDispatchFailures).
		// If an error is returned, the status map entry must still be present.
		if resp.Type == "error" {
			_, still := d.crucibleStatuses.Load(compositeKey)
			assert.True(t, still, "crucible status should NOT be removed when resume fails")
		}
	})
}

// TestHandleLifecycleAction_CloseBead verifies that when bellows emits
// EventPRMerged, the lifecycle manager dispatches ActionCloseBead and the
// daemon calls bd close for the bead. This covers the deferred-close path
// where the pipeline skipped closing because the bead had dependents.
func TestHandleLifecycleAction_CloseBead(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-close-bead-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create a fake bd script that records close calls via a marker file.
	markerFile := filepath.Join(tmpDir, "bd-close-called.txt")
	var bdScript string
	var bdContent string
	if runtime.GOOS == "windows" {
		bdScript = filepath.Join(tmpDir, "bd.bat")
		bdContent = "@echo off\r\nif \"%1\"==\"close\" (\r\n    echo %2 %3 > \"" + markerFile + "\"\r\n    echo {\"id\": \"%2\", \"status\": \"closed\"}\r\n    exit /b 0\r\n)\r\nexit /b 0\r\n"
	} else {
		bdScript = filepath.Join(tmpDir, "bd")
		bdContent = "#!/bin/sh\nif [ \"$1\" = \"close\" ]; then\n    echo \"$2 $3\" > '" + markerFile + "'\n    echo '{\"id\": \"'\"$2\"'\", \"status\": \"closed\"}'\n    exit 0\nfi\nexit 0\n"
	}
	require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
	defer os.Setenv("PATH", oldPath)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := &Daemon{
		db:            db,
		logger:        logger,
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {Path: tmpDir},
		},
	})

	// Wire the lifecycle manager to dispatch through the daemon's handler.
	lm := lifecycle.New(db, logger, d.handleLifecycleAction)
	d.lifecycleMgr = lm

	// Simulate bellows emitting EventPRMerged.
	lm.HandleEvent(context.Background(), bellows.PREvent{
		PRNumber:  42,
		BeadID:    "DEFERRED-1",
		Anvil:     "test-anvil",
		Branch:    "forge/DEFERRED-1",
		EventType: bellows.EventPRMerged,
	})

	// Wait for the background goroutine to complete.
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for handleLifecycleAction to complete")
	}

	// Verify bd close was called by checking the marker file.
	_, err = os.Stat(markerFile)
	assert.NoError(t, err, "bd close should have been called (marker file should exist)")

	if err == nil {
		content, _ := os.ReadFile(markerFile)
		assert.Contains(t, string(content), "DEFERRED-1", "bd close should have been called with the correct bead ID")
	}

	// Verify lifecycle state shows the PR as merged.
	st := lm.GetState("test-anvil", 42)
	require.NotNil(t, st)
	assert.True(t, st.Merged, "lifecycle state should show PR as merged")
}

// TestHandleLifecycleAction_CloseBead_Error verifies that when bd close fails
// in the ActionCloseBead handler, the error is logged (not silently discarded)
// and the goroutine completes without panicking.
func TestHandleLifecycleAction_CloseBead_Error(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-close-bead-err-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create a fake bd script that fails on close.
	var bdScript string
	var bdContent string
	if runtime.GOOS == "windows" {
		bdScript = filepath.Join(tmpDir, "bd.bat")
		bdContent = "@echo off\r\nif \"%1\"==\"close\" (\r\n    echo bead not found 1>&2\r\n    exit /b 1\r\n)\r\nexit /b 0\r\n"
	} else {
		bdScript = filepath.Join(tmpDir, "bd")
		bdContent = "#!/bin/sh\nif [ \"$1\" = \"close\" ]; then\n    echo 'bead not found' >&2\n    exit 1\nfi\nexit 0\n"
	}
	require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
	defer os.Setenv("PATH", oldPath)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := &Daemon{
		db:            db,
		logger:        logger,
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {Path: tmpDir},
		},
	})

	lm := lifecycle.New(db, logger, d.handleLifecycleAction)
	d.lifecycleMgr = lm

	// Simulate EventPRMerged — bd close will fail but should not panic.
	lm.HandleEvent(context.Background(), bellows.PREvent{
		PRNumber:  99,
		BeadID:    "FAIL-CLOSE-1",
		Anvil:     "test-anvil",
		Branch:    "forge/FAIL-CLOSE-1",
		EventType: bellows.EventPRMerged,
	})

	// Wait for the background goroutine to complete without panic.
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
		// Success — the error was handled gracefully.
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for handleLifecycleAction to complete")
	}

	// Verify lifecycle state still shows merged despite the close error.
	st := lm.GetState("test-anvil", 99)
	require.NotNil(t, st)
	assert.True(t, st.Merged, "lifecycle state should show PR as merged even when bd close fails")
}

// TestEffectiveMaxLifecycleWorkers verifies the fallback to the package default
// when the configured limit is unset or non-positive.
func TestEffectiveMaxLifecycleWorkers(t *testing.T) {
	assert.Equal(t, config.DefaultMaxLifecycleWorkers, effectiveMaxLifecycleWorkers(0))
	assert.Equal(t, config.DefaultMaxLifecycleWorkers, effectiveMaxLifecycleWorkers(-5))
	assert.Equal(t, 1, effectiveMaxLifecycleWorkers(1))
	assert.Equal(t, 7, effectiveMaxLifecycleWorkers(7))
}

// TestReserveLifecycleSlot_Concurrent verifies that no more than the configured
// limit of lifecycle slots can be reserved concurrently, and that releasing a
// slot lets a subsequent reservation succeed.
func TestReserveLifecycleSlot_Concurrent(t *testing.T) {
	const limit = 2
	const goroutines = 16

	d := &Daemon{lifecycleCond: sync.NewCond(&sync.Mutex{})}

	var granted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d.reserveLifecycleSlot(limit) {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(limit), granted.Load(), "exactly %d slots should be granted", limit)
	assert.Equal(t, int64(limit), d.lifecycleActive.Load(), "counter should reflect granted slots")

	// At capacity: further reservations fail.
	assert.False(t, d.reserveLifecycleSlot(limit), "reservation must fail when at capacity")

	// Releasing one slot frees capacity for exactly one more reservation.
	d.releaseLifecycleSlot()
	assert.True(t, d.reserveLifecycleSlot(limit), "reservation must succeed after a slot frees")
	assert.False(t, d.reserveLifecycleSlot(limit), "reservation must fail again once back at capacity")
}

// TestReserveLifecycleSlot_DefaultLimit verifies that a non-positive limit falls
// back to the package default.
func TestReserveLifecycleSlot_DefaultLimit(t *testing.T) {
	d := &Daemon{lifecycleCond: sync.NewCond(&sync.Mutex{})}
	for i := 0; i < config.DefaultMaxLifecycleWorkers; i++ {
		assert.True(t, d.reserveLifecycleSlot(0), "reservation %d should succeed under default limit", i)
	}
	assert.False(t, d.reserveLifecycleSlot(0), "reservation beyond default limit must fail")
}

// TestHandleLifecycleAction_RespectsLifecycleCap verifies that a fix action
// blocks when the lifecycle concurrency cap is saturated, and proceeds once a
// slot frees — without burning retry counters.
func TestHandleLifecycleAction_RespectsLifecycleCap(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-lifecycle-cap-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	d := &Daemon{
		db:            db,
		logger:        logger,
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
		lifecycleCond: sync.NewCond(&sync.Mutex{}),
	}
	d.cfg.Store(&config.Config{
		Settings: config.SettingsConfig{MaxLifecycleWorkers: 1},
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {Path: tmpDir},
		},
	})

	lm := lifecycle.New(db, logger, d.handleLifecycleAction)
	d.lifecycleMgr = lm

	// Saturate the single lifecycle slot to simulate an in-flight fix worker.
	require.True(t, d.reserveLifecycleSlot(1))

	// Emit a CI failure: the lifecycle manager dispatches ActionFixCI, but the
	// handler must block waiting for a slot (not defer/reset).
	lm.HandleEvent(context.Background(), bellows.PREvent{
		PRNumber:  7,
		BeadID:    "CAP-1",
		Anvil:     "test-anvil",
		Branch:    "forge/CAP-1",
		EventType: bellows.EventCIFailed,
	})

	// Give the handler goroutine time to park on the Cond wait.
	time.Sleep(100 * time.Millisecond)

	// The counter must still reflect only the pre-reserved slot — the blocked
	// handler has not yet acquired a slot.
	assert.Equal(t, int64(1), d.lifecycleActive.Load(), "blocked action must not change the lifecycle counter")

	// Release the pre-reserved slot to unblock the waiting handler. It will
	// proceed to acquire a slot and then fail at worktree creation (the anvil
	// path is a temp dir, not a git repo), releasing the slot and finishing.
	d.releaseLifecycleSlot()

	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for handleLifecycleAction to complete")
	}

	// After the handler finishes (worktree creation fails), the slot must be
	// released back, leaving the counter at zero.
	assert.Equal(t, int64(0), d.lifecycleActive.Load(), "lifecycle counter must return to zero after handler finishes")
}

// TestWaitForLifecycleSlot_CancelledContext verifies that waitForLifecycleSlot
// returns false promptly when the context is cancelled while waiting.
func TestWaitForLifecycleSlot_CancelledContext(t *testing.T) {
	d := &Daemon{
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		lifecycleCond: sync.NewCond(&sync.Mutex{}),
	}

	// Saturate the slot.
	require.True(t, d.reserveLifecycleSlot(1))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan bool, 1)
	go func() {
		done <- d.waitForLifecycleSlot(ctx, 1)
	}()

	// Give the goroutine time to park on the Cond wait.
	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case got := <-done:
		assert.False(t, got, "waitForLifecycleSlot must return false on cancelled context")
	case <-time.After(5 * time.Second):
		t.Fatal("waitForLifecycleSlot did not return after context cancellation")
	}

	// The counter must still be 1 (only the pre-reserved slot).
	assert.Equal(t, int64(1), d.lifecycleActive.Load())
}

// mockVCSProvider implements vcs.Provider for testing.
type mockVCSProvider struct {
	mergeCalls atomic.Int32
	mergeErr   error
	// createPRResult and createPRErr control what CreatePR returns.
	// Zero values return nil, nil (success with nil PR — safe for tests that
	// don't exercise PR creation). Set createPRResult to a non-nil *vcs.PR
	// when the caller will dereference the return value.
	createPRResult *vcs.PR
	createPRErr    error
	createPRCalls  atomic.Int32
	// lastCreateParams captures the CreateParams of the most recent CreatePR
	// call so tests can assert the PR base branch (e.g. a Crucible child's
	// resolved epic branch) is wired through correctly.
	lastCreateParams vcs.CreateParams
	// createPRFunc, when non-nil, overrides the static createPRResult/createPRErr
	// and is passed the 1-based call count so tests can simulate a transient
	// failure on the first attempt followed by success on a retry.
	createPRFunc func(call int) (*vcs.PR, error)
	// openPRs controls what ListOpenPRs and GetPRByHeadBranch return. Used by
	// tests that exercise the ErrPRAlreadyExists recovery path.
	openPRs []vcs.OpenPR
	// getPRByHeadBranchFunc, when non-nil, overrides GetPRByHeadBranch and is
	// passed the 1-based call count so tests can simulate a PR appearing
	// concurrently between the pre-dispatch lookup and the recovery re-check.
	getPRByHeadBranchFunc func(branch string, call int) (*vcs.OpenPR, error)
	getPRCalls            atomic.Int32
}

func (m *mockVCSProvider) MergePR(_ context.Context, _ string, _ int, _ string) error {
	m.mergeCalls.Add(1)
	return m.mergeErr
}
func (m *mockVCSProvider) CreatePR(_ context.Context, params vcs.CreateParams) (*vcs.PR, error) {
	m.lastCreateParams = params
	call := int(m.createPRCalls.Add(1))
	if m.createPRFunc != nil {
		return m.createPRFunc(call)
	}
	return m.createPRResult, m.createPRErr
}
func (m *mockVCSProvider) CheckStatus(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	return nil, nil
}
func (m *mockVCSProvider) CheckStatusLight(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	return nil, nil
}
func (m *mockVCSProvider) ListOpenPRs(_ context.Context, _ string) ([]vcs.OpenPR, error) {
	return m.openPRs, nil
}
func (m *mockVCSProvider) GetPRByHeadBranch(_ context.Context, _ string, branch string) (*vcs.OpenPR, error) {
	call := int(m.getPRCalls.Add(1))
	if m.getPRByHeadBranchFunc != nil {
		return m.getPRByHeadBranchFunc(branch, call)
	}
	for i := range m.openPRs {
		if m.openPRs[i].Branch == branch {
			return &m.openPRs[i], nil
		}
	}
	return nil, nil
}
func (m *mockVCSProvider) GetRepoOwnerAndName(_ context.Context, _ string) (string, string, error) {
	return "", "", nil
}
func (m *mockVCSProvider) FetchUnresolvedThreadCount(_ context.Context, _ string, _ int) (int, error) {
	return 0, nil
}
func (m *mockVCSProvider) FetchPendingReviewRequests(_ context.Context, _ string, _ int) ([]vcs.ReviewRequest, error) {
	return nil, nil
}
func (m *mockVCSProvider) FetchPRChecks(_ context.Context, _ string, _ int) (string, []vcs.CICheck, error) {
	return "", nil, nil
}
func (m *mockVCSProvider) FetchCILogs(_ context.Context, _ string, _ []vcs.CICheck) (map[string]string, error) {
	return nil, nil
}
func (m *mockVCSProvider) FetchReviewComments(_ context.Context, _ string, _ int) ([]vcs.ReviewComment, error) {
	return nil, nil
}
func (m *mockVCSProvider) ResolveThread(_ context.Context, _ string, _ string) error {
	return nil
}
func (m *mockVCSProvider) Platform() vcs.Platform { return vcs.GitHub }

func TestHandleAutoMerge(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-auto-merge-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// newDaemon creates a fresh Daemon with its own mockVCSProvider so that
	// subtests do not share mutable state (avoids data races on mergeErr).
	newDaemon := func(mergeErr error) (*Daemon, *mockVCSProvider) {
		mock := &mockVCSProvider{mergeErr: mergeErr}
		d := &Daemon{
			db:     db,
			logger: logger,
			vcsProviders: map[string]vcs.Provider{
				"test-anvil": mock,
			},
		}
		return d, mock
	}

	t.Run("skips external PRs", func(t *testing.T) {
		d, mock := newDaemon(nil)
		d.cfg.Store(&config.Config{
			Anvils: map[string]config.AnvilConfig{
				"test-anvil": {Path: tmpDir, AutoMerge: true},
			},
		})
		pr := state.PR{Number: 1, BeadID: "ext-123", Anvil: "test-anvil"}
		d.handleAutoMerge(context.Background(), "test-anvil", pr)
		// handleAutoMerge returns synchronously for external PRs.
		assert.Equal(t, int32(0), mock.mergeCalls.Load(), "should not merge external PRs")
	})

	t.Run("skips when auto_merge disabled", func(t *testing.T) {
		d, mock := newDaemon(nil)
		d.cfg.Store(&config.Config{
			Anvils: map[string]config.AnvilConfig{
				"test-anvil": {Path: tmpDir, AutoMerge: false},
			},
		})
		pr := state.PR{Number: 2, BeadID: "BEAD-1", Anvil: "test-anvil"}
		d.handleAutoMerge(context.Background(), "test-anvil", pr)
		// handleAutoMerge returns synchronously when auto_merge is off.
		assert.Equal(t, int32(0), mock.mergeCalls.Load(), "should not merge when auto_merge is false")
	})

	t.Run("merges when auto_merge enabled", func(t *testing.T) {
		d, mock := newDaemon(nil)
		d.cfg.Store(&config.Config{
			Anvils: map[string]config.AnvilConfig{
				"test-anvil": {Path: tmpDir, AutoMerge: true},
			},
			Settings: config.SettingsConfig{MergeStrategy: "squash"},
		})
		pr := state.PR{Number: 3, BeadID: "BEAD-2", Anvil: "test-anvil"}
		d.handleAutoMerge(context.Background(), "test-anvil", pr)
		// doAutoMerge runs in a goroutine — wait briefly for it.
		assert.Eventually(t, func() bool {
			return mock.mergeCalls.Load() == 1
		}, 5*time.Second, 10*time.Millisecond, "should call MergePR once")
	})

	t.Run("handles merge failure gracefully", func(t *testing.T) {
		// mergeErr is set at construction time so MergePR reads it without racing.
		d, mock := newDaemon(fmt.Errorf("merge conflict"))
		d.cfg.Store(&config.Config{
			Anvils: map[string]config.AnvilConfig{
				"test-anvil": {Path: tmpDir, AutoMerge: true},
			},
			Settings: config.SettingsConfig{MergeStrategy: "rebase"},
		})
		pr := state.PR{Number: 4, BeadID: "BEAD-3", Anvil: "test-anvil"}
		d.handleAutoMerge(context.Background(), "test-anvil", pr)
		// Should still call MergePR and handle the error without panicking.
		assert.Eventually(t, func() bool {
			return mock.mergeCalls.Load() == 1
		}, 5*time.Second, 10*time.Millisecond, "should attempt merge even if it fails")
	})

	t.Run("defaults strategy to squash", func(t *testing.T) {
		d, mock := newDaemon(nil)
		d.cfg.Store(&config.Config{
			Anvils: map[string]config.AnvilConfig{
				"test-anvil": {Path: tmpDir, AutoMerge: true},
			},
			Settings: config.SettingsConfig{MergeStrategy: ""}, // empty
		})
		pr := state.PR{Number: 5, BeadID: "BEAD-4", Anvil: "test-anvil"}
		d.doAutoMerge(context.Background(), "test-anvil", tmpDir, pr)
		assert.Equal(t, int32(1), mock.mergeCalls.Load(), "should call MergePR")
	})
}

// TestApplyNoChangesNeededOutcome verifies the terminal no-changes-needed path:
// - on success: retries are cleared and EventNoChangesNeeded is logged
// - on close failure: bead is immediately marked needs_human (not circuit-broken)
func TestApplyNoChangesNeededOutcome(t *testing.T) {
	// Use a real git repo so forgeBranchAheadOfMain can run ls-remote without
	// erroring (no forge branch exists → returns false → close proceeds normally).
	anvilPath := initTestGitRepo(t)

	dbPath := filepath.Join(anvilPath, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	const anvil = "test-anvil"

	t.Run("success: clears retry and logs event", func(t *testing.T) {
		const beadID = "NCN-SUCCESS"

		// Seed a prior dispatch failure to confirm ClearRetry runs.
		_, _, err := db.IncrementDispatchFailures(beadID, anvil, 10, "prior failure")
		require.NoError(t, err)

		// Build a fake bd script that handles 'bd close'.
		scriptDir, err := os.MkdirTemp("", "forge-ncn-bd-*")
		require.NoError(t, err)
		defer os.RemoveAll(scriptDir)
		var bdScript, bdContent string
		if runtime.GOOS == "windows" {
			bdScript = filepath.Join(scriptDir, "bd.bat")
			bdContent = "@echo off\r\nif \"%1\"==\"close\" ( exit /b 0 )\r\nexit /b 1\r\n"
		} else {
			bdScript = filepath.Join(scriptDir, "bd")
			bdContent = "#!/bin/sh\nif [ \"$1\" = \"close\" ]; then exit 0; fi\nexit 1\n"
		}
		require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))
		oldPath := os.Getenv("PATH")
		os.Setenv("PATH", scriptDir+string(os.PathListSeparator)+oldPath)
		defer os.Setenv("PATH", oldPath)

		d := &Daemon{
			db:          db,
			logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			worktreeMgr: worktree.NewManager(),
		}
		d.cfg.Store(&config.Config{})

		bead := poller.Bead{ID: beadID, Anvil: anvil}
		d.applyNoChangesNeededOutcome(context.Background(), bead, anvilPath, "already fixed upstream")

		// Retry record should be cleared.
		r, err := db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		assert.Nil(t, r, "retry record should be cleared on successful close")

		// EventNoChangesNeeded should be logged.
		events, err := db.RecentEvents(20)
		require.NoError(t, err)
		found := false
		for _, ev := range events {
			if ev.Type == state.EventNoChangesNeeded && ev.BeadID == beadID {
				found = true
				assert.Contains(t, ev.Message, "already fixed upstream")
				break
			}
		}
		assert.True(t, found, "EventNoChangesNeeded should be logged after successful close")
	})

	t.Run("close failure: marks needs_human immediately", func(t *testing.T) {
		const beadID = "NCN-CLOSE-FAIL"

		// Place a failing bd stub at the FRONT of PATH, keeping the rest of PATH
		// intact so that git remains accessible (needed by forgeBranchAheadOfMain).
		failScriptDir, err := os.MkdirTemp("", "forge-fail-bd-*")
		require.NoError(t, err)
		defer os.RemoveAll(failScriptDir)
		var failBdScript, failBdContent string
		if runtime.GOOS == "windows" {
			failBdScript = filepath.Join(failScriptDir, "bd.bat")
			failBdContent = "@echo off\r\nexit /b 1\r\n"
		} else {
			failBdScript = filepath.Join(failScriptDir, "bd")
			failBdContent = "#!/bin/sh\nexit 1\n"
		}
		require.NoError(t, os.WriteFile(failBdScript, []byte(failBdContent), 0o755))

		oldPath := os.Getenv("PATH")
		os.Setenv("PATH", failScriptDir+string(os.PathListSeparator)+oldPath)
		defer os.Setenv("PATH", oldPath)

		d := &Daemon{
			db:          db,
			logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			worktreeMgr: worktree.NewManager(),
		}
		d.cfg.Store(&config.Config{})

		bead := poller.Bead{ID: beadID, Anvil: anvil}
		d.applyNoChangesNeededOutcome(context.Background(), bead, anvilPath, "no work needed")

		// Bead should be immediately marked needs_human (not waiting for circuit breaker).
		r, err := db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		require.NotNil(t, r, "retry record should exist after failed close")
		assert.True(t, r.NeedsHuman, "bead should be marked needs_human when close fails")
		assert.Contains(t, r.LastError, "no changes needed but close failed")

		// EventNoChangesNeeded must NOT be logged (close failed).
		events, err := db.RecentEvents(20)
		require.NoError(t, err)
		for _, ev := range events {
			if ev.Type == state.EventNoChangesNeeded && ev.BeadID == beadID {
				t.Fatal("EventNoChangesNeeded should NOT be logged when close fails")
			}
		}
	})

	t.Run("orphaned branch: auto-creates PR on success", func(t *testing.T) {
		const beadID = "NCN-ORPHAN-OK"
		orphanAnvilPath := initTestGitRepo(t)

		orphanDB, err := state.Open(filepath.Join(orphanAnvilPath, "state-orphan-ok.db"))
		require.NoError(t, err)
		defer orphanDB.Close()

		// Create a forge branch with a commit ahead of main and push it to origin,
		// simulating a prior dispatch that pushed work but never created a PR.
		branchName := worktree.BranchName(beadID)
		gitLocal := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = orphanAnvilPath
			cmd.Env = cleanGitTestEnv()
			out, runErr := cmd.CombinedOutput()
			require.NoError(t, runErr, "git %v: %s", args, out)
		}
		gitLocal("checkout", "-b", branchName)
		require.NoError(t, os.WriteFile(filepath.Join(orphanAnvilPath, "orphan.txt"), []byte("work\n"), 0o644))
		gitLocal("add", "orphan.txt")
		gitLocal("commit", "-m", "orphaned work")
		gitLocal("push", "origin", branchName)
		gitLocal("checkout", "main")

		mockVCS := &mockVCSProvider{
			createPRResult: &vcs.PR{Number: 42, URL: "https://github.com/test/repo/pull/42"},
		}
		d := &Daemon{
			db:           orphanDB,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			worktreeMgr:  worktree.NewManager(),
			vcsProviders: map[string]vcs.Provider{anvil: mockVCS},
		}
		d.cfg.Store(&config.Config{})

		// Simulate a prior run that failed and left a needs_human=1 retry record —
		// this is the core regression scenario: the bead is stuck in Needs Attention
		// even though the branch exists and a PR can now be created.
		require.NoError(t, orphanDB.MarkNeedsHuman(beadID, anvil, "prior PR creation timeout"))

		bead := poller.Bead{ID: beadID, Anvil: anvil, Title: "Test bead"}
		d.applyNoChangesNeededOutcome(context.Background(), bead, orphanAnvilPath, "already done")

		// Retry record must be cleared — bead should leave Needs Attention.
		r, err := orphanDB.GetRetry(beadID, anvil)
		require.NoError(t, err)
		assert.Nil(t, r, "retry record should be cleared after successful auto PR creation")

		// EventPRCreated should be logged.
		events, err := orphanDB.RecentEvents(20)
		require.NoError(t, err)
		found := false
		for _, ev := range events {
			if ev.Type == state.EventPRCreated && ev.BeadID == beadID {
				found = true
				assert.Contains(t, ev.Message, "Auto-created PR")
				break
			}
		}
		assert.True(t, found, "EventPRCreated should be logged after auto PR creation")
	})

	t.Run("orphaned branch: PR creation failure escalates to needs_human", func(t *testing.T) {
		const beadID = "NCN-ORPHAN-FAIL"
		orphanAnvilPath := initTestGitRepo(t)

		orphanDB, err := state.Open(filepath.Join(orphanAnvilPath, "state-orphan-fail.db"))
		require.NoError(t, err)
		defer orphanDB.Close()

		// Create a forge branch with a commit ahead of main and push it.
		branchName := worktree.BranchName(beadID)
		gitLocal := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = orphanAnvilPath
			cmd.Env = cleanGitTestEnv()
			out, runErr := cmd.CombinedOutput()
			require.NoError(t, runErr, "git %v: %s", args, out)
		}
		gitLocal("checkout", "-b", branchName)
		require.NoError(t, os.WriteFile(filepath.Join(orphanAnvilPath, "orphan-fail.txt"), []byte("work\n"), 0o644))
		gitLocal("add", "orphan-fail.txt")
		gitLocal("commit", "-m", "orphaned work")
		gitLocal("push", "origin", branchName)
		gitLocal("checkout", "main")

		mockVCS := &mockVCSProvider{
			createPRErr: fmt.Errorf("GitHub timeout"),
		}
		d := &Daemon{
			db:           orphanDB,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			worktreeMgr:  worktree.NewManager(),
			vcsProviders: map[string]vcs.Provider{anvil: mockVCS},
		}
		d.cfg.Store(&config.Config{})

		bead := poller.Bead{ID: beadID, Anvil: anvil, Title: "Test bead"}
		d.applyNoChangesNeededOutcome(context.Background(), bead, orphanAnvilPath, "already done")

		// Bead must be marked needs_human because auto PR creation failed.
		r, err := orphanDB.GetRetry(beadID, anvil)
		require.NoError(t, err)
		require.NotNil(t, r, "retry record should exist after PR creation failure")
		assert.True(t, r.NeedsHuman, "bead should be needs_human when auto PR creation fails")
		assert.Contains(t, r.LastError, "GitHub timeout")
	})

	// Regression for Forge-oinq: when CreatePR returns ErrPRAlreadyExists on
	// the orphan-branch path, the existing PR must be registered in state.db
	// so HasOpenPRForBead returns true on the next orphan-recovery sweep.
	// Without this, the bead is reset to open and re-dispatched in a loop,
	// with Smith burning tokens to declare NO_CHANGES_NEEDED each iteration.
	t.Run("orphaned branch: ErrPRAlreadyExists registers existing PR", func(t *testing.T) {
		const beadID = "NCN-ORPHAN-DUP"
		orphanAnvilPath := initTestGitRepo(t)

		orphanDB, err := state.Open(filepath.Join(orphanAnvilPath, "state-orphan-dup.db"))
		require.NoError(t, err)
		defer orphanDB.Close()

		branchName := worktree.BranchName(beadID)
		gitLocal := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = orphanAnvilPath
			cmd.Env = cleanGitTestEnv()
			out, runErr := cmd.CombinedOutput()
			require.NoError(t, runErr, "git %v: %s", args, out)
		}
		gitLocal("checkout", "-b", branchName)
		require.NoError(t, os.WriteFile(filepath.Join(orphanAnvilPath, "orphan-dup.txt"), []byte("work\n"), 0o644))
		gitLocal("add", "orphan-dup.txt")
		gitLocal("commit", "-m", "orphaned work")
		gitLocal("push", "origin", branchName)
		gitLocal("checkout", "main")

		// Mock VCS: CreatePR fails with ErrPRAlreadyExists, GetPRByHeadBranch
		// returns the matching open PR so registerExistingPRByBranch can find it.
		mockVCS := &mockVCSProvider{
			createPRErr: fmt.Errorf("gh pr create: %w: already exists", vcs.ErrPRAlreadyExists),
			openPRs: []vcs.OpenPR{
				{Number: 255, Title: "Existing PR", Branch: branchName},
			},
		}
		d := &Daemon{
			db:           orphanDB,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			worktreeMgr:  worktree.NewManager(),
			vcsProviders: map[string]vcs.Provider{anvil: mockVCS},
		}
		d.cfg.Store(&config.Config{})

		bead := poller.Bead{ID: beadID, Anvil: anvil, Title: "Test bead"}
		d.applyNoChangesNeededOutcome(context.Background(), bead, orphanAnvilPath, "already done")

		// Retry record must be cleared — bead should not be marked needs_human
		// because the PR exists; the work is represented and bellows will
		// eventually merge/close it.
		r, err := orphanDB.GetRetry(beadID, anvil)
		require.NoError(t, err)
		assert.Nil(t, r, "retry record should be cleared when PR already exists")

		// PR must be registered in state.db so HasOpenPRForBead returns true,
		// breaking the orphan-recovery → re-dispatch loop.
		hasPR, err := orphanDB.HasOpenPRForBead(beadID, anvil)
		require.NoError(t, err)
		assert.True(t, hasPR, "existing PR must be registered to break orphan-recovery loop")

		// PR record fields should match the mocked open PR.
		dbPR, err := orphanDB.GetPRByNumber(anvil, 255)
		require.NoError(t, err)
		require.NotNil(t, dbPR, "PR #255 must be inserted into state.db")
		assert.Equal(t, beadID, dbPR.BeadID)
		assert.Equal(t, branchName, dbPR.Branch)
		assert.Equal(t, state.PROpen, dbPR.Status)
	})
}

// TestHandleIPC_WardenRerun verifies the warden_rerun IPC handler validates
// payloads, checks anvil config, and looks up the branch from the DB.
func TestHandleIPC_WardenRerun(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {Path: tmpDir},
		},
	})

	t.Run("invalid payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "warden_rerun", Payload: []byte("invalid")})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "invalid warden_rerun payload")
	})

	t.Run("missing fields", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.WardenRerunPayload{BeadID: "X"})
		resp := d.handleIPC(ipc.Command{Type: "warden_rerun", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id and anvil are required")
	})

	t.Run("unknown anvil", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.WardenRerunPayload{BeadID: "X", Anvil: "nope"})
		resp := d.handleIPC(ipc.Command{Type: "warden_rerun", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "not found")
	})

	t.Run("no branch found", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.WardenRerunPayload{BeadID: "NO-BRANCH", Anvil: "test-anvil"})
		resp := d.handleIPC(ipc.Command{Type: "warden_rerun", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "no branch found")
	})

	t.Run("success starts background goroutine", func(t *testing.T) {
		// Insert a worker record so LastWorkerBranchForBead returns a branch.
		require.NoError(t, db.InsertWorker(&state.Worker{
			ID:        "w-rerun-1",
			BeadID:    "BD-RERUN",
			Anvil:     "test-anvil",
			Branch:    "forge/BD-RERUN",
			Status:    state.WorkerFailed,
			StartedAt: time.Now(),
		}))

		payload, _ := json.Marshal(ipc.WardenRerunPayload{BeadID: "BD-RERUN", Anvil: "test-anvil"})
		resp := d.handleIPC(ipc.Command{Type: "warden_rerun", Payload: payload})
		assert.Equal(t, "ok", resp.Type)

		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Equal(t, "warden re-review started", msg["message"])

		// Wait for background goroutine to finish (it will fail because no git repo, but that's OK).
		done := make(chan struct{})
		go func() { d.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for background goroutine")
		}

		// Verify event was logged.
		events, err := db.RecentEvents(20)
		require.NoError(t, err)
		found := false
		for _, ev := range events {
			if ev.Type == state.EventWardenRerun && ev.BeadID == "BD-RERUN" {
				found = true
				break
			}
		}
		assert.True(t, found, "warden_rerun event should be logged")
	})
}

// TestHandleIPC_AssayRerun verifies the assay_rerun IPC handler validates
// payloads, checks anvil config, looks up the PR from the DB, and verifies
// anvil ownership.
func TestHandleIPC_AssayRerun(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {Path: tmpDir},
		},
	})

	t.Run("invalid payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "assay_rerun", Payload: []byte("invalid")})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "invalid assay_rerun payload")
	})

	t.Run("missing fields", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.AssayRerunPayload{Anvil: "test-anvil"})
		resp := d.handleIPC(ipc.Command{Type: "assay_rerun", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "requires anvil and pr")
	})

	t.Run("unknown anvil", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.AssayRerunPayload{Anvil: "nope", PR: 1})
		resp := d.handleIPC(ipc.Command{Type: "assay_rerun", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "unknown anvil")
	})

	t.Run("missing PR id", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.AssayRerunPayload{Anvil: "test-anvil", PR: 999})
		resp := d.handleIPC(ipc.Command{Type: "assay_rerun", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "not found")
	})

	t.Run("anvil mismatch", func(t *testing.T) {
		require.NoError(t, db.InsertPR(&state.PR{
			Number: 42, Anvil: "other-anvil", BeadID: "BD-MM",
			Branch: "b", Status: state.PROpen, CreatedAt: time.Now(),
		}))
		pr, err := db.GetPRByNumber("other-anvil", 42)
		require.NoError(t, err)

		payload, _ := json.Marshal(ipc.AssayRerunPayload{Anvil: "test-anvil", PR: pr.ID})
		resp := d.handleIPC(ipc.Command{Type: "assay_rerun", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "belongs to anvil")
	})

	t.Run("success starts background goroutine", func(t *testing.T) {
		require.NoError(t, db.InsertPR(&state.PR{
			Number: 77, Anvil: "test-anvil", BeadID: "BD-ASSAY",
			Branch: "forge/BD-ASSAY", Status: state.PROpen, CreatedAt: time.Now(),
		}))
		pr, err := db.GetPRByNumber("test-anvil", 77)
		require.NoError(t, err)

		payload, _ := json.Marshal(ipc.AssayRerunPayload{Anvil: "test-anvil", PR: pr.ID})
		resp := d.handleIPC(ipc.Command{Type: "assay_rerun", Payload: payload})
		assert.Equal(t, "ok", resp.Type)

		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "Assay re-review started")

		done := make(chan struct{})
		go func() { d.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for background goroutine")
		}

		events, err := db.RecentEvents(20)
		require.NoError(t, err)
		found := false
		for _, ev := range events {
			if ev.Type == state.EventPRReviewNeeded && ev.BeadID == "BD-ASSAY" {
				found = true
				break
			}
		}
		assert.True(t, found, "assay_rerun event should be logged")
	})
}

// TestHandleIPC_ApproveAsIs verifies the approve_as_is IPC handler validates
// payloads, checks anvil config, and dispatches a background goroutine.
func TestHandleIPC_ApproveAsIs(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {Path: tmpDir},
		},
	})

	t.Run("invalid payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "approve_as_is", Payload: []byte("invalid")})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "invalid approve_as_is payload")
	})

	t.Run("missing fields", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.ApproveAsIsPayload{BeadID: "X"})
		resp := d.handleIPC(ipc.Command{Type: "approve_as_is", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id and anvil are required")
	})

	t.Run("unknown anvil", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.ApproveAsIsPayload{BeadID: "X", Anvil: "nope"})
		resp := d.handleIPC(ipc.Command{Type: "approve_as_is", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "not found")
	})

	t.Run("no branch found", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.ApproveAsIsPayload{BeadID: "NO-BRANCH", Anvil: "test-anvil"})
		resp := d.handleIPC(ipc.Command{Type: "approve_as_is", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "no branch found")
	})

	t.Run("success starts background goroutine", func(t *testing.T) {
		require.NoError(t, db.InsertWorker(&state.Worker{
			ID:        "w-approve-1",
			BeadID:    "BD-APPROVE",
			Anvil:     "test-anvil",
			Branch:    "forge/BD-APPROVE",
			Status:    state.WorkerFailed,
			StartedAt: time.Now(),
		}))

		payload, _ := json.Marshal(ipc.ApproveAsIsPayload{BeadID: "BD-APPROVE", Anvil: "test-anvil"})
		resp := d.handleIPC(ipc.Command{Type: "approve_as_is", Payload: payload})
		assert.Equal(t, "ok", resp.Type)

		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Equal(t, "approve as-is started", msg["message"])

		// Wait for background goroutine.
		done := make(chan struct{})
		go func() { d.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for background goroutine")
		}

		// Verify event was logged.
		events, err := db.RecentEvents(20)
		require.NoError(t, err)
		found := false
		for _, ev := range events {
			if ev.Type == state.EventApproveAsIs && ev.BeadID == "BD-APPROVE" {
				found = true
				break
			}
		}
		assert.True(t, found, "approve_as_is event should be logged")
	})
}

// TestHandleIPC_ForceSmith verifies the force_smith IPC handler validates
// payloads, checks anvil config, and dispatches a background goroutine.
func TestHandleIPC_ForceSmith(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {Path: tmpDir},
		},
	})

	t.Run("invalid payload", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "force_smith", Payload: []byte("invalid")})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "invalid force_smith payload")
	})

	t.Run("missing fields", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.ForceSmithPayload{BeadID: "X"})
		resp := d.handleIPC(ipc.Command{Type: "force_smith", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "bead_id and anvil are required")
	})

	t.Run("unknown anvil", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.ForceSmithPayload{BeadID: "X", Anvil: "nope"})
		resp := d.handleIPC(ipc.Command{Type: "force_smith", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "not found")
	})

	t.Run("no branch found", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.ForceSmithPayload{BeadID: "NO-BRANCH", Anvil: "test-anvil"})
		resp := d.handleIPC(ipc.Command{Type: "force_smith", Payload: payload})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "no branch found")
	})

	t.Run("success with user note starts background goroutine", func(t *testing.T) {
		require.NoError(t, db.InsertWorker(&state.Worker{
			ID:        "w-force-1",
			BeadID:    "BD-FORCE",
			Anvil:     "test-anvil",
			Branch:    "forge/BD-FORCE",
			Status:    state.WorkerFailed,
			StartedAt: time.Now(),
		}))

		payload, _ := json.Marshal(ipc.ForceSmithPayload{
			BeadID:   "BD-FORCE",
			Anvil:    "test-anvil",
			UserNote: "these issues are real, please actually fix them",
		})
		resp := d.handleIPC(ipc.Command{Type: "force_smith", Payload: payload})
		assert.Equal(t, "ok", resp.Type)

		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Equal(t, "force smith started", msg["message"])

		// Wait for background goroutine.
		done := make(chan struct{})
		go func() { d.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for background goroutine")
		}

		// Verify event was logged.
		events, err := db.RecentEvents(20)
		require.NoError(t, err)
		found := false
		for _, ev := range events {
			if ev.Type == state.EventForceSmith && ev.BeadID == "BD-FORCE" {
				found = true
				break
			}
		}
		assert.True(t, found, "force_smith event should be logged")
	})
}

// TestReconcileMergedBeads verifies the startup catch-up behaviour:
//   - ext-* PRs are skipped (filtered in SQL, so they never arrive)
//   - PRs whose anvil is not in the config are skipped without error
//   - A valid merged PR causes a bd close call
// TestReconcileOpenPRs_RequiresPerInstanceForgeManagedMarker is a regression
// test for Forge-m1ui (#578) extended for Forge-i1g7. The marker must now
// carry the forge instance id (`<!-- forge-managed: <id> -->`) so that:
//
//  1. Contributor PRs that merely reference a bead are still tracked as
//     external (the original m1ui fix).
//  2. PRs authored by another Forge instance pointing at the same anvil are
//     also tracked as external — this is the Forge-i1g7 fix that closes the
//     multi-forge race where every instance saw the same generic marker and
//     adopted each others' PRs.
//  3. Legacy generic-marker PRs (`<!-- forge-managed: true -->`) are not
//     adopted by any instance after the upgrade — in a multi-forge deployment
//     we cannot tell which instance authored them.
func TestReconcileOpenPRs_RequiresPerInstanceForgeManagedMarker(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-reconcile-open-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	const myID = "local-forge"
	const siblingID = "skybert-forge"

	prevID := vcs.ForgeID()
	vcs.SetForgeID(myID)
	defer vcs.SetForgeID(prevID)

	mock := &mockVCSProvider{
		openPRs: []vcs.OpenPR{
			// 3030: contributor's manual PR — references a bead, no marker.
			// Mirrors PR #3030 in the original m1ui bug report.
			{
				Number: 3030,
				Title:  "Manual fix for the metadata loader",
				Branch: "sophie/manual-fix",
				Body:   "Fixes the loader.\n\n**Bead**: Fhi.Metadata-tpc00\n",
			},
			// 3038: PR authored by a SIBLING Forge instance — carries the
			// sibling's per-instance marker. This is the live case from the
			// Forge-i1g7 bead: skybert-forge must NOT adopt PR #3038 created
			// by local-forge. (Numbering matches the bead's 2026-05-08 trace.)
			{
				Number: 3038,
				Title:  "forge: Fhi.Metadata-g1a58",
				Branch: "forge/Fhi.Metadata-g1a58",
				Body: "## Changes\n\nDid the work.\n\n---\nBead: Fhi.Metadata-g1a58 | Branch: forge/g1a58\n" +
					"Generated by The Forge\n" + vcs.MarkerForID(siblingID),
			},
			// 4040: a PR THIS instance created — carries our per-instance
			// marker. Must be adopted as bellows-managed.
			{
				Number: 4040,
				Title:  "forge: Forge-real",
				Branch: "forge/Forge-real",
				Body:   "## Changes\n\nDid the work.\n\n---\nBead: Forge-real | Branch: forge/Forge-real\n" + vcs.MarkerForID(myID),
			},
			// 5050: purely external PR with no bead reference at all.
			{
				Number: 5050,
				Title:  "Random external work",
				Branch: "feature/random",
				Body:   "Just some changes.",
			},
			// 6060: a legacy pre-i1g7 PR carrying the generic marker. After
			// the upgrade no instance can prove ownership, so it must NOT be
			// adopted as bellows-managed.
			{
				Number: 6060,
				Title:  "forge: Forge-legacy",
				Branch: "forge/Forge-legacy",
				Body:   "## Changes\n\nLegacy work.\n\n---\nBead: Forge-legacy | Branch: forge/Forge-legacy\n<!-- forge-managed: true -->",
			},
		},
	}

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		vcsProviders: map[string]vcs.Provider{
			"test-anvil": mock,
		},
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {Path: tmpDir},
		},
		Settings: config.SettingsConfig{ForgeID: myID},
	})

	d.reconcileOpenPRs(context.Background())

	sophie, err := db.GetPRByNumber("test-anvil", 3030)
	require.NoError(t, err)
	require.NotNil(t, sophie, "Sophie's manual PR should be tracked")
	assert.Equal(t, "ext-3030", sophie.BeadID,
		"PR with only a **Bead**: reference must be tracked as external")
	assert.False(t, sophie.BellowsManaged,
		"PR without any forge-managed marker must NOT be bellows-managed")

	sibling, err := db.GetPRByNumber("test-anvil", 3038)
	require.NoError(t, err)
	require.NotNil(t, sibling, "sibling-forge's PR should still be tracked (so Hearth can display it)")
	assert.Equal(t, "ext-3038", sibling.BeadID,
		"PR authored by a sibling forge instance must be tracked as external — adopting it re-creates the multi-forge race")
	assert.False(t, sibling.BellowsManaged,
		"PR with another instance's forge-managed marker must NOT be bellows-managed (this is the Forge-i1g7 fix)")

	mine, err := db.GetPRByNumber("test-anvil", 4040)
	require.NoError(t, err)
	require.NotNil(t, mine, "this instance's own PR should be tracked")
	assert.Equal(t, "Forge-real", mine.BeadID,
		"PR with our per-instance marker should be tracked under its bead ID")
	assert.True(t, mine.BellowsManaged,
		"PR with our per-instance forge-managed marker must be bellows-managed")

	random, err := db.GetPRByNumber("test-anvil", 5050)
	require.NoError(t, err)
	require.NotNil(t, random, "external PR with no bead reference should still be tracked")
	assert.Equal(t, "ext-5050", random.BeadID)
	assert.False(t, random.BellowsManaged,
		"PR with no bead reference must not be bellows-managed")

	legacy, err := db.GetPRByNumber("test-anvil", 6060)
	require.NoError(t, err)
	require.NotNil(t, legacy, "legacy generic-marker PR should still be tracked")
	assert.Equal(t, "ext-6060", legacy.BeadID,
		"legacy generic-marker PRs must NOT be adopted under their bead ID after the upgrade — ownership is ambiguous")
	assert.False(t, legacy.BellowsManaged,
		"legacy generic-marker PRs must NOT be bellows-managed in a multi-forge deployment")
}

// TestReconcileOpenPRs_RecoversBeadIDFromForgeBranch is the regression test for
// Forge-wor5. When this forge's own PR carries our forge-managed marker but its
// body has no parseable "Bead:" reference (e.g. an auto-opened stranded-branch
// recovery PR, or a body edited after creation), reconcile must recover the real
// bead ID from the canonical forge/<bead-id> branch name instead of storing the
// synthetic ext-<number> placeholder. Without the real bead_id, merge-close
// (handleBeadCloseOnMerge) never fires and the bead is left open after the PR
// merges. Branch recovery is gated on OUR marker so the multi-forge safety from
// Forge-i1g7 is preserved (a sibling forge's forge/<id> PR still stays external).
func TestReconcileOpenPRs_RecoversBeadIDFromForgeBranch(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-reconcile-branch-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	const myID = "local-forge"
	const siblingID = "skybert-forge"

	prevID := vcs.ForgeID()
	vcs.SetForgeID(myID)
	defer vcs.SetForgeID(prevID)

	mock := &mockVCSProvider{
		openPRs: []vcs.OpenPR{
			// 7070: our own recovery PR — carries OUR marker but the body has no
			// "Bead:" footer. The bead ID must be recovered from the forge branch.
			{
				Number: 7070,
				Title:  "forge: recovered stranded branch",
				Branch: "forge/Forge-stranded",
				Body:   "## Changes\n\nAuto-opened for a stranded branch.\n\n" + vcs.MarkerForID(myID),
			},
			// 7071: our marker but a NON-forge branch and no body ref — there is no
			// bead ID to recover, so it must keep the ext-<number> placeholder.
			{
				Number: 7071,
				Title:  "forge: odd branch",
				Branch: "release/cut-1.2",
				Body:   "## Changes\n\nNo bead ref here.\n\n" + vcs.MarkerForID(myID),
			},
			// 7072: a SIBLING forge's PR on a forge/<id> branch with no body ref.
			// Branch recovery is gated on OUR marker, so this must stay external
			// (preserving the Forge-i1g7 multi-forge safety guarantee).
			{
				Number: 7072,
				Title:  "forge: sibling branch",
				Branch: "forge/Forge-sibling",
				Body:   "## Changes\n\nSibling work.\n\n" + vcs.MarkerForID(siblingID),
			},
		},
	}

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		vcsProviders: map[string]vcs.Provider{
			"test-anvil": mock,
		},
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"test-anvil": {Path: tmpDir},
		},
		Settings: config.SettingsConfig{ForgeID: myID},
	})

	d.reconcileOpenPRs(context.Background())

	recovered, err := db.GetPRByNumber("test-anvil", 7070)
	require.NoError(t, err)
	require.NotNil(t, recovered)
	assert.Equal(t, "Forge-stranded", recovered.BeadID,
		"forge-managed PR with no body bead ref must recover the bead ID from its forge/<id> branch")
	assert.True(t, recovered.BellowsManaged,
		"a PR carrying our forge-managed marker must be bellows-managed")

	odd, err := db.GetPRByNumber("test-anvil", 7071)
	require.NoError(t, err)
	require.NotNil(t, odd)
	assert.Equal(t, "ext-7071", odd.BeadID,
		"forge-managed PR on a non-forge branch with no bead ref must keep the ext-<number> placeholder")
	assert.False(t, odd.BellowsManaged,
		"a forge-managed-but-unparseable PR (synthetic ext-* id) must not be bellows-managed")

	sibling, err := db.GetPRByNumber("test-anvil", 7072)
	require.NoError(t, err)
	require.NotNil(t, sibling)
	assert.Equal(t, "ext-7072", sibling.BeadID,
		"a sibling forge's forge/<id> PR must stay external — branch recovery is gated on our own marker")
	assert.False(t, sibling.BellowsManaged,
		"a sibling forge's PR must not be bellows-managed (Forge-i1g7 multi-forge safety)")
}

// TestReconcileOpenPRs_ReevaluatesAlreadyTrackedPRs is the regression test
// for the second half of Forge-i1g7. Before the fix, reconcileOpenPRs
// short-circuited with "continue // already tracked", so PRs adopted under
// the m1ui-era logic kept bellows_managed=true forever even after the
// per-instance marker rolled out — operators had to UPDATE state.db by hand
// or reset the pod from scratch. Now reconcile re-checks the body on every
// pass and flips bellows_managed off when the current instance no longer
// owns the PR (and on, conversely, when the marker arrives later).
func TestReconcileOpenPRs_ReevaluatesAlreadyTrackedPRs(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-reconcile-reeval-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	const myID = "local-forge"
	const siblingID = "skybert-forge"

	prevID := vcs.ForgeID()
	vcs.SetForgeID(myID)
	defer vcs.SetForgeID(prevID)

	now := time.Now()

	// 100: tracked under its bead with bellows_managed=true (the legacy state),
	//      but the current PR body carries a SIBLING's marker. Reconcile must
	//      flip bellows_managed off so the sibling can manage it.
	require.NoError(t, db.InsertPR(&state.PR{
		Number: 100, Anvil: "test-anvil", BeadID: "Fhi.Metadata-g1a58",
		Branch: "forge/g1a58", Status: state.PROpen, CreatedAt: now,
	}))
	row100, _ := db.GetPRByNumber("test-anvil", 100)
	require.NoError(t, db.UpdatePRBellowsManaged(row100.ID, true))

	// 200: tracked with bellows_managed=true and STILL ours (body carries our
	//      marker). Reconcile must leave bellows_managed=true.
	require.NoError(t, db.InsertPR(&state.PR{
		Number: 200, Anvil: "test-anvil", BeadID: "Forge-mine",
		Branch: "forge/Forge-mine", Status: state.PROpen, CreatedAt: now,
	}))
	row200, _ := db.GetPRByNumber("test-anvil", 200)
	require.NoError(t, db.UpdatePRBellowsManaged(row200.ID, true))

	// 300: tracked under a synthetic ext-* id with bellows_managed=true (a
	//      defensive bug from earlier code paths). Reconcile must clear it
	//      regardless of marker state — ext-* is never bellows-managed.
	require.NoError(t, db.InsertPR(&state.PR{
		Number: 300, Anvil: "test-anvil", BeadID: "ext-300",
		Branch: "feature/legacy-ext", Status: state.PROpen, CreatedAt: now,
	}))
	row300, _ := db.GetPRByNumber("test-anvil", 300)
	require.NoError(t, db.UpdatePRBellowsManaged(row300.ID, true))

	// 400: tracked under a real bead but bellows_managed=false (e.g. demoted
	//      by an earlier reconcile). The PR has since been re-pushed with our
	//      marker; reconcile must promote it back to bellows-managed.
	require.NoError(t, db.InsertPR(&state.PR{
		Number: 400, Anvil: "test-anvil", BeadID: "Forge-promote",
		Branch: "forge/Forge-promote", Status: state.PROpen, CreatedAt: now,
	}))
	row400, _ := db.GetPRByNumber("test-anvil", 400)
	require.NoError(t, db.UpdatePRBellowsManaged(row400.ID, false))

	// 500: tracked under a real bead with bellows_managed=true and the
	//      current PR body carries the LEGACY generic marker. Multi-forge
	//      ambiguity: must be released to bellows_managed=false.
	require.NoError(t, db.InsertPR(&state.PR{
		Number: 500, Anvil: "test-anvil", BeadID: "Forge-legacy",
		Branch: "forge/Forge-legacy", Status: state.PROpen, CreatedAt: now,
	}))
	row500, _ := db.GetPRByNumber("test-anvil", 500)
	require.NoError(t, db.UpdatePRBellowsManaged(row500.ID, true))

	// 600: synthetic ext-* PR that the user explicitly assigned to bellows via
	//      the assign_bellows IPC action. Both bellows_managed=1 and
	//      bellows_manually_assigned=1 are set. Reconcile must leave it alone
	//      (Forge-l125): the previous overly-broad ext-* clobber was reverting
	//      manual assignments within one poll cycle.
	require.NoError(t, db.InsertPR(&state.PR{
		Number: 600, Anvil: "test-anvil", BeadID: "ext-600",
		Branch: "feature/user-assigned", Status: state.PROpen, CreatedAt: now,
	}))
	row600, _ := db.GetPRByNumber("test-anvil", 600)
	require.NoError(t, db.UpdatePRBellowsAssignment(row600.ID, true, true))

	mock := &mockVCSProvider{
		openPRs: []vcs.OpenPR{
			{Number: 100, Branch: "forge/g1a58", Body: "Bead: Fhi.Metadata-g1a58\n" + vcs.MarkerForID(siblingID)},
			{Number: 200, Branch: "forge/Forge-mine", Body: "Bead: Forge-mine\n" + vcs.MarkerForID(myID)},
			{Number: 300, Branch: "feature/legacy-ext", Body: "Some external PR with our marker by accident\n" + vcs.MarkerForID(myID)},
			{Number: 400, Branch: "forge/Forge-promote", Body: "Bead: Forge-promote\n" + vcs.MarkerForID(myID)},
			{Number: 500, Branch: "forge/Forge-legacy", Body: "Bead: Forge-legacy\n<!-- forge-managed: true -->"},
			{Number: 600, Branch: "feature/user-assigned", Body: "external PR body, no markers"},
		},
	}

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		vcsProviders: map[string]vcs.Provider{
			"test-anvil": mock,
		},
	}
	d.cfg.Store(&config.Config{
		Anvils:   map[string]config.AnvilConfig{"test-anvil": {Path: tmpDir}},
		Settings: config.SettingsConfig{ForgeID: myID},
	})

	d.reconcileOpenPRs(context.Background())

	got100, err := db.GetPRByNumber("test-anvil", 100)
	require.NoError(t, err)
	assert.False(t, got100.BellowsManaged,
		"PR carrying a sibling instance's marker must have bellows_managed flipped off without a manual state.db edit (Forge-i1g7 core fix)")

	got200, err := db.GetPRByNumber("test-anvil", 200)
	require.NoError(t, err)
	assert.True(t, got200.BellowsManaged,
		"PR carrying our own marker must keep bellows_managed=true across reconciles")

	got300, err := db.GetPRByNumber("test-anvil", 300)
	require.NoError(t, err)
	assert.False(t, got300.BellowsManaged,
		"legacy auto-adopted ext-* PRs (no manual-assignment flag) must be released by reconcile — defensive correction")

	got400, err := db.GetPRByNumber("test-anvil", 400)
	require.NoError(t, err)
	assert.True(t, got400.BellowsManaged,
		"a previously-external PR that gains our marker must be promoted back to bellows-managed on the next reconcile")

	got500, err := db.GetPRByNumber("test-anvil", 500)
	require.NoError(t, err)
	assert.False(t, got500.BellowsManaged,
		"PR carrying only the legacy generic marker is ambiguous in multi-forge deployments and must be released")

	got600, err := db.GetPRByNumber("test-anvil", 600)
	require.NoError(t, err)
	assert.True(t, got600.BellowsManaged,
		"ext-* PRs that the user manually assigned via assign_bellows must NOT be clobbered by reconcile (Forge-l125)")
	assert.True(t, got600.BellowsManuallyAssigned,
		"manual-assignment marker must persist across reconcile cycles")
}

func TestReconcileMergedBeads(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-reconcile-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create a fake bd script that records close calls via a marker file.
	markerFile := filepath.Join(tmpDir, "bd-reconcile-close.txt")
	var bdScript string
	var bdContent string
	if runtime.GOOS == "windows" {
		bdScript = filepath.Join(tmpDir, "bd.bat")
		bdContent = "@echo off\r\nif \"%1\"==\"close\" (\r\n    echo %2 >> \"" + markerFile + "\"\r\n    echo {\"id\": \"%2\", \"status\": \"closed\"}\r\n    exit /b 0\r\n)\r\nexit /b 0\r\n"
	} else {
		bdScript = filepath.Join(tmpDir, "bd")
		bdContent = "#!/bin/sh\nif [ \"$1\" = \"close\" ]; then\n    echo \"$2\" >> '" + markerFile + "'\n    echo '{\"id\": \"'\"$2\"'\", \"status\": \"closed\"}'\n    exit 0\nfi\nexit 0\n"
	}
	require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
	defer os.Setenv("PATH", oldPath)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	// Insert a variety of PRs:
	//   PR 1: merged with a valid bead in the configured anvil — should be closed.
	//   PR 2: merged but anvil not in config — should be skipped.
	//   PR 3: ext-* bead — filtered out at the SQL level, won't reach reconcile.
	//   PR 4: merged with empty bead_id — filtered out at the SQL level.
	now := time.Now()
	prs := []state.PR{
		{Number: 1, Anvil: "known-anvil", BeadID: "REC-1", Branch: "forge/REC-1", Status: state.PRMerged, CreatedAt: now},
		{Number: 2, Anvil: "unknown-anvil", BeadID: "REC-2", Branch: "forge/REC-2", Status: state.PRMerged, CreatedAt: now},
		{Number: 3, Anvil: "known-anvil", BeadID: "ext-abc", Branch: "some-branch", Status: state.PRMerged, CreatedAt: now},
		{Number: 4, Anvil: "known-anvil", BeadID: "", Branch: "other-branch", Status: state.PRMerged, CreatedAt: now},
	}
	for i := range prs {
		require.NoError(t, db.InsertPR(&prs[i]))
	}

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"known-anvil": {Path: tmpDir},
		},
	})

	d.reconcileMergedBeads(context.Background())

	// Only REC-1 should have been passed to bd close.
	content, err := os.ReadFile(markerFile)
	require.NoError(t, err, "bd close should have been called (marker file should exist)")
	lines := strings.Fields(strings.TrimSpace(string(content)))
	require.Len(t, lines, 1, "expected exactly one bd close call")
	assert.Equal(t, "REC-1", lines[0], "bd close should have been called with REC-1")
}

// cleanGitTestEnv returns os.Environ with git worktree vars stripped. Used by
// test git commands so they operate on the test repo (via cmd.Dir) rather than
// the outer Forge worker process's repo (set via GIT_DIR / GIT_WORK_TREE).
func cleanGitTestEnv() []string {
	skip := map[string]bool{"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_CEILING_DIRECTORIES": true}
	var out []string
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if !skip[k] {
			out = append(out, e)
		}
	}
	return out
}

// initTestGitRepo sets up a bare "remote" repo and a local clone that serves
// as the anvilPath for forgeBranchAheadOfMain tests. It returns the local path.
func initTestGitRepo(t *testing.T) (anvilPath string) {
	t.Helper()
	base, err := os.MkdirTemp("", "forge-git-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(base) })

	remotePath := filepath.Join(base, "remote")
	anvilPath = filepath.Join(base, "local")

	gitSetup := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		// Strip git worktree env vars so setup commands run against the test
		// repo rather than inheriting the outer Forge worker process's context.
		cmd.Env = cleanGitTestEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}

	// Bare remote.
	require.NoError(t, os.MkdirAll(remotePath, 0o755))
	gitSetup(remotePath, "init", "--bare")

	// Local clone.
	gitSetup(base, "clone", remotePath, "local")
	gitSetup(anvilPath, "config", "user.email", "test@forge.test")
	gitSetup(anvilPath, "config", "user.name", "Forge Test")

	// Ensure the initial branch is explicitly named "main" regardless of Git defaults.
	gitSetup(anvilPath, "branch", "-M", "main")

	// Seed main with an initial commit so origin/main exists.
	require.NoError(t, os.WriteFile(filepath.Join(anvilPath, "README.md"), []byte("init\n"), 0o644))
	gitSetup(anvilPath, "add", "README.md")
	gitSetup(anvilPath, "commit", "-m", "initial commit")
	// Push the local main branch to origin and set upstream tracking.
	gitSetup(anvilPath, "push", "-u", "origin", "main")
	// Fetch from origin to ensure the local remote-tracking ref origin/main is present.
	gitSetup(anvilPath, "fetch", "origin", "main")

	return anvilPath
}

// TestForgeBranchAheadOfMain verifies the three key scenarios for the
// un-PR'd branch detection logic.
func TestForgeBranchAheadOfMain(t *testing.T) {
	anvilPath := initTestGitRepo(t)

	dbPath := filepath.Join(anvilPath, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:          db,
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr: worktree.NewManager(),
	}

	beadID := "TEST-jvpg"
	branchName := worktree.BranchName(beadID) // "forge/TEST-jvpg"

	gitLocal := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = anvilPath
		cmd.Env = cleanGitTestEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}

	t.Run("branch absent on origin", func(t *testing.T) {
		branch, ok := d.forgeBranchAheadOfMain(context.Background(), anvilPath, beadID, "")
		assert.False(t, ok)
		assert.Empty(t, branch)
	})

	t.Run("branch present but no commits ahead of main", func(t *testing.T) {
		// Push the branch at the same tip as main — no ahead commits.
		gitLocal("checkout", "-b", branchName)
		gitLocal("push", "origin", branchName)
		gitLocal("checkout", "main")

		branch, ok := d.forgeBranchAheadOfMain(context.Background(), anvilPath, beadID, "")
		assert.False(t, ok, "branch at same tip as main should not be detected as ahead")
		assert.Empty(t, branch)

		// Clean up for next sub-test.
		gitLocal("branch", "-D", branchName)
		gitLocal("push", "origin", "--delete", branchName)
	})

	t.Run("branch present with commits ahead of main", func(t *testing.T) {
		// Create a forge branch with a commit ahead of main and push it.
		gitLocal("checkout", "-b", branchName)
		require.NoError(t, os.WriteFile(filepath.Join(anvilPath, "work.txt"), []byte("change\n"), 0o644))
		gitLocal("add", "work.txt")
		gitLocal("commit", "-m", "smith work")
		gitLocal("push", "origin", branchName)
		gitLocal("checkout", "main")

		branch, ok := d.forgeBranchAheadOfMain(context.Background(), anvilPath, beadID, "")
		assert.True(t, ok, "branch with commits ahead of main should be detected")
		assert.Equal(t, branchName, branch)
	})
}

// TestPreDispatchRemoteBranchCheck verifies the dispatch-time guard that
// detects stranded origin/forge/<bead-id> branches from a prior worker. The
// three transition outcomes the daemon must implement:
//   - Absent → proceed (returns true, no state changes)
//   - Merged → delete stale branch, proceed (returns true, branch gone on origin)
//   - Stranded with no PR → mark needs_human and abort (returns false)
//   - Stranded with PR → log, register PR, abort (returns false; no needs_human)
func TestPreDispatchRemoteBranchCheck(t *testing.T) {
	const anvilName = "test-anvil"

	gitLocal := func(t *testing.T, anvilPath string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = anvilPath
		cmd.Env = cleanGitTestEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}

	t.Run("absent branch: dispatch proceeds", func(t *testing.T) {
		anvilPath := initTestGitRepo(t)
		db, err := state.Open(filepath.Join(anvilPath, "state.db"))
		require.NoError(t, err)
		defer db.Close()

		d := &Daemon{
			db:           db,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			worktreeMgr:  worktree.NewManager(),
			vcsProviders: map[string]vcs.Provider{anvilName: &mockVCSProvider{}},
		}
		d.cfg.Store(&config.Config{})

		bead := poller.Bead{ID: "PRD-ABSENT", Anvil: anvilName, Title: "absent"}
		if !d.preDispatchRemoteBranchCheck(context.Background(), bead, anvilPath) {
			t.Fatal("expected dispatch to proceed when branch is absent")
		}

		r, err := db.GetRetry(bead.ID, bead.Anvil)
		require.NoError(t, err)
		if r != nil && r.NeedsHuman {
			t.Errorf("bead should not be marked needs_human when branch is absent")
		}
	})

	t.Run("merged branch: dispatch proceeds and stale branch is deleted", func(t *testing.T) {
		anvilPath := initTestGitRepo(t)
		db, err := state.Open(filepath.Join(anvilPath, "state.db"))
		require.NoError(t, err)
		defer db.Close()

		d := &Daemon{
			db:           db,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			worktreeMgr:  worktree.NewManager(),
			vcsProviders: map[string]vcs.Provider{anvilName: &mockVCSProvider{}},
		}
		d.cfg.Store(&config.Config{})

		bead := poller.Bead{ID: "PRD-MERGED", Anvil: anvilName, Title: "merged"}
		branch := worktree.BranchName(bead.ID)
		// Push a branch pointing at the same commit as main → fully merged.
		gitLocal(t, anvilPath, "checkout", "-b", branch)
		gitLocal(t, anvilPath, "push", "origin", branch)
		gitLocal(t, anvilPath, "checkout", "main")

		if !d.preDispatchRemoteBranchCheck(context.Background(), bead, anvilPath) {
			t.Fatal("expected dispatch to proceed for merged stale branch")
		}

		// Branch must be deleted from origin.
		lsCmd := exec.Command("git", "ls-remote", "--heads", "origin", "--", branch)
		lsCmd.Dir = anvilPath
		lsCmd.Env = cleanGitTestEnv()
		out, err := lsCmd.Output()
		require.NoError(t, err)
		if strings.TrimSpace(string(out)) != "" {
			t.Errorf("expected stale merged branch %s to be deleted from origin; got %q", branch, string(out))
		}
	})

	t.Run("stranded branch without PR: dispatch blocked and marked needs_human", func(t *testing.T) {
		anvilPath := initTestGitRepo(t)
		db, err := state.Open(filepath.Join(anvilPath, "state.db"))
		require.NoError(t, err)
		defer db.Close()

		d := &Daemon{
			db:           db,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			worktreeMgr:  worktree.NewManager(),
			vcsProviders: map[string]vcs.Provider{anvilName: &mockVCSProvider{}},
		}
		d.cfg.Store(&config.Config{})

		bead := poller.Bead{ID: "PRD-STRANDED", Anvil: anvilName, Title: "stranded"}
		branch := worktree.BranchName(bead.ID)
		// Push a branch with a commit not reachable from main → stranded.
		gitLocal(t, anvilPath, "checkout", "-b", branch)
		require.NoError(t, os.WriteFile(filepath.Join(anvilPath, "stranded.txt"), []byte("prior\n"), 0o644))
		gitLocal(t, anvilPath, "add", "stranded.txt")
		gitLocal(t, anvilPath, "commit", "-m", "stranded prior work")
		gitLocal(t, anvilPath, "push", "origin", branch)
		gitLocal(t, anvilPath, "checkout", "main")

		if d.preDispatchRemoteBranchCheck(context.Background(), bead, anvilPath) {
			t.Fatal("expected dispatch to be blocked for stranded branch")
		}

		r, err := db.GetRetry(bead.ID, bead.Anvil)
		require.NoError(t, err)
		require.NotNil(t, r, "retry row should be created for stranded bead")
		if !r.NeedsHuman {
			t.Errorf("bead should be marked needs_human when branch is stranded")
		}
		if !strings.Contains(r.LastError, "prior worker") {
			t.Errorf("needs_human reason should mention prior worker; got %q", r.LastError)
		}

		// An EventDispatchBlockedStrandedBranch event should be logged.
		events, err := db.RecentEvents(20)
		require.NoError(t, err)
		var found bool
		for _, ev := range events {
			if ev.Type == state.EventDispatchBlockedStrandedBranch && ev.BeadID == bead.ID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected EventDispatchBlockedStrandedBranch to be logged for %s", bead.ID)
		}
	})

	t.Run("stranded branch with existing PR: dispatch blocked, no needs_human", func(t *testing.T) {
		anvilPath := initTestGitRepo(t)
		db, err := state.Open(filepath.Join(anvilPath, "state.db"))
		require.NoError(t, err)
		defer db.Close()

		bead := poller.Bead{ID: "PRD-STRANDED-PR", Anvil: anvilName, Title: "stranded with PR"}
		branch := worktree.BranchName(bead.ID)
		mockVCS := &mockVCSProvider{
			openPRs: []vcs.OpenPR{{Number: 77, Branch: branch}},
		}
		d := &Daemon{
			db:           db,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			worktreeMgr:  worktree.NewManager(),
			vcsProviders: map[string]vcs.Provider{anvilName: mockVCS},
		}
		d.cfg.Store(&config.Config{})

		// Push a stranded branch.
		gitLocal(t, anvilPath, "checkout", "-b", branch)
		require.NoError(t, os.WriteFile(filepath.Join(anvilPath, "with-pr.txt"), []byte("prior\n"), 0o644))
		gitLocal(t, anvilPath, "add", "with-pr.txt")
		gitLocal(t, anvilPath, "commit", "-m", "stranded prior work with PR")
		gitLocal(t, anvilPath, "push", "origin", branch)
		gitLocal(t, anvilPath, "checkout", "main")

		if d.preDispatchRemoteBranchCheck(context.Background(), bead, anvilPath) {
			t.Fatal("expected dispatch to be blocked when PR exists for stranded branch")
		}

		r, err := db.GetRetry(bead.ID, bead.Anvil)
		require.NoError(t, err)
		if r != nil && r.NeedsHuman {
			t.Errorf("bead must NOT be marked needs_human when a PR already exists; bellows takes over")
		}

		// The existing PR must be left untouched: no auto-open is attempted and
		// no recovery is performed. The branch is handed straight to bellows.
		require.Equal(t, int32(0), mockVCS.createPRCalls.Load(),
			"CreatePR must NOT be called when a PR already exists for the stranded branch")

		// The pre-existing PR is registered (so bellows owns it) under the real
		// bead ID derived from the forge/<bead> branch — never a synthetic ext-* id.
		pr, err := db.GetPRByNumber(bead.Anvil, 77)
		require.NoError(t, err)
		require.NotNil(t, pr, "the already-open PR should be registered for bellows")
		assert.Equal(t, bead.ID, pr.BeadID,
			"registered PR must carry the real bead ID, not a synthetic ext-* placeholder")

		// No recovery event must fire — recovery only applies when there is no PR.
		events, err := db.RecentEvents(20)
		require.NoError(t, err)
		for _, ev := range events {
			if ev.Type == state.EventDispatchRecoveredStrandedBranch && ev.BeadID == bead.ID {
				t.Errorf("EventDispatchRecoveredStrandedBranch must NOT be logged when a PR already exists")
			}
		}
	})

	// pushStrandedWithFragment creates a stranded forge branch carrying a
	// changelog fragment for the bead (the completion signal), pushes it to
	// origin, and returns to main.
	pushStrandedWithFragment := func(t *testing.T, anvilPath, branch, beadID string) {
		t.Helper()
		gitLocal(t, anvilPath, "checkout", "-b", branch)
		require.NoError(t, os.MkdirAll(filepath.Join(anvilPath, "changelog.d"), 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(anvilPath, "changelog.d", beadID+".md"),
			[]byte("category: Fixed\n- **Done** - completion signal. ("+beadID+")\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(anvilPath, "impl.txt"), []byte("prior\n"), 0o644))
		gitLocal(t, anvilPath, "add", "changelog.d", "impl.txt")
		gitLocal(t, anvilPath, "commit", "-m", "stranded prior work with fragment")
		gitLocal(t, anvilPath, "push", "origin", branch)
		gitLocal(t, anvilPath, "checkout", "main")
	}

	t.Run("stranded branch with completion signal: auto-opens PR, no needs_human", func(t *testing.T) {
		anvilPath := initTestGitRepo(t)
		db, err := state.Open(filepath.Join(anvilPath, "state.db"))
		require.NoError(t, err)
		defer db.Close()

		bead := poller.Bead{ID: "PRD-RECOVER", Anvil: anvilName, Title: "recover"}
		branch := worktree.BranchName(bead.ID)
		mockVCS := &mockVCSProvider{
			createPRResult: &vcs.PR{Number: 91, URL: "https://example.com/pr/91", Title: bead.Title},
		}
		d := &Daemon{
			db:           db,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			worktreeMgr:  worktree.NewManager(),
			vcsProviders: map[string]vcs.Provider{anvilName: mockVCS},
		}
		d.cfg.Store(&config.Config{})

		pushStrandedWithFragment(t, anvilPath, branch, bead.ID)

		if d.preDispatchRemoteBranchCheck(context.Background(), bead, anvilPath) {
			t.Fatal("expected dispatch to be skipped after auto-opening PR")
		}

		require.Equal(t, int32(1), mockVCS.createPRCalls.Load(), "CreatePR should be called exactly once")

		r, err := db.GetRetry(bead.ID, bead.Anvil)
		require.NoError(t, err)
		if r != nil && r.NeedsHuman {
			t.Errorf("bead must NOT be marked needs_human when the PR is auto-recovered")
		}

		// The PR should be registered so bellows owns it.
		pr, err := db.GetPRByNumber(bead.Anvil, 91)
		require.NoError(t, err)
		require.NotNil(t, pr, "auto-opened PR should be registered in state.db")
		// Registration must use the real bead ID derived from the forge/<bead>
		// branch — not a synthetic ext-<number> placeholder — so merge-close
		// (handleBeadCloseOnMerge) can key off it and close the bead on merge.
		assert.Equal(t, bead.ID, pr.BeadID,
			"recovered PR must be registered under the real bead ID, not ext-<number>")
		assert.Equal(t, branch, pr.Branch,
			"recovered PR must record the forge/<bead> branch it was opened from")
		// "forge-managed": a recovery PR registered under a real bead ID must be
		// bellows-managed so bellows drives it through CI/review/merge.
		assert.True(t, pr.BellowsManaged,
			"auto-recovered PR must be bellows-managed (forge owns its lifecycle)")

		events, err := db.RecentEvents(20)
		require.NoError(t, err)
		var recovered bool
		for _, ev := range events {
			if ev.Type == state.EventDispatchRecoveredStrandedBranch && ev.BeadID == bead.ID {
				recovered = true
				break
			}
		}
		require.True(t, recovered, "expected EventDispatchRecoveredStrandedBranch to be logged")
	})

	t.Run("stranded branch with completion signal but PR appears concurrently: no duplicate CreatePR", func(t *testing.T) {
		anvilPath := initTestGitRepo(t)
		db, err := state.Open(filepath.Join(anvilPath, "state.db"))
		require.NoError(t, err)
		defer db.Close()

		bead := poller.Bead{ID: "PRD-RACE", Anvil: anvilName, Title: "race"}
		branch := worktree.BranchName(bead.ID)
		// First lookup (the stranded guard) returns nil; the second lookup (the
		// fresh re-check inside recovery) finds a PR opened concurrently.
		mockVCS := &mockVCSProvider{
			getPRByHeadBranchFunc: func(b string, call int) (*vcs.OpenPR, error) {
				if call >= 2 {
					return &vcs.OpenPR{Number: 55, Branch: b}, nil
				}
				return nil, nil
			},
		}
		d := &Daemon{
			db:           db,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			worktreeMgr:  worktree.NewManager(),
			vcsProviders: map[string]vcs.Provider{anvilName: mockVCS},
		}
		d.cfg.Store(&config.Config{})

		pushStrandedWithFragment(t, anvilPath, branch, bead.ID)

		// Pre-populate a stale retry record (e.g. from a prior dispatch failure)
		// to verify the concurrent-PR path clears it.
		require.NoError(t, db.MarkNeedsHuman(bead.ID, bead.Anvil, "prior failure"))

		if d.preDispatchRemoteBranchCheck(context.Background(), bead, anvilPath) {
			t.Fatal("expected dispatch to be skipped when a PR exists for the branch")
		}

		require.Equal(t, int32(0), mockVCS.createPRCalls.Load(), "CreatePR must not be called when a PR already exists")

		r, err := db.GetRetry(bead.ID, bead.Anvil)
		require.NoError(t, err)
		require.Nil(t, r, "stale retry record must be cleared when a PR is found concurrently")
		pr, err := db.GetPRByNumber(bead.Anvil, 55)
		require.NoError(t, err)
		require.NotNil(t, pr, "concurrently-opened PR should be registered for bellows")
	})

	t.Run("stranded branch with completion signal but CreatePR fails: falls through to needs_human", func(t *testing.T) {
		anvilPath := initTestGitRepo(t)
		db, err := state.Open(filepath.Join(anvilPath, "state.db"))
		require.NoError(t, err)
		defer db.Close()

		bead := poller.Bead{ID: "PRD-RECOVER-FAIL", Anvil: anvilName, Title: "recover fail"}
		branch := worktree.BranchName(bead.ID)
		mockVCS := &mockVCSProvider{
			createPRErr: fmt.Errorf("gh pr create exploded"),
		}
		d := &Daemon{
			db:           db,
			logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
			worktreeMgr:  worktree.NewManager(),
			vcsProviders: map[string]vcs.Provider{anvilName: mockVCS},
		}
		d.cfg.Store(&config.Config{})

		pushStrandedWithFragment(t, anvilPath, branch, bead.ID)

		if d.preDispatchRemoteBranchCheck(context.Background(), bead, anvilPath) {
			t.Fatal("expected dispatch to be blocked when CreatePR fails")
		}

		require.Equal(t, int32(1), mockVCS.createPRCalls.Load(), "CreatePR should be attempted once")

		r, err := db.GetRetry(bead.ID, bead.Anvil)
		require.NoError(t, err)
		require.NotNil(t, r, "retry row should be created on fall-through to needs_human")
		if !r.NeedsHuman {
			t.Errorf("bead should be marked needs_human when auto-recovery fails")
		}

		events, err := db.RecentEvents(20)
		require.NoError(t, err)
		var blocked bool
		for _, ev := range events {
			if ev.Type == state.EventDispatchBlockedStrandedBranch && ev.BeadID == bead.ID {
				blocked = true
				break
			}
		}
		require.True(t, blocked, "expected EventDispatchBlockedStrandedBranch on fall-through")
	})
}

func TestHandleIPC_WicketScan(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
	}
	d.cfg.Store(&config.Config{})

	t.Run("monitor not running returns error", func(t *testing.T) {
		// wicketMonitor is nil by default
		resp := d.handleIPC(ipc.Command{Type: "wicket_scan"})
		assert.Equal(t, "error", resp.Type)
		var msg map[string]string
		require.NoError(t, json.Unmarshal(resp.Payload, &msg))
		assert.Contains(t, msg["message"], "wicket monitor is not running")
	})

	t.Run("monitor running returns ok", func(t *testing.T) {
		wm := wicket.New(&config.Config{}, db)
		d.wicketMu.Lock()
		d.wicketMonitor = wm
		d.wicketMu.Unlock()
		defer func() {
			d.wicketMu.Lock()
			d.wicketMonitor = nil
			d.wicketMu.Unlock()
		}()

		resp := d.handleIPC(ipc.Command{Type: "wicket_scan"})
		assert.Equal(t, "ok", resp.Type)
		var msg map[string]string
		require.NoError(t, json.Unmarshal(resp.Payload, &msg))
		assert.Contains(t, msg["message"], "wicket scan triggered")
	})
}

func TestResolveTemperConfig_StepsOverridesSlots(t *testing.T) {
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	d := &Daemon{logger: logger}

	t.Run("steps only", func(t *testing.T) {
		logBuf.Reset()
		anvilCfg := config.AnvilConfig{
			Path: "/tmp/test-anvil",
			Temper: &config.TemperCommandsConfig{
				Steps: []config.TemperStepConfig{
					{Name: "build", Command: "make", Args: []string{"build"}},
					{Name: "test", Command: "make", Args: []string{"test"}},
				},
			},
		}
		cfg := d.resolveTemperConfig(anvilCfg)
		require.NotNil(t, cfg)
		assert.Len(t, cfg.Steps, 2)
		assert.Equal(t, "build", cfg.Steps[0].Name)
		assert.Equal(t, "test", cfg.Steps[1].Name)
		assert.NotContains(t, logBuf.String(), "overrides")
	})

	t.Run("steps with build also set logs warning", func(t *testing.T) {
		logBuf.Reset()
		anvilCfg := config.AnvilConfig{
			Path: "/tmp/test-anvil",
			Temper: &config.TemperCommandsConfig{
				Build: "make build",
				Steps: []config.TemperStepConfig{
					{Name: "custom-build", Command: "cargo", Args: []string{"build"}},
				},
			},
		}
		cfg := d.resolveTemperConfig(anvilCfg)
		require.NotNil(t, cfg)
		assert.Len(t, cfg.Steps, 1)
		assert.Equal(t, "custom-build", cfg.Steps[0].Name)
		assert.Contains(t, logBuf.String(), "overrides")
	})

	t.Run("legacy slots without steps", func(t *testing.T) {
		logBuf.Reset()
		anvilCfg := config.AnvilConfig{
			Path: "/tmp/test-anvil",
			Temper: &config.TemperCommandsConfig{
				Build: "make build",
				Test:  "make test",
			},
		}
		cfg := d.resolveTemperConfig(anvilCfg)
		require.NotNil(t, cfg)
		assert.Len(t, cfg.Steps, 2)
		assert.Equal(t, "build", cfg.Steps[0].Name)
		assert.Equal(t, "test", cfg.Steps[1].Name)
	})

	t.Run("empty temper returns nil", func(t *testing.T) {
		anvilCfg := config.AnvilConfig{
			Path:   "/tmp/test-anvil",
			Temper: &config.TemperCommandsConfig{},
		}
		cfg := d.resolveTemperConfig(anvilCfg)
		assert.Nil(t, cfg)
	})
}

func TestFetchExternalRef(t *testing.T) {
	t.Run("returns external_ref from bd show", func(t *testing.T) {
		d := &Daemon{
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		d.beadShower = func(anvilPath, beadID string) ([]byte, string, error) {
			return []byte(`{"id":"Forge-test","external_ref":"gh-42"}`), "", nil
		}
		got := d.fetchExternalRef("/tmp/anvil", "Forge-test")
		assert.Equal(t, "gh-42", got)
	})

	t.Run("returns empty when external_ref absent", func(t *testing.T) {
		d := &Daemon{
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		d.beadShower = func(anvilPath, beadID string) ([]byte, string, error) {
			return []byte(`{"id":"Forge-test"}`), "", nil
		}
		got := d.fetchExternalRef("/tmp/anvil", "Forge-test")
		assert.Equal(t, "", got)
	})

	t.Run("returns empty when bd show fails", func(t *testing.T) {
		d := &Daemon{
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		d.beadShower = func(anvilPath, beadID string) ([]byte, string, error) {
			return nil, "command not found", fmt.Errorf("exec: bd: not found")
		}
		got := d.fetchExternalRef("/tmp/anvil", "Forge-test")
		assert.Equal(t, "", got)
	})

	t.Run("unwraps JSON array", func(t *testing.T) {
		d := &Daemon{
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		d.beadShower = func(anvilPath, beadID string) ([]byte, string, error) {
			return []byte(`[{"id":"Forge-test","external_ref":"gh-99"}]`), "", nil
		}
		got := d.fetchExternalRef("/tmp/anvil", "Forge-test")
		assert.Equal(t, "gh-99", got)
	})
}

// TestReleaseBeadClaim verifies that releaseBeadClaim invokes bd with the
// correct arguments (--status=open --assignee= and, when stripTag=true,
// --remove-label for the auto_dispatch_tag), preserves the tag when
// stripTag=false (transient failure path — see Forge-dua2), and does NOT
// invoke bd when the anvil is absent from config (covering the claim-race /
// pre-claim safety condition).
func TestReleaseBeadClaim(t *testing.T) {
	t.Run("stripTag=true removes auto_dispatch_tag", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "forge-release-bead-*")
		require.NoError(t, err)
		defer os.RemoveAll(tmpDir)

		argsFile := filepath.Join(tmpDir, "bd-args.txt")
		var bdScript, bdContent string
		if runtime.GOOS == "windows" {
			bdScript = filepath.Join(tmpDir, "bd.bat")
			bdContent = "@echo off\necho %* > \"" + argsFile + "\"\nexit /b 0\n"
		} else {
			bdScript = filepath.Join(tmpDir, "bd")
			bdContent = "#!/bin/sh\necho \"$@\" > '" + argsFile + "'\nexit 0\n"
		}
		require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))

		oldPath := os.Getenv("PATH")
		os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
		defer os.Setenv("PATH", oldPath)

		dbPath := filepath.Join(tmpDir, "state.db")
		db, err := state.Open(dbPath)
		require.NoError(t, err)
		defer db.Close()

		d := &Daemon{
			db:     db,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			runCtx: context.Background(),
		}
		d.cfg.Store(&config.Config{
			Anvils: map[string]config.AnvilConfig{
				"test-anvil": {Path: tmpDir, AutoDispatchTag: "forgeReady"},
			},
		})

		d.releaseBeadClaim("TEST-1", "test-anvil", true)

		require.FileExists(t, argsFile)
		argsBytes, err := os.ReadFile(argsFile)
		require.NoError(t, err)
		args := strings.TrimSpace(string(argsBytes))
		assert.Contains(t, args, "update")
		assert.Contains(t, args, "TEST-1")
		assert.Contains(t, args, "--status=open")
		assert.Contains(t, args, "--assignee=")
		assert.Contains(t, args, "--remove-label=forgeReady")
	})

	t.Run("stripTag=false preserves auto_dispatch_tag (transient failure)", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "forge-release-bead-keeptag-*")
		require.NoError(t, err)
		defer os.RemoveAll(tmpDir)

		argsFile := filepath.Join(tmpDir, "bd-args.txt")
		var bdScript, bdContent string
		if runtime.GOOS == "windows" {
			bdScript = filepath.Join(tmpDir, "bd.bat")
			bdContent = "@echo off\necho %* > \"" + argsFile + "\"\nexit /b 0\n"
		} else {
			bdScript = filepath.Join(tmpDir, "bd")
			bdContent = "#!/bin/sh\necho \"$@\" > '" + argsFile + "'\nexit 0\n"
		}
		require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))

		oldPath := os.Getenv("PATH")
		os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
		defer os.Setenv("PATH", oldPath)

		dbPath := filepath.Join(tmpDir, "state.db")
		db, err := state.Open(dbPath)
		require.NoError(t, err)
		defer db.Close()

		d := &Daemon{
			db:     db,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			runCtx: context.Background(),
		}
		d.cfg.Store(&config.Config{
			Anvils: map[string]config.AnvilConfig{
				"test-anvil": {Path: tmpDir, AutoDispatchTag: "forgeReady"},
			},
		})

		d.releaseBeadClaim("TEST-KEEP", "test-anvil", false)

		require.FileExists(t, argsFile)
		argsBytes, err := os.ReadFile(argsFile)
		require.NoError(t, err)
		args := strings.TrimSpace(string(argsBytes))
		assert.Contains(t, args, "update")
		assert.Contains(t, args, "TEST-KEEP")
		assert.Contains(t, args, "--status=open")
		assert.Contains(t, args, "--assignee=")
		assert.NotContains(t, args, "--remove-label",
			"transient releaseBeadClaim must NOT strip the auto_dispatch_tag — "+
				"otherwise tagged-dispatch beads are silently stranded (Forge-dua2)")
	})

	t.Run("does not call bd when anvil is missing from config (claim-race safety)", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "forge-release-bead-noanvil-*")
		require.NoError(t, err)
		defer os.RemoveAll(tmpDir)

		calledFile := filepath.Join(tmpDir, "bd-called.txt")
		var bdScript, bdContent string
		if runtime.GOOS == "windows" {
			bdScript = filepath.Join(tmpDir, "bd.bat")
			bdContent = "@echo off\necho called > \"" + calledFile + "\"\nexit /b 0\n"
		} else {
			bdScript = filepath.Join(tmpDir, "bd")
			bdContent = "#!/bin/sh\ntouch '" + calledFile + "'\nexit 0\n"
		}
		require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))

		oldPath := os.Getenv("PATH")
		os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
		defer os.Setenv("PATH", oldPath)

		dbPath := filepath.Join(tmpDir, "state.db")
		db, err := state.Open(dbPath)
		require.NoError(t, err)
		defer db.Close()

		d := &Daemon{
			db:     db,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			runCtx: context.Background(),
		}
		// Anvil "unknown-anvil" is not in config — simulates pre-claim or race condition.
		d.cfg.Store(&config.Config{
			Anvils: map[string]config.AnvilConfig{},
		})

		d.releaseBeadClaim("TEST-2", "unknown-anvil", true)

		assert.NoFileExists(t, calledFile, "bd must not be called when anvil is absent from config")
	})

	t.Run("recordDispatchFailure skips bd release when releaseClaim is false", func(t *testing.T) {
		tmpDir, err := os.MkdirTemp("", "forge-release-bead-skip-*")
		require.NoError(t, err)
		defer os.RemoveAll(tmpDir)

		calledFile := filepath.Join(tmpDir, "bd-called.txt")
		var bdScript, bdContent string
		if runtime.GOOS == "windows" {
			bdScript = filepath.Join(tmpDir, "bd.bat")
			bdContent = "@echo off\necho called > \"" + calledFile + "\"\nexit /b 0\n"
		} else {
			bdScript = filepath.Join(tmpDir, "bd")
			bdContent = "#!/bin/sh\ntouch '" + calledFile + "'\nexit 0\n"
		}
		require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))

		oldPath := os.Getenv("PATH")
		os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
		defer os.Setenv("PATH", oldPath)

		dbPath := filepath.Join(tmpDir, "state.db")
		db, err := state.Open(dbPath)
		require.NoError(t, err)
		defer db.Close()

		d := &Daemon{
			db:     db,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			runCtx: context.Background(),
		}
		d.cfg.Store(&config.Config{
			Anvils: map[string]config.AnvilConfig{
				"test-anvil": {Path: tmpDir},
			},
		})

		// releaseClaim=false simulates a claim failure — bd must not be invoked.
		d.recordDispatchFailure("TEST-3", "test-anvil", "claim failed: connection refused", false)

		assert.NoFileExists(t, calledFile, "bd must not be called when releaseClaim is false")
	})
}

// TestRecordDispatchFailure_TagPreservation verifies the Forge-dua2 fix: on a
// tagged-dispatch anvil, a transient pipeline failure (dispatch_failures <
// MaxDispatchFailures) must NOT strip the auto_dispatch_tag, otherwise the
// bead becomes invisible to `bd ready` and is silently stranded. Once the
// circuit breaker trips (dispatch_failures >= MaxDispatchFailures), the bead
// is escalated to needs_human and the tag is stripped to keep `bd ready` clean.
func TestRecordDispatchFailure_TagPreservation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-dispatch-tag-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	argsLog := filepath.Join(tmpDir, "bd-args.log")
	var bdScript, bdContent string
	if runtime.GOOS == "windows" {
		bdScript = filepath.Join(tmpDir, "bd.bat")
		bdContent = "@echo off\necho %* >> \"" + argsLog + "\"\nexit /b 0\n"
	} else {
		bdScript = filepath.Join(tmpDir, "bd")
		bdContent = "#!/bin/sh\necho \"$@\" >> '" + argsLog + "'\nexit 0\n"
	}
	require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
	defer os.Setenv("PATH", oldPath)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		runCtx: context.Background(),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"munin": {Path: tmpDir, AutoDispatchTag: "forgeReady"},
		},
	})

	const beadID = "Fhi.Metadata-tw59f"
	const anvil = "munin"

	// Simulate transient failures up to (but not including) the circuit breaker
	// threshold. Each call must clear the bead claim but preserve the tag.
	for i := 1; i < MaxDispatchFailures; i++ {
		d.recordDispatchFailure(beadID, anvil, "temper exhausted", true)
	}

	logBytes, err := os.ReadFile(argsLog)
	require.NoError(t, err)
	transientLog := string(logBytes)

	assert.Contains(t, transientLog, "update "+beadID,
		"bd update should be invoked to clear the claim on transient failures")
	assert.Contains(t, transientLog, "--status=open")
	assert.Contains(t, transientLog, "--assignee=")
	assert.NotContains(t, transientLog, "--remove-label",
		"transient dispatch failure must NOT strip the auto_dispatch_tag — "+
			"otherwise tagged-dispatch beads are silently stranded")

	// Confirm the circuit breaker has not tripped yet — bead is still retryable.
	r, err := db.GetRetry(beadID, anvil)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.False(t, r.NeedsHuman, "bead must remain retryable below the dispatch threshold")
	assert.Equal(t, MaxDispatchFailures-1, r.DispatchFailures)

	// One more failure should trip the circuit breaker (needs_human=1) and
	// strip the tag, since the bead is now in Needs Attention.
	require.NoError(t, os.Truncate(argsLog, 0))
	d.recordDispatchFailure(beadID, anvil, "temper exhausted", true)

	logBytes, err = os.ReadFile(argsLog)
	require.NoError(t, err)
	brokenLog := string(logBytes)

	assert.Contains(t, brokenLog, "--remove-label=forgeReady",
		"tag must be stripped when the dispatch circuit breaker trips")

	r, err = db.GetRetry(beadID, anvil)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.True(t, r.NeedsHuman, "needs_human must be set after the circuit breaker trips")
	assert.GreaterOrEqual(t, r.DispatchFailures, MaxDispatchFailures)
}

func TestDedupeCacheItems(t *testing.T) {
	t.Run("no duplicates returns input unchanged", func(t *testing.T) {
		in := []state.QueueItem{
			{BeadID: "Forge-1", Anvil: "forge", Section: state.QueueSectionUnlabeled},
			{BeadID: "Forge-2", Anvil: "forge", Section: state.QueueSectionReady},
			{BeadID: "Hytte-1", Anvil: "hytte", Section: state.QueueSectionUnlabeled},
		}
		out := dedupeCacheItems(in)
		assert.Len(t, out, 3)
		assert.Equal(t, in, out)
	})

	t.Run("ready+in_progress collision keeps in_progress", func(t *testing.T) {
		// Caller appends in_progress entries after ready entries, so
		// last-write-wins must preserve the in_progress row.
		in := []state.QueueItem{
			{BeadID: "Forge-1", Anvil: "forge", Section: state.QueueSectionReady, Title: "ready-row"},
			{BeadID: "Forge-2", Anvil: "forge", Section: state.QueueSectionReady},
			{BeadID: "Forge-1", Anvil: "forge", Section: state.QueueSectionInProgress, Title: "in-progress-row"},
		}
		out := dedupeCacheItems(in)
		assert.Len(t, out, 2)
		// Find the Forge-1 entry and verify it's the in-progress one.
		var found *state.QueueItem
		for i := range out {
			if out[i].BeadID == "Forge-1" {
				found = &out[i]
			}
		}
		if assert.NotNil(t, found, "Forge-1 must survive dedupe") {
			assert.Equal(t, state.QueueSectionInProgress, found.Section)
			assert.Equal(t, "in-progress-row", found.Title)
		}
	})

	t.Run("same bead id on different anvils kept separately", func(t *testing.T) {
		in := []state.QueueItem{
			{BeadID: "shared-1", Anvil: "forge", Section: state.QueueSectionReady},
			{BeadID: "shared-1", Anvil: "hytte", Section: state.QueueSectionReady},
		}
		out := dedupeCacheItems(in)
		assert.Len(t, out, 2)
	})

	t.Run("empty input returns empty", func(t *testing.T) {
		assert.Empty(t, dedupeCacheItems(nil))
		assert.Empty(t, dedupeCacheItems([]state.QueueItem{}))
	})

	t.Run("multiple duplicates collapse to last occurrence", func(t *testing.T) {
		in := []state.QueueItem{
			{BeadID: "Forge-1", Anvil: "forge", Title: "first"},
			{BeadID: "Forge-1", Anvil: "forge", Title: "second"},
			{BeadID: "Forge-1", Anvil: "forge", Title: "third"},
		}
		out := dedupeCacheItems(in)
		assert.Len(t, out, 1)
		assert.Equal(t, "third", out[0].Title)
	})
}

func TestHandleIPC_Queue(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-ipc-queue-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.cfg.Store(&config.Config{})

	t.Run("empty queue returns empty items array", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "queue"})
		require.Equal(t, "ok", resp.Type)

		var payload ipc.QueueResponse
		require.NoError(t, json.Unmarshal(resp.Payload, &payload))
		assert.NotNil(t, payload.Items)
		assert.Empty(t, payload.Items)
	})

	t.Run("seeded queue items are returned", func(t *testing.T) {
		seed := []state.QueueItem{
			{
				BeadID:   "Forge-abc1",
				Anvil:    "forge",
				Title:    "Add feature X",
				Priority: 2,
				Status:   "ready",
				Labels:   `["forgeReady"]`,
				Section:  state.QueueSectionReady,
			},
			{
				BeadID:   "Forge-def2",
				Anvil:    "forge",
				Title:    "Fix bug Y",
				Priority: 1,
				Status:   "ready",
				Labels:   `[]`,
				Section:  state.QueueSectionReady,
			},
		}
		require.NoError(t, db.ReplaceQueueCacheForAnvils([]string{"forge"}, seed))

		resp := d.handleIPC(ipc.Command{Type: "queue"})
		require.Equal(t, "ok", resp.Type)

		var payload ipc.QueueResponse
		require.NoError(t, json.Unmarshal(resp.Payload, &payload))
		require.Len(t, payload.Items, 2)

		ids := []string{payload.Items[0].BeadID, payload.Items[1].BeadID}
		assert.Contains(t, ids, "Forge-abc1")
		assert.Contains(t, ids, "Forge-def2")

		// Verify label parsing: "forgeReady" label should be a slice element.
		for _, item := range payload.Items {
			if item.BeadID == "Forge-abc1" {
				assert.Equal(t, []string{"forgeReady"}, item.Labels)
			}
		}
	})
}

// TestHandleIPC_Queue_Timestamps verifies that the queue endpoint surfaces
// created_at / updated_at sourced from the in-memory poller snapshot and
// emits both keys (even as empty strings) so the QueueItem JSON shape stays
// stable for the frontend date-column work that depends on this contract.
func TestHandleIPC_Queue_Timestamps(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-ipc-queue-ts-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.cfg.Store(&config.Config{})

	seed := []state.QueueItem{
		{
			BeadID:   "Forge-abc1",
			Anvil:    "forge",
			Title:    "With timestamps",
			Priority: 2,
			Status:   "ready",
			Labels:   `[]`,
			Section:  state.QueueSectionReady,
		},
		{
			BeadID:   "Forge-def2",
			Anvil:    "forge",
			Title:    "No snapshot entry",
			Priority: 3,
			Status:   "ready",
			Labels:   `[]`,
			Section:  state.QueueSectionReady,
		},
	}
	require.NoError(t, db.ReplaceQueueCacheForAnvils([]string{"forge"}, seed))

	const created = "2026-05-08T04:15:41Z"
	const updated = "2026-05-12T11:08:11Z"
	d.replaceQueueTimestamps(
		map[string]struct{}{"forge": {}},
		map[string]queueTimestamp{
			"forge/Forge-abc1": {CreatedAt: created, UpdatedAt: updated},
		},
	)

	resp := d.handleIPC(ipc.Command{Type: "queue"})
	require.Equal(t, "ok", resp.Type)

	// Verify the raw JSON contains the new keys so the wire contract is
	// stable for the frontend (created_at/updated_at must always be present,
	// even when empty).
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	require.NoError(t, json.Unmarshal(resp.Payload, &raw))
	require.Len(t, raw.Items, 2)
	for _, item := range raw.Items {
		_, hasCreated := item["created_at"]
		_, hasUpdated := item["updated_at"]
		assert.True(t, hasCreated, "every queue item must expose a created_at key")
		assert.True(t, hasUpdated, "every queue item must expose an updated_at key")
	}

	var payload ipc.QueueResponse
	require.NoError(t, json.Unmarshal(resp.Payload, &payload))
	require.Len(t, payload.Items, 2)

	byID := map[string]ipc.QueueItem{}
	for _, item := range payload.Items {
		byID[item.BeadID] = item
	}
	assert.Equal(t, created, byID["Forge-abc1"].CreatedAt)
	assert.Equal(t, updated, byID["Forge-abc1"].UpdatedAt)
	// Beads missing from the snapshot fall back to empty strings rather
	// than crashing the handler or omitting the row entirely.
	assert.Equal(t, "", byID["Forge-def2"].CreatedAt)
	assert.Equal(t, "", byID["Forge-def2"].UpdatedAt)
}

func TestHandleIPC_Workers(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-ipc-workers-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.cfg.Store(&config.Config{})

	t.Run("no active workers returns empty array", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "workers"})
		require.Equal(t, "ok", resp.Type)

		var payload ipc.WorkersResponse
		require.NoError(t, json.Unmarshal(resp.Payload, &payload))
		assert.NotNil(t, payload.Workers)
		assert.Empty(t, payload.Workers)
	})

	t.Run("active workers are returned with correct schema", func(t *testing.T) {
		w := &state.Worker{
			ID:        "worker-1",
			BeadID:    "Forge-abc1",
			Anvil:     "forge",
			Branch:    "forge/Forge-abc1",
			Title:     "Add feature X",
			Status:    state.WorkerRunning,
			Phase:     "smith",
			PID:       12345,
			StartedAt: time.Now().UTC().Truncate(time.Second),
		}
		require.NoError(t, db.InsertWorker(w))

		resp := d.handleIPC(ipc.Command{Type: "workers"})
		require.Equal(t, "ok", resp.Type)

		var payload ipc.WorkersResponse
		require.NoError(t, json.Unmarshal(resp.Payload, &payload))
		require.Len(t, payload.Workers, 1)

		got := payload.Workers[0]
		assert.Equal(t, "worker-1", got.ID)
		assert.Equal(t, "Forge-abc1", got.BeadID)
		assert.Equal(t, "forge", got.Anvil)
		assert.Equal(t, "forge/Forge-abc1", got.Branch)
		assert.Equal(t, "running", got.Status)
		assert.Equal(t, "smith", got.Phase)
		assert.Equal(t, 12345, got.PID)
		assert.NotEmpty(t, got.StartedAt)
		assert.Empty(t, got.CompletedAt)
	})

	t.Run("phase is populated for every pipeline stage", func(t *testing.T) {
		// Clear out the row inserted by the previous subtest so we control
		// the exact set of phases under assertion here.
		_, err := db.Conn().Exec(`DELETE FROM workers`)
		require.NoError(t, err)

		phases := []string{"schematic", "smith", "temper", "warden", "bellows", "quench", "burnish", "rebase"}
		started := time.Now().UTC().Truncate(time.Second)
		for i, phase := range phases {
			w := &state.Worker{
				ID:        fmt.Sprintf("worker-phase-%d", i),
				BeadID:    fmt.Sprintf("Forge-ph%02d", i),
				Anvil:     "forge",
				Status:    state.WorkerRunning,
				Phase:     phase,
				StartedAt: started,
			}
			require.NoError(t, db.InsertWorker(w))
		}

		resp := d.handleIPC(ipc.Command{Type: "workers"})
		require.Equal(t, "ok", resp.Type)
		var payload ipc.WorkersResponse
		require.NoError(t, json.Unmarshal(resp.Payload, &payload))
		require.Len(t, payload.Workers, len(phases))

		seen := map[string]bool{}
		for _, w := range payload.Workers {
			assert.NotEmpty(t, w.Phase, "worker %s missing phase", w.ID)
			seen[w.Phase] = true
		}
		for _, phase := range phases {
			assert.Truef(t, seen[phase], "expected to see phase %q in workers response", phase)
		}
	})
}

// TestHandleIPC_Status_MaxTotalSmiths verifies that the daemon's status
// response exposes the configured concurrent-Smith cap so the Hearth 2.0
// SPA can size the Workers pane's "Idle" placeholder slots.
func TestHandleIPC_Status_MaxTotalSmiths(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-ipc-status-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:        db,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		startTime: time.Now(),
	}
	d.cfg.Store(&config.Config{Settings: config.SettingsConfig{MaxTotalSmiths: 7}})

	resp := d.handleIPC(ipc.Command{Type: "status"})
	require.Equal(t, "status", resp.Type)

	var payload ipc.StatusPayload
	require.NoError(t, json.Unmarshal(resp.Payload, &payload))
	assert.Equal(t, 7, payload.MaxTotalSmiths)
}

// TestDaemon_RecordAnvilPoll verifies that the in-memory last-poll map is
// updated on every poll completion (success and failure) and that
// anvilPollSnapshots returns a defensive copy so callers cannot mutate the
// daemon's internal state.
func TestDaemon_RecordAnvilPoll(t *testing.T) {
	d := &Daemon{
		lastPollMap: make(map[string]anvilPollSnapshot),
	}

	// Empty anvil names must be ignored so callers do not silently overwrite
	// an empty-string key with timestamps that will never match a real anvil.
	d.recordAnvilPoll("", true, "should be dropped")
	assert.Empty(t, d.anvilPollSnapshots(), "empty anvil name must not populate the map")

	before := time.Now()
	d.recordAnvilPoll("anvil-a", true, "[fast] 3 ready")
	d.recordAnvilPoll("anvil-b", false, "bd ready failed: boom")
	after := time.Now()

	snaps := d.anvilPollSnapshots()
	require.Len(t, snaps, 2)

	a := snaps["anvil-a"]
	assert.True(t, a.OK)
	assert.Equal(t, "[fast] 3 ready", a.Message)
	assert.False(t, a.Timestamp.Before(before))
	assert.False(t, a.Timestamp.After(after))

	b := snaps["anvil-b"]
	assert.False(t, b.OK)
	assert.Equal(t, "bd ready failed: boom", b.Message)

	// The returned map must be a copy; mutating it must not affect the daemon.
	snaps["anvil-a"] = anvilPollSnapshot{Message: "tampered"}
	delete(snaps, "anvil-b")
	again := d.anvilPollSnapshots()
	assert.Equal(t, "[fast] 3 ready", again["anvil-a"].Message, "internal map must not be mutated by callers")
	assert.Contains(t, again, "anvil-b", "deleting from the copy must not remove the original entry")

	// A second record for the same anvil must overwrite the previous snapshot.
	d.recordAnvilPoll("anvil-a", false, "bd ready failed: second")
	updated := d.anvilPollSnapshots()
	assert.False(t, updated["anvil-a"].OK)
	assert.Equal(t, "bd ready failed: second", updated["anvil-a"].Message)
}

// TestHandleIPC_Status_AnvilLastPoll verifies that the in-memory per-anvil
// last-poll snapshot is projected into the status IPC response so Hearth /
// Mezzanine / `forge status` can read freshness without hitting SQLite.
func TestHandleIPC_Status_AnvilLastPoll(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-status-poll-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:          db,
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		lastPollMap: make(map[string]anvilPollSnapshot),
		startTime:   time.Now(),
	}
	d.cfg.Store(&config.Config{})

	t.Run("empty when no polls have completed", func(t *testing.T) {
		resp := d.handleIPC(ipc.Command{Type: "status"})
		require.Equal(t, "status", resp.Type)
		var payload ipc.StatusPayload
		require.NoError(t, json.Unmarshal(resp.Payload, &payload))
		assert.Empty(t, payload.AnvilLastPoll)
	})

	t.Run("includes per-anvil snapshots after polls", func(t *testing.T) {
		d.recordAnvilPoll("zeta", true, "[fast] 0 ready")
		d.recordAnvilPoll("alpha", false, "bd ready failed: boom")

		resp := d.handleIPC(ipc.Command{Type: "status"})
		require.Equal(t, "status", resp.Type)
		var payload ipc.StatusPayload
		require.NoError(t, json.Unmarshal(resp.Payload, &payload))

		require.Len(t, payload.AnvilLastPoll, 2)
		// Entries must be sorted by anvil name for deterministic IPC output.
		assert.Equal(t, "alpha", payload.AnvilLastPoll[0].Anvil)
		assert.False(t, payload.AnvilLastPoll[0].OK)
		assert.Equal(t, "bd ready failed: boom", payload.AnvilLastPoll[0].Message)
		assert.False(t, payload.AnvilLastPoll[0].Timestamp.IsZero())

		assert.Equal(t, "zeta", payload.AnvilLastPoll[1].Anvil)
		assert.True(t, payload.AnvilLastPoll[1].OK)
		assert.Equal(t, "[fast] 0 ready", payload.AnvilLastPoll[1].Message)
	})
}

// TestHandleIPC_Workers_ReadyToMerge verifies that the daemon's workers IPC
// response promotes a synthetic bellows monitor worker's phase to
// "ready_to_merge" when its underlying PR satisfies every ready-to-merge
// condition (CI green, no pending reviews, no unresolved threads, not
// conflicting, non-terminal status). PRs that fail any single flag must
// keep the worker on phase "bellows" so the SPA's PipelineBar counts them
// in the PR/Bellows stage instead. Merged PRs leave the workers list
// entirely once their synthetic monitor is swept.
func TestHandleIPC_Workers_ReadyToMerge(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-ipc-workers-rtm-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	defer db.Close()

	now := time.Now()
	// Three PRs across two anvils so the test also exercises the
	// per-anvil grouping required by the bead's "per-anvil section" note.
	// - anvil-a/PR 100: every flag green → ready_to_merge.
	// - anvil-a/PR 101: CI failing → stays in PR/Bellows.
	// - anvil-b/PR 200: every flag green → ready_to_merge.
	prs := []state.PR{
		{Number: 100, Anvil: "anvil-a", BeadID: "Forge-rtm1", Branch: "forge/Forge-rtm1", Status: state.PROpen, CreatedAt: now, Title: "rtm1"},
		{Number: 101, Anvil: "anvil-a", BeadID: "Forge-rtm2", Branch: "forge/Forge-rtm2", Status: state.PROpen, CreatedAt: now, Title: "rtm2"},
		{Number: 200, Anvil: "anvil-b", BeadID: "Forge-rtm3", Branch: "forge/Forge-rtm3", Status: state.PROpen, CreatedAt: now, Title: "rtm3"},
	}
	for i := range prs {
		require.NoError(t, db.InsertPR(&prs[i]))
	}
	require.NoError(t, db.UpdatePRMergeability(prs[0].ID, true, false, false, false, true, true))
	require.NoError(t, db.UpdatePRMergeability(prs[1].ID, false, false, false, false, true, true))
	require.NoError(t, db.UpdatePRMergeability(prs[2].ID, true, false, false, false, true, true))

	for _, p := range prs {
		require.NoError(t, db.InsertWorkerIfMissing(&state.Worker{
			ID:        fmt.Sprintf("bellows-%s-%d", p.Anvil, p.Number),
			BeadID:    p.BeadID,
			Anvil:     p.Anvil,
			Branch:    p.Branch,
			Status:    state.WorkerMonitoring,
			Phase:     "bellows",
			Title:     p.Title,
			PRNumber:  p.Number,
			StartedAt: now,
		}))
	}

	d := &Daemon{
		db:        db,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		startTime: now,
	}
	d.cfg.Store(&config.Config{})

	resp := d.handleIPC(ipc.Command{Type: "workers"})
	require.Equal(t, "ok", resp.Type)
	var payload ipc.WorkersResponse
	require.NoError(t, json.Unmarshal(resp.Payload, &payload))

	phaseByID := make(map[string]string, len(payload.Workers))
	for _, w := range payload.Workers {
		phaseByID[w.ID] = w.Phase
	}

	assert.Equal(t, "ready_to_merge", phaseByID["bellows-anvil-a-100"],
		"bellows monitor for a PR meeting every condition must be promoted to phase=ready_to_merge")
	assert.Equal(t, "bellows", phaseByID["bellows-anvil-a-101"],
		"bellows monitor for a PR with CI failing must stay on phase=bellows so PipelineBar counts it in PR/Bellows")
	assert.Equal(t, "ready_to_merge", phaseByID["bellows-anvil-b-200"],
		"per-anvil grouping: a ready PR on a different anvil must also be promoted")

	// Counting by anvil mirrors what the SPA does client-side. The
	// per-anvil ready_to_merge tally must reflect only the PRs whose flags
	// are all green; the failing-CI PR must NOT be counted.
	readyByAnvil := map[string]int{}
	for _, w := range payload.Workers {
		if w.Phase == "ready_to_merge" {
			readyByAnvil[w.Anvil]++
		}
	}
	assert.Equal(t, 1, readyByAnvil["anvil-a"], "anvil-a has exactly one ready_to_merge PR")
	assert.Equal(t, 1, readyByAnvil["anvil-b"], "anvil-b has exactly one ready_to_merge PR")
}

// TestHandleIPC_Workers_ReadyToMerge_FlagSensitivity verifies that flipping
// any single ready-to-merge condition (is_conflicting, has_unresolved_threads,
// has_pending_reviews, ci_passing) causes the bellows monitor worker's phase
// to fall back from "ready_to_merge" to "bellows" so the PR is counted in the
// PR/Bellows stage rather than Ready-to-merge.
func TestHandleIPC_Workers_ReadyToMerge_FlagSensitivity(t *testing.T) {
	cases := []struct {
		name                 string
		ciPassing            bool
		isConflicting        bool
		hasUnresolvedThreads bool
		hasPendingReviews    bool
		wantPhase            string
	}{
		{"all green", true, false, false, false, "ready_to_merge"},
		{"ci failing", false, false, false, false, "bellows"},
		{"conflicting", true, true, false, false, "bellows"},
		{"unresolved threads", true, false, true, false, "bellows"},
		{"pending reviews", true, false, false, true, "bellows"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "forge-rtm-flag-*")
			require.NoError(t, err)
			defer os.RemoveAll(tmpDir)

			db, err := state.Open(filepath.Join(tmpDir, "state.db"))
			require.NoError(t, err)
			defer db.Close()

			now := time.Now()
			pr := state.PR{Number: 42, Anvil: "anvil", BeadID: "Forge-rtmflag", Branch: "forge/Forge-rtmflag", Status: state.PROpen, CreatedAt: now}
			require.NoError(t, db.InsertPR(&pr))
			require.NoError(t, db.UpdatePRMergeability(pr.ID, tc.ciPassing, tc.isConflicting, tc.hasUnresolvedThreads, tc.hasPendingReviews, true, true))
			require.NoError(t, db.InsertWorkerIfMissing(&state.Worker{
				ID:        "bellows-anvil-42",
				BeadID:    pr.BeadID,
				Anvil:     pr.Anvil,
				Status:    state.WorkerMonitoring,
				Phase:     "bellows",
				PRNumber:  pr.Number,
				StartedAt: now,
			}))

			d := &Daemon{
				db:        db,
				logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
				startTime: now,
			}
			d.cfg.Store(&config.Config{})

			resp := d.handleIPC(ipc.Command{Type: "workers"})
			require.Equal(t, "ok", resp.Type)
			var payload ipc.WorkersResponse
			require.NoError(t, json.Unmarshal(resp.Payload, &payload))
			require.Len(t, payload.Workers, 1)
			assert.Equal(t, tc.wantPhase, payload.Workers[0].Phase)
		})
	}
}

// TestPollAndDispatch_NoSuccessPollEvent verifies the events table no longer
// receives a row per successful poll — successful polls are tracked only in
// the daemon's in-memory map — while failures still appear as poll_error rows
// so they remain visible in `forge history events` and the Hearth/Mezzanine
// events panels.
func TestPollAndDispatch_NoSuccessPollEvent(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-poll-events-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Two fake bd scripts: one that succeeds (returns an empty ready list) and
	// one that fails. We run them in two separate daemon instances rather than
	// flipping PATH mid-test because pollAndDispatch may resolve the script via
	// exec.LookPath once and cache it.
	successDir := filepath.Join(tmpDir, "success")
	failureDir := filepath.Join(tmpDir, "failure")
	require.NoError(t, os.MkdirAll(successDir, 0o755))
	require.NoError(t, os.MkdirAll(failureDir, 0o755))

	var successScript, failureScript, successContent, failureContent string
	if runtime.GOOS == "windows" {
		successScript = filepath.Join(successDir, "bd.bat")
		failureScript = filepath.Join(failureDir, "bd.bat")
		successContent = "@echo off\r\nif \"%1\"==\"ready\" (echo []\r\nexit /b 0)\r\nif \"%1\"==\"list\" (echo []\r\nexit /b 0)\r\nexit /b 0\r\n"
		failureContent = "@echo off\r\nif \"%1\"==\"ready\" (echo bd failed 1>&2\r\nexit /b 1)\r\nif \"%1\"==\"list\" (echo []\r\nexit /b 0)\r\nexit /b 0\r\n"
	} else {
		successScript = filepath.Join(successDir, "bd")
		failureScript = filepath.Join(failureDir, "bd")
		successContent = "#!/bin/sh\ncase \"$1\" in ready|list) echo '[]'; exit 0 ;; esac\nexit 0\n"
		failureContent = "#!/bin/sh\ncase \"$1\" in ready) echo 'bd failed' 1>&2; exit 1 ;; list) echo '[]'; exit 0 ;; esac\nexit 0\n"
	}
	require.NoError(t, os.WriteFile(successScript, []byte(successContent), 0o755))
	require.NoError(t, os.WriteFile(failureScript, []byte(failureContent), 0o755))

	cfg := &config.Config{
		Settings: config.SettingsConfig{
			MaxTotalSmiths: 1,
			PollInterval:   10 * time.Second,
		},
		Anvils: map[string]config.AnvilConfig{
			"poll-anvil": {Path: tmpDir, AutoDispatch: "off"},
		},
	}

	countEventsByType := func(db *state.DB, typ state.EventType) int {
		events, err := db.RecentEvents(100)
		require.NoError(t, err)
		n := 0
		for _, e := range events {
			if e.Type == typ {
				n++
			}
		}
		return n
	}

	t.Run("success path emits no poll event", func(t *testing.T) {
		dbPath := filepath.Join(tmpDir, "success.db")
		db, err := state.Open(dbPath)
		require.NoError(t, err)
		defer db.Close()

		oldPath := os.Getenv("PATH")
		os.Setenv("PATH", successDir+string(os.PathListSeparator)+oldPath)
		defer os.Setenv("PATH", oldPath)

		d := &Daemon{
			db:            db,
			logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
			worktreeMgr:   worktree.NewManager(),
			promptBuilder: prompt.NewBuilder(),
			lastPollMap:   make(map[string]anvilPollSnapshot),
		}
		d.cfg.Store(cfg)

		d.pollAndDispatch(context.Background(), true)

		assert.Equal(t, 0, countEventsByType(db, state.EventPoll), "successful polls must not write EventPoll rows")
		assert.Equal(t, 0, countEventsByType(db, state.EventPollError), "successful polls must not write EventPollError rows")

		snaps := d.anvilPollSnapshots()
		require.Len(t, snaps, 1, "in-memory map must track the poll completion")
		got, ok := snaps["poll-anvil"]
		require.True(t, ok, "expected an entry for the configured anvil")
		assert.True(t, got.OK, "successful poll must mark snapshot OK=true")
		assert.False(t, got.Timestamp.IsZero(), "snapshot timestamp must be set on success")
	})

	t.Run("failure path emits exactly one poll_error event", func(t *testing.T) {
		dbPath := filepath.Join(tmpDir, "failure.db")
		db, err := state.Open(dbPath)
		require.NoError(t, err)
		defer db.Close()

		oldPath := os.Getenv("PATH")
		os.Setenv("PATH", failureDir+string(os.PathListSeparator)+oldPath)
		defer os.Setenv("PATH", oldPath)

		d := &Daemon{
			db:            db,
			logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
			worktreeMgr:   worktree.NewManager(),
			promptBuilder: prompt.NewBuilder(),
			lastPollMap:   make(map[string]anvilPollSnapshot),
		}
		d.cfg.Store(cfg)

		d.pollAndDispatch(context.Background(), true)

		assert.Equal(t, 0, countEventsByType(db, state.EventPoll), "failed polls must not write EventPoll rows")
		assert.Equal(t, 1, countEventsByType(db, state.EventPollError), "failed polls must write exactly one EventPollError row")

		snaps := d.anvilPollSnapshots()
		require.Len(t, snaps, 1, "in-memory map must track the failed poll")
		got, ok := snaps["poll-anvil"]
		require.True(t, ok, "expected an entry for the configured anvil")
		assert.False(t, got.OK, "failed poll must mark snapshot OK=false")
		assert.NotEmpty(t, got.Message, "snapshot message must capture the error text")
	})
}

// writeFakeBd installs a fake `bd` on PATH that logs its arguments to bdLog and
// exits 0 for `bd update ...` (any status), exiting 1 otherwise. It returns the
// path of the args log. The caller is responsible for restoring PATH (done via
// t.Cleanup here).
func writeFakeBd(t *testing.T, dir string) (bdLog string) {
	t.Helper()
	bdLog = filepath.Join(dir, "bd-args.log")
	var bdScript, bdContent string
	if runtime.GOOS == "windows" {
		bdScript = filepath.Join(dir, "bd.bat")
		bdContent = "@echo off\r\necho %*>>\"" + bdLog + "\"\r\nif \"%1\"==\"update\" (\r\n  echo {\"id\":\"%2\",\"status\":\"open\"}\r\n  exit /b 0\r\n)\r\nexit /b 1\r\n"
	} else {
		bdScript = filepath.Join(dir, "bd")
		bdContent = "#!/bin/sh\necho \"$@\" >> \"" + bdLog + "\"\nif [ \"$1\" = \"update\" ]; then\n  echo '{\"id\":\"'\"$2\"'\",\"status\":\"open\"}'\n  exit 0\nfi\nexit 1\n"
	}
	require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))

	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath)
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })
	return bdLog
}

// TestAbortClaim_KilledClaimReleasesAndFailsWorker verifies the Forge-au4z
// fix: when a bead claim is killed/timed out (non-atomic — the server-side
// write may have landed), abortClaim must (a) mark the pending worker failed
// immediately so it stops counting toward capacity, and (b) release the claim
// back to open so the bead self-heals via `bd ready`.
func TestAbortClaim_KilledClaimReleasesAndFailsWorker(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	bdLog := writeFakeBd(t, tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		runCtx: context.Background(),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"munin": {Path: tmpDir, AutoDispatchTag: "forgeReady"},
		},
	})

	const beadID = "BD-KILLED"

	workerID := d.insertPendingWorker(beadID, "munin", "killed claim title")
	require.NotEmpty(t, workerID)

	has, err := db.HasWorkerRecord(beadID, "munin")
	require.NoError(t, err)
	assert.True(t, has, "pre-inserted pending worker must give the bead a worker record")

	killedErr := fmt.Errorf("bd update %s --status=in_progress --json: signal: killed", beadID)
	d.abortClaim(beadID, "munin", workerID, "claim failed: signal: killed", killedErr)

	// (a) The pending worker must be marked failed first.
	w, err := db.GetWorker(workerID)
	require.NoError(t, err)
	require.NotNil(t, w)
	assert.Equal(t, state.WorkerFailed, w.Status, "pending worker must be marked failed after a claim abort")

	// (b) The bead must have been reverted to open via `bd update --status=open`
	// because this was a killed/timeout error (non-atomic).
	logBytes, err := os.ReadFile(bdLog)
	require.NoError(t, err)
	logged := string(logBytes)
	assert.Contains(t, logged, "update "+beadID, "abortClaim must issue a bd update for the bead")
	assert.Contains(t, logged, "--status=open", "killed claim must release the bead back to open")

	r, err := db.GetRetry(beadID, "munin")
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.GreaterOrEqual(t, r.DispatchFailures, 1, "abortClaim must record a dispatch failure")
}

// TestAbortClaim_ConflictDoesNotReleaseClaim verifies that when a claim fails
// with a non-timeout error (e.g. the bead is already in_progress, owned by
// another instance), abortClaim does NOT release the claim — doing so would
// unassign a bead legitimately owned by someone else. The pending worker must
// still be marked failed and a dispatch failure recorded.
func TestAbortClaim_ConflictDoesNotReleaseClaim(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	bdLog := writeFakeBd(t, tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		runCtx: context.Background(),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"munin": {Path: tmpDir, AutoDispatchTag: "forgeReady"},
		},
	})

	const beadID = "BD-CONFLICT"

	workerID := d.insertPendingWorker(beadID, "munin", "conflict claim title")
	require.NotEmpty(t, workerID)

	conflictErr := fmt.Errorf("bd update %s --status=in_progress --json: exit status 1\nbead already in_progress", beadID)
	d.abortClaim(beadID, "munin", workerID, fmt.Sprintf("claim failed: %v", conflictErr), conflictErr)

	// The pending worker must still be marked failed.
	w, err := db.GetWorker(workerID)
	require.NoError(t, err)
	require.NotNil(t, w)
	assert.Equal(t, state.WorkerFailed, w.Status, "pending worker must be marked failed even for conflict errors")

	// A dispatch failure must still be recorded.
	r, err := db.GetRetry(beadID, "munin")
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.GreaterOrEqual(t, r.DispatchFailures, 1, "abortClaim must record a dispatch failure")

	// The claim must NOT have been released — no bd update --status=open call.
	// The log file may not even exist if bd was never invoked, which is correct.
	logBytes, _ := os.ReadFile(bdLog)
	logged := string(logBytes)
	assert.NotContains(t, logged, "--status=open",
		"conflict error must not release the claim — another instance may own the bead")
}

// TestClaimBead_SuccessLeavesPendingWorkerIntact verifies the success path is
// not regressed by the Forge-au4z reordering: when the claim succeeds, the
// worker row pre-inserted before the claim is left pending (not reverted or
// failed) so the pipeline can take it over.
func TestClaimBead_SuccessLeavesPendingWorkerIntact(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	writeFakeBd(t, tmpDir) // fake bd exits 0 on update → claim succeeds

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		runCtx: context.Background(),
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{"munin": {Path: tmpDir}},
	})

	const beadID = "BD-OK"
	workerID := d.insertPendingWorker(beadID, "munin", "ok title")
	require.NotEmpty(t, workerID)

	require.NoError(t, d.claimBead(context.Background(), beadID, tmpDir),
		"claim should succeed when bd update exits 0")

	w, err := db.GetWorker(workerID)
	require.NoError(t, err)
	require.NotNil(t, w)
	assert.Equal(t, state.WorkerPending, w.Status, "a successful claim must leave the worker pending for the pipeline")

	// No dispatch failure should be recorded on the success path.
	r, err := db.GetRetry(beadID, "munin")
	require.NoError(t, err)
	if r != nil {
		assert.Equal(t, 0, r.DispatchFailures, "a successful claim must not record a dispatch failure")
	}
}

// TestFinalizePipelineCreatePRRetry verifies that the end-of-pipeline CreatePR
// is wrapped in transient-failure retry: a transient gh/GitHub error (e.g. a
// momentary 401) is auto-retried so the bead is NOT stranded, while a permanent
// error (e.g. 422 validation) surfaces immediately to the needs_human path with
// no retry. Covers Forge-ficr acceptance criteria (1), (2).
func TestFinalizePipelineCreatePRRetry(t *testing.T) {
	const anvilName = "test-anvil"

	newDaemon := func(t *testing.T, mock *mockVCSProvider) (*Daemon, *state.DB) {
		t.Helper()
		tmpDir, err := os.MkdirTemp("", "forge-finalize-*")
		require.NoError(t, err)
		t.Cleanup(func() { os.RemoveAll(tmpDir) })

		db, err := state.Open(filepath.Join(tmpDir, "state.db"))
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })

		zero := github.RetryBackoff{} // no real sleeps in tests
		d := &Daemon{
			db:             db,
			logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
			vcsProviders:   map[string]vcs.Provider{anvilName: mock},
			prRetryBackoff: &zero,
		}
		d.cfg.Store(&config.Config{})
		return d, db
	}

	t.Run("transient 401 then success: auto-retried, not stranded", func(t *testing.T) {
		mock := &mockVCSProvider{
			createPRFunc: func(call int) (*vcs.PR, error) {
				if call == 1 {
					return nil, fmt.Errorf("failed to create PR: HTTP 401: Bad credentials")
				}
				return &vcs.PR{Number: 42, URL: "https://example.com/pr/42", Title: "t"}, nil
			},
		}
		d, db := newDaemon(t, mock)

		bead := poller.Bead{ID: "FICR-401", Anvil: anvilName, Title: "retry me", ExternalRef: "gh-1"}
		outcome := &pipeline.Outcome{Branch: worktree.BranchName(bead.ID), Iterations: 1}

		d.finalizePipeline(context.Background(), outcome, bead, t.TempDir(), "worker-1")

		require.Equal(t, int32(2), mock.createPRCalls.Load(),
			"CreatePR should be retried once after the transient 401")

		// The bead must NOT be stranded/needs_human after a successful retry.
		r, err := db.GetRetry(bead.ID, bead.Anvil)
		require.NoError(t, err)
		if r != nil {
			assert.False(t, r.NeedsHuman, "bead must not be marked needs_human when CreatePR succeeds on retry")
		}
		// No PR-creation-failed event should have been logged.
		events, err := db.RecentEvents(20)
		require.NoError(t, err)
		for _, ev := range events {
			assert.NotEqual(t, state.EventPRCreationFailed, ev.Type,
				"no PR creation failure should be logged when the retry succeeds")
		}
	})

	t.Run("permanent 422: no retry, immediate needs_human", func(t *testing.T) {
		mock := &mockVCSProvider{
			createPRFunc: func(call int) (*vcs.PR, error) {
				return nil, fmt.Errorf("failed to create PR: HTTP 422: Validation Failed (No commits between main and feature)")
			},
		}
		d, db := newDaemon(t, mock)

		bead := poller.Bead{ID: "FICR-422", Anvil: anvilName, Title: "permanent", ExternalRef: "gh-1"}
		outcome := &pipeline.Outcome{Branch: worktree.BranchName(bead.ID), Iterations: 1}

		d.finalizePipeline(context.Background(), outcome, bead, t.TempDir(), "worker-2")

		require.Equal(t, int32(1), mock.createPRCalls.Load(),
			"a permanent 422 must NOT be retried")

		// The bead must fall through to needs_human immediately.
		r, err := db.GetRetry(bead.ID, bead.Anvil)
		require.NoError(t, err)
		require.NotNil(t, r, "permanent CreatePR failure should mark the bead needs_human")
		assert.True(t, r.NeedsHuman, "permanent CreatePR failure must surface to needs_human")
	})
}

func TestCheckStaleWorkers_StallAndRecoverRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.cfg.Store(&config.Config{})

	const interval = 5 * time.Minute

	// A worker whose log went silent long ago.
	logFile := filepath.Join(tmpDir, "smith.log")
	require.NoError(t, os.WriteFile(logFile, []byte("log"), 0o644))
	old := time.Now().Add(-20 * time.Minute)
	require.NoError(t, os.Chtimes(logFile, old, old))
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-1", BeadID: "BD-1", Anvil: "anvil-1",
		Status: state.WorkerReviewing, Phase: "warden",
		StartedAt: time.Now().Add(-25 * time.Minute), LogPath: logFile,
	}))

	// First pass: the worker should be flagged stalled.
	d.checkStaleWorkers(interval)
	w, err := db.GetWorker("w-1")
	require.NoError(t, err)
	require.Equal(t, state.WorkerStalled, w.Status, "worker should be stalled after first pass")

	// The underlying process resumes and writes to its log.
	now := time.Now()
	require.NoError(t, os.Chtimes(logFile, now, now))

	// Next pass: the worker should be restored to its pre-stall status.
	d.checkStaleWorkers(interval)
	w, err = db.GetWorker("w-1")
	require.NoError(t, err)
	assert.Equal(t, state.WorkerReviewing, w.Status, "worker should return to its prior phase after recovery")
}
