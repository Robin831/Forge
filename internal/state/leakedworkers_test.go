package state

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func leakDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func insertLeakWorker(t *testing.T, db *DB, id string, status WorkerStatus) {
	t.Helper()
	require.NoError(t, db.InsertWorker(&Worker{
		ID:        id,
		BeadID:    "Forge-g1j7",
		Anvil:     "repo",
		Status:    status,
		Phase:     "smith",
		StartedAt: time.Now(),
	}))
}

func setPrevStatus(t *testing.T, db *DB, id string, prev WorkerStatus) {
	t.Helper()
	_, err := db.conn.Exec(`UPDATE workers SET prev_status = ? WHERE id = ?`, string(prev), id)
	require.NoError(t, err)
}

func candidateIDs(t *testing.T, db *DB) []string {
	t.Helper()
	cands, err := db.LeakCandidates()
	require.NoError(t, err)
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.ID)
	}
	return ids
}

// A fresh database carries the two ownership columns, and so does one upgraded
// from a schema that predates them — with every row it already held surviving
// the upgrade carrying no generation at all.
//
// That empty value is the migration's whole contract: there is no generation a
// backfill could honestly write for a row inserted by a daemon lifetime nobody
// can name, and empty is read everywhere as "not this daemon's", which is the
// conservative reading for the only decision that consults it.
func TestOwnershipColumnsMigrateOntoAnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Stand up a workers table in the shape it had before the columns existed,
	// with a row in it, and let migrate() upgrade it.
	legacy, err := Open(path)
	require.NoError(t, err)
	_, err = legacy.conn.Exec(`DROP TABLE workers`)
	require.NoError(t, err)
	_, err = legacy.conn.Exec(`CREATE TABLE workers (
		id TEXT PRIMARY KEY, bead_id TEXT NOT NULL, anvil TEXT NOT NULL,
		branch TEXT NOT NULL DEFAULT '', pid INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'pending', phase TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '', started_at TEXT NOT NULL,
		completed_at TEXT, log_path TEXT NOT NULL DEFAULT '',
		prev_status TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '')`)
	require.NoError(t, err)
	_, err = legacy.conn.Exec(
		`INSERT INTO workers (id, bead_id, anvil, status, started_at) VALUES ('old-1', 'Forge-old', 'repo', 'running', ?)`,
		time.Now().Format(dbTimeLayout))
	require.NoError(t, err)

	for _, col := range []string{"daemon_generation", "heartbeat_at"} {
		exists, cerr := legacy.columnExists("workers", col)
		require.NoError(t, cerr)
		require.False(t, exists, "precondition: the legacy table must not carry %s", col)
	}

	require.NoError(t, legacy.migrate())
	for _, col := range []string{"daemon_generation", "heartbeat_at"} {
		exists, cerr := legacy.columnExists("workers", col)
		require.NoError(t, cerr)
		assert.True(t, exists, "%s must exist after the migration", col)
	}

	own, err := legacy.WorkerOwnershipOf("old-1")
	require.NoError(t, err)
	assert.Equal(t, "", own.Generation, "a pre-migration row must carry no generation, not a guessed one")
	assert.Equal(t, "", own.Heartbeat)
	assert.True(t, own.HeartbeatAt().IsZero())
	require.NoError(t, legacy.Close())

	// And a database created from the current schema has them without any
	// migration having to run.
	fresh := leakDB(t)
	for _, col := range []string{"daemon_generation", "heartbeat_at"} {
		exists, cerr := fresh.columnExists("workers", col)
		require.NoError(t, cerr)
		assert.True(t, exists, "%s must be in the base schema too", col)
	}
}

// Every insert stamps the generation and an opening heartbeat, from the DB's
// own setting rather than from the caller — which is the property that makes
// forgetting impossible at the ten call sites that write worker rows.
func TestInsertStampsGenerationAndHeartbeat(t *testing.T) {
	db := leakDB(t)

	// Before a generation is set — a CLI subcommand, a test — rows carry none.
	insertLeakWorker(t, db, "unstamped", WorkerRunning)
	own, err := db.WorkerOwnershipOf("unstamped")
	require.NoError(t, err)
	assert.Equal(t, "", own.Generation)
	assert.False(t, own.HeartbeatAt().IsZero(), "the heartbeat is stamped even with no generation")

	db.SetDaemonGeneration("gen-1")
	insertLeakWorker(t, db, "stamped", WorkerRunning)
	own, err = db.WorkerOwnershipOf("stamped")
	require.NoError(t, err)
	assert.Equal(t, "gen-1", own.Generation)
	assert.WithinDuration(t, time.Now(), own.HeartbeatAt(), time.Minute)

	// InsertWorkerIfMissing stamps identically: bellows' monitor rows are
	// exempt by status, but nothing in the insert path may depend on that.
	require.NoError(t, db.InsertWorkerIfMissing(&Worker{
		ID: "monitor", BeadID: "b", Anvil: "repo",
		Status: WorkerMonitoring, Phase: "bellows", StartedAt: time.Now(),
	}))
	own, err = db.WorkerOwnershipOf("monitor")
	require.NoError(t, err)
	assert.Equal(t, "gen-1", own.Generation)
}

