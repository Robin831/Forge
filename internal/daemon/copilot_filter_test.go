package daemon

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newCopilotFilterDaemon builds a minimal Daemon with a fresh state DB and the
// given copilot daily request limit, suitable for exercising
// filterCopilotIfLimited and the auth-escalation dedupe helper.
func newCopilotFilterDaemon(t *testing.T, limit int) (*Daemon, *state.DB) {
	t.Helper()
	tmpDir := t.TempDir()
	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	cfg := &config.Config{
		Settings: config.SettingsConfig{
			CopilotDailyRequestLimit: limit,
		},
	}
	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		authEscalated: make(map[string]bool),
	}
	d.cfg.Store(cfg)
	return d, db
}

func TestFilterCopilotIfLimited(t *testing.T) {
	copilot := provider.Provider{Kind: provider.Copilot}
	claude := provider.Provider{Kind: provider.Claude}

	t.Run("limit disabled returns unchanged", func(t *testing.T) {
		d, _ := newCopilotFilterDaemon(t, 0)
		in := []provider.Provider{copilot, claude}
		out := d.filterCopilotIfLimited(in)
		assert.Equal(t, in, out)
	})

	t.Run("under limit returns unchanged", func(t *testing.T) {
		d, db := newCopilotFilterDaemon(t, 10)
		require.NoError(t, db.AddCopilotRequest(time.Now().Format("2006-01-02"), 3))
		out := d.filterCopilotIfLimited([]provider.Provider{copilot, claude})
		assert.Len(t, out, 2)
	})

	t.Run("over limit filters copilot out", func(t *testing.T) {
		d, db := newCopilotFilterDaemon(t, 5)
		require.NoError(t, db.AddCopilotRequest(time.Now().Format("2006-01-02"), 5))
		out := d.filterCopilotIfLimited([]provider.Provider{copilot, claude})
		require.Len(t, out, 1)
		assert.Equal(t, provider.Claude, out[0].Kind)
	})

	t.Run("empty-list guard: copilot-only over limit keeps copilot", func(t *testing.T) {
		d, db := newCopilotFilterDaemon(t, 5)
		require.NoError(t, db.AddCopilotRequest(time.Now().Format("2006-01-02"), 5))
		in := []provider.Provider{copilot}
		out := d.filterCopilotIfLimited(in)
		// Never hand back zero providers — the original list is returned.
		require.Len(t, out, 1)
		assert.Equal(t, provider.Copilot, out[0].Kind)
		// A copilot_limit_hit event is recorded so the overshoot is visible.
		events, err := db.RecentEvents(10)
		require.NoError(t, err)
		var found bool
		for _, e := range events {
			if e.Type == state.EventCopilotLimitHit {
				found = true
				break
			}
		}
		assert.True(t, found, "expected a copilot_limit_hit event to be recorded")
	})

	t.Run("DB error fails closed: copilot filtered", func(t *testing.T) {
		d, db := newCopilotFilterDaemon(t, 5)
		// Close the DB so GetTodayCopilotRequests errors. The gate must fail
		// CLOSED — assume the limit is reached and filter copilot out.
		require.NoError(t, db.Close())
		out := d.filterCopilotIfLimited([]provider.Provider{copilot, claude})
		require.Len(t, out, 1)
		assert.Equal(t, provider.Claude, out[0].Kind)
	})
}

func TestFirstAuthFailureToday(t *testing.T) {
	d, _ := newCopilotFilterDaemon(t, 0)

	// First failure for a provider today → true (emit the loud alert).
	assert.True(t, d.firstAuthFailureToday("claude/opus"))
	// Repeat for the same provider → false (deduped).
	assert.False(t, d.firstAuthFailureToday("claude/opus"))
	assert.False(t, d.firstAuthFailureToday("claude/opus"))
	// A different provider is tracked independently → true.
	assert.True(t, d.firstAuthFailureToday("gemini/pro"))
	assert.False(t, d.firstAuthFailureToday("gemini/pro"))
}
