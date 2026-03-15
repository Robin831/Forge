package state

import (
	"fmt"
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
	if err := db.UpdatePRLifecycle(pr.ID, 5, 3, 0, false); err != nil {
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

	// 3b. Section ordering: ready → unlabeled → in_progress, then priority within section.
	// An empty Section is normalized to QueueSectionReady on insert.
	sectioned := []QueueItem{
		{BeadID: "bd-s3", Anvil: "anvil-a", Title: "In progress bead", Priority: 1, Status: "in_progress", Section: QueueSectionInProgress},
		{BeadID: "bd-s1", Anvil: "anvil-a", Title: "Ready bead", Priority: 2, Status: "open", Section: QueueSectionReady},
		{BeadID: "bd-s2", Anvil: "anvil-a", Title: "Unlabeled bead", Priority: 1, Status: "open", Section: QueueSectionUnlabeled},
		{BeadID: "bd-s4", Anvil: "anvil-a", Title: "Empty section normalizes to ready", Priority: 0, Status: "open", Section: ""},
	}
	if err := db.ReplaceQueueCacheForAnvils([]string{"anvil-a", "anvil-b", "anvil-c"}, sectioned); err != nil {
		t.Fatal(err)
	}
	items, err = db.QueueCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 sectioned items, got %d", len(items))
	}
	// bd-s4 has empty section (normalized to ready) and priority 0, so it sorts first
	if items[0].BeadID != "bd-s4" || items[0].Section != QueueSectionReady {
		t.Errorf("expected bd-s4 (normalized ready, priority 0) first, got %s (%s)", items[0].BeadID, items[0].Section)
	}
	// bd-s1 is ready with priority 2, second among ready items
	if items[1].BeadID != "bd-s1" || items[1].Section != QueueSectionReady {
		t.Errorf("expected bd-s1 (ready) second, got %s (%s)", items[1].BeadID, items[1].Section)
	}
	// unlabeled third
	if items[2].BeadID != "bd-s2" || items[2].Section != QueueSectionUnlabeled {
		t.Errorf("expected bd-s2 (unlabeled) third, got %s (%s)", items[2].BeadID, items[2].Section)
	}
	// in_progress last
	if items[3].BeadID != "bd-s3" || items[3].Section != QueueSectionInProgress {
		t.Errorf("expected bd-s3 (in_progress) last, got %s (%s)", items[3].BeadID, items[3].Section)
	}

	// 3c. Labels round-trip: nil/empty labels stored as "[]", not "null"
	withLabels := []QueueItem{
		{BeadID: "bd-l1", Anvil: "anvil-l", Title: "Has labels", Priority: 1, Status: "open", Labels: `["dispatch"]`, Section: QueueSectionReady},
		{BeadID: "bd-l2", Anvil: "anvil-l", Title: "No labels (empty JSON array)", Priority: 2, Status: "open", Labels: "[]", Section: QueueSectionUnlabeled}, // Explicit empty JSON array
		{BeadID: "bd-l3", Anvil: "anvil-l", Title: "No labels (empty string)", Priority: 3, Status: "open", Labels: "", Section: QueueSectionUnlabeled},       // Empty string
	}
	if err := db.ReplaceQueueCacheForAnvils([]string{"anvil-a", "anvil-l"}, withLabels); err != nil {
		t.Fatal(err)
	}
	items, err = db.QueueCache()
	if err != nil {
		t.Fatal(err)
	}
	var l1, l2, l3 *QueueItem
	for i := range items {
		switch items[i].BeadID {
		case "bd-l1":
			l1 = &items[i]
		case "bd-l2":
			l2 = &items[i]
		case "bd-l3":
			l3 = &items[i]
		}
	}
	if l1 == nil || l1.Labels != `["dispatch"]` {
		t.Errorf("expected bd-l1 labels=[\"dispatch\"], got %v", l1)
	}
	if l2 == nil || l2.Labels != `[]` {
		t.Errorf("expected bd-l2 labels=[], got %v", l2)
	}
	if l3 == nil || l3.Labels != `[]` {
		t.Errorf("expected bd-l3 labels=[], got %v", l3)
	}

	// 4. Replacing with no items clears the cache for the specified anvils
	if err := db.ReplaceQueueCacheForAnvils([]string{"anvil-a", "anvil-b", "anvil-c", "anvil-l"}, nil); err != nil {
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

func TestDB_QueueCacheDescriptionRoundTrip(t *testing.T) {
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

	input := []QueueItem{
		{BeadID: "bd-d1", Anvil: "anvil-a", Title: "With desc", Description: "A detailed description", Priority: 1, Status: "open"},
		{BeadID: "bd-d2", Anvil: "anvil-a", Title: "No desc", Description: "", Priority: 2, Status: "open"},
	}
	if err := db.ReplaceQueueCacheForAnvils([]string{"anvil-a"}, input); err != nil {
		t.Fatal(err)
	}

	items, err := db.QueueCache()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].Description != "A detailed description" {
		t.Errorf("expected description 'A detailed description', got %q", items[0].Description)
	}
	if items[1].Description != "" {
		t.Errorf("expected empty description, got %q", items[1].Description)
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
	if err != nil {
		t.Fatalf("unexpected error from GetRetry: %v", err)
	}
	if r != nil {
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
	// Reason should be preserved when clearing; clearing clarification does not overwrite LastError
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

func TestDB_ClarificationNeededBeads_ExcludesNeedsHuman(t *testing.T) {
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

	// Set clarification_needed on two beads
	if err := db.SetClarificationNeeded("BD-1", "anvil-1", true, "reason1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetClarificationNeeded("BD-2", "anvil-1", true, "reason2"); err != nil {
		t.Fatal(err)
	}

	// Also mark BD-2 as needs_human (simulating exhausted retries)
	if err := db.UpsertRetry(&RetryRecord{BeadID: "BD-2", Anvil: "anvil-1", NeedsHuman: true, ClarificationNeeded: true, LastError: "reason2"}); err != nil {
		t.Fatal(err)
	}

	beads, err := db.ClarificationNeededBeads()
	if err != nil {
		t.Fatal(err)
	}
	if len(beads) != 1 {
		t.Errorf("expected 1 bead (needs_human should be excluded), got %d", len(beads))
	}
	if len(beads) > 0 && beads[0].BeadID != "BD-1" {
		t.Errorf("expected BD-1, got %s", beads[0].BeadID)
	}
}

func TestDB_ResetRetry(t *testing.T) {
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

	// ResetRetry on non-existent record should return error.
	if err := db.ResetRetry("BD-MISSING", "anvil-1"); err == nil {
		t.Error("expected error for missing bead, got nil")
	}

	// Insert a retry record with flags set.
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	if err := db.UpsertRetry(&RetryRecord{
		BeadID:              "BD-1",
		Anvil:               "anvil-1",
		RetryCount:          3,
		DispatchFailures:    3,
		NeedsHuman:          true,
		ClarificationNeeded: true,
		LastError:           "something went wrong",
		NextRetry:           &past,
	}); err != nil {
		t.Fatal(err)
	}

	// ResetRetry should clear flags and reset count.
	if err := db.ResetRetry("BD-1", "anvil-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, err := db.GetRetry("BD-1", "anvil-1")
	if err != nil {
		t.Fatal(err)
	}
	if r == nil {
		t.Fatal("expected record to still exist after reset")
	}
	if r.NeedsHuman {
		t.Error("expected NeedsHuman=false after reset")
	}
	if r.ClarificationNeeded {
		t.Error("expected ClarificationNeeded=false after reset")
	}
	if r.RetryCount != 0 {
		t.Errorf("expected RetryCount=0 after reset, got %d", r.RetryCount)
	}
	if r.DispatchFailures != 0 {
		t.Errorf("expected DispatchFailures=0 after reset, got %d", r.DispatchFailures)
	}
}

func TestDB_DismissRetry(t *testing.T) {
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

	// DismissRetry on non-existent record should return error.
	if err := db.DismissRetry("BD-MISSING", "anvil-1"); err == nil {
		t.Error("expected error for missing bead, got nil")
	}

	// Insert a retry record.
	if err := db.UpsertRetry(&RetryRecord{
		BeadID:     "BD-2",
		Anvil:      "anvil-1",
		NeedsHuman: true,
		LastError:  "too many retries",
	}); err != nil {
		t.Fatal(err)
	}

	// DismissRetry should remove the record entirely.
	if err := db.DismissRetry("BD-2", "anvil-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, err := db.GetRetry("BD-2", "anvil-1")
	if err != nil {
		t.Fatal(err)
	}
	if r != nil {
		t.Error("expected record to be deleted after dismiss, but it still exists")
	}
}

func TestDB_LastWorkerLogPath(t *testing.T) {
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

	// No workers: should return empty string with no error.
	logPath, err := db.LastWorkerLogPath("BD-NONE")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if logPath != "" {
		t.Errorf("expected empty log path, got %q", logPath)
	}

	// Insert two workers for the same bead; the most recent should win.
	w1 := &Worker{
		ID:        "worker-1",
		BeadID:    "BD-3",
		Anvil:     "anvil-1",
		Status:    WorkerDone,
		LogPath:   "/logs/first.log",
		StartedAt: time.Now().Add(-2 * time.Minute),
	}
	if err := db.InsertWorker(w1); err != nil {
		t.Fatal(err)
	}
	w2 := &Worker{
		ID:        "worker-2",
		BeadID:    "BD-3",
		Anvil:     "anvil-1",
		Status:    WorkerDone,
		LogPath:   "/logs/latest.log",
		StartedAt: time.Now().Add(-1 * time.Minute),
	}
	if err := db.InsertWorker(w2); err != nil {
		t.Fatal(err)
	}

	logPath, err = db.LastWorkerLogPath("BD-3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if logPath != "/logs/latest.log" {
		t.Errorf("expected latest log path, got %q", logPath)
	}
}

func TestDB_HasOpenPRForBead(t *testing.T) {
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

	// No PR exists
	has, err := db.HasOpenPRForBead("bd-1", "anvil-1")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("expected no open PR initially")
	}

	// Insert an open PR
	if err := db.InsertPR(&PR{
		Number: 42, Anvil: "anvil-1", BeadID: "bd-1",
		Branch: "fix-1", Status: PROpen, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	has, err = db.HasOpenPRForBead("bd-1", "anvil-1")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("expected open PR to be found")
	}

	// Different anvil should not match
	has, err = db.HasOpenPRForBead("bd-1", "anvil-2")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("expected no match for different anvil")
	}

	// Recently-merged PR should still count (grace period protects against
	// orphan recovery racing with async bead close).
	pr2 := &PR{
		Number: 43, Anvil: "anvil-2", BeadID: "bd-2",
		Branch: "fix-2", Status: PRMerged, CreatedAt: time.Now(),
	}
	if err := db.InsertPR(pr2); err != nil {
		t.Fatal(err)
	}
	// Set last_checked to now (simulates just-merged).
	if err := db.UpdatePRStatus(pr2.ID, PRMerged); err != nil {
		t.Fatal(err)
	}
	has, err = db.HasOpenPRForBead("bd-2", "anvil-2")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("recently-merged PR should still count within grace period")
	}

	// Merged PR with last_checked well outside grace period should NOT count.
	_, err = db.conn.Exec(
		`UPDATE prs SET last_checked = ? WHERE id = ?`,
		time.Now().Add(-mergedPRGracePeriod-time.Hour).Format(dbTimeLayout),
		pr2.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	has, err = db.HasOpenPRForBead("bd-2", "anvil-2")
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("old merged PR should not count as open")
	}
}

func TestDB_StalledWorkers(t *testing.T) {
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

	// Create a log file that is "old" (modified 10 minutes ago)
	logFile := filepath.Join(tmpDir, "smith-old.log")
	if err := os.WriteFile(logFile, []byte("some log output"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(logFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// Create a log file that is "fresh"
	freshLog := filepath.Join(tmpDir, "smith-fresh.log")
	if err := os.WriteFile(freshLog, []byte("recent output"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Insert workers
	if err := db.InsertWorker(&Worker{
		ID:        "w-old",
		BeadID:    "BD-1",
		Anvil:     "anvil-1",
		Status:    WorkerRunning,
		Phase:     "smith",
		StartedAt: time.Now().Add(-15 * time.Minute),
		LogPath:   logFile,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertWorker(&Worker{
		ID:        "w-fresh",
		BeadID:    "BD-2",
		Anvil:     "anvil-1",
		Status:    WorkerRunning,
		Phase:     "smith",
		StartedAt: time.Now().Add(-5 * time.Minute),
		LogPath:   freshLog,
	}); err != nil {
		t.Fatal(err)
	}

	// Query with 5-minute threshold — only the old worker should be stale
	stalled, err := db.StalledWorkers(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(stalled) != 1 {
		t.Fatalf("expected 1 stalled worker, got %d", len(stalled))
	}
	if stalled[0].ID != "w-old" {
		t.Errorf("expected w-old, got %s", stalled[0].ID)
	}

	// Mark as stalled
	if err := db.MarkWorkerStalled("w-old"); err != nil {
		t.Fatal(err)
	}

	// Verify status changed
	workers, err := db.ActiveWorkers()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, w := range workers {
		if w.ID == "w-old" {
			found = true
			if w.Status != WorkerStalled {
				t.Errorf("expected stalled status, got %s", w.Status)
			}
		}
	}
	if !found {
		t.Error("stalled worker should still appear in ActiveWorkers")
	}

	// Stalled worker should appear in NeedsAttentionBeads
	attention, err := db.NeedsAttentionBeads(DefaultMaxCIFixAttempts, DefaultMaxReviewFixAttempts, DefaultMaxRebaseAttempts)
	if err != nil {
		t.Fatal(err)
	}
	if len(attention) != 1 {
		t.Fatalf("expected 1 needs-attention item, got %d", len(attention))
	}
	if attention[0].BeadID != "BD-1" {
		t.Errorf("expected BD-1 in needs attention, got %s", attention[0].BeadID)
	}
	if attention[0].Reason == "" {
		t.Error("expected a reason for the stalled worker")
	}
}

func TestDB_StalledWorkers_ExcludesLongRunningPhases(t *testing.T) {
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

	// Create stale log files for all workers
	makeStaleLog := func(name string) string {
		p := filepath.Join(tmpDir, name)
		if err := os.WriteFile(p, []byte("log"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-20 * time.Minute)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Smith worker — should be flagged as stale
	if err := db.InsertWorker(&Worker{
		ID: "w-smith", BeadID: "BD-1", Anvil: "anvil-1",
		Status: WorkerRunning, Phase: "smith",
		StartedAt: time.Now().Add(-25 * time.Minute),
		LogPath:   makeStaleLog("smith.log"),
	}); err != nil {
		t.Fatal(err)
	}
	// Bellows worker — should be excluded
	if err := db.InsertWorker(&Worker{
		ID: "w-bellows", BeadID: "BD-2", Anvil: "anvil-1",
		Status: WorkerRunning, Phase: "bellows",
		StartedAt: time.Now().Add(-25 * time.Minute),
		LogPath:   makeStaleLog("bellows.log"),
	}); err != nil {
		t.Fatal(err)
	}
	// Cifix worker — should be excluded
	if err := db.InsertWorker(&Worker{
		ID: "w-cifix", BeadID: "BD-3", Anvil: "anvil-1",
		Status: WorkerRunning, Phase: "quench",
		StartedAt: time.Now().Add(-25 * time.Minute),
		LogPath:   makeStaleLog("quench.log"),
	}); err != nil {
		t.Fatal(err)
	}
	// Reviewfix worker — should be excluded
	if err := db.InsertWorker(&Worker{
		ID: "w-reviewfix", BeadID: "BD-4", Anvil: "anvil-1",
		Status: WorkerRunning, Phase: "burnish",
		StartedAt: time.Now().Add(-25 * time.Minute),
		LogPath:   makeStaleLog("burnish.log"),
	}); err != nil {
		t.Fatal(err)
	}

	stalled, err := db.StalledWorkers(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(stalled) != 1 {
		t.Fatalf("expected 1 stalled worker (smith only), got %d", len(stalled))
	}
	if stalled[0].ID != "w-smith" {
		t.Errorf("expected w-smith, got %s", stalled[0].ID)
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

func TestDB_ExhaustedPRs(t *testing.T) {
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

	// Insert a PR with ci_fix_count at the threshold.
	exhaustedCI := &PR{
		Number: 10, Anvil: "anvil-1", BeadID: "bd-ci", Branch: "fix-ci",
		Status: PROpen, CreatedAt: time.Now(),
	}
	if err := db.InsertPR(exhaustedCI); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdatePRLifecycle(exhaustedCI.ID, 5, 0, 0, true); err != nil {
		t.Fatal(err)
	}

	// Insert a PR with review_fix_count over the threshold.
	exhaustedRev := &PR{
		Number: 11, Anvil: "anvil-1", BeadID: "bd-rev", Branch: "fix-rev",
		Status: PROpen, CreatedAt: time.Now(),
	}
	if err := db.InsertPR(exhaustedRev); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdatePRLifecycle(exhaustedRev.ID, 0, 6, 0, true); err != nil {
		t.Fatal(err)
	}

	// Insert a PR with rebase_count at the threshold.
	exhaustedRebase := &PR{
		Number: 12, Anvil: "anvil-1", BeadID: "bd-rebase", Branch: "fix-rebase",
		Status: PROpen, CreatedAt: time.Now(),
	}
	if err := db.InsertPR(exhaustedRebase); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdatePRLifecycle(exhaustedRebase.ID, 0, 0, 3, true); err != nil {
		t.Fatal(err)
	}

	// Insert a healthy PR that should NOT appear.
	healthy := &PR{
		Number: 13, Anvil: "anvil-1", BeadID: "bd-ok", Branch: "fix-ok",
		Status: PROpen, CreatedAt: time.Now(),
	}
	if err := db.InsertPR(healthy); err != nil {
		t.Fatal(err)
	}

	exhausted, err := db.ExhaustedPRs(5, 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(exhausted) != 3 {
		t.Fatalf("expected 3 exhausted PRs, got %d", len(exhausted))
	}

	// Verify the Reason field is populated with meaningful text.
	var foundCI, foundRev, foundRebase bool
	for _, ep := range exhausted {
		switch ep.BeadID {
		case "bd-ci":
			foundCI = true
			if ep.Reason == "" {
				t.Error("expected non-empty Reason for CI-exhausted PR")
			}
		case "bd-rev":
			foundRev = true
			if ep.Reason == "" {
				t.Error("expected non-empty Reason for review-exhausted PR")
			}
		case "bd-rebase":
			foundRebase = true
			if ep.Reason == "" {
				t.Error("expected non-empty Reason for rebase-exhausted PR")
			}
		}
	}
	if !foundCI || !foundRev || !foundRebase {
		t.Errorf("missing exhausted PR: ci=%v rev=%v rebase=%v", foundCI, foundRev, foundRebase)
	}

	// Zero thresholds are normalized to defaults — should produce the same result.
	exhaustedDefaults, err := db.ExhaustedPRs(0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(exhaustedDefaults) != len(exhausted) {
		t.Errorf("zero thresholds should fall back to defaults: got %d want %d", len(exhaustedDefaults), len(exhausted))
	}
}

func TestDB_ResetPRFixCounts(t *testing.T) {
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

	pr := &PR{
		Number: 20, Anvil: "anvil-1", BeadID: "bd-reset", Branch: "fix-reset",
		Status: PROpen, CreatedAt: time.Now(),
	}
	if err := db.InsertPR(pr); err != nil {
		t.Fatal(err)
	}
	// Drive it to exhaustion with ci_passing=false.
	if err := db.UpdatePRLifecycle(pr.ID, 5, 3, 2, false); err != nil {
		t.Fatal(err)
	}

	// Should appear in exhausted list.
	exhausted, err := db.ExhaustedPRs(5, 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(exhausted) != 1 {
		t.Fatalf("expected 1 exhausted PR before reset, got %d", len(exhausted))
	}

	// Reset.
	if err := db.ResetPRFixCounts(pr.ID); err != nil {
		t.Fatal(err)
	}

	// Should no longer appear in exhausted list.
	exhausted, err = db.ExhaustedPRs(5, 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(exhausted) != 0 {
		t.Errorf("expected 0 exhausted PRs after reset, got %d", len(exhausted))
	}

	// Counters and ci_passing should be reset.
	pr2, err := db.GetPRByNumber("anvil-1", 20)
	if err != nil || pr2 == nil {
		t.Fatal("PR not found after reset")
	}
	if pr2.CIFixCount != 0 || pr2.ReviewFixCount != 0 || pr2.RebaseCount != 0 {
		t.Errorf("counts not reset: ci=%d rev=%d rebase=%d", pr2.CIFixCount, pr2.ReviewFixCount, pr2.RebaseCount)
	}
	if !pr2.CIPassing {
		t.Error("ci_passing should be reset to true")
	}
	if pr2.IsConflicting {
		t.Error("is_conflicting should be reset to false")
	}
	if pr2.HasUnresolvedThreads {
		t.Error("has_unresolved_threads should be reset to false")
	}
	if pr2.Status != PROpen {
		t.Errorf("status should be open after reset, got %s", pr2.Status)
	}
}

func TestDB_DismissExhaustedPR(t *testing.T) {
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

	pr := &PR{
		Number: 30, Anvil: "anvil-1", BeadID: "bd-dismiss", Branch: "fix-dismiss",
		Status: PROpen, CreatedAt: time.Now(),
	}
	if err := db.InsertPR(pr); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdatePRLifecycle(pr.ID, 5, 0, 0, true); err != nil {
		t.Fatal(err)
	}

	// Confirm it appears as exhausted.
	exhausted, err := db.ExhaustedPRs(5, 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(exhausted) != 1 {
		t.Fatalf("expected 1 exhausted PR, got %d", len(exhausted))
	}

	// Dismiss it.
	if err := db.DismissExhaustedPR(pr.ID); err != nil {
		t.Fatal(err)
	}

	// Should no longer appear in exhausted list (terminal status).
	exhausted, err = db.ExhaustedPRs(5, 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(exhausted) != 0 {
		t.Errorf("expected 0 exhausted PRs after dismiss, got %d", len(exhausted))
	}

	// PR status should be closed.
	pr2, err := db.GetPRByNumber("anvil-1", 30)
	if err != nil || pr2 == nil {
		t.Fatal("PR not found after dismiss")
	}
	if pr2.Status != PRClosed {
		t.Errorf("expected status closed after dismiss, got %s", pr2.Status)
	}
}

func TestDB_NeedsAttentionBeads_IncludesExhaustedPRs(t *testing.T) {
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

	// Insert an exhausted PR.
	pr := &PR{
		Number: 40, Anvil: "anvil-1", BeadID: "bd-na", Branch: "fix-na",
		Status: PROpen, CreatedAt: time.Now(),
	}
	if err := db.InsertPR(pr); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdatePRLifecycle(pr.ID, 5, 0, 0, true); err != nil {
		t.Fatal(err)
	}

	beads, err := db.NeedsAttentionBeads(5, 5, 3)
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, b := range beads {
		if b.PRID == pr.ID && b.PRNumber == 40 && b.BeadID == "bd-na" {
			found = true
			if b.Reason == "" {
				t.Error("expected non-empty Reason for exhausted PR in NeedsAttentionBeads")
			}
		}
	}
	if !found {
		t.Error("exhausted PR not found in NeedsAttentionBeads results")
	}
}

func TestDB_ReadyToMerge(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-rtm-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	insert := func(number int, status PRStatus, ciPassing, conflicting, unresolvedThreads, pendingReviews bool) *PR {
		t.Helper()
		pr := &PR{
			Number:    number,
			Anvil:     "anvil-rtm",
			BeadID:    fmt.Sprintf("bd-%d", number),
			Branch:    fmt.Sprintf("branch-%d", number),
			Status:    status,
			CreatedAt: time.Now(),
		}
		if err := db.InsertPR(pr); err != nil {
			t.Fatalf("InsertPR: %v", err)
		}
		// ci_passing defaults to 1 on insert; update lifecycle to set false if needed.
		if err := db.UpdatePRLifecycle(pr.ID, 0, 0, 0, ciPassing); err != nil {
			t.Fatalf("UpdatePRLifecycle: %v", err)
		}
		if err := db.UpdatePRMergeability(pr.ID, ciPassing, conflicting, unresolvedThreads, pendingReviews, false); err != nil {
			t.Fatalf("UpdatePRMergeability: %v", err)
		}
		return pr
	}

	// approved, CI passing, not conflicting, no unresolved threads, no pending reviews → ready
	prReady := insert(201, PRApproved, true, false, false, false)
	// approved but CI failing → not ready
	prCIFail := insert(202, PRApproved, false, false, false, false)
	// approved, CI passing, conflicting → not ready
	prConflict := insert(203, PRApproved, true, true, false, false)
	// approved, CI passing, has unresolved threads → not ready
	prThreads := insert(204, PRApproved, true, false, true, false)
	// open (not approved) but all conditions met → ready (approval not required)
	prOpen := insert(205, PROpen, true, false, false, false)
	// needs_fix → not ready (active fix cycle)
	prNeedsFix := insert(206, PRNeedsFix, true, false, false, false)
	// approved, CI passing, but has pending review requests → not ready
	prPendingReview := insert(207, PRApproved, true, false, false, true)

	// IsPRReadyToMerge
	cases := []struct {
		pr   *PR
		want bool
		name string
	}{
		{prReady, true, "approved+ci+no_conflict+no_threads"},
		{prCIFail, false, "ci_failing"},
		{prConflict, false, "conflicting"},
		{prThreads, false, "unresolved_threads"},
		{prOpen, true, "open_all_conditions_met"},
		{prNeedsFix, false, "needs_fix"},
		{prPendingReview, false, "pending_reviews"},
	}
	for _, tc := range cases {
		got, err := db.IsPRReadyToMerge(tc.pr.ID)
		if err != nil {
			t.Fatalf("IsPRReadyToMerge(%s): %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("IsPRReadyToMerge(%s): got %v, want %v", tc.name, got, tc.want)
		}
	}

	// ReadyToMergePRs should return prReady and prOpen (both meet conditions)
	ready, err := db.ReadyToMergePRs()
	if err != nil {
		t.Fatalf("ReadyToMergePRs: %v", err)
	}
	if len(ready) != 2 {
		t.Fatalf("ReadyToMergePRs: expected 2 results, got %d", len(ready))
	}

	// UpdatePRMergeability: make prConflict non-conflicting → now ready
	if err := db.UpdatePRMergeability(prConflict.ID, true, false, false, false, false); err != nil {
		t.Fatalf("UpdatePRMergeability clear: %v", err)
	}
	ready, err = db.ReadyToMergePRs()
	if err != nil {
		t.Fatalf("ReadyToMergePRs after update: %v", err)
	}
	if len(ready) != 3 {
		t.Errorf("ReadyToMergePRs after update: expected 3 results, got %d", len(ready))
	}
}

// TestDB_InsertPR_DefaultsPendingReviews verifies that newly inserted PRs
// default to has_pending_reviews=1 so they don't appear in Ready to Merge
// until bellows confirms no reviews are pending.
func TestDB_InsertPR_DefaultsPendingReviews(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-pending-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	pr := &PR{
		Number:    999,
		Anvil:     "anvil-pending",
		BeadID:    "bd-999",
		Branch:    "branch-999",
		Status:    PROpen,
		CreatedAt: time.Now(),
	}
	if err := db.InsertPR(pr); err != nil {
		t.Fatalf("InsertPR: %v", err)
	}
	// Make CI pass explicitly (default is 1 so it should already be passing).
	if err := db.UpdatePRLifecycle(pr.ID, 0, 0, 0, true); err != nil {
		t.Fatalf("UpdatePRLifecycle: %v", err)
	}

	// Without calling UpdatePRMergeability, the PR should NOT be ready to merge
	// because has_pending_reviews defaults to 1.
	ready, err := db.IsPRReadyToMerge(pr.ID)
	if err != nil {
		t.Fatalf("IsPRReadyToMerge: %v", err)
	}
	if ready {
		t.Error("newly inserted PR should not be ready to merge (has_pending_reviews should default to 1)")
	}

	// After bellows confirms no pending reviews, the PR should be ready.
	if err := db.UpdatePRMergeability(pr.ID, true, false, false, false, false); err != nil {
		t.Fatalf("UpdatePRMergeability: %v", err)
	}
	ready, err = db.IsPRReadyToMerge(pr.ID)
	if err != nil {
		t.Fatalf("IsPRReadyToMerge: %v", err)
	}
	if !ready {
		t.Error("PR should be ready to merge after bellows confirms no pending reviews")
	}
}

func TestDB_HasWorkerRecord(t *testing.T) {
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

	// No record yet — should return false.
	has, err := db.HasWorkerRecord("bd-orphan-1", "anvil-1")
	if err != nil {
		t.Fatalf("HasWorkerRecord: %v", err)
	}
	if has {
		t.Fatal("expected no worker record before insert")
	}

	// Insert a worker (any status).
	w := &Worker{
		ID:        "w-1",
		BeadID:    "bd-orphan-1",
		Anvil:     "anvil-1",
		Status:    WorkerDone,
		StartedAt: time.Now(),
	}
	if err := db.InsertWorker(w); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	// Now should return true.
	has, err = db.HasWorkerRecord("bd-orphan-1", "anvil-1")
	if err != nil {
		t.Fatalf("HasWorkerRecord after insert: %v", err)
	}
	if !has {
		t.Fatal("expected worker record after insert")
	}

	// Different anvil — should return false.
	has, err = db.HasWorkerRecord("bd-orphan-1", "anvil-2")
	if err != nil {
		t.Fatalf("HasWorkerRecord different anvil: %v", err)
	}
	if has {
		t.Fatal("expected no worker record for different anvil")
	}
}

func TestDB_NeedsHumanBeadIDSet(t *testing.T) {
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

	upsert := func(beadID, anvil string, needsHuman bool, lastError string) {
		t.Helper()
		if err := db.UpsertRetry(&RetryRecord{
			BeadID:     beadID,
			Anvil:      anvil,
			NeedsHuman: needsHuman,
			LastError:  lastError,
		}); err != nil {
			t.Fatalf("UpsertRetry(%s): %v", beadID, err)
		}
	}

	// needs_human=1 with dispatch circuit breaker reason
	upsert("bd-1", "anvil-a", true, "circuit breaker: too many failures")
	// needs_human=1 with crucible child failure reason (non-circuit-breaker prefix)
	upsert("bd-2", "anvil-a", true, "crucible child failed: temper rejected")
	// needs_human=1 with exhausted retries reason
	upsert("bd-3", "anvil-b", true, "exhausted retries")
	// needs_human=0 — should NOT appear in set
	upsert("bd-4", "anvil-a", false, "")

	set, err := db.NeedsHumanBeadIDSet()
	if err != nil {
		t.Fatalf("NeedsHumanBeadIDSet: %v", err)
	}

	included := []struct{ id, anvil string }{
		{"bd-1", "anvil-a"},
		{"bd-2", "anvil-a"},
		{"bd-3", "anvil-b"},
	}
	for _, tc := range included {
		key := tc.id + "\x00" + tc.anvil
		if _, ok := set[key]; !ok {
			t.Errorf("expected %s/%s to be in NeedsHumanBeadIDSet", tc.id, tc.anvil)
		}
	}

	// needs_human=0 must be excluded
	excluded := "bd-4\x00anvil-a"
	if _, ok := set[excluded]; ok {
		t.Errorf("expected bd-4/anvil-a to be excluded from NeedsHumanBeadIDSet (needs_human=0)")
	}

	if len(set) != 3 {
		t.Errorf("expected 3 entries in set, got %d", len(set))
	}
}

func TestDB_NeedsAttentionBeads_Description(t *testing.T) {
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

	beadID := "bd-descr"
	anvil := "anvil-1"
	description := "Test description content"

	// 1. Populate queue_cache with description
	err = db.ReplaceQueueCacheForAnvils([]string{anvil}, []QueueItem{
		{
			BeadID:      beadID,
			Anvil:       anvil,
			Title:       "Test Title",
			Description: description,
			Section:     QueueSectionReady,
			Priority:    1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Add retry record with needs_human=1
	err = db.UpsertRetry(&RetryRecord{
		BeadID:     beadID,
		Anvil:      anvil,
		NeedsHuman: true,
		LastError:  "Too many retries",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3. Verify description flows through NeedsAttentionBeads
	beads, err := db.NeedsAttentionBeads(5, 5, 3)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, b := range beads {
		if b.BeadID == beadID && b.Anvil == anvil {
			found = true
			if b.Description != description {
				t.Errorf("expected description %q, got %q", description, b.Description)
			}
			break
		}
	}
	if !found {
		t.Error("bead not found in NeedsAttentionBeads")
	}

	// 4. Verify duplicate-merge path (stalled worker + retry row)
	// Add a stalled worker for the same bead
	err = db.InsertWorker(&Worker{
		ID:        "w-stalled",
		BeadID:    beadID,
		Anvil:     anvil,
		Status:    WorkerStalled,
		Phase:     "smith",
		StartedAt: time.Now().Add(-2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	beads, err = db.NeedsAttentionBeads(5, 5, 3)
	if err != nil {
		t.Fatal(err)
	}

	// Should still have only 1 bead due to merge
	count := 0
	for _, b := range beads {
		if b.BeadID == beadID && b.Anvil == anvil {
			count++
			if b.Description != description {
				t.Errorf("merged bead: expected description %q, got %q", description, b.Description)
			}
		}
	}
	if count != 1 {
		t.Errorf("expected 1 merged bead, got %d", count)
	}
}

func TestDB_LastPollPerAnvil(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-poll-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Insert poll events for two anvils, oldest first.
	logEvent := func(typ EventType, anvil, msg string) {
		t.Helper()
		if err := db.LogEvent(typ, msg, "", anvil); err != nil {
			t.Fatalf("LogEvent(%s/%s): %v", typ, anvil, err)
		}
	}

	logEvent(EventPollError, "anvil-a", "connect timeout")
	logEvent(EventPoll, "anvil-a", "fetched 3 beads")   // newest for anvil-a
	logEvent(EventPoll, "anvil-b", "fetched 0 beads")   // only event for anvil-b
	logEvent(EventPoll, "anvil-c", "fetched 1 bead")    // not requested

	t.Run("returns latest row per requested anvil", func(t *testing.T) {
		results, err := db.LastPollPerAnvil([]string{"anvil-a", "anvil-b"})
		if err != nil {
			t.Fatalf("LastPollPerAnvil: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("expected 2 results, got %d", len(results))
		}
		byAnvil := make(map[string]AnvilPollStatus)
		for _, r := range results {
			byAnvil[r.Anvil] = r
		}
		if !byAnvil["anvil-a"].OK {
			t.Error("anvil-a: expected OK=true (latest event is poll, not poll_error)")
		}
		if byAnvil["anvil-a"].Message != "fetched 3 beads" {
			t.Errorf("anvil-a: unexpected message %q", byAnvil["anvil-a"].Message)
		}
		if !byAnvil["anvil-b"].OK {
			t.Error("anvil-b: expected OK=true")
		}
		// anvil-c was not requested and should not appear.
		if _, ok := byAnvil["anvil-c"]; ok {
			t.Error("anvil-c should not be returned when not requested")
		}
	})

	t.Run("poll_error sets OK=false", func(t *testing.T) {
		logEvent(EventPollError, "anvil-b", "network error") // make anvil-b newest = error
		results, err := db.LastPollPerAnvil([]string{"anvil-b"})
		if err != nil {
			t.Fatalf("LastPollPerAnvil: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("expected 1 result, got %d", len(results))
		}
		if results[0].OK {
			t.Error("expected OK=false when latest event is poll_error")
		}
	})

	t.Run("anvil with no history is omitted", func(t *testing.T) {
		results, err := db.LastPollPerAnvil([]string{"anvil-unknown"})
		if err != nil {
			t.Fatalf("LastPollPerAnvil: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("expected no results for unknown anvil, got %d", len(results))
		}
	})

	t.Run("empty input returns nil", func(t *testing.T) {
		results, err := db.LastPollPerAnvil(nil)
		if err != nil {
			t.Fatalf("LastPollPerAnvil: %v", err)
		}
		if results != nil {
			t.Errorf("expected nil for empty input, got %v", results)
		}
	})

	t.Run("duplicate anvil names handled correctly", func(t *testing.T) {
		// Passing the same anvil name twice should not cause an extra result.
		results, err := db.LastPollPerAnvil([]string{"anvil-a", "anvil-a"})
		if err != nil {
			t.Fatalf("LastPollPerAnvil: %v", err)
		}
		if len(results) != 1 {
			t.Errorf("expected 1 result for duplicate anvil name, got %d", len(results))
		}
	})

	t.Run("all-empty-strings input returns nil without querying", func(t *testing.T) {
		results, err := db.LastPollPerAnvil([]string{"", ""})
		if err != nil {
			t.Fatalf("LastPollPerAnvil: %v", err)
		}
		if results != nil {
			t.Errorf("expected nil for all-empty-string input, got %v", results)
		}
	})
}

func TestDB_OpenPRsWithDetail(t *testing.T) {
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

	now := time.Now()

	// Insert PRs in various statuses.
	prs := []PR{
		{Number: 1, Anvil: "anvil-a", BeadID: "bd-10", Branch: "fix-10", Status: PROpen, CreatedAt: now},
		{Number: 2, Anvil: "anvil-a", BeadID: "bd-20", Branch: "fix-20", Status: PRApproved, CreatedAt: now.Add(time.Second)},
		{Number: 3, Anvil: "anvil-a", BeadID: "bd-30", Branch: "fix-30", Status: PRNeedsFix, CreatedAt: now.Add(2 * time.Second)},
		{Number: 4, Anvil: "anvil-a", BeadID: "bd-40", Branch: "fix-40", Status: PRMerged, CreatedAt: now.Add(3 * time.Second)},
		{Number: 5, Anvil: "anvil-a", BeadID: "bd-50", Branch: "fix-50", Status: PRClosed, CreatedAt: now.Add(4 * time.Second)},
	}
	for i := range prs {
		if err := db.InsertPR(&prs[i]); err != nil {
			t.Fatalf("InsertPR #%d: %v", prs[i].Number, err)
		}
	}

	// Set boolean flags on PR #1 for flag mapping verification.
	_, err = db.conn.Exec(
		`UPDATE prs SET ci_passing = 0, is_conflicting = 1, has_unresolved_threads = 1,
		 has_pending_reviews = 0, has_approval = 1, ci_fix_count = 3, review_fix_count = 2, rebase_count = 1
		 WHERE number = 1 AND anvil = 'anvil-a'`)
	if err != nil {
		t.Fatalf("update PR flags: %v", err)
	}

	// Insert queue_cache title for bd-10 (highest-priority title source).
	_, err = db.conn.Exec(
		`INSERT INTO queue_cache (bead_id, anvil, title, priority, status, updated_at)
		 VALUES ('bd-10', 'anvil-a', 'Title from queue_cache', 2, 'open', ?)`,
		now.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert queue_cache: %v", err)
	}

	// Insert a worker for bd-10 too — queue_cache should take precedence.
	if err := db.InsertWorker(&Worker{
		ID:        "w-10",
		BeadID:    "bd-10",
		Anvil:     "anvil-a",
		Branch:    "fix-10",
		Status:    WorkerDone,
		Title:     "Title from worker",
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertWorker w-10: %v", err)
	}

	// Insert a worker for bd-20 (no queue_cache) — title should fall back to worker.
	if err := db.InsertWorker(&Worker{
		ID:        "w-20",
		BeadID:    "bd-20",
		Anvil:     "anvil-a",
		Branch:    "fix-20",
		Status:    WorkerDone,
		Title:     "Title from worker fallback",
		StartedAt: now,
	}); err != nil {
		t.Fatalf("InsertWorker w-20: %v", err)
	}

	// bd-30 has neither queue_cache nor worker — title should be empty.

	results, err := db.OpenPRsWithDetail()
	if err != nil {
		t.Fatalf("OpenPRsWithDetail: %v", err)
	}

	// Should exclude merged (#4) and closed (#5).
	if len(results) != 3 {
		t.Fatalf("expected 3 open PRs, got %d", len(results))
	}

	// Results are ordered by created_at, so #1, #2, #3.
	t.Run("title resolved from queue_cache first", func(t *testing.T) {
		if results[0].BeadID != "bd-10" {
			t.Fatalf("expected bd-10, got %s", results[0].BeadID)
		}
		if results[0].Title != "Title from queue_cache" {
			t.Errorf("expected queue_cache title, got %q", results[0].Title)
		}
	})

	t.Run("title falls back to worker", func(t *testing.T) {
		if results[1].BeadID != "bd-20" {
			t.Fatalf("expected bd-20, got %s", results[1].BeadID)
		}
		if results[1].Title != "Title from worker fallback" {
			t.Errorf("expected worker fallback title, got %q", results[1].Title)
		}
	})

	t.Run("title empty when no source", func(t *testing.T) {
		if results[2].BeadID != "bd-30" {
			t.Fatalf("expected bd-30, got %s", results[2].BeadID)
		}
		if results[2].Title != "" {
			t.Errorf("expected empty title, got %q", results[2].Title)
		}
	})

	t.Run("boolean flag mapping", func(t *testing.T) {
		pr := results[0] // PR #1 with custom flags
		if pr.CIPassing {
			t.Error("expected CIPassing=false")
		}
		if !pr.IsConflicting {
			t.Error("expected IsConflicting=true")
		}
		if !pr.HasUnresolvedThreads {
			t.Error("expected HasUnresolvedThreads=true")
		}
		if pr.HasPendingReviews {
			t.Error("expected HasPendingReviews=false")
		}
		if !pr.HasApproval {
			t.Error("expected HasApproval=true")
		}
	})

	t.Run("integer counts", func(t *testing.T) {
		pr := results[0]
		if pr.CIFixCount != 3 {
			t.Errorf("CIFixCount: got %d, want 3", pr.CIFixCount)
		}
		if pr.ReviewFixCount != 2 {
			t.Errorf("ReviewFixCount: got %d, want 2", pr.ReviewFixCount)
		}
		if pr.RebaseCount != 1 {
			t.Errorf("RebaseCount: got %d, want 1", pr.RebaseCount)
		}
	})

	t.Run("status values preserved", func(t *testing.T) {
		if results[0].Status != PROpen {
			t.Errorf("expected PROpen, got %s", results[0].Status)
		}
		if results[1].Status != PRApproved {
			t.Errorf("expected PRApproved, got %s", results[1].Status)
		}
		if results[2].Status != PRNeedsFix {
			t.Errorf("expected PRNeedsFix, got %s", results[2].Status)
		}
	})

	t.Run("basic fields populated", func(t *testing.T) {
		pr := results[0]
		if pr.ID == 0 {
			t.Error("expected non-zero ID")
		}
		if pr.Number != 1 {
			t.Errorf("Number: got %d, want 1", pr.Number)
		}
		if pr.Anvil != "anvil-a" {
			t.Errorf("Anvil: got %q, want anvil-a", pr.Anvil)
		}
		if pr.Branch != "fix-10" {
			t.Errorf("Branch: got %q, want fix-10", pr.Branch)
		}
	})
}
