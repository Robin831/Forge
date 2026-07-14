package state

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDB_UpdateWorkerSession(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-state-session-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	w := &Worker{
		ID:        "worker-session-1",
		BeadID:    "bd-session",
		Anvil:     "anvil-1",
		Status:    WorkerRunning,
		StartedAt: time.Now(),
	}
	if err := db.InsertWorker(w); err != nil {
		t.Fatal(err)
	}

	// Defaults are empty before any session is recorded.
	got, err := db.WorkersByBead("bd-session", "anvil-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 worker, got %d", len(got))
	}
	if got[0].SessionID != "" || got[0].Model != "" {
		t.Errorf("expected empty session/model defaults, got session=%q model=%q", got[0].SessionID, got[0].Model)
	}

	if err := db.UpdateWorkerSession("worker-session-1", "sess-abc", "claude-opus-4-8"); err != nil {
		t.Fatal(err)
	}

	got, err = db.WorkersByBead("bd-session", "anvil-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", got[0].SessionID, "sess-abc")
	}
	if got[0].Model != "claude-opus-4-8" {
		t.Errorf("Model = %q, want %q", got[0].Model, "claude-opus-4-8")
	}
}

func TestWorker_ResumeState(t *testing.T) {
	full := &Worker{ID: "w1", BeadID: "bd-1", Anvil: "anvil-1", Branch: "forge/bd-1", SessionID: "sess-abc"}
	rs, err := full.ResumeState()
	if err != nil {
		t.Fatalf("ResumeState on complete worker: %v", err)
	}
	if rs.BeadID != "bd-1" || rs.Anvil != "anvil-1" || rs.Branch != "forge/bd-1" || rs.SessionID != "sess-abc" {
		t.Errorf("ResumeState = %+v; fields not carried through", rs)
	}

	cases := []struct {
		name   string
		worker *Worker
	}{
		{"missing branch", &Worker{ID: "w2", Anvil: "anvil-1", SessionID: "sess"}},
		{"missing anvil", &Worker{ID: "w3", Branch: "forge/bd", SessionID: "sess"}},
		{"missing session_id", &Worker{ID: "w4", Anvil: "anvil-1", Branch: "forge/bd"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.worker.ResumeState(); err == nil {
				t.Errorf("ResumeState should fail for %s", tc.name)
			}
		})
	}
}

// TestDB_ResumableWorkerByBeadID verifies the DB-side gate that selects the
// worker row a resume-with-message can act on: a row is resumable only when it
// records BOTH a branch and a session_id, and when several qualify the most
// recently started one wins. This is the backend counterpart of the UI's
// "resume only when a branch exists" gating.
func TestDB_ResumableWorkerByBeadID(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// insert stamps the session_id after the row exists (InsertWorker does not
	// persist session_id; UpdateWorkerSession does), mirroring the real flow.
	insert := func(id, beadID, anvil, branch, session string, started time.Time) {
		t.Helper()
		if err := db.InsertWorker(&Worker{
			ID: id, BeadID: beadID, Anvil: anvil, Branch: branch,
			Status: WorkerPaused, StartedAt: started,
		}); err != nil {
			t.Fatalf("InsertWorker %s: %v", id, err)
		}
		if session != "" {
			if err := db.UpdateWorkerSession(id, session, "opus"); err != nil {
				t.Fatalf("UpdateWorkerSession %s: %v", id, err)
			}
		}
	}

	// No worker at all → (nil, nil), not an error.
	got, err := db.ResumableWorkerByBeadID("bd-missing")
	if err != nil {
		t.Fatalf("ResumableWorkerByBeadID (missing): %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for a bead with no worker, got %+v", got)
	}

	// A row with a branch but no session_id is filtered out (nothing to resume).
	insert("w-nosess", "bd-nosess", "anvil-1", "forge/bd-nosess", "", time.Now())
	got, err = db.ResumableWorkerByBeadID("bd-nosess")
	if err != nil {
		t.Fatalf("ResumableWorkerByBeadID (no session): %v", err)
	}
	if got != nil {
		t.Errorf("a worker without session_id must not be resumable, got %+v", got)
	}

	// A row with a session_id but no branch is filtered out too.
	insert("w-nobranch", "bd-nobranch", "anvil-1", "", "sess-x", time.Now())
	got, err = db.ResumableWorkerByBeadID("bd-nobranch")
	if err != nil {
		t.Fatalf("ResumableWorkerByBeadID (no branch): %v", err)
	}
	if got != nil {
		t.Errorf("a worker without a branch must not be resumable, got %+v", got)
	}

	// Two qualifying rows for one bead: the most recently started one wins.
	base := time.Now()
	insert("w-old", "bd-multi", "anvil-1", "forge/bd-multi", "sess-old", base.Add(-2*time.Hour))
	insert("w-new", "bd-multi", "anvil-2", "forge/bd-multi", "sess-new", base.Add(-1*time.Minute))
	got, err = db.ResumableWorkerByBeadID("bd-multi")
	if err != nil {
		t.Fatalf("ResumableWorkerByBeadID (multi): %v", err)
	}
	if got == nil {
		t.Fatal("expected a resumable worker for bd-multi, got nil")
	}
	if got.ID != "w-new" {
		t.Errorf("expected the most recent row w-new, got %s", got.ID)
	}
	if got.SessionID != "sess-new" || got.Branch != "forge/bd-multi" {
		t.Errorf("resumable worker fields not carried through: %+v", got)
	}
}

// TestDB_MigrationIdempotent verifies that running the column migrations twice
// on the same database is safe (session_id/model migrations must not fail if
// the columns already exist).
func TestDB_MigrationIdempotent(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-state-migrate-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Open already ran migrate() once; a second explicit pass must be a no-op.
	if err := db.migrate(); err != nil {
		t.Fatalf("second migrate() failed: %v", err)
	}

	for _, col := range []string{"session_id", "model"} {
		exists, err := db.columnExists("workers", col)
		if err != nil {
			t.Fatalf("columnExists(%q): %v", col, err)
		}
		if !exists {
			t.Errorf("expected workers.%s column to exist after migration", col)
		}
	}
}

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

