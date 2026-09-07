package daemon

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

// The two REPORTING halves of the epic: acceptance criteria (d) and (e).
//
// Its siblings assert that a wedged worker row is recovered (wedge_recovery_test.go)
// and that a parked one is not (paused_survives_restart_test.go). This file asks
// the question an operator actually asks first, which neither of those answers:
// what does Forge SAY while any of that is happening.
//
//   - (d) `forge status` must never report an active worker whose process does
//     not exist. The claim is bounded and stated as such below: it is honest
//     once the daemon's own recovery has run, and each test drives the
//     production recovery rather than asserting the end state into existence.
//   - (e) the WARN that separates a wedged holder of the global limit from an
//     ordinary busy forge must fire for the first and stay silent for the
//     second, and its threshold must be the SHIPPED one.
//
// Both are driven through real entry points over a real state.db: the `status`
// and `workers` IPC handlers for (d) — which is where `forge status`'s number
// comes from, since cmd/forge renders StatusPayload.Workers and nothing else —
// and pollAndDispatch for (e), whose capacity gate is the only caller of
// reportGlobalLimitReached.

const (
	// statusDeadBead / statusLiveBead name the two rows the honesty tests
	// distinguish between. Separate beads rather than two rows for one, so the
	// assertions can address them the way an operator reads a status listing.
	statusDeadBead = "Forge-status-dead"
	statusLiveBead = "Forge-status-live"

	// statusStaleInterval is the stale_interval the detector passes run at, on
	// the sibling recovery test's argument: the seeded rows are silent by any
	// threshold, so the value is one a real deployment might hold rather than
	// one tuned to make the test pass.
	statusStaleInterval = time.Minute

	// statusRecentWindow is the recent_seconds the `workers` listing is asked
	// for. A terminal row leaves ActiveWorkers the moment it is finalised, so
	// this is what lets the test assert that the dead-pid row is REPORTED as
	// terminal rather than merely absent — "gone from the listing" and
	// "reported as failed" are different claims and only the second says the
	// operator was told what happened.
	statusRecentWindow = 3600

	// wedgedLimitWARN is the message the wedged-holder escalation carries. It
	// is matched as a substring of the shipped line (limitholder.go) rather
	// than restated in full: the assertion is about the escalation firing once,
	// not about its wording, and the distinctive clause is what an operator
	// greps for.
	wedgedLimitWARN = "it may be wedged"

	// limitHeldBead is the bead whose row holds the single global slot in the
	// (e) tests, and limitRotatingBead the prefix for the rows a HEALTHY
	// saturated forge turns over.
	limitHeldBead     = "Forge-limit-holder"
	limitRotatingBead = "Forge-limit-rotating"
)

// ---------------------------------------------------------------------------
// (d) forge status honesty
// ---------------------------------------------------------------------------

// The status assembly these tests bind to is statusPayload (pause_test.go),
// which drives the real `status` IPC handler over the daemon under test — not a
// second copy of it here. That handler is what `forge status` renders:
// cmd/forge/status.go prints StatusPayload.Workers verbatim over IPC, and its
// no-daemon fallback reads db.ActiveWorkers(), the very call the handler makes.
// So binding here covers both renderings without a package-main test that could
// only re-implement one.

// listedWorkers drives the real `workers` IPC handler — the per-row listing
// behind the status command's "Active Workers" section and both dashboards.
//
// recentSeconds > 0 asks for recently-finished rows as well, which is the only
// way to read what a row that has just been recovered is being REPORTED as.
func listedWorkers(t *testing.T, e *testEnv, recentSeconds int) []ipc.WorkerInfo {
	t.Helper()
	cmd := ipc.Command{Type: "workers"}
	if recentSeconds > 0 {
		payload, err := json.Marshal(map[string]int{"recent_seconds": recentSeconds})
		require.NoError(t, err)
		cmd.Payload = payload
	}
	resp := e.d.handleIPC(cmd)
	require.Equal(t, "ok", resp.Type, "the workers handler must answer")
	var out ipc.WorkersResponse
	require.NoError(t, json.Unmarshal(resp.Payload, &out))
	return out.Workers
}