// The candidate query is the reaper's structural safety rule, and the row that
// must never appear in it is the paused one: on 2026-09-07 Fhi.Metadata-2p3ck
// sat paused with a dead pid and a perfectly resumable session, so every
// downstream signal would have called it leaked.
func TestLeakCandidatesExcludeEveryRowThatIsNotClaimingLiveWork(t *testing.T) {
	db := leakDB(t)

	live := []WorkerStatus{WorkerPending, WorkerRunning, WorkerReviewing, WorkerStalled}
	for _, s := range live {
		insertLeakWorker(t, db, "live-"+string(s), s)
	}
	exempt := []WorkerStatus{
		WorkerDone, WorkerFailed, WorkerPartial, WorkerTimeout, WorkerKilled,
		WorkerMonitoring, WorkerDetached, WorkerPaused,
	}
	for _, s := range exempt {
		insertLeakWorker(t, db, "exempt-"+string(s), s)
	}

	got := candidateIDs(t, db)
	want := []string{"live-pending", "live-running", "live-reviewing", "live-stalled"}
	assert.ElementsMatch(t, want, got)
}

// 'stalled' is a mask, so it is decided by the status underneath: the watchdog
// can stall a row Bellows already owns (a pipeline flips to monitoring at
// warden approval and then writes no log through the push and the PR create),
// and reaping that would end an open PR's monitor.
func TestStalledCandidacyIsDecidedByThePrevStatus(t *testing.T) {
	db := leakDB(t)

	insertLeakWorker(t, db, "stalled-over-running", WorkerStalled)
	setPrevStatus(t, db, "stalled-over-running", WorkerRunning)
	insertLeakWorker(t, db, "stalled-over-monitoring", WorkerStalled)
	setPrevStatus(t, db, "stalled-over-monitoring", WorkerMonitoring)
	insertLeakWorker(t, db, "stalled-over-paused", WorkerStalled)
	setPrevStatus(t, db, "stalled-over-paused", WorkerPaused)
	insertLeakWorker(t, db, "stalled-unrecorded", WorkerStalled)

	assert.ElementsMatch(t,
		[]string{"stalled-over-running", "stalled-unrecorded"},
		candidateIDs(t, db),
		"a stalled row is a candidate only when the status it masks was itself claiming live work")
}

// The candidate query and the dispatch-exit backstop's predicate are the same
// rule, so neither can be given a status the other has not been. The query is
// SQL and the predicate is Go, which is exactly the arrangement that drifts.
func TestLeakCandidacyMatchesTheBackstopPredicate(t *testing.T) {
	db := leakDB(t)

	all := []WorkerStatus{
		WorkerPending, WorkerRunning, WorkerReviewing, WorkerMonitoring,
		WorkerDetached, WorkerStalled, WorkerPaused, WorkerDone, WorkerFailed,
		WorkerPartial, WorkerTimeout, WorkerKilled, WorkerStatus("something-new"),
	}
	prevs := []WorkerStatus{"", WorkerRunning, WorkerMonitoring, WorkerPaused}

	want := map[string]bool{}
	for _, s := range all {
		for _, prev := range prevs {
			id := string(s) + "/" + string(prev)
			insertLeakWorker(t, db, id, s)
			setPrevStatus(t, db, id, prev)
			want[id] = WorkerBackstopState{Status: s, PrevStatus: prev}.NeedsTerminalBackstop()
		}
	}

	inQuery := map[string]bool{}
	for _, id := range candidateIDs(t, db) {
		inQuery[id] = true
	}
	for id, expected := range want {
		assert.Equal(t, expected, inQuery[id], "candidacy for %s must match NeedsTerminalBackstop", id)
	}
}

// The reap is a compare-and-set on the evidence the decision was made from, so
// a row that stopped being a leak between the read and the write is left alone
// — in both of the ways the evidence models it.
func TestReapLeakedWorkerIsAConditionalWrite(t *testing.T) {
	db := leakDB(t)
	db.SetDaemonGeneration("gen-1")

	t.Run("reaps on matching evidence", func(t *testing.T) {
		insertLeakWorker(t, db, "match", WorkerRunning)
		own, err := db.WorkerOwnershipOf("match")
		require.NoError(t, err)
		reaped, err := db.ReapLeakedWorker("match", own.Generation, own.Heartbeat)
		require.NoError(t, err)
		assert.True(t, reaped)
		status, err := db.GetWorkerStatus("match")
		require.NoError(t, err)
		assert.Equal(t, WorkerFailed, status)
	})

	t.Run("declines when the generation moved", func(t *testing.T) {
		insertLeakWorker(t, db, "regen", WorkerRunning)
		own, err := db.WorkerOwnershipOf("regen")
		require.NoError(t, err)
		reaped, err := db.ReapLeakedWorker("regen", "some-other-generation", own.Heartbeat)
		require.NoError(t, err)
		assert.False(t, reaped)
		status, err := db.GetWorkerStatus("regen")
		require.NoError(t, err)
		assert.Equal(t, WorkerRunning, status)
	})

	t.Run("declines when a heartbeat landed", func(t *testing.T) {
		insertLeakWorker(t, db, "beat", WorkerRunning)
		own, err := db.WorkerOwnershipOf("beat")
		require.NoError(t, err)
		n, err := db.HeartbeatWorkers([]string{"beat"})
		require.NoError(t, err)
		require.Equal(t, 1, n)
		reaped, err := db.ReapLeakedWorker("beat", own.Generation, own.Heartbeat)
		require.NoError(t, err)
		assert.False(t, reaped, "the observed heartbeat is stale, so the row is no longer the one that was judged")
	})

	t.Run("declines when the row became exempt", func(t *testing.T) {
		insertLeakWorker(t, db, "parked", WorkerRunning)
		own, err := db.WorkerOwnershipOf("parked")
		require.NoError(t, err)
		require.NoError(t, db.UpdateWorkerStatus("parked", WorkerPaused))
		reaped, err := db.ReapLeakedWorker("parked", own.Generation, own.Heartbeat)
		require.NoError(t, err)
		assert.False(t, reaped)
		status, err := db.GetWorkerStatus("parked")
		require.NoError(t, err)
		assert.Equal(t, WorkerPaused, status)
	})
}

