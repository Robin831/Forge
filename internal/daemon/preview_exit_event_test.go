package daemon

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/kiln"
	"github.com/Robin831/Forge/internal/state"
)

func newPreviewExitDaemon(t *testing.T) (*Daemon, *state.DB) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &Daemon{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, db
}

// previewExitEvents returns the preview service-exit events in the feed, oldest
// first.
func previewExitEvents(t *testing.T, db *state.DB) []state.Event {
	t.Helper()
	all, err := db.RecentEvents(50)
	require.NoError(t, err)
	var out []state.Event
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Type == state.EventPreviewServiceExited {
			out = append(out, all[i])
		}
	}
	return out
}

// TestHandlePreviewServiceExit_EmitsOneEvent — the daemon half of the exit hook.
// Kiln owns the healthy → exited transition; this is the only surface that tells
// an operator about it without them opening the preview panel, so the message has
// to carry all four facts they would otherwise have to infer from a status chip
// changing colour: which service, why it died, which bead, and what the preview
// can still serve.
func TestHandlePreviewServiceExit_EmitsOneEvent(t *testing.T) {
	d, db := newPreviewExitDaemon(t)

	d.handlePreviewServiceExit(kiln.ServiceExit{
		BeadID:   "Forge-abc1",
		Anvil:    "forge",
		Service:  "api",
		ExitCode: intPtr(1),
		Detail:   "exited (exit 1, lived 7m31s)",
		Status:   state.PreviewDegraded,
	})

	events := previewExitEvents(t, db)
	require.Len(t, events, 1)
	assert.Equal(t, "Forge-abc1", events[0].BeadID)
	assert.Equal(t, "forge", events[0].Anvil)
	assert.Contains(t, events[0].Message, `preview service "api"`)
	assert.Contains(t, events[0].Message, "exited (exit 1, lived 7m31s)")
	assert.Contains(t, events[0].Message, "preview is now "+state.PreviewDegraded)
	assert.NotContains(t, events[0].Message, "entry service",
		"a non-entry service's death leaves the preview link alone")
}

// TestHandlePreviewServiceExit_NamesTheEntryService: the entry service is the
// preview's address, so its death is the difference between "part of this is
// broken" and "the link is dead" — and only the event says which, since the
// status is `degraded` either way.
func TestHandlePreviewServiceExit_NamesTheEntryService(t *testing.T) {
	d, db := newPreviewExitDaemon(t)

	d.handlePreviewServiceExit(kiln.ServiceExit{
		BeadID:    "Forge-abc1",
		Anvil:     "forge",
		Service:   "web",
		Entry:     true,
		Detail:    "exited (killed by a signal, lived 2m)",
		Status:    state.PreviewDegraded,
		StartedAt: time.Now().Add(-2 * time.Minute),
		ExitedAt:  time.Now(),
	})

	events := previewExitEvents(t, db)
	require.Len(t, events, 1)
	assert.Contains(t, events[0].Message, `preview service "web"`)
	assert.Contains(t, events[0].Message, "(entry service — no preview URL)")
}

// TestHandlePreviewServiceExit_StripsTerminalEscapes: the service name comes
// from a previewed repo's `.forge/preview.yaml` and the detail can carry a
// process's own wait-error text, while Hearth renders an event message with
// nothing but word-wrapping. So the producer strips, as Assay's event reasons
// do — otherwise a manifest could inject control sequences into every Hearth
// session showing the feed.
func TestHandlePreviewServiceExit_StripsTerminalEscapes(t *testing.T) {
	d, db := newPreviewExitDaemon(t)

	d.handlePreviewServiceExit(kiln.ServiceExit{
		BeadID:  "Forge-abc1",
		Anvil:   "forge",
		Service: "a\x1b[2Kpi",
		Detail:  "exited (\x1b]0;pwn\x07exit 1, lived 7m31s)",
		Status:  state.PreviewDegraded,
	})

	events := previewExitEvents(t, db)
	require.Len(t, events, 1)
	assert.NotContains(t, events[0].Message, "\x1b")
	assert.Contains(t, events[0].Message, `preview service "api"`)
	assert.Contains(t, events[0].Message, "exited (exit 1, lived 7m31s)")
}

// TestHandlePreviewServiceExit_WithoutADBIsANoOp — the hook is wired to the
// manager unconditionally, so it has to survive a daemon with no state DB rather
// than panic on the way to reporting a dead process.
func TestHandlePreviewServiceExit_WithoutADBIsANoOp(t *testing.T) {
	d := &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	assert.NotPanics(t, func() {
		d.handlePreviewServiceExit(kiln.ServiceExit{BeadID: "Forge-abc1", Service: "api"})
	})
}

func intPtr(i int) *int { return &i }
