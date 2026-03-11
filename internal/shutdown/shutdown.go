// Package shutdown manages graceful daemon shutdown and orphan prevention.
//
// On shutdown:
//  1. Stop accepting new work
//  2. Wait for active workers to finish (up to grace period)
//  3. Kill remaining workers
//  4. Clean up worktrees
//  5. Update state.db
//  6. Remove PID file
//
// On startup:
//  1. Detect stale workers from previous crash
//  2. Kill orphaned claude processes
//  3. Clean up abandoned worktrees
//  4. Reset worker states in DB
package shutdown

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/worktree"
)

const (
	// DefaultGracePeriod is how long to wait for workers to finish.
	DefaultGracePeriod = 60 * time.Second

	// KillTimeout is how long to wait after SIGTERM before SIGKILL.
	KillTimeout = 10 * time.Second
)

// Manager handles graceful shutdown and orphan cleanup.
type Manager struct {
	db          *state.DB
	worktrees   *worktree.Manager
	logger      *slog.Logger
	gracePeriod time.Duration
	anvils      map[string]string // anvil name -> directory path

	// isCrucibleActive, when set, is called during orphan recovery to check
	// whether a given bead ID in a given anvil is currently being orchestrated
	// by a Crucible. Crucible parent beads are in_progress without a direct
	// worker row, so orphan recovery must not reset them. The anvil parameter
	// scopes the check correctly when multiple anvils share the same bead ID.
	// This callback is set by the daemon after construction via
	// SetCrucibleActiveCheck.
	isCrucibleActive func(beadID, anvil string) bool
}

// NewManager creates a new shutdown manager.
func NewManager(db *state.DB, wm *worktree.Manager, logger *slog.Logger, anvils map[string]string) *Manager {
	return &Manager{
		db:          db,
		worktrees:   wm,
		logger:      logger,
		gracePeriod: DefaultGracePeriod,
		anvils:      anvils,
	}
}

// SetCrucibleActiveCheck registers a callback that orphan recovery uses to
// determine whether a bead ID in a given anvil has an active Crucible run. If
// the callback returns true the bead is skipped — it is not orphaned, just
// managed by the Crucible rather than a direct worker row. The anvil parameter
// scopes the check so that two anvils with the same bead ID are handled
// independently.
func (m *Manager) SetCrucibleActiveCheck(fn func(beadID, anvil string) bool) {
	m.isCrucibleActive = fn
}

// SetGracePeriod configures the shutdown grace period.
func (m *Manager) SetGracePeriod(d time.Duration) {
	m.gracePeriod = d
}

// GracefulShutdown performs an orderly shutdown of all active workers.
// Returns the number of workers that had to be forcefully killed.
func (m *Manager) GracefulShutdown() int {
	m.logger.Info("beginning graceful shutdown", "grace_period", m.gracePeriod)

	workers, err := m.db.ActiveWorkers()
	if err != nil {
		m.logger.Error("failed to query active workers", "error", err)
		return 0
	}

	if len(workers) == 0 {
		m.logger.Info("no active workers to shut down")
		return 0
	}

	m.logger.Info("waiting for workers to finish", "count", len(workers))

	// Phase 1: Send SIGINT to all workers (graceful)
	for _, w := range workers {
		if w.PID > 0 {
			m.signalProcess(w.PID, syscall.SIGINT)
		}
	}

	// Phase 2: Wait for grace period, checking periodically
	deadline := time.Now().Add(m.gracePeriod)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		<-ticker.C
		active, _ := m.db.ActiveWorkers()
		if len(active) == 0 {
			m.logger.Info("all workers finished gracefully")
			return 0
		}
		m.logger.Debug("waiting for workers", "remaining", len(active),
			"time_left", time.Until(deadline).Round(time.Second))
	}

	// Phase 3: Force-kill remaining workers
	remaining, _ := m.db.ActiveWorkers()
	killed := 0
	for _, w := range remaining {
		if w.PID > 0 {
			m.logger.Warn("force-killing worker", "id", w.ID, "pid", w.PID)
			m.killProcess(w.PID)
			killed++
		}
		_ = m.db.UpdateWorkerStatus(w.ID, state.WorkerFailed)
	}

	m.logger.Info("shutdown complete", "graceful", len(workers)-killed, "killed", killed)
	return killed
}

