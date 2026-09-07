package daemon

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

// The "recovery does not overreach" half of the epic: parent acceptance
// criterion (c), the explicitly-flagged regression.
//
// Its sibling (wedge_recovery_test.go) asserts that every shape of a wedged
// worker row reaches a terminal status and hands its dispatch slot back. This
// file asserts the one row that must survive all of that untouched: a bead an
// operator PAUSED, whose parked pipeline goroutine did not outlive the daemon
// and whose recorded pid is long gone, but whose Claude session is intact and
// resumable in place. On 2026-09-07 this host held exactly that row —
// Fhi.Metadata-2p3ck, status=paused, phase=smith, pid=2157387, with no
// /proc/2157387 — and a reaper that had read the pid would have destroyed it.
//
// The two files must not contradict each other, and the only way to make that
// structural rather than hoped for is to read the SAME predicate from both:
// leakVerdictFor (wedge_recovery_test.go) runs the shipped judgeLeak over a real
// state.db row, so "fires for a leaked row" over there and "declines for a
// paused row" here are two answers from one decision table rather than two
// restatements of a rule.
//
// What the survival assertion is made AGAINST is the whole point. It is never
// pid liveness — the marker is the (generation, heartbeat) pair, and the pid is
// a red herring in BOTH directions, which is why
// TestPausedRowDistinguishedByGenerationMarkerNotPid drives all four
// combinations of {paused, running} x {dead pid, live pid} through one pass.

const (
	// pausedRestartBead is the bead parked before the restart. It carries a
	// branch and a session id because those are what the cold-resume path reads
	// back off the surviving row.
	pausedRestartBead = "Forge-paused-restart"

	// pausedRestartSession is the Claude session the paused row records — the
	// thing the whole exemption exists to keep resumable.
	pausedRestartSession = "sess-paused-restart"

	// The two daemon lifetimes. Named rather than derived so the assertions can
	// say which one wrote the row and which one is judging it.
	pausedRestartGenA = "gen-lifetime-that-crashed"
	pausedRestartGenB = "gen-lifetime-after-restart"
)

// deadChildPID returns the pid of a process that genuinely ran and is genuinely
// gone.
//
// Both halves are asserted rather than assumed: a pid that was never alive
// proves nothing about a reaper written not to read pids, and a killed child
// nothing has waited for is a ZOMBIE that still answers kill(pid, 0) — so a
// test that skipped the reap would be asserting against a pid the OS still
// reports as running. e.killWorker does the reap and blocks until processAlive
// is false; the check here is what makes a harness regression fail loudly
// instead of quietly weakening every assertion below it.
func deadChildPID(t *testing.T, e *testEnv) int {
	t.Helper()
	pid, _ := e.spawnDummyWorker(t)
	require.True(t, processAlive(pid), "the child must be running before it is killed")
	e.killWorker(pid)
	require.False(t, processAlive(pid), "the child must be reaped, not merely signalled")
	return pid
}

// liveChildPID returns the pid of a process that is still running, standing in
// for a pid number the OS has RECYCLED onto an unrelated process. The child is
// killed by the harness's own cleanup.
func liveChildPID(t *testing.T, e *testEnv) int {
	t.Helper()
	pid, _ := e.spawnDummyWorker(t)
	require.True(t, processAlive(pid), "the child must be running")
	return pid
}

// stubBdShowOnPath installs a `bd` on PATH that answers `show` with one bead
// record and everything else with an empty array.
//
// The sibling stub (stubBdOnPath) answers `ready`, which is what a dispatch
// test needs; the cold-resume path needs `bd show --id=<id> --json
// --include-dependents`, because coldResumePausedWorker reconstructs the bead
// from bd before it rebuilds the pipeline goroutine. Two stubs rather than one
// widened one: they are fixtures for two different questions, and a stub that
// answers everything hides which call a test actually depends on.
func stubBdShowOnPath(t *testing.T, dir, beadJSON string) {
	t.Helper()
	script := filepath.Join(dir, "bd")
	content := "#!/bin/sh\nif [ \"$1\" = \"show\" ]; then\n  echo '" + beadJSON + "'\n  exit 0\nfi\necho '[]'\nexit 0\n"
	if runtime.GOOS == "windows" {
		script = filepath.Join(dir, "bd.bat")
		content = "@echo off\r\nif \"%1\"==\"show\" (\r\n  echo " + beadJSON + "\r\n  exit /b 0\r\n)\r\necho []\r\nexit /b 0\r\n"
	}
	require.NoError(t, os.WriteFile(script, []byte(content), 0o755))
	oldPath := os.Getenv("PATH")
	require.NoError(t, os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath))
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
}

