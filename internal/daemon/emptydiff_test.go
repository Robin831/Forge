package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/pipeline"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// emptyDiffDaemon builds a Daemon wired to a temp state DB with an injectable
// bead closer, and returns it alongside a pointer to the recorded close call.
func emptyDiffDaemon(t *testing.T, closeErr error) (*Daemon, *state.DB, *[]string) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var closes []string
	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		beadCloser: func(_ context.Context, beadID, _, reason string) error {
			closes = append(closes, beadID+": "+reason)
			return closeErr
		},
	}
	// No anvils configured: releaseBeadClaim logs and returns without shelling
	// out to bd, which keeps this test hermetic.
	d.cfg.Store(&config.Config{})
	return d, db, &closes
}

func emptyDiffOutcome(action string) *pipeline.Outcome {
	return &pipeline.Outcome{
		EmptyDiff:       true,
		EmptyDiffAction: action,
		EmptyDiffBase:   "origin/main",
		Branch:          "forge/EMPTY-1",
	}
}

func findEvent(t *testing.T, db *state.DB, beadID string, evType state.EventType) (string, bool) {
	t.Helper()
	events, err := db.RecentEvents(50)
	require.NoError(t, err)
	for _, ev := range events {
		if ev.Type == evType && ev.BeadID == beadID {
			return ev.Message, true
		}
	}
	return "", false
}

// TestApplyEmptyDiffOutcome_Close verifies the close action: the bead is closed
// with an explanatory note, the retry record is cleared, and the empty-result
// event is on record.
func TestApplyEmptyDiffOutcome_Close(t *testing.T) {
	d, db, closes := emptyDiffDaemon(t, nil)
	const beadID, anvil = "EMPTY-CLOSE", "test-anvil"

	// A prior dispatch failure must not survive: the outcome is terminal.
	_, _, err := db.IncrementDispatchFailures(beadID, anvil, 10, "prior failure")
	require.NoError(t, err)

	bead := poller.Bead{ID: beadID, Anvil: anvil}
	d.applyEmptyDiffOutcome(context.Background(), bead, t.TempDir(), emptyDiffOutcome(config.EmptyDiffActionClose))

	require.Len(t, *closes, 1, "the bead must be closed exactly once")
	assert.Contains(t, (*closes)[0], "no commits vs origin/main")

	r, err := db.GetRetry(beadID, anvil)
	require.NoError(t, err)
	assert.Nil(t, r, "no retry record may survive an empty-diff outcome")

	msg, ok := findEvent(t, db, beadID, state.EventSmithEmptyResult)
	assert.True(t, ok, "smith_empty_result must be logged")
	assert.Contains(t, msg, "Bead closed")
}

// TestApplyEmptyDiffOutcome_Attention verifies the default action: the bead is
// left open, flagged for the operator, and never counted as a dispatch failure.
func TestApplyEmptyDiffOutcome_Attention(t *testing.T) {
	d, db, closes := emptyDiffDaemon(t, nil)
	const beadID, anvil = "EMPTY-ATTENTION", "test-anvil"

	_, _, err := db.IncrementDispatchFailures(beadID, anvil, 10, "prior failure")
	require.NoError(t, err)

	bead := poller.Bead{ID: beadID, Anvil: anvil}
	d.applyEmptyDiffOutcome(context.Background(), bead, t.TempDir(), emptyDiffOutcome(config.EmptyDiffActionAttention))

	assert.Empty(t, *closes, "attention mode must not close the bead")

	r, err := db.GetRetry(beadID, anvil)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.True(t, r.NeedsHuman, "the bead must surface in Needs Attention")
	assert.Equal(t, 0, r.DispatchFailures, "an empty branch must not feed the circuit breaker")
	assert.Nil(t, r.NextRetry, "no retry may be scheduled for an empty branch")
	assert.Contains(t, r.LastError, "no commits vs origin/main")

	msg, ok := findEvent(t, db, beadID, state.EventSmithEmptyResult)
	assert.True(t, ok, "smith_empty_result must be logged")
	assert.Contains(t, msg, "Needs attention")
}

// TestApplyEmptyDiffOutcome_CloseFailureEscalates verifies that a bead whose
// auto-close fails is escalated rather than silently stranded.
func TestApplyEmptyDiffOutcome_CloseFailureEscalates(t *testing.T) {
	d, db, closes := emptyDiffDaemon(t, errors.New("bd close exploded"))
	const beadID, anvil = "EMPTY-CLOSE-FAIL", "test-anvil"

	bead := poller.Bead{ID: beadID, Anvil: anvil}
	d.applyEmptyDiffOutcome(context.Background(), bead, t.TempDir(), emptyDiffOutcome(config.EmptyDiffActionClose))

	require.Len(t, *closes, 1)

	r, err := db.GetRetry(beadID, anvil)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.True(t, r.NeedsHuman)
	assert.Equal(t, 0, r.DispatchFailures, "a failed auto-close still must not trip the circuit breaker")
	assert.Contains(t, r.LastError, "Auto-close failed")
}

// TestResolveEmptyDiffAction verifies the daemon's config resolution, including
// the conservative fallback for a misconfigured value.
func TestResolveEmptyDiffAction(t *testing.T) {
	d := &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	for _, tc := range []struct {
		configured string
		want       string
	}{
		{"", config.EmptyDiffActionAttention},
		{"attention", config.EmptyDiffActionAttention},
		{"close", config.EmptyDiffActionClose},
		{"CLOSE", config.EmptyDiffActionClose},
		{"typo", config.EmptyDiffActionAttention},
	} {
		cfg := &config.Config{Settings: config.SettingsConfig{EmptyDiffAction: tc.configured}}
		assert.Equal(t, tc.want, d.resolveEmptyDiffAction(cfg), "empty_diff_action=%q", tc.configured)
	}
}
