package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDB_PRLifecycle(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-state-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	dbPath := filepath.Join(tmpDir, "state.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// 1. Insert PR with defaults
	pr := &PR{
		Number:    123,
		Anvil:     "anvil-1",
		BeadID:    "bd-1",
		Branch:    "fix-1",
		Status:    PROpen,
		CreatedAt: time.Now(),
	}
	if err := db.InsertPR(pr); err != nil {
		t.Fatal(err)
	}
	if pr.ID == 0 {
		t.Fatal("expected ID to be set")
	}

	// 2. Fetch and check
	pr2, err := db.GetPRByNumber("anvil-1", 123)
	if err != nil {
		t.Fatal(err)
	}
	if pr2 == nil {
		t.Fatal("PR not found")
	}
	if pr2.Number != 123 || !pr2.CIPassing {
		t.Errorf("incorrect data: Number=%d, CIPassing=%v", pr2.Number, pr2.CIPassing)
	}

	// 3. Update lifecycle
	if err := db.UpdatePRLifecycle(pr.ID, 5, 3, false); err != nil {
		t.Fatal(err)
	}

	// 4. Fetch and check again
	pr3, err := db.GetPRByNumber("anvil-1", 123)
	if err != nil {
		t.Fatal(err)
	}
	if pr3.CIFixCount != 5 || pr3.ReviewFixCount != 3 || pr3.CIPassing {
		t.Errorf("incorrect lifecycle data: Fixes=%d/%d, CIPassing=%v",
			pr3.CIFixCount, pr3.ReviewFixCount, pr3.CIPassing)
	}
}
