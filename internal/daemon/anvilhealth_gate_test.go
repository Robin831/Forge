package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/anvilhealth"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/notify"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/worktree"
)

// wedgedFixture is the dolt_conflicts reply for an anvil that is mid-merge.
const wedgedConflicts = `[{"conflict_table":"issues","conflict_count":3}]`

// stubBdOnPath installs a `bd` on PATH that answers `ready` with the given JSON
// array and everything else (claims, label updates) with success, so a test can
// drive pollAndDispatch without a real beads database.
func stubBdOnPath(t *testing.T, dir, readyJSON string) {
	t.Helper()
	script := filepath.Join(dir, "bd")
	content := "#!/bin/sh\nif [ \"$1\" = \"ready\" ]; then\n  echo '" + readyJSON + "'\n  exit 0\nfi\necho '[]'\nexit 0\n"
	if runtime.GOOS == "windows" {
		script = filepath.Join(dir, "bd.bat")
		content = "@echo off\r\nif \"%1\"==\"ready\" (\r\n  echo " + readyJSON + "\r\n  exit /b 0\r\n)\r\necho []\r\nexit /b 0\r\n"
	}
	require.NoError(t, os.WriteFile(script, []byte(content), 0o755))
	oldPath := os.Getenv("PATH")
	require.NoError(t, os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath))
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
}

// newGateTestDaemon builds a Daemon with a temp state.db and an auto-dispatching
// anvil, wired to the given conflict probe.
func newGateTestDaemon(t *testing.T, runner *scriptedRunner) (*Daemon, *state.DB, string) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "forge-wedge-gate-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	db, err := state.Open(filepath.Join(tmpDir, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	d := &Daemon{
		db:            db,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		runCtx:        context.Background(),
		reqTracker:    *ipc.NewRequestTracker("test-"),
	}
	if runner != nil {
		d.anvilHealth = &anvilhealth.Checker{Run: runner.run}
	}
	d.cfg.Store(&config.Config{
		Settings: config.SettingsConfig{
			MaxTotalSmiths:       1,
			PollInterval:         10 * time.Second,
			CruciblePollInterval: time.Minute,
		},
		Anvils: map[string]config.AnvilConfig{
			"munin": {Path: tmpDir, MaxSmiths: 1, AutoDispatch: "all"},
		},
	})
	return d, db, tmpDir
}

func eventCount(t *testing.T, db *state.DB, kind state.EventType) int {
	t.Helper()
	events, err := db.RecentEvents(200)
	require.NoError(t, err)
	n := 0
	for _, e := range events {
		if e.Type == kind {
			n++
		}
	}
	return n
}

// TestPollAndDispatch_SkipsBeadsInWedgedAnvil pins the dispatch gate itself, not
// just its inputs: a ready bead in a wedged anvil is neither dispatched nor left
// occupying an activeBeads slot. The released slot is the load-bearing half — a
// dropped releaseBeadSlot would leak one slot per skipped bead per poll and
// starve dispatch silently, even after the wedge is resolved.
func TestPollAndDispatch_SkipsBeadsInWedgedAnvil(t *testing.T) {
	runner := &scriptedRunner{conflicts: wedgedConflicts}
	d, db, tmpDir := newGateTestDaemon(t, runner)
	stubBdOnPath(t, tmpDir, `[{"id": "TEST-1", "title": "Test Bead", "status": "ready", "priority": 1}]`)

	d.pollAndDispatch(context.Background(), true)

	rows, err := db.WedgedAnvils()
	require.NoError(t, err)
	require.Len(t, rows, 1, "the full poll must flag the wedged anvil")

	workers, err := db.AllWorkers(10)
	require.NoError(t, err)
	assert.Empty(t, workers, "no work may be dispatched into a wedged anvil")

	_, stillHeld := d.activeBeads.Load("TEST-1")
	assert.False(t, stillHeld, "the skipped bead must release its dispatch slot")

	// Non-vacuity: the same fixture dispatches once the conflicts are resolved,
	// so the assertions above are attributable to the wedge and nothing else.
	runner.set(`[]`, nil)
	d.pollAndDispatch(context.Background(), true)

	rows, err = db.WedgedAnvils()
	require.NoError(t, err)
	require.Empty(t, rows, "the flag must clear once the conflicts are gone")

	workers, err = db.AllWorkers(10)
	require.NoError(t, err)
	assert.NotEmpty(t, workers, "a recovered anvil must dispatch again")

	// Let the dispatched pipeline unwind before the temp dirs are removed.
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("timeout waiting for the dispatched worker to finish")
	}
}

