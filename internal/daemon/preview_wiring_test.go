package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/kiln"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePreviewManager records what the daemon asked of the Kiln manager. Every
// field is guarded because the reaper runs on its own goroutine and the
// teardown handler stops previews from one too.
type fakePreviewManager struct {
	mu           sync.Mutex
	reconciled   int
	reaping      bool
	reaperDone   bool
	started      []kiln.StartOptions
	startCtxErrs []error
	stopped      []string
	touched      []string
	stopAllCalls int

	// envs is what List reports and what Start hands back, keyed by bead id.
	envs map[string]*kiln.Environment

	reconcileErr error
	startErr     error
	stopErr      error
	stopAllErr   error
	// reaperStarted closes the first time RunReaper is entered, so a test can
	// wait for the goroutine instead of sleeping.
	reaperStarted chan struct{}
}

func newFakePreviewManager() *fakePreviewManager {
	return &fakePreviewManager{reaperStarted: make(chan struct{})}
}

func (f *fakePreviewManager) Reconcile(context.Context) error {
	f.mu.Lock()
	f.reconciled++
	f.mu.Unlock()
	return f.reconcileErr
}

func (f *fakePreviewManager) RunReaper(ctx context.Context) {
	f.mu.Lock()
	if !f.reaping {
		f.reaping = true
		close(f.reaperStarted)
	}
	f.mu.Unlock()
	<-ctx.Done()
	f.mu.Lock()
	f.reaperDone = true
	f.mu.Unlock()
}

func (f *fakePreviewManager) Start(ctx context.Context, opts kiln.StartOptions) (*kiln.Environment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, opts)
	// Recorded so a caller can prove it handed us a context that outlives the
	// one it was invoked with (bellows cancels its poll context per cycle).
	f.startCtxErrs = append(f.startCtxErrs, ctx.Err())
	if f.startErr != nil {
		return nil, f.startErr
	}
	env := &kiln.Environment{
		BeadID: opts.BeadID,
		Anvil:  opts.Anvil,
		Branch: opts.Branch,
	}
	if f.envs == nil {
		f.envs = make(map[string]*kiln.Environment)
	}
	f.envs[opts.BeadID] = env
	return env, nil
}

func (f *fakePreviewManager) List() []*kiln.Environment {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*kiln.Environment, 0, len(f.envs))
	for _, env := range f.envs {
		out = append(out, env)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BeadID < out[j].BeadID })
	return out
}

func (f *fakePreviewManager) Get(beadID string) (*kiln.Environment, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	env, ok := f.envs[beadID]
	return env, ok
}

func (f *fakePreviewManager) Touch(beadID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, beadID)
}

// touchedBeads reports the beads whose idle clock was reset, in call order.
func (f *fakePreviewManager) touchedBeads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.touched...)
}

func (f *fakePreviewManager) startedOptions() []kiln.StartOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]kiln.StartOptions(nil), f.started...)
}

// startContextErrors reports ctx.Err() as observed at the start of each Start.
func (f *fakePreviewManager) startContextErrors() []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]error(nil), f.startCtxErrs...)
}

func (f *fakePreviewManager) Stop(_ context.Context, beadID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, beadID)
	if f.stopErr != nil {
		return f.stopErr
	}
	// A stopped preview leaves the registry, so a follow-up List/Get sees it
	// gone the way the real manager's does.
	delete(f.envs, beadID)
	return nil
}

func (f *fakePreviewManager) StopAll(context.Context) error {
	f.mu.Lock()
	f.stopAllCalls++
	f.mu.Unlock()
	return f.stopAllErr
}

func (f *fakePreviewManager) reconcileCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reconciled
}

func (f *fakePreviewManager) stoppedBeads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stopped...)
}

func (f *fakePreviewManager) reaperFinished() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reaperDone
}

func (f *fakePreviewManager) stopAllCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopAllCalls
}

// newPreviewDaemon builds a Daemon whose preview manager construction is faked,
// so the wiring is exercised without worktrees, ports or child processes. The
// returned int pointer counts how many times construction was attempted.
func newPreviewDaemon(t *testing.T, cfg *config.Config, mgr *fakePreviewManager) (*Daemon, *int) {
	t.Helper()
	builds := 0
	d := &Daemon{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d.cfg.Store(cfg)
	d.newPreviewManager = func(context.Context, *config.Config, map[string]string) (previewManager, error) {
		builds++
		if mgr == nil {
			return nil, errors.New("no manager")
		}
		return mgr, nil
	}
	return d, &builds
}

func previewConfig(global bool, perAnvil *bool) *config.Config {
	return &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"forge": {Path: "/tmp/forge", PreviewEnabled: perAnvil},
		},
		Settings: config.SettingsConfig{PreviewEnabled: global},
	}
}