// findListedWorker returns the listing entry for a worker row id.
func findListedWorker(t *testing.T, listed []ipc.WorkerInfo, workerID string) (ipc.WorkerInfo, bool) {
	t.Helper()
	for _, w := range listed {
		if w.ID == workerID {
			return w, true
		}
	}
	return ipc.WorkerInfo{}, false
}

// newStatusEnv is a harness env wired for the honesty tests: a stale interval
// the detector can act on, and nothing else. There is deliberately no anvil and
// no `bd` stub — these tests never poll, because what `forge status` reports is
// a question about state.db and the handlers over it, and an env that could
// dispatch would let a background row wander into the count being asserted.
func newStatusEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestDaemon(t)
	e.reconfigure(func(c *config.Config) {
		c.Settings.StaleInterval = statusStaleInterval
	})
	return e
}

// freshLogFile writes a log file whose mtime is now, and returns its path.
//
// It is what makes a seeded row read as a worker that is PRODUCING output:
// state.StalledWorkers stats the log path, so a row without one that started
// before the cutoff is stale by definition. A live worker in these tests has to
// be genuinely non-stale, or the detector would mask it and the "the live one
// is still reported" assertion would be about a row the pass had also touched.
func freshLogFile(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("{\"type\":\"assistant\"}\n"), 0o644))
	return path
}

// seedDeadPidWorker plants the row the honesty tests are about: a worker
// claiming a Smith session whose process is provably gone.
//
// Phase `smith` and a real, reaped pid are both load-bearing. The pid column
// names the last process the row's PHASE spawned, so it is dead by design
// everywhere else (staleliveness.go); only in `smith`/`schematic` does it name
// the session the row is IN, which is what makes a dead one evidence of
// anything at all.
func seedDeadPidWorker(t *testing.T, e *testEnv, generation string, heartbeat time.Time) (workerRow, int) {
	t.Helper()
	pid := deadChildPID(t, e)
	e.seedBead(statusDeadBead, "in_progress")
	row := e.seedWorkerRow(workerRow{
		BeadID: statusDeadBead,
		Status: state.WorkerRunning,
		Phase:  "smith",
		PID:    pid,
		// An hour old with no log file: silent by any stale_interval.
		StartedAt:  time.Now().Add(-time.Hour),
		Generation: generation,
		Heartbeat:  heartbeat,
	})
	require.False(t, processAlive(pid), "the seeded row's process must be gone, not merely signalled")
	return row, pid
}

// TestStatusNeverReportsActiveWorkerWithDeadProcess is acceptance criterion (d).
//
// The claim it makes is deliberately bounded, and the pre-assertion in each
// subtest is what bounds it: a row whose process died a moment ago IS reported
// as active until something observes the death, because nothing in the status
// path probes a pid — and nothing should, since a pid is not evidence in either
// direction (see reaper.go). What must hold is that the daemon's own recovery
// makes the report honest, and that recovery is driven here through the
// production path rather than simulated by writing a terminal status.
//
// Both recoveries are covered because "a worker whose process is gone" reaches
// status through two different mechanisms, and status has to end up honest
// whichever one owns the row: the stale detector's liveness escalation for a
// row of the RUNNING daemon lifetime, and the leaked-worker reaper for one left
// behind by a lifetime that ended.
//
// Without the pre-assertion each subtest would pass over a status path that
// never reported the row at all, which is a different (and false) claim.
func TestStatusNeverReportsActiveWorkerWithDeadProcess(t *testing.T) {
	t.Run("recovered by the stale detector's liveness escalation", func(t *testing.T) {
		e := newStatusEnv(t)

		// Of the running generation with a fresh heartbeat — the honest
		// description of a row whose daemon is still alive, which keeps the
		// reaper out of this subtest so the recovery under test is the one
		// named.
		row, pid := seedDeadPidWorker(t, e, e.generation(), time.Now())

		verdict, candidate := leakVerdictFor(t, e, row.ID)
		require.True(t, candidate, "a running row must reach the reaper's predicate at all")
		require.False(t, verdict.leaked,
			"the reaper must not claim this row (its daemon is alive): %s", verdict.reason)

		require.Equal(t, 1, statusPayload(t, e.d).Workers,
			"nothing probes a pid on the status path: until the death is OBSERVED the row is reported active")

		// The production recovery. Two passes by design (staleliveness.go): one
		// sighting of a dead pid is also what a healthy worker looks like for
		// the width of a process launch.
		e.d.checkStaleWorkers(statusStaleInterval)
		e.d.checkStaleWorkers(statusStaleInterval)

		assertStatusHonest(t, e, row.ID, pid)
	})

	t.Run("recovered by the leaked-worker reaper", func(t *testing.T) {
		e := newStatusEnv(t)

		// A lifetime that ended, and a heartbeat far past workerHeartbeatGrace:
		// no goroutine in this process can own the row, which is a proof rather
		// than an inference and is decided without reading the pid.
		row, pid := seedDeadPidWorker(t, e, "gen-lifetime-that-crashed", time.Now().Add(-2*time.Hour))

		verdict, candidate := leakVerdictFor(t, e, row.ID)
		require.True(t, candidate)
		require.True(t, verdict.leaked,
			"a row from a lifetime that ended, registered by nothing, is leaked: %s", verdict.reason)

		require.Equal(t, 1, statusPayload(t, e.d).Workers,
			"a row left by a dead daemon is reported active until the reaper looks at it")

		require.NoError(t, e.d.reapLeakedWorkers(e.ctx))

		assertStatusHonest(t, e, row.ID, pid)
	})
}