// TestHandleIPC_RunBeadRefusesWedgedAnvil covers the manual-dispatch half of the
// gate — the incident-motivating path, where Hearth silently discarded dispatch
// clicks. The refusal must carry the real reason, record the block, and hand
// back the bead slot claimed earlier in the handler.
func TestHandleIPC_RunBeadRefusesWedgedAnvil(t *testing.T) {
	d, db, _ := newGateTestDaemon(t, nil)

	// The bead is resolvable from the poll cache, so the handler reaches the gate.
	d.lastBeadsMu.Lock()
	d.lastBeads = []poller.Bead{{ID: "TEST-1", Anvil: "munin", Title: "Test Bead"}}
	d.lastBeadsMu.Unlock()

	_, _, err := db.MarkAnvilWedged(state.AnvilHealth{
		Anvil:          "munin",
		Wedged:         true,
		ConflictTables: "issues (3)",
		ConflictCount:  3,
		Detail:         "Beads database is mid-merge with unresolved conflicts. Conflicted tables: issues (3).",
	})
	require.NoError(t, err)

	payload, _ := json.Marshal(ipc.RunBeadPayload{BeadID: "TEST-1", Anvil: "munin"})
	resp := d.handleIPC(ipc.Command{Type: "run_bead", Payload: payload})

	require.Equal(t, "error", resp.Type)
	var msg map[string]string
	require.NoError(t, json.Unmarshal(resp.Payload, &msg))
	assert.Contains(t, msg["message"], "wedged")
	assert.Contains(t, msg["message"], "issues (3)", "the operator must get the real reason, not a bare refusal")

	_, stillHeld := d.activeBeads.Load("TEST-1")
	assert.False(t, stillHeld, "a refused dispatch must release the bead slot it claimed")

	assert.Equal(t, 1, eventCount(t, db, state.EventDispatchBlockedAnvilWedged))

	workers, err := db.AllWorkers(10)
	require.NoError(t, err)
	assert.Empty(t, workers, "a refused dispatch must not spawn a worker")
}

// TestHandleIPC_ForceSmithRefusesWedgedAnvil pins the same gate on force_smith:
// a forced session against a wedged anvil burns tokens and loses every status
// and label write it makes.
func TestHandleIPC_ForceSmithRefusesWedgedAnvil(t *testing.T) {
	d, db, _ := newGateTestDaemon(t, nil)

	// force_smith requires a recorded branch from a previous worker.
	require.NoError(t, db.InsertWorker(&state.Worker{
		ID:        "munin-TEST-1-1",
		BeadID:    "TEST-1",
		Anvil:     "munin",
		Status:    state.WorkerDone,
		Branch:    "forge/TEST-1",
		StartedAt: time.Now(),
	}))
	_, _, err := db.MarkAnvilWedged(state.AnvilHealth{
		Anvil: "munin", Wedged: true, ConflictTables: "issues (3)", ConflictCount: 3,
		Detail: "Beads database is mid-merge with unresolved conflicts. Conflicted tables: issues (3).",
	})
	require.NoError(t, err)

	payload, _ := json.Marshal(ipc.ForceSmithPayload{BeadID: "TEST-1", Anvil: "munin"})
	resp := d.handleIPC(ipc.Command{Type: "force_smith", Payload: payload})

	require.Equal(t, "error", resp.Type)
	var msg map[string]string
	require.NoError(t, json.Unmarshal(resp.Payload, &msg))
	assert.Contains(t, msg["message"], "wedged")

	_, stillHeld := d.activeBeads.Load("TEST-1")
	assert.False(t, stillHeld, "the refusal happens before the slot is claimed")
	assert.Equal(t, 1, eventCount(t, db, state.EventDispatchBlockedAnvilWedged))
	assert.Zero(t, eventCount(t, db, state.EventForceSmith), "a refused force_smith must not be recorded as requested")
}

// TestHandleIPC_PRActionFixVerbsRefuseWedgedAnvil covers the bellows-triggered
// fix workers reachable from Hearth. Each spawns a claude session whose bd
// writes would roll back, consuming a max_*_attempts budget for nothing.
func TestHandleIPC_PRActionFixVerbsRefuseWedgedAnvil(t *testing.T) {
	d, db, _ := newGateTestDaemon(t, nil)
	_, _, err := db.MarkAnvilWedged(state.AnvilHealth{
		Anvil: "munin", Wedged: true, ConflictTables: "issues (3)", ConflictCount: 3,
		Detail: "Beads database is mid-merge with unresolved conflicts. Conflicted tables: issues (3).",
	})
	require.NoError(t, err)

	for _, action := range []string{"quench", "cifix", "burnish", "reviewfix", "rebase"} {
		t.Run(action, func(t *testing.T) {
			payload, _ := json.Marshal(ipc.PRActionPayload{
				Action:   action,
				PRNumber: 7,
				Anvil:    "munin",
				BeadID:   "TEST-1",
				Branch:   "forge/TEST-1",
			})
			resp := d.handleIPC(ipc.Command{Type: "pr_action", Payload: payload})
			require.Equal(t, "error", resp.Type)
			var msg map[string]string
			require.NoError(t, json.Unmarshal(resp.Payload, &msg))
			assert.Contains(t, msg["message"], "wedged")
		})
	}
	assert.Equal(t, 5, eventCount(t, db, state.EventDispatchBlockedAnvilWedged))
}

