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
	// the workers table and the process working directory — a process is never
	// killed on the basis of a cmdline substring, so the operator's own Claude
	// Code session (and unrelated tools whose argv merely mentions "claude") are
	// left untouched. The `workers` snapshot taken above carries the recorded
	// PIDs and start times used for verification.
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
	ctx, cancel := context.WithTimeout(context.Background(), executil.DefaultBdTimeout)
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

// resetBead marks a bead as open via bd update and clears the assignee.
// Clearing the assignee is required so the poller can re-dispatch the bead —
// the poller filters out any bead with a non-empty assignee.
func (m *Manager) resetBead(beadID, anvilPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), executil.DefaultBdTimeout)
	defer cancel()

	// Clear labels so the poller does not auto-dispatch the recovered bead
	// before a human reviews it. Without this, orphaned beads with the
	// auto_dispatch_tag would immediately be re-claimed on the next poll,
	// creating a zombie loop.
	var stderrBuf bytes.Buffer
	cmd := executil.HideWindow(exec.CommandContext(ctx, "bd", "update", beadID, "--status=open", "--assignee=", "--remove-label=forgeReady", "--json"))
	cmd.Dir = anvilPath
	cmd.Stderr = &stderrBuf
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("bd update %s timed out: %w", beadID, context.DeadlineExceeded)
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
// process start time, and the working directory — never from a cmdline
// substring.
type procInfo struct {
	pid       int
	pgid      int      // process group id (equals pid for a group leader)
	argv      []string // argv[0..n] from /proc/<pid>/cmdline
	startTime time.Time
	cwd       string // resolved target of /proc/<pid>/cwd
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
// a package-level var so tests can inject a fake process table. The default
// implementation reads /proc on Unix and is a no-op on other platforms.
var listProcesses = listProcessesDefault

func listProcessesDefault() ([]procInfo, error) {
	if runtime.GOOS == "windows" {
		return listProcessesWindows()
	}
	return listProcessesUnix()
}

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
	return identifyForgeOwnedProcesses(workers, procs, m.worktreeRoots(), m.logger)
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
// Two sources of positive evidence are used, and every kill requires the
// process (or its process-group leader) to be running inside a Forge worktree:
//
//  1. Primary — the workers table: a live process whose PID matches a recorded
//     worker, whose start time is consistent with the worker row (guards PID
//     recycling), and which is running inside a worktree.
//  2. Secondary — lost workers: a process whose PID was never recorded but whose
//     argv[0] basename is an EXACT match for a Forge worker binary AND which is
//     running inside a worktree.
//
// A process is never matched on a cmdline substring, so the operator's own
// Claude Code session (cwd outside any worktree) and unrelated tools whose argv
// merely contains ".claude" are always spared.
func identifyForgeOwnedProcesses(workers []state.Worker, procs []procInfo, worktreeRoots []string, logger *slog.Logger) []ownedProc {
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
		cwd, in := cwdEvidence(p)
		if !in {
			logger.Warn("skipping PID matching a worker row but not running inside a worktree; cannot confirm ownership",
				"pid", w.PID, "worker", w.ID, "bead", w.BeadID, "cwd", p.cwd)
			continue
		}
		owned = append(owned, ownedProc{
			pid:      p.pid,
			evidence: fmt.Sprintf("worker=%s start-time-ok %s", w.ID, cwd),
		})
		seen[p.pid] = true
	}

	// Secondary sweep: genuinely lost workers whose PID was never recorded.
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

// listProcessesWindows is a no-op: process enumeration on Windows is not
// implemented, so the orphan sweep reaps nothing there (preserving the prior
// findClaudeProcessesWindows behavior). If implemented, it MUST apply the same
// ownership contract as identifyForgeOwnedProcesses — never selecting a process
// on a cmdline substring alone.
func listProcessesWindows() ([]procInfo, error) {
	return nil, nil
}

// listProcessesUnix walks /proc to build a process table for ownership
// verification. Best-effort: unreadable entries are skipped.
func listProcessesUnix() ([]procInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	boot, clk := bootTimeSeconds(), clockTicks()
	procs := make([]procInfo, 0, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		p := procInfo{pid: pid}

		if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
			p.argv = splitCmdline(data)
		}
		if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			pgid, start := parseStat(data)
			p.pgid = pgid
			if start > 0 && boot > 0 && clk > 0 {
				p.startTime = time.Unix(boot+int64(start/uint64(clk)), 0)
			}
		}
		if target, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
			p.cwd = target
		}
		procs = append(procs, p)
	}
	return procs, nil
}

// splitCmdline splits a NUL-delimited /proc/<pid>/cmdline into argv.
func splitCmdline(data []byte) []string {
	data = bytes.TrimRight(data, "\x00")
	if len(data) == 0 {
		return nil
	}
	parts := bytes.Split(data, []byte{0})
	argv := make([]string, 0, len(parts))
	for _, part := range parts {
		argv = append(argv, string(part))
	}
	return argv
}

// parseStat extracts the process group id (field 5) and the start time in clock
// ticks since boot (field 22) from /proc/<pid>/stat. The comm field (field 2)
// is wrapped in parentheses and may itself contain spaces and parentheses, so
// parsing of the space-delimited fields resumes after the final ')'.
func parseStat(data []byte) (pgid int, starttime uint64) {
	s := string(data)
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen+1 >= len(s) {
		return 0, 0
	}
	fields := strings.Fields(s[rparen+1:])
	// fields[0] is field 3 (state); field N maps to index N-3.
	// pgrp = field 5 -> index 2; starttime = field 22 -> index 19.
	if len(fields) > 2 {
		pgid, _ = strconv.Atoi(fields[2])
	}
	if len(fields) > 19 {
		starttime, _ = strconv.ParseUint(fields[19], 10, 64)
	}
	return pgid, starttime
}

// bootTimeSeconds returns the system boot time in seconds since the Unix epoch,
// read from /proc/stat's "btime" line. Returns 0 when unavailable.
func bootTimeSeconds() int64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			v, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				return 0
			}
			return v
		}
	}
	return 0
}

// clockTicks returns the number of clock ticks per second (USER_HZ), used to
// convert a process start time from ticks to seconds. Linux fixes USER_HZ at
// 100 on effectively all supported architectures, and there is no cgo-free way
// to read sysconf(_SC_CLK_TCK), so 100 is assumed.
func clockTicks() int64 {
	return 100
}
