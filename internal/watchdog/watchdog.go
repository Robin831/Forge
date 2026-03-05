// Package watchdog provides timeout enforcement for Smith worker processes.
//
// The Watchdog monitors running workers and kills any that exceed the
// configured smith_timeout. It uses graceful shutdown (context cancellation)
// first, then forceful kill after a grace period.
package watchdog

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// TrackedProcess represents a running process being monitored.
type TrackedProcess struct {
	// WorkerID is the worker's unique identifier.
	WorkerID string
	// BeadID is the bead being worked on.
	BeadID string
	// Anvil is the anvil name.
	Anvil string
	// StartedAt is when the process was started.
	StartedAt time.Time
	// Cancel cancels the process context (graceful shutdown).
	Cancel context.CancelFunc
	// Kill forcefully terminates the process.
	Kill func() error
	// Done returns a channel that closes when the process finishes.
	Done func() <-chan struct{}
}

// Watchdog monitors running processes and enforces timeouts.
type Watchdog struct {
	// Timeout is the maximum duration a Smith process can run.
	Timeout time.Duration
	// GracePeriod is how long to wait after context cancellation before killing.
	GracePeriod time.Duration

	db        *state.DB
	mu        sync.Mutex
	tracked   map[string]*TrackedProcess
	checkFreq time.Duration
}

// New creates a new Watchdog with the given timeout.
func New(db *state.DB, timeout time.Duration) *Watchdog {
	grace := 30 * time.Second
	if timeout < 2*time.Minute {
		grace = 10 * time.Second
	}

	freq := 30 * time.Second
	if timeout < 5*time.Minute {
		freq = 10 * time.Second
	}

	return &Watchdog{
		Timeout:     timeout,
		GracePeriod: grace,
		db:          db,
		tracked:     make(map[string]*TrackedProcess),
		checkFreq:   freq,
	}
}

// Track adds a process to the watchdog's monitoring list.
func (w *Watchdog) Track(p *TrackedProcess) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tracked[p.WorkerID] = p
	log.Printf("[watchdog] Tracking worker %s (bead %s, timeout %s)", p.WorkerID, p.BeadID, w.Timeout)
}

// Untrack removes a process from monitoring (e.g., completed normally).
func (w *Watchdog) Untrack(workerID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.tracked, workerID)
}

// Run starts the watchdog loop. It checks all tracked processes periodically
// and kills any that have exceeded the timeout. Blocks until ctx is cancelled.
func (w *Watchdog) Run(ctx context.Context) {
	ticker := time.NewTicker(w.checkFreq)
	defer ticker.Stop()

	log.Printf("[watchdog] Started (timeout=%s, grace=%s, check=%s)", w.Timeout, w.GracePeriod, w.checkFreq)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[watchdog] Shutting down")
			w.killAll()
			return
		case <-ticker.C:
			w.check()
		}
	}
}

// check inspects all tracked processes and kills timed-out ones.
func (w *Watchdog) check() {
	w.mu.Lock()
	// Copy tracked map to avoid holding lock during kill operations
	toCheck := make([]*TrackedProcess, 0, len(w.tracked))
	for _, p := range w.tracked {
		toCheck = append(toCheck, p)
	}
	w.mu.Unlock()

	now := time.Now()
	for _, p := range toCheck {
		// Check if already done
		select {
		case <-p.Done():
			w.Untrack(p.WorkerID)
			continue
		default:
		}

		elapsed := now.Sub(p.StartedAt)
		if elapsed > w.Timeout {
			log.Printf("[watchdog] Worker %s exceeded timeout (%.1f min > %.1f min), killing",
				p.WorkerID, elapsed.Minutes(), w.Timeout.Minutes())
			w.killProcess(p)
		}
	}
}

// killProcess performs graceful then forceful shutdown of a single process.
func (w *Watchdog) killProcess(p *TrackedProcess) {
	// Log the timeout event
	_ = w.db.LogEvent(state.EventError,
		fmt.Sprintf("Worker %s timed out after %s (bead %s)",
			p.WorkerID, w.Timeout, p.BeadID),
		p.BeadID, p.Anvil)

	// Update worker status to timeout
	_ = w.db.UpdateWorkerStatus(p.WorkerID, state.WorkerTimeout)

	// Step 1: Cancel context (graceful shutdown)
	p.Cancel()

	// Step 2: Wait for grace period
	select {
	case <-p.Done():
		log.Printf("[watchdog] Worker %s stopped gracefully", p.WorkerID)
		w.Untrack(p.WorkerID)
		return
	case <-time.After(w.GracePeriod):
		// Grace period expired, force kill
	}

	// Step 3: Force kill
	log.Printf("[watchdog] Worker %s did not stop gracefully, force killing", p.WorkerID)
	if err := p.Kill(); err != nil {
		log.Printf("[watchdog] Error killing worker %s: %v", p.WorkerID, err)
	}

	// Wait a bit more for cleanup
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		log.Printf("[watchdog] Worker %s still alive after force kill", p.WorkerID)
	}

	w.Untrack(p.WorkerID)
}

// killAll kills all tracked processes (used during shutdown).
func (w *Watchdog) killAll() {
	w.mu.Lock()
	processes := make([]*TrackedProcess, 0, len(w.tracked))
	for _, p := range w.tracked {
		processes = append(processes, p)
	}
	w.mu.Unlock()

	if len(processes) == 0 {
		return
	}

	log.Printf("[watchdog] Killing %d remaining workers", len(processes))

	// Cancel all contexts first (graceful)
	for _, p := range processes {
		p.Cancel()
	}

	// Wait for grace period
	time.Sleep(w.GracePeriod)

	// Force kill any remaining
	for _, p := range processes {
		select {
		case <-p.Done():
			continue
		default:
			_ = p.Kill()
		}
	}
}

// TrackedCount returns the number of currently tracked processes.
func (w *Watchdog) TrackedCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.tracked)
}