// TestDB_BellowsAssignment exercises the bellows_managed / bellows_manually_assigned
// pair set by the assign_bellows IPC action (Forge-l125). The manual flag must
// persist across reads so the reconcile loop can distinguish user-pinned PRs
// from legacy auto-adoption.
func TestDB_BellowsAssignment(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-state-bellows-*")
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
		Number: 42, Anvil: "anvil-1", BeadID: "ext-42",
		Branch: "feature/external", Status: PROpen, CreatedAt: time.Now(),
	}
	if err := db.InsertPR(pr); err != nil {
		t.Fatal(err)
	}

	// Fresh insert: both flags must default to 0 for ext-* PRs (the migration
	// data-fixup clears bellows_managed=1 defaults on ext-* rows; manual flag
	// defaults to 0 column-wise).
	got, err := db.GetPRByID(pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BellowsManaged || got.BellowsManuallyAssigned {
		t.Fatalf("fresh ext-* PR should have both flags off, got managed=%v manual=%v",
			got.BellowsManaged, got.BellowsManuallyAssigned)
	}

	// assign_bellows sets both flags.
	if err := db.UpdatePRBellowsAssignment(pr.ID, true, true); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetPRByID(pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.BellowsManaged || !got.BellowsManuallyAssigned {
		t.Fatalf("after assign_bellows expected both flags set, got managed=%v manual=%v",
			got.BellowsManaged, got.BellowsManuallyAssigned)
	}

	// unassign_bellows clears both flags.
	if err := db.UpdatePRBellowsAssignment(pr.ID, false, false); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetPRByID(pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BellowsManaged || got.BellowsManuallyAssigned {
		t.Fatalf("after unassign_bellows expected both flags clear, got managed=%v manual=%v",
			got.BellowsManaged, got.BellowsManuallyAssigned)
	}

	// UpdatePRBellowsManaged alone must not touch the manual flag.
	if err := db.UpdatePRBellowsAssignment(pr.ID, true, true); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdatePRBellowsManaged(pr.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetPRByID(pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BellowsManaged {
		t.Fatalf("expected bellows_managed=false after UpdatePRBellowsManaged(false)")
	}
	if !got.BellowsManuallyAssigned {
		t.Fatalf("UpdatePRBellowsManaged must not clear bellows_manually_assigned")
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

func TestDB_ClearNeedsAttention_ZeroesColumns(t *testing.T) {
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

	past := time.Now().Add(-1 * time.Hour)
	firstFailure := time.Now().Add(-30 * time.Minute)
	if err := db.UpsertRetry(&RetryRecord{
		BeadID:               "BD-1",
		Anvil:                "anvil-1",
		RetryCount:           4,
		NextRetry:            &past,
		NeedsHuman:           true,
		ClarificationNeeded:  true,
		DispatchFailures:     3,
		RecoveryFailures:     2,
		FirstRecoveryFailure: &firstFailure,
		LastError:            "boom",
	}); err != nil {
		t.Fatal(err)
	}

	pre, err := db.GetRetry("BD-1", "anvil-1")
	if err != nil || pre == nil {
		t.Fatalf("seed record missing: %v", err)
	}
	preUpdatedAt := pre.UpdatedAt
	time.Sleep(5 * time.Millisecond)

	if err := db.ClearNeedsAttention("BD-1", "anvil-1"); err != nil {
		t.Fatalf("ClearNeedsAttention returned error: %v", err)
	}

	r, err := db.GetRetry("BD-1", "anvil-1")
	if err != nil {
		t.Fatal(err)
	}
	if r == nil {
		t.Fatal("expected record to still exist after clear")
	}
	if r.NeedsHuman {
		t.Error("expected NeedsHuman=false after clear")
	}
	if r.DispatchFailures != 0 {
		t.Errorf("expected DispatchFailures=0 after clear, got %d", r.DispatchFailures)
	}
	if r.RecoveryFailures != 0 {
		t.Errorf("expected RecoveryFailures=0 after clear, got %d", r.RecoveryFailures)
	}
	if r.FirstRecoveryFailure != nil {
		t.Errorf("expected FirstRecoveryFailure=nil after clear, got %v", r.FirstRecoveryFailure)
	}
	if r.LastError != "" {
		t.Errorf("expected LastError empty after clear, got %q", r.LastError)
	}
	// Fields the bead spec says must NOT be touched.
	if !r.ClarificationNeeded {
		t.Error("expected ClarificationNeeded to be preserved (true) after clear")
	}
	if r.RetryCount != 4 {
		t.Errorf("expected RetryCount preserved (=4) after clear, got %d", r.RetryCount)
	}
	if r.NextRetry == nil {
		t.Error("expected NextRetry preserved (non-nil) after clear")
	}
	if !r.UpdatedAt.After(preUpdatedAt) {
		t.Errorf("expected UpdatedAt to advance, before=%v after=%v", preUpdatedAt, r.UpdatedAt)
	}
}

func TestDB_ClearNeedsAttention_Idempotent(t *testing.T) {
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

	// No-op success when no row exists at all.
	if err := db.ClearNeedsAttention("BD-NONE", "anvil-1"); err != nil {
		t.Fatalf("ClearNeedsAttention on missing row should be no-op success, got: %v", err)
	}

	// Seed a clean row, clear twice, both should succeed without error.
	if err := db.UpsertRetry(&RetryRecord{
		BeadID: "BD-CLEAN",
		Anvil:  "anvil-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ClearNeedsAttention("BD-CLEAN", "anvil-1"); err != nil {
		t.Fatalf("first clear failed: %v", err)
	}
	if err := db.ClearNeedsAttention("BD-CLEAN", "anvil-1"); err != nil {
		t.Fatalf("second clear failed: %v", err)
	}

	r, err := db.GetRetry("BD-CLEAN", "anvil-1")
	if err != nil || r == nil {
		t.Fatalf("expected row to still exist, err=%v", err)
	}
	if r.NeedsHuman || r.DispatchFailures != 0 || r.RecoveryFailures != 0 || r.LastError != "" {
		t.Errorf("expected fully clean row after two clears, got %+v", r)
	}
}

func TestDB_ClearNeedsAttention_OtherRowsUntouched(t *testing.T) {
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

	if err := db.UpsertRetry(&RetryRecord{
		BeadID:           "BD-TARGET",
		Anvil:            "anvil-1",
		NeedsHuman:       true,
		DispatchFailures: 2,
		LastError:        "target",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRetry(&RetryRecord{
		BeadID:           "BD-SIBLING",
		Anvil:            "anvil-1",
		NeedsHuman:       true,
		DispatchFailures: 5,
		LastError:        "untouched",
	}); err != nil {
		t.Fatal(err)
	}
	// Cross-anvil row with the same bead ID — must also stay untouched.
	if err := db.UpsertRetry(&RetryRecord{
		BeadID:           "BD-TARGET",
		Anvil:            "anvil-2",
		NeedsHuman:       true,
		DispatchFailures: 7,
		LastError:        "other-anvil",
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.ClearNeedsAttention("BD-TARGET", "anvil-1"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	target, err := db.GetRetry("BD-TARGET", "anvil-1")
	if err != nil || target == nil {
		t.Fatalf("target row missing: %v", err)
	}
	if target.NeedsHuman || target.DispatchFailures != 0 || target.LastError != "" {
		t.Errorf("target row not cleared: %+v", target)
	}

	sibling, err := db.GetRetry("BD-SIBLING", "anvil-1")
	if err != nil || sibling == nil {
		t.Fatalf("sibling row missing: %v", err)
	}
	if !sibling.NeedsHuman || sibling.DispatchFailures != 5 || sibling.LastError != "untouched" {
		t.Errorf("sibling row was modified: %+v", sibling)
	}

	otherAnvil, err := db.GetRetry("BD-TARGET", "anvil-2")
	if err != nil || otherAnvil == nil {
		t.Fatalf("cross-anvil row missing: %v", err)
	}
	if !otherAnvil.NeedsHuman || otherAnvil.DispatchFailures != 7 || otherAnvil.LastError != "other-anvil" {
		t.Errorf("cross-anvil row was modified: %+v", otherAnvil)
	}
}

func TestDB_ClearNeedsAttention_DoesNotScheduleRetry(t *testing.T) {
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

	// Seed a row with no scheduled retry; after clear, NextRetry must
	// remain unset and the bead must drop out of the needs-human set.
	if err := db.UpsertRetry(&RetryRecord{
		BeadID:           "BD-NOENQ",
		Anvil:            "anvil-1",
		NeedsHuman:       true,
		DispatchFailures: 3,
		LastError:        "boom",
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.ClearNeedsAttention("BD-NOENQ", "anvil-1"); err != nil {
		t.Fatalf("clear: %v", err)
	}

	r, err := db.GetRetry("BD-NOENQ", "anvil-1")
	if err != nil || r == nil {
		t.Fatalf("expected row to exist, err=%v", err)
	}
	if r.NextRetry != nil {
		t.Errorf("ClearNeedsAttention must not schedule a retry; got NextRetry=%v", r.NextRetry)
	}

	needsHuman, err := db.NeedsHumanBeadIDSet()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := needsHuman["BD-NOENQ\x00anvil-1"]; ok {
		t.Error("expected BD-NOENQ to no longer be in needs-human set after clear")
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

func TestDB_NullWorkerLogPathsUnder(t *testing.T) {
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

	removed := filepath.Join(tmpDir, "logs", "BD-1")
	sibling := filepath.Join(tmpDir, "logs", "BD-1-extra") // shares a name prefix
	kept := filepath.Join(tmpDir, "logs", "BD-2")

	insert := func(id, logPath string) {
		w := &Worker{ID: id, BeadID: "BD", Status: WorkerDone, LogPath: logPath, StartedAt: time.Now()}
		if err := db.InsertWorker(w); err != nil {
			t.Fatal(err)
		}
	}
	insert("w-under", filepath.Join(removed, "smith.log"))
	insert("w-sibling", filepath.Join(sibling, "smith.log"))
	insert("w-other", filepath.Join(kept, "smith.log"))
	insert("w-empty", "")

	n, err := db.NullWorkerLogPathsUnder(removed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 row nulled, got %d", n)
	}

	got := map[string]string{}
	for _, id := range []string{"w-under", "w-sibling", "w-other"} {
		var lp string
		if err := db.conn.QueryRow(`SELECT log_path FROM workers WHERE id = ?`, id).Scan(&lp); err != nil {
			t.Fatal(err)
		}
		got[id] = lp
	}

	if got["w-under"] != "" {
		t.Errorf("expected w-under log_path cleared, got %q", got["w-under"])
	}
	if got["w-sibling"] == "" {
		t.Errorf("sibling directory row must not be cleared (name-prefix collision)")
	}
	if got["w-other"] == "" {
		t.Errorf("unrelated directory row must not be cleared")
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

func TestDB_MergedPRs(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-state-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	db, err := Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now()

	// Insert a mix of open, merged, and closed PRs.
	prs := []PR{
		{Number: 1, Anvil: "anvil-a", BeadID: "bd-1", Branch: "fix-1", Status: PROpen, CreatedAt: now},
		{Number: 2, Anvil: "anvil-a", BeadID: "bd-2", Branch: "fix-2", Status: PRMerged, CreatedAt: now},
		{Number: 3, Anvil: "anvil-b", BeadID: "bd-3", Branch: "fix-3", Status: PRMerged, CreatedAt: now},
		{Number: 4, Anvil: "anvil-a", BeadID: "bd-4", Branch: "fix-4", Status: PRClosed, CreatedAt: now},
	}
	for i := range prs {
		if err := db.InsertPR(&prs[i]); err != nil {
			t.Fatal(err)
		}
	}

	merged, err := db.MergedPRs()
	if err != nil {
		t.Fatal(err)
	}

	if len(merged) != 2 {
		t.Fatalf("expected 2 merged PRs, got %d", len(merged))
	}
	// Use a set to avoid dependence on undefined ordering for same-timestamp rows.
	mergedNums := map[int]bool{}
	for _, pr := range merged {
		mergedNums[pr.Number] = true
	}
	if !mergedNums[2] || !mergedNums[3] {
		t.Errorf("unexpected merged PRs: %v", merged)
	}

	// No merged PRs after removing them.
	for _, pr := range merged {
		if err := db.UpdatePRStatus(pr.ID, PRClosed); err != nil {
			t.Fatal(err)
		}
	}
	merged, err = db.MergedPRs()
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 0 {
		t.Errorf("expected 0 merged PRs after status change, got %d", len(merged))
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
	// Quench worker — should be excluded
	if err := db.InsertWorker(&Worker{
		ID: "w-quench", BeadID: "BD-3", Anvil: "anvil-1",
		Status: WorkerRunning, Phase: "quench",
		StartedAt: time.Now().Add(-25 * time.Minute),
		LogPath:   makeStaleLog("quench.log"),
	}); err != nil {
		t.Fatal(err)
	}
	// Burnish worker — should be excluded
	if err := db.InsertWorker(&Worker{
		ID: "w-burnish", BeadID: "BD-4", Anvil: "anvil-1",
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

// TestDB_StalledWorkers_ExcludesPaused verifies that a worker parked by an
// operator pause (status 'paused') is never flagged stale, even when its log file
// is far older than the stale threshold. A parked pipeline legitimately stops
// producing log output while it awaits a resume, so the watchdog must skip it.
func TestDB_StalledWorkers_ExcludesPaused(t *testing.T) {
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

	// A stale (20-min-old) log file for a parked worker in the smith phase — the
	// exact combination that would be flagged if the worker were running.
	logFile := filepath.Join(tmpDir, "paused-smith.log")
	if err := os.WriteFile(logFile, []byte("log"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-20 * time.Minute)
	if err := os.Chtimes(logFile, old, old); err != nil {
		t.Fatal(err)
	}

	if err := db.InsertWorker(&Worker{
		ID: "w-paused", BeadID: "BD-1", Anvil: "anvil-1",
		Status: WorkerPaused, Phase: "smith",
		StartedAt: time.Now().Add(-25 * time.Minute),
		LogPath:   logFile,
	}); err != nil {
		t.Fatal(err)
	}

	stalled, err := db.StalledWorkers(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(stalled) != 0 {
		t.Fatalf("expected 0 stalled workers (paused must be excluded), got %d: %+v", len(stalled), stalled)
	}
}

// TestDB_PausedWorkers verifies PausedWorkers returns exactly the paused workers
// (for worktree-retention unions) and nothing in other statuses.
func TestDB_PausedWorkers(t *testing.T) {
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

	if err := db.InsertWorker(&Worker{
		ID: "w-paused", BeadID: "BD-1", Anvil: "anvil-1", Branch: "forge/BD-1",
		Status: WorkerPaused, Phase: "smith", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertWorker(&Worker{
		ID: "w-running", BeadID: "BD-2", Anvil: "anvil-1", Branch: "forge/BD-2",
		Status: WorkerRunning, Phase: "smith", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertWorker(&Worker{
		ID: "w-done", BeadID: "BD-3", Anvil: "anvil-1", Branch: "forge/BD-3",
		Status: WorkerDone, Phase: "smith", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	paused, err := db.PausedWorkers()
	if err != nil {
		t.Fatal(err)
	}
	if len(paused) != 1 {
		t.Fatalf("expected 1 paused worker, got %d: %+v", len(paused), paused)
	}
	if paused[0].ID != "w-paused" {
		t.Errorf("expected w-paused, got %s", paused[0].ID)
	}
	if paused[0].Branch != "forge/BD-1" {
		t.Errorf("expected branch forge/BD-1 to be returned for worktree matching, got %q", paused[0].Branch)
	}
}

func TestDB_PausedWorkerByBead(t *testing.T) {
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

	if err := db.InsertWorker(&Worker{
		ID: "w-paused", BeadID: "BD-1", Anvil: "anvil-1", Branch: "forge/BD-1",
		Status: WorkerPaused, Phase: "smith", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateWorkerSession("w-paused", "sess-123", "claude-opus-4-8"); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertWorker(&Worker{
		ID: "w-running", BeadID: "BD-2", Anvil: "anvil-1", Branch: "forge/BD-2",
		Status: WorkerRunning, Phase: "smith", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Scoped lookup matches the paused worker and carries its session_id.
	got, err := db.PausedWorkerByBead("BD-1", "anvil-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != "w-paused" {
		t.Fatalf("expected w-paused, got %+v", got)
	}
	if got.SessionID != "sess-123" || got.Model != "claude-opus-4-8" {
		t.Errorf("expected session/model to round-trip, got session=%q model=%q", got.SessionID, got.Model)
	}

	// A running worker is not returned by the paused lookup.
	if got, err := db.PausedWorkerByBead("BD-2", "anvil-1"); err != nil {
		t.Fatal(err)
	} else if got != nil {
		t.Errorf("expected nil for running bead, got %+v", got)
	}

	// Wrong anvil does not match.
	if got, err := db.PausedWorkerByBead("BD-1", "other-anvil"); err != nil {
		t.Fatal(err)
	} else if got != nil {
		t.Errorf("expected nil for wrong anvil, got %+v", got)
	}

	// Bead-only lookup finds the paused worker across anvils.
	byID, err := db.PausedWorkerByBeadID("BD-1")
	if err != nil {
		t.Fatal(err)
	}
	if byID == nil || byID.ID != "w-paused" || byID.Anvil != "anvil-1" {
		t.Fatalf("expected w-paused on anvil-1, got %+v", byID)
	}
	if none, err := db.PausedWorkerByBeadID("BD-2"); err != nil {
		t.Fatal(err)
	} else if none != nil {
		t.Errorf("expected nil for running bead, got %+v", none)
	}
}

func TestDB_NeedsAttentionBeads_SurfacesPaused(t *testing.T) {
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

	if err := db.InsertWorker(&Worker{
		ID: "w-paused", BeadID: "BD-PAUSE", Anvil: "anvil-1", Branch: "forge/BD-PAUSE",
		Status: WorkerPaused, Phase: "smith", Title: "Paused bead title", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// A running worker must NOT surface in Needs Attention.
	if err := db.InsertWorker(&Worker{
		ID: "w-running", BeadID: "BD-RUN", Anvil: "anvil-1", Branch: "forge/BD-RUN",
		Status: WorkerRunning, Phase: "smith", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	beads, err := db.NeedsAttentionBeads(DefaultMaxCIFixAttempts, DefaultMaxReviewFixAttempts, DefaultMaxRebaseAttempts)
	if err != nil {
		t.Fatal(err)
	}
	var found *NeedsAttentionBead
	for i := range beads {
		if beads[i].BeadID == "BD-PAUSE" {
			found = &beads[i]
		}
		if beads[i].BeadID == "BD-RUN" {
			t.Errorf("running bead should not surface in Needs Attention: %+v", beads[i])
		}
	}
	if found == nil {
		t.Fatalf("expected paused bead BD-PAUSE in Needs Attention, got %+v", beads)
	}
	if found.Title != "Paused bead title" {
		t.Errorf("expected worker title to enrich the paused entry, got %q", found.Title)
	}
	if found.Reason == "" {
		t.Errorf("expected a non-empty reason for the paused entry")
	}
	if found.NeedsHuman || found.ClarificationNeeded {
		t.Errorf("paused entry should carry no needs_human/clarification flags, got %+v", found)
	}
}

func TestDB_StalledWorkers_LifecycleWithPerWorkerTimeout(t *testing.T) {
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

	// Burnish worker without StaleTimeout — should be excluded (background phase, no per-worker timeout)
	if err := db.InsertWorker(&Worker{
		ID: "w-burnish-no-timeout", BeadID: "BD-1", Anvil: "anvil-1",
		Status: WorkerRunning, Phase: "burnish",
		StartedAt: time.Now().Add(-25 * time.Minute),
		LogPath:   makeStaleLog("burnish-no-timeout.log"),
	}); err != nil {
		t.Fatal(err)
	}
	// Quench worker with StaleTimeout — should be detected as stale
	if err := db.InsertWorker(&Worker{
		ID: "w-quench-with-timeout", BeadID: "BD-2", Anvil: "anvil-1",
		Status: WorkerRunning, Phase: "quench",
		StartedAt:    time.Now().Add(-25 * time.Minute),
		LogPath:      makeStaleLog("quench-with-timeout.log"),
		StaleTimeout: 10 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	// Rebase worker with StaleTimeout but recently active — should NOT be stale
	recentLog := filepath.Join(tmpDir, "rebase-recent.log")
	if err := os.WriteFile(recentLog, []byte("log"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertWorker(&Worker{
		ID: "w-rebase-recent", BeadID: "BD-3", Anvil: "anvil-1",
		Status: WorkerRunning, Phase: "rebase",
		StartedAt:    time.Now().Add(-25 * time.Minute),
		LogPath:      recentLog,
		StaleTimeout: 10 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}

	// Assay worker with StaleTimeout — should be detected as stale
	if err := db.InsertWorker(&Worker{
		ID: "w-assay-with-timeout", BeadID: "BD-4", Anvil: "anvil-1",
		Status: WorkerRunning, Phase: "assay",
		StartedAt:    time.Now().Add(-25 * time.Minute),
		LogPath:      makeStaleLog("assay-with-timeout.log"),
		StaleTimeout: 10 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}

	stalled, err := db.StalledWorkers(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(stalled) != 2 {
		ids := make([]string, len(stalled))
		for i, w := range stalled {
			ids[i] = w.ID
		}
		t.Fatalf("expected 2 stalled workers, got %d: %v", len(stalled), ids)
	}
	stalledIDs := map[string]bool{}
	for _, w := range stalled {
		stalledIDs[w.ID] = true
	}
	if !stalledIDs["w-quench-with-timeout"] {
		t.Errorf("expected w-quench-with-timeout in stalled set")
	}
	if !stalledIDs["w-assay-with-timeout"] {
		t.Errorf("expected w-assay-with-timeout in stalled set")
	}
}

// getWorkerStatus is a small test helper that returns a worker's current status.
func getWorkerStatus(t *testing.T, db *DB, id string) WorkerStatus {
	t.Helper()
	w, err := db.GetWorker(id)
	if err != nil {
		t.Fatalf("GetWorker(%s): %v", id, err)
	}
	return w.Status
}

func TestDB_MarkWorkerStalled_PreservesPriorStatus(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Cover the full round-trip for every active status that can stall.
	phases := []struct {
		id     string
		status WorkerStatus
	}{
		{"w-pending", WorkerPending},
		{"w-running", WorkerRunning},
		{"w-reviewing", WorkerReviewing},
		{"w-monitoring", WorkerMonitoring},
	}
	for _, p := range phases {
		logFile := filepath.Join(tmpDir, p.id+".log")
		if err := os.WriteFile(logFile, []byte("log"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := db.InsertWorker(&Worker{
			ID: p.id, BeadID: "BD-" + p.id, Anvil: "anvil-1",
			Status: p.status, Phase: "smith",
			StartedAt: time.Now().Add(-15 * time.Minute),
			LogPath:   logFile,
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.MarkWorkerStalled(p.id); err != nil {
			t.Fatal(err)
		}
		if got := getWorkerStatus(t, db, p.id); got != WorkerStalled {
			t.Fatalf("%s: expected stalled, got %s", p.id, got)
		}

		// Bump the log so the worker counts as recovered, then un-stall it and
		// confirm it returns to its original status.
		now := time.Now()
		if err := os.Chtimes(logFile, now, now); err != nil {
			t.Fatal(err)
		}
		if err := db.UnstallWorker(p.id); err != nil {
			t.Fatal(err)
		}
		if got := getWorkerStatus(t, db, p.id); got != p.status {
			t.Errorf("%s: expected restored status %s, got %s", p.id, p.status, got)
		}
	}
}

func TestDB_RecoveredStalledWorkers(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	staleLog := func(name string) string {
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

	// Stalled worker with a fresh log → recoverable.
	freshLog := filepath.Join(tmpDir, "fresh.log")
	if err := os.WriteFile(freshLog, []byte("recent"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertWorker(&Worker{
		ID: "w-fresh", BeadID: "BD-1", Anvil: "anvil-1",
		Status: WorkerStalled, Phase: "smith",
		StartedAt: time.Now().Add(-25 * time.Minute), LogPath: freshLog,
	}); err != nil {
		t.Fatal(err)
	}
	// Stalled worker whose log is still old → not recoverable.
	if err := db.InsertWorker(&Worker{
		ID: "w-stillstale", BeadID: "BD-2", Anvil: "anvil-1",
		Status: WorkerStalled, Phase: "smith",
		StartedAt: time.Now().Add(-25 * time.Minute), LogPath: staleLog("stale.log"),
	}); err != nil {
		t.Fatal(err)
	}
	// Stalled worker with no log path → not recoverable (stalled on age).
	if err := db.InsertWorker(&Worker{
		ID: "w-nolog", BeadID: "BD-3", Anvil: "anvil-1",
		Status: WorkerStalled, Phase: "smith",
		StartedAt: time.Now().Add(-25 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	// Active (non-stalled) worker with a fresh log → not in scope.
	if err := db.InsertWorker(&Worker{
		ID: "w-running", BeadID: "BD-4", Anvil: "anvil-1",
		Status: WorkerRunning, Phase: "smith",
		StartedAt: time.Now().Add(-25 * time.Minute), LogPath: freshLog,
	}); err != nil {
		t.Fatal(err)
	}

	recovered, err := db.RecoveredStalledWorkers(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 {
		ids := make([]string, len(recovered))
		for i, w := range recovered {
			ids[i] = w.ID
		}
		t.Fatalf("expected 1 recovered worker, got %d: %v", len(recovered), ids)
	}
	if recovered[0].ID != "w-fresh" {
		t.Errorf("expected w-fresh, got %s", recovered[0].ID)
	}
}

// TestDB_RecoveredStalledWorkers_LifecycleThreshold guards the recovery/stall
// threshold symmetry: lifecycle workers (quench/cifix/burnish/reviewfix/rebase/
// assay) are stalled against their own per-worker StaleTimeout, so they must be
// un-stalled against that SAME threshold — not the (typically longer) global
// staleThreshold. Otherwise a lifecycle worker whose log is older than its own
// timeout but newer than the global threshold would be recovered and then
// immediately re-stalled, flapping.
func TestDB_RecoveredStalledWorkers_LifecycleThreshold(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	logWithAge := func(name string, age time.Duration) string {
		p := filepath.Join(tmpDir, name)
		if err := os.WriteFile(p, []byte("log"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		return p
	}

	const globalThreshold = 5 * time.Minute
	const lifecycleTimeout = 1 * time.Minute

	// Lifecycle worker whose log is 2m old: stale by its own 1m timeout, but
	// "fresh" by the global 5m threshold. It must NOT recover — recovering it
	// would un-stall a worker that is genuinely stalled under its own threshold.
	if err := db.InsertWorker(&Worker{
		ID: "w-lifecycle-stillstale", BeadID: "BD-1", Anvil: "anvil-1",
		Status: WorkerStalled, Phase: "quench",
		StartedAt: time.Now().Add(-25 * time.Minute), LogPath: logWithAge("quench.log", 2*time.Minute),
		StaleTimeout: lifecycleTimeout,
	}); err != nil {
		t.Fatal(err)
	}
	// Lifecycle worker with a genuinely fresh log (within its 1m timeout) → recovers.
	if err := db.InsertWorker(&Worker{
		ID: "w-lifecycle-fresh", BeadID: "BD-2", Anvil: "anvil-1",
		Status: WorkerStalled, Phase: "burnish",
		StartedAt: time.Now().Add(-25 * time.Minute), LogPath: logWithAge("burnish.log", 10*time.Second),
		StaleTimeout: lifecycleTimeout,
	}); err != nil {
		t.Fatal(err)
	}

	recovered, err := db.RecoveredStalledWorkers(globalThreshold)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ID != "w-lifecycle-fresh" {
		ids := make([]string, len(recovered))
		for i, w := range recovered {
			ids[i] = w.ID
		}
		t.Fatalf("expected only w-lifecycle-fresh to recover, got %v", ids)
	}
}

func TestDB_UnstallWorker_NoOpWhenTerminal(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	logFile := filepath.Join(tmpDir, "w.log")
	if err := os.WriteFile(logFile, []byte("log"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertWorker(&Worker{
		ID: "w-1", BeadID: "BD-1", Anvil: "anvil-1",
		Status: WorkerRunning, Phase: "smith",
		StartedAt: time.Now().Add(-15 * time.Minute), LogPath: logFile,
	}); err != nil {
		t.Fatal(err)
	}
	// Stall it, then let the real process finish (terminal status) while it is
	// still flagged stalled — UpdateWorkerStatus has no status guard, mirroring
	// the production race the WHERE guard protects against.
	if err := db.MarkWorkerStalled("w-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateWorkerStatus("w-1", WorkerDone); err != nil {
		t.Fatal(err)
	}
	// UnstallWorker must not resurrect a terminal worker.
	if err := db.UnstallWorker("w-1"); err != nil {
		t.Fatal(err)
	}
	if got := getWorkerStatus(t, db, "w-1"); got != WorkerDone {
		t.Errorf("expected status to remain done, got %s", got)
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
		if err := db.UpdatePRMergeability(pr.ID, ciPassing, conflicting, unresolvedThreads, pendingReviews, false, true); err != nil {
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
	if err := db.UpdatePRMergeability(prConflict.ID, true, false, false, false, false, true); err != nil {
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

// TestDB_ReadyToMerge_PendingAssayWorker verifies the Forge-75cx guard: a PR
// that is otherwise ready must be excluded from both IsPRReadyToMerge and
// ReadyToMergePRs while an Assay review worker is pending or running for it, and
// surface again once that worker completes.
func TestDB_ReadyToMerge_PendingAssayWorker(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-rtm-assay-test-*")
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
		Number:    300,
		Anvil:     "anvil-assay",
		BeadID:    "bd-300",
		Branch:    "branch-300",
		Status:    PRApproved,
		CreatedAt: time.Now(),
	}
	if err := db.InsertPR(pr); err != nil {
		t.Fatalf("InsertPR: %v", err)
	}
	// Clean mergeability: CI passing, no conflicts/threads/pending reviews.
	if err := db.UpdatePRMergeability(pr.ID, true, false, false, false, false, true); err != nil {
		t.Fatalf("UpdatePRMergeability: %v", err)
	}

	// Baseline: no assay worker → ready.
	if ok, err := db.IsPRReadyToMerge(pr.ID); err != nil || !ok {
		t.Fatalf("baseline IsPRReadyToMerge: got %v (err %v), want true", ok, err)
	}
	if list, err := db.ReadyToMergePRs(); err != nil || len(list) != 1 {
		t.Fatalf("baseline ReadyToMergePRs: got %d (err %v), want 1", len(list), err)
	}

	// A running Assay worker for this PR blocks readiness.
	worker := &Worker{
		ID:        "assay-anvil-assay-300",
		BeadID:    pr.BeadID,
		Anvil:     pr.Anvil,
		Status:    WorkerRunning,
		Phase:     "assay",
		PRNumber:  pr.Number,
		StartedAt: time.Now(),
	}
	if err := db.InsertWorker(worker); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	if ok, err := db.IsPRReadyToMerge(pr.ID); err != nil || ok {
		t.Errorf("with running assay worker IsPRReadyToMerge: got %v (err %v), want false", ok, err)
	}
	if list, err := db.ReadyToMergePRs(); err != nil || len(list) != 0 {
		t.Errorf("with running assay worker ReadyToMergePRs: got %d (err %v), want 0", len(list), err)
	}

	// A stalled Assay worker still blocks readiness (it may recover).
	if err := db.UpdateWorkerStatus(worker.ID, WorkerStalled); err != nil {
		t.Fatalf("UpdateWorkerStatus stalled: %v", err)
	}
	if ok, err := db.IsPRReadyToMerge(pr.ID); err != nil || ok {
		t.Errorf("with stalled assay worker IsPRReadyToMerge: got %v (err %v), want false", ok, err)
	}
	if list, err := db.ReadyToMergePRs(); err != nil || len(list) != 0 {
		t.Errorf("with stalled assay worker ReadyToMergePRs: got %d (err %v), want 0", len(list), err)
	}

	// Once the worker completes, the PR is ready again.
	if err := db.UpdateWorkerStatus(worker.ID, WorkerDone); err != nil {
		t.Fatalf("UpdateWorkerStatus: %v", err)
	}
	if ok, err := db.IsPRReadyToMerge(pr.ID); err != nil || !ok {
		t.Errorf("after assay worker done IsPRReadyToMerge: got %v (err %v), want true", ok, err)
	}
	if list, err := db.ReadyToMergePRs(); err != nil || len(list) != 1 {
		t.Errorf("after assay worker done ReadyToMergePRs: got %d (err %v), want 1", len(list), err)
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
	if err := db.UpdatePRMergeability(pr.ID, true, false, false, false, false, true); err != nil {
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

// TestDB_ReadyToMerge_AssayGate verifies the assay_up_to_date column gates
// readiness consistently with the bellows event gate (Forge-s0en): a PR that is
// otherwise ready but whose current head has not been assayed must not appear
// ready until assay_up_to_date is set true.
func TestDB_ReadyToMerge_AssayGate(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	pr := &PR{Number: 4219, Anvil: "munin", BeadID: "bd-4219", Branch: "b", Status: PROpen, CreatedAt: time.Now()}
	if err := db.InsertPR(pr); err != nil {
		t.Fatalf("InsertPR: %v", err)
	}

	// Otherwise ready, but assay not yet up to date → not ready.
	if err := db.UpdatePRMergeability(pr.ID, true, false, false, false, false, false); err != nil {
		t.Fatalf("UpdatePRMergeability: %v", err)
	}
	ready, err := db.IsPRReadyToMerge(pr.ID)
	if err != nil {
		t.Fatalf("IsPRReadyToMerge: %v", err)
	}
	if ready {
		t.Error("PR should NOT be ready while assay_up_to_date=0")
	}
	if rows, err := db.ReadyToMergePRs(); err != nil {
		t.Fatalf("ReadyToMergePRs: %v", err)
	} else if len(rows) != 0 {
		t.Errorf("ReadyToMergePRs should be empty while assay_up_to_date=0, got %d", len(rows))
	}

	// Once the head is assayed → ready.
	if err := db.UpdatePRMergeability(pr.ID, true, false, false, false, false, true); err != nil {
		t.Fatalf("UpdatePRMergeability: %v", err)
	}
	ready, err = db.IsPRReadyToMerge(pr.ID)
	if err != nil {
		t.Fatalf("IsPRReadyToMerge: %v", err)
	}
	if !ready {
		t.Error("PR should be ready once assay_up_to_date=1")
	}
}

// TestDB_AssayWorkerInFlight verifies the in-flight check used by the bellows
// trigger gate to avoid double-dispatching Assay (Forge-o81n).
func TestDB_AssayWorkerInFlight(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const anvil, pr = "munin", 4219
	check := func(want bool, msg string) {
		t.Helper()
		got, err := db.AssayWorkerInFlight(anvil, pr)
		if err != nil {
			t.Fatalf("AssayWorkerInFlight: %v", err)
		}
		if got != want {
			t.Errorf("%s: AssayWorkerInFlight=%v, want %v", msg, got, want)
		}
	}

	check(false, "no workers")

	// A running assay worker for this PR → in flight.
	if err := db.InsertWorker(&Worker{ID: "assay-1", BeadID: "bd-4219", Anvil: anvil, Status: WorkerRunning, Phase: "assay", PRNumber: pr, StartedAt: time.Now()}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	check(true, "running assay worker")

	// A completed assay worker does not count.
	if err := db.InsertWorker(&Worker{ID: "assay-1", BeadID: "bd-4219", Anvil: anvil, Status: WorkerDone, Phase: "assay", PRNumber: pr, StartedAt: time.Now()}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	check(false, "done assay worker")

	// A stalled assay worker is intentionally excluded (may be stuck).
	if err := db.InsertWorker(&Worker{ID: "assay-1", BeadID: "bd-4219", Anvil: anvil, Status: WorkerStalled, Phase: "assay", PRNumber: pr, StartedAt: time.Now()}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	check(false, "stalled assay worker")

	// A worker for a different PR does not count.
	if err := db.InsertWorker(&Worker{ID: "assay-2", BeadID: "bd-9", Anvil: anvil, Status: WorkerRunning, Phase: "assay", PRNumber: 9999, StartedAt: time.Now()}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	check(false, "different PR")
}

// TestDB_BeadFixWorkerActive verifies the bead-in-flight check used by the
// bellows Assay trigger to avoid busy-looping while a fix worker is running
// (Forge-dkso): a non-Assay worker counts, but Assay/monitor workers and
// completed workers do not.
func TestDB_BeadFixWorkerActive(t *testing.T) {
	tmpDir := t.TempDir()
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const anvil, bead = "munin", "Fhi.Metadata-cvstg"
	check := func(want bool, msg string) {
		t.Helper()
		got, err := db.BeadFixWorkerActive(anvil, bead)
		if err != nil {
			t.Fatalf("BeadFixWorkerActive: %v", err)
		}
		if got != want {
			t.Errorf("%s: BeadFixWorkerActive=%v, want %v", msg, got, want)
		}
	}

	check(false, "no workers")

	// The synthetic bellows monitor must NOT count as a fix worker.
	if err := db.InsertWorker(&Worker{ID: "bellows-munin-4243", BeadID: bead, Anvil: anvil, Status: WorkerMonitoring, Phase: "bellows", PRNumber: 4243, StartedAt: time.Now()}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	check(false, "bellows monitor only")

	// A running quench (CI-fix) worker counts.
	if err := db.InsertWorker(&Worker{ID: "quench-1", BeadID: bead, Anvil: anvil, Status: WorkerRunning, Phase: "quench", PRNumber: 4243, StartedAt: time.Now()}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	check(true, "running quench worker")

	// Once it completes, it no longer counts.
	if err := db.InsertWorker(&Worker{ID: "quench-1", BeadID: bead, Anvil: anvil, Status: WorkerDone, Phase: "quench", PRNumber: 4243, StartedAt: time.Now()}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	check(false, "done quench worker")

	// An in-flight Assay worker must NOT suppress itself.
	if err := db.InsertWorker(&Worker{ID: "assay-x", BeadID: bead, Anvil: anvil, Status: WorkerRunning, Phase: "assay", PRNumber: 4243, StartedAt: time.Now()}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}
	check(false, "assay worker does not count as fix worker")
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
	logEvent(EventPoll, "anvil-a", "fetched 3 beads") // newest for anvil-a
	logEvent(EventPoll, "anvil-b", "fetched 0 beads") // only event for anvil-b
	logEvent(EventPoll, "anvil-c", "fetched 1 bead")  // not requested

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

func TestDB_LastPollAllAnvils(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-poll-all-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	logEvent := func(typ EventType, anvil, msg string) {
		t.Helper()
		if err := db.LogEvent(typ, msg, "", anvil); err != nil {
			t.Fatalf("LogEvent(%s/%s): %v", typ, anvil, err)
		}
	}

	t.Run("empty DB returns nil", func(t *testing.T) {
		results, err := db.LastPollAllAnvils()
		if err != nil {
			t.Fatalf("LastPollAllAnvils: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("expected 0 results for empty DB, got %d", len(results))
		}
	})

	// Seed events for two anvils.
	logEvent(EventPollError, "forge", "connect timeout")
	logEvent(EventPoll, "forge", "fetched 2 beads") // newest for forge
	logEvent(EventPoll, "hytte", "fetched 0 beads") // only event for hytte

	t.Run("returns latest row per distinct anvil", func(t *testing.T) {
		results, err := db.LastPollAllAnvils()
		if err != nil {
			t.Fatalf("LastPollAllAnvils: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("expected 2 results, got %d", len(results))
		}
		byAnvil := make(map[string]AnvilPollStatus)
		for _, r := range results {
			byAnvil[r.Anvil] = r
		}
		if !byAnvil["forge"].OK {
			t.Error("forge: expected OK=true (latest event is poll, not poll_error)")
		}
		if byAnvil["forge"].Message != "fetched 2 beads" {
			t.Errorf("forge: unexpected message %q", byAnvil["forge"].Message)
		}
		if !byAnvil["hytte"].OK {
			t.Error("hytte: expected OK=true")
		}
	})

	t.Run("poll_error sets OK=false", func(t *testing.T) {
		logEvent(EventPollError, "hytte", "network error")
		results, err := db.LastPollAllAnvils()
		if err != nil {
			t.Fatalf("LastPollAllAnvils: %v", err)
		}
		byAnvil := make(map[string]AnvilPollStatus)
		for _, r := range results {
			byAnvil[r.Anvil] = r
		}
		if byAnvil["hytte"].OK {
			t.Error("hytte: expected OK=false when latest event is poll_error")
		}
	})

	t.Run("events with empty anvil are ignored", func(t *testing.T) {
		// Log an event with no anvil (e.g. daemon_started).
		logEvent(EventPoll, "", "should be ignored")
		results, err := db.LastPollAllAnvils()
		if err != nil {
			t.Fatalf("LastPollAllAnvils: %v", err)
		}
		for _, r := range results {
			if r.Anvil == "" {
				t.Error("empty-anvil event should not appear in results")
			}
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

func TestDB_PendingWardenRules(t *testing.T) {
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

	// Insert rules across two anvils.
	if err := db.InsertPendingRule("anvil-a", "rule: no-fmt", "PR-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertPendingRule("anvil-b", "rule: no-lint", "PR-2"); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertPendingRule("anvil-a", "rule: no-vet", "PR-3"); err != nil {
		t.Fatal(err)
	}

	t.Run("QueryPendingRulesByAnvil groups correctly", func(t *testing.T) {
		result, err := db.QueryPendingRulesByAnvil()
		if err != nil {
			t.Fatal(err)
		}
		if len(result["anvil-a"]) != 2 {
			t.Errorf("anvil-a: got %d rules, want 2", len(result["anvil-a"]))
		}
		if len(result["anvil-b"]) != 1 {
			t.Errorf("anvil-b: got %d rules, want 1", len(result["anvil-b"]))
		}
	})

	t.Run("rules are ordered by created_at within anvil", func(t *testing.T) {
		result, err := db.QueryPendingRulesByAnvil()
		if err != nil {
			t.Fatal(err)
		}
		rules := result["anvil-a"]
		if len(rules) == 2 && rules[0].CreatedAt.After(rules[1].CreatedAt) {
			t.Error("anvil-a rules not in ascending created_at order")
		}
	})

	t.Run("CreatedAt is parsed into non-zero time", func(t *testing.T) {
		result, err := db.QueryPendingRulesByAnvil()
		if err != nil {
			t.Fatal(err)
		}
		for anvil, rules := range result {
			for _, r := range rules {
				if r.CreatedAt.IsZero() {
					t.Errorf("anvil %s rule %d has zero CreatedAt", anvil, r.ID)
				}
			}
		}
	})

	t.Run("DeletePendingRules removes only the requested IDs", func(t *testing.T) {
		result, err := db.QueryPendingRulesByAnvil()
		if err != nil {
			t.Fatal(err)
		}
		// Delete only the first anvil-a rule.
		idToDelete := result["anvil-a"][0].ID
		if err := db.DeletePendingRules([]int{idToDelete}); err != nil {
			t.Fatal(err)
		}
		after, err := db.QueryPendingRulesByAnvil()
		if err != nil {
			t.Fatal(err)
		}
		if len(after["anvil-a"]) != 1 {
			t.Errorf("anvil-a: got %d rules after delete, want 1", len(after["anvil-a"]))
		}
		if len(after["anvil-b"]) != 1 {
			t.Errorf("anvil-b: got %d rules after delete, want 1 (should be untouched)", len(after["anvil-b"]))
		}
		// Confirm the remaining anvil-a rule is the other one.
		if after["anvil-a"][0].ID == idToDelete {
			t.Error("deleted rule still present after DeletePendingRules")
		}
	})

	t.Run("DeletePendingRules with empty slice is a no-op", func(t *testing.T) {
		if err := db.DeletePendingRules(nil); err != nil {
			t.Errorf("DeletePendingRules(nil) returned error: %v", err)
		}
	})
}

func TestDB_WicketIssueCRUD(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-state-wicket-*")
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

	const repo = "owner/repo"
	const issueNum = 42

	// 1. Not tracked before insert.
	tracked, err := db.IsIssueTracked(repo, issueNum)
	if err != nil {
		t.Fatal(err)
	}
	if tracked {
		t.Fatal("issue should not be tracked before insert")
	}

	// GetWicketIssue on missing row returns nil, nil.
	got, err := db.GetWicketIssue(repo, issueNum)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil for missing issue, got %+v", got)
	}

	// 2. Insert.
	issue := WicketIssue{
		Repo:         repo,
		IssueNumber:  issueNum,
		Title:        "Test issue",
		Body:         "Body text",
		Author:       "alice",
		State:        "pending",
		TriageAction: "",
		TriageReason: "",
		BeadID:       "",
	}
	if err := db.InsertWicketIssue(issue); err != nil {
		t.Fatalf("InsertWicketIssue: %v", err)
	}

	// 3. IsIssueTracked should be true now.
	tracked, err = db.IsIssueTracked(repo, issueNum)
	if err != nil {
		t.Fatal(err)
	}
	if !tracked {
		t.Fatal("issue should be tracked after insert")
	}

	// 4. GetWicketIssue returns correct fields.
	got, err = db.GetWicketIssue(repo, issueNum)
	if err != nil {
		t.Fatalf("GetWicketIssue: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil after insert")
	}
	if got.Repo != repo || got.IssueNumber != issueNum {
		t.Errorf("repo/number mismatch: got %s/%d", got.Repo, got.IssueNumber)
	}
	if got.Title != "Test issue" || got.Author != "alice" {
		t.Errorf("unexpected fields: Title=%q Author=%q", got.Title, got.Author)
	}
	if got.State != "pending" {
		t.Errorf("expected state=pending, got %q", got.State)
	}
	if got.ProcessedAt != nil {
		t.Errorf("expected ProcessedAt=nil, got %v", got.ProcessedAt)
	}

	// 5. Update — triage the issue.
	now := time.Now().UTC()
	got.State = "triaged"
	got.TriageAction = "create_bead"
	got.TriageReason = "valid bug"
	got.BeadID = "Forge-xyz1"
	got.ProcessedAt = &now
	if err := db.UpdateWicketIssue(*got); err != nil {
		t.Fatalf("UpdateWicketIssue: %v", err)
	}

	// 6. Verify update persisted.
	updated, err := db.GetWicketIssue(repo, issueNum)
	if err != nil {
		t.Fatalf("GetWicketIssue after update: %v", err)
	}
	if updated.State != "triaged" {
		t.Errorf("expected state=triaged, got %q", updated.State)
	}
	if updated.TriageAction != "create_bead" {
		t.Errorf("expected triage_action=create_bead, got %q", updated.TriageAction)
	}
	if updated.BeadID != "Forge-xyz1" {
		t.Errorf("expected bead_id=Forge-xyz1, got %q", updated.BeadID)
	}
	if updated.ProcessedAt == nil {
		t.Error("expected ProcessedAt to be set after update")
	}

	// 7. Insert a second issue then ListWicketIssues.
	issue2 := WicketIssue{
		Repo:        repo,
		IssueNumber: 99,
		Title:       "Another issue",
		Author:      "bob",
		State:       "pending",
	}
	if err := db.InsertWicketIssue(issue2); err != nil {
		t.Fatalf("InsertWicketIssue second: %v", err)
	}

	// List all for repo.
	all, err := db.ListWicketIssues(ListWicketIssuesOpts{Repo: repo})
	if err != nil {
		t.Fatalf("ListWicketIssues: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 issues, got %d", len(all))
	}

	// Filter by state=pending should return only issue2.
	pending, err := db.ListWicketIssues(ListWicketIssuesOpts{Repo: repo, State: "pending"})
	if err != nil {
		t.Fatalf("ListWicketIssues pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending issue, got %d", len(pending))
	}
	if pending[0].IssueNumber != 99 {
		t.Errorf("expected issue 99, got %d", pending[0].IssueNumber)
	}

	// Filter by state=triaged should return only issue 42.
	triaged, err := db.ListWicketIssues(ListWicketIssuesOpts{Repo: repo, State: "triaged"})
	if err != nil {
		t.Fatalf("ListWicketIssues triaged: %v", err)
	}
	if len(triaged) != 1 || triaged[0].IssueNumber != issueNum {
		t.Errorf("expected issue %d in triaged list, got %v", issueNum, triaged)
	}

	// Limit=1 should return 1 result.
	limited, err := db.ListWicketIssues(ListWicketIssuesOpts{Repo: repo, Limit: 1})
	if err != nil {
		t.Fatalf("ListWicketIssues limit: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("expected 1 with limit=1, got %d", len(limited))
	}

	// 8. Duplicate insert should fail (unique constraint on repo+issue_number).
	if err := db.InsertWicketIssue(issue); err == nil {
		t.Error("expected error on duplicate insert, got nil")
	}
}

func TestDB_GetWicketSummary(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-state-wicket-summary-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Empty table returns empty slice.
	summaries, err := db.GetWicketSummary()
	if err != nil {
		t.Fatalf("GetWicketSummary on empty table: %v", err)
	}
	if len(summaries) != 0 {
		t.Errorf("expected empty summaries, got %d", len(summaries))
	}

	// Insert issues across two repos with varied states.
	insertIssue := func(repo string, number int, state string) {
		t.Helper()
		if err := db.InsertWicketIssue(WicketIssue{
			Repo:        repo,
			IssueNumber: number,
			Title:       "issue",
			State:       state,
		}); err != nil {
			t.Fatalf("InsertWicketIssue: %v", err)
		}
	}

	// repo-a: 3 open, 1 needs_human, 1 closed
	insertIssue("org/repo-a", 1, "pending")
	insertIssue("org/repo-a", 2, "needs_human")
	insertIssue("org/repo-a", 3, "bead_created")
	insertIssue("org/repo-a", 4, "closed") // not counted as open
	insertIssue("org/repo-a", 5, "merged") // not counted as open

	// repo-b: 2 open, 0 needs_human
	insertIssue("org/repo-b", 10, "ask_clarify")
	insertIssue("org/repo-b", 11, "dispatched")

	// repo-c: all closed — should NOT appear in results
	insertIssue("org/repo-c", 20, "closed")
	insertIssue("org/repo-c", 21, "merged")

	summaries, err = db.GetWicketSummary()
	if err != nil {
		t.Fatalf("GetWicketSummary: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("expected 2 repos with open issues, got %d: %+v", len(summaries), summaries)
	}

	// Results are sorted by repo name.
	if summaries[0].Repo != "org/repo-a" {
		t.Errorf("expected first repo to be org/repo-a, got %q", summaries[0].Repo)
	}
	if summaries[1].Repo != "org/repo-b" {
		t.Errorf("expected second repo to be org/repo-b, got %q", summaries[1].Repo)
	}

	// repo-a: 3 open (pending, needs_human, bead_created), 1 needs_human
	if summaries[0].OpenCount != 3 {
		t.Errorf("repo-a: expected 3 open, got %d", summaries[0].OpenCount)
	}
	if summaries[0].NeedsHumanCount != 1 {
		t.Errorf("repo-a: expected 1 needs_human, got %d", summaries[0].NeedsHumanCount)
	}

	// repo-b: 2 open, 0 needs_human
	if summaries[1].OpenCount != 2 {
		t.Errorf("repo-b: expected 2 open, got %d", summaries[1].OpenCount)
	}
	if summaries[1].NeedsHumanCount != 0 {
		t.Errorf("repo-b: expected 0 needs_human, got %d", summaries[1].NeedsHumanCount)
	}
}

func TestDB_IncrementRecoveryFailures_TripsAfterThree(t *testing.T) {
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

	bead, anvil := "BD-REC1", "anvil-1"

	// First two failures should not trip.
	for i := 1; i <= 2; i++ {
		failures, tripped, err := db.IncrementRecoveryFailures(bead, anvil, "bd timeout")
		if err != nil {
			t.Fatalf("failure %d: %v", i, err)
		}
		if failures != i {
			t.Errorf("failure %d: expected count=%d, got %d", i, i, failures)
		}
		if tripped {
			t.Errorf("failure %d: should not trip yet", i)
		}
	}

	// Third failure should trip.
	failures, tripped, err := db.IncrementRecoveryFailures(bead, anvil, "bd timeout")
	if err != nil {
		t.Fatal(err)
	}
	if failures != 3 {
		t.Errorf("expected 3 failures, got %d", failures)
	}
	if !tripped {
		t.Error("expected needs_human to be set after 3 failures")
	}

	// Verify via GetRetry.
	r, err := db.GetRetry(bead, anvil)
	if err != nil {
		t.Fatal(err)
	}
	if !r.NeedsHuman {
		t.Error("expected NeedsHuman=true")
	}
	if r.RecoveryFailures != 3 {
		t.Errorf("expected RecoveryFailures=3, got %d", r.RecoveryFailures)
	}
	if r.FirstRecoveryFailure == nil {
		t.Error("expected FirstRecoveryFailure to be set")
	}
}

func TestDB_IncrementRecoveryFailures_TripsAfterTimeWindow(t *testing.T) {
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

	bead, anvil := "BD-REC2", "anvil-1"

	// Insert a record with first_recovery_failure_at 31 minutes ago.
	oldTime := time.Now().Add(-31 * time.Minute).Format(dbTimeLayout)
	now := time.Now().Format(dbTimeLayout)
	_, err = db.conn.Exec(
		`INSERT INTO retries (bead_id, anvil, retry_count, needs_human, clarification_needed, dispatch_failures, recovery_failures, first_recovery_failure_at, last_error, updated_at)
		 VALUES (?, ?, 0, 0, 0, 0, 1, ?, '', ?)`,
		bead, anvil, oldTime, now,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Second failure should trip due to time window (>30 min since first).
	failures, tripped, err := db.IncrementRecoveryFailures(bead, anvil, "bd timeout")
	if err != nil {
		t.Fatal(err)
	}
	if failures != 2 {
		t.Errorf("expected 2 failures, got %d", failures)
	}
	if !tripped {
		t.Error("expected needs_human to be set due to time window")
	}
}

func TestDB_IncrementRecoveryFailures_NoTripUnder30Min(t *testing.T) {
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

	bead, anvil := "BD-REC3", "anvil-1"

	// Insert with first_recovery_failure_at 29 minutes ago (just under threshold).
	recentTime := time.Now().Add(-29 * time.Minute).Format(dbTimeLayout)
	now := time.Now().Format(dbTimeLayout)
	_, err = db.conn.Exec(
		`INSERT INTO retries (bead_id, anvil, retry_count, needs_human, clarification_needed, dispatch_failures, recovery_failures, first_recovery_failure_at, last_error, updated_at)
		 VALUES (?, ?, 0, 0, 0, 0, 1, ?, '', ?)`,
		bead, anvil, recentTime, now,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Second failure should NOT trip (only 2 failures, under 30 min).
	failures, tripped, err := db.IncrementRecoveryFailures(bead, anvil, "bd timeout")
	if err != nil {
		t.Fatal(err)
	}
	if failures != 2 {
		t.Errorf("expected 2 failures, got %d", failures)
	}
	if tripped {
		t.Error("should not trip with 2 failures under 30 min")
	}
}

func TestDB_ResetRecoveryFailures_ClearsOnSuccess(t *testing.T) {
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

	bead, anvil := "BD-REC4", "anvil-1"

	// Trip the recovery failure circuit breaker.
	for i := 0; i < 3; i++ {
		if _, _, err := db.IncrementRecoveryFailures(bead, anvil, "bd timeout"); err != nil {
			t.Fatalf("IncrementRecoveryFailures iteration %d: %v", i, err)
		}
	}
	r, err := db.GetRetry(bead, anvil)
	if err != nil {
		t.Fatalf("GetRetry: %v", err)
	}
	if !r.NeedsHuman {
		t.Fatal("expected NeedsHuman=true after 3 failures")
	}

	// Reset should clear recovery fields and needs_human.
	if err := db.ResetRecoveryFailures(bead, anvil); err != nil {
		t.Fatal(err)
	}

	r, err = db.GetRetry(bead, anvil)
	if err != nil {
		t.Fatalf("GetRetry after reset: %v", err)
	}
	if r.NeedsHuman {
		t.Error("expected NeedsHuman=false after reset")
	}
	if r.RecoveryFailures != 0 {
		t.Errorf("expected RecoveryFailures=0, got %d", r.RecoveryFailures)
	}
	if r.FirstRecoveryFailure != nil {
		t.Error("expected FirstRecoveryFailure=nil after reset")
	}
}

func TestDB_ResetRecoveryFailures_PreservesUnrelatedNeedsHuman(t *testing.T) {
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

	bead, anvil := "BD-REC5", "anvil-1"

	// Set needs_human via MarkNeedsHuman (not recovery failures).
	if err := db.MarkNeedsHuman(bead, anvil, "no-diff hard reject"); err != nil {
		t.Fatal(err)
	}

	// ResetRecoveryFailures should NOT clear this needs_human.
	if err := db.ResetRecoveryFailures(bead, anvil); err != nil {
		t.Fatal(err)
	}

	r, err := db.GetRetry(bead, anvil)
	if err != nil {
		t.Fatalf("GetRetry: %v", err)
	}
	if !r.NeedsHuman {
		t.Error("expected NeedsHuman=true to be preserved (unrelated to recovery)")
	}
}

func TestDB_ResetRecoveryFailures_ClearsBeforeCircuitTrips(t *testing.T) {
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

	bead, anvil := "BD-REC7", "anvil-1"

	// Record 2 failures — not enough to trip the circuit.
	for i := 0; i < 2; i++ {
		_, tripped, err := db.IncrementRecoveryFailures(bead, anvil, "bd timeout")
		if err != nil {
			t.Fatalf("IncrementRecoveryFailures iteration %d: %v", i, err)
		}
		if tripped {
			t.Fatalf("circuit should not trip after %d failures", i+1)
		}
	}

	r, err := db.GetRetry(bead, anvil)
	if err != nil {
		t.Fatalf("GetRetry: %v", err)
	}
	if r.RecoveryFailures != 2 {
		t.Errorf("expected RecoveryFailures=2, got %d", r.RecoveryFailures)
	}
	if r.FirstRecoveryFailure == nil {
		t.Error("expected FirstRecoveryFailure to be set")
	}

	// A successful recovery should clear the counter and timestamp even though
	// needs_human was never set.
	if err := db.ResetRecoveryFailures(bead, anvil); err != nil {
		t.Fatal(err)
	}

	r, err = db.GetRetry(bead, anvil)
	if err != nil {
		t.Fatalf("GetRetry after reset: %v", err)
	}
	if r.RecoveryFailures != 0 {
		t.Errorf("expected RecoveryFailures=0 after reset, got %d", r.RecoveryFailures)
	}
	if r.FirstRecoveryFailure != nil {
		t.Error("expected FirstRecoveryFailure=nil after reset")
	}
	if r.NeedsHuman {
		t.Error("expected NeedsHuman=false (was never set)")
	}
}

func TestDB_ResetRetry_ClearsRecoveryFailures(t *testing.T) {
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

	bead, anvil := "BD-REC6", "anvil-1"

	// Trip recovery failures.
	for i := 0; i < 3; i++ {
		if _, _, err := db.IncrementRecoveryFailures(bead, anvil, "bd timeout"); err != nil {
			t.Fatalf("IncrementRecoveryFailures iteration %d: %v", i, err)
		}
	}

	// Full reset should clear everything.
	if err := db.ResetRetry(bead, anvil); err != nil {
		t.Fatal(err)
	}

	r, err := db.GetRetry(bead, anvil)
	if err != nil {
		t.Fatalf("GetRetry: %v", err)
	}
	if r.RecoveryFailures != 0 {
		t.Errorf("expected RecoveryFailures=0 after full reset, got %d", r.RecoveryFailures)
	}
	if r.FirstRecoveryFailure != nil {
		t.Error("expected FirstRecoveryFailure=nil after full reset")
	}
	if r.NeedsHuman {
		t.Error("expected NeedsHuman=false after full reset")
	}
}

func TestDB_InsertFindingDedup(t *testing.T) {
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

	f := Finding{
		Anvil:       "anvil-1",
		PRNumber:    42,
		HeadSHA:     "abc123",
		FindingHash: "hash-1",
		File:        "main.go",
		Severity:    "high",
		Title:       "potential nil deref",
	}
	if err := db.InsertFinding(f); err != nil {
		t.Fatalf("first InsertFinding: %v", err)
	}
	// Inserting a finding with the same finding_hash is a silent no-op.
	if err := db.InsertFinding(f); err != nil {
		t.Fatalf("duplicate InsertFinding: %v", err)
	}

	var count int
	if err := db.Conn().QueryRow(
		`SELECT COUNT(*) FROM pr_findings WHERE finding_hash = ?`, "hash-1",
	).Scan(&count); err != nil {
		t.Fatalf("counting findings: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row after duplicate insert, got %d", count)
	}
}

func TestDB_LastReviewedSHA(t *testing.T) {
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

	// No runs yet → empty string, no error.
	sha, err := db.LastReviewedSHA("anvil-1", 7)
	if err != nil {
		t.Fatalf("LastReviewedSHA (none): %v", err)
	}
	if sha != "" {
		t.Errorf("expected empty SHA before any run, got %q", sha)
	}

	if err := db.RecordAssayRun(&AssayRun{Anvil: "anvil-1", PRNumber: 7, HeadSHA: "sha-old"}); err != nil {
		t.Fatalf("RecordAssayRun (old): %v", err)
	}
	if err := db.RecordAssayRun(&AssayRun{Anvil: "anvil-1", PRNumber: 7, HeadSHA: "sha-new"}); err != nil {
		t.Fatalf("RecordAssayRun (new): %v", err)
	}

	sha, err = db.LastReviewedSHA("anvil-1", 7)
	if err != nil {
		t.Fatalf("LastReviewedSHA (after runs): %v", err)
	}
	if sha != "sha-new" {
		t.Errorf("expected latest SHA %q, got %q", "sha-new", sha)
	}
}

func TestDB_CountAssayRuns(t *testing.T) {
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

	// No runs yet → 0.
	n, err := db.CountAssayRuns("anvil-1", 7)
	if err != nil {
		t.Fatalf("CountAssayRuns (none): %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 runs before any, got %d", n)
	}

	// Two executed runs plus one skipped run (skipped must NOT count).
	if err := db.RecordAssayRun(&AssayRun{Anvil: "anvil-1", PRNumber: 7, HeadSHA: "sha-1"}); err != nil {
		t.Fatalf("RecordAssayRun 1: %v", err)
	}
	if err := db.RecordAssayRun(&AssayRun{Anvil: "anvil-1", PRNumber: 7, HeadSHA: "sha-2"}); err != nil {
		t.Fatalf("RecordAssayRun 2: %v", err)
	}
	if err := db.RecordAssayRun(&AssayRun{Anvil: "anvil-1", PRNumber: 7, HeadSHA: "sha-3", SkippedReason: "diff fetch failed"}); err != nil {
		t.Fatalf("RecordAssayRun skipped: %v", err)
	}
	// A run for a different PR must not bleed in.
	if err := db.RecordAssayRun(&AssayRun{Anvil: "anvil-1", PRNumber: 8, HeadSHA: "sha-x"}); err != nil {
		t.Fatalf("RecordAssayRun other PR: %v", err)
	}

	n, err = db.CountAssayRuns("anvil-1", 7)
	if err != nil {
		t.Fatalf("CountAssayRuns (after runs): %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 executed runs (skipped excluded), got %d", n)
	}
}

func TestDB_IncrementConsecutiveMiss(t *testing.T) {
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

	if err := db.InsertFinding(Finding{
		Anvil:       "anvil-1",
		PRNumber:    1,
		FindingHash: "miss-hash",
	}); err != nil {
		t.Fatalf("InsertFinding: %v", err)
	}

	if err := db.IncrementConsecutiveMiss("miss-hash"); err != nil {
		t.Fatalf("IncrementConsecutiveMiss (1): %v", err)
	}
	if err := db.IncrementConsecutiveMiss("miss-hash"); err != nil {
		t.Fatalf("IncrementConsecutiveMiss (2): %v", err)
	}

	var misses int
	if err := db.Conn().QueryRow(
		`SELECT consecutive_misses FROM pr_findings WHERE finding_hash = ?`, "miss-hash",
	).Scan(&misses); err != nil {
		t.Fatalf("reading consecutive_misses: %v", err)
	}
	if misses != 2 {
		t.Errorf("expected consecutive_misses=2, got %d", misses)
	}
}

func TestDB_MarkResolved(t *testing.T) {
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

	if err := db.InsertFinding(Finding{
		Anvil:       "anvil-1",
		PRNumber:    1,
		FindingHash: "resolve-hash",
	}); err != nil {
		t.Fatalf("InsertFinding: %v", err)
	}

	if err := db.MarkResolved("resolve-hash"); err != nil {
		t.Fatalf("MarkResolved: %v", err)
	}

	var resolvedAt *string
	if err := db.Conn().QueryRow(
		`SELECT resolved_at FROM pr_findings WHERE finding_hash = ?`, "resolve-hash",
	).Scan(&resolvedAt); err != nil {
		t.Fatalf("reading resolved_at: %v", err)
	}
	if resolvedAt == nil || *resolvedAt == "" {
		t.Error("expected resolved_at to be set after MarkResolved")
	}
}

func TestDB_RecentEventsMatching(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "forge-eventmatch-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Seed one old event that matches our filter, then bury it under far more
	// than EventFetchLimit (100) newer, non-matching events. An in-memory
	// filter over the most-recent ~100 rows would never see the old one.
	if err := db.LogEventAt(EventBeadClaimed, "claimed Forge-OLD1", "Forge-OLD1", "anvil-a", base); err != nil {
		t.Fatalf("seed old event: %v", err)
	}
	for i := 0; i < 150; i++ {
		at := base.Add(time.Duration(i+1) * time.Minute)
		if err := db.LogEventAt(EventSmithDone, "smith finished noise", fmt.Sprintf("Forge-N%03d", i), "anvil-a", at); err != nil {
			t.Fatalf("seed noise event %d: %v", i, err)
		}
	}
	// Excluded poll/poll_error rows that also contain the search term — they
	// must never appear in results.
	if err := db.LogEventAt(EventPoll, "poll Forge-OLD1", "Forge-OLD1", "anvil-a", base.Add(200*time.Minute)); err != nil {
		t.Fatalf("seed poll event: %v", err)
	}
	if err := db.LogEventAt(EventPollError, "poll error forge-old1", "Forge-OLD1", "anvil-a", base.Add(201*time.Minute)); err != nil {
		t.Fatalf("seed poll_error event: %v", err)
	}

	excluded := []EventType{EventPoll, EventPollError}

	t.Run("matches an event older than the load window", func(t *testing.T) {
		results, err := db.RecentEventsMatching("Forge-OLD1", 500, excluded)
		if err != nil {
			t.Fatalf("RecentEventsMatching: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("expected exactly 1 match, got %d", len(results))
		}
		if results[0].BeadID != "Forge-OLD1" || results[0].Type != EventBeadClaimed {
			t.Fatalf("unexpected match: %+v", results[0])
		}
	})

	t.Run("is case-insensitive", func(t *testing.T) {
		results, err := db.RecentEventsMatching("forge-old1", 500, excluded)
		if err != nil {
			t.Fatalf("RecentEventsMatching: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("expected exactly 1 case-insensitive match, got %d", len(results))
		}
		if results[0].BeadID != "Forge-OLD1" {
			t.Fatalf("unexpected match: %+v", results[0])
		}
	})

	t.Run("excludes poll and poll_error types", func(t *testing.T) {
		results, err := db.RecentEventsMatching("Forge-OLD1", 500, excluded)
		if err != nil {
			t.Fatalf("RecentEventsMatching: %v", err)
		}
		for _, e := range results {
			if e.Type == EventPoll || e.Type == EventPollError {
				t.Fatalf("excluded type leaked into results: %+v", e)
			}
		}
	})

	t.Run("enforces the result cap", func(t *testing.T) {
		// "smith" matches all 150 noise events; the cap must bound the result.
		results, err := db.RecentEventsMatching("smith", 25, excluded)
		if err != nil {
			t.Fatalf("RecentEventsMatching: %v", err)
		}
		if len(results) != 25 {
			t.Fatalf("expected capped 25 results, got %d", len(results))
		}
		// Newest-first ordering: the most recent noise event should be first.
		if results[0].BeadID != "Forge-N149" {
			t.Fatalf("expected newest-first ordering, got first=%+v", results[0])
		}
	})

	t.Run("empty pattern falls back to exclusion-only", func(t *testing.T) {
		results, err := db.RecentEventsMatching("", 10, excluded)
		if err != nil {
			t.Fatalf("RecentEventsMatching: %v", err)
		}
		if len(results) != 10 {
			t.Fatalf("expected 10 results for empty pattern, got %d", len(results))
		}
		for _, e := range results {
			if e.Type == EventPoll || e.Type == EventPollError {
				t.Fatalf("excluded type leaked into empty-pattern results: %+v", e)
			}
		}
	})

	t.Run("escapes LIKE wildcards literally", func(t *testing.T) {
		if err := db.LogEventAt(EventSmithDone, "100% done", "Forge-PCT1", "anvil-a", base.Add(300*time.Minute)); err != nil {
			t.Fatalf("seed percent event: %v", err)
		}
		// "%" should NOT match everything — only the literal character.
		results, err := db.RecentEventsMatching("%", 500, excluded)
		if err != nil {
			t.Fatalf("RecentEventsMatching: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("expected 1 match for literal '%%', got %d", len(results))
		}
		if results[0].BeadID != "Forge-PCT1" {
			t.Fatalf("unexpected match: %+v", results[0])
		}

		// "_" should NOT match any single character — only the literal underscore.
		if err := db.LogEventAt(EventSmithDone, "under_score test", "Forge-US1", "anvil-a", base.Add(301*time.Minute)); err != nil {
			t.Fatalf("seed underscore event: %v", err)
		}
		results, err = db.RecentEventsMatching("r_s", 500, excluded)
		if err != nil {
			t.Fatalf("RecentEventsMatching: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("expected 1 match for literal 'r_s', got %d", len(results))
		}
		if results[0].BeadID != "Forge-US1" {
			t.Fatalf("unexpected match: %+v", results[0])
		}
	})
}

// TestDB_ActiveDispatchWorkers_CountsSchematic is a regression test for the bug
// where a dispatched worker in the 'schematic' pre-analysis phase dropped out of
// the dispatch capacity count, allowing a second Smith to be dispatched while the
// max_total_smiths cap was 1. Schematic runs on the claimed worker before Smith,
// so it must keep counting against the dispatch cap.
func TestDB_ActiveDispatchWorkers_CountsSchematic(t *testing.T) {
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

	// A worker that has entered the schematic pre-analysis phase.
	if err := db.InsertWorker(&Worker{
		ID: "w-schematic", BeadID: "BD-1", Anvil: "anvil-1",
		Status: WorkerRunning, Phase: "schematic",
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// A bellows (background) worker that must NOT count against the dispatch cap.
	if err := db.InsertWorker(&Worker{
		ID: "w-bellows", BeadID: "BD-2", Anvil: "anvil-1",
		Status: WorkerMonitoring, Phase: "bellows",
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Global dispatch count must include the schematic worker.
	dispatch, err := db.ActiveDispatchWorkers()
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatch) != 1 {
		t.Fatalf("expected 1 dispatch worker (schematic), got %d: %+v", len(dispatch), dispatch)
	}
	if dispatch[0].ID != "w-schematic" {
		t.Errorf("expected w-schematic to count against dispatch cap, got %s", dispatch[0].ID)
	}

	// Per-anvil dispatch count must also include the schematic worker so a second
	// dispatch is blocked when max_smiths/max_total_smiths is 1.
	byAnvil, err := db.ActiveDispatchWorkersByAnvil("anvil-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(byAnvil) != 1 {
		t.Fatalf("expected 1 per-anvil dispatch worker (schematic), got %d: %+v", len(byAnvil), byAnvil)
	}
	if byAnvil[0].ID != "w-schematic" {
		t.Errorf("expected w-schematic in per-anvil dispatch count, got %s", byAnvil[0].ID)
	}
}

// TestDB_ActiveDispatchWorkers_CountsPaused is a regression test for the bug
// where a pipeline worker parked by an operator pause (status 'paused') dropped
// out of the dispatch capacity count. A parked worker still holds its worktree
// and respawns a running smith on resume, so it must keep occupying its dispatch
// slot; excluding it would let the daemon dispatch a replacement and then
// over-subscribe max_total_smiths once the paused worker resumes.
func TestDB_ActiveDispatchWorkers_CountsPaused(t *testing.T) {
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

	// A pipeline worker in the smith phase that an operator has paused/parked.
	if err := db.InsertWorker(&Worker{
		ID: "w-paused", BeadID: "BD-1", Anvil: "anvil-1",
		Status: WorkerPaused, Phase: "smith",
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Global dispatch count must include the paused worker so its slot is held.
	dispatch, err := db.ActiveDispatchWorkers()
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatch) != 1 {
		t.Fatalf("expected 1 dispatch worker (paused), got %d: %+v", len(dispatch), dispatch)
	}
	if dispatch[0].ID != "w-paused" {
		t.Errorf("expected w-paused to count against dispatch cap, got %s", dispatch[0].ID)
	}

	// Per-anvil dispatch count must also include the paused worker so a second
	// dispatch is blocked while it is parked awaiting resume.
	byAnvil, err := db.ActiveDispatchWorkersByAnvil("anvil-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(byAnvil) != 1 {
		t.Fatalf("expected 1 per-anvil dispatch worker (paused), got %d: %+v", len(byAnvil), byAnvil)
	}
	if byAnvil[0].ID != "w-paused" {
		t.Errorf("expected w-paused in per-anvil dispatch count, got %s", byAnvil[0].ID)
	}
}
