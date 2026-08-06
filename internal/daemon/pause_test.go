package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

// newPauseDaemon builds a minimal daemon over a real state.db — enough for the
// pause/resume IPC handlers and the status payload.
func newPauseDaemon(t *testing.T) (*Daemon, *state.DB) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	d := &Daemon{
		db:        db,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		startTime: time.Now(),
	}
	d.cfg.Store(&config.Config{})
	d.runCtx = context.Background()
	// Pre-set pollRunning so the async poll spawned by resume_dispatch no-ops
	// instead of racing with test teardown.
	d.pollRunning.Store(true)
	return d, db
}

func statusPayload(t *testing.T, d *Daemon) ipc.StatusPayload {
	t.Helper()
	resp := d.handleIPC(ipc.Command{Type: "status"})
	require.Equal(t, "status", resp.Type)
	var s ipc.StatusPayload
	require.NoError(t, json.Unmarshal(resp.Payload, &s))
	return s
}

// TestSetDispatchPaused_NormalizesReason verifies the invariant the status line
// depends on: a pause always carries a reason (an unspecified one is manual),
// and resuming clears reason and detail along with the flag.
func TestSetDispatchPaused_NormalizesReason(t *testing.T) {
	d := &Daemon{}

	assert.Equal(t, pauseState{}, d.dispatchPauseState(), "never-set state reads as unpaused")
	assert.False(t, d.dispatchIsPaused())

	d.setDispatchPaused(true, PauseReasonNone, "")
	assert.Equal(t, pauseState{Paused: true, Reason: PauseReasonManual}, d.dispatchPauseState(),
		"a pause with no stated reason is an operator pause")

	d.setDispatchPaused(true, PauseReasonSelfDeploy, "max 30m")
	assert.Equal(t, pauseState{Paused: true, Reason: PauseReasonSelfDeploy, Detail: "max 30m"},
		d.dispatchPauseState())

	d.setDispatchPaused(false, PauseReasonSelfDeploy, "max 30m")
	assert.Equal(t, pauseState{}, d.dispatchPauseState(), "resuming clears reason and detail too")
}

// TestStatus_ReportsManualPauseReason verifies `pause_dispatch` labels itself as
// a manual pause on the wire, and that the boolean is unchanged for clients that
// only know it.
func TestStatus_ReportsManualPauseReason(t *testing.T) {
	d, _ := newPauseDaemon(t)

	s := statusPayload(t, d)
	assert.False(t, s.DispatchPaused)
	assert.Empty(t, s.DispatchPauseReason, "an unpaused daemon reports no reason")

	require.Equal(t, "ok", d.handleIPC(ipc.Command{Type: "pause_dispatch"}).Type)

	s = statusPayload(t, d)
	assert.True(t, s.DispatchPaused, "the boolean stays authoritative")
	assert.Equal(t, ipc.PauseReasonManual, s.DispatchPauseReason)
	assert.Empty(t, s.DispatchPauseDetail)
	assert.Equal(t, "PAUSED (manual) — running workers continue",
		ipc.FormatDispatchPause(s.DispatchPaused, s.DispatchPauseReason, s.DispatchPauseDetail))

	require.Equal(t, "ok", d.handleIPC(ipc.Command{Type: "resume_dispatch"}).Type)
	s = statusPayload(t, d)
	assert.False(t, s.DispatchPaused)
	assert.Empty(t, s.DispatchPauseReason)
}

// TestStatus_ReportsSelfDeployPauseReason verifies a drain pause is reported as
// a self-deploy — the bug this fixes was it reading as "PAUSED (manual)" — and
// that the held-up worker count in the detail is live rather than frozen at the
// moment the pause was taken.
func TestStatus_ReportsSelfDeployPauseReason(t *testing.T) {
	d, db := newPauseDaemon(t)
	d.setDispatchPaused(true, PauseReasonSelfDeploy, "max 30m")

	s := statusPayload(t, d)
	assert.True(t, s.DispatchPaused)
	assert.Equal(t, ipc.PauseReasonSelfDeploy, s.DispatchPauseReason)
	assert.Equal(t, "max 30m", s.DispatchPauseDetail, "no active workers => no waiting clause")

	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-run", BeadID: "b1", Anvil: "forge", Status: state.WorkerRunning, StartedAt: time.Now(),
	}))

	s = statusPayload(t, d)
	assert.Equal(t, "waiting on 1 worker, max 30m", s.DispatchPauseDetail)
	assert.Equal(t, "PAUSED (self-deploy drain, waiting on 1 worker, max 30m) — running workers continue",
		ipc.FormatDispatchPause(s.DispatchPaused, s.DispatchPauseReason, s.DispatchPauseDetail))

	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-run-2", BeadID: "b2", Anvil: "forge", Status: state.WorkerRunning, StartedAt: time.Now(),
	}))
	s = statusPayload(t, d)
	assert.Equal(t, "waiting on 2 workers, max 30m", s.DispatchPauseDetail)
}

// TestRestoreDispatchPause_LegacyStateIsManual verifies backwards compatibility
// with a state.db written before the reason existed: a persisted pause with no
// recorded reason restores as a manual pause rather than an unlabelled one.
func TestRestoreDispatchPause_LegacyStateIsManual(t *testing.T) {
	d, db := newPauseDaemon(t)
	require.NoError(t, db.SetSetting(state.SettingDispatchPaused, "1"))
	require.NoError(t, db.SetSetting(state.SettingDispatchPausedAt, time.Now().Format(time.RFC3339)))
	// Deliberately no SettingDispatchPauseReason — that is the legacy shape.

	d.restoreDispatchPause()

	ps := d.dispatchPauseState()
	assert.True(t, ps.Paused)
	assert.Equal(t, PauseReasonManual, ps.Reason)
}

// TestPauseDispatch_PersistsReason verifies the manual pause persists its reason
// so a restart restores it as manual, and that resuming clears it.
func TestPauseDispatch_PersistsReason(t *testing.T) {
	d, db := newPauseDaemon(t)
	require.Equal(t, "ok", d.handleIPC(ipc.Command{Type: "pause_dispatch"}).Type)

	reason, ok, err := db.GetSetting(state.SettingDispatchPauseReason)
	require.NoError(t, err)
	require.True(t, ok, "the pause reason should be persisted")
	assert.Equal(t, ipc.PauseReasonManual, reason)

	// A restart over the same db restores the pause with its reason intact.
	d2 := &Daemon{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d2.cfg.Store(&config.Config{})
	d2.restoreDispatchPause()
	assert.Equal(t, PauseReasonManual, d2.dispatchPauseState().Reason)

	require.Equal(t, "ok", d.handleIPC(ipc.Command{Type: "resume_dispatch"}).Type)
	reason, _, err = db.GetSetting(state.SettingDispatchPauseReason)
	require.NoError(t, err)
	assert.Empty(t, reason, "resume clears the persisted reason")
}
