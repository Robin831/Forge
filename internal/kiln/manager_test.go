package kiln

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// --- fakes ---------------------------------------------------------------

// fakeWorktrees records the detached checkouts the manager asks for. Each
// created preview gets a real (empty) directory so the manager's callers can
// assert on removal, and so a lifecycle command has somewhere to run.
type fakeWorktrees struct {
	root string

	mu        sync.Mutex
	created   []string
	removed   []string
	createErr error
	removeErr error
}

func (f *fakeWorktrees) CreateDetached(_ context.Context, anvilPath, beadID, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	path := filepath.Join(f.root, ".previews", beadID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", err
	}
	f.created = append(f.created, path)
	return path, nil
}

func (f *fakeWorktrees) RemoveDetached(_ context.Context, anvilPath, beadID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	path := filepath.Join(f.root, ".previews", beadID)
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	f.removed = append(f.removed, path)
	return nil
}

func (f *fakeWorktrees) createdPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...)
}

func (f *fakeWorktrees) removedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

// fakeRunner stands in for the process-spawning runtime. It runs the request's
// Setup callback (so the manager's setup wiring is exercised), writes the state
// row the real runtime writes before spawning, and returns a fakeInstance with
// whatever health the test asked for.
type fakeRunner struct {
	// store mirrors the real runtime's "persist before spawning" behaviour, so
	// the manager's row cleanup has something to clean up.
	store Store

	mu sync.Mutex
	// startErr, when set, fails the start before any service is "spawned".
	startErr error
	// status is the status the returned instance reports.
	status string
	// serviceErr is the per-service failure reported in the record.
	serviceErr string
	// skipSetup suppresses the Setup callback (a runtime that failed before
	// reaching it).
	skipSetup bool

	starts    []StartRequest
	instances []*fakeInstance
}

func (f *fakeRunner) Start(ctx context.Context, req StartRequest) (Instance, error) {
	f.mu.Lock()
	f.starts = append(f.starts, req)
	startErr, status, serviceErr, skipSetup := f.startErr, f.status, f.serviceErr, f.skipSetup
	store := f.store
	f.mu.Unlock()

	if store != nil {
		if err := store.UpsertPreview(state.Preview{
			BeadID:       req.BeadID,
			Anvil:        req.Anvil,
			Branch:       req.Branch,
			Status:       state.PreviewStarting,
			WorktreePath: req.WorktreePath,
			LastActiveAt: time.Now().Add(-time.Hour),
		}); err != nil {
			return nil, err
		}
	}

	if !skipSetup && req.Setup != nil {
		expanded, err := req.Manifest.Expand(Context{
			PreviewID: SanitizePreviewID(req.BeadID),
			Host:      "127.0.0.1",
			Ports:     fakePorts(req.Manifest),
		})
		if err != nil {
			return nil, err
		}
		penv := PreviewEnv{
			PreviewID:    SanitizePreviewID(req.BeadID),
			BeadID:       req.BeadID,
			WorktreePath: req.WorktreePath,
			Ports:        fakePorts(req.Manifest),
		}
		if err := req.Setup(ctx, expanded, penv); err != nil {
			return nil, err
		}
	}
	if startErr != nil {
		return nil, startErr
	}
	if status == "" {
		status = state.PreviewRunning
	}

	inst := &fakeInstance{
		beadID:     req.BeadID,
		anvil:      req.Anvil,
		branch:     req.Branch,
		worktree:   req.WorktreePath,
		status:     status,
		serviceErr: serviceErr,
		ports:      fakePorts(req.Manifest),
	}
	f.mu.Lock()
	f.instances = append(f.instances, inst)
	f.mu.Unlock()
	return inst, nil
}

func (f *fakeRunner) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

// fakePorts assigns deterministic ports so template expansion works.
func fakePorts(m *Manifest) map[string]int {
	ports := make(map[string]int, len(m.Services))
	for i, svc := range m.Services {
		ports[svc.Name] = 42000 + i
	}
	return ports
}

