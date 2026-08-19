package rebase

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
)

func openTestDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestRebaseWithSmith_RecordsSmithPID pins that rebase records its Smith PID on
// the worker row. The PID is the only handle the detach/kill path has on the
// session: a row without one is merely marked failed while the claude process
// keeps running — and keeps force-pushing the branch that was just taken off
// the automatic loop.
func TestRebaseWithSmith_RecordsSmithPID(t *testing.T) {
	orig := smithSpawnFn
	defer func() { smithSpawnFn = orig }()

	db := openTestDB(t)
	workerID := "test-rebase-pid"
	if err := db.InsertWorker(&state.Worker{
		ID:        workerID,
		BeadID:    "test-1",
		Anvil:     "test-anvil",
		Branch:    "forge/test",
		Status:    state.WorkerRunning,
		Phase:     "rebase",
		PRNumber:  42,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InsertWorker: %v", err)
	}

	smithSpawnFn = func(_ context.Context, _, _, _ string, _ provider.Provider, _ []string) (*smith.Process, error) {
		proc := smith.NewProcessForTest(&smith.Result{ExitCode: 0, ResultSubtype: "success"})
		proc.PID = 4244
		return proc, nil
	}

	p := &Params{
		WorktreePath: t.TempDir(),
		Branch:       "forge/test",
		BaseBranch:   "main",
		BeadID:       "test-1",
		AnvilName:    "test-anvil",
		PRNumber:     42,
		DB:           db,
		WorkerID:     workerID,
		Providers:    []provider.Provider{{Kind: provider.Claude, Model: "test"}},
	}
	if err := p.rebaseWithSmith(context.Background(), p.Providers); err != nil {
		t.Fatalf("rebaseWithSmith: %v", err)
	}

	w, err := db.GetWorker(workerID)
	if err != nil {
		t.Fatalf("GetWorker: %v", err)
	}
	if w == nil {
		t.Fatalf("worker %s not found", workerID)
	}
	if w.PID != 4244 {
		t.Errorf("worker PID = %d, want 4244 — without it the detach kill is a no-op that marks the row failed while the session keeps running", w.PID)
	}
}
