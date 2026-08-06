package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/kiln"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// anvilWithManifest returns a temp dir carrying a minimal .forge/preview.yaml,
// which is the second gate (after the config) an auto-start has to pass.
func anvilWithManifest(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(path, ".forge"), 0o755))
	require.NoError(t, os.WriteFile(kiln.ManifestPath(path),
		[]byte("services:\n  web:\n    command: run\n"), 0o644))
	return path
}

// autoPreviewConfig builds a single-anvil config with previews enabled globally
// and the given per-anvil preview_auto mode.
func autoPreviewConfig(anvilPath, mode string) *config.Config {
	return &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"forge": {Path: anvilPath, PreviewAuto: mode},
		},
		Settings: config.SettingsConfig{PreviewEnabled: true},
	}
}

// readyEvent is the bellows event the auto-start reacts to.
func readyEvent() bellows.PREvent {
	return bellows.PREvent{
		EventType: bellows.EventPRReadyToMerge,
		BeadID:    "Forge-abc1",
		Anvil:     "forge",
		Branch:    "forge/Forge-abc1",
		PRNumber:  42,
	}
}

// startAutoPreviewDaemon wires a daemon with a fake Kiln manager and returns it
// ready to receive bellows events.
func startAutoPreviewDaemon(t *testing.T, cfg *config.Config) (*Daemon, *fakePreviewManager, context.Context) {
	t.Helper()
	mgr := newFakePreviewManager()
	d, _ := newPreviewDaemon(t, cfg, mgr)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	d.startPreviews(ctx)
	return d, mgr, ctx
}

// requireNoStart gives the handler's goroutine a window to do the wrong thing
// before declaring that it did not.
func requireNoStart(t *testing.T, mgr *fakePreviewManager) {
	t.Helper()
	assert.Never(t, func() bool {
		return len(mgr.startedOptions()) > 0
	}, 200*time.Millisecond, 10*time.Millisecond)
}