// fakeInstance is a started preview whose "processes" are just a stopped flag.
type fakeInstance struct {
	beadID     string
	anvil      string
	branch     string
	worktree   string
	status     string
	serviceErr string
	ports      map[string]int

	mu      sync.Mutex
	stops   int
	stopErr error
}

func (f *fakeInstance) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	f.status = state.PreviewStopped
	return f.stopErr
}

func (f *fakeInstance) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

func (f *fakeInstance) Status() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeInstance) EntryURL() string { return "http://127.0.0.1:42000/" }

func (f *fakeInstance) Ports() []int {
	out := make([]int, 0, len(f.ports))
	for _, p := range f.ports {
		out = append(out, p)
	}
	return out
}

func (f *fakeInstance) Record() state.Preview {
	rec := state.Preview{
		BeadID:       f.beadID,
		Anvil:        f.anvil,
		Branch:       f.branch,
		Status:       f.Status(),
		WorktreePath: f.worktree,
		CreatedAt:    time.Now(),
		LastActiveAt: time.Now(),
	}
	health := state.PreviewServiceHealthy
	if f.serviceErr != "" {
		health = state.PreviewServiceFailed
	}
	for name, port := range f.ports {
		rec.Services = append(rec.Services, state.PreviewService{
			Name: name, Port: port, Health: health, Error: f.serviceErr,
		})
	}
	return rec
}

// --- harness -------------------------------------------------------------

const testManifestYAML = `
version: 1
services:
  api:
    command: "run-api"
    health: /healthz
`

func testManifest(t *testing.T) *Manifest {
	t.Helper()
	m, err := Parse([]byte(testManifestYAML))
	if err != nil {
		t.Fatalf("parsing test manifest: %v", err)
	}
	return m
}

// testStore opens a real state.db in a temp dir: the manager's persistence
// contract is worth testing against the actual schema, not a map.
func testStore(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("opening state db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

type harness struct {
	mgr    *Manager
	runner *fakeRunner
	wts    *fakeWorktrees
	procs  *fakeProcesses
	store  *state.DB
	anvil  string
	// manifest is what LoadManifest returns; tests mutate it before starting.
	manifest *Manifest
	loadErr  error
}

func newHarness(t *testing.T, cfg ManagerConfig) *harness {
	t.Helper()
	anvil := t.TempDir()
	store := testStore(t)
	h := &harness{
		runner:   &fakeRunner{store: store},
		wts:      &fakeWorktrees{root: anvil},
		procs:    newFakeProcesses(),
		store:    store,
		anvil:    anvil,
		manifest: testManifest(t),
	}
	// Reconciliation scans the configured anvils; the harness has exactly one,
	// under the name h.opts() starts previews with.
	if cfg.Anvils == nil {
		cfg.Anvils = map[string]string{"forge": anvil}
	}
	// Preview logs land under the real ~/.forge/logs; point HOME at a temp dir
	// so a lifecycle command in a test cannot write into the operator's.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	mgr, err := NewManager(ManagerDeps{
		Runtime:   h.runner,
		Worktrees: h.wts,
		Store:     h.store,
		Processes: h.procs,
		Config:    cfg,
		LoadManifest: func(string) (*Manifest, error) {
			if h.loadErr != nil {
				return nil, h.loadErr
			}
			return h.manifest, nil
		},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	h.mgr = mgr
	return h
}

func (h *harness) opts(beadID string) StartOptions {
	return StartOptions{
		BeadID:    beadID,
		Anvil:     "forge",
		AnvilPath: h.anvil,
		Branch:    "forge/" + beadID,
	}
}

// --- tests ---------------------------------------------------------------

func TestManagerStartRegistersAndPersists(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})

	// The runtime writes the row with a stale last_active_at (see fakeRunner);
	// Start must bump it, because the idle reaper measures against it.
	env, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if env.BeadID != "Forge-aaa1" {
		t.Errorf("BeadID = %q, want Forge-aaa1", env.BeadID)
	}
	if env.Status() != state.PreviewRunning {
		t.Errorf("Status = %q, want %q", env.Status(), state.PreviewRunning)
	}
	if env.LastActive().IsZero() {
		t.Error("LastActive was never set")
	}
	if _, ok := h.mgr.Get("Forge-aaa1"); !ok {
		t.Error("preview missing from the registry after Start")
	}
	if got := len(h.mgr.List()); got != 1 {
		t.Errorf("List() returned %d previews, want 1", got)
	}
	if paths := h.wts.createdPaths(); len(paths) != 1 {
		t.Fatalf("CreateDetached called %d times, want 1", len(paths))
	}
	if _, err := os.Stat(env.WorktreePath); err != nil {
		t.Errorf("preview worktree missing: %v", err)
	}

	row, err := h.store.GetPreview("Forge-aaa1")
	if err != nil || row == nil {
		t.Fatalf("GetPreview: row=%v err=%v", row, err)
	}
	if !row.LastActiveAt.After(time.Now().Add(-time.Minute)) {
		t.Errorf("last_active_at = %s, want it bumped to ~now", row.LastActiveAt)
	}
}

