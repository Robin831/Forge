//go:build !windows

package kiln

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// TestHelperListener is not a test: it is the process a preview "service"
// starts in these tests. The real thing is an app server, which is impossible
// to depend on here, so the test binary re-executes itself and listens on the
// port Kiln allocated. It runs only when KILN_HELPER_PORT is set, which the
// manifest's service env does via {{.Port}}.
func TestHelperListener(t *testing.T) {
	port := os.Getenv("KILN_HELPER_PORT")
	if port == "" {
		t.Skip("helper process; only runs when spawned by a preview")
	}
	// KILN_HELPER_SULK_ONCE is the inverse of KILN_HELPER_DIE_ONCE and names the
	// same marker file: once the first life has created it, every later life
	// runs without ever binding the port, so a relaunch spawns fine and then
	// never passes its readiness check. That is the failed-relaunch case — the
	// one that must be terminal — and the sulking process stays alive past the
	// readiness timeout so its eventual death can prove no watcher was re-armed.
	if marker := os.Getenv("KILN_HELPER_SULK_ONCE"); marker != "" {
		if _, err := os.Stat(marker); err == nil {
			d := 3 * time.Second
			if raw := os.Getenv("KILN_HELPER_SULK_FOR"); raw != "" {
				if parsed, err := time.ParseDuration(raw); err == nil {
					d = parsed
				}
			}
			time.Sleep(d)
			os.Exit(3)
		}
	}

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "helper: listen on %s: %v\n", port, err)
		os.Exit(1)
	}
	defer ln.Close()

	// KILN_HELPER_DIE_AFTER makes the helper serve normally and then exit on its
	// own — the case this package had no way to exercise before: a service that
	// passes its readiness check and dies minutes later, which is exactly the
	// production incident (a vite dev server, exit 1, seven minutes in).
	if after := os.Getenv("KILN_HELPER_DIE_AFTER"); after != "" {
		d, err := time.ParseDuration(after)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper: bad KILN_HELPER_DIE_AFTER %q: %v\n", after, err)
			os.Exit(1)
		}
		code := 1
		if c := os.Getenv("KILN_HELPER_EXIT_CODE"); c != "" {
			if parsed, err := strconv.Atoi(c); err == nil {
				code = parsed
			}
		}
		// KILN_HELPER_DIE_ONCE names a marker file: the first process to run
		// dies, every later one serves normally. That "later one" is a relaunch
		// under `restart: on-failure`, so this is how the flaky-dev-server case
		// — dies once, comes back fine — is exercised without waiting on real
		// flakiness.
		die := true
		if marker := os.Getenv("KILN_HELPER_DIE_ONCE"); marker != "" {
			if _, err := os.Stat(marker); err == nil {
				die = false
			} else if f, err := os.Create(marker); err == nil {
				f.Close()
			}
		}
		if die {
			go func() {
				time.Sleep(d)
				os.Exit(code)
			}()
		}
	}

	if os.Getenv("KILN_HELPER_MODE") == "tcp" {
		// Port-open readiness: accept and drop connections forever.
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_ = http.Serve(ln, mux)
}

// helperCommand is the manifest command line that starts TestHelperListener.
func helperCommand() string {
	return "'" + os.Args[0] + "' -test.run=TestHelperListener"
}

type fakeStore struct {
	mu      sync.Mutex
	records []state.Preview
	err     error
}

func (f *fakeStore) UpsertPreview(p state.Preview) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.records = append(f.records, p)
	return nil
}

func (f *fakeStore) snapshots() []state.Preview {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]state.Preview(nil), f.records...)
}

func (f *fakeStore) last(t *testing.T) state.Preview {
	t.Helper()
	records := f.snapshots()
	if len(records) == 0 {
		t.Fatal("nothing was persisted")
	}
	return records[len(records)-1]
}

// newTestRuntime returns a runtime whose port range is free right now.
func newTestRuntime(t *testing.T, store Store) *Runtime {
	t.Helper()
	lo := freePort(t)
	ports, err := NewPortAllocator("127.0.0.1", lo, lo+50)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}
	rt, err := NewRuntime(RuntimeConfig{
		Store:       store,
		Ports:       ports,
		BindHost:    "127.0.0.1",
		StopTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return rt
}

func mustParse(t *testing.T, yaml string) *Manifest {
	t.Helper()
	m, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("parsing manifest: %v", err)
	}
	return m
}