// assertStatusHonest is the shared post-condition of both recoveries: the row
// is gone from every active count `forge status` renders, and it is REPORTED as
// terminal rather than merely omitted.
//
// The pid is re-checked at the end because every assertion above it is a claim
// about a row whose process is gone, and a pid the OS had recycled onto
// something else in the meantime would quietly make them claims about
// something else.
func assertStatusHonest(t *testing.T, e *testEnv, workerID string, pid int) {
	t.Helper()

	payload := statusPayload(t, e.d)
	assert.Zero(t, payload.Workers,
		"forge status must not count a worker whose process does not exist")

	active := listedWorkers(t, e, 0)
	_, listedActive := findListedWorker(t, active, workerID)
	assert.False(t, listedActive,
		"the dead-pid row must not appear in the active worker listing either")

	// Reported, not merely dropped. The recent window is what an operator's
	// dashboard asks for, and it is where the row has to say what became of it.
	recent := listedWorkers(t, e, statusRecentWindow)
	entry, found := findListedWorker(t, recent, workerID)
	require.True(t, found,
		"the row must still be reported, so the operator is told what happened to it")
	assert.Equal(t, string(state.WorkerFailed), entry.Status,
		"a worker whose process is gone must be reported as terminal, not as active")

	status, err := e.db.GetWorkerStatus(workerID)
	require.NoError(t, err)
	assert.True(t, status.IsTerminal(),
		"the underlying row must be terminal, not masked with a recoverable status")

	require.False(t, processAlive(pid),
		"the pid must still be gone, or every assertion above is about a different process")
}

