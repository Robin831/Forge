package state

import (
	"path/filepath"
	"testing"
	"time"
)

// RecentlyFinishedWorkers backs the dashboard's lingering finished panels
// (Forge-hyla): terminal workers inside the window are returned newest first;
// older terminal workers, and workers still running, are not.
func TestDB_RecentlyFinishedWorkers(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	insert := func(id string, status WorkerStatus, completedAgo time.Duration) {
		t.Helper()
		if err := db.InsertWorker(&Worker{
			ID:        id,
			BeadID:    "Forge-abc1",
			Anvil:     "anvil-a",
			Status:    status,
			Phase:     "smith",
			StartedAt: time.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if completedAgo >= 0 {
			completed := time.Now().Add(-completedAgo).Format(dbTimeLayout)
			if _, err := db.conn.Exec(`UPDATE workers SET completed_at = ? WHERE id = ?`, completed, id); err != nil {
				t.Fatal(err)
			}
		}
	}

	insert("w-done-recent", WorkerDone, time.Minute)
	insert("w-failed-recent", WorkerFailed, 30*time.Second)
	insert("w-done-old", WorkerDone, 10*time.Minute)
	insert("w-running", WorkerRunning, -1)

	// A half-covered Assay run finishes 'partial'. It goes through the real
	// UpdateWorkerStatus rather than a direct UPDATE, since that function's
	// terminal-status list is the thing that has to stamp completed_at — a
	// partial worker without one lingers as apparently-running forever.
	insert("w-partial-recent", WorkerRunning, -1)
	if err := db.UpdateWorkerStatus("w-partial-recent", WorkerPartial); err != nil {
		t.Fatal(err)
	}

	got, err := db.RecentlyFinishedWorkers(3 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 recent finished workers, got %d: %v", len(got), workerIDs(got))
	}
	if ids := workerIDs(got); ids[0] != "w-partial-recent" || ids[1] != "w-failed-recent" || ids[2] != "w-done-recent" {
		t.Errorf("expected newest-first [w-partial-recent w-failed-recent w-done-recent], got %v", ids)
	}
	if got[0].CompletedAt == nil {
		t.Error("UpdateWorkerStatus(partial) left completed_at unset")
	}

	// The same status must survive the history query, which keeps its own
	// hand-maintained terminal-status list.
	completed, err := db.CompletedWorkers(0)
	if err != nil {
		t.Fatal(err)
	}
	if !containsWorker(completed, "w-partial-recent") {
		t.Errorf("CompletedWorkers dropped the partial worker: %v", workerIDs(completed))
	}
}

func workerIDs(workers []Worker) []string {
	ids := make([]string, len(workers))
	for i, w := range workers {
		ids[i] = w.ID
	}
	return ids
}

func containsWorker(workers []Worker, id string) bool {
	for _, w := range workers {
		if w.ID == id {
			return true
		}
	}
	return false
}
