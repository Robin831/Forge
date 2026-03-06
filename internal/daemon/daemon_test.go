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

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/poller"
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
		cfg:           cfg,
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
	}

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
		cfg:           cfg,
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
	}

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

	t.Run("retry_bead: does not clear unrelated needs_human", func(t *testing.T) {
		// Set needs_human via exhausted retries (no circuit breaker prefix).
		const beadID = "NH-BEAD"
		const anvil = "test-anvil"
		err := db.UpsertRetry(&state.RetryRecord{
			BeadID:    beadID,
			Anvil:     anvil,
			NeedsHuman: true,
			LastError: "exhausted retries",
		})
		require.NoError(t, err)

		r, err := db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		require.True(t, r.NeedsHuman)

		// retry_bead should succeed (no DB error) but NOT clear the flag.
		payload, _ := json.Marshal(ipc.RetryBeadPayload{BeadID: beadID, Anvil: anvil})
		resp := d.handleIPC(ipc.Command{
			Type:    "retry_bead",
			Payload: payload,
		})
		assert.Equal(t, "ok", resp.Type)

		r, err = db.GetRetry(beadID, anvil)
		require.NoError(t, err)
		assert.True(t, r.NeedsHuman, "needs_human should remain set for non-circuit-breaker reasons")
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
		Anvils: map[string]config.AnvilConfig{}, // no anvils — poll returns empty
	}

	d := &Daemon{
		cfg:           cfg,
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
	}

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

	// First poll: dispatch should be skipped and event logged once.
	d.pollAndDispatch(context.Background())
	assert.Equal(t, 0, len(d.lastBeads), "no beads should be dispatched")
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
		cfg:           &config.Config{},
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
	}

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

func TestHandleIPC_DismissBead(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		cfg:           &config.Config{},
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
	}

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

func TestHandleIPC_ViewLogs(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-test-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	defer db.Close()

	d := &Daemon{
		cfg:           &config.Config{},
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
	}

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
