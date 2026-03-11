package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/lifecycle"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/schematic"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/state"
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
	bm := bellows.New(db, time.Minute, map[string]string{"test-anvil": tmpDir}, nil, nil)

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
	require.NoError(t, db.UpdatePRMergeability(pr.ID, true, true, false, false))

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
}
