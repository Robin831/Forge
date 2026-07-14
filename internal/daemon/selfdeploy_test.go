package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSelfDeployDaemon builds a minimal Daemon with a fresh state DB and the
// given self-deploy config, suitable for exercising the daemon-side gating,
// drain, and pause logic without touching git/go/systemctl.
func newSelfDeployDaemon(t *testing.T, sd config.SelfDeployConfig, anvils map[string]config.AnvilConfig) (*Daemon, *state.DB) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{SelfDeploy: sd, Anvils: anvils}
	d := &Daemon{
		db:     db,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	d.cfg.Store(cfg)
	return d, db
}

func mergedEvent(anvil string, pr int) bellows.PREvent {
	return bellows.PREvent{EventType: bellows.EventPRMerged, Anvil: anvil, PRNumber: pr}
}

func insertPRWithBase(t *testing.T, db *state.DB, anvil string, number int, base string) {
	t.Helper()
	require.NoError(t, db.InsertPR(&state.PR{
		Number:     number,
		Anvil:      anvil,
		BeadID:     "bead-1",
		Branch:     "forge/bead-1",
		BaseBranch: base,
		Status:     state.PRMerged,
		CreatedAt:  time.Now(),
	}))
}

// TestSelfDeployAccepts_BranchAndGating covers the enable/anvil/base-branch
// gating in selfDeployAccepts — in particular that an empty or mismatched
// recorded base branch is treated conservatively as "do not deploy".
func TestSelfDeployAccepts_BranchAndGating(t *testing.T) {
	enabled := config.SelfDeployConfig{Enabled: true, Anvil: "forge", Branch: "main"}

	tests := []struct {
		name    string
		sd      config.SelfDeployConfig
		event   bellows.PREvent
		prBase  string // base branch recorded on the PR ("" = none inserted)
		insert  bool   // whether to insert a PR record at all
		wantYes bool
	}{
		{
			name:    "matching base branch qualifies",
			sd:      enabled,
			event:   mergedEvent("forge", 10),
			prBase:  "main",
			insert:  true,
			wantYes: true,
		},
		{
			name:    "empty recorded base branch does not qualify",
			sd:      enabled,
			event:   mergedEvent("forge", 11),
			prBase:  "",
			insert:  true,
			wantYes: false,
		},
		{
			name:    "mismatched base branch does not qualify",
			sd:      enabled,
			event:   mergedEvent("forge", 12),
			prBase:  "develop",
			insert:  true,
			wantYes: false,
		},
		{
			name:    "missing PR record does not qualify",
			sd:      enabled,
			event:   mergedEvent("forge", 13),
			insert:  false,
			wantYes: false,
		},
		{
			name:    "non-merge event does not qualify",
			sd:      enabled,
			event:   bellows.PREvent{EventType: bellows.EventPRConflicting, Anvil: "forge", PRNumber: 10},
			prBase:  "main",
			insert:  true,
			wantYes: false,
		},
		{
			name:    "disabled does not qualify",
			sd:      config.SelfDeployConfig{Enabled: false, Anvil: "forge"},
			event:   mergedEvent("forge", 10),
			prBase:  "main",
			insert:  true,
			wantYes: false,
		},
		{
			name:    "empty configured anvil does not qualify",
			sd:      config.SelfDeployConfig{Enabled: true, Anvil: ""},
			event:   mergedEvent("forge", 10),
			prBase:  "main",
			insert:  true,
			wantYes: false,
		},
		{
			name:    "different anvil does not qualify",
			sd:      enabled,
			event:   mergedEvent("other", 10),
			prBase:  "main",
			insert:  true,
			wantYes: false,
		},
		{
			name:    "custom branch matches recorded base",
			sd:      config.SelfDeployConfig{Enabled: true, Anvil: "forge", Branch: "release"},
			event:   mergedEvent("forge", 14),
			prBase:  "release",
			insert:  true,
			wantYes: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, db := newSelfDeployDaemon(t, tc.sd, nil)
			if tc.insert {
				insertPRWithBase(t, db, tc.event.Anvil, tc.event.PRNumber, tc.prBase)
			}
			assert.Equal(t, tc.wantYes, d.selfDeployAccepts(tc.event))
		})
	}
}

// TestHandleSelfDeploy_SingleFlight verifies that once a deploy is in flight a
// second qualifying merge event is ignored (the in-flight flag is not reset and
// no second run is launched).
func TestHandleSelfDeploy_SingleFlight(t *testing.T) {
	sd := config.SelfDeployConfig{Enabled: true, Anvil: "forge", Branch: "main"}
	d, db := newSelfDeployDaemon(t, sd, nil)
	insertPRWithBase(t, db, "forge", 20, "main")

	// Simulate a deploy already running.
	require.True(t, d.selfDeployInFlight.CompareAndSwap(false, true))

	// A qualifying event must be a no-op: the flag stays set (still in flight)
	// and no goroutine is launched to clear it.
	d.handleSelfDeploy(context.Background(), mergedEvent("forge", 20))
	assert.True(t, d.selfDeployInFlight.Load(), "in-flight flag must remain set")
}

