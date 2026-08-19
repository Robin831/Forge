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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

	// OnOrphanFound, when set, is called when an orphaned bead is detected
	// before auto-recovery. If it returns true, the bead has been handled
	// externally (e.g. deferred to the Hearth dialog) and should NOT be
	// auto-recovered. If nil or returns false, auto-recovery proceeds as
	// before. Set by the daemon after construction.
	OnOrphanFound func(beadID, anvil, title, branch string) bool

	// OnNeedsHuman, when set, is called when a bead is flagged as
	// needs-human due to repeated recovery failures. The daemon uses this
	// to fire webhook notifications. Parameters: beadID, anvil, title
	// (may be empty if unknown), failure count, and the underlying error.
	OnNeedsHuman func(beadID, anvil, title string, failures int, reason string)
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
						if !errors.Is(err, context.DeadlineExceeded) {
							failures, tripped, dbErr := m.db.IncrementRecoveryFailures(w.BeadID, w.Anvil, err.Error())
							if dbErr != nil {
								m.logger.Warn("failed to track recovery failure", "bead", w.BeadID, "error", dbErr)
							} else if tripped {
								m.logger.Warn("orphan worker recovery flagged as needs-human after repeated failures", "bead", w.BeadID, "anvil", w.Anvil, "failures", failures)
								m.db.LogEvent(state.EventRecoveryCircuitBreak,
									fmt.Sprintf("Orphaned worker bead %s flagged needs-human after %d recovery failures: %s", w.BeadID, failures, err.Error()),
									w.BeadID, w.Anvil)
								if m.OnNeedsHuman != nil {
									m.OnNeedsHuman(w.BeadID, w.Anvil, w.Title, failures, err.Error())
								}
							}
						}
					} else {
						_ = m.db.ResetRecoveryFailures(w.BeadID, w.Anvil)
						m.logger.Info("reset bead status to open", "bead", w.BeadID, "anvil", w.Anvil)
					}
				}
			}

			cleaned++
		}
	}

	// 2. Kill any orphaned Forge worker processes. Ownership is verified from
	// the workers table plus, on Unix, the process working directory; on Windows
	// (where a process cwd is unavailable) it rests on the PID + creation-time
	// match alone, with the Job Object assigned at spawn as the primary
	// containment layer. A process is never killed on the basis of a cmdline
	// substring, so the operator's own Claude Code session (and unrelated tools
	// whose argv merely mentions "claude") are left untouched. The `workers`
	// snapshot taken above carries the recorded PIDs and start times used for
	// verification.
	for _, op := range m.findOrphanedClaude(workers) {
		m.logger.Warn("killing orphaned worker process", "pid", op.pid, "evidence", op.evidence)
		m.killProcess(op.pid)
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

		// Union in paused (parked) workers: a bead parked by an operator pause
		// still holds its worktree and will respawn a running smith on resume, so
		// its worktree must NOT be treated as abandoned. ActiveWorkers excludes
		// paused workers, so they are fetched and appended explicitly here.
		if paused, err := m.db.PausedWorkers(); err != nil {
			m.logger.Error("failed to query paused workers for worktree cleanup", "error", err)
		} else if len(paused) > 0 {
			workers = append(workers, paused...)
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
		beads, err := m.listInProgressBeads(anvilName, anvilPath)
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

			// Skip beads that are parked for human attention. Beads with
			// needs_human=1 or clarification_needed=1 are intentionally
			// waiting — they don't have an open PR by design and should only
			// be re-dispatched via explicit user action (Retry, Force Smith,
			// etc.).
			if retryRec, err := m.db.GetRetry(beadID, anvilName); err != nil {
				m.logger.Warn("failed to query retry state for bead; skipping to avoid resetting a parked bead", "bead", beadID, "anvil", anvilName, "err", err)
				continue
			} else if retryRec != nil && (retryRec.NeedsHuman || retryRec.ClarificationNeeded) {
				m.logger.Debug("skipping bead parked for human attention", "bead", beadID, "anvil", anvilName, "needs_human", retryRec.NeedsHuman, "clarification_needed", retryRec.ClarificationNeeded)
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

			// Skip beads parked by an operator pause. A paused worker row and its
			// worktree survive a daemon restart even though the parked pipeline
			// goroutine does not; the bead is intentionally waiting for the operator
			// to resume or discard it (surfaced in Needs Attention). ActiveWorkers /
			// ActiveWorkerByBeadAndAnvil exclude the paused status, so without this
			// explicit guard a long-paused bead would look orphaned and be reset to
			// open, destroying the retained resume state.
			if pausedWorker, err := m.db.PausedWorkerByBead(beadID, anvilName); err != nil {
				m.logger.Warn("failed to query paused worker for bead; skipping to avoid resetting a parked bead", "bead", beadID, "anvil", anvilName, "err", err)
				continue
			} else if pausedWorker != nil {
				m.logger.Debug("skipping bead parked by operator pause", "bead", beadID, "anvil", anvilName)
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

			// This bead was claimed by Forge but has no active worker or PR — it's orphaned.
			m.logger.Warn("found orphaned in-progress bead", "bead", beadID, "anvil", anvilName)

			// If a callback is registered (e.g., Hearth is connected), give it the
			// chance to defer recovery to the user dialog instead of auto-recovering.
			if m.OnOrphanFound != nil && m.OnOrphanFound(beadID, anvilName, bead.Title, bead.Branch) {
				m.logger.Info("orphaned bead deferred to Hearth dialog", "bead", beadID, "anvil", anvilName)
				// Do not increment recovered — the bead has not been reset yet.
				continue
			}

			// Fall through to auto-recovery (headless/CI mode or no Hearth client).
			if err := m.resetBead(beadID, anvilPath); err != nil {
				m.logger.Warn("failed to reset orphaned bead", "bead", beadID, "error", err)
				if !errors.Is(err, context.DeadlineExceeded) {
					failures, tripped, dbErr := m.db.IncrementRecoveryFailures(beadID, anvilName, err.Error())
					if dbErr != nil {
						m.logger.Warn("failed to track recovery failure", "bead", beadID, "error", dbErr)
					} else if tripped {
						m.logger.Warn("orphan recovery flagged as needs-human after repeated failures", "bead", beadID, "anvil", anvilName, "failures", failures)
						m.db.LogEvent(state.EventRecoveryCircuitBreak,
							fmt.Sprintf("Orphaned bead %s flagged needs-human after %d recovery failures: %s", beadID, failures, err.Error()),
							beadID, anvilName)
						if m.OnNeedsHuman != nil {
							m.OnNeedsHuman(beadID, anvilName, bead.Title, failures, err.Error())
						}
					}
				}
				continue
			}
			_ = m.db.ResetRecoveryFailures(beadID, anvilName)
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

// inProgressBead holds the id, title, branch, and last-update time of an in-progress bead.
type inProgressBead struct {
	ID        string
	Title     string
	Branch    string
	UpdatedAt time.Time
}

// listInProgressBeads returns in-progress beads for an anvil, including their
// last-updated timestamps and most-recent worker branch so callers can filter
// by age and display the branch in dialogs.
func (m *Manager) listInProgressBeads(anvilName, anvilPath string) ([]inProgressBead, error) {
	cmd, cancel := executil.BdCommand(context.Background(), "list", "--status=in_progress", "--json")
	defer cancel()
	cmd.Dir = anvilPath
	// Capture stderr separately so that any warnings/progress lines written to
	// stderr by bd do not corrupt the JSON we parse from stdout.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("bd list --status=in_progress --json: %w\n%s", err, stderr.String())
	}

	// Parse JSON array of beads — we need "id", "title", and "updated_at".
	var raw []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return nil, fmt.Errorf("parsing bd list output: %w", err)
	}

	beads := make([]inProgressBead, len(raw))
	for i, b := range raw {
		beads[i].ID = b.ID
		beads[i].Title = b.Title
		// Parse RFC3339 timestamp; zero value if missing/unparseable (treated as old).
		if t, err := time.Parse(time.RFC3339, b.UpdatedAt); err == nil {
			beads[i].UpdatedAt = t
		}
		// Best-effort: populate branch from the most recent worker record in
		// state.db so the orphan dialog can show which branch was in use.
		if branch, err := m.db.LastWorkerBranchForBead(b.ID, anvilName); err == nil {
			beads[i].Branch = branch
		}
	}
	return beads, nil
}

// ResetBead marks a bead as open via bd update and clears the assignee.
// This is exported so the daemon's IPC handler can reset a bead when the user
// chooses the "Recover" action in the orphan dialog.
func (m *Manager) ResetBead(beadID, anvilPath string) error {
	return m.resetBead(beadID, anvilPath)
}

// CleanupWorktrees removes worktrees across all anvils (for full shutdown).
//
// Worktrees belonging to paused (parked) workers are RETAINED: a bead parked by
// an operator pause is treated like a drained pipeline on shutdown — its state
// (worker row + worktree) is persisted and the daemon exits, so the bead can be
// resumed or discarded after restart. Removing its worktree here would strip the
// in-progress work the resume relies on. ActiveWorkers excludes paused workers,
// so PausedWorkers is queried explicitly to build the retention set.
func (m *Manager) CleanupWorktrees() {
	if m.worktrees == nil {
		return
	}

	// Build the set of worktree directory names to retain (one per paused
	// worker). A worktree dir is named after the bead branch minus the forge/
	// prefix (see the matching logic in CleanupOrphans).
	retain := make(map[string]struct{})
	if paused, err := m.db.PausedWorkers(); err != nil {
		m.logger.Error("failed to query paused workers for shutdown worktree cleanup; not retaining any", "error", err)
	} else {
		for _, w := range paused {
			if w.Branch == "" {
				continue
			}
			retain[strings.TrimPrefix(w.Branch, "forge/")] = struct{}{}
		}
	}

	ctx := context.Background()
	for _, anvilPath := range m.anvils {
		wts, err := m.worktrees.List(anvilPath)
		if err != nil {
			continue
		}
		for _, wtPath := range wts {
			if _, keep := retain[filepath.Base(wtPath)]; keep {
				m.logger.Info("retaining paused worker worktree across shutdown", "path", wtPath, "anvil", anvilPath)
				continue
			}
			m.logger.Debug("removing worktree", "path", wtPath, "anvil", anvilPath)
			beadID := filepath.Base(wtPath)
			_ = m.worktrees.Remove(ctx, anvilPath, &worktree.Worktree{
				Path:   wtPath,
				Branch: "forge/" + beadID,
			})
		}
	}
}

// resetBead marks a bead as open via bd update and clears the assignee.
// Clearing the assignee is required so the poller can re-dispatch the bead —
// the poller filters out any bead with a non-empty assignee.
func (m *Manager) resetBead(beadID, anvilPath string) error {
	// Clear labels so the poller does not auto-dispatch the recovered bead
	// before a human reviews it. Without this, orphaned beads with the
	// auto_dispatch_tag would immediately be re-claimed on the next poll,
	// creating a zombie loop.
	var stderrBuf bytes.Buffer
	cmd, cancel := executil.BdCommand(context.Background(), "update", beadID, "--status=open", "--assignee=", "--remove-label=forgeReady", "--json")
	defer cancel()
	cmd.Dir = anvilPath
	cmd.Stderr = &stderrBuf
	out, err := cmd.Output()
	if err != nil {
		// A deadline kill already names the command and the time it got, so
		// pass it through rather than restating it as a bare timeout.
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("bd update %s: %w", beadID, err)
		}

		stdoutText := strings.TrimSpace(string(out))
		stderrText := strings.TrimSpace(stderrBuf.String())

		var details []string
		if stdoutText != "" {
			details = append(details, "stdout:\n"+stdoutText)
		}
		if stderrText != "" {
			details = append(details, "stderr:\n"+stderrText)
		}
		if len(details) > 0 {
			return fmt.Errorf(
				"bd update %s --status=open --assignee= --remove-label=forgeReady --json: %w\n%s",
				beadID,
				err,
				strings.Join(details, "\n"),
			)
		}
		return fmt.Errorf("bd update %s --status=open --assignee= --remove-label=forgeReady --json: %w", beadID, err)
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

// ownedProc is a process positively identified as Forge-owned, together with
// the evidence used to establish ownership. Only processes with an ownedProc
// entry are ever signalled by the orphan sweep — name/cmdline matching alone is
// never sufficient.
type ownedProc struct {
	pid      int
	evidence string
}

// procInfo is a snapshot of one running process used for ownership
// verification. Identity is established from the recorded worker PID, the
// process start time, and (where the platform exposes it) the working
// directory — never from a cmdline substring.
type procInfo struct {
	pid       int
	pgid      int      // process group id (equals pid for a group leader); 0 when unavailable (Windows)
	argv      []string // argv[0..n] from /proc/<pid>/cmdline (Unix) or the exe basename (Windows)
	startTime time.Time
	cwd       string // resolved target of /proc/<pid>/cwd (Unix); empty on Windows
}

const (
	// startTimeSkew tolerates small clock/rounding differences between a worker
	// row's recorded started_at and the process start time derived from /proc
	// (which has ~1s resolution).
	startTimeSkew = 90 * time.Second

	// maxSpawnDelay bounds how long after a worker row is created its process
	// may legitimately start — schematic analysis and worktree setup run in
	// between claim (started_at) and the actual claude spawn. A process whose
	// start time is beyond this window relative to the worker row is treated as
	// a recycled PID and is NOT considered Forge-owned.
	maxSpawnDelay = 30 * time.Minute
)

// forgeWorkerBasenames is the exact set of argv[0] basenames Forge spawns as
// worker processes. The secondary sweep matches these EXACT basenames only —
// never a cmdline substring — and even then only after the cwd-in-worktree
// check passes.
var forgeWorkerBasenames = map[string]bool{
	"claude":      true,
	"claude.exe":  true,
	"gemini":      true,
	"gemini.exe":  true,
	"copilot":     true,
	"copilot.exe": true,
}

// listProcesses enumerates running processes for ownership verification. It is
// a package-level var so tests can inject a fake process table. listProcessesPlatform
// is build-tagged: it walks /proc on Unix (procInfo with cwd) and uses a
// toolhelp snapshot on Windows (procInfo with creation time, no cwd).
var listProcesses = listProcessesPlatform

// findOrphanedClaude returns processes positively identified as Forge-owned
// worker processes left behind by a previous daemon — safe to reap on startup.
// Identity is established from the workers table and the process working
// directory; a process is NEVER selected on the basis of a cmdline substring.
// The workers slice is the snapshot of worker rows from state.db (passed in so
// the caller controls the snapshot used for verification).
func (m *Manager) findOrphanedClaude(workers []state.Worker) []ownedProc {
	procs, err := listProcesses()
	if err != nil {
		m.logger.Warn("failed to list processes for orphan sweep; skipping", "error", err)
		return nil
	}
	return identifyForgeOwnedProcesses(workers, procs, m.worktreeRoots(), platformSupportsProcessCwd, m.logger)
}

// worktreeRoots returns the absolute .workers directories for every configured
// anvil. A process is only ever reaped when its working directory (or its
// process-group leader's) resolves under one of these roots.
func (m *Manager) worktreeRoots() []string {
	workersDir := ".workers"
	if m.worktrees != nil && m.worktrees.WorkersDir != "" {
		workersDir = m.worktrees.WorkersDir
	}
	roots := make([]string, 0, len(m.anvils))
	for _, anvilPath := range m.anvils {
		if anvilPath == "" {
			continue
		}
		roots = append(roots, filepath.Clean(filepath.Join(anvilPath, workersDir)))
	}
	return roots
}

// identifyForgeOwnedProcesses returns the subset of procs that are positively
// identified as Forge-owned worker processes, with per-process evidence.
//
// Two sources of positive evidence are used:
//
//  1. Primary — the workers table: a live process whose PID matches a recorded
//     worker and whose start time is consistent with the worker row (guards PID
//     recycling). On platforms that expose a process working directory
//     (requireWorktreeCwd=true, i.e. Unix), the process (or its process-group
//     leader) must additionally be running inside a Forge worktree.
//  2. Secondary — lost workers: a process whose PID was never recorded but whose
//     argv[0] basename is an EXACT match for a Forge worker binary AND which is
//     running inside a worktree. This sweep relies entirely on the cwd check for
//     safety, so it only runs when requireWorktreeCwd is true.
//
// requireWorktreeCwd is true on platforms where /proc-style per-process working
// directories are available (Unix). On Windows a process cwd cannot be resolved
// cheaply, so it is false: ownership rests on the strong PID + creation-time
// match against a recorded worker row (Windows worker containment is handled
// primarily by the Job Object assigned at spawn — see executil.ContainProcess),
// and the unrecorded-PID secondary sweep is skipped to avoid ever killing the
// operator's own Claude Code session.
//
// A process is never matched on a cmdline substring, so the operator's own
// Claude Code session and unrelated tools whose argv merely contains ".claude"
// are always spared.
func identifyForgeOwnedProcesses(workers []state.Worker, procs []procInfo, worktreeRoots []string, requireWorktreeCwd bool, logger *slog.Logger) []ownedProc {
	byPID := make(map[int]procInfo, len(procs))
	for _, p := range procs {
		byPID[p.pid] = p
	}

	// cwdEvidence reports the worktree root a process (or its process-group
	// leader) is running inside, if any. The pgid-leader fallback catches child
	// processes (e.g. claude's agent/bg-pty-host helpers) that inherited the
	// group but may have changed directory.
	cwdEvidence := func(p procInfo) (string, bool) {
		if root, ok := pathUnderAny(p.cwd, worktreeRoots); ok {
			return "cwd-in-worktree=" + root, true
		}
		if p.pgid > 0 && p.pgid != p.pid {
			if leader, ok := byPID[p.pgid]; ok {
				if root, ok := pathUnderAny(leader.cwd, worktreeRoots); ok {
					return "pgid-leader-cwd-in-worktree=" + root, true
				}
			}
		}
		return "", false
	}

	seen := make(map[int]bool)
	var owned []ownedProc

	// Primary source: the workers table.
	for _, w := range workers {
		if w.PID <= 0 {
			continue
		}
		p, ok := byPID[w.PID]
		if !ok {
			continue // recorded PID is not live — nothing to reap
		}
		if seen[p.pid] {
			continue
		}
		if !startTimeConsistent(p.startTime, w.StartedAt) {
			logger.Warn("skipping PID matching a worker row but with inconsistent start time (likely recycled PID)",
				"pid", w.PID, "worker", w.ID, "bead", w.BeadID,
				"proc_start", p.startTime, "worker_started_at", w.StartedAt)
			continue
		}
		evidence := fmt.Sprintf("worker=%s start-time-ok", w.ID)
		if requireWorktreeCwd {
			cwd, in := cwdEvidence(p)
			if !in {
				logger.Warn("skipping PID matching a worker row but not running inside a worktree; cannot confirm ownership",
					"pid", w.PID, "worker", w.ID, "bead", w.BeadID, "cwd", p.cwd)
				continue
			}
			evidence += " " + cwd
		}
		owned = append(owned, ownedProc{
			pid:      p.pid,
			evidence: evidence,
		})
		seen[p.pid] = true
	}

	// Secondary sweep: genuinely lost workers whose PID was never recorded.
	// This relies entirely on the cwd-in-worktree check for safety (argv[0]
	// basename alone would match the operator's own Claude session), so it is
	// skipped on platforms that cannot resolve a process working directory.
	if !requireWorktreeCwd {
		return owned
	}
	for _, p := range procs {
		if seen[p.pid] {
			continue
		}
		base := argvBasename(p.argv)
		if !forgeWorkerBasenames[base] {
			continue
		}
		cwd, in := cwdEvidence(p)
		if !in {
			continue
		}
		owned = append(owned, ownedProc{
			pid:      p.pid,
			evidence: fmt.Sprintf("lost-worker argv0=%s %s", base, cwd),
		})
		seen[p.pid] = true
	}

	return owned
}

// startTimeConsistent reports whether a process start time is consistent with a
// worker row's recorded started_at. The worker row is created at claim time and
// its process spawns shortly after (following any schematic/worktree setup), so
// a genuine worker starts within [started_at - skew, started_at + maxSpawnDelay].
// A process outside that window occupying a recycled PID is not our worker. When
// either timestamp is unknown, ownership cannot be established and the result is
// false (fail closed).
func startTimeConsistent(procStart, workerStart time.Time) bool {
	if workerStart.IsZero() || procStart.IsZero() {
		return false
	}
	if procStart.Before(workerStart.Add(-startTimeSkew)) {
		return false
	}
	if procStart.After(workerStart.Add(maxSpawnDelay)) {
		return false
	}
	return true
}

// pathUnderAny reports the first root that dir is equal to or nested within.
// The "(deleted)" suffix Linux appends to /proc/<pid>/cwd for removed
// directories is stripped first so an already-removed worktree still matches.
func pathUnderAny(dir string, roots []string) (string, bool) {
	dir = strings.TrimSuffix(dir, " (deleted)")
	if dir == "" {
		return "", false
	}
	dir = filepath.Clean(dir)
	for _, root := range roots {
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		if dir == root || strings.HasPrefix(dir, root+string(filepath.Separator)) {
			return root, true
		}
	}
	return "", false
}

// argvBasename returns the executable basename from argv[0], handling both Unix
// and Windows path separators regardless of host OS. Returns "" for empty argv.
func argvBasename(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	a0 := argv[0]
	if i := strings.LastIndexAny(a0, `/\`); i >= 0 {
		a0 = a0[i+1:]
	}
	return a0
}
