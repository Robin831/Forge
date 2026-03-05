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

			// Reset bead status to open in bd
			if anvilPath, ok := m.anvils[w.Anvil]; ok {
				if err := m.resetBead(w.BeadID, anvilPath); err != nil {
					m.logger.Warn("failed to reset bead status", "bead", w.BeadID, "error", err)
				} else {
					m.logger.Info("reset bead status to open", "bead", w.BeadID, "anvil", w.Anvil)
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
		workers, err = m.db.ActiveWorkers()
		if err != nil {
			m.logger.Error("failed to refresh active workers for worktree cleanup", "error", err)
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
					_ = m.worktrees.Remove(ctx, anvilPath, &worktree.Worktree{
						Path:   wtPath,
						Branch: filepath.Base(wtPath),
					})
					cleaned++
				}
			}
		}
	}

	if cleaned > 0 {
		m.logger.Info("orphan cleanup complete", "cleaned", cleaned)
		m.db.LogEvent("orphan_cleanup",
			fmt.Sprintf("Cleaned up %d orphaned resources on startup", cleaned),
			"", "")
	} else {
		m.logger.Info("no orphaned resources found")
	}

	return cleaned
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
			_ = m.worktrees.Remove(ctx, anvilPath, &worktree.Worktree{
				Path:   wtPath,
				Branch: filepath.Base(wtPath),
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