// TestActiveWorkerCount_CountsActiveAndPaused verifies the drain guard counts
// both active (running/pending/…) and operator-paused workers, since a paused
// worker still holds a worktree and would resume into a running Smith.
func TestActiveWorkerCount_CountsActiveAndPaused(t *testing.T) {
	d, db := newSelfDeployDaemon(t, config.SelfDeployConfig{}, nil)

	n, err := d.activeWorkerCount()
	require.NoError(t, err)
	assert.Equal(t, 0, n, "no workers => zero")

	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-run", BeadID: "b1", Anvil: "forge", Status: state.WorkerRunning, StartedAt: time.Now(),
	}))
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-pause", BeadID: "b2", Anvil: "forge", Status: state.WorkerPaused, StartedAt: time.Now(),
	}))
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-done", BeadID: "b3", Anvil: "forge", Status: state.WorkerDone, StartedAt: time.Now(),
	}))

	n, err = d.activeWorkerCount()
	require.NoError(t, err)
	assert.Equal(t, 2, n, "running + paused count; done excluded")
}

// TestWaitForDrain verifies the drain loop returns true when no workers are
// active and false when the timeout elapses with a worker still active.
func TestWaitForDrain(t *testing.T) {
	d, db := newSelfDeployDaemon(t, config.SelfDeployConfig{}, nil)

	// No workers: drains immediately.
	assert.True(t, d.waitForDrain(context.Background(), time.Second))

	// An active worker never drains within the (tiny) timeout.
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-run", BeadID: "b1", Anvil: "forge", Status: state.WorkerRunning, StartedAt: time.Now(),
	}))
	assert.False(t, d.waitForDrain(context.Background(), time.Nanosecond))
}

// TestWaitForDrain_ContextCancel verifies a cancelled context aborts the drain
// wait even when the timeout has not yet elapsed.
func TestWaitForDrain_ContextCancel(t *testing.T) {
	d, db := newSelfDeployDaemon(t, config.SelfDeployConfig{}, nil)
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-run", BeadID: "b1", Anvil: "forge", Status: state.WorkerRunning, StartedAt: time.Now(),
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.False(t, d.waitForDrain(ctx, time.Hour))
}

// TestRunSelfDeploy_DrainTimeoutDefersAndRestoresPause verifies the pause/drain
// path: when workers do not drain, runSelfDeploy logs a skipped event and undoes
// the pause it introduced (without ever touching git/go/systemctl).
func TestRunSelfDeploy_DrainTimeoutDefersAndRestoresPause(t *testing.T) {
	sd := config.SelfDeployConfig{
		Enabled:      true,
		Anvil:        "forge",
		Branch:       "main",
		DrainTimeout: time.Nanosecond, // never drains before this elapses
	}
	anvils := map[string]config.AnvilConfig{"forge": {Path: t.TempDir()}}
	d, db := newSelfDeployDaemon(t, sd, anvils)

	// An always-active worker guarantees the drain never completes.
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-run", BeadID: "b1", Anvil: "forge", Status: state.WorkerRunning, StartedAt: time.Now(),
	}))

	require.False(t, d.dispatchPaused.Load(), "precondition: not paused")

	d.runSelfDeploy(sd)

	assert.False(t, d.dispatchPaused.Load(), "pause introduced for the deploy must be undone on defer")
	assert.True(t, hasEvent(t, db, state.EventSelfDeploySkipped), "a skipped event must be logged")
}

// TestRunSelfDeploy_DrainTimeoutKeepsPriorPause verifies that when dispatch was
// already paused by an operator, a deferred deploy leaves it paused.
func TestRunSelfDeploy_DrainTimeoutKeepsPriorPause(t *testing.T) {
	sd := config.SelfDeployConfig{
		Enabled:      true,
		Anvil:        "forge",
		Branch:       "main",
		DrainTimeout: time.Nanosecond,
	}
	anvils := map[string]config.AnvilConfig{"forge": {Path: t.TempDir()}}
	d, db := newSelfDeployDaemon(t, sd, anvils)
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-run", BeadID: "b1", Anvil: "forge", Status: state.WorkerRunning, StartedAt: time.Now(),
	}))

	d.dispatchPaused.Store(true) // operator pause predates the deploy

	d.runSelfDeploy(sd)

	assert.True(t, d.dispatchPaused.Load(), "a pre-existing operator pause must survive a deferred deploy")
}

// TestRunSelfDeploy_MissingAnvilAborts verifies runSelfDeploy aborts (with a
// failed event) and does not pause dispatch when the configured anvil is absent.
func TestRunSelfDeploy_MissingAnvilAborts(t *testing.T) {
	sd := config.SelfDeployConfig{Enabled: true, Anvil: "forge", Branch: "main"}
	d, db := newSelfDeployDaemon(t, sd, nil) // no anvils configured

	d.runSelfDeploy(sd)

	assert.False(t, d.dispatchPaused.Load(), "must not pause when it aborts before draining")
	assert.True(t, hasEvent(t, db, state.EventSelfDeployFailed), "a failed event must be logged")
}

func hasEvent(t *testing.T, db *state.DB, typ state.EventType) bool {
	t.Helper()
	events, err := db.RecentEvents(50)
	require.NoError(t, err)
	for _, e := range events {
		if e.Type == typ {
			return true
		}
	}
	return false
}
