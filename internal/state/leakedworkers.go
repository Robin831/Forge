package state

import (
	"strings"
	"sync/atomic"
	"time"
)

// The daemon generation and the heartbeat are the two columns that let a worker
// row be shown to be LEAKED rather than merely quiet.
//
// The obvious evidence — the pid the row records — cannot carry that claim, and
// the case that proves it was measured on this host on 2026-09-07:
// Fhi.Metadata-2p3ck sat at status=paused, phase=smith, pid=2157387, with no
// /proc/2157387, while its Claude session was intact and `resume_bead` would
// have continued it in place. The pid names the last process the row's phase
// spawned, not the owner of the row: it is dead by design for every phase past
// the spawn (temper, warden, monitoring), it is dead for the width of every
// re-spawn, and on a busy host it can be RECYCLED, so a live pid is not evidence
// of life either. A reaper that read it would destroy resumable work in one
// direction and leave the leak it exists to close in the other.
//
// What is decidable is ownership. A worker row is owned by the goroutine that
// inserted it, and a goroutine cannot outlive its process — so:
//
//   - daemon_generation names the daemon lifetime that inserted the row. A row
//     carrying any generation other than the running one was inserted by a
//     process that no longer exists, and therefore has no owner anywhere. That
//     is a proof, not an inference, and it is what closes the crash/restart
//     leak: it holds whatever the pid says, including a pid that reads alive
//     because the number was reused.
//   - heartbeat_at is refreshed by the running daemon for every worker row it
//     still owns. Within ONE generation it is the same claim made from the
//     other side: a row nothing has heartbeated for is a row nothing is
//     tracking. It is deliberately a weaker signal than the generation and is
//     only ever read together with the daemon's in-memory registry.
//
// Both are empty for a row written before the columns existed, and empty is
// read as "not recorded" at every site — never as a zero timestamp, which would
// read as infinitely stale.

// generationHolder is the atomic box DB.currentGeneration lives in. Naming it
// keeps the DB struct readable, and an atomic (rather than a mutex-guarded
// string) means the read on every insert never contends with anything.
type generationHolder = atomic.Pointer[string]

// currentGeneration is the daemon-lifetime marker stamped onto every worker row
// this process inserts. It is a field on the DB rather than a package global so
// a test can drive two "generations" against one database without racing
// another test's global, and so nothing outside a wired daemon can stamp rows
// by accident.
//
// The zero value is the empty string, which is exactly what a row inserted by a
// process that never called SetDaemonGeneration should carry: a CLI subcommand
// or a test writes no generation, and the reaper reads that as "not this
// daemon's" — the conservative direction, since such a row has no goroutine in
// the daemon either.
func (db *DB) generation() string {
	if v := db.currentGeneration.Load(); v != nil {
		return *v
	}
	return ""
}

// SetDaemonGeneration records the generation marker every subsequent
// InsertWorker / InsertWorkerIfMissing stamps onto the rows it writes.
//
// It is set on the DB rather than passed to each insert because there are ten
// insert call sites across three packages and a leak is created by whichever
// one forgets: an unstamped row reads as belonging to a previous lifetime and
// is reaped while its goroutine is still running it. Stamping at the single
// point every insert already goes through makes forgetting impossible.
func (db *DB) SetDaemonGeneration(generation string) {
	db.currentGeneration.Store(&generation)
}

// WorkerOwnership is the pair of columns the leak decision is made from, read
// back for one worker row. It exists for tests and diagnostics; the reaper
// itself reads them through LeakCandidates.
type WorkerOwnership struct {
	Generation string
	// Heartbeat is the raw column value — empty when never recorded.
	Heartbeat string
}

// HeartbeatAt is Heartbeat parsed, or the zero time when nothing was recorded.
func (o WorkerOwnership) HeartbeatAt() time.Time {
	return parseTime(o.Heartbeat)
}

// WorkerOwnershipOf returns one row's generation and heartbeat.
func (db *DB) WorkerOwnershipOf(id string) (WorkerOwnership, error) {
	var o WorkerOwnership
	err := db.conn.QueryRow(
		`SELECT daemon_generation, heartbeat_at FROM workers WHERE id = ?`, id,
	).Scan(&o.Generation, &o.Heartbeat)
	if err != nil {
		return WorkerOwnership{}, err
	}
	return o, nil
}

