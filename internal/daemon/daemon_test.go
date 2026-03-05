package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
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
		cfg: cfg,
		db:  db,
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
