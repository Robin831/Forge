package daemon

import (
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
		bdContent = "@echo off\r\nif \"%1\"==\"ready\" (\r\n    echo [{\"id\": \"TEST-1\", \"title\": \"Test Bead\", \"status\": \"ready\", \"priority\": 1, \"tags\": [\"test\"]}]\r\n    exit /b 0\r\n)\r\nif \"%1\"==\"update\" (\r\n    echo {\"id\": \"TEST-1\", \"status\": \"in_progress\"}\r\n    exit /b 0\r\n)\r\nexit /b 1\r\n"
	} else {
		bdScript = filepath.Join(tmpDir, "bd")
		bdContent = "#!/bin/sh\nif [ \"$1\" = \"ready\" ]; then\n    echo '[{\"id\": \"TEST-1\", \"title\": \"Test Bead\", \"status\": \"ready\", \"priority\": 1, \"tags\": [\"test\"]}]'\n    exit 0\nfi\nif [ \"$1\" = \"update\" ]; then\n    echo '{\"id\": \"TEST-1\", \"status\": \"in_progress\"}'\n    exit 0\nfi\nexit 1\n"
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
