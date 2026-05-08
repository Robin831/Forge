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
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/schematic"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
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
	bm := bellows.New(db, nil, time.Minute, map[string]string{"test-anvil": tmpDir}, nil, nil)

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
	require.NoError(t, db.UpdatePRMergeability(pr.ID, true, true, false, false, false))

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
	bm := bellows.New(db, nil, time.Minute, map[string]string{"test-anvil": tmpDir}, nil, nil)

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
	require.NoError(t, db.UpdatePRMergeability(pr.ID, true, true, false, false, false))

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
	// openPRs controls what ListOpenPRs and GetPRByHeadBranch return. Used by
	// tests that exercise the ErrPRAlreadyExists recovery path.
	openPRs []vcs.OpenPR
}

func (m *mockVCSProvider) MergePR(_ context.Context, _ string, _ int, _ string) error {
	m.mergeCalls.Add(1)
	return m.mergeErr
}
func (m *mockVCSProvider) CreatePR(_ context.Context, _ vcs.CreateParams) (*vcs.PR, error) {
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
// TestReconcileOpenPRs_RequiresForgeManagedMarker is a regression test for
// Forge-m1ui. Before the fix, reconcileOpenPRs adopted any PR whose body
// merely contained "**Bead**: <id>" — including PR templates, "Closes"
// references, and contributor PRs that mention a bead in the description.
// Adopted PRs were stored with bellows_managed=true (the column default),
// causing Bellows to push review-fix commits and attempt auto-merge against
// PRs Forge had no business touching.
//
// After the fix, only PRs that contain the explicit forge-managed marker
// (vcs.ForgeManagedMarker) are adopted as bellows-managed. PRs that merely
// reference a bead are tracked as ext-<number> with bellows_managed=false.
func TestReconcileOpenPRs_RequiresForgeManagedMarker(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-reconcile-open-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	mock := &mockVCSProvider{
		openPRs: []vcs.OpenPR{
			// A contributor's manual PR that references a bead in the body but
			// was NOT created by Forge — Sophie's PR #3030 in the bug report.
			{
				Number: 3030,
				Title:  "Manual fix for the metadata loader",
				Branch: "sophie/manual-fix",
				Body:   "Fixes the loader.\n\n**Bead**: Fhi.Metadata-tpc00\n",
			},
			// A PR Forge created — body carries the forge-managed marker.
			{
				Number: 4040,
				Title:  "forge: Forge-real",
				Branch: "forge/Forge-real",
				Body:   "## Changes\n\nDid the work.\n\n---\nBead: Forge-real | Branch: forge/Forge-real\n" + vcs.ForgeManagedMarker,
			},
			// A purely external PR with no bead reference at all.
			{
				Number: 5050,
				Title:  "Random external work",
				Branch: "feature/random",
				Body:   "Just some changes.",
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
	})

	d.reconcileOpenPRs(context.Background())

	sophie, err := db.GetPRByNumber("test-anvil", 3030)
	require.NoError(t, err)
	require.NotNil(t, sophie, "Sophie's manual PR should be tracked")
	assert.Equal(t, "ext-3030", sophie.BeadID,
		"PR with only a **Bead**: reference must be tracked as external, not adopted under the referenced bead")
	assert.False(t, sophie.BellowsManaged,
		"PR without the forge-managed marker must NOT be bellows-managed (this is the core fix)")

	real, err := db.GetPRByNumber("test-anvil", 4040)
	require.NoError(t, err)
	require.NotNil(t, real, "Forge's own PR should be tracked")
	assert.Equal(t, "Forge-real", real.BeadID,
		"PR with the forge-managed marker should be tracked under its bead ID")
	assert.True(t, real.BellowsManaged,
		"PR with the forge-managed marker must be bellows-managed")

	random, err := db.GetPRByNumber("test-anvil", 5050)
	require.NoError(t, err)
	require.NotNil(t, random, "external PR with no bead reference should still be tracked")
	assert.Equal(t, "ext-5050", random.BeadID)
	assert.False(t, random.BellowsManaged,
		"PR with no bead reference must not be bellows-managed")
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
}