func boolPtr(b bool) *bool { return &b }

// TestPreviewAnvils_Gating covers the tri-state resolution that decides whether
// the manager is built at all: the global gate is mandatory, and an anvil
// without an explicit override inherits it.
func TestPreviewAnvils_Gating(t *testing.T) {
	tests := []struct {
		name     string
		global   bool
		perAnvil *bool
		want     bool
	}{
		{name: "global off, anvil unset", global: false, perAnvil: nil, want: false},
		{name: "global off, anvil opts in", global: false, perAnvil: boolPtr(true), want: false},
		{name: "global on, anvil unset inherits", global: true, perAnvil: nil, want: true},
		{name: "global on, anvil opts in", global: true, perAnvil: boolPtr(true), want: true},
		{name: "global on, anvil opts out", global: true, perAnvil: boolPtr(false), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			anvils := previewAnvils(previewConfig(tc.global, tc.perAnvil))
			if tc.want {
				require.Equal(t, map[string]string{"forge": "/tmp/forge"}, anvils)
				return
			}
			require.Empty(t, anvils)
		})
	}
}

// TestPreviewAnvils_SkipsPathlessAnvils keeps an anvil with no checkout out of
// the map the manager reconciles against — there is no directory to scan.
func TestPreviewAnvils_SkipsPathlessAnvils(t *testing.T) {
	cfg := &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"forge":  {Path: "/tmp/forge"},
			"broken": {Path: ""},
		},
		Settings: config.SettingsConfig{PreviewEnabled: true},
	}
	require.Equal(t, map[string]string{"forge": "/tmp/forge"}, previewAnvils(cfg))
}

// TestPreviewableAnvils_RequiresAManifest covers the extra gate the list
// endpoint applies on top of previewAnvils: an anvil previews are enabled for
// but which declares no `.forge/preview.yaml` could only ever answer a start
// with "no preview manifest", so it must not be advertised to clients.
func TestPreviewableAnvils_RequiresAManifest(t *testing.T) {
	withManifest := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(withManifest, ".forge"), 0o755))
	require.NoError(t, os.WriteFile(kiln.ManifestPath(withManifest), []byte("services:\n  web:\n    command: run\n"), 0o644))

	cfg := &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"withManifest": {Path: withManifest},
			"noManifest":   {Path: t.TempDir()},
			"optedOut":     {Path: withManifest, PreviewEnabled: boolPtr(false)},
		},
		Settings: config.SettingsConfig{PreviewEnabled: true},
	}
	require.Equal(t, []string{"withManifest"}, previewableAnvils(cfg))
}

// TestPreviewableAnvils_EmptyWhenPreviewsDisabled keeps the field an empty list
// rather than nil, so the JSON stays an array the SPA can index.
func TestPreviewableAnvils_EmptyWhenPreviewsDisabled(t *testing.T) {
	require.Empty(t, previewableAnvils(previewConfig(false, nil)))
	require.Empty(t, previewableAnvils(nil))
}

// TestStartPreviews_DisabledGlobally leaves the manager nil, so every consumer
// degrades to "no previews" instead of dereferencing it.
func TestStartPreviews_DisabledGlobally(t *testing.T) {
	d, builds := newPreviewDaemon(t, previewConfig(false, nil), newFakePreviewManager())

	d.startPreviews(t.Context())

	assert.Zero(t, *builds, "a disabled Kiln must not build a manager")
	assert.Nil(t, d.previews())
	assert.False(t, d.previewsEnabled())
}

// TestStartPreviews_DisabledByAnvilTriState covers the global-on/anvil-off case:
// the only configured anvil opts out, so there is nothing to preview.
func TestStartPreviews_DisabledByAnvilTriState(t *testing.T) {
	d, builds := newPreviewDaemon(t, previewConfig(true, boolPtr(false)), newFakePreviewManager())

	d.startPreviews(t.Context())

	assert.Zero(t, *builds, "every anvil opting out must not build a manager")
	assert.Nil(t, d.previews())
}