// TestStatusReportsLiveWorkerAlongsideDeadOne is the variant that keeps the
// honesty claim from being satisfied by a blanket sweep.
//
// Two rows, identical in every respect the recovery reads except the two that
// matter — one names a live process and is writing its log, the other names a
// reaped one and has been silent for an hour — go through ONE pair of detector
// passes. Status must come out reporting exactly the live one.
//
// A regression that failed every silent row, or every row in phase `smith`, or
// simply everything on a pass that found one dead worker, fails here rather
// than passing faster.
func TestStatusReportsLiveWorkerAlongsideDeadOne(t *testing.T) {
	e := newStatusEnv(t)

	deadRow, deadPid := seedDeadPidWorker(t, e, e.generation(), time.Now())

	livePid := liveChildPID(t, e)
	e.seedBead(statusLiveBead, "in_progress")
	liveRow := e.seedWorkerRow(workerRow{
		BeadID: statusLiveBead,
		Status: state.WorkerRunning,
		Phase:  "smith",
		PID:    livePid,
		// Started just as long ago as the dead row, so age is not what
		// separates them — the log is.
		StartedAt:  time.Now().Add(-time.Hour),
		LogPath:    freshLogFile(t, e.dir, "live-worker.log"),
		Generation: e.generation(),
		Heartbeat:  time.Now(),
	})

	require.Equal(t, 2, statusPayload(t, e.d).Workers,
		"both rows must be reported before anything observes either process")

	e.d.checkStaleWorkers(statusStaleInterval)
	e.d.checkStaleWorkers(statusStaleInterval)

	// Both pid claims must still hold at the moment the assertions are made.
	require.False(t, processAlive(deadPid), "the dead row's process must still be gone")
	require.True(t, processAlive(livePid), "the live row's process must still be running")

	payload := statusPayload(t, e.d)
	assert.Equal(t, 1, payload.Workers,
		"exactly the worker whose process exists must be reported active")

	active := listedWorkers(t, e, 0)
	require.Len(t, active, 1)
	assert.Equal(t, liveRow.ID, active[0].ID,
		"and it must be the LIVE row, matched by id rather than by count")
	assert.Equal(t, string(state.WorkerRunning), active[0].Status,
		"a worker that is writing its log must not be masked by a pass that failed its neighbour")

	deadStatus, err := e.db.GetWorkerStatus(deadRow.ID)
	require.NoError(t, err)
	assert.Equal(t, state.WorkerFailed, deadStatus,
		"the dead-pid row must still reach a terminal status with a healthy sibling beside it")
	assert.True(t, eventLogged(t, e, state.EventWorkerProcessGone, statusDeadBead))
	assert.False(t, eventLogged(t, e, state.EventWorkerProcessGone, statusLiveBead),
		"nothing may be announced about the worker that is working")
}

// ---------------------------------------------------------------------------
// (e) the wedged-limit-holder WARN
// ---------------------------------------------------------------------------

// countWarns reports how many captured WARN records carry substr.
//
// It counts rather than tests for presence because the whole difficulty of this
// escalation is the SECOND one: a condition that persists is re-observed on
// every poll, and a check that announced itself each time would be the
// re-announcement failure depcheck's blocked-scan signature and the smelter's
// contradiction announcer both exist to prevent. "At least one" would pass for
// a WARN emitted every poll interval forever.
func countWarns(e *testEnv, substr string) int {
	n := 0
	for _, rec := range e.logs() {
		if rec.Level == slog.LevelWarn && strings.Contains(rec.Message, substr) {
			n++
		}
	}
	return n
}

// newLimitEnv is a harness env wired for the one question the (e) tests ask:
// what does the daemon SAY about a full global limit.
//
// max_total_smiths is 1, so a single seeded row fills it and the poll's
// capacity gate — the only caller of reportGlobalLimitReached — is reached on
// every cycle. `bd ready` answers with nothing: these tests are about the gate
// and its log lines, and a bead that could be dispatched the moment the limit
// clears would put a worker row the test never planted into the holder set the
// escalation keys on.
func newLimitEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestDaemon(t, withWorkerLimit(1))
	e.reconfigure(func(c *config.Config) {
		// A positive crucible poll interval is what makes a FAST poll legal:
		// pollAndDispatch promotes every poll to a full one below it, and a
		// full poll runs the anvil-health probe against a real dolt database
		// this test has no business reaching.
		c.Settings.CruciblePollInterval = time.Minute
		c.Anvils = map[string]config.AnvilConfig{
			testAnvil: {Path: e.dir, MaxSmiths: 1, AutoDispatch: "all"},
		}
	})
	stubBdOnPath(t, e.dir, "[]")
	t.Cleanup(func() { drainDispatch(t, e) })
	return e
}

// wedgedLimitThreshold is the SHIPPED consecutive-poll threshold, read back off
// the daemon under test rather than written down here.
//
// The env leaves settings.wedged_limit_polls unset, so this resolves through
// the same tri-state the daemon uses (0 means unset and takes
// config.DefaultWedgedLimitPolls; only a negative value disables). Reading it
// is what makes the boundary assertions below pin the policy rather than a
// number a test chose — retuning the default moves these tests with it, and
// disabling the check by accident fails them outright.
func wedgedLimitThreshold(t *testing.T, e *testEnv) int {
	t.Helper()
	threshold, enabled := e.d.config().Settings.ResolvedWedgedLimitPolls()
	require.True(t, enabled, "the wedged-holder escalation must be on by default")
	require.Positive(t, threshold)
	return threshold
}