func TestRuntimeStartsAndStopsAHealthyPreview(t *testing.T) {
	home := fakeHome(t)
	worktree := t.TempDir()
	store := &fakeStore{}
	rt := newTestRuntime(t, store)

	manifest := mustParse(t, `
version: 1
services:
  api:
    command: `+strconv.Quote(helperCommand())+`
    env:
      KILN_HELPER_PORT: "{{.Port}}"
    health: "/healthz"
    ready_timeout: 30s
`)

	preview, err := rt.Start(context.Background(), StartRequest{
		BeadID:       "Forge-ir70",
		Anvil:        "forge",
		AnvilPath:    "/anvil",
		Branch:       "forge/Forge-ir70",
		WorktreePath: worktree,
		Manifest:     manifest,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = preview.Stop() })

	if preview.Status() != state.PreviewRunning {
		t.Fatalf("status = %q, want %q (log: %s)", preview.Status(), state.PreviewRunning,
			readFile(t, filepath.Join(home, ".forge", "logs", "Forge-ir70", "preview-api.log")))
	}
	if preview.PreviewID != "forge_ir70" {
		t.Errorf("PreviewID = %q, want %q", preview.PreviewID, "forge_ir70")
	}
	ports := preview.Ports()
	if len(ports) != 1 || ports[0] == 0 {
		t.Fatalf("Ports = %v, want one allocated port", ports)
	}
	wantURL := fmt.Sprintf("http://127.0.0.1:%d/", ports[0])
	if preview.EntryURL() != wantURL {
		t.Errorf("EntryURL = %q, want %q", preview.EntryURL(), wantURL)
	}

	// The record must be written before the services start (so a crash mid-start
	// leaves a trace) and again once health is known.
	records := store.snapshots()
	if len(records) < 2 {
		t.Fatalf("got %d persisted snapshots, want at least 2", len(records))
	}
	if records[0].Status != state.PreviewStarting {
		t.Errorf("first snapshot status = %q, want %q", records[0].Status, state.PreviewStarting)
	}
	final := records[len(records)-1]
	if final.Status != state.PreviewRunning || final.BeadID != "Forge-ir70" ||
		final.Branch != "forge/Forge-ir70" || final.WorktreePath != worktree {
		t.Errorf("final snapshot wrong: %+v", final)
	}
	api, ok := final.Service("api")
	if !ok {
		t.Fatal("api service missing from the persisted record")
	}
	if api.Health != state.PreviewServiceHealthy {
		t.Errorf("api health = %q, want %q", api.Health, state.PreviewServiceHealthy)
	}
	if api.PID <= 0 || api.Port != ports[0] || !api.Entry {
		t.Errorf("api service record wrong: %+v", api)
	}
	if want := filepath.Join(home, ".forge", "logs", "Forge-ir70", "preview-api.log"); api.LogPath != want {
		t.Errorf("api log path = %q, want %q", api.LogPath, want)
	}
	if _, err := os.Stat(api.LogPath); err != nil {
		t.Errorf("service log not created: %v", err)
	}

	if err := preview.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if preview.Status() != state.PreviewStopped {
		t.Errorf("status after Stop = %q, want %q", preview.Status(), state.PreviewStopped)
	}
	if store.last(t).Status != state.PreviewStopped {
		t.Errorf("stopped status not persisted: %+v", store.last(t))
	}
	if rt.ports.InUse() != 0 {
		t.Errorf("%d ports still reserved after Stop", rt.ports.InUse())
	}
	// The port is genuinely free again.
	waitFor(t, 5*time.Second, "the released port to be bindable", func() bool {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", ports[0]))
		if err != nil {
			return false
		}
		ln.Close()
		return true
	})
	// Stopping twice is a no-op.
	if err := preview.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

func TestRuntimeFailedServiceLeavesSiblingsRunning(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()
	store := &fakeStore{}
	rt := newTestRuntime(t, store)

	manifest := mustParse(t, `
version: 1
services:
  api:
    command: `+strconv.Quote(helperCommand())+`
    env:
      KILN_HELPER_PORT: "{{.Port}}"
      KILN_HELPER_MODE: "tcp"
    ready_timeout: 30s
    entry: true
  broken:
    command: "exit 7"
    ready_timeout: 2s
`)

	preview, err := rt.Start(context.Background(), StartRequest{
		BeadID:       "Forge-mixed",
		Anvil:        "forge",
		Branch:       "forge/Forge-mixed",
		WorktreePath: worktree,
		Manifest:     manifest,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = preview.Stop() })

	if preview.Status() != state.PreviewDegraded {
		t.Errorf("status = %q, want %q", preview.Status(), state.PreviewDegraded)
	}
	record := store.last(t)
	api, _ := record.Service("api")
	if api.Health != state.PreviewServiceHealthy {
		t.Errorf("api health = %q, want %q — a failing sibling must not take it down", api.Health, state.PreviewServiceHealthy)
	}
	broken, _ := record.Service("broken")
	if broken.Health != state.PreviewServiceFailed {
		t.Errorf("broken health = %q, want %q", broken.Health, state.PreviewServiceFailed)
	}
	if broken.Error == "" {
		t.Error("failed service recorded without an explanation")
	}
	// The healthy service is still serving on its port.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", api.Port), 2*time.Second)
	if err != nil {
		t.Fatalf("healthy service is not reachable after its sibling failed: %v", err)
	}
	conn.Close()
}

// TestRuntimeDemotesAServiceThatExitsAfterReadiness is the regression for
// Forge-bci1: Kiln health used to be one-shot readiness, so a service that died
// after passing its check left the record, the previews payload and both front
// ends reporting `healthy` with a growing uptime over a dead process.
func TestRuntimeDemotesAServiceThatExitsAfterReadiness(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()
	store := &fakeStore{}
	rt := newTestRuntime(t, store)

	var (
		exitMu   sync.Mutex
		observed []ServiceExit
	)
	rt.onServiceExit = func(e ServiceExit) {
		exitMu.Lock()
		defer exitMu.Unlock()
		observed = append(observed, e)
	}

	manifest := mustParse(t, `
version: 1
services:
  client:
    command: `+strconv.Quote(helperCommand())+`
    env:
      KILN_HELPER_PORT: "{{.Port}}"
      KILN_HELPER_MODE: "tcp"
      KILN_HELPER_DIE_AFTER: "1500ms"
      KILN_HELPER_EXIT_CODE: "3"
    ready_timeout: 30s
    entry: true
  api:
    command: `+strconv.Quote(helperCommand())+`
    env:
      KILN_HELPER_PORT: "{{.Port}}"
      KILN_HELPER_MODE: "tcp"
    ready_timeout: 30s
`)

	preview, err := rt.Start(context.Background(), StartRequest{
		BeadID:       "Forge-exits",
		Anvil:        "forge",
		Branch:       "forge/Forge-exits",
		WorktreePath: worktree,
		Manifest:     manifest,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = preview.Stop() })

	// Readiness semantics are untouched: both services came up healthy.
	if preview.Status() != state.PreviewRunning {
		t.Fatalf("status right after start = %q, want %q", preview.Status(), state.PreviewRunning)
	}
	entryURL := preview.EntryURL()
	if entryURL == "" {
		t.Fatal("a healthy entry service produced no entry URL")
	}

	// One supervisor observation later, the death is everywhere.
	waitFor(t, 20*time.Second, "the exited service to be demoted", func() bool {
		svc, ok := preview.Record().Service("client")
		return ok && svc.Health == state.PreviewServiceExited
	})

	rec := preview.Record()
	client, _ := rec.Service("client")
	if client.ExitCode == nil || *client.ExitCode != 3 {
		t.Errorf("client exit code = %v, want 3", client.ExitCode)
	}
	if client.ExitedAt.IsZero() || client.StartedAt.IsZero() {
		t.Errorf("client timestamps not recorded: started=%v exited=%v", client.StartedAt, client.ExitedAt)
	}
	if !strings.Contains(client.Error, "exit 3") {
		t.Errorf("client error = %q, want it to name the exit status", client.Error)
	}

	// Uptime freezes: the lifetime is the same whether read now or later.
	frozen := client.Lifetime(time.Now())
	time.Sleep(250 * time.Millisecond)
	if again := client.Lifetime(time.Now()); again != frozen {
		t.Errorf("lifetime moved after the exit: %v then %v", frozen, again)
	}

	// The surviving sibling keeps the preview alive, by the same fold that ran
	// at startup: one service down out of two is degraded, not failed.
	if preview.Status() != state.PreviewDegraded {
		t.Errorf("status = %q, want %q", preview.Status(), state.PreviewDegraded)
	}
	if api, _ := rec.Service("api"); api.Health != state.PreviewServiceHealthy {
		t.Errorf("api health = %q, want %q — a dying sibling must not take it down",
			api.Health, state.PreviewServiceHealthy)
	}
	if got := preview.EntryURL(); got != "" {
		t.Errorf("EntryURL = %q, want it withheld once the entry service died", got)
	}
	if last := store.last(t); last.Status != state.PreviewDegraded {
		t.Errorf("persisted status = %q, want %q", last.Status, state.PreviewDegraded)
	}

	exitMu.Lock()
	defer exitMu.Unlock()
	if len(observed) != 1 {
		t.Fatalf("OnServiceExit fired %d times, want exactly 1: %+v", len(observed), observed)
	}
	got := observed[0]
	if got.Service != "client" || got.BeadID != "Forge-exits" || !got.Entry {
		t.Errorf("exit reported wrong service: %+v", got)
	}
	if got.ExitCode == nil || *got.ExitCode != 3 || got.Status != state.PreviewDegraded {
		t.Errorf("exit report wrong: %+v", got)
	}
	if !strings.Contains(got.Detail, "exit 3") || !strings.Contains(got.Detail, "lived") {
		t.Errorf("detail = %q, want it to name the code and the lifetime", got.Detail)
	}
}

// A service dying because Kiln killed it is the teardown working, not a
// failure: recording the exit is right, demoting the preview and announcing it
// is not.
func TestRuntimeDoesNotDemoteServicesKilledByStop(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()
	store := &fakeStore{}
	rt := newTestRuntime(t, store)

	var exits int32
	rt.onServiceExit = func(ServiceExit) { atomic.AddInt32(&exits, 1) }

	manifest := mustParse(t, `
version: 1
services:
  api:
    command: `+strconv.Quote(helperCommand())+`
    env:
      KILN_HELPER_PORT: "{{.Port}}"
      KILN_HELPER_MODE: "tcp"
    ready_timeout: 30s
`)
	preview, err := rt.Start(context.Background(), StartRequest{
		BeadID:       "Forge-stopped",
		WorktreePath: worktree,
		Manifest:     manifest,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := preview.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The watchers run on their own goroutines, so give a wrong demotion time to
	// land before concluding it did not.
	time.Sleep(500 * time.Millisecond)
	if got := atomic.LoadInt32(&exits); got != 0 {
		t.Errorf("OnServiceExit fired %d times for a teardown", got)
	}
	if preview.Status() != state.PreviewStopped {
		t.Errorf("status = %q, want %q", preview.Status(), state.PreviewStopped)
	}
	if last := store.last(t); last.Status != state.PreviewStopped {
		t.Errorf("persisted status = %q, want %q", last.Status, state.PreviewStopped)
	}
}

// The other half of the same suppression: on daemon shutdown it is the run
// context's cancellation that kills preview processes, and that can reach the
// watchers before Stop/StopAll has set `stopped`. A shutdown must be as quiet
// as a stop — no demotion, no persisted `exited` record, no feed event.
func TestRuntimeDoesNotDemoteServicesKilledByLifetimeCancel(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()
	store := &fakeStore{}

	lo := freePort(t)
	ports, err := NewPortAllocator("127.0.0.1", lo, lo+50)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}
	lifetime, shutdown := context.WithCancel(context.Background())
	defer shutdown()

	var exits int32
	rt, err := NewRuntime(RuntimeConfig{
		Store:         store,
		Ports:         ports,
		BindHost:      "127.0.0.1",
		StopTimeout:   2 * time.Second,
		Lifetime:      lifetime,
		OnServiceExit: func(ServiceExit) { atomic.AddInt32(&exits, 1) },
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}

	manifest := mustParse(t, `
version: 1
services:
  api:
    command: `+strconv.Quote(helperCommand())+`
    env:
      KILN_HELPER_PORT: "{{.Port}}"
      KILN_HELPER_MODE: "tcp"
    ready_timeout: 30s
`)
	preview, err := rt.Start(context.Background(), StartRequest{
		BeadID:       "Forge-shutdown",
		Anvil:        "forge",
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
	api, ok := preview.Record().Service("api")
	if !ok {
		t.Fatal("the manifest's api service is missing from the record")
	}

	// Daemon shutdown: cancel the lifetime and let the processes die. The port
	// closing is the proof the service is actually gone, so the assertions below
	// are about a death that happened rather than one that has yet to.
	shutdown()
	waitFor(t, 20*time.Second, "the service process to die", func() bool {
		conn, err := net.DialTimeout("tcp",
			net.JoinHostPort("127.0.0.1", strconv.Itoa(api.Port)), 200*time.Millisecond)
		if err != nil {
			return true
		}
		conn.Close()
		return false
	})

	// The watchers run on their own goroutines, so give a wrong demotion time to
	// land before concluding it did not.
	time.Sleep(500 * time.Millisecond)
	if got := atomic.LoadInt32(&exits); got != 0 {
		t.Errorf("OnServiceExit fired %d times for a shutdown", got)
	}
	if got := preview.Status(); got != state.PreviewRunning {
		t.Errorf("status = %q, want %q — a shutdown must not demote the preview", got, state.PreviewRunning)
	}
	if svc, _ := preview.Record().Service("api"); svc.Health != state.PreviewServiceHealthy {
		t.Errorf("api health = %q, want %q", svc.Health, state.PreviewServiceHealthy)
	}
	for _, rec := range store.snapshots() {
		if rec.Status != state.PreviewRunning && rec.Status != state.PreviewStarting {
			t.Errorf("persisted status = %q, want the shutdown to have written nothing new", rec.Status)
		}
	}
}

func TestRuntimeMarksEverythingFailedWhenNothingComesUp(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()
	store := &fakeStore{}
	rt := newTestRuntime(t, store)

	manifest := mustParse(t, `
version: 1
services:
  api:
    command: "exit 1"
    ready_timeout: 2s
`)
	preview, err := rt.Start(context.Background(), StartRequest{
		BeadID:       "Forge-dead",
		Anvil:        "forge",
		WorktreePath: worktree,
		Manifest:     manifest,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = preview.Stop() })

	if preview.Status() != state.PreviewFailed {
		t.Errorf("status = %q, want %q", preview.Status(), state.PreviewFailed)
	}
	if got := store.last(t).Status; got != state.PreviewFailed {
		t.Errorf("persisted status = %q, want %q", got, state.PreviewFailed)
	}
}

func TestRuntimeStartReleasesPortsWhenTheRangeIsTooSmall(t *testing.T) {
	fakeHome(t)
	lo := freePort(t)
	allocator, err := NewPortAllocator("127.0.0.1", lo, lo)
	if err != nil {
		t.Fatalf("NewPortAllocator: %v", err)
	}
	rt, err := NewRuntime(RuntimeConfig{Ports: allocator, BindHost: "127.0.0.1"})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}

	manifest := mustParse(t, `
version: 1
services:
  api:
    command: "true"
    entry: true
  client:
    command: "true"
`)
	if _, err := rt.Start(context.Background(), StartRequest{
		BeadID:       "Forge-noports",
		WorktreePath: t.TempDir(),
		Manifest:     manifest,
	}); err == nil {
		t.Fatal("Start succeeded with a one-port range and two services")
	}
	if allocator.InUse() != 0 {
		t.Errorf("%d ports leaked after a failed start", allocator.InUse())
	}
}

func TestRuntimeStartValidatesItsRequest(t *testing.T) {
	rt := newTestRuntime(t, nil)
	manifest := mustParse(t, "version: 1\nservices:\n  api:\n    command: \"true\"\n")

	tests := []struct {
		name string
		req  StartRequest
		want string
	}{
		{"no manifest", StartRequest{BeadID: "b", WorktreePath: "/tmp"}, "manifest"},
		{"no bead", StartRequest{Manifest: manifest, WorktreePath: "/tmp"}, "bead id"},
		{"no worktree", StartRequest{Manifest: manifest, BeadID: "b"}, "worktree"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rt.Start(context.Background(), tc.req)
			if err == nil {
				t.Fatal("Start accepted an incomplete request")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %v does not mention %q", err, tc.want)
			}
		})
	}
	if rt.ports.InUse() != 0 {
		t.Errorf("%d ports reserved by rejected requests", rt.ports.InUse())
	}
}

func TestRuntimeStartCleansUpOnContextCancel(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()
	rt := newTestRuntime(t, nil)

	// A service that starts but never becomes healthy, so the start is still
	// in its health phase when the caller gives up.
	manifest := mustParse(t, `
version: 1
services:
  api:
    command: "sleep 300"
    ready_timeout: 60s
`)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	preview, err := rt.Start(ctx, StartRequest{
		BeadID:       "Forge-cancelled",
		WorktreePath: worktree,
		Manifest:     manifest,
	})
	if err == nil {
		_ = preview.Stop()
		t.Fatal("Start returned success for a cancelled context")
	}
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Errorf("error = %v, want it to report the cancellation", err)
	}
	if rt.ports.InUse() != 0 {
		t.Errorf("%d ports still reserved after a cancelled start", rt.ports.InUse())
	}
}

func TestRuntimeRequiresAPortAllocator(t *testing.T) {
	if _, err := NewRuntime(RuntimeConfig{}); err == nil {
		t.Fatal("NewRuntime accepted a config without a port allocator")
	}
}