// The heartbeat write is confined to rows the reaper could act on: heartbeating
// a finished row would assert that a worker nothing owns is alive, over a
// column nothing else can contradict.
func TestHeartbeatWorkersSkipsRowsTheReaperWouldNotTouch(t *testing.T) {
	db := leakDB(t)
	db.SetDaemonGeneration("gen-1")

	insertLeakWorker(t, db, "running", WorkerRunning)
	insertLeakWorker(t, db, "done", WorkerDone)
	insertLeakWorker(t, db, "paused", WorkerPaused)

	before := map[string]string{}
	for _, id := range []string{"running", "done", "paused"} {
		own, err := db.WorkerOwnershipOf(id)
		require.NoError(t, err)
		before[id] = own.Heartbeat
	}

	time.Sleep(2 * time.Millisecond)
	n, err := db.HeartbeatWorkers([]string{"running", "done", "paused", "no-such-row"})
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the row still claiming live work is heartbeated")

	after, err := db.WorkerOwnershipOf("running")
	require.NoError(t, err)
	assert.NotEqual(t, before["running"], after.Heartbeat)
	for _, id := range []string{"done", "paused"} {
		own, oerr := db.WorkerOwnershipOf(id)
		require.NoError(t, oerr)
		assert.Equal(t, before[id], own.Heartbeat, "%s must not be heartbeated", id)
	}

	// Nothing registered writes nothing at all.
	n, err = db.HeartbeatWorkers(nil)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// LastSeen degrades to the insertion time rather than to the zero time, because
// a zero there reads as infinitely stale — which turns "I have no measurement"
// into "reap it".
func TestLeakCandidateLastSeenFallsBackToStartedAt(t *testing.T) {
	started := time.Now().Add(-time.Hour)
	beat := time.Now().Add(-time.Minute)

	withBeat := LeakCandidate{Heartbeat: beat.Format(dbTimeLayout), StartedAt: started}
	assert.WithinDuration(t, beat, withBeat.LastSeen(), time.Millisecond)

	noBeat := LeakCandidate{StartedAt: started}
	assert.WithinDuration(t, started, noBeat.LastSeen(), time.Millisecond)

	assert.True(t, LeakCandidate{}.LastSeen().IsZero(), "nothing recorded stays nothing, never a zero timestamp to age")
}

// The reaper's candidate set and the non-terminal set ActiveWorkers returns are
// two queries over the same table written in two places, and they have to agree
// in one direction: every row the reaper may end must be one ActiveWorkers
// reports, or the reap would free nothing an operator can see and the dispatch
// slot it was holding would never have been counted in the first place.
//
// The reverse containment is deliberately NOT asserted — ActiveWorkers is the
// wider set, since monitoring, detached and stalled-over-monitoring rows are
// live and unreapable at the same time.
func TestEveryLeakCandidateIsAlsoAnActiveWorker(t *testing.T) {
	db := leakDB(t)

	all := []WorkerStatus{
		WorkerPending, WorkerRunning, WorkerReviewing, WorkerMonitoring,
		WorkerDetached, WorkerStalled, WorkerPaused, WorkerDone, WorkerFailed,
		WorkerPartial, WorkerTimeout, WorkerKilled,
	}
	for _, s := range all {
		insertLeakWorker(t, db, string(s), s)
	}

	active := map[string]bool{}
	workers, err := db.ActiveWorkers()
	require.NoError(t, err)
	for _, w := range workers {
		active[w.ID] = true
	}

	for _, id := range candidateIDs(t, db) {
		assert.True(t, active[id], "leak candidate %s must be one ActiveWorkers reports", id)
	}

	// And nothing terminal reaches either set, which is the shared premise both
	// queries rest on.
	for _, s := range terminalWorkerStatuses {
		assert.False(t, active[string(s)], "%s is terminal and must not be active", s)
	}
}
