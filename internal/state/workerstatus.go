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
//
// 'stalled' is on the second list rather than the first, but it is the one
// status that does not describe itself: the watchdog sets it over whatever the
// row held (MarkWorkerStalled captures that into prev_status) and UnstallWorker
// puts the old status back when the log goes live again, so a stalled row may
// be masking a monitoring one — a pipeline flips to monitoring at warden
// approval and the push and PR creation that follow write no log, which is a
// window long enough for the watchdog to stall a row Bellows already owns.
// WorkerBackstopState.NeedsTerminalBackstop is therefore the predicate the
// backstop asks, and it reads the pair. What is NOT exempted is a row stalled
// over a live dispatch status: at the moment its goroutine exits, the session
// that row describes is over — pipeline.Run returns only once Smith has — and
// the recovery pass that would clear the stall is driven by fresh writes from
// that same session, so nothing would ever move the row again. Left alone it
// holds a dispatch slot forever (ActiveDispatchWorkers counts 'stalled'), which
// is the leak this backstop exists to close, in its likeliest shape: a dispatch
// going wrong usually goes quiet first.
//
// It is a package-level set built once rather than a function that rebuilds it
// per call, so the SQL IN-list below and the predicate above are literally the
// same values and not two renderings of one intent.
var backstopExemptStatuses = func() []WorkerStatus {
	exempt := make([]WorkerStatus, 0, len(terminalWorkerStatuses)+3)
	exempt = append(exempt, terminalWorkerStatuses...)
	return append(exempt, WorkerMonitoring, WorkerDetached, WorkerPaused)
}()

// backstopExemptSQL is the SQL IN-list for backstopExemptStatuses, built from
// those constants rather than hand-written beside them: the Go predicate
// (NeedsTerminalBackstop) and the conditional UPDATE that enforces it must
// agree by construction, since a status present in one list and absent from the
// other is a row the check declines to touch and the write clobbers anyway.
var backstopExemptSQL = statusSQLList(backstopExemptStatuses)

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
	for _, exempt := range backstopExemptStatuses {
		if s == exempt {
			return false
		}
	}
	return true
}

// WorkerBackstopState is one row's status together with the status the watchdog
// masked when it stalled it. It is a pair rather than a status because 'stalled'
// is a mask and not an ending: only prev_status says whether the row underneath
// was claiming live work or had already been handed off (see
// backstopExemptStatuses).
type WorkerBackstopState struct {
	Status     WorkerStatus
	PrevStatus WorkerStatus
}

// NeedsTerminalBackstop reports whether this row, observed at the moment its
// dispatch goroutine exits, must be forced to a terminal status. It is the
// predicate the backstop asks, and it is what FailWorkerIfUnfinished's WHERE
// clause enforces.
//
// A stalled row is decided by the status it was stalled FROM: stalled over
// monitoring is a handoff the watchdog happened to mask and must be left for
// UnstallWorker to restore, while stalled over a dispatch status is an
// abandoned row. An unrecorded prev_status — the column's empty default, or a
// row stalled by a build that predates it — is not exempt, on the same rule
// every unmodelled value follows here: a row nobody can prove was handed off is
// one that would otherwise hold a dispatch slot forever.
func (r WorkerBackstopState) NeedsTerminalBackstop() bool {
	if !r.Status.NeedsTerminalBackstop() {
		return false
	}
	if r.Status == WorkerStalled {
		return r.PrevStatus.NeedsTerminalBackstop()
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

// GetWorkerBackstopState returns the status pair the dispatch-exit backstop
// decides on. It is GetWorkerStatus plus prev_status, read in the same
// statement so the two columns cannot describe two different moments, and it
// raises the same ErrWorkerNotFound for a row that is gone.
func (db *DB) GetWorkerBackstopState(id string) (WorkerBackstopState, error) {
	var status, prev string
	err := db.conn.QueryRow(`SELECT status, prev_status FROM workers WHERE id = ?`, id).Scan(&status, &prev)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkerBackstopState{}, ErrWorkerNotFound
	}
	if err != nil {
		return WorkerBackstopState{}, err
	}
	return WorkerBackstopState{Status: WorkerStatus(status), PrevStatus: WorkerStatus(prev)}, nil
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
//
// The second clause is the stalled mask: the watchdog can flip an exempt row
// (monitoring, most of all) to 'stalled' while its status column says nothing
// about the handoff underneath, so the exemption is tested against prev_status
// there. It is WorkerBackstopState.NeedsTerminalBackstop as a WHERE clause, and
// it is the write's own test rather than the caller's for the same reason the
// first clause is.
func (db *DB) FailWorkerIfUnfinished(id string) (bool, error) {
	res, err := db.conn.Exec(
		`UPDATE workers SET status = ?, completed_at = ?
		 WHERE id = ?
		   AND status NOT IN `+backstopExemptSQL+`
		   AND NOT (status = '`+string(WorkerStalled)+`' AND prev_status IN `+backstopExemptSQL+`)`,
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