// holdGlobalLimit plants one running row that fills max_total_smiths = 1.
func holdGlobalLimit(t *testing.T, e *testEnv, beadID string) workerRow {
	t.Helper()
	e.seedBead(beadID, "in_progress")
	row := e.seedWorkerRow(workerRow{
		BeadID:     beadID,
		Status:     state.WorkerRunning,
		Phase:      "smith",
		StartedAt:  time.Now().Add(-90 * time.Minute),
		Generation: e.generation(),
		Heartbeat:  time.Now(),
	})
	require.Len(t, slotHolders(t, e), 1, "the seeded row must hold the only dispatch slot")
	return row
}

// setWorkerStatus flips a seeded row's status, which is how a test makes a
// worker take or hand back the global slot between polls.
//
// Written straight to the row for the same reason seedWorkerRow is: the
// production transitions carry their own bookkeeping (completed_at, prev_status,
// the ownership columns), and a test that reached for one of them would be
// asserting about that path rather than about the poll gate.
func setWorkerStatus(t *testing.T, e *testEnv, workerID string, status state.WorkerStatus) {
	t.Helper()
	res, err := e.conn().Exec(`UPDATE workers SET status = ? WHERE id = ?`, string(status), workerID)
	require.NoError(t, err, "updating the status of %s", workerID)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "expected to update exactly the row %s", workerID)
}

// TestWarnFiresForWorkerHoldingGlobalLimitAcrossPolls is acceptance criterion
// (e)'s positive half, driven through the production poll cycle.
//
// One worker holds the only dispatch slot across consecutive polls and never
// moves. The threshold is the shipped one, so the boundary asserted here is the
// policy Forge actually runs: the poll ON the threshold is still quiet (a busy
// forge that has been busy for three cycles is just busy) and the poll PAST it
// escalates.
//
// The counts are exact at every step. `assert.Empty` before the crossing would
// pass for a check that announced early on some other poll, and "at least one"
// after it would pass for one that re-announces on every poll interval forever
// — which is the failure mode that makes an escalation worthless.
func TestWarnFiresForWorkerHoldingGlobalLimitAcrossPolls(t *testing.T) {
	e := newLimitEnv(t)
	threshold := wedgedLimitThreshold(t, e)

	row := holdGlobalLimit(t, e, limitHeldBead)

	// N-1 polls quiet: the limit is full on every one of them, and the INFO
	// line saying so is the whole report while the run is within the threshold.
	for poll := 1; poll <= threshold; poll++ {
		e.d.pollAndDispatch(e.ctx, false)
		require.Zero(t, countWarns(e, wedgedLimitWARN),
			"poll %d of %d is still within the shipped threshold", poll, threshold)
	}
	assert.True(t, e.hasLog(slog.LevelInfo, "global smith limit reached, skipping dispatch"),
		"ordinary saturation must still be reported at INFO while nothing is escalated")

	// N polls warn.
	e.d.pollAndDispatch(e.ctx, false)
	require.Equal(t, 1, countWarns(e, wedgedLimitWARN),
		"the poll past the threshold must escalate the holder exactly once")

	// The WARN has to be actionable on its own, or the operator is sent back to
	// the dashboard for the three things it already knew.
	rec, ok := e.findLog(slog.LevelWarn, wedgedLimitWARN)
	require.True(t, ok)
	assert.Equal(t, row.ID, rec.Attrs["worker"])
	assert.Equal(t, limitHeldBead, rec.Attrs["bead"])
	assert.Equal(t, testAnvil, rec.Attrs["anvil"])
	assert.Equal(t, "smith", rec.Attrs["phase"])
	// int64 because slog stores an int attribute as an Int64 value; the cast is
	// on the expectation rather than on what was captured, so the assertion
	// reads against the record as it was actually logged.
	assert.Equal(t, int64(threshold+1), rec.Attrs["polls"])
	assert.Equal(t, int64(threshold), rec.Attrs["threshold"])

	// And it stays at exactly one while the condition persists: the scan
	// repeats every poll, so an unsuppressed escalation would re-announce the
	// same wedged worker for as long as it is wedged.
	for poll := 0; poll < 5; poll++ {
		e.d.pollAndDispatch(e.ctx, false)
	}
	assert.Equal(t, 1, countWarns(e, wedgedLimitWARN),
		"a wedged worker must be announced once, not on every poll interval forever")
}

