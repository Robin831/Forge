// Package worker implements the Worker lifecycle state machine (FSM).
//
// A Worker ties together the worktree manager, Smith (Claude) spawner,
// prompt builder, and state database to manage the full lifecycle:
//
//	pending → running → reviewing → done
//	                  ↘ failed
//	                  ↘ timeout
package worker

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/worktree"
)

// Worker represents a single bead being worked on by a Smith.
type Worker struct {
	// ID is a unique identifier for this worker session.
	ID string
	// Bead is the bead being worked on.
	Bead poller.Bead
	// AnvilName is the name of the anvil this bead belongs to.
	AnvilName string
	// AnvilConfig is the anvil configuration.
	AnvilConfig config.AnvilConfig
	// Status is the current lifecycle state.
	Status state.WorkerStatus

	// Internal references
	wt      *worktree.Worktree
	process *smith.Process
	db      *state.DB
	wtMgr   *worktree.Manager
	builder *prompt.Builder
}

// Params holds the dependencies needed to create a Worker.
type Params struct {
	DB              *state.DB
	WorktreeManager *worktree.Manager
	PromptBuilder   *prompt.Builder
	AnvilName       string
	AnvilConfig     config.AnvilConfig
	Bead            poller.Bead
	ExtraFlags      []string
}

// New creates a new Worker in the pending state and records it in the state DB.
func New(p Params) (*Worker, error) {
	id := fmt.Sprintf("%s-%s-%d", p.AnvilName, p.Bead.ID, time.Now().Unix())

	w := &Worker{
		ID:          id,
		Bead:        p.Bead,
		AnvilName:   p.AnvilName,
		AnvilConfig: p.AnvilConfig,
		Status:      state.WorkerPending,
		db:          p.DB,
		wtMgr:       p.WorktreeManager,
		builder:     p.PromptBuilder,
	}

	// Record in state DB
	dbWorker := &state.Worker{
		ID:        id,
		BeadID:    p.Bead.ID,
		Anvil:     p.AnvilName,
		Branch:    "forge/" + p.Bead.ID,
		PID:       0,
		Status:    state.WorkerPending,
		StartedAt: time.Now(),
	}

	if err := p.DB.InsertWorker(dbWorker); err != nil {
		return nil, fmt.Errorf("recording worker in state DB: %w", err)
	}

	_ = p.DB.LogEvent(state.EventBeadClaimed, fmt.Sprintf("Worker %s created for bead %s", id, p.Bead.ID), p.Bead.ID, p.AnvilName)

	return w, nil
}

// Run executes the full worker lifecycle:
//  1. Create worktree
//  2. Build prompt
//  3. Spawn Smith process
//  4. Wait for completion
//  5. Teardown worktree
//
// The returned Result comes from the Smith process.
// Run transitions the worker through: pending → running → done/failed.
func (w *Worker) Run(ctx context.Context, extraFlags []string) (*smith.Result, error) {
	var result *smith.Result

	defer func() {
		// Always attempt worktree cleanup
		if w.wt != nil {
			if err := w.wtMgr.Remove(ctx, w.AnvilConfig.Path, w.wt); err != nil {
				log.Printf("Warning: failed to remove worktree for %s: %v", w.ID, err)
			}
		}
	}()

	// Step 1: Create worktree
	log.Printf("[%s] Creating worktree for bead %s", w.ID, w.Bead.ID)
	wt, err := w.wtMgr.Create(ctx, w.AnvilConfig.Path, w.Bead.ID)
	if err != nil {
		w.fail(fmt.Sprintf("worktree creation failed: %v", err))
		return nil, fmt.Errorf("creating worktree: %w", err)
	}
	w.wt = wt

	// Step 2: Build prompt
	log.Printf("[%s] Building prompt", w.ID)
	beadCtx := prompt.BeadContext{
		BeadID:       w.Bead.ID,
		Title:        w.Bead.Title,
		Description:  w.Bead.Description,
		IssueType:    w.Bead.IssueType,
		Priority:     w.Bead.Priority,
		Parent:       w.Bead.Parent,
		Branch:       wt.Branch,
		AnvilName:    w.AnvilName,
		AnvilPath:    w.AnvilConfig.Path,
		WorktreePath: wt.Path,
	}

	// Check for custom template
	customTmpl := prompt.LoadCustomTemplate(w.AnvilConfig.Path)
	if customTmpl != "" {
		w.builder.CustomTemplate = customTmpl
	}

	promptText, err := w.builder.Build(beadCtx)
	if err != nil {
		w.fail(fmt.Sprintf("prompt build failed: %v", err))
		return nil, fmt.Errorf("building prompt: %w", err)
	}

	// Step 3: Transition to running and spawn Smith
	w.transition(state.WorkerRunning)
	_ = w.db.LogEvent(state.EventSmithStarted, fmt.Sprintf("Smith spawned in %s", wt.Path), w.Bead.ID, w.AnvilName)

	log.Printf("[%s] Spawning Smith in %s", w.ID, wt.Path)
	logDir := filepath.Join(w.AnvilConfig.Path, ".workers", "logs")
	process, err := smith.Spawn(ctx, wt.Path, promptText, logDir, extraFlags)
	if err != nil {
		w.fail(fmt.Sprintf("smith spawn failed: %v", err))
		return nil, fmt.Errorf("spawning smith: %w", err)
	}
	w.process = process

	// Update PID in state DB
	_ = w.db.UpdateWorkerPID(w.ID, process.PID)

	// Step 4: Wait for Smith to complete
	log.Printf("[%s] Waiting for Smith (PID %d)", w.ID, process.PID)
	result = process.Wait()

	// Update log path in state DB
	_ = w.db.UpdateWorkerLogPath(w.ID, process.LogPath)

	// Step 5: Determine outcome
	if result.ExitCode == 0 {
		w.transition(state.WorkerDone)
		_ = w.db.LogEvent(state.EventSmithDone,
			fmt.Sprintf("Smith completed successfully (%.1fs, $%.4f)", result.Duration.Seconds(), result.CostUSD),
			w.Bead.ID, w.AnvilName)
		log.Printf("[%s] Smith completed successfully in %s", w.ID, result.Duration)
	} else {
		w.transition(state.WorkerFailed)
		_ = w.db.LogEvent(state.EventSmithFailed,
			fmt.Sprintf("Smith failed with exit code %d (%.1fs)", result.ExitCode, result.Duration.Seconds()),
			w.Bead.ID, w.AnvilName)
		log.Printf("[%s] Smith failed with exit code %d", w.ID, result.ExitCode)
	}

	return result, nil
}