// TestStartPreviews_EnabledReconcilesAndReaps is the happy path: the manager is
// built, startup reconciliation runs once before traffic, and the reaper is
// running under the daemon's context and waitgroup.
func TestStartPreviews_EnabledReconcilesAndReaps(t *testing.T) {
	for _, tc := range []struct {
		name     string
		perAnvil *bool
	}{
		{name: "anvil inherits the global value", perAnvil: nil},
		{name: "anvil opts in explicitly", perAnvil: boolPtr(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newFakePreviewManager()
			d, builds := newPreviewDaemon(t, previewConfig(true, tc.perAnvil), mgr)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			d.startPreviews(ctx)

			require.Equal(t, 1, *builds)
			require.Same(t, previewManager(mgr), d.previews())
			require.True(t, d.previewsEnabled())
			assert.Equal(t, 1, mgr.reconcileCount(), "reconciliation runs once, before traffic")

			select {
			case <-mgr.reaperStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("the idle reaper was never started")
			}

			// Cancelling the run context stops the reaper, and the daemon's
			// waitgroup covers it so shutdown does not race it.
			cancel()
			d.wg.Wait()
			assert.True(t, mgr.reaperFinished())
		})
	}
}

// TestStartPreviews_ConstructionFailureLeavesManagerNil keeps a bad port range
// (or any other construction error) from taking the daemon down with it.
func TestStartPreviews_ConstructionFailureLeavesManagerNil(t *testing.T) {
	d, builds := newPreviewDaemon(t, previewConfig(true, nil), nil)

	d.startPreviews(t.Context())

	assert.Equal(t, 1, *builds)
	assert.Nil(t, d.previews())
}

// TestStartPreviews_ReconcileFailureStillStartsReaper — a stale row that will
// not clear is housekeeping, not a reason to run without an idle reaper.
func TestStartPreviews_ReconcileFailureStillStartsReaper(t *testing.T) {
	mgr := newFakePreviewManager()
	mgr.reconcileErr = errors.New("dolt is having a day")
	d, _ := newPreviewDaemon(t, previewConfig(true, nil), mgr)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	d.startPreviews(ctx)

	require.NotNil(t, d.previews())
	select {
	case <-mgr.reaperStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the idle reaper was never started")
	}
	cancel()
	d.wg.Wait()
}

// TestStopPreviews_TearsDownEverythingLive covers shutdown: cancelling the run
// context stops the reaper but not the previews, so StopAll must still be
// called — with a context that survives the cancellation.
func TestStopPreviews_TearsDownEverythingLive(t *testing.T) {
	mgr := newFakePreviewManager()
	d, _ := newPreviewDaemon(t, previewConfig(true, nil), mgr)
	ctx, cancel := context.WithCancel(t.Context())
	d.startPreviews(ctx)
	cancel()
	d.wg.Wait()

	d.stopPreviews(ctx)

	assert.Equal(t, 1, mgr.stopAllCount())
}

// TestStopPreviews_NilManagerIsNoOp — shutdown with previews disabled must not
// panic.
func TestStopPreviews_NilManagerIsNoOp(t *testing.T) {
	d, _ := newPreviewDaemon(t, previewConfig(false, nil), newFakePreviewManager())
	assert.NotPanics(t, func() { d.stopPreviews(t.Context()) })
}

// TestHandlePreviewTeardownOnPRClose covers the auto-teardown handler: a PR that
// reaches a terminal state releases its preview, and nothing else does.
func TestHandlePreviewTeardownOnPRClose(t *testing.T) {
	tests := []struct {
		name      string
		event     bellows.PREvent
		wantStops []string
	}{
		{
			name:      "merged PR tears the preview down",
			event:     bellows.PREvent{EventType: bellows.EventPRMerged, BeadID: "Forge-abc1", Anvil: "forge", PRNumber: 7},
			wantStops: []string{"Forge-abc1"},
		},
		{
			name:      "closed PR tears the preview down",
			event:     bellows.PREvent{EventType: bellows.EventPRClosed, BeadID: "Forge-abc1", Anvil: "forge", PRNumber: 7},
			wantStops: []string{"Forge-abc1"},
		},
		{
			name:  "a non-terminal event leaves the preview running",
			event: bellows.PREvent{EventType: bellows.EventPRReadyToMerge, BeadID: "Forge-abc1", Anvil: "forge", PRNumber: 7},
		},
		{
			name:  "an event with no bead is ignored",
			event: bellows.PREvent{EventType: bellows.EventPRMerged, Anvil: "forge", PRNumber: 7},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newFakePreviewManager()
			d, _ := newPreviewDaemon(t, previewConfig(true, nil), mgr)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			d.startPreviews(ctx)

			d.handlePreviewTeardownOnPRClose(ctx, tc.event)

			if len(tc.wantStops) == 0 {
				// Nothing to wait for; give the (unexpected) goroutine a chance
				// to run before asserting it never happened.
				time.Sleep(50 * time.Millisecond)
				assert.Empty(t, mgr.stoppedBeads())
				return
			}
			require.Eventually(t, func() bool {
				return len(mgr.stoppedBeads()) == len(tc.wantStops)
			}, 2*time.Second, 10*time.Millisecond)
			assert.Equal(t, tc.wantStops, mgr.stoppedBeads())
		})
	}
}

