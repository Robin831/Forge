// Package logsweep implements the retention sweep for preserved bead-log
// directories under ~/.forge/logs/<beadID>/.
//
// The daemon repoints worker log_path rows into these directories when a
// worktree is cleaned up (see state.RepointWorkerLogPaths /
// BackfillDanglingWorkerLogPaths), so they accumulate without bound. The sweep
// runs as a daily background monitor — mirroring the depcheck/vulncheck monitor
// pattern — and deletes directories whose newest file is older than the
// configured retention window, provided no worker is currently running for that
// bead. It never touches the live daemon.log file (that lives at the root of
// logs/, not in a per-bead subdirectory) — rotation of daemon.log is an
// independent mechanism (see internal/logrotate).
package logsweep

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Robin831/Forge/internal/forge"
	"github.com/Robin831/Forge/internal/state"
)

// workerLister is the subset of *state.DB the sweep needs; it exists so tests
// can inject a lightweight double.
type workerLister interface {
	ActiveWorkers() ([]state.Worker, error)
	NullWorkerLogPathsUnder(dir string) (int, error)
	LogEvent(typ state.EventType, message, beadID, anvil string) error
}

// Monitor runs the retention sweep on a schedule.
type Monitor struct {
	db       workerLister
	logger   *slog.Logger
	logsRoot string
	interval time.Duration
	// retentionDays is read fresh each sweep so config hot-reload takes effect
	// without restarting the monitor. 0 (or negative) disables the sweep.
	retentionDays func() int
	// now is overridable in tests; defaults to time.Now.
	now func() time.Time
}

// New constructs a Monitor. logsRoot is ~/.forge/logs. retentionDays is a getter
// so the current config value is consulted on every sweep.
func New(db workerLister, logger *slog.Logger, logsRoot string, interval time.Duration, retentionDays func() int) *Monitor {
	return &Monitor{
		db:            db,
		logger:        logger,
		logsRoot:      logsRoot,
		interval:      interval,
		retentionDays: retentionDays,
		now:           time.Now,
	}
}

// RunScheduled is a blocking loop that runs the sweep on m.interval. It should
// be launched as a goroutine. An interval <= 0 disables scheduled sweeps.
func (m *Monitor) RunScheduled(ctx context.Context) {
	if m.interval <= 0 {
		m.logger.Info("log retention sweep disabled (interval=0)")
		return
	}

	m.logger.Info("log retention sweep started", "interval", m.interval)

	// Short startup delay so the sweep does not compete with other daemon init.
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}
	m.runOnce(ctx)

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.logger.Info("log retention sweep stopped")
			return
		case <-ticker.C:
			m.runOnce(ctx)
		}
	}
}

// SweepResult reports what a single sweep removed.
type SweepResult struct {
	DirsRemoved int
	BytesFreed  int64
}

// runOnce performs a single sweep pass and emits a summary event. Errors are
// logged, not returned, so the scheduled loop keeps running.
func (m *Monitor) runOnce(ctx context.Context) {
	retention := m.retentionDays()
	if retention <= 0 {
		return // disabled
	}

	activeBeadDirs, err := m.activeBeadDirs()
	if err != nil {
		m.logger.Warn("log sweep: could not list active workers, skipping this pass", "error", err)
		return
	}

	entries, err := os.ReadDir(m.logsRoot)
	if err != nil {
		if !os.IsNotExist(err) {
			m.logger.Warn("log sweep: could not read logs directory", "root", m.logsRoot, "error", err)
		}
		return
	}

	now := m.now()
	var result SweepResult
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		// Only per-bead subdirectories are eligible; the live daemon.log (and
		// its rotated backups) are files at the root and are never touched.
		if !e.IsDir() {
			continue
		}
		dirName := e.Name()
		dir := filepath.Join(m.logsRoot, dirName)

		newest, bytes, err := dirNewestAndSize(dir)
		if err != nil {
			m.logger.Warn("log sweep: could not stat bead-log dir", "dir", dir, "error", err)
			continue
		}

		hasRunningWorker := activeBeadDirs[dirName]
		if !ShouldSweep(newest, retention, hasRunningWorker, now) {
			continue
		}

		if err := os.RemoveAll(dir); err != nil {
			m.logger.Warn("log sweep: failed to remove bead-log dir", "dir", dir, "error", err)
			continue
		}
		if nulled, err := m.db.NullWorkerLogPathsUnder(dir); err != nil {
			m.logger.Warn("log sweep: failed to clear worker log paths", "dir", dir, "error", err)
		} else if nulled > 0 {
			m.logger.Info("log sweep: cleared dangling worker log paths", "dir", dir, "rows", nulled)
		}
		result.DirsRemoved++
		result.BytesFreed += bytes
	}

	if result.DirsRemoved > 0 {
		m.logger.Info("log retention sweep complete",
			"dirs_removed", result.DirsRemoved,
			"bytes_freed", result.BytesFreed,
			"retention_days", retention)
	}
	msg := formatSummary(result, retention)
	if err := m.db.LogEvent(state.EventLogSweepDone, msg, "", ""); err != nil {
		m.logger.Warn("log sweep: failed to record summary event", "error", err)
	}
}

// activeBeadDirs returns the set of sanitized bead directory names that have a
// currently active worker. Directory names on disk are SanitizeBeadID(beadID),
// so bead IDs are sanitized here to match.
func (m *Monitor) activeBeadDirs() (map[string]bool, error) {
	workers, err := m.db.ActiveWorkers()
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(workers))
	for _, w := range workers {
		if w.BeadID == "" {
			continue
		}
		set[forge.SanitizeBeadID(w.BeadID)] = true
	}
	return set, nil
}

// ShouldSweep is the pure retention decision: a bead-log directory is removable
// when retention is enabled (retentionDays > 0), no worker is currently running
// for the bead, and its newest file's mtime is strictly older than the
// retention cutoff (now - retentionDays). A directory whose newest file is
// exactly at the cutoff is retained.
func ShouldSweep(newest time.Time, retentionDays int, hasRunningWorker bool, now time.Time) bool {
	if retentionDays <= 0 {
		return false
	}
	if hasRunningWorker {
		return false
	}
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	return newest.Before(cutoff)
}

// dirNewestAndSize walks dir (one level, recursively via WalkDir) and returns
// the most recent file modification time and the total size of all regular
// files. An empty directory yields a zero time and zero size, which — being far
// in the past — makes it eligible for removal.
func dirNewestAndSize(dir string) (time.Time, int64, error) {
	var newest time.Time
	var total int64
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			// A file that vanished mid-walk is ignored rather than aborting.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if mt := info.ModTime(); mt.After(newest) {
			newest = mt
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return time.Time{}, 0, err
	}
	return newest, total, nil
}

// formatSummary renders the one-line summary stored on the sweep event.
func formatSummary(r SweepResult, retentionDays int) string {
	return fmt.Sprintf("log retention sweep: removed %d dir(s), freed %d bytes (retention %dd)",
		r.DirsRemoved, r.BytesFreed, retentionDays)
}
