package kiln

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/worktree"
)

const (
	// previewStartSkew tolerates small clock/rounding differences between a
	// preview row's created_at and the process start time the OS reports
	// (/proc has ~1s resolution).
	previewStartSkew = 90 * time.Second

	// maxPreviewSpawnDelay bounds how long after a preview row is written its
	// service processes may legitimately have started. The row is persisted
	// before anything is spawned, but the manifest's setup command (a database
	// restore, say) runs in between. A process whose start time falls outside
	// that window is occupying a recycled PID and is NOT the recorded service.
	maxPreviewSpawnDelay = 30 * time.Minute

	// orphanStopGrace is how long an orphaned service's process group gets to
	// exit after a polite signal before it is killed. Short, because this runs
	// on the daemon's startup path.
	orphanStopGrace = 5 * time.Second
)

// ProcessInfo is a live process as reconciliation sees it — the evidence used
// to decide whether a PID recorded in a preview row is still that preview's
// service, or a PID the OS has since handed to something else.
type ProcessInfo struct {
	// PID is the process id that was inspected.
	PID int
	// StartTime is when the process started; the zero value means the platform
	// could not report it, which fails ownership closed.
	StartTime time.Time
	// Cwd is the process's working directory, empty when it could not be read.
	Cwd string
	// CwdSupported reports whether this platform exposes a process working
	// directory at all (Linux does, via /proc; Windows does not). When it is
	// true the cwd check is mandatory, so an unreadable cwd also fails closed.
	CwdSupported bool
}

// Processes inspects and terminates OS processes. It exists as an interface so
// reconciliation can be tested without spawning — or, much more importantly,
// without ever signalling — a real process.
type Processes interface {
	// Inspect returns a snapshot of the live process with the given pid. The
	// second return is false when no such process is running.
	Inspect(pid int) (ProcessInfo, bool)
	// Terminate stops the process group led by pid: a polite signal, a brief
	// grace period, then a kill. The whole group is signalled because a preview
	// service is a shell line that routinely forks, and the children are what
	// hold the port.
	Terminate(pid int) error
}

// OSProcesses is the real Processes implementation, backed by the platform
// helpers in procinfo_unix.go / procinfo_windows.go.
type OSProcesses struct{}

// Inspect implements Processes.
func (OSProcesses) Inspect(pid int) (ProcessInfo, bool) { return inspectProcess(pid) }

// Terminate implements Processes.
func (OSProcesses) Terminate(pid int) error { return terminateProcessGroup(pid, orphanStopGrace) }

// Reconcile clears out the preview environments a previous daemon left behind.
// It is run once at manager startup, before any preview is served: a preview's
// processes, worktree and row all outlive a daemon crash, and a stale port
// binding or an abandoned checkout would otherwise silently break the next
// preview of the same bead.
//
// Previews are not resumed, only cleaned up. A preview is cheap to recreate and
// an operator asks for one when they want it; adopting a set of processes whose
// health we never observed would be guesswork.
//
// For every persisted row it kills the recorded service process groups, removes
// the detached checkout and deletes the row; it then prunes any
// <anvil>/.previews/ directory with no live preview behind it. A PID is only
// ever signalled once the live process is positively identified as that
// preview's service (see previewOwnsProcess) — killing a recycled PID is far
// worse than leaving a stray. Every step runs even when an earlier one failed;
// the errors are joined and returned so startup can log them without any of
// them aborting the sweep.
func (m *Manager) Reconcile(ctx context.Context) error {
	m.logger.Info("kiln: checking for orphaned preview environments")

	rows, err := m.listPreviewRows()
	if err != nil {
		// Without the rows we cannot tell an abandoned checkout from a live
		// one, so nothing is pruned either.
		return err
	}

	var errs []error
	reconciled := 0
	for _, row := range rows {
		// Defensive: Reconcile is a startup step and the registry is empty
		// then, but a preview this manager owns must never be torn down here.
		if _, live := m.Get(row.BeadID); live {
			continue
		}
		if err := m.reconcileRow(ctx, row); err != nil {
			errs = append(errs, err)
		}
		reconciled++
	}

	pruned, err := m.prunePreviewDirs(ctx)
	if err != nil {
		errs = append(errs, err)
	}

	if reconciled == 0 && pruned == 0 {
		m.logger.Info("kiln: no orphaned previews found")
	} else {
		m.logger.Info("kiln: preview reconciliation complete",
			"rows", reconciled, "pruned_directories", pruned)
	}
	return errors.Join(errs...)
}

