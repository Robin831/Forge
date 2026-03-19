package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ingot"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/worktree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestDaemon(t *testing.T) (*Daemon, *state.DB) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "forge-ingot-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

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

	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := state.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
	}
	d.cfg.Store(cfg)
	return d, db
}

func TestHandleIPC_GetIngots_Empty(t *testing.T) {
	d, _ := setupTestDaemon(t)

	payload, _ := json.Marshal(ipc.GetIngotsPayload{})
	resp := d.handleIPC(ipc.Command{
		Type:    "get_ingots",
		Payload: payload,
	})
	assert.Equal(t, "ok", resp.Type)

	var ingots []ingot.Ingot
	err := json.Unmarshal(resp.Payload, &ingots)
	require.NoError(t, err)
	assert.Empty(t, ingots)
}

func TestHandleIPC_GetIngots_WithData(t *testing.T) {
	d, db := setupTestDaemon(t)
	conn := db.Conn()

	// Insert test ingots
	ig1 := &ingot.Ingot{
		BeadID:   "TEST-001",
		Anvil:    "test-anvil",
		WorkerID: "w1",
		Status:   ingot.StatusPROpen,
		Title:    "First ingot",
	}
	ig2 := &ingot.Ingot{
		BeadID:   "TEST-002",
		Anvil:    "other-anvil",
		WorkerID: "w2",
		Status:   ingot.StatusSmith,
		Title:    "Second ingot",
	}
	require.NoError(t, ingot.InsertIngot(conn, ig1))
	require.NoError(t, ingot.InsertIngot(conn, ig2))

	t.Run("no filters returns all", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.GetIngotsPayload{})
		resp := d.handleIPC(ipc.Command{Type: "get_ingots", Payload: payload})
		assert.Equal(t, "ok", resp.Type)

		var ingots []ingot.Ingot
		require.NoError(t, json.Unmarshal(resp.Payload, &ingots))
		assert.Len(t, ingots, 2)
	})

	t.Run("filter by anvil", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.GetIngotsPayload{Anvil: "test-anvil"})
		resp := d.handleIPC(ipc.Command{Type: "get_ingots", Payload: payload})
		assert.Equal(t, "ok", resp.Type)

		var ingots []ingot.Ingot
		require.NoError(t, json.Unmarshal(resp.Payload, &ingots))
		assert.Len(t, ingots, 1)
		assert.Equal(t, "TEST-001", ingots[0].BeadID)
	})

	t.Run("filter by status", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.GetIngotsPayload{Status: "smith"})
		resp := d.handleIPC(ipc.Command{Type: "get_ingots", Payload: payload})
		assert.Equal(t, "ok", resp.Type)

		var ingots []ingot.Ingot
		require.NoError(t, json.Unmarshal(resp.Payload, &ingots))
		assert.Len(t, ingots, 1)
		assert.Equal(t, "TEST-002", ingots[0].BeadID)
	})

	t.Run("filter returns empty for non-matching", func(t *testing.T) {
		payload, _ := json.Marshal(ipc.GetIngotsPayload{Anvil: "nonexistent"})
		resp := d.handleIPC(ipc.Command{Type: "get_ingots", Payload: payload})
		assert.Equal(t, "ok", resp.Type)

		var ingots []ingot.Ingot
		require.NoError(t, json.Unmarshal(resp.Payload, &ingots))
		assert.Empty(t, ingots)
	})
}

func TestHandleIPC_GetIngots_InvalidPayload(t *testing.T) {
	d, _ := setupTestDaemon(t)

	resp := d.handleIPC(ipc.Command{
		Type:    "get_ingots",
		Payload: []byte("not-json"),
	})
	assert.Equal(t, "error", resp.Type)

	var msg map[string]string
	require.NoError(t, json.Unmarshal(resp.Payload, &msg))
	assert.Contains(t, msg["message"], "invalid payload")
}

func TestHandleIPC_GetIngot_Found(t *testing.T) {
	d, db := setupTestDaemon(t)
	conn := db.Conn()

	prNum := 42
	ig := &ingot.Ingot{
		BeadID:       "TEST-001",
		Anvil:        "test-anvil",
		WorkerID:     "w1",
		Status:       ingot.StatusPROpen,
		Title:        "Test ingot with PR",
		Branch:       "forge/TEST-001",
		PRNumber:     &prNum,
		PRURL:        "https://github.com/org/repo/pull/42",
		TemperPassed: true,
	}
	require.NoError(t, ingot.InsertIngot(conn, ig))

	// Insert a test result
	tr := &ingot.TestResult{
		IngotID:    ig.ID,
		StepIndex:  0,
		StepName:   "build",
		Command:    "go build ./...",
		ExitCode:   0,
		DurationMs: 1200,
		Passed:     true,
	}
	require.NoError(t, ingot.InsertTestResult(conn, tr))

	payload, _ := json.Marshal(ipc.GetIngotPayload{BeadID: "TEST-001", Anvil: "test-anvil"})
	resp := d.handleIPC(ipc.Command{Type: "get_ingot", Payload: payload})
	assert.Equal(t, "ok", resp.Type)

	var got ingot.Ingot
	require.NoError(t, json.Unmarshal(resp.Payload, &got))
	assert.Equal(t, "TEST-001", got.BeadID)
	assert.Equal(t, "test-anvil", got.Anvil)
	assert.Equal(t, ingot.StatusPROpen, got.Status)
	assert.NotNil(t, got.PRNumber)
	assert.Equal(t, 42, *got.PRNumber)
	assert.Len(t, got.TestResults, 1)
	assert.Equal(t, "build", got.TestResults[0].StepName)
	assert.True(t, got.TestResults[0].Passed)
}