// HeartbeatWorkers stamps heartbeat_at = now on the given worker rows and
// reports how many rows it moved.
//
// The write is confined to rows the reaper could otherwise act on — the same
// exemption the candidate query applies — for two reasons. A terminal row must
// never be touched at all: heartbeating one would say a finished worker is
// alive, and the column is read by nothing else that could contradict it. And a
// caller that keeps a stale id registered (a release that never ran) must not
// be able to resurrect the appearance of ownership over a row somebody else has
// already ended.
//
// A caller with nothing registered writes nothing: an empty id list is a no-op
// rather than an UPDATE with an empty IN-list, which SQLite would happily match
// against no rows but which reads, at a glance, like a statement with a missing
// filter.
func (db *DB) HeartbeatWorkers(ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, time.Now().Format(dbTimeLayout))
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := db.conn.Exec(
		`UPDATE workers SET heartbeat_at = ?
		 WHERE id IN (`+placeholders(len(ids))+`)
		   AND status NOT IN `+backstopExemptSQL+`
		   AND NOT (status = `+backstopStalledSQL+` AND prev_status IN `+backstopExemptSQL+`)`,
		args...,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// placeholders renders n bound-parameter placeholders as a comma-separated
// list. Callers wrap it in their own parentheses.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// LeakCandidate is one worker row the reaper must decide about: a row still
// claiming live work, together with the evidence that says whether anything
// still owns it.
//
// The pid rides along deliberately, and deliberately takes no part in the
// decision: it is the one field an operator reading the WARN will want (it says
// whether a process was left behind to clean up by hand), and the comment at
// the top of this file is why it can be nothing more than that.
type LeakCandidate struct {
	ID     string
	BeadID string
	Anvil  string
	Phase  string
	Title  string
	PID    int

	Status     WorkerStatus
	PrevStatus WorkerStatus

	// Generation is the daemon lifetime that inserted the row, empty when the
	// row predates the column.
	Generation string
	// Heartbeat is the raw heartbeat_at column value, empty when never
	// recorded.
	Heartbeat string
	// StartedAt is the fallback the heartbeat degrades to: a row inserted by
	// this generation is always heartbeated at insert, so an empty heartbeat
	// within the running generation means the row was written by a build that
	// did not yet have the column, and its age is all there is.
	StartedAt time.Time
}

// LastSeen is the most recent moment anything is known to have owned this row:
// its heartbeat, or its insertion when no heartbeat was ever recorded.
//
// Never the zero time for a row with a start time, because a zero there would
// read as infinitely stale — the one reading that turns "I have no measurement"
// into "reap it".
func (c LeakCandidate) LastSeen() time.Time {
	if hb := parseTime(c.Heartbeat); !hb.IsZero() {
		return hb
	}
	return c.StartedAt
}

// LeakCandidates returns every worker row that is still claiming live work —
// the rows a reaper may consider, before any ownership evidence is read.
//
// The filter is WorkerBackstopState.NeedsTerminalBackstop expressed as SQL, and
// it is deliberately the same one the dispatch-exit backstop uses rather than a
// second list meaning roughly the same thing:
//
//   - the terminal statuses are already finished;
//   - monitoring / detached rows are a live HANDOFF to Bellows, owned by no
//     goroutine by design and outliving every restart — reaping them would end
//     every open PR's monitor row on the first tick;
//   - paused is the status this whole feature exists to protect. A parked
//     pipeline's row survives a restart on purpose so `resume_bead` can
//     continue the session in place, and its goroutine is gone by definition.
//     Every other piece of evidence here says "leaked" about exactly that row,
//     which is why the exclusion is structural (it is in the query) and not a
//     judgement made from the evidence;
//   - a 'stalled' row is decided by the status it was stalled FROM, since the
//     watchdog can mask a monitoring handoff with it.
//
// Rows are returned oldest first so a reap pass reports the longest-standing
// leak first.
func (db *DB) LeakCandidates() ([]LeakCandidate, error) {
	rows, err := db.conn.Query(
		`SELECT id, bead_id, anvil, phase, title, pid, status, prev_status,
		        daemon_generation, heartbeat_at, started_at
		 FROM workers
		 WHERE status NOT IN ` + backstopExemptSQL + `
		   AND NOT (status = ` + backstopStalledSQL + ` AND prev_status IN ` + backstopExemptSQL + `)
		 ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LeakCandidate
	for rows.Next() {
		var c LeakCandidate
		var status, prev, startedAt string
		if err := rows.Scan(&c.ID, &c.BeadID, &c.Anvil, &c.Phase, &c.Title, &c.PID,
			&status, &prev, &c.Generation, &c.Heartbeat, &startedAt); err != nil {
			return nil, err
		}
		c.Status = WorkerStatus(status)
		c.PrevStatus = WorkerStatus(prev)
		c.StartedAt = parseTime(startedAt)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ReapLeakedWorker marks a leaked worker row failed, and reports whether it did.
//
// It is a compare-and-set on the evidence the decision was made from: the
// generation and the heartbeat must still be exactly what LeakCandidates read.
// The reaper decides from a snapshot and writes some milliseconds later, and in
// that window a row can stop being a leak in both of the ways the evidence
// models — a cold resume can re-stamp it with the running generation, and a
// heartbeat can land for an id that was registered a moment after the snapshot
// was taken. Re-testing the pair is what makes those two outcomes a no-op
// instead of a live worker ended underneath its owner.
//
// The status exemption is re-tested here too, for the reason
// FailWorkerIfUnfinished states: the ordinary finalisers race this write, and a
// row that has become terminal, or been paused, or been handed to Bellows since
// the snapshot must be left exactly as it is.
func (db *DB) ReapLeakedWorker(id, generation, heartbeat string) (bool, error) {
	res, err := db.conn.Exec(
		`UPDATE workers SET status = ?, completed_at = ?
		 WHERE id = ?
		   AND daemon_generation = ?
		   AND heartbeat_at = ?
		   AND status NOT IN `+backstopExemptSQL+`
		   AND NOT (status = `+backstopStalledSQL+` AND prev_status IN `+backstopExemptSQL+`)`,
		string(WorkerFailed), time.Now().Format(dbTimeLayout), id, generation, heartbeat,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
