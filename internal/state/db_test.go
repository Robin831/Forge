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
	if err := db.ReplaceQueueCacheForAnvils([]string{"anvil-a", "anvil-b"}, input); err != nil {
		t.Fatal(err)
	}

	items, err = db.QueueCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}

	// Verify ordering: priority ASC, then bead_id ASC, then anvil ASC
	if items[0].BeadID != "bd-1" || items[0].Priority != 1 {
		t.Errorf("expected bd-1 first, got %s (priority %d)", items[0].BeadID, items[0].Priority)
	}
	if items[1].BeadID != "bd-2" || items[1].Priority != 2 {
		t.Errorf("expected bd-2 second, got %s (priority %d)", items[1].BeadID, items[1].Priority)
	}
	if items[2].BeadID != "bd-3" || items[2].Priority != 3 {
		t.Errorf("expected bd-3 third, got %s (priority %d)", items[2].BeadID, items[2].Priority)
	}

	// 2b. Duplicate bead ID across anvils: deterministic tie-break by anvil
	dupes := []QueueItem{
		{BeadID: "bd-5", Anvil: "anvil-z", Title: "Same bead Z", Priority: 1, Status: "open"},
		{BeadID: "bd-5", Anvil: "anvil-a", Title: "Same bead A", Priority: 1, Status: "open"},
		{BeadID: "bd-4", Anvil: "anvil-b", Title: "Higher pri", Priority: 0, Status: "open"},
	}
	if err := db.ReplaceQueueCacheForAnvils([]string{"anvil-a", "anvil-b", "anvil-z"}, dupes); err != nil {
		t.Fatal(err)
	}
	items, err = db.QueueCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	// bd-4 (priority 0) first, then bd-5/anvil-a, then bd-5/anvil-z
	if items[0].BeadID != "bd-4" {
		t.Errorf("expected bd-4 first, got %s", items[0].BeadID)
	}
	if items[1].BeadID != "bd-5" || items[1].Anvil != "anvil-a" {
		t.Errorf("expected bd-5/anvil-a second, got %s/%s", items[1].BeadID, items[1].Anvil)
	}
	if items[2].BeadID != "bd-5" || items[2].Anvil != "anvil-z" {
		t.Errorf("expected bd-5/anvil-z third, got %s/%s", items[2].BeadID, items[2].Anvil)
	}

	// 3. Replace semantics: new call replaces old data for specified anvils
	replacement := []QueueItem{
		{BeadID: "bd-99", Anvil: "anvil-c", Title: "Only item", Priority: 0, Status: "ready"},
	}
	if err := db.ReplaceQueueCacheForAnvils([]string{"anvil-a", "anvil-b", "anvil-c", "anvil-z"}, replacement); err != nil {
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

	// 4. Replacing with no items clears the cache for the specified anvils
	if err := db.ReplaceQueueCacheForAnvils([]string{"anvil-c"}, nil); err != nil {
		t.Fatal(err)
	}
	items, err = db.QueueCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("expected empty cache after nil replace, got %d items", len(items))
	}

	// 5. Per-anvil replacement preserves rows for unspecified anvils
	seed := []QueueItem{
		{BeadID: "bd-10", Anvil: "anvil-x", Title: "X item", Priority: 1, Status: "open"},
		{BeadID: "bd-11", Anvil: "anvil-y", Title: "Y item", Priority: 2, Status: "open"},
	}
	if err := db.ReplaceQueueCacheForAnvils([]string{"anvil-x", "anvil-y"}, seed); err != nil {
		t.Fatal(err)
	}
	// Now update only anvil-x; anvil-y should be retained
	updated := []QueueItem{
		{BeadID: "bd-12", Anvil: "anvil-x", Title: "X updated", Priority: 0, Status: "open"},
	}
	if err := db.ReplaceQueueCacheForAnvils([]string{"anvil-x"}, updated); err != nil {
		t.Fatal(err)
	}
	items, err = db.QueueCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items (anvil-x updated + anvil-y retained), got %d", len(items))
	}
	if items[0].BeadID != "bd-12" || items[0].Anvil != "anvil-x" {
		t.Errorf("expected bd-12/anvil-x first, got %s/%s", items[0].BeadID, items[0].Anvil)
	}
	if items[1].BeadID != "bd-11" || items[1].Anvil != "anvil-y" {
		t.Errorf("expected bd-11/anvil-y second, got %s/%s", items[1].BeadID, items[1].Anvil)
	}
}