// CleanupOrphans detects and cleans up orphaned resources from a previous crash.
// Call this on daemon startup.
func (m *Manager) CleanupOrphans() (cleaned int) {
	m.logger.Info("checking for orphaned resources")

	// 1. Find stale workers (status pending/running but process dead)
	workers, err := m.db.ActiveWorkers()
	if err != nil {
		m.logger.Error("failed to query workers for orphan check", "error", err)
		return 0
	}

	for _, w := range workers {
		isDead := false
		if w.PID > 0 && !isProcessAlive(w.PID) {
			m.logger.Warn("found orphaned worker (process dead)", "id", w.ID, "pid", w.PID)
			isDead = true
		} else if w.PID == 0 {
			// Worker with no PID is stale
			m.logger.Warn("found stale worker (no PID)", "id", w.ID)
			isDead = true
		}

		if isDead {
			_ = m.db.UpdateWorkerStatus(w.ID, state.WorkerFailed)
			m.db.LogEvent(state.EventError,
				fmt.Sprintf("Orphaned worker %s cleaned up", w.ID),
				w.BeadID, w.Anvil)

			// Only reset bead to open if there is no open PR for it.
			// Bellows/monitoring workers die on daemon restart, but their
			// PRs are still open and should not be re-dispatched.
			if anvilPath, ok := m.anvils[w.Anvil]; ok {
				hasPR, prErr := m.db.HasOpenPRForBead(w.BeadID, w.Anvil)
				if prErr != nil {
					m.logger.Warn("failed to check open PR for orphaned worker", "bead", w.BeadID, "error", prErr)
				} else if hasPR {
					m.logger.Info("orphaned worker has open PR, keeping bead in_progress", "bead", w.BeadID, "anvil", w.Anvil)
				} else {
					if err := m.resetBead(w.BeadID, anvilPath); err != nil {
						m.logger.Warn("failed to reset bead status", "bead", w.BeadID, "error", err)
					} else {
						m.logger.Info("reset bead status to open", "bead", w.BeadID, "anvil", w.Anvil)
					}
				}
			}

			cleaned++
		}
	}

	// 2. Kill any orphaned claude processes
	orphanedPIDs := m.findOrphanedClaude()
	for _, pid := range orphanedPIDs {
		m.logger.Warn("killing orphaned claude process", "pid", pid)
		m.killProcess(pid)
		cleaned++
	}

	// 3. Clean up abandoned worktrees across all anvils
	if m.worktrees != nil {
		// Refresh active workers list to ensure we don't skip worktrees of workers we just marked as failed
		if refreshed, err := m.db.ActiveWorkers(); err != nil {
			m.logger.Error("failed to refresh active workers for worktree cleanup", "error", err)
			// Keep existing workers slice — do not overwrite with nil on error
		} else {
			workers = refreshed
		}

		ctx := context.Background()
		for name, anvilPath := range m.anvils {
			wts, err := m.worktrees.List(anvilPath)
			if err != nil {
				continue
			}
			for _, wtPath := range wts {
				// Check if any active worker references this worktree
				used := false
				for _, w := range workers {
					// Precisely match worktree directory name with the worker's branch (minus forge/ prefix)
					if w.Anvil == name && w.Branch != "" {
						dirName := strings.TrimPrefix(w.Branch, "forge/")
						if filepath.Base(wtPath) == dirName {
							used = true
							break
						}
					}
				}
				if !used {
					m.logger.Warn("cleaning abandoned worktree", "path", wtPath, "anvil", anvilPath)
					// Extract bead ID from path for Worktree struct
					beadID := filepath.Base(wtPath)
					_ = m.worktrees.Remove(ctx, anvilPath, &worktree.Worktree{
						Path:   wtPath,
						Branch: "forge/" + beadID,
					})
					cleaned++
				}
			}
		}
	}

	if cleaned > 0 {
		m.logger.Info("orphan cleanup complete", "cleaned", cleaned)
		m.db.LogEvent(state.EventOrphanCleanup,
			fmt.Sprintf("Cleaned up %d orphaned resources on startup", cleaned),
			"", "")
	} else {
		m.logger.Info("no orphaned resources found")
	}

	return cleaned
}

// orphanMinAge is the minimum time a bead must have been in_progress before it
// is considered orphaned. This prevents recovery from racing with the normal
// dispatch path, where a bead is marked in_progress in bd before the worker
// row is inserted into state.db.
const orphanMinAge = 5 * time.Minute

