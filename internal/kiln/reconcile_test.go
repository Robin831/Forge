package kiln

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// --- fake process table --------------------------------------------------

// fakeProcesses is the Processes stand-in every test uses: no test may inspect
// or signal a real PID, and ownership verification has to be driven from both
// sides (a process that matches the row, and one that merely reuses its PID).
type fakeProcesses struct {
	mu         sync.Mutex
	live       map[int]ProcessInfo
	terminated []int
	termErr    error
}

func newFakeProcesses() *fakeProcesses {
	return &fakeProcesses{live: make(map[int]ProcessInfo)}
}

func (f *fakeProcesses) add(pid int, info ProcessInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info.PID = pid
	f.live[pid] = info
}

func (f *fakeProcesses) Inspect(pid int) (ProcessInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.live[pid]
	return info, ok
}

func (f *fakeProcesses) Terminate(pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, pid)
	if f.termErr != nil {
		return f.termErr
	}
	delete(f.live, pid)
	return nil
}

func (f *fakeProcesses) killed() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.terminated...)
}

// --- helpers -------------------------------------------------------------

// writeOrphanRow persists a preview row as a crashed daemon would have left it,
// with the checkout still on disk, and returns the worktree path.
func writeOrphanRow(t *testing.T, h *harness, beadID string, createdAt time.Time, services ...state.PreviewService) string {
	t.Helper()
	path := previewDir(t, h, beadID)
	if err := h.store.UpsertPreview(state.Preview{
		BeadID:       beadID,
		Anvil:        "forge",
		Branch:       "forge/" + beadID,
		Status:       state.PreviewRunning,
		WorktreePath: path,
		Services:     services,
		CreatedAt:    createdAt,
		LastActiveAt: createdAt,
	}); err != nil {
		t.Fatalf("UpsertPreview(%s): %v", beadID, err)
	}
	return path
}

// previewDir creates <anvil>/.previews/<beadID> and returns it.
func previewDir(t *testing.T, h *harness, beadID string) string {
	t.Helper()
	path := filepath.Join(h.anvil, ".previews", beadID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("creating preview directory: %v", err)
	}
	return path
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s still exists: %v", path, err)
	}
}

func assertNoRow(t *testing.T, h *harness, beadID string) {
	t.Helper()
	row, err := h.store.GetPreview(beadID)
	if err != nil {
		t.Fatalf("GetPreview(%s): %v", beadID, err)
	}
	if row != nil {
		t.Errorf("preview row for %s survived reconciliation: %+v", beadID, row)
	}
}

// --- tests ---------------------------------------------------------------

func TestReconcileKillsOrphanAndRemovesEverything(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	created := time.Now().Add(-2 * time.Hour)
	path := writeOrphanRow(t, h, "Forge-aaa1", created,
		state.PreviewService{Name: "api", Port: 42000, PID: 4242, Health: state.PreviewServiceHealthy})
	h.procs.add(4242, ProcessInfo{StartTime: created.Add(time.Second), Cwd: path, CwdSupported: true})

	if err := h.mgr.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := h.procs.killed(); len(got) != 1 || got[0] != 4242 {
		t.Errorf("terminated %v, want [4242]", got)
	}
	if got := h.wts.removedPaths(); len(got) != 1 || got[0] != path {
		t.Errorf("RemoveDetached calls = %v, want [%s]", got, path)
	}
	assertGone(t, path)
	assertNoRow(t, h, "Forge-aaa1")
}

func TestReconcileKillsServiceRunningInASubdirectory(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	created := time.Now().Add(-time.Hour)
	path := writeOrphanRow(t, h, "Forge-aaa1", created,
		state.PreviewService{Name: "web", Port: 42001, PID: 77, Health: state.PreviewServiceHealthy})
	// A manifest `dir:` puts the service one level down; it is still ours.
	h.procs.add(77, ProcessInfo{StartTime: created, Cwd: filepath.Join(path, "web"), CwdSupported: true})

	if err := h.mgr.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := h.procs.killed(); len(got) != 1 {
		t.Errorf("terminated %v, want the service in the checkout subdirectory", got)
	}
}

func TestReconcileSkipsRecycledPID(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	created := time.Now().Add(-3 * time.Hour)
	path := writeOrphanRow(t, h, "Forge-aaa1", created,
		state.PreviewService{Name: "api", Port: 42000, PID: 4242, Health: state.PreviewServiceHealthy})
	// Alive, and sitting in the right directory — but started long after the
	// preview did, so the OS handed this PID to something else.
	h.procs.add(4242, ProcessInfo{StartTime: time.Now(), Cwd: path, CwdSupported: true})

	if err := h.mgr.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := h.procs.killed(); len(got) != 0 {
		t.Errorf("terminated %v, want nothing — the PID was recycled", got)
	}
	// The rest of the cleanup still has to happen.
	assertGone(t, path)
	assertNoRow(t, h, "Forge-aaa1")
}

