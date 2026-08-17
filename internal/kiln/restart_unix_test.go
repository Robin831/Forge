//go:build !windows

package kiln

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// restartRecorder collects both halves of a restart's visibility: the death
// that triggered it and the outcome it reached. A restart nobody is told about
// is the failure mode of the whole feature — it would put a service that died
// straight back to `healthy` with the death erased — so every test here asserts
// on what was announced, not only on what the record says.
type restartRecorder struct {
	mu       sync.Mutex
	exits    []ServiceExit
	restarts []ServiceRestart
}

func (r *restartRecorder) onExit(e ServiceExit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exits = append(r.exits, e)
}

func (r *restartRecorder) onRestart(s ServiceRestart) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restarts = append(r.restarts, s)
}

func (r *restartRecorder) snapshot() ([]ServiceExit, []ServiceRestart) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ServiceExit(nil), r.exits...), append([]ServiceRestart(nil), r.restarts...)
}

// wireRestarts attaches the recorder and collapses the backoff: the wait exists
// so a relaunch does not race the previous process off its port, which a test
// does not need to spend real seconds proving.
func wireRestarts(rt *Runtime) *restartRecorder {
	rec := &restartRecorder{}
	rt.onServiceExit = rec.onExit
	rt.onServiceRestart = rec.onRestart
	rt.restartBackoff = func(int) time.Duration { return 50 * time.Millisecond }
	return rec
}

// TestRuntimeRestartsAFlakyServiceOnFailure is the motivating case: a dev server
// that exits 1 once, silently, and works perfectly on the next run. With
// `restart: on-failure` the preview recovers on its own — same port, real
// readiness check, status back to running — and says so twice.
func TestRuntimeRestartsAFlakyServiceOnFailure(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()
	marker := filepath.Join(t.TempDir(), "died-once")
	store := &fakeStore{}
	rt := newTestRuntime(t, store)
	rec := wireRestarts(rt)

	manifest := mustParse(t, `
version: 1
services:
  client:
    command: `+strconv.Quote(helperCommand())+`
    env:
      KILN_HELPER_PORT: "{{.Port}}"
      KILN_HELPER_MODE: "tcp"
      KILN_HELPER_DIE_AFTER: "700ms"
      KILN_HELPER_EXIT_CODE: "1"
      KILN_HELPER_DIE_ONCE: `+strconv.Quote(marker)+`
    ready_timeout: 30s
    restart: on-failure
    max_restarts: 2
`)

	preview, err := rt.Start(context.Background(), StartRequest{
		BeadID:       "Forge-flaky",
		Anvil:        "forge",
		Branch:       "forge/Forge-flaky",
		WorktreePath: worktree,
		Manifest:     manifest,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = preview.Stop() })

	if preview.Status() != state.PreviewRunning {
		t.Fatalf("status right after start = %q, want %q", preview.Status(), state.PreviewRunning)
	}
	port := preview.Ports()[0]
	entryURL := preview.EntryURL()

	waitFor(t, 30*time.Second, "the flaky service to be restarted and healthy again", func() bool {
		svc, ok := preview.Record().Service("client")
		return ok && svc.Restarts == 1 && svc.Health == state.PreviewServiceHealthy
	})

	// The allocator reserved this port for the preview's whole life, so a
	// relaunch reuses it — a new port would silently invalidate every link and
	// every sibling that was told where this service listens.
	if got := preview.Ports()[0]; got != port {
		t.Errorf("port after restart = %d, want the originally allocated %d", got, port)
	}
	if got := preview.EntryURL(); got != entryURL {
		t.Errorf("EntryURL after restart = %q, want the original %q", got, entryURL)
	}
	if preview.Status() != state.PreviewRunning {
		t.Errorf("status after a successful restart = %q, want %q", preview.Status(), state.PreviewRunning)
	}

	// A relaunch re-runs the stored spec verbatim, so it lands on the same
	// deterministic log path the dead process wrote to. The supervisor opens it
	// O_APPEND for exactly this reason: truncating here would erase the last
	// output of the process whose death is the thing anyone would open the log
	// to explain.
	assertLogKeptBothLives(t, "Forge-flaky", "client")

	rec2 := preview.Record()
	client, _ := rec2.Service("client")
	if !client.ExitedAt.IsZero() || client.ExitCode != nil {
		t.Errorf("the previous life's exit was not cleared: %+v", client)
	}
	if client.Error != "" {
		t.Errorf("a healthy restarted service still carries an error: %q", client.Error)
	}
	if client.PID <= 0 {
		t.Errorf("restarted service has no PID: %+v", client)
	}
	if last := store.last(t); last.Status != state.PreviewRunning {
		t.Errorf("persisted status = %q, want %q", last.Status, state.PreviewRunning)
	}

	exits, restarts := rec.snapshot()
	if len(exits) != 1 {
		t.Fatalf("OnServiceExit fired %d times, want 1: %+v", len(exits), exits)
	}
	// The death is reported even though it was recovered from: the window
	// between it and the relaunch is a window in which nothing was serving.
	if !exits[0].Restarting || exits[0].Restarts != 1 || exits[0].MaxRestarts != 2 {
		t.Errorf("exit did not announce the pending restart: %+v", exits[0])
	}
	if len(restarts) != 1 {
		t.Fatalf("OnServiceRestart fired %d times, want 1: %+v", len(restarts), restarts)
	}
	got := restarts[0]
	if got.Service != "client" || got.BeadID != "Forge-flaky" || !got.Entry {
		t.Errorf("restart reported the wrong service: %+v", got)
	}
	if got.Attempt != 1 || got.MaxRestarts != 2 || got.Err != nil || got.Exhausted {
		t.Errorf("restart report wrong: %+v", got)
	}
	if got.Health != state.PreviewServiceHealthy || got.Status != state.PreviewRunning {
		t.Errorf("restart settled at %q/%q, want healthy/running", got.Health, got.Status)
	}
	if got.Detail != "restarted (attempt 1 of 2): healthy" {
		t.Errorf("detail = %q, want it to name the attempt and the outcome", got.Detail)
	}
}

