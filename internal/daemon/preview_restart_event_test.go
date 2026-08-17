package daemon

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/kiln"
	"github.com/Robin831/Forge/internal/state"
)

// previewRestartEvents returns the preview service-restart events in the feed,
// oldest first.
func previewRestartEvents(t *testing.T, db *state.DB) []state.Event {
	t.Helper()
	all, err := db.RecentEvents(50)
	require.NoError(t, err)
	var out []state.Event
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Type == state.EventPreviewServiceRestarted {
			out = append(out, all[i])
		}
	}
	return out
}

// A death Kiln is about to recover from still gets its event — the window
// between the death and a working relaunch is a window in which nothing is
// serving — but it says what happens next, because "wait" and "act" are
// different instructions to an operator reading the feed.
func TestHandlePreviewServiceExit_AnnouncesAPendingRestart(t *testing.T) {
	d, db := newPreviewExitDaemon(t)

	d.handlePreviewServiceExit(kiln.ServiceExit{
		BeadID:      "Forge-abc1",
		Anvil:       "forge",
		Service:     "client",
		Detail:      "exited (exit 1, lived 7m31s)",
		Status:      state.PreviewDegraded,
		Restarting:  true,
		Restarts:    1,
		MaxRestarts: 3,
	})

	events := previewExitEvents(t, db)
	require.Len(t, events, 1)
	assert.Contains(t, events[0].Message, "restarting (attempt 1 of 3)")
	assert.NotContains(t, events[0].Message, "exhausted")
}

// The death that spends the last attempt is the end of the automatic story and
// the beginning of the operator's, so it reads differently from the ones before
// it.
func TestHandlePreviewServiceExit_AnnouncesAnExhaustedBudget(t *testing.T) {
	d, db := newPreviewExitDaemon(t)

	d.handlePreviewServiceExit(kiln.ServiceExit{
		BeadID:      "Forge-abc1",
		Anvil:       "forge",
		Service:     "client",
		Detail:      "exited (exit 1, lived 12s)",
		Status:      state.PreviewFailed,
		Restarts:    3,
		MaxRestarts: 3,
	})

	events := previewExitEvents(t, db)
	require.Len(t, events, 1)
	assert.Contains(t, events[0].Message, "restart attempts exhausted (3 of 3)")
}

// The restart policy refuses a clean exit before it ever looks at the budget, so
// a service that flapped through its allowance and later exited 0 deliberately
// was not refused for want of attempts. Naming the budget there would tell the
// operator a relaunch was wanted and unaffordable, when none was ever due.
func TestHandlePreviewServiceExit_DoesNotBlameTheBudgetForACleanExit(t *testing.T) {
	d, db := newPreviewExitDaemon(t)
	clean := 0

	d.handlePreviewServiceExit(kiln.ServiceExit{
		BeadID:      "Forge-abc1",
		Anvil:       "forge",
		Service:     "client",
		ExitCode:    &clean,
		Detail:      "exited (exit 0, lived 41m12s)",
		Status:      state.PreviewDegraded,
		Restarts:    3,
		MaxRestarts: 3,
	})

	events := previewExitEvents(t, db)
	require.Len(t, events, 1)
	assert.NotContains(t, events[0].Message, "exhausted")
}

// A service on the default policy — the overwhelming majority — must not grow a
// clause about a feature it did not turn on.
func TestHandlePreviewServiceExit_SaysNothingAboutRestartsByDefault(t *testing.T) {
	d, db := newPreviewExitDaemon(t)

	d.handlePreviewServiceExit(kiln.ServiceExit{
		BeadID:  "Forge-abc1",
		Anvil:   "forge",
		Service: "client",
		Detail:  "exited (exit 1, lived 7m31s)",
		Status:  state.PreviewDegraded,
	})

	events := previewExitEvents(t, db)
	require.Len(t, events, 1)
	assert.NotContains(t, events[0].Message, "restart")
}

// A relaunch that worked is announced too. Without it, the service would go
// from `exited` back to `healthy` with nothing anywhere saying it had died —
// which is precisely the silence the exited state was added to break.
func TestHandlePreviewServiceRestart_EmitsOneEvent(t *testing.T) {
	d, db := newPreviewExitDaemon(t)

	d.handlePreviewServiceRestart(kiln.ServiceRestart{
		BeadID:      "Forge-abc1",
		Anvil:       "forge",
		Service:     "client",
		Attempt:     1,
		MaxRestarts: 3,
		Health:      state.PreviewServiceHealthy,
		Status:      state.PreviewRunning,
		Detail:      "restarted (attempt 1 of 3): healthy",
	})

	events := previewRestartEvents(t, db)
	require.Len(t, events, 1)
	assert.Equal(t, "Forge-abc1", events[0].BeadID)
	assert.Equal(t, "forge", events[0].Anvil)
	assert.Contains(t, events[0].Message, `preview service "client"`)
	assert.Contains(t, events[0].Message, "restarted (attempt 1 of 3): healthy")
	assert.Contains(t, events[0].Message, "preview is now "+state.PreviewRunning)
	assert.NotContains(t, events[0].Message, "no further restarts")
}

// A relaunch that did not come back is the last word for that service, whatever
// the budget still says — so the event says so, rather than leaving "of 3" to
// read as two more tries coming.
func TestHandlePreviewServiceRestart_NamesATerminalFailure(t *testing.T) {
	d, db := newPreviewExitDaemon(t)

	d.handlePreviewServiceRestart(kiln.ServiceRestart{
		BeadID:      "Forge-abc1",
		Anvil:       "forge",
		Service:     "client",
		Attempt:     3,
		MaxRestarts: 3,
		Health:      state.PreviewServiceFailed,
		Err:         errors.New("not healthy within 30s"),
		Status:      state.PreviewFailed,
		Detail:      "restart failed (attempt 3 of 3): not healthy within 30s",
		Exhausted:   true,
	})

	events := previewRestartEvents(t, db)
	require.Len(t, events, 1)
	assert.Contains(t, events[0].Message, "restart failed (attempt 3 of 3)")
	assert.Contains(t, events[0].Message, "(no further restarts)")
}

// Same stripping as the exit event, and for the same reason: the service name
// is a manifest's, and the detail can quote a spawn error naming a command line.
func TestHandlePreviewServiceRestart_StripsTerminalEscapes(t *testing.T) {
	d, db := newPreviewExitDaemon(t)

	d.handlePreviewServiceRestart(kiln.ServiceRestart{
		BeadID:  "Forge-abc1",
		Anvil:   "forge",
		Service: "cli\x1b[2Kent",
		Detail:  "restarted (\x1b]0;pwn\x07attempt 1 of 3): healthy",
		Status:  state.PreviewRunning,
	})

	events := previewRestartEvents(t, db)
	require.Len(t, events, 1)
	assert.NotContains(t, events[0].Message, "\x1b")
	assert.Contains(t, events[0].Message, `preview service "client"`)
	assert.Contains(t, events[0].Message, "restarted (attempt 1 of 3): healthy")
}

// The hook is wired to the manager unconditionally, so it has to survive a
// daemon with no state DB rather than panic on the way to reporting a restart.
func TestHandlePreviewServiceRestart_WithoutADBIsANoOp(t *testing.T) {
	d := &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	assert.NotPanics(t, func() {
		d.handlePreviewServiceRestart(kiln.ServiceRestart{BeadID: "Forge-abc1", Service: "api"})
	})
}