func TestReconcileSkipsProcessOutsideTheCheckout(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	created := time.Now().Add(-time.Hour)
	writeOrphanRow(t, h, "Forge-aaa1", created,
		state.PreviewService{Name: "api", Port: 42000, PID: 4242, Health: state.PreviewServiceHealthy})
	// Start time fits, but the process is the operator's own shell somewhere
	// else entirely.
	h.procs.add(4242, ProcessInfo{StartTime: created, Cwd: t.TempDir(), CwdSupported: true})

	if err := h.mgr.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := h.procs.killed(); len(got) != 0 {
		t.Errorf("terminated %v, want nothing — the process is not in the preview checkout", got)
	}
}

func TestReconcileSkipsProcessWithNoStartTime(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	created := time.Now().Add(-time.Hour)
	path := writeOrphanRow(t, h, "Forge-aaa1", created,
		state.PreviewService{Name: "api", Port: 42000, PID: 4242, Health: state.PreviewServiceHealthy})
	// A platform that cannot report a start time cannot establish ownership.
	h.procs.add(4242, ProcessInfo{Cwd: path, CwdSupported: true})

	if err := h.mgr.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := h.procs.killed(); len(got) != 0 {
		t.Errorf("terminated %v, want nothing — ownership must fail closed", got)
	}
}

// TestReconcileKillsOnAPlatformWithoutCwd covers Windows, where a process
// working directory is unavailable and ownership rests on the PID +
// creation-time match alone.
func TestReconcileKillsOnAPlatformWithoutCwd(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	created := time.Now().Add(-time.Hour)
	writeOrphanRow(t, h, "Forge-aaa1", created,
		state.PreviewService{Name: "api", Port: 42000, PID: 4242, Health: state.PreviewServiceHealthy})
	h.procs.add(4242, ProcessInfo{StartTime: created.Add(time.Minute)})

	if err := h.mgr.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := h.procs.killed(); len(got) != 1 {
		t.Errorf("terminated %v, want the recorded service", got)
	}
}

func TestReconcileIgnoresDeadAndUnrecordedPIDs(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	created := time.Now().Add(-time.Hour)
	path := writeOrphanRow(t, h, "Forge-aaa1", created,
		state.PreviewService{Name: "api", PID: 0, Health: state.PreviewServiceFailed},
		state.PreviewService{Name: "web", PID: 9999, Health: state.PreviewServiceHealthy})
	// 9999 is not in the process table: it exited with the daemon.

	if err := h.mgr.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := h.procs.killed(); len(got) != 0 {
		t.Errorf("terminated %v, want nothing", got)
	}
	assertGone(t, path)
	assertNoRow(t, h, "Forge-aaa1")
}

func TestReconcileReportsAFailedKillButFinishesTheSweep(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	h.procs.termErr = errors.New("operation not permitted")
	created := time.Now().Add(-time.Hour)
	path := writeOrphanRow(t, h, "Forge-aaa1", created,
		state.PreviewService{Name: "api", Port: 42000, PID: 4242, Health: state.PreviewServiceHealthy})
	h.procs.add(4242, ProcessInfo{StartTime: created, Cwd: path, CwdSupported: true})

	err := h.mgr.Reconcile(context.Background())
	if err == nil {
		t.Fatal("Reconcile hid a failed kill")
	}
	// The row and the checkout still have to go, or the next startup faces the
	// same orphan plus a worktree git will refuse to re-add.
	assertGone(t, path)
	assertNoRow(t, h, "Forge-aaa1")
}

func TestReconcilePrunesDirectoryWithNoRow(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	// A daemon killed between `git worktree add` and the first row write.
	stale := previewDir(t, h, "Forge-stale")

	if err := h.mgr.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertGone(t, stale)
	if got := h.wts.removedPaths(); len(got) != 1 || got[0] != stale {
		t.Errorf("RemoveDetached calls = %v, want [%s]", got, stale)
	}
}

func TestReconcileLeavesFilesAndUnconfiguredAnvilsAlone(t *testing.T) {
	other := t.TempDir()
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})

	// A stray file (not a checkout) in the previews directory.
	previewsRoot := filepath.Join(h.anvil, ".previews")
	if err := os.MkdirAll(previewsRoot, 0o755); err != nil {
		t.Fatalf("creating previews root: %v", err)
	}
	stray := filepath.Join(previewsRoot, "notes.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing stray file: %v", err)
	}
	// A previews directory in an anvil this Forge does not manage.
	unmanaged := filepath.Join(other, ".previews", "Forge-zzz9")
	if err := os.MkdirAll(unmanaged, 0o755); err != nil {
		t.Fatalf("creating unmanaged preview: %v", err)
	}

	if err := h.mgr.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("a non-directory entry was pruned: %v", err)
	}
	if _, err := os.Stat(unmanaged); err != nil {
		t.Errorf("a preview outside the configured anvils was pruned: %v", err)
	}
}

