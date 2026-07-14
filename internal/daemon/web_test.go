package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
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

func TestSessionDurationEnv(t *testing.T) {
	d := &Daemon{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"empty", "", 0},
		{"unset", "", 0},
		{"valid_hours", "168h", 168 * time.Hour},
		{"valid_minutes", "30m", 30 * time.Minute},
		{"negative", "-5h", -5 * time.Hour},
		{"garbage", "notaduration", 0},
		{"number_without_unit", "168", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envVar := "FORGE_TEST_SESSION_" + tt.name
			if tt.value != "" {
				t.Setenv(envVar, tt.value)
			}
			got := d.sessionDurationEnv(envVar)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHandleIPC_RevokeWebSessions(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-revoke-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	d := &Daemon{
		db:         db,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		runCtx:     context.Background(),
		reqTracker: *ipc.NewRequestTracker("test-"),
	}
	d.cfg.Store(&config.Config{})

	// Seed some sessions.
	now := time.Now().UTC()
	for _, token := range []string{"tok-a", "tok-b", "tok-c"} {
		require.NoError(t, db.CreateWebSession(state.WebSession{
			TokenHash: token,
			Username:  "alice",
			CreatedAt: now,
			ExpiresAt: now.Add(24 * time.Hour),
			LastSeen:  now,
		}))
	}

	resp := d.handleIPC(ipc.Command{Type: "revoke_web_sessions"})
	require.Equal(t, "ok", resp.Type)

	var body struct {
		Revoked int64  `json:"revoked"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(resp.Payload, &body))
	assert.Equal(t, int64(3), body.Revoked)
	assert.Equal(t, "revoked 3 web session(s)", body.Message)

	// Verify audit event was logged.
	events, err := db.RecentEvents(10)
	require.NoError(t, err)
	found := false
	for _, e := range events {
		if e.Type == state.EventWebSessionsRevoked {
			found = true
			assert.Contains(t, e.Message, "revoked 3 web session(s)")
			break
		}
	}
	assert.True(t, found, "expected EventWebSessionsRevoked audit event")
}