// workerOwnership reads the two columns the reaper decides on, straight off the
// row, so a test can state what the evidence SAYS before asserting what is done
// with it.
func workerOwnership(t *testing.T, e *testEnv, workerID string) (generation string, heartbeat string) {
	t.Helper()
	err := e.conn().QueryRow(
		`SELECT daemon_generation, heartbeat_at FROM workers WHERE id = ?`, workerID,
	).Scan(&generation, &heartbeat)
	require.NoError(t, err, "reading the ownership columns for %s", workerID)
	return generation, heartbeat
}

// TestPausedBeadWithDeadPidSurvivesDaemonRestart is acceptance criterion (c)
// end to end: a bead parked by one daemon lifetime, with a pid that is provably
// gone, must still be paused after the next lifetime's startup sweep, and must
// still be resumable.
//
// The restart is a real one rather than a planted generation string: lifetime A
// builds the database and writes the row, lifetime A is stopped, and lifetime B
// is opened over the SAME state.db file. Nothing tells B about the row, which
// is exactly the condition — B has to decide from the columns alone.
func TestPausedBeadWithDeadPidSurvivesDaemonRestart(t *testing.T) {
	// ---- lifetime A: park the bead, then die ------------------------------
	first := newTestDaemon(t, withGeneration(pausedRestartGenA))

	pid := deadChildPID(t, first)

	first.seedBead(pausedRestartBead, "in_progress")
	row := first.seedWorkerRow(workerRow{
		BeadID:    pausedRestartBead,
		Status:    state.WorkerPaused,
		Phase:     "smith",
		PID:       pid,
		Branch:    "forge/" + pausedRestartBead,
		SessionID: pausedRestartSession,
		StartedAt: time.Now().Add(-2 * time.Hour),
		// Written by lifetime A and never refreshed since — which is what a
		// crash leaves, and which every ownership signal reads as leaked.
		Generation: first.generation(),
		Heartbeat:  time.Now().Add(-2 * time.Hour),
	})
	dbPath := first.dbPath

	// The crash/restart. Stopping the env early is documented as safe; its
	// registered cleanup becomes a no-op.
	first.stop()

	// ---- lifetime B: comes up over lifetime A's database ------------------
	second := newTestDaemon(t, withDBPath(dbPath), withGeneration(pausedRestartGenB))
	// Registered after the env's own teardown, so it runs BEFORE it: the resume
	// below launches a dispatch goroutine on d.wg, and a goroutine still
	// unwinding when the database is closed is a test failing on its teardown.
	t.Cleanup(func() { drainDispatch(t, second) })

	second.reconfigure(func(c *config.Config) {
		// A positive crucible poll interval keeps any poll this test provokes
		// off the full-poll path, which probes a dolt database it has no
		// business reaching.
		c.Settings.CruciblePollInterval = time.Minute
		c.Anvils = map[string]config.AnvilConfig{
			testAnvil: {Path: second.dir, MaxSmiths: 1, AutoDispatch: "all"},
		}
	})
	stubBdShowOnPath(t, second.dir,
		`{"id": "`+pausedRestartBead+`", "title": "parked bead", "status": "in_progress", "priority": 1}`)

	// The ownership loops, wired as Daemon.Run wires them.
	second.start()

	// ---- (i) the row survives the startup sweep ---------------------------

	// State the evidence first, so what follows is a claim about a row every
	// ownership signal condemns rather than about a row that happened to look
	// healthy.
	gen, beat := workerOwnership(t, second, row.ID)
	require.Equal(t, pausedRestartGenA, gen)
	require.NotEqual(t, second.generation(), gen,
		"the row must carry a lifetime other than the running one — the generation marker's own definition of leaked")
	require.NotEmpty(t, beat)
	require.False(t, processAlive(pid),
		"and its recorded pid must be gone — the signal that must NOT be what decides")

	// Read against the MARKER and not the pid: a paused row is excluded by
	// state.LeakCandidates itself (WorkerBackstopState.NeedsTerminalBackstop as
	// SQL), so it never reaches judgeLeak at all. That structural exclusion is
	// the assertion — a paused row spared by a judgement made further down would
	// be one line of reasoning away from being reaped again.
	//
	// Asked BEFORE the pass, because candidacy is derived from the row's current
	// status: a row the pass had already ended would read as exempt for the
	// wrong reason.
	_, candidate := leakVerdictFor(t, second, row.ID)
	assert.False(t, candidate,
		"a paused row must be exempt structurally, before any ownership evidence is weighed")

	// The startup pass Daemon.Run makes before dispatch begins.
	require.NoError(t, second.d.reapLeakedWorkers(second.ctx))

	status, err := second.db.GetWorkerStatus(row.ID)
	require.NoError(t, err)
	require.Equal(t, state.WorkerPaused, status,
		"a paused row from a lifetime that ended must survive the startup sweep untouched")
	assert.False(t, eventLogged(t, second, state.EventWorkerLeaked, pausedRestartBead),
		"nothing may announce this row as leaked")
	assert.False(t, second.hasLog(slog.LevelWarn, "reaped a leaked worker row"))

	// The production surface an operator sees for the same fact.
	assert.Equal(t, 1, second.d.recoverPausedWorkers(),
		"the paused bead must be surfaced as having survived the restart")
	assert.True(t, eventLogged(t, second, state.EventBeadRecovered, pausedRestartBead))

	// ---- (ii) and it is still resumable -----------------------------------

	// resume_bead with no live control handle is the COLD resume: the parked
	// goroutine died with lifetime A, so handleResumeBead falls back to
	// coldResumePausedWorker, which reconstructs the bead from bd, re-registers
	// a control handle against the SAME worker row (the row id is reused by
	// design — it is what keeps the retained worktree and the recorded session
	// addressable) and launches a fresh dispatch goroutine for it.
	_, live := second.d.lookupControlHandle(pausedRestartBead)
	require.False(t, live, "no parked goroutine may survive a restart — this must be the cold path")

	payload, err := json.Marshal(ipc.ResumeBeadPayload{BeadID: pausedRestartBead})
	require.NoError(t, err)
	resp := second.d.handleIPC(ipc.Command{Type: "resume_bead", Payload: payload})
	require.Equal(t, "ok", resp.Type,
		"the surviving paused row must be resumable after a restart; got %s", steerMsg(t, resp))

	var resumed ipc.ResumeBeadResponse
	require.NoError(t, json.Unmarshal(resp.Payload, &resumed))
	assert.Equal(t, pausedRestartBead, resumed.BeadID)

	// That it took the COLD path specifically is asserted from the two records
	// coldResumePausedWorker writes synchronously, before it launches the
	// goroutine — the warm path logs "resume requested" and emits nothing here.
	// The rebuilt control handle is deliberately NOT asserted: the dispatch
	// releases it on its way out (releaseBeadSlotIfOwner), so its presence is a
	// race against a goroutine this test wants to finish quickly.
	assert.True(t, eventLogged(t, second, state.EventBeadResumed, pausedRestartBead))
	assert.True(t, second.hasLog(slog.LevelInfo, "cold-resuming paused bead after restart"))

	// The dispatch itself: the row leaves 'paused' and is finalised. What the
	// resumed pipeline then does is out of a package test's reach — the anvil
	// here is a bare directory, so worktree creation fails and the pipeline
	// returns at its first step, exactly as the sibling wedge tests stop short
	// of a real Smith session. The load-bearing claim is the one asserted: the
	// bead was accepted for resume, a goroutine ran for it, and the row it left
	// behind is terminal rather than wedged back where it started.
	second.waitFor(t, func() bool {
		s, err := second.db.GetWorkerStatus(row.ID)
		return err == nil && s.IsTerminal()
	}, "the resumed worker row to be finalised")

	status, err = second.db.GetWorkerStatus(row.ID)
	require.NoError(t, err)
	assert.NotEqual(t, state.WorkerPaused, status,
		"a resumed bead must not be left parked — the resume either runs or reports why")
}