func TestReconcileKeepsRunningPreviews(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	env, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Reconcile is a startup step, but running it against a live manager must
	// not tear the previews it owns down.
	if err := h.mgr.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if _, ok := h.mgr.Get("Forge-aaa1"); !ok {
		t.Error("a running preview was dropped from the registry")
	}
	if _, err := os.Stat(env.WorktreePath); err != nil {
		t.Errorf("a running preview lost its checkout: %v", err)
	}
	row, err := h.store.GetPreview("Forge-aaa1")
	if err != nil || row == nil {
		t.Errorf("a running preview lost its state row: row=%v err=%v", row, err)
	}
	if got := h.procs.killed(); len(got) != 0 {
		t.Errorf("terminated %v while reconciling around a running preview", got)
	}
}

func TestReconcileWithoutAStorePrunesDirectories(t *testing.T) {
	anvil := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	wts := &fakeWorktrees{root: anvil}
	mgr, err := NewManager(ManagerDeps{
		Runtime:      &fakeRunner{},
		Worktrees:    wts,
		Processes:    newFakeProcesses(),
		Config:       ManagerConfig{MaxConcurrent: 1, Anvils: map[string]string{"forge": anvil}},
		LoadManifest: func(string) (*Manifest, error) { return testManifest(t), nil },
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	stale := filepath.Join(anvil, ".previews", "Forge-stale")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatalf("creating stale preview: %v", err)
	}

	if err := mgr.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile without a store: %v", err)
	}
	assertGone(t, stale)
}

func TestReconcileDerivesTheAnvilFromTheWorktreePath(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	// The anvil this row names is no longer configured (renamed, or removed),
	// so nothing scans its previews directory: only the path recorded on the
	// row can lead the cleanup to the checkout.
	h.mgr.cfg.Anvils = nil
	path := writeOrphanRow(t, h, "Forge-aaa1", time.Now().Add(-time.Hour))

	if err := h.mgr.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertGone(t, path)
	assertNoRow(t, h, "Forge-aaa1")
}

func TestPreviewOwnsProcess(t *testing.T) {
	created := time.Now().Add(-time.Hour)
	row := state.Preview{BeadID: "Forge-aaa1", WorktreePath: "/anvil/.previews/Forge-aaa1", CreatedAt: created}

	cases := map[string]struct {
		info ProcessInfo
		want bool
	}{
		"same instant": {
			ProcessInfo{StartTime: created, Cwd: row.WorktreePath, CwdSupported: true}, true,
		},
		"within the setup window": {
			ProcessInfo{StartTime: created.Add(20 * time.Minute), Cwd: row.WorktreePath, CwdSupported: true}, true,
		},
		"a little before, within clock skew": {
			ProcessInfo{StartTime: created.Add(-time.Second), Cwd: row.WorktreePath, CwdSupported: true}, true,
		},
		"long before the preview": {
			ProcessInfo{StartTime: created.Add(-time.Hour), Cwd: row.WorktreePath, CwdSupported: true}, false,
		},
		"long after the preview": {
			ProcessInfo{StartTime: created.Add(time.Hour), Cwd: row.WorktreePath, CwdSupported: true}, false,
		},
		"unknown start time": {
			ProcessInfo{Cwd: row.WorktreePath, CwdSupported: true}, false,
		},
		"deleted checkout still counts": {
			ProcessInfo{StartTime: created, Cwd: row.WorktreePath + " (deleted)", CwdSupported: true}, true,
		},
		"unreadable cwd where the platform has one": {
			ProcessInfo{StartTime: created, CwdSupported: true}, false,
		},
		"a sibling preview's checkout": {
			ProcessInfo{StartTime: created, Cwd: "/anvil/.previews/Forge-bbb2", CwdSupported: true}, false,
		},
		"no cwd on this platform": {
			ProcessInfo{StartTime: created}, true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			evidence, got := previewOwnsProcess(row, tc.info)
			if got != tc.want {
				t.Fatalf("previewOwnsProcess = %v (%q), want %v", got, evidence, tc.want)
			}
			if got && evidence == "" {
				t.Error("an owned process came back without evidence")
			}
		})
	}
}

func TestPreviewOwnsProcessRejectsARowWithNoCreationTime(t *testing.T) {
	row := state.Preview{BeadID: "Forge-aaa1", WorktreePath: "/anvil/.previews/Forge-aaa1"}
	if _, ok := previewOwnsProcess(row, ProcessInfo{StartTime: time.Now()}); ok {
		t.Error("ownership was established against a row with no created_at")
	}
}
