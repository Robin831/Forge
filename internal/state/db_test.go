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

func TestDB_QueueCache(t *testing.T) {
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

	// 1. Empty cache returns empty slice
	items, err := db.QueueCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("expected empty cache, got %d items", len(items))
	}

	// 2. Write items and read back
	input := []QueueItem{
		{BeadID: "bd-3", Anvil: "anvil-a", Title: "Low priority", Priority: 3, Status: "open"},
		{BeadID: "bd-1", Anvil: "anvil-b", Title: "High priority", Priority: 1, Status: "open"},
		{BeadID: "bd-2", Anvil: "anvil-a", Title: "Mid priority", Priority: 2, Status: "open"},
	}
	if err := db.ReplaceQueueCache(input); err != nil {
		t.Fatal(err)
	}

	items, err = db.QueueCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}

	// Verify ordering: priority ASC, then bead_id ASC
	if items[0].BeadID != "bd-1" || items[0].Priority != 1 {
		t.Errorf("expected bd-1 first, got %s (priority %d)", items[0].BeadID, items[0].Priority)
	}
	if items[1].BeadID != "bd-2" || items[1].Priority != 2 {
		t.Errorf("expected bd-2 second, got %s (priority %d)", items[1].BeadID, items[1].Priority)
	}
	if items[2].BeadID != "bd-3" || items[2].Priority != 3 {
		t.Errorf("expected bd-3 third, got %s (priority %d)", items[2].BeadID, items[2].Priority)
	}

	// 3. Replace semantics: new call replaces old data
	replacement := []QueueItem{
		{BeadID: "bd-99", Anvil: "anvil-c", Title: "Only item", Priority: 0, Status: "ready"},
	}
	if err := db.ReplaceQueueCache(replacement); err != nil {
		t.Fatal(err)
	}

	items, err = db.QueueCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item after replacement, got %d", len(items))
	}
	if items[0].BeadID != "bd-99" || items[0].Anvil != "anvil-c" || items[0].Status != "ready" {
		t.Errorf("unexpected item: %+v", items[0])
	}

	// 4. Replace with empty clears cache
	if err := db.ReplaceQueueCache(nil); err != nil {
		t.Fatal(err)
	}
	items, err = db.QueueCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("expected empty cache after nil replace, got %d items", len(items))
	}
}
