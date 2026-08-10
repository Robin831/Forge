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

	got, err := db.RecentlyFinishedWorkers(3 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		ids := make([]string, len(got))
		for i, w := range got {
			ids[i] = w.ID
		}
		t.Fatalf("expected 2 recent finished workers, got %d: %v", len(got), ids)
	}
	if got[0].ID != "w-failed-recent" || got[1].ID != "w-done-recent" {
		t.Errorf("expected newest-first [w-failed-recent w-done-recent], got [%s %s]", got[0].ID, got[1].ID)
	}
}
