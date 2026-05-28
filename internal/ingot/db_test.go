package ingot

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// openTestDB opens a temporary state database and returns its *sql.DB so that
// the ingots tables (added via state migrations) are ready to use.
func openTestDB(t *testing.T) (*state.DB, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "forge-ingot-test-*")
	if err != nil {
		t.Fatal(err)
	}
	sdb, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	return sdb, func() {
		sdb.Close()
		os.RemoveAll(dir)
	}
}

func TestInsertAndGetIngot(t *testing.T) {
	sdb, cleanup := openTestDB(t)
	defer cleanup()
	db := sdb.Conn()

	prNum := 42
	ingot := &Ingot{
		BeadID:   "bd-1",
		Anvil:    "anvil-1",
		WorkerID: "worker-abc",
		Status:   StatusInit,
		Title:    "My feature",
		Branch:   "forge/bd-1",
		PRNumber: &prNum,
		PRURL:    "https://github.com/org/repo/pull/42",
	}

	if err := InsertIngot(db, ingot); err != nil {
		t.Fatalf("InsertIngot: %v", err)
	}
	if ingot.ID == 0 {
		t.Fatal("expected ID to be set after insert")
	}
	if ingot.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}

	got, err := GetIngot(db, "bd-1", "anvil-1")
	if err != nil {
		t.Fatalf("GetIngot: %v", err)
	}
	if got == nil {
		t.Fatal("GetIngot returned nil")
	}
	if got.BeadID != "bd-1" {
		t.Errorf("BeadID = %q; want %q", got.BeadID, "bd-1")
	}
	if got.Title != "My feature" {
		t.Errorf("Title = %q; want %q", got.Title, "My feature")
	}
	if got.PRNumber == nil || *got.PRNumber != 42 {
		t.Errorf("PRNumber = %v; want 42", got.PRNumber)
	}
	if got.Status != StatusInit {
		t.Errorf("Status = %q; want %q", got.Status, StatusInit)
	}
}

func TestGetIngot_NotFound(t *testing.T) {
	sdb, cleanup := openTestDB(t)
	defer cleanup()

	got, err := GetIngot(sdb.Conn(), "nonexistent", "anvil-1")
	if err != nil {
		t.Fatalf("GetIngot: %v", err)
	}
	if got != nil {
		t.Fatal("expected nil for missing ingot")
	}
}

func TestUpdateIngotStatus(t *testing.T) {
	sdb, cleanup := openTestDB(t)
	defer cleanup()
	db := sdb.Conn()

	ingot := &Ingot{BeadID: "bd-2", Anvil: "anvil-1", Status: StatusInit}
	if err := InsertIngot(db, ingot); err != nil {
		t.Fatal(err)
	}

	if err := UpdateIngotStatus(db, "bd-2", "anvil-1", StatusSmith); err != nil {
		t.Fatalf("UpdateIngotStatus: %v", err)
	}

	got, err := GetIngot(db, "bd-2", "anvil-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSmith {
		t.Errorf("Status = %q; want %q", got.Status, StatusSmith)
	}
}

func TestUpdateIngotTemperResults(t *testing.T) {
	sdb, cleanup := openTestDB(t)
	defer cleanup()
	db := sdb.Conn()

	ingot := &Ingot{BeadID: "bd-3", Anvil: "anvil-1", Status: StatusTemper}
	if err := InsertIngot(db, ingot); err != nil {
		t.Fatal(err)
	}

	if err := UpdateIngotTemperResults(db, "bd-3", "anvil-1", false, "go test", 5000); err != nil {
		t.Fatalf("UpdateIngotTemperResults: %v", err)
	}

	got, err := GetIngot(db, "bd-3", "anvil-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.TemperPassed {
		t.Error("TemperPassed = true; want false")
	}
	if got.TemperFailedStep != "go test" {
		t.Errorf("TemperFailedStep = %q; want %q", got.TemperFailedStep, "go test")
	}
	if got.TemperDurationMs != 5000 {
		t.Errorf("TemperDurationMs = %d; want 5000", got.TemperDurationMs)
	}
}

func TestUpdateIngotPR(t *testing.T) {
	sdb, cleanup := openTestDB(t)
	defer cleanup()
	db := sdb.Conn()

	// Insert a real PR row so the FK constraint on ingots.pr_id is satisfied.
	pr := &state.PR{
		Number:    99,
		Anvil:     "anvil-1",
		BeadID:    "bd-4",
		Branch:    "forge/bd-4",
		Status:    state.PROpen,
		CreatedAt: time.Now(),
	}
	if err := sdb.InsertPR(pr); err != nil {
		t.Fatalf("InsertPR: %v", err)
	}

	ingot := &Ingot{BeadID: "bd-4", Anvil: "anvil-1", Status: StatusApproved}
	if err := InsertIngot(db, ingot); err != nil {
		t.Fatal(err)
	}

	prID := pr.ID
	if err := UpdateIngotPR(db, "bd-4", "anvil-1", 99, "https://github.com/org/repo/pull/99", &prID); err != nil {
		t.Fatalf("UpdateIngotPR: %v", err)
	}

	got, err := GetIngot(db, "bd-4", "anvil-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.PRNumber == nil || *got.PRNumber != 99 {
		t.Errorf("PRNumber = %v; want 99", got.PRNumber)
	}
	if got.PRURL != "https://github.com/org/repo/pull/99" {
		t.Errorf("PRURL = %q", got.PRURL)
	}
	if got.PRID == nil || *got.PRID != pr.ID {
		t.Errorf("PRID = %v; want %d", got.PRID, pr.ID)
	}
}