func TestManagerStartRejectsOverCap(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 1})

	if _, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1")); err != nil {
		t.Fatalf("first Start: %v", err)
	}

	_, err := h.mgr.Start(context.Background(), h.opts("Forge-bbb2"))
	if err == nil {
		t.Fatal("second Start succeeded, want a cap rejection")
	}
	if !errors.Is(err, ErrTooManyPreviews) {
		t.Errorf("error %v does not match ErrTooManyPreviews", err)
	}
	var capErr *TooManyPreviewsError
	if !errors.As(err, &capErr) {
		t.Fatalf("error %v is not a *TooManyPreviewsError", err)
	}
	if capErr.Limit != 1 || capErr.Running != 1 {
		t.Errorf("cap error = %+v, want Running=1 Limit=1", capErr)
	}
	if !strings.Contains(err.Error(), "1") {
		t.Errorf("message %q does not mention the limit", err.Error())
	}

	// The rejected bead must leave nothing behind.
	if _, ok := h.mgr.Get("Forge-bbb2"); ok {
		t.Error("rejected bead ended up in the registry")
	}
	if len(h.wts.createdPaths()) != 1 {
		t.Errorf("CreateDetached ran for the rejected bead: %v", h.wts.createdPaths())
	}
}

func TestManagerStartDefaultsToConfiguredCap(t *testing.T) {
	h := newHarness(t, ManagerConfig{})
	if got := h.mgr.MaxConcurrent(); got != 2 {
		t.Errorf("MaxConcurrent = %d, want the default 2", got)
	}
}

func TestManagerStartRollsBackWhenEveryServiceFails(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	h.runner.status = state.PreviewFailed
	h.runner.serviceErr = "health check timed out"

	_, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1"))
	if err == nil {
		t.Fatal("Start succeeded despite every service failing")
	}
	if !strings.Contains(err.Error(), "health check timed out") {
		t.Errorf("error %q does not carry the per-service failure", err.Error())
	}

	// The instance the runtime handed back must have been stopped as part of
	// the unwind, so no process group survives the failure.
	if len(h.runner.instances) != 1 || h.runner.instances[0].stopCount() == 0 {
		t.Error("failed preview was not stopped during rollback")
	}
	assertFullyUnwound(t, h, "Forge-aaa1")
}

func TestManagerStartKeepsDegradedPreview(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	h.runner.status = state.PreviewDegraded

	env, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1"))
	if err != nil {
		t.Fatalf("Start of a degraded preview: %v", err)
	}
	if env.Status() != state.PreviewDegraded {
		t.Errorf("Status = %q, want %q", env.Status(), state.PreviewDegraded)
	}
	if _, ok := h.mgr.Get("Forge-aaa1"); !ok {
		t.Error("a degraded preview must stay registered — its healthy services still serve")
	}
}

func TestManagerStartRollsBackWhenRuntimeFails(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	h.runner.skipSetup = true
	h.runner.startErr = errors.New("ports exhausted")

	if _, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1")); err == nil {
		t.Fatal("Start succeeded despite a runtime failure")
	}
	assertFullyUnwound(t, h, "Forge-aaa1")
}