// RecoverOrphanedBeads detects beads with status=in_progress in the beads DB
// that have no active worker and no open PR in Forge's state DB. These are
// beads that were claimed but orphaned (e.g., daemon crashed mid-session).
// Only beads belonging to this Forge's configured anvils are considered, and
// only beads that Forge has previously claimed (i.e., have a worker record in
// state.db) are eligible for recovery — beads set to in_progress by humans or
// external tools are left untouched.
// This runs both at startup and periodically during normal operation (every
// 10 poll cycles) so recovery is not limited to crash scenarios.
// Returns the number of beads recovered.
func (m *Manager) RecoverOrphanedBeads() (recovered int) {
	m.logger.Info("checking for orphaned in-progress beads")

	for anvilName, anvilPath := range m.anvils {
		beads, err := m.listInProgressBeads(anvilPath)
		if err != nil {
			m.logger.Warn("failed to list in-progress beads", "anvil", anvilName, "error", err)
			continue
		}

		for _, bead := range beads {
			beadID := bead.ID

			// Only recover beads that Forge has previously claimed. Beads can
			// be in_progress because a human or another tool (e.g. Copilot) is
			// working on them — we must not reset those.
			hasRecord, err := m.db.HasWorkerRecord(beadID, anvilName)
			if err != nil {
				m.logger.Warn("failed to check worker record", "bead", beadID, "error", err)
				continue
			}
			if !hasRecord {
				// No worker record means Forge never claimed this bead. It was
				// set to in_progress by a human or an external tool (e.g.
				// Copilot). Never reset these — we must not touch beads that
				// Forge didn't claim, regardless of how long they've been
				// in_progress.
				m.logger.Debug("skipping bead without worker record (not claimed by Forge)", "bead", beadID, "anvil", anvilName)
				continue
			}

			// Skip beads that are currently being orchestrated by the Crucible.
			// Crucible parent beads are in_progress for the duration of the
			// entire feature-branch orchestration, but they may not always have
			// an active worker row in state.db (e.g. if the daemon was briefly
			// interrupted and the pending worker was cleaned up on startup while
			// the Crucible goroutine is still running in-process). The
			// crucibleStatuses map is the authoritative in-memory source for
			// this — if the Crucible is live, the bead is not orphaned.
			if m.isCrucibleActive != nil && m.isCrucibleActive(beadID, anvilName) {
				m.logger.Debug("skipping bead with active crucible", "bead", beadID, "anvil", anvilName)
				continue
			}

			// Skip beads that were recently claimed: the pending worker row is
			// inserted at claim time, but a brand-new claim may not yet have
			// aged enough to be considered orphaned. Only recover beads that
			// have been in_progress longer than orphanMinAge.
			if !bead.UpdatedAt.IsZero() && time.Since(bead.UpdatedAt) < orphanMinAge {
				m.logger.Debug("skipping recently-claimed bead", "bead", beadID, "age", time.Since(bead.UpdatedAt).Round(time.Second))
				continue
			}

			// Check if there's an active worker for this bead in this anvil.
			// Using the anvil-scoped query prevents a worker in a different anvil
			// (with the same bead ID) from masking an orphan here.
			activeWorker, err := m.db.ActiveWorkerByBeadAndAnvil(beadID, anvilName)
			if err != nil {
				m.logger.Warn("failed to check active worker", "bead", beadID, "error", err)
				continue
			}
			if activeWorker != nil {
				continue // has an active worker, not orphaned
			}

			// Check if there's an open PR for this bead
			hasPR, err := m.db.HasOpenPRForBead(beadID, anvilName)
			if err != nil {
				m.logger.Warn("failed to check open PR", "bead", beadID, "error", err)
				continue
			}
			if hasPR {
				continue // has an open PR, not orphaned
			}

			// This bead was claimed by Forge but has no active worker or PR — it's orphaned
			m.logger.Warn("found orphaned in-progress bead", "bead", beadID, "anvil", anvilName)
			if err := m.resetBead(beadID, anvilPath); err != nil {
				m.logger.Warn("failed to reset orphaned bead", "bead", beadID, "error", err)
				continue
			}
			m.logger.Info("recovered orphaned bead to open", "bead", beadID, "anvil", anvilName)
			m.db.LogEvent(state.EventBeadRecovered,
				fmt.Sprintf("Orphaned in-progress bead %s recovered to open", beadID),
				beadID, anvilName)
			recovered++
		}
	}

	if recovered > 0 {
		m.logger.Info("orphaned bead recovery complete", "recovered", recovered)
	} else {
		m.logger.Info("no orphaned in-progress beads found")
	}

	return recovered
}