func TestDB_SetClarificationNeeded(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-state-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Initially no record exists
	r, err := db.GetRetry("BD-1", "anvil-1")
	if err == nil && r != nil {
		t.Fatal("expected no retry record initially")
	}

	// Set clarification needed
	if err := db.SetClarificationNeeded("BD-1", "anvil-1", true, "which auth library?"); err != nil {
		t.Fatal(err)
	}

	// Verify it was set
	r, err = db.GetRetry("BD-1", "anvil-1")
	if err != nil {
		t.Fatal(err)
	}
	if !r.ClarificationNeeded {
		t.Error("expected ClarificationNeeded=true")
	}
	if r.LastError != "which auth library?" {
		t.Errorf("expected reason in LastError, got %q", r.LastError)
	}

	// Clear clarification
	if err := db.SetClarificationNeeded("BD-1", "anvil-1", false, ""); err != nil {
		t.Fatal(err)
	}
	r, err = db.GetRetry("BD-1", "anvil-1")
	if err != nil {
		t.Fatal(err)
	}
	if r.ClarificationNeeded {
		t.Error("expected ClarificationNeeded=false after clearing")
	}
	// Reason should be preserved when clearing (the SQL uses CASE)
	if r.LastError != "which auth library?" {
		t.Errorf("expected reason preserved after clear, got %q", r.LastError)
	}
}

func TestDB_ClarificationNeededBeads(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-state-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Empty initially
	beads, err := db.ClarificationNeededBeads()
	if err != nil {
		t.Fatal(err)
	}
	if len(beads) != 0 {
		t.Errorf("expected 0 beads, got %d", len(beads))
	}

	// Add two clarification-needed beads
	if err := db.SetClarificationNeeded("BD-1", "anvil-1", true, "reason1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetClarificationNeeded("BD-2", "anvil-1", true, "reason2"); err != nil {
		t.Fatal(err)
	}

	beads, err = db.ClarificationNeededBeads()
	if err != nil {
		t.Fatal(err)
	}
	if len(beads) != 2 {
		t.Errorf("expected 2 beads, got %d", len(beads))
	}

	// Clear one
	if err := db.SetClarificationNeeded("BD-1", "anvil-1", false, ""); err != nil {
		t.Fatal(err)
	}
	beads, err = db.ClarificationNeededBeads()
	if err != nil {
		t.Fatal(err)
	}
	if len(beads) != 1 {
		t.Errorf("expected 1 bead, got %d", len(beads))
	}
	if beads[0].BeadID != "BD-2" {
		t.Errorf("expected BD-2, got %s", beads[0].BeadID)
	}
}

func TestDB_PendingRetries_ExcludesClarification(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-state-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Insert a normal retry record
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	if err := db.UpsertRetry(&RetryRecord{
		BeadID:     "BD-NORMAL",
		Anvil:      "anvil-1",
		RetryCount: 1,
		NextRetry:  &past,
	}); err != nil {
		t.Fatal(err)
	}

	// Insert a clarification-needed retry record
	if err := db.UpsertRetry(&RetryRecord{
		BeadID:              "BD-CLAR",
		Anvil:               "anvil-1",
		RetryCount:          0,
		ClarificationNeeded: true,
		NextRetry:           &past,
	}); err != nil {
		t.Fatal(err)
	}

	pending, err := db.PendingRetries()
	if err != nil {
		t.Fatal(err)
	}

	// Only the normal one should appear
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending retry, got %d", len(pending))
	}
	if pending[0].BeadID != "BD-NORMAL" {
		t.Errorf("expected BD-NORMAL, got %s", pending[0].BeadID)
	}
}