// TestHandlePreviewTeardownOnPRClose_BeadWithoutPreview — Stop is a no-op for a
// bead that has no preview, and the handler must not escalate the resulting
// error into anything the caller sees.
func TestHandlePreviewTeardownOnPRClose_BeadWithoutPreview(t *testing.T) {
	mgr := newFakePreviewManager()
	d, _ := newPreviewDaemon(t, previewConfig(true, nil), mgr)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d.startPreviews(ctx)

	assert.NotPanics(t, func() {
		d.handlePreviewTeardownOnPRClose(ctx, bellows.PREvent{
			EventType: bellows.EventPRMerged, BeadID: "Forge-none", Anvil: "forge", PRNumber: 9,
		})
	})
	require.Eventually(t, func() bool {
		return len(mgr.stoppedBeads()) == 1
	}, 2*time.Second, 10*time.Millisecond)
}

// TestHandlePreviewTeardownOnPRClose_PreviewsDisabled — the handler is
// registered unconditionally, so it has to survive a nil manager.
func TestHandlePreviewTeardownOnPRClose_PreviewsDisabled(t *testing.T) {
	d, _ := newPreviewDaemon(t, previewConfig(false, nil), newFakePreviewManager())
	d.startPreviews(t.Context())
	require.Nil(t, d.previews())

	assert.NotPanics(t, func() {
		d.handlePreviewTeardownOnPRClose(t.Context(), bellows.PREvent{
			EventType: bellows.EventPRMerged, BeadID: "Forge-abc1", Anvil: "forge", PRNumber: 7,
		})
	})
}

// TestHandlePreviewTeardownOnPRClose_TeardownErrorIsContained — a teardown that
// fails is logged, not propagated into the bellows handler chain.
func TestHandlePreviewTeardownOnPRClose_TeardownErrorIsContained(t *testing.T) {
	mgr := newFakePreviewManager()
	mgr.stopErr = errors.New("teardown script exited 1")
	d, _ := newPreviewDaemon(t, previewConfig(true, nil), mgr)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d.startPreviews(ctx)

	d.handlePreviewTeardownOnPRClose(ctx, bellows.PREvent{
		EventType: bellows.EventPRMerged, BeadID: "Forge-abc1", Anvil: "forge", PRNumber: 7,
	})

	require.Eventually(t, func() bool {
		return len(mgr.stoppedBeads()) == 1
	}, 2*time.Second, 10*time.Millisecond)
}

// TestPreviewBeadForAnvil covers the depcheck.PreviewLivenessFunc the daemon
// injects: it names a bead only when that anvil actually has a live preview, so
// depcheck skips its `npm ci` exactly then and never otherwise.
func TestPreviewBeadForAnvil(t *testing.T) {
	mgr := newFakePreviewManager()
	_, err := mgr.Start(t.Context(), kiln.StartOptions{
		BeadID: "Forge-prev", Anvil: "heimdall", Branch: "forge/Forge-prev",
	})
	require.NoError(t, err)
	d := newPreviewAPIDaemon(t, previewConfig(true, nil), mgr)

	assert.Equal(t, "Forge-prev", d.previewBeadForAnvil("heimdall"))
	assert.Empty(t, d.previewBeadForAnvil("forge"), "another anvil's preview must not block this one")
	assert.Empty(t, d.previewBeadForAnvil(""))
}

// TestPreviewBeadForAnvil_PreviewsDisabled verifies that a daemon without a
// Kiln manager reports no holder, leaving depcheck's behaviour unchanged.
func TestPreviewBeadForAnvil_PreviewsDisabled(t *testing.T) {
	d := newPreviewAPIDaemon(t, previewConfig(false, nil), nil)
	assert.Empty(t, d.previewBeadForAnvil("heimdall"))
}