func TestInsertAndGetTestResults(t *testing.T) {
	sdb, cleanup := openTestDB(t)
	defer cleanup()
	db := sdb.Conn()

	ingot := &Ingot{BeadID: "bd-5", Anvil: "anvil-1", Status: StatusTemper}
	if err := InsertIngot(db, ingot); err != nil {
		t.Fatal(err)
	}

	// Insert steps out of order to verify ORDER BY step_index
	steps := []TestResult{
		{IngotID: ingot.ID, StepIndex: 2, StepName: "go test", Command: "go test ./...", ExitCode: 0, DurationMs: 1200, Passed: true, RecordedAt: time.Now()},
		{IngotID: ingot.ID, StepIndex: 0, StepName: "go build", Command: "go build ./...", ExitCode: 0, DurationMs: 800, Passed: true, RecordedAt: time.Now()},
		{IngotID: ingot.ID, StepIndex: 1, StepName: "go vet", Command: "go vet ./...", ExitCode: 1, DurationMs: 200, Passed: false, Optional: true, OutputSummary: "some warnings", RecordedAt: time.Now()},
		// A path-skipped step records Passed=true, Skipped=true, zero duration/exit.
		{IngotID: ingot.ID, StepIndex: 3, StepName: "client-build", Command: "npm run build", ExitCode: 0, DurationMs: 0, Passed: true, Skipped: true, RecordedAt: time.Now()},
	}
	for i := range steps {
		if err := InsertTestResult(db, &steps[i]); err != nil {
			t.Fatalf("InsertTestResult[%d]: %v", i, err)
		}
		if steps[i].ID == 0 {
			t.Errorf("step[%d].ID not set", i)
		}
	}

	results, err := GetTestResults(db, ingot.ID)
	if err != nil {
		t.Fatalf("GetTestResults: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("got %d results; want 4", len(results))
	}
	// Verify ordering by step_index
	for i, want := range []int{0, 1, 2, 3} {
		if results[i].StepIndex != want {
			t.Errorf("results[%d].StepIndex = %d; want %d", i, results[i].StepIndex, want)
		}
	}
	if results[1].StepName != "go vet" {
		t.Errorf("results[1].StepName = %q; want go vet", results[1].StepName)
	}
	if results[1].Passed {
		t.Error("results[1].Passed = true; want false")
	}
	if !results[1].Optional {
		t.Error("results[1].Optional = false; want true")
	}
	if results[1].Skipped {
		t.Error("results[1].Skipped = true; want false")
	}
	// The skipped step must round-trip its Skipped flag.
	if !results[3].Skipped {
		t.Error("results[3].Skipped = false; want true")
	}
	if !results[3].Passed {
		t.Error("results[3].Passed = false; want true (skipped steps are not failures)")
	}
}

func TestGetIngotsByStatus(t *testing.T) {
	sdb, cleanup := openTestDB(t)
	defer cleanup()
	db := sdb.Conn()

	for _, tc := range []struct {
		bead   string
		status Status
	}{
		{"bd-10", StatusPROpen},
		{"bd-11", StatusPROpen},
		{"bd-12", StatusFailed},
	} {
		if err := InsertIngot(db, &Ingot{BeadID: tc.bead, Anvil: "anvil-1", Status: tc.status}); err != nil {
			t.Fatal(err)
		}
	}

	open, err := GetIngotsByStatus(db, StatusPROpen, 10)
	if err != nil {
		t.Fatalf("GetIngotsByStatus(pr_open): %v", err)
	}
	if len(open) != 2 {
		t.Errorf("got %d pr_open ingots; want 2", len(open))
	}

	failed, err := GetIngotsByStatus(db, StatusFailed, 10)
	if err != nil {
		t.Fatalf("GetIngotsByStatus(failed): %v", err)
	}
	if len(failed) != 1 {
		t.Errorf("got %d failed ingots; want 1", len(failed))
	}

	// Verify limit is respected
	limited, err := GetIngotsByStatus(db, StatusPROpen, 1)
	if err != nil {
		t.Fatalf("GetIngotsByStatus(limit=1): %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("got %d results with limit=1; want 1", len(limited))
	}
}

func TestUniqueConstraint(t *testing.T) {
	sdb, cleanup := openTestDB(t)
	defer cleanup()
	db := sdb.Conn()

	ingot := &Ingot{BeadID: "bd-20", Anvil: "anvil-1", Status: StatusInit}
	if err := InsertIngot(db, ingot); err != nil {
		t.Fatal(err)
	}

	duplicate := &Ingot{BeadID: "bd-20", Anvil: "anvil-1", Status: StatusSmith}
	err := InsertIngot(db, duplicate)
	if err == nil {
		t.Fatal("expected error on duplicate (bead_id, anvil); got nil")
	}
}

func TestEagerLoadTestResults(t *testing.T) {
	sdb, cleanup := openTestDB(t)
	defer cleanup()
	db := sdb.Conn()

	ingot := &Ingot{BeadID: "bd-30", Anvil: "anvil-1", Status: StatusTemper}
	if err := InsertIngot(db, ingot); err != nil {
		t.Fatal(err)
	}
	tr := &TestResult{
		IngotID:   ingot.ID,
		StepIndex: 0,
		StepName:  "go build",
		Passed:    true,
		RecordedAt: time.Now(),
	}
	if err := InsertTestResult(db, tr); err != nil {
		t.Fatal(err)
	}

	got, err := GetIngot(db, "bd-30", "anvil-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.TestResults) != 1 {
		t.Fatalf("expected 1 eager-loaded TestResult; got %d", len(got.TestResults))
	}
	if got.TestResults[0].StepName != "go build" {
		t.Errorf("StepName = %q; want go build", got.TestResults[0].StepName)
	}
}
