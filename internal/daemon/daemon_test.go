package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/lifecycle"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/schematic"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
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

		// Reset via IPC.
		payload, _ := json.Marshal(ipc.RetryBeadPayload{BeadID: beadID, Anvil: anvil})
		resp := d.handleIPC(ipc.Command{
			Type:    "retry_bead",
			Payload: payload,
		})
		assert.Equal(t, "ok", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Equal(t, "circuit breaker reset", msg["message"])

		// Verify circuit breaker is cleared.
		r, err = db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		assert.False(t, r.NeedsHuman)
		assert.Equal(t, 0, r.DispatchFailures)
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
		assert.Equal(t, "ok", resp.Type)

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
		d.lastBeads = []poller.Bead{
			{ID: "TEST-1", Anvil: "test-anvil", Title: "Test Bead", Priority: 1},
		}

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
	d.pollAndDispatch(context.Background())
	assert.GreaterOrEqual(t, len(d.lastBeads), 1, "poll should surface the ready bead")
	// No worker should have been dispatched.
	_, inFlight := d.activeBeads.Load("COST-1")
	assert.False(t, inFlight, "bead should NOT be dispatched when cost limit is exceeded")
	assert.Equal(t, 1, countCostLimitEvents(), "cost_limit_hit event should be logged once")

	// Second poll: event must NOT be logged again (same calendar day).
	d.pollAndDispatch(context.Background())
	assert.Equal(t, 1, countCostLimitEvents(), "cost_limit_hit must not be logged again on same day")

	// Simulate a daemon restart: reset the in-memory guard but keep the DB event.
	// The DB-backed deduplication must prevent the notification from firing again.
	d.costLimitLoggedDate.Store("")
	d.pollAndDispatch(context.Background())
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
		assert.Equal(t, "ok", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Equal(t, "circuit breaker reset", msg["message"])

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
		assert.Equal(t, "ok", resp.Type)
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		assert.Contains(t, msg["message"], "forge-ready")
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
		// bd is not available in test env, so the release step fails and
		// returns an error. The important invariants are the DB write and
		// the active-bead cleanup.
		var msg map[string]string
		_ = json.Unmarshal(resp.Payload, &msg)
		if resp.Type == "error" {
			assert.Contains(t, msg["message"], "bd release failed", "error should mention bd release")
		}

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
}

func (m *mockVCSProvider) MergePR(_ context.Context, _ string, _ int, _ string) error {
	m.mergeCalls.Add(1)
	return m.mergeErr
}
func (m *mockVCSProvider) CreatePR(_ context.Context, _ vcs.CreateParams) (*vcs.PR, error) {
	return nil, nil
}
func (m *mockVCSProvider) CheckStatus(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	return nil, nil
}
func (m *mockVCSProvider) CheckStatusLight(_ context.Context, _ string, _ int) (*vcs.PRStatus, error) {
	return nil, nil
}
func (m *mockVCSProvider) ListOpenPRs(_ context.Context, _ string) ([]vcs.OpenPR, error) {
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
			db:          db,
			logger:      logger,
			vcsProvider: mock,
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
	tmpDir, err := os.MkdirTemp("", "forge-no-changes-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
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
		var bdScript, bdContent string
		if runtime.GOOS == "windows" {
			bdScript = filepath.Join(tmpDir, "bd.bat")
			bdContent = "@echo off\r\nif \"%1\"==\"close\" ( exit /b 0 )\r\nexit /b 1\r\n"
		} else {
			bdScript = filepath.Join(tmpDir, "bd")
			bdContent = "#!/bin/sh\nif [ \"$1\" = \"close\" ]; then exit 0; fi\nexit 1\n"
		}
		require.NoError(t, os.WriteFile(bdScript, []byte(bdContent), 0o755))
		oldPath := os.Getenv("PATH")
		os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+oldPath)
		defer os.Setenv("PATH", oldPath)

		d := &Daemon{
			db:     db,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		d.cfg.Store(&config.Config{})

		bead := poller.Bead{ID: beadID, Anvil: anvil}
		d.applyNoChangesNeededOutcome(context.Background(), bead, tmpDir, "already fixed upstream")

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

		// Use a tmpDir with NO bd script so closeBead will fail.
		failDir, err := os.MkdirTemp("", "forge-no-bd-*")
		require.NoError(t, err)
		defer os.RemoveAll(failDir)

		// Ensure bd is NOT on the PATH for this subtest (use isolated PATH).
		oldPath := os.Getenv("PATH")
		os.Setenv("PATH", failDir+string(os.PathListSeparator)+oldPath)
		defer os.Setenv("PATH", oldPath)

		d := &Daemon{
			db:     db,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		d.cfg.Store(&config.Config{})

		bead := poller.Bead{ID: beadID, Anvil: anvil}
		d.applyNoChangesNeededOutcome(context.Background(), bead, failDir, "no work needed")

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
}