// Cancel stops a running worker.
func (w *Worker) Cancel() {
	if w.process != nil {
		_ = w.process.Kill()
	}
	w.transition(state.WorkerFailed)
	_ = w.db.LogEvent(state.EventSmithFailed, "Worker cancelled", w.Bead.ID, w.AnvilName)
}

// transition updates the worker status in both the Worker struct and state DB.
func (w *Worker) transition(newStatus state.WorkerStatus) {
	w.Status = newStatus
	if err := w.db.UpdateWorkerStatus(w.ID, newStatus); err != nil {
		log.Printf("Warning: failed to update worker %s status to %s: %v", w.ID, newStatus, err)
	}
}

// fail is a shortcut for transitioning to failed and logging the reason.
func (w *Worker) fail(reason string) {
	w.transition(state.WorkerFailed)
	_ = w.db.LogEvent(state.EventSmithFailed, reason, w.Bead.ID, w.AnvilName)
	log.Printf("[%s] Failed: %s", w.ID, reason)
}

// ActiveCount returns the number of active workers for an anvil from the state DB.
func ActiveCount(db *state.DB, anvilName string) (int, error) {
	workers, err := db.WorkersByAnvil(anvilName)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, w := range workers {
		if w.Status == state.WorkerPending || w.Status == state.WorkerRunning || w.Status == state.WorkerReviewing {
			count++
		}
	}
	return count, nil
}

// CanSpawn checks if a new worker can be spawned for the given anvil,
// respecting the max_smiths limit.
func CanSpawn(db *state.DB, anvilName string, maxSmiths int) (bool, error) {
	active, err := ActiveCount(db, anvilName)
	if err != nil {
		return false, err
	}
	return active < maxSmiths, nil
}

// TotalActiveCount returns the total number of active workers across all anvils.
func TotalActiveCount(db *state.DB) (int, error) {
	workers, err := db.ActiveWorkers()
	if err != nil {
		return 0, err
	}
	return len(workers), nil
}

// CanSpawnGlobal checks if a new worker can be spawned globally,
// respecting the max_total_smiths limit.
func CanSpawnGlobal(db *state.DB, maxTotal int) (bool, error) {
	total, err := TotalActiveCount(db)
	if err != nil {
		return false, err
	}
	return total < maxTotal, nil
}

// cleanWorkerID sanitizes the worker ID for use in log messages.
func cleanWorkerID(id string) string {
	if len(id) > 50 {
		return id[:50]
	}
	return id
}

// logDir returns the log directory path for a worker.
func logDir(anvilPath string) string {
	return filepath.Join(anvilPath, ".workers", "logs")
}

// init ensures necessary directories exist. Called internally.
func ensureLogDir(anvilPath string) {
	dir := logDir(anvilPath)
	_ = os.MkdirAll(dir, 0o755)
}