func TestManagerStartReleasesSlotAfterFailure(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 1})
	h.runner.skipSetup = true
	h.runner.startErr = errors.New("boom")

	if _, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1")); err == nil {
		t.Fatal("Start succeeded despite a runtime failure")
	}

	// A failed start must hand its reservation back, or the cap leaks a slot
	// per failure until no preview can ever start again.
	h.runner.startErr = nil
	if _, err := h.mgr.Start(context.Background(), h.opts("Forge-bbb2")); err != nil {
		t.Fatalf("Start after a failed start: %v", err)
	}
}

func TestManagerStartIsIdempotent(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})

	first, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1"))
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	second, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1"))
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}

	if first != second {
		t.Error("second Start returned a different preview")
	}
	if got := h.runner.startCount(); got != 1 {
		t.Errorf("runtime started %d times, want 1", got)
	}
	if got := len(h.wts.createdPaths()); got != 1 {
		t.Errorf("CreateDetached called %d times, want 1", got)
	}
}

func TestManagerStopTearsEverythingDown(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})

	env, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := h.mgr.Stop(context.Background(), "Forge-aaa1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if got := h.runner.instances[0].stopCount(); got != 1 {
		t.Errorf("instance stopped %d times, want 1", got)
	}
	if got := h.wts.removedPaths(); len(got) != 1 {
		t.Errorf("RemoveDetached called %d times, want 1", len(got))
	}
	if _, err := os.Stat(env.WorktreePath); !os.IsNotExist(err) {
		t.Errorf("preview worktree still present: %v", err)
	}
	if _, ok := h.mgr.Get("Forge-aaa1"); ok {
		t.Error("preview still in the registry after Stop")
	}
	row, err := h.store.GetPreview("Forge-aaa1")
	if err != nil {
		t.Fatalf("GetPreview: %v", err)
	}
	if row != nil {
		t.Errorf("preview row survived Stop: %+v", row)
	}
}

func TestManagerStopUnknownBeadIsNoOp(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	if err := h.mgr.Stop(context.Background(), "Forge-nope"); err != nil {
		t.Errorf("Stop of an unknown bead = %v, want nil", err)
	}
	if got := h.wts.removedPaths(); len(got) != 0 {
		t.Errorf("Stop of an unknown bead removed %v", got)
	}
}

func TestManagerStopIsIdempotent(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	if _, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.mgr.Stop(context.Background(), "Forge-aaa1"); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := h.mgr.Stop(context.Background(), "Forge-aaa1"); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if got := h.runner.instances[0].stopCount(); got != 1 {
		t.Errorf("instance stopped %d times, want 1", got)
	}
}

func TestManagerStopFreesACapSlot(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 1})
	if _, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := h.mgr.Stop(context.Background(), "Forge-aaa1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := h.mgr.Start(context.Background(), h.opts("Forge-bbb2")); err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
}

func TestManagerTouch(t *testing.T) {
	clock := time.Now()
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	h.mgr.now = func() time.Time { return clock }

	env, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !env.LastActive().Equal(clock) {
		t.Errorf("LastActive = %s, want %s", env.LastActive(), clock)
	}

	clock = clock.Add(20 * time.Minute)
	h.mgr.Touch("Forge-aaa1")
	if !env.LastActive().Equal(clock) {
		t.Errorf("LastActive after Touch = %s, want %s", env.LastActive(), clock)
	}

	// Touching a bead with no preview must not panic or create anything.
	h.mgr.Touch("Forge-nope")
}

func TestManagerStartValidatesOptions(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	cases := map[string]StartOptions{
		"no bead id": {AnvilPath: h.anvil, Branch: "b"},
		"no anvil":   {BeadID: "Forge-aaa1", Branch: "b"},
		"no branch":  {BeadID: "Forge-aaa1", AnvilPath: h.anvil},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := h.mgr.Start(context.Background(), opts); err == nil {
				t.Fatal("Start accepted incomplete options")
			}
		})
	}
}