// pausedPidRow is one row in the 2x2 below: a status, a pid whose liveness is
// stated, and the outcome the reaper must produce for it.
type pausedPidRow struct {
	name      string
	beadID    string
	status    state.WorkerStatus
	pidIsLive bool
	// wantReaped is the whole point: it tracks the STATUS and never the pid.
	wantReaped bool
}

// TestPausedRowDistinguishedByGenerationMarkerNotPid is the inverse constraint
// of the wedge-recovery reaper tests, stated as a 2x2 so it cannot be satisfied
// by accident.
//
// All four rows are planted with the same ended generation and the same ancient
// heartbeat, and all four go through ONE reap pass. Within each status the two
// rows differ only in whether their recorded pid names a live process:
//
//	paused  + dead pid  -> paused   (the 2026-09-07 production row)
//	paused  + live pid  -> paused   (a recycled pid number is not evidence either)
//	running + dead pid  -> failed
//	running + live pid  -> failed   (a live pid is not evidence of life)
//
// So the outcome is a function of the marker and the status alone, and a reaper
// that started reading pids in either direction fails here — including the
// tempting "spare anything whose process is alive", which would strand every
// row whose pid number had been reused.
func TestPausedRowDistinguishedByGenerationMarkerNotPid(t *testing.T) {
	e := newTestDaemon(t)

	deadPid := deadChildPID(t, e)
	livePid := liveChildPID(t, e)

	rows := []pausedPidRow{
		{name: "paused row whose process is gone", beadID: "Forge-paused-dead",
			status: state.WorkerPaused, pidIsLive: false, wantReaped: false},
		{name: "paused row whose pid was recycled onto a live process", beadID: "Forge-paused-live",
			status: state.WorkerPaused, pidIsLive: true, wantReaped: false},
		{name: "running row whose process is gone", beadID: "Forge-running-dead",
			status: state.WorkerRunning, pidIsLive: false, wantReaped: true},
		{name: "running row whose pid was recycled onto a live process", beadID: "Forge-running-live",
			status: state.WorkerRunning, pidIsLive: true, wantReaped: true},
	}

	ids := make(map[string]string, len(rows))
	for _, tc := range rows {
		pid := deadPid
		if tc.pidIsLive {
			pid = livePid
		}
		seeded := e.seedWorkerRow(workerRow{
			BeadID:    tc.beadID,
			Status:    tc.status,
			Phase:     "smith",
			PID:       pid,
			SessionID: pausedRestartSession,
			StartedAt: time.Now().Add(-2 * time.Hour),
			// Identical ownership evidence on every row: a lifetime that ended
			// and a heartbeat far past workerHeartbeatGrace. Nothing but the
			// status distinguishes the pairs.
			Generation: pausedRestartGenA,
			Heartbeat:  time.Now().Add(-2 * time.Hour),
		})
		ids[tc.beadID] = seeded.ID
	}

	// Both pid claims must still hold at the moment the pass runs, or every
	// assertion below is about something other than what it says.
	require.False(t, processAlive(deadPid), "the dead pid must still be gone")
	require.True(t, processAlive(livePid), "the live pid must still be running")

	// The predicate is read BEFORE the pass and the answers kept, because
	// candidacy is derived from the row's CURRENT status: once the pass has
	// ended a leaked row it is terminal and therefore no longer a candidate, so
	// asking afterwards would report every reaped row as exempt.
	type reading struct {
		verdict   leakVerdict
		candidate bool
	}
	before := make(map[string]reading, len(rows))
	for _, tc := range rows {
		v, candidate := leakVerdictFor(t, e, ids[tc.beadID])
		before[tc.beadID] = reading{verdict: v, candidate: candidate}
	}

	require.NoError(t, e.d.reapLeakedWorkers(e.ctx))

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			id := ids[tc.beadID]

			read := before[tc.beadID]
			if tc.wantReaped {
				require.True(t, read.candidate,
					"a non-paused row must reach the reaper's predicate")
				assert.True(t, read.verdict.leaked,
					"a row from a lifetime that ended, registered by nothing, is leaked: %s", read.verdict.reason)
			} else {
				assert.False(t, read.candidate,
					"a paused row must never reach the predicate at all — the exclusion is in state.LeakCandidates")
			}

			status, err := e.db.GetWorkerStatus(id)
			require.NoError(t, err)
			if tc.wantReaped {
				assert.Equal(t, state.WorkerFailed, status,
					"the marker says no goroutine owns this row, whatever its pid reports")
				assert.True(t, eventLogged(t, e, state.EventWorkerLeaked, tc.beadID))
			} else {
				assert.Equal(t, state.WorkerPaused, status,
					"a parked row's session is resumable in place; its pid says nothing about that")
				assert.False(t, eventLogged(t, e, state.EventWorkerLeaked, tc.beadID))
			}
		})
	}
}