func TestHandleIPC_GetIngot_NotFound(t *testing.T) {
	d, _ := setupTestDaemon(t)

	payload, _ := json.Marshal(ipc.GetIngotPayload{BeadID: "NONEXISTENT", Anvil: "test-anvil"})
	resp := d.handleIPC(ipc.Command{Type: "get_ingot", Payload: payload})
	assert.Equal(t, "error", resp.Type)

	var msg map[string]string
	require.NoError(t, json.Unmarshal(resp.Payload, &msg))
	assert.Contains(t, msg["message"], "not found")
}

func TestHandleIPC_GetIngot_MissingBeadID(t *testing.T) {
	d, _ := setupTestDaemon(t)

	payload, _ := json.Marshal(ipc.GetIngotPayload{BeadID: ""})
	resp := d.handleIPC(ipc.Command{Type: "get_ingot", Payload: payload})
	assert.Equal(t, "error", resp.Type)

	var msg map[string]string
	require.NoError(t, json.Unmarshal(resp.Payload, &msg))
	assert.Contains(t, msg["message"], "bead_id is required")
}

func TestHandleIPC_GetIngot_SearchAcrossAnvils(t *testing.T) {
	d, db := setupTestDaemon(t)
	conn := db.Conn()

	ig := &ingot.Ingot{
		BeadID:   "TEST-001",
		Anvil:    "test-anvil",
		WorkerID: "w1",
		Status:   ingot.StatusSmith,
		Title:    "Searchable ingot",
	}
	require.NoError(t, ingot.InsertIngot(conn, ig))

	// Search without specifying anvil — should find it via DB lookup.
	payload, _ := json.Marshal(ipc.GetIngotPayload{BeadID: "TEST-001"})
	resp := d.handleIPC(ipc.Command{Type: "get_ingot", Payload: payload})
	assert.Equal(t, "ok", resp.Type)

	var got ingot.Ingot
	require.NoError(t, json.Unmarshal(resp.Payload, &got))
	assert.Equal(t, "TEST-001", got.BeadID)
}

func TestHandleIPC_GetIngot_MultipleAnvils_RequiresDisambiguation(t *testing.T) {
	d, db := setupTestDaemon(t)
	conn := db.Conn()

	// Insert the same bead_id in two different anvils.
	ig1 := &ingot.Ingot{
		BeadID:   "TEST-001",
		Anvil:    "alpha-anvil",
		WorkerID: "w1",
		Status:   ingot.StatusSmith,
		Title:    "First",
	}
	ig2 := &ingot.Ingot{
		BeadID:   "TEST-001",
		Anvil:    "beta-anvil",
		WorkerID: "w2",
		Status:   ingot.StatusSmith,
		Title:    "Second",
	}
	require.NoError(t, ingot.InsertIngot(conn, ig1))
	require.NoError(t, ingot.InsertIngot(conn, ig2))

	// Without --anvil the daemon should report ambiguity, not pick one randomly.
	payload, _ := json.Marshal(ipc.GetIngotPayload{BeadID: "TEST-001"})
	resp := d.handleIPC(ipc.Command{Type: "get_ingot", Payload: payload})
	assert.Equal(t, "error", resp.Type)

	var msg map[string]string
	require.NoError(t, json.Unmarshal(resp.Payload, &msg))
	assert.Contains(t, msg["message"], "multiple anvils")
	assert.Contains(t, msg["message"], "--anvil")
}

func TestHandleIPC_GetIngot_InvalidPayload(t *testing.T) {
	d, _ := setupTestDaemon(t)

	resp := d.handleIPC(ipc.Command{
		Type:    "get_ingot",
		Payload: []byte("{bad"),
	})
	assert.Equal(t, "error", resp.Type)

	var msg map[string]string
	require.NoError(t, json.Unmarshal(resp.Payload, &msg))
	assert.Contains(t, msg["message"], "invalid payload")
}