// inProgressBead holds the id and last-update time of an in-progress bead.
type inProgressBead struct {
	ID        string
	UpdatedAt time.Time
}

// listInProgressBeads returns in-progress beads for an anvil, including their
// last-updated timestamps so callers can filter by age.
func (m *Manager) listInProgressBeads(anvilPath string) ([]inProgressBead, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(ctx, "bd", "list", "--status=in_progress", "--json"))
	cmd.Dir = anvilPath
	// Capture stderr separately so that any warnings/progress lines written to
	// stderr by bd do not corrupt the JSON we parse from stdout.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bd list --status=in_progress --json: %w\n%s", err, stderr.String())
	}

	// Parse JSON array of beads — we need "id" and "updated_at".
	var raw []struct {
		ID        string `json:"id"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return nil, fmt.Errorf("parsing bd list output: %w", err)
	}

	beads := make([]inProgressBead, len(raw))
	for i, b := range raw {
		beads[i].ID = b.ID
		// Parse RFC3339 timestamp; zero value if missing/unparseable (treated as old).
		if t, err := time.Parse(time.RFC3339, b.UpdatedAt); err == nil {
			beads[i].UpdatedAt = t
		}
	}
	return beads, nil
}

// CleanupWorktrees removes all worktrees across all anvils (for full shutdown).
func (m *Manager) CleanupWorktrees() {
	if m.worktrees == nil {
		return
	}
	ctx := context.Background()
	for _, anvilPath := range m.anvils {
		wts, err := m.worktrees.List(anvilPath)
		if err != nil {
			continue
		}
		for _, wtPath := range wts {
			m.logger.Debug("removing worktree", "path", wtPath, "anvil", anvilPath)
			beadID := filepath.Base(wtPath)
			_ = m.worktrees.Remove(ctx, anvilPath, &worktree.Worktree{
				Path:   wtPath,
				Branch: "forge/" + beadID,
			})
		}
	}
}

// resetBead marks a bead as open via bd update.
func (m *Manager) resetBead(beadID, anvilPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(ctx, "bd", "update", beadID, "--status=open", "--json"))
	cmd.Dir = anvilPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bd update %s --status=open --json: %w\n%s", beadID, err, out)
	}
	return nil
}

// signalProcess sends a signal to a process.
func (m *Manager) signalProcess(pid int, sig syscall.Signal) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(sig)
}

// killProcess forcefully terminates a process.
func (m *Manager) killProcess(pid int) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Kill()
}

// isProcessAlive checks if a process with the given PID exists and is running.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 tests process existence without side effects
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// findOrphanedClaude finds claude processes not tracked by any active worker.
// This is platform-specific and best-effort.
func (m *Manager) findOrphanedClaude() []int {
	workers, _ := m.db.ActiveWorkers()
	trackedPIDs := make(map[int]bool)
	for _, w := range workers {
		if w.PID > 0 {
			trackedPIDs[w.PID] = true
		}
	}

	// Platform-specific process listing
	return findClaudeProcesses(trackedPIDs)
}

// findClaudeProcesses finds claude process PIDs not in the tracked set.
func findClaudeProcesses(tracked map[int]bool) []int {
	var orphans []int

	if runtime.GOOS == "windows" {
		orphans = findClaudeProcessesWindows(tracked)
	} else {
		orphans = findClaudeProcessesUnix(tracked)
	}

	return orphans
}

// findClaudeProcessesWindows uses tasklist to find claude.exe processes.
func findClaudeProcessesWindows(tracked map[int]bool) []int {
	// Use os.exec would be better but we avoid import overhead
	// For now, check tracked PIDs only — full enumeration deferred
	return nil
}

// findClaudeProcessesUnix uses /proc or ps to find claude processes.
func findClaudeProcessesUnix(tracked map[int]bool) []int {
	// Read /proc directory for claude processes
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}

	var orphans []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if tracked[pid] {
			continue
		}

		// Check if this is a claude process
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		if strings.Contains(string(cmdline), "claude") {
			orphans = append(orphans, pid)
		}
	}

	return orphans
}