// TestHandlePreviewAutoStart_OptedInAnvilStarts is the happy path: an anvil
// with preview_auto: ready_to_merge and a manifest gets a preview for the
// branch named in the event.
func TestHandlePreviewAutoStart_OptedInAnvilStarts(t *testing.T) {
	anvilPath := anvilWithManifest(t)
	d, mgr, ctx := startAutoPreviewDaemon(t, autoPreviewConfig(anvilPath, config.PreviewAutoReadyToMerge))

	d.handlePreviewAutoStart(ctx, readyEvent())

	require.Eventually(t, func() bool {
		return len(mgr.startedOptions()) == 1
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, []kiln.StartOptions{{
		BeadID:    "Forge-abc1",
		Anvil:     "forge",
		AnvilPath: anvilPath,
		Branch:    "forge/Forge-abc1",
		// Nobody asked for this preview, so it never evicts one that somebody
		// did ask for — not even with settings.preview_evict_lru on.
		NoEvict: true,
	}}, mgr.startedOptions())
}

// TestHandlePreviewAutoStart_FallsBackToTheForgeBranch — an event without a
// head ref still resolves to the branch Forge would have created for the bead.
func TestHandlePreviewAutoStart_FallsBackToTheForgeBranch(t *testing.T) {
	anvilPath := anvilWithManifest(t)
	d, mgr, ctx := startAutoPreviewDaemon(t, autoPreviewConfig(anvilPath, config.PreviewAutoReadyToMerge))

	event := readyEvent()
	event.Branch = "  "
	d.handlePreviewAutoStart(ctx, event)

	require.Eventually(t, func() bool {
		return len(mgr.startedOptions()) == 1
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, "forge/Forge-abc1", mgr.startedOptions()[0].Branch)
}

// TestHandlePreviewAutoStart_OnlyOnTheReadyToMergeTransition — bellows emits
// EventPRReadyToMerge on the rising edge only, and this handler must not widen
// that by reacting to any other event on the same PR.
func TestHandlePreviewAutoStart_OnlyOnTheReadyToMergeTransition(t *testing.T) {
	anvilPath := anvilWithManifest(t)
	d, mgr, ctx := startAutoPreviewDaemon(t, autoPreviewConfig(anvilPath, config.PreviewAutoReadyToMerge))

	for _, other := range []string{
		bellows.EventCIPassed,
		bellows.EventReviewApproved,
		bellows.EventPRMerged,
		bellows.EventPRClosed,
		bellows.EventPRReviewNeeded,
	} {
		event := readyEvent()
		event.EventType = other
		d.handlePreviewAutoStart(ctx, event)
	}

	requireNoStart(t, mgr)

	// The real transition still starts exactly one preview.
	d.handlePreviewAutoStart(ctx, readyEvent())
	require.Eventually(t, func() bool {
		return len(mgr.startedOptions()) == 1
	}, 2*time.Second, 10*time.Millisecond)
}

// TestHandlePreviewAutoStart_RequiresTheOptIn keeps the feature opt-in: an
// anvil that says nothing (or explicitly says off) gets no preview, even though
// previews are enabled for it and it has a manifest.
func TestHandlePreviewAutoStart_RequiresTheOptIn(t *testing.T) {
	for _, mode := range []string{"", config.PreviewAutoOff} {
		t.Run("preview_auto="+mode, func(t *testing.T) {
			d, mgr, ctx := startAutoPreviewDaemon(t, autoPreviewConfig(anvilWithManifest(t), mode))

			d.handlePreviewAutoStart(ctx, readyEvent())

			requireNoStart(t, mgr)
		})
	}
}

// TestHandlePreviewAutoStart_RespectsThePerAnvilOptOut — preview_enabled: false
// on the anvil beats its own preview_auto. (The manager is still built here
// only because the fake construction hook ignores the anvil map; in production
// a lone opted-out anvil leaves it nil, which the disabled case covers.)
func TestHandlePreviewAutoStart_RespectsThePerAnvilOptOut(t *testing.T) {
	cfg := autoPreviewConfig(anvilWithManifest(t), config.PreviewAutoReadyToMerge)
	anvil := cfg.Anvils["forge"]
	anvil.PreviewEnabled = boolPtr(false)
	cfg.Anvils["forge"] = anvil
	d, mgr, ctx := startAutoPreviewDaemon(t, cfg)

	d.handlePreviewAutoStart(ctx, readyEvent())

	requireNoStart(t, mgr)
}

// TestHandlePreviewAutoStart_SkipsAnvilsWithoutAManifest — an anvil with no
// .forge/preview.yaml could only ever fail the start, so it is skipped before
// the manager is asked.
func TestHandlePreviewAutoStart_SkipsAnvilsWithoutAManifest(t *testing.T) {
	d, mgr, ctx := startAutoPreviewDaemon(t, autoPreviewConfig(t.TempDir(), config.PreviewAutoReadyToMerge))

	d.handlePreviewAutoStart(ctx, readyEvent())

	requireNoStart(t, mgr)
}

// TestHandlePreviewAutoStart_SkipsExternalPRs — an ext-* PR has a synthetic
// bead id and a branch Forge never created; there is nothing to check out.
func TestHandlePreviewAutoStart_SkipsExternalPRs(t *testing.T) {
	d, mgr, ctx := startAutoPreviewDaemon(t, autoPreviewConfig(anvilWithManifest(t), config.PreviewAutoReadyToMerge))

	event := readyEvent()
	event.BeadID = "ext-42"
	d.handlePreviewAutoStart(ctx, event)

	requireNoStart(t, mgr)
}

// TestHandlePreviewAutoStart_SkipsUnknownAnvils — an event for an anvil that is
// no longer configured resolves to nothing rather than starting a preview from
// an empty path.
func TestHandlePreviewAutoStart_SkipsUnknownAnvils(t *testing.T) {
	d, mgr, ctx := startAutoPreviewDaemon(t, autoPreviewConfig(anvilWithManifest(t), config.PreviewAutoReadyToMerge))

	event := readyEvent()
	event.Anvil = "gone"
	d.handlePreviewAutoStart(ctx, event)

	requireNoStart(t, mgr)
}

// TestHandlePreviewAutoStart_CapReachedIsSkippedSilently — hitting
// preview_max_concurrent is a logged skip, not an error surfaced anywhere: the
// preview was Forge's idea, so a full cap must not become an operator task.
func TestHandlePreviewAutoStart_CapReachedIsSkippedSilently(t *testing.T) {
	mgr := newFakePreviewManager()
	mgr.startErr = &kiln.TooManyPreviewsError{Running: 2, Limit: 2}
	d, _ := newPreviewDaemon(t, autoPreviewConfig(anvilWithManifest(t), config.PreviewAutoReadyToMerge), mgr)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d.startPreviews(ctx)

	assert.NotPanics(t, func() { d.handlePreviewAutoStart(ctx, readyEvent()) })

	require.Eventually(t, func() bool {
		return len(mgr.startedOptions()) == 1
	}, 2*time.Second, 10*time.Millisecond)
}

// TestHandlePreviewAutoStart_StartFailureIsContained — a start that fails for
// any other reason is logged and dropped; it must not propagate into the
// bellows handler chain.
func TestHandlePreviewAutoStart_StartFailureIsContained(t *testing.T) {
	mgr := newFakePreviewManager()
	mgr.startErr = context.DeadlineExceeded
	d, _ := newPreviewDaemon(t, autoPreviewConfig(anvilWithManifest(t), config.PreviewAutoReadyToMerge), mgr)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d.startPreviews(ctx)

	assert.NotPanics(t, func() { d.handlePreviewAutoStart(ctx, readyEvent()) })

	require.Eventually(t, func() bool {
		return len(mgr.startedOptions()) == 1
	}, 2*time.Second, 10*time.Millisecond)
}

// TestHandlePreviewAutoStart_PreviewsDisabled — the handler is registered
// unconditionally, so a nil manager has to be a no-op rather than a panic.
func TestHandlePreviewAutoStart_PreviewsDisabled(t *testing.T) {
	cfg := autoPreviewConfig(anvilWithManifest(t), config.PreviewAutoReadyToMerge)
	cfg.Settings.PreviewEnabled = false
	d, mgr, ctx := startAutoPreviewDaemon(t, cfg)
	require.Nil(t, d.previews())

	assert.NotPanics(t, func() { d.handlePreviewAutoStart(ctx, readyEvent()) })
	requireNoStart(t, mgr)
}

// TestHandlePreviewAutoStart_SurvivesTheBellowsPollContext — bellows cancels the
// poll context at the end of a cycle; the start it triggered must outlive it,
// since a checkout plus a setup script takes minutes.
func TestHandlePreviewAutoStart_SurvivesTheBellowsPollContext(t *testing.T) {
	anvilPath := anvilWithManifest(t)
	mgr := newFakePreviewManager()
	d, _ := newPreviewDaemon(t, autoPreviewConfig(anvilPath, config.PreviewAutoReadyToMerge), mgr)
	runCtx, cancelRun := context.WithCancel(t.Context())
	defer cancelRun()
	d.startPreviews(runCtx)

	// Worst case for the handler: the cycle ended before its goroutine ran.
	pollCtx, cancelPoll := context.WithCancel(runCtx)
	cancelPoll()
	d.handlePreviewAutoStart(pollCtx, readyEvent())

	require.Eventually(t, func() bool {
		return len(mgr.startedOptions()) == 1
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, []error{nil}, mgr.startContextErrors(),
		"the start must run on a context detached from the bellows poll cycle")
}