// TestCheckAnvilHealth_DisabledReleasesExistingFlags pins the other half of the
// config gate. Probing is the only thing that ever clears a flag, so turning the
// check off must first release whatever it raised — otherwise the anvil is
// pinned in needs-attention and `forge status` forever while dispatch, gated on
// the same setting, proceeds as if nothing were wrong.
func TestCheckAnvilHealth_DisabledReleasesExistingFlags(t *testing.T) {
	runner := &scriptedRunner{conflicts: wedgedConflicts}
	d, db, _ := newHealthTestDaemon(t, runner)

	d.checkAnvilHealth(context.Background(), d.cfg.Load())
	rows, err := db.WedgedAnvils()
	require.NoError(t, err)
	require.Len(t, rows, 1)

	disabled := *d.cfg.Load()
	enabled := false
	disabled.Settings.AnvilHealthCheck = &enabled
	d.cfg.Store(&disabled)

	d.checkAnvilHealth(context.Background(), &disabled)

	rows, err = db.WedgedAnvils()
	require.NoError(t, err)
	assert.Empty(t, rows, "a disabled check must not strand the flags it raised")
	assert.Empty(t, d.wedgedAnvilSet(), "the dispatch gate and the read surfaces must agree")
}

// TestCheckAnvilHealth_UnconfiguredAnvilFlagIsReleased covers the sibling case:
// an anvil that is no longer probed (deregistered, or left without a path) must
// not keep a flag alive that nothing can ever clear.
func TestCheckAnvilHealth_UnconfiguredAnvilFlagIsReleased(t *testing.T) {
	runner := &scriptedRunner{conflicts: wedgedConflicts}
	d, db, _ := newHealthTestDaemon(t, runner)

	d.checkAnvilHealth(context.Background(), d.cfg.Load())
	require.Len(t, mustWedged(t, db), 1)

	// Same anvil, path removed: it is skipped by the probe loop, so its row must
	// be pruned rather than left behind.
	pathless := *d.cfg.Load()
	pathless.Anvils = map[string]config.AnvilConfig{"munin": {Path: ""}}
	d.checkAnvilHealth(context.Background(), &pathless)

	assert.Empty(t, mustWedged(t, db), "an unprobeable anvil must not keep a stale flag")
}

func mustWedged(t *testing.T, db *state.DB) []state.AnvilHealth {
	t.Helper()
	rows, err := db.WedgedAnvils()
	require.NoError(t, err)
	return rows
}

// TestCheckAnvilHealth_NotifiesOnWedgeAndRecovery pins that a wedge reaches the
// webhook channel, like every other operator-attention condition (bead_failed,
// orphan_recovery_failed, daily_cost). The motivating incident was precisely a
// silent one: an operator who watches webhooks rather than logs or Hearth heard
// nothing for ~3h40m. Notification is once per wedge, not once per poll.
func TestCheckAnvilHealth_NotifiesOnWedgeAndRecovery(t *testing.T) {
	var mu sync.Mutex
	var got []notify.GenericPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p notify.GenericPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	runner := &scriptedRunner{conflicts: wedgedConflicts}
	d, _, _ := newHealthTestDaemon(t, runner)
	disp := notify.NewWebhookDispatcher([]notify.WebhookTarget{{Name: "test", URL: srv.URL}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NotNil(t, disp)
	d.dispatcher.Store(disp)

	cfg := d.cfg.Load()
	d.checkAnvilHealth(context.Background(), cfg)
	// Drain the dispatcher before triggering the next transition: each delivery
	// is its own goroutine, so without this the wedge POST and the recovery
	// POST race and the recorded order flips on a slow runner. In production
	// the two events are separated by whole poll intervals; the back-to-back
	// calls are a test artifact, so the ordering must be enforced here.
	disp.Wait()
	// A second poll while still wedged must not re-notify.
	d.checkAnvilHealth(context.Background(), cfg)
	disp.Wait()
	runner.set(`[]`, nil)
	d.checkAnvilHealth(context.Background(), cfg)
	disp.Wait()

	mu.Lock()
	defer mu.Unlock()
	var types []string
	for _, p := range got {
		types = append(types, p.EventType)
		assert.Equal(t, "munin", p.Anvil)
	}
	assert.Equal(t, []string{string(notify.EventAnvilWedged), string(notify.EventAnvilRecovered)}, types)
	assert.Contains(t, got[0].Message, "issues (3)", "the notification must carry the conflict detail")
}