func TestManagerStartPropagatesManifestError(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 2})
	h.loadErr = ErrNoManifest

	_, err := h.mgr.Start(context.Background(), h.opts("Forge-aaa1"))
	if !errors.Is(err, ErrNoManifest) {
		t.Fatalf("error = %v, want ErrNoManifest", err)
	}
	// A missing manifest must not have created a checkout to clean up.
	if got := h.wts.createdPaths(); len(got) != 0 {
		t.Errorf("CreateDetached ran without a manifest: %v", got)
	}
}

// TestManagerConcurrentStartStop is the race-detector case: many callers doing
// the two things the web layer lets them do, against the same bead and against
// different ones, with the cap in the middle.
func TestManagerConcurrentStartStop(t *testing.T) {
	h := newHarness(t, ManagerConfig{MaxConcurrent: 3})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		beadID := fmt.Sprintf("Forge-b%02d", i%3)
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := h.mgr.Start(context.Background(), h.opts(beadID)); err != nil &&
				!errors.Is(err, ErrTooManyPreviews) {
				t.Errorf("Start(%s): %v", beadID, err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := h.mgr.Stop(context.Background(), beadID); err != nil {
				t.Errorf("Stop(%s): %v", beadID, err)
			}
		}()
	}
	wg.Wait()

	// Whatever interleaving happened, the cap must still hold and every
	// registry entry must be a real, started preview.
	if got := len(h.mgr.List()); got > h.mgr.MaxConcurrent() {
		t.Errorf("%d previews registered, over the cap of %d", got, h.mgr.MaxConcurrent())
	}
	for _, env := range h.mgr.List() {
		if env.Status() != state.PreviewRunning {
			t.Errorf("preview %s left in status %q", env.BeadID, env.Status())
		}
	}
}

func TestNewManagerRequiresDeps(t *testing.T) {
	if _, err := NewManager(ManagerDeps{Worktrees: &fakeWorktrees{}}); err == nil {
		t.Error("NewManager accepted a nil runtime")
	}
	if _, err := NewManager(ManagerDeps{Runtime: &fakeRunner{}}); err == nil {
		t.Error("NewManager accepted a nil worktree provider")
	}
}

func TestManagerWorksWithoutAStore(t *testing.T) {
	anvil := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	mgr, err := NewManager(ManagerDeps{
		Runtime:      &fakeRunner{},
		Worktrees:    &fakeWorktrees{root: anvil},
		Config:       ManagerConfig{MaxConcurrent: 1},
		LoadManifest: func(string) (*Manifest, error) { return testManifest(t), nil },
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	opts := StartOptions{BeadID: "Forge-aaa1", Anvil: "forge", AnvilPath: anvil, Branch: "b"}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("Start without a store: %v", err)
	}
	mgr.Touch("Forge-aaa1")
	if err := mgr.Stop(context.Background(), "Forge-aaa1"); err != nil {
		t.Fatalf("Stop without a store: %v", err)
	}
}

// assertFullyUnwound checks that a failed start left no registry entry, no
// worktree, no state row and no leaked cap slot.
func assertFullyUnwound(t *testing.T, h *harness, beadID string) {
	t.Helper()
	if _, ok := h.mgr.Get(beadID); ok {
		t.Error("failed start left an entry in the registry")
	}
	created := h.wts.createdPaths()
	if len(created) != 1 {
		t.Fatalf("CreateDetached called %d times, want 1", len(created))
	}
	if got := h.wts.removedPaths(); len(got) != 1 {
		t.Errorf("RemoveDetached called %d times, want 1", len(got))
	}
	if _, err := os.Stat(created[0]); !os.IsNotExist(err) {
		t.Errorf("failed start left the worktree behind: %v", err)
	}
	row, err := h.store.GetPreview(beadID)
	if err != nil {
		t.Fatalf("GetPreview: %v", err)
	}
	if row != nil {
		t.Errorf("failed start left a state row behind: %+v", row)
	}
}
