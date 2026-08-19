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

// TestActiveWorkerIDs_ReportsActiveAndPaused verifies the drain guard reports
// both active (running/pending/…) and operator-paused workers, since a paused
// worker still holds a worktree and would resume into a running Smith. The IDs
// are what a drain timeout names as holding the deploy up.
func TestActiveWorkerIDs_ReportsActiveAndPaused(t *testing.T) {
	d, db := newSelfDeployDaemon(t, config.SelfDeployConfig{}, nil)

	ids, err := d.activeWorkerIDs()
	require.NoError(t, err)
	assert.Empty(t, ids, "no workers => empty set")

	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-run", BeadID: "b1", Anvil: "forge", Status: state.WorkerRunning, StartedAt: time.Now(),
	}))
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-pause", BeadID: "b2", Anvil: "forge", Status: state.WorkerPaused, StartedAt: time.Now(),
	}))
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-done", BeadID: "b3", Anvil: "forge", Status: state.WorkerDone, StartedAt: time.Now(),
	}))

	ids, err = d.activeWorkerIDs()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"b1", "b2"}, ids, "running + paused reported by bead; done excluded")
}

// TestActiveWorkerIDs_ExcludesBellowsMonitorRows pins the drain guard to work
// that owns a process: bellows' per-PR monitor rows are non-terminal (so
// ActiveWorkers returns them) but hold no PID, no worktree and no pipeline
// goroutine, and they survive for as long as their PR is open. Counting them
// made a PR parked in the fix loop defer every self-deploy for the full
// max_drain_wait (Forge-ti4e).
func TestActiveWorkerIDs_ExcludesBellowsMonitorRows(t *testing.T) {
	d, db := newSelfDeployDaemon(t, config.SelfDeployConfig{}, nil)

	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "bellows-forge-832", BeadID: "b-mon", Anvil: "forge", Status: state.WorkerMonitoring,
		Phase: "bellows", PRNumber: 832, StartedAt: time.Now(),
	}))
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "bellows-forge-901", BeadID: "b-det", Anvil: "forge", Status: state.WorkerDetached,
		Phase: "bellows", PRNumber: 901, StartedAt: time.Now(),
	}))

	ids, err := d.activeWorkerIDs()
	require.NoError(t, err)
	assert.Empty(t, ids, "monitor rows alone must let the deploy through")

	// A real fix worker on the same PR still holds the deploy: it owns a Smith
	// process a restart would kill mid-run.
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "quench-forge-832", BeadID: "b-fix", Anvil: "forge", Status: state.WorkerRunning,
		Phase: "quench", PID: 4242, PRNumber: 832, StartedAt: time.Now(),
	}))

	ids, err = d.activeWorkerIDs()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"b-fix"}, ids, "lifecycle fix workers still drain")
}

// TestRunSelfDeploy_DrainTimeoutDefersAndRestoresPause verifies the pause/drain
// path: when workers do not drain, runSelfDeploy logs a skipped event and undoes
// the pause it introduced (without ever touching git/go/systemctl).
func TestRunSelfDeploy_DrainTimeoutDefersAndRestoresPause(t *testing.T) {
	sd := config.SelfDeployConfig{
		Enabled:      true,
		Anvil:        "forge",
		Branch:       "main",
		MaxDrainWait: time.Nanosecond, // never drains before this elapses
	}
	anvils := map[string]config.AnvilConfig{"forge": {Path: t.TempDir()}}
	d, db := newSelfDeployDaemon(t, sd, anvils)

	// An always-active worker guarantees the drain never completes.
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID: "w-run", BeadID: "b1", Anvil: "forge", Status: state.WorkerRunning, StartedAt: time.Now(),
	}))

	require.False(t, d.dispatchIsPaused(), "precondition: not paused")

	d.runSelfDeploy(sd)

	assert.False(t, d.dispatchIsPaused(), "pause introduced for the deploy must be undone on defer")
	assert.True(t, hasEvent(t, db, state.EventSelfDeploySkipped), "a skipped event must be logged")
}