// TestNoWarnForTransientSaturation is (e)'s negative half: the three shapes of
// a forge that is merely busy, each driven through the same production poll
// cycle as the test above.
//
// They are three rather than one because "transient saturation" is not one
// condition, and a check keyed on the blocked condition instead of on identity
// would pass the first and fail the other two.
func TestNoWarnForTransientSaturation(t *testing.T) {
	t.Run("saturated for fewer polls than the threshold", func(t *testing.T) {
		e := newLimitEnv(t)
		threshold := wedgedLimitThreshold(t, e)

		holdGlobalLimit(t, e, limitHeldBead)

		// The boundary from below, derived from the same constant the test
		// above crosses: exactly threshold consecutive polls stay quiet, so the
		// pair pins the policy rather than each pinning a number of its own.
		for poll := 1; poll <= threshold; poll++ {
			e.d.pollAndDispatch(e.ctx, false)
		}
		assert.Zero(t, countWarns(e, wedgedLimitWARN),
			"a forge saturated for %d consecutive polls is busy, not wedged", threshold)
	})

	t.Run("saturation interrupted before the threshold resets the run", func(t *testing.T) {
		e := newLimitEnv(t)
		threshold := wedgedLimitThreshold(t, e)

		row := holdGlobalLimit(t, e, limitHeldBead)

		for poll := 1; poll <= threshold; poll++ {
			e.d.pollAndDispatch(e.ctx, false)
		}
		require.Zero(t, countWarns(e, wedgedLimitWARN))

		// The slot comes back for one cycle, which is the poll that calls
		// limitHolders.release(): the run the count was measuring is over.
		setWorkerStatus(t, e, row.ID, state.WorkerDone)
		require.Empty(t, slotHolders(t, e), "the slot must genuinely be free for this poll")
		e.d.pollAndDispatch(e.ctx, false)

		// The SAME worker takes it again. Counting from where it left off would
		// cross the threshold immediately; counting from zero is what keeps an
		// intermittently-full forge from accumulating its way to a WARN.
		setWorkerStatus(t, e, row.ID, state.WorkerRunning)
		require.Len(t, slotHolders(t, e), 1)
		for poll := 1; poll <= threshold; poll++ {
			e.d.pollAndDispatch(e.ctx, false)
		}
		assert.Zero(t, countWarns(e, wedgedLimitWARN),
			"a run interrupted by a free poll must restart from zero, not resume")

		// And the run that follows the reset still reaches the threshold: the
		// reset delays the escalation, it does not disable it.
		e.d.pollAndDispatch(e.ctx, false)
		assert.Equal(t, 1, countWarns(e, wedgedLimitWARN),
			"a worker that goes on holding the limit after a reset is still announced")
	})

	t.Run("a full limit whose holder turns over every poll", func(t *testing.T) {
		e := newLimitEnv(t)
		threshold := wedgedLimitThreshold(t, e)

		// The healthy busy forge: the limit is full on every single poll and
		// dispatch is refused every time, but by a different worker each cycle.
		// This is the case the escalation exists to NOT fire on, and the reason
		// the tracker keys on the worker id rather than on the blocked
		// condition — which is identical here to the wedged test above.
		var previous string
		for poll := 0; poll < threshold*3; poll++ {
			row := e.seedWorkerRow(workerRow{
				BeadID:     limitRotatingBead,
				Status:     state.WorkerRunning,
				Phase:      "smith",
				StartedAt:  time.Now(),
				Generation: e.generation(),
				Heartbeat:  time.Now(),
			})
			if previous != "" {
				setWorkerStatus(t, e, previous, state.WorkerDone)
			}
			previous = row.ID

			require.Len(t, slotHolders(t, e), 1,
				"poll %d: exactly one worker must hold the limit", poll)
			e.d.pollAndDispatch(e.ctx, false)
		}

		require.True(t, e.hasLog(slog.LevelInfo, "global smith limit reached, skipping dispatch"),
			"dispatch must have been refused on these polls, or the case is not saturation at all")
		assert.Zero(t, countWarns(e, wedgedLimitWARN),
			"a forge whose slots turn over every poll is saturated, not wedged")
	})
}