// listPreviewRows returns every persisted preview. A manager without a store
// has no rows to reconcile — only stray directories.
func (m *Manager) listPreviewRows() ([]state.Preview, error) {
	if m.store == nil {
		return nil, nil
	}
	rows, err := m.store.ListPreviews()
	if err != nil {
		return nil, fmt.Errorf("kiln: listing previews for reconciliation: %w", err)
	}
	return rows, nil
}

// reconcileRow tears one orphaned preview down: processes, then checkout, then
// row — the same order Stop uses, and for the same reasons (a running service
// holds files in the worktree open, and the row must outlive both so a failure
// halfway through is still visible to the next startup).
func (m *Manager) reconcileRow(ctx context.Context, row state.Preview) error {
	m.logger.Warn("kiln: found orphaned preview",
		"bead", row.BeadID, "anvil", row.Anvil, "status", row.Status,
		"worktree", row.WorktreePath, "created", row.CreatedAt)

	var errs []error
	for _, svc := range row.Services {
		if err := m.killOrphanService(row, svc); err != nil {
			errs = append(errs, err)
		}
	}

	if anvilPath := m.anvilPathFor(row); anvilPath == "" {
		m.logger.Warn("kiln: cannot locate the anvil of an orphaned preview; leaving its checkout in place",
			"bead", row.BeadID, "anvil", row.Anvil, "worktree", row.WorktreePath)
	} else if err := m.worktrees.RemoveDetached(ctx, anvilPath, row.BeadID); err != nil {
		errs = append(errs, fmt.Errorf("kiln: removing orphaned preview worktree for %s: %w", row.BeadID, err))
	}

	if m.store != nil {
		if err := m.store.DeletePreview(row.BeadID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// killOrphanService kills one recorded service's process group, but only after
// the live process has been positively identified as that service.
func (m *Manager) killOrphanService(row state.Preview, svc state.PreviewService) error {
	if svc.PID <= 0 {
		return nil
	}
	info, alive := m.processes.Inspect(svc.PID)
	if !alive {
		m.logger.Debug("kiln: orphaned preview service is already gone",
			"bead", row.BeadID, "service", svc.Name, "pid", svc.PID)
		return nil
	}
	evidence, owned := previewOwnsProcess(row, info)
	if !owned {
		m.logger.Warn("kiln: skipping a live PID recorded by a preview but not identifiable as its service (likely a recycled PID)",
			"bead", row.BeadID, "service", svc.Name, "pid", svc.PID,
			"proc_start", info.StartTime, "preview_created", row.CreatedAt, "cwd", info.Cwd)
		return nil
	}
	m.logger.Warn("kiln: killing orphaned preview service",
		"bead", row.BeadID, "service", svc.Name, "pid", svc.PID, "evidence", evidence)
	if err := m.processes.Terminate(svc.PID); err != nil {
		return fmt.Errorf("kiln: killing orphaned preview service %s of %s (pid %d): %w",
			svc.Name, row.BeadID, svc.PID, err)
	}
	return nil
}

// previewOwnsProcess reports whether the live process is the service the
// preview row recorded, with the evidence that established it.
//
// Two independent checks, both fail-closed, mirroring the worker orphan sweep
// in internal/shutdown:
//
//  1. Start time — the process must have started inside the window the preview
//     could have spawned it in. This is what guards against PID recycling.
//  2. Working directory — where the platform exposes one, the process (which is
//     the shell leading the service's group) must be running inside the
//     preview's own checkout.
//
// A process is never matched on its command line: a preview command is
// arbitrary project shell (`npm run dev`) that looks exactly like what the
// operator may be running by hand in the same repository.
func previewOwnsProcess(row state.Preview, info ProcessInfo) (string, bool) {
	if !startTimeWithinPreview(info.StartTime, row.CreatedAt) {
		return "", false
	}
	evidence := fmt.Sprintf("preview=%s start-time-ok", row.BeadID)
	if info.CwdSupported {
		if !pathWithin(info.Cwd, row.WorktreePath) {
			return "", false
		}
		evidence += " cwd-in-preview=" + filepath.Clean(row.WorktreePath)
	}
	return evidence, true
}

// startTimeWithinPreview reports whether a process start time is consistent
// with a preview row's created_at: not before it (bar clock skew) and not
// absurdly long after it. Either timestamp being unknown means ownership cannot
// be established, which is a "no".
func startTimeWithinPreview(procStart, createdAt time.Time) bool {
	if procStart.IsZero() || createdAt.IsZero() {
		return false
	}
	if procStart.Before(createdAt.Add(-previewStartSkew)) {
		return false
	}
	if procStart.After(createdAt.Add(maxPreviewSpawnDelay)) {
		return false
	}
	return true
}

// pathWithin reports whether dir is root or nested inside it. A service may run
// from a subdirectory of the checkout (manifest `dir:`), so this is a prefix
// test rather than an equality test. Linux appends " (deleted)" to
// /proc/<pid>/cwd once the directory is gone, which is exactly the case of a
// process still running out of a half-removed preview, so that suffix is
// stripped before comparing.
func pathWithin(dir, root string) bool {
	dir = strings.TrimSuffix(dir, " (deleted)")
	if dir == "" || root == "" {
		return false
	}
	dir = filepath.Clean(dir)
	root = filepath.Clean(root)
	return dir == root || strings.HasPrefix(dir, root+string(filepath.Separator))
}

// anvilPathFor resolves the main checkout an orphaned preview belongs to. The
// configured anvils are authoritative; a row whose anvil is no longer
// configured (renamed, removed) falls back to deriving the path from the
// recorded worktree, which is always <anvilPath>/.previews/<beadID>. Returns ""
// when neither works, in which case the checkout is left alone rather than
// guessed at.
func (m *Manager) anvilPathFor(row state.Preview) string {
	if path, ok := m.cfg.Anvils[row.Anvil]; ok && strings.TrimSpace(path) != "" {
		return path
	}
	if strings.TrimSpace(row.WorktreePath) == "" {
		return ""
	}
	previewsDir := filepath.Dir(filepath.Clean(row.WorktreePath))
	if filepath.Base(previewsDir) != worktree.PreviewsDir {
		return ""
	}
	return filepath.Dir(previewsDir)
}

// prunePreviewDirs removes <anvil>/.previews/ checkouts that no live preview
// owns — what a daemon killed between `git worktree add` and the first row
// write leaves behind, and what a row whose removal failed last time leaves
// behind too. Directories belonging to previews this manager currently runs are
// kept, so calling Reconcile outside startup cannot cut a running preview's
// checkout out from under it.
func (m *Manager) prunePreviewDirs(ctx context.Context) (int, error) {
	keep := make(map[string]bool)
	for _, env := range m.List() {
		if env.WorktreePath != "" {
			keep[filepath.Clean(env.WorktreePath)] = true
		}
	}

	var errs []error
	pruned := 0
	for name, anvilPath := range m.cfg.Anvils {
		if strings.TrimSpace(anvilPath) == "" {
			continue
		}
		root := filepath.Join(anvilPath, worktree.PreviewsDir)
		entries, err := os.ReadDir(root)
		if err != nil {
			// An anvil that has never had a preview has no .previews directory;
			// that is the common case, not a problem.
			if !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("kiln: scanning %s for abandoned previews: %w", root, err))
			}
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(root, entry.Name())
			if keep[filepath.Clean(path)] {
				continue
			}
			m.logger.Warn("kiln: pruning abandoned preview checkout", "path", path, "anvil", name)
			// RemoveDetached rather than a bare delete: the directory may still
			// be a registered git worktree, and the registration has to go too.
			// The directory name is already the sanitized bead id, so it round
			// trips to the same path.
			if err := m.worktrees.RemoveDetached(ctx, anvilPath, entry.Name()); err != nil {
				errs = append(errs, fmt.Errorf("kiln: pruning abandoned preview checkout %s: %w", path, err))
				continue
			}
			pruned++
		}
	}
	return pruned, errors.Join(errs...)
}
