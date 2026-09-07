package state

import (
	"errors"
	"testing"
	"time"
)

// The terminal set is the vocabulary the dispatch-exit backstop and its sibling
// call sites share. Every status this package models is asserted in one
// direction or the other so a new constant cannot be added without deciding
// which side it belongs on.
func TestIsTerminalCoversEveryModelledStatus(t *testing.T) {
	terminal := []WorkerStatus{WorkerDone, WorkerFailed, WorkerPartial, WorkerTimeout, WorkerKilled}
	live := []WorkerStatus{WorkerPending, WorkerRunning, WorkerReviewing, WorkerMonitoring, WorkerDetached, WorkerStalled, WorkerPaused}

	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%q: IsTerminal() = false, want true", s)
		}
		if !IsTerminalWorkerStatus(string(s)) {
			t.Errorf("%q: IsTerminalWorkerStatus() = false, want true", s)
		}
	}
	for _, s := range live {
		if s.IsTerminal() {
			t.Errorf("%q: IsTerminal() = true, want false", s)
		}
		if IsTerminalWorkerStatus(string(s)) {
			t.Errorf("%q: IsTerminalWorkerStatus() = true, want false", s)
		}
	}
}

// An unmodelled value is not terminal: nobody can prove such a row is finished,
// and reading it as finished is what leaves a phantom Smith holding a slot.
func TestUnknownStatusIsNotTerminalAndNeedsTheBackstop(t *testing.T) {
	for _, s := range []WorkerStatus{"", "wat", "Done", "DONE"} {
		if s.IsTerminal() {
			t.Errorf("%q: IsTerminal() = true, want false", s)
		}
		if !s.NeedsTerminalBackstop() {
			t.Errorf("%q: NeedsTerminalBackstop() = false, want true", s)
		}
	}
}

// NeedsTerminalBackstop is narrower than !IsTerminal by exactly the statuses a
// dispatch hands off live: monitoring/detached (Bellows owns the PR from here)
// and paused (a cold resume owns the parked session).
func TestNeedsTerminalBackstopExemptsHandedOffRows(t *testing.T) {
	needs := []WorkerStatus{WorkerPending, WorkerRunning, WorkerReviewing, WorkerStalled}
	exempt := []WorkerStatus{
		WorkerDone, WorkerFailed, WorkerPartial, WorkerTimeout, WorkerKilled,
		WorkerMonitoring, WorkerDetached, WorkerPaused,
	}

	for _, s := range needs {
		if !s.NeedsTerminalBackstop() {
			t.Errorf("%q: NeedsTerminalBackstop() = false, want true", s)
		}
	}
	for _, s := range exempt {
		if s.NeedsTerminalBackstop() {
			t.Errorf("%q: NeedsTerminalBackstop() = true, want false", s)
		}
	}
}

func insertWorkerWithStatus(t *testing.T, db *DB, id string, status WorkerStatus) {
	t.Helper()
	if err := db.InsertWorker(&Worker{
		ID:        id,
		BeadID:    "Forge-52bp",
		Anvil:     "anvil-a",
		Status:    status,
		Phase:     "smith",
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGetWorkerStatus(t *testing.T) {
	db := openTestDB(t)
	insertWorkerWithStatus(t, db, "w-1", WorkerRunning)

	got, err := db.GetWorkerStatus("w-1")
	if err != nil {
		t.Fatalf("GetWorkerStatus: %v", err)
	}
	if got != WorkerRunning {
		t.Errorf("GetWorkerStatus = %q, want %q", got, WorkerRunning)
	}

	// A row that is gone is its own answer, not a read failure: nothing is left
	// to finalise, and the backstop must be able to tell the two apart.
	if _, err := db.GetWorkerStatus("no-such-worker"); !errors.Is(err, ErrWorkerNotFound) {
		t.Errorf("GetWorkerStatus(missing) error = %v, want ErrWorkerNotFound", err)
	}
}

// FailWorkerIfUnfinished fires for a row still claiming live work, is a no-op
// for one an ordinary path already finalised or deliberately handed off, and
// stamps completed_at so the row ages out of the recent-workers window like any
// other terminal row.
func TestFailWorkerIfUnfinished(t *testing.T) {
	cases := []struct {
		status     WorkerStatus
		wantFailed bool
	}{
		{WorkerPending, true},
		{WorkerRunning, true},
		{WorkerReviewing, true},
		{WorkerStalled, true},
		{WorkerDone, false},
		{WorkerFailed, false},
		{WorkerPartial, false},
		{WorkerTimeout, false},
		{WorkerKilled, false},
		{WorkerMonitoring, false},
		{WorkerDetached, false},
		{WorkerPaused, false},
	}

	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			db := openTestDB(t)
			insertWorkerWithStatus(t, db, "w-1", tc.status)

			failed, err := db.FailWorkerIfUnfinished("w-1")
			if err != nil {
				t.Fatalf("FailWorkerIfUnfinished: %v", err)
			}
			if failed != tc.wantFailed {
				t.Fatalf("FailWorkerIfUnfinished = %v, want %v", failed, tc.wantFailed)
			}

			got, err := db.GetWorkerStatus("w-1")
			if err != nil {
				t.Fatalf("GetWorkerStatus: %v", err)
			}
			want := tc.status
			if tc.wantFailed {
				want = WorkerFailed
			}
			if got != want {
				t.Errorf("status after backstop = %q, want %q", got, want)
			}

			if tc.wantFailed {
				var completed *string
				if err := db.conn.QueryRow(`SELECT completed_at FROM workers WHERE id = ?`, "w-1").Scan(&completed); err != nil {
					t.Fatalf("read completed_at: %v", err)
				}
				if completed == nil || *completed == "" {
					t.Error("backstop left completed_at unset")
				}
			}
		})
	}
}

// The backstop is idempotent with itself and with every explicit termination:
// running a second time changes nothing, which is what makes it safe to install
// on a path that already terminates the row (preDispatchRemoteBranchCheck).
func TestFailWorkerIfUnfinishedIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	insertWorkerWithStatus(t, db, "w-1", WorkerRunning)

	if failed, err := db.FailWorkerIfUnfinished("w-1"); err != nil || !failed {
		t.Fatalf("first call: failed=%v err=%v, want true/nil", failed, err)
	}
	if failed, err := db.FailWorkerIfUnfinished("w-1"); err != nil || failed {
		t.Fatalf("second call: failed=%v err=%v, want false/nil", failed, err)
	}
}

// A row the bellows sweep already deleted is nothing to finalise; the backstop
// must not resurrect it.
func TestFailWorkerIfUnfinishedIgnoresAMissingRow(t *testing.T) {
	db := openTestDB(t)

	failed, err := db.FailWorkerIfUnfinished("no-such-worker")
	if err != nil {
		t.Fatalf("FailWorkerIfUnfinished: %v", err)
	}
	if failed {
		t.Error("FailWorkerIfUnfinished reported a write against a row that does not exist")
	}
	if _, err := db.GetWorkerStatus("no-such-worker"); !errors.Is(err, ErrWorkerNotFound) {
		t.Errorf("backstop created a row: GetWorkerStatus error = %v", err)
	}
}