// assertLogKeptBothLives checks that a restarted service's log still holds the
// banner StartService writes for every life, i.e. that the relaunch appended to
// the dead process's log rather than truncating it.
func assertLogKeptBothLives(t *testing.T, beadID, service string) {
	t.Helper()
	path, err := ServiceLogPath(beadID, service)
	if err != nil {
		t.Fatalf("ServiceLogPath: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if got := strings.Count(string(data), "service "+strconv.Quote(service)+" started"); got != 2 {
		t.Errorf("service log holds %d start banners, want 2 — the restart truncated the previous life's output:\n%s",
			got, data)
	}
}

// A relaunch that does not come back is the terminal branch of the policy, and
// it is terminal for a reason that has nothing to do with the budget: a service
// which spawns and then fails its readiness check is not the flakiness this
// exists to absorb, so it settles at `failed`, reports Exhausted with attempts
// still in hand, and — the property that keeps "terminal" true — is not watched
// again, so the still-running unready process's own death cannot claim another.
func TestRuntimeDoesNotRetryARelaunchThatNeverComesBack(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()
	marker := filepath.Join(t.TempDir(), "first-life")
	store := &fakeStore{}
	rt := newTestRuntime(t, store)
	rec := wireRestarts(rt)

	// The first life serves and then exits 1; every later one never binds the
	// port at all, so the relaunch spawns and its readiness check times out.
	manifest := mustParse(t, `
version: 1
services:
  client:
    command: `+strconv.Quote(helperCommand())+`
    env:
      KILN_HELPER_PORT: "{{.Port}}"
      KILN_HELPER_MODE: "tcp"
      KILN_HELPER_DIE_AFTER: "500ms"
      KILN_HELPER_EXIT_CODE: "1"
      KILN_HELPER_DIE_ONCE: `+strconv.Quote(marker)+`
      KILN_HELPER_SULK_ONCE: `+strconv.Quote(marker)+`
      KILN_HELPER_SULK_FOR: "2s"
    ready_timeout: 1s
    restart: on-failure
    max_restarts: 3
`)

	preview, err := rt.Start(context.Background(), StartRequest{
		BeadID:       "Forge-norecover",
		Anvil:        "forge",
		Branch:       "forge/Forge-norecover",
		WorktreePath: worktree,
		Manifest:     manifest,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = preview.Stop() })

	waitFor(t, 30*time.Second, "the relaunch to fail its readiness check", func() bool {
		_, restarts := rec.snapshot()
		return len(restarts) == 1
	})

	got := restarts1(t, rec)
	if got.Err == nil {
		t.Errorf("a relaunch that never became healthy reported no error: %+v", got)
	}
	if got.Health != state.PreviewServiceFailed {
		t.Errorf("health = %q, want %q — the service did not come up, which is what failed means everywhere else",
			got.Health, state.PreviewServiceFailed)
	}
	// Exhausted is about the outcome, not the budget: two attempts are left and
	// neither will be taken.
	if !got.Exhausted {
		t.Errorf("a relaunch that did not come back was not reported as terminal: %+v", got)
	}
	if got.Attempt != 1 || got.MaxRestarts != 3 {
		t.Errorf("restart report wrong: %+v", got)
	}
	if got.Status != state.PreviewFailed {
		t.Errorf("preview status = %q, want %q with nothing serving", got.Status, state.PreviewFailed)
	}

	svc, _ := preview.Record().Service("client")
	if svc.Health != state.PreviewServiceFailed {
		t.Errorf("record health = %q, want %q — not exited, and not left at starting",
			svc.Health, state.PreviewServiceFailed)
	}
	if svc.Error == "" {
		t.Error("a failed relaunch left no reason on the record")
	}
	if svc.Restarts != 1 {
		t.Errorf("restarts = %d, want 1", svc.Restarts)
	}
	if preview.EntryURL() != "" {
		t.Error("an entry URL was handed out for a service that never came back")
	}

	// The unready process is still alive at this point and dies on its own two
	// seconds in. Nothing may follow it: a re-armed watcher would consume that
	// death, claim attempt 2 and loop straight past the terminal outcome above.
	time.Sleep(2500 * time.Millisecond)
	exits, restarts := rec.snapshot()
	if len(exits) != 1 {
		t.Errorf("OnServiceExit fired %d times, want 1 — the failed relaunch's death was watched: %+v",
			len(exits), exits)
	}
	if len(restarts) != 1 {
		t.Errorf("OnServiceRestart fired %d times, want 1 — a terminal relaunch was retried: %+v",
			len(restarts), restarts)
	}
	if last := store.last(t); last.Status != state.PreviewFailed {
		t.Errorf("persisted status = %q, want %q", last.Status, state.PreviewFailed)
	}
}

// restarts1 returns the single recorded restart, failing the test if there is
// not exactly one.
func restarts1(t *testing.T, rec *restartRecorder) ServiceRestart {
	t.Helper()
	_, restarts := rec.snapshot()
	if len(restarts) != 1 {
		t.Fatalf("OnServiceRestart fired %d times, want 1: %+v", len(restarts), restarts)
	}
	return restarts[0]
}

// The default policy is unchanged, which is the regression guard for Forge-bci1:
// a service that dies without opting in goes to `exited` and stays there.
func TestRuntimeDoesNotRestartWithoutOptIn(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()
	store := &fakeStore{}
	rt := newTestRuntime(t, store)
	rec := wireRestarts(rt)

	manifest := mustParse(t, `
version: 1
services:
  client:
    command: `+strconv.Quote(helperCommand())+`
    env:
      KILN_HELPER_PORT: "{{.Port}}"
      KILN_HELPER_MODE: "tcp"
      KILN_HELPER_DIE_AFTER: "500ms"
      KILN_HELPER_EXIT_CODE: "1"
    ready_timeout: 30s
`)

	preview, err := rt.Start(context.Background(), StartRequest{
		BeadID:       "Forge-nooptin",
		WorktreePath: worktree,
		Manifest:     manifest,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = preview.Stop() })

	waitFor(t, 20*time.Second, "the service to be demoted", func() bool {
		svc, ok := preview.Record().Service("client")
		return ok && svc.Health == state.PreviewServiceExited
	})
	// Long enough for a relaunch to have happened if one were coming.
	time.Sleep(time.Second)

	svc, _ := preview.Record().Service("client")
	if svc.Health != state.PreviewServiceExited || svc.Restarts != 0 {
		t.Errorf("service = %+v, want it left exited with no restarts", svc)
	}
	exits, restarts := rec.snapshot()
	if len(restarts) != 0 {
		t.Errorf("a service on the default policy was restarted: %+v", restarts)
	}
	if len(exits) != 1 || exits[0].Restarting || exits[0].MaxRestarts != 0 {
		t.Errorf("exit report wrong for the default policy: %+v", exits)
	}
}

// A clean exit is the service doing what it was told, so `on-failure` leaves it
// alone — otherwise a one-shot service would be relaunched forever for
// succeeding.
func TestRuntimeDoesNotRestartACleanExit(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()
	rt := newTestRuntime(t, &fakeStore{})
	rec := wireRestarts(rt)

	manifest := mustParse(t, `
version: 1
services:
  once:
    command: `+strconv.Quote(helperCommand())+`
    env:
      KILN_HELPER_PORT: "{{.Port}}"
      KILN_HELPER_MODE: "tcp"
      KILN_HELPER_DIE_AFTER: "500ms"
      KILN_HELPER_EXIT_CODE: "0"
    ready_timeout: 30s
    restart: on-failure
`)

	preview, err := rt.Start(context.Background(), StartRequest{
		BeadID:       "Forge-clean",
		WorktreePath: worktree,
		Manifest:     manifest,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = preview.Stop() })

	waitFor(t, 20*time.Second, "the service to be demoted", func() bool {
		svc, ok := preview.Record().Service("once")
		return ok && svc.Health == state.PreviewServiceExited
	})
	time.Sleep(time.Second)

	if _, restarts := rec.snapshot(); len(restarts) != 0 {
		t.Errorf("a clean exit was restarted: %+v", restarts)
	}
	svc, _ := preview.Record().Service("once")
	if svc.Restarts != 0 {
		t.Errorf("restarts = %d, want 0 for a clean exit", svc.Restarts)
	}
}

// A service that keeps dying spends its budget and is then left exited exactly
// as the default policy would leave it — the point of the bound. The exit that
// spends the last attempt says so, so an exhausted flapping service is
// distinguishable from a single clean death.
func TestRuntimeStopsRestartingWhenTheBudgetIsSpent(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()
	store := &fakeStore{}
	rt := newTestRuntime(t, store)
	rec := wireRestarts(rt)

	manifest := mustParse(t, `
version: 1
services:
  flapper:
    command: `+strconv.Quote(helperCommand())+`
    env:
      KILN_HELPER_PORT: "{{.Port}}"
      KILN_HELPER_MODE: "tcp"
      KILN_HELPER_DIE_AFTER: "500ms"
      KILN_HELPER_EXIT_CODE: "2"
    ready_timeout: 30s
    restart: on-failure
    max_restarts: 1
`)

	preview, err := rt.Start(context.Background(), StartRequest{
		BeadID:       "Forge-flap",
		WorktreePath: worktree,
		Manifest:     manifest,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = preview.Stop() })

	waitFor(t, 30*time.Second, "the restart budget to be spent", func() bool {
		exits, _ := rec.snapshot()
		return len(exits) >= 2
	})
	// Long enough for a third life, if the budget were not being honoured.
	time.Sleep(1500 * time.Millisecond)

	exits, restarts := rec.snapshot()
	if len(exits) != 2 {
		t.Fatalf("OnServiceExit fired %d times, want exactly 2: %+v", len(exits), exits)
	}
	if len(restarts) != 1 {
		t.Fatalf("OnServiceRestart fired %d times, want exactly 1: %+v", len(restarts), restarts)
	}
	if !exits[0].Restarting {
		t.Errorf("the first death should have announced a restart: %+v", exits[0])
	}
	last := exits[1]
	if last.Restarting {
		t.Errorf("the second death restarted past the budget: %+v", last)
	}
	if last.Restarts != 1 || last.MaxRestarts != 1 {
		t.Errorf("the exhausted death did not carry its counts: %+v", last)
	}

	svc, _ := preview.Record().Service("flapper")
	if svc.Health != state.PreviewServiceExited {
		t.Errorf("health = %q, want %q once the budget is spent", svc.Health, state.PreviewServiceExited)
	}
	if svc.Restarts != 1 {
		t.Errorf("restarts = %d, want 1 — the count is what shows it flapped", svc.Restarts)
	}
	if preview.Status() != state.PreviewFailed {
		t.Errorf("status = %q, want %q with nothing left serving", preview.Status(), state.PreviewFailed)
	}
	if preview.EntryURL() != "" {
		t.Error("an entry URL was handed out for a service that is dead for good")
	}
}

// Teardown wins: a preview being stopped must not have a service spawned back
// into it, however recently one died. The backoff here is longer than the test
// takes, so the stop lands squarely inside the restart's waiting window.
func TestRuntimeDoesNotRestartDuringTeardown(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()
	store := &fakeStore{}
	rt := newTestRuntime(t, store)
	rec := wireRestarts(rt)
	rt.restartBackoff = func(int) time.Duration { return 10 * time.Second }

	manifest := mustParse(t, `
version: 1
services:
  client:
    command: `+strconv.Quote(helperCommand())+`
    env:
      KILN_HELPER_PORT: "{{.Port}}"
      KILN_HELPER_MODE: "tcp"
      KILN_HELPER_DIE_AFTER: "500ms"
      KILN_HELPER_EXIT_CODE: "1"
    ready_timeout: 30s
    restart: on-failure
    max_restarts: 3
`)

	preview, err := rt.Start(context.Background(), StartRequest{
		BeadID:       "Forge-teardown",
		WorktreePath: worktree,
		Manifest:     manifest,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, 20*time.Second, "the death that schedules a restart", func() bool {
		exits, _ := rec.snapshot()
		return len(exits) == 1
	})
	if err := preview.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	if _, restarts := rec.snapshot(); len(restarts) != 0 {
		t.Errorf("a service was restarted into a torn-down preview: %+v", restarts)
	}
	if preview.Status() != state.PreviewStopped {
		t.Errorf("status = %q, want %q", preview.Status(), state.PreviewStopped)
	}
	if last := store.last(t); last.Status != state.PreviewStopped {
		t.Errorf("persisted status = %q, want %q — a restart wrote over the teardown", last.Status, state.PreviewStopped)
	}
	if rt.ports.InUse() != 0 {
		t.Errorf("%d ports still reserved after a stop during a pending restart", rt.ports.InUse())
	}
}