// TestRunSelfDeploy_DrainTimeoutKeepsPriorPause verifies that when dispatch was
// already paused by an operator, a deferred deploy leaves it paused.
//
// It also pins the deprecated drain_timeout key: an existing config that still
// sets it must keep bounding the wait.
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

	d.setDispatchPaused(true, PauseReasonManual, "") // operator pause predates the deploy

	d.runSelfDeploy(sd)

	assert.True(t, d.dispatchIsPaused(), "a pre-existing operator pause must survive a deferred deploy")
	assert.Equal(t, PauseReasonManual, d.dispatchPauseState().Reason,
		"the operator's pause must be restored as manual, not relabelled as a self-deploy pause")
}

// TestPauseForSelfDeploy_LabelsAndRestores covers the pause a deploy holds for
// its drain: while held it must be labelled a self-deploy — the bug being fixed
// is `forge status` reporting it as "PAUSED (manual)", an operator action nobody
// took — and on restore it must return exactly what it found.
func TestPauseForSelfDeploy_LabelsAndRestores(t *testing.T) {
	t.Run("unpaused daemon", func(t *testing.T) {
		d := &Daemon{}

		restore := d.pauseForSelfDeploy(30 * time.Minute)
		assert.Equal(t, pauseState{Paused: true, Reason: PauseReasonSelfDeploy, Detail: "max 30m0s"},
			d.dispatchPauseState(), "the drain pause names itself and carries its budget")

		restore(false)
		assert.Equal(t, pauseState{}, d.dispatchPauseState(),
			"an unpaused daemon comes back unpaused, with no lingering reason")
	})

	t.Run("operator pause predates the deploy", func(t *testing.T) {
		d := &Daemon{}
		d.setDispatchPaused(true, PauseReasonManual, "")

		restore := d.pauseForSelfDeploy(30 * time.Minute)
		require.Equal(t, PauseReasonSelfDeploy, d.dispatchPauseState().Reason)

		restore(false)
		assert.Equal(t, pauseState{Paused: true, Reason: PauseReasonManual}, d.dispatchPauseState(),
			"the operator's pause is restored as manual, never relabelled")
	})

	t.Run("restart requested keeps the pause", func(t *testing.T) {
		d := &Daemon{}

		restore := d.pauseForSelfDeploy(30 * time.Minute)
		restore(true)
		assert.True(t, d.dispatchIsPaused(),
			"after the binary swap dispatch stays paused until systemd stops the process")
	})
}

// TestRunSelfDeploy_MissingAnvilAborts verifies runSelfDeploy aborts (with a
// failed event) and does not pause dispatch when the configured anvil is absent.
func TestRunSelfDeploy_MissingAnvilAborts(t *testing.T) {
	sd := config.SelfDeployConfig{Enabled: true, Anvil: "forge", Branch: "main"}
	d, db := newSelfDeployDaemon(t, sd, nil) // no anvils configured

	d.runSelfDeploy(sd)

	assert.False(t, d.dispatchIsPaused(), "must not pause when it aborts before draining")
	assert.True(t, hasEvent(t, db, state.EventSelfDeployFailed), "a failed event must be logged")
}

// TestRunSelfDeploy_DeployFailureResumesDispatch verifies the deferred resume
// covers failures after the drain too: with nothing to drain the deploy runs and
// fails at the first step (an unusable repo path), and dispatch must come back.
func TestRunSelfDeploy_DeployFailureResumesDispatch(t *testing.T) {
	sd := config.SelfDeployConfig{
		Enabled:  true,
		Anvil:    "forge",
		Branch:   "main",
		RepoPath: filepath.Join(t.TempDir(), "does-not-exist"),
	}
	anvils := map[string]config.AnvilConfig{"forge": {Path: t.TempDir()}}
	d, db := newSelfDeployDaemon(t, sd, anvils)

	d.runSelfDeploy(sd)

	assert.False(t, d.dispatchIsPaused(), "a failed deploy must not leave dispatch paused")
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
