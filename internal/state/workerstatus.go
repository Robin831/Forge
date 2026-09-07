package state

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// terminalWorkerStatuses is the ONE definition of "this worker row is
// finished": the work it describes is over, nothing owns it any more, and no
// path will move it again. Every reader that needs the set — the dispatch
// goroutine's exit backstop here, and the sibling call sites that ask the same
// question of a row they did not write — reads it through IsTerminal rather
// than restating the list, because a copy that gains 'partial' in one place and
// not the other reports a finished worker as a live one on whichever surface
// lost the race.
//
// 'killed' is on the list for the same reason the other four are: `kill_worker`
// / `forge queue stop` is an ending, and a row left claiming a live Smith after
// one is exactly the state this vocabulary exists to make impossible to assert.
var terminalWorkerStatuses = []WorkerStatus{
	WorkerDone,
	WorkerFailed,
	WorkerPartial,
	WorkerTimeout,
	WorkerKilled,
}

// backstopExemptStatuses are the statuses a dispatch goroutine's exit must
// leave exactly as it found them. It is the terminal set plus the two shapes of
// a row that is deliberately still live after the goroutine that created it has
// returned:
//
//   - monitoring / detached (WorkerStatus.IsMonitorOnly): the pipeline moves its
//     own row to monitoring at warden approval and finalizePipeline leaves it
//     there on success, precisely so Bellows can pick the PR up. Failing that
//     row would report every successful dispatch as a failure.
//   - paused: a parked pipeline cancelled by shutdown returns a non-nil error
//     with the worker deliberately left paused so a resume after restart can
//     continue in place (internal/pipeline parkPipeline). Failing it would
//     destroy the one signal the cold-resume path reads.
//
// Everything else — pending, running, reviewing, stalled, and any value this
// package does not model — means a goroutine exited while its row still claimed
// live work.
func backstopExemptStatuses() []WorkerStatus {
	exempt := make([]WorkerStatus, 0, len(terminalWorkerStatuses)+3)
	exempt = append(exempt, terminalWorkerStatuses...)
	return append(exempt, WorkerMonitoring, WorkerDetached, WorkerPaused)
}

// backstopExemptSQL is the SQL IN-list for backstopExemptStatuses, built from
// those constants rather than hand-written beside them: the Go predicate
// (NeedsTerminalBackstop) and the conditional UPDATE that enforces it must
// agree by construction, since a status present in one list and absent from the
// other is a row the check declines to touch and the write clobbers anyway.
var backstopExemptSQL = statusSQLList(backstopExemptStatuses())

// statusSQLList renders worker statuses as a parenthesised SQL literal list.
// Its inputs are this package's own constants — never a value read back from
// the database or supplied by a caller — so the values are quoted rather than
// bound.
func statusSQLList(statuses []WorkerStatus) string {
	quoted := make([]string, len(statuses))
	for i, s := range statuses {
		quoted[i] = "'" + string(s) + "'"
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}

// IsTerminal reports whether this status is an ending — the work the row
// describes is over and no path will move it again.
func (s WorkerStatus) IsTerminal() bool {
	for _, terminal := range terminalWorkerStatuses {
		if s == terminal {
			return true
		}
	}
	return false
}

// IsTerminalWorkerStatus is IsTerminal for a caller holding an untyped status
// string (a raw column value, an IPC payload field). It is a conversion and not
// a second definition, so the two can never disagree.
//
// A status this package does not model — including the empty string — is NOT
// terminal, which is the safe direction at every call site: an unrecognised
// value is a row nobody can prove is finished, and treating it as finished is
// what leaves a phantom Smith holding a dispatch slot forever.
func IsTerminalWorkerStatus(status string) bool {
	return WorkerStatus(status).IsTerminal()
}

// NeedsTerminalBackstop reports whether a row carrying this status, observed at
// the moment its dispatch goroutine exits, must be forced to a terminal status.
//
// It is deliberately narrower than !IsTerminal: see backstopExemptStatuses for
// the two live statuses a goroutine hands off rather than abandons.
func (s WorkerStatus) NeedsTerminalBackstop() bool {
	for _, exempt := range backstopExemptStatuses() {
		if s == exempt {
			return false
		}
	}
	return true
}

// ErrWorkerNotFound is returned by GetWorkerStatus for an id with no row. It is
// its own error because the two answers need opposite handling: a read that
// failed says nothing about the worker, while a row that is gone (the bellows
// poll loop sweeps repurposed pipeline rows) is a row with nothing left to
// finalise.
var ErrWorkerNotFound = errors.New("worker not found")

// GetWorkerStatus returns one worker row's current status.
func (db *DB) GetWorkerStatus(id string) (WorkerStatus, error) {
	var status string
	err := db.conn.QueryRow(`SELECT status FROM workers WHERE id = ?`, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrWorkerNotFound
	}
	if err != nil {
		return "", err
	}
	return WorkerStatus(status), nil
}

// FailWorkerIfUnfinished marks a worker failed only if its row is still
// claiming live work, and reports whether it did.
//
// The test is in the UPDATE's own WHERE clause rather than in a read the caller
// makes first, because the caller is a deferred backstop racing every ordinary
// path that finalises the same row: between a read that observed 'running' and
// a write that acted on it, finalizePipeline can land 'done', and a
// read-then-write would then report a successful dispatch as a failure. A row
// that does not exist, and one already exempt, are both no-ops.
func (db *DB) FailWorkerIfUnfinished(id string) (bool, error) {
	res, err := db.conn.Exec(
		`UPDATE workers SET status = ?, completed_at = ?
		 WHERE id = ? AND status NOT IN `+backstopExemptSQL,
		string(WorkerFailed), time.Now().Format(dbTimeLayout), id,
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
