package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/kiln"
)

// previewStopTimeout bounds the teardown of all live previews on shutdown, so a
// wedged teardown script cannot hold the daemon open indefinitely.
const previewStopTimeout = 2 * time.Minute

// previewManager is the slice of *kiln.Manager the daemon drives. It is an
// interface rather than the concrete type so the wiring can be exercised
// without real worktrees, ports or child processes.
type previewManager interface {
	// Reconcile clears previews left behind by a previous daemon lifetime.
	Reconcile(ctx context.Context) error
	// RunReaper tears down idle previews until ctx is cancelled.
	RunReaper(ctx context.Context)
	// Stop tears down one bead's preview; a bead without one is a no-op.
	Stop(ctx context.Context, beadID string) error
	// StopAll tears down every live preview.
	StopAll(ctx context.Context) error
}

// *kiln.Manager is what the daemon actually wires in, so keep the two in step
// at compile time.
var _ previewManager = (*kiln.Manager)(nil)

// previewAnvils returns anvil name → main checkout path for every configured
// anvil that previews are enabled for: the global preview_enabled gate must be
// on and the anvil must not have opted out via its own tri-state
// (nil/unset inherits the global value).
//
// The result doubles as the Kiln manager's anvil map, which is what startup
// reconciliation scans for abandoned <anvil>/.previews/ checkouts.
func previewAnvils(cfg *config.Config) map[string]string {
	if cfg == nil || !cfg.Settings.PreviewEnabled {
		return nil
	}
	enabled := make(map[string]string)
	for name, anvil := range cfg.Anvils {
		if anvil.Path == "" || !cfg.IsPreviewEnabledForAnvil(name) {
			continue
		}
		enabled[name] = anvil.Path
	}
	if len(enabled) == 0 {
		return nil
	}
	return enabled
}

// previews returns the live preview manager, or nil when previews are disabled.
// Every caller outside the startup path goes through it so a disabled Kiln
// degrades to "no previews" rather than a nil dereference.
func (d *Daemon) previews() previewManager {
	if d == nil {
		return nil
	}
	d.previewMu.Lock()
	defer d.previewMu.Unlock()
	return d.previewMgr
}

// previewsEnabled reports whether preview environments are running.
func (d *Daemon) previewsEnabled() bool { return d.previews() != nil }

// startPreviews constructs the Kiln manager when previews are enabled, clears
// anything a previous daemon lifetime left running, and starts the idle reaper
// under the daemon's run context. When previews are disabled it leaves
// d.previewMgr nil and returns without side effects.
//
// Reconciliation failures are logged rather than fatal: a stale preview row is
// housekeeping, not a reason to refuse to orchestrate.
func (d *Daemon) startPreviews(ctx context.Context) {
	cfg := d.config()
	anvils := previewAnvils(cfg)
	if len(anvils) == 0 {
		if cfg != nil && cfg.Settings.PreviewEnabled {
			d.logger.Info("kiln previews enabled but no anvil opts in (preview_enabled=false on every anvil); skipping preview manager")
		}
		return
	}

	build := d.newPreviewManager
	if build == nil {
		build = d.buildPreviewManager
	}
	mgr, err := build(ctx, cfg, anvils)
	if err != nil {
		d.logger.Error("failed to start Kiln preview manager; previews disabled", "error", err)
		return
	}
	if mgr == nil {
		return
	}

	d.previewMu.Lock()
	d.previewMgr = mgr
	d.previewMu.Unlock()

	// Before accepting traffic: a preview row from a crashed daemon still names
	// a port range, a worktree and a process group that nothing owns.
	if err := mgr.Reconcile(ctx); err != nil {
		d.logger.Error("Kiln startup reconciliation failed", "error", err)
	}

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		mgr.RunReaper(ctx)
	}()

	d.logger.Info("kiln preview manager started", "anvils", len(anvils),
		"idle_timeout", cfg.Settings.PreviewIdleTimeout,
		"max_concurrent", cfg.Settings.ResolvedPreviewMaxConcurrent())
}

// buildPreviewManager assembles the real Kiln manager: a port allocator over
// settings.preview_port_range, a runtime tied to the daemon's run context (so
// cancelling it kills every preview process), and the manager around them.
func (d *Daemon) buildPreviewManager(ctx context.Context, cfg *config.Config, anvils map[string]string) (previewManager, error) {
	lo, hi, err := cfg.Settings.PreviewPortRangeBounds()
	if err != nil {
		return nil, fmt.Errorf("preview port range: %w", err)
	}
	bindHost := cfg.Settings.ResolvedPreviewBindHost()
	publicHost := cfg.Settings.ResolvedPreviewPublicHost()

	ports, err := kiln.NewPortAllocator(bindHost, lo, hi)
	if err != nil {
		return nil, err
	}
	runtime, err := kiln.NewRuntime(kiln.RuntimeConfig{
		Store:      d.db,
		Ports:      ports,
		BindHost:   bindHost,
		PublicHost: publicHost,
		Lifetime:   ctx,
		Logger:     d.logger,
	})
	if err != nil {
		return nil, err
	}
	return kiln.NewManager(kiln.ManagerDeps{
		Runtime:   kiln.RuntimeRunner(runtime),
		Worktrees: kiln.GitWorktrees{},
		Store:     d.db,
		Logger:    d.logger,
		Config: kiln.ManagerConfig{
			MaxConcurrent: cfg.Settings.ResolvedPreviewMaxConcurrent(),
			IdleTimeout:   cfg.Settings.PreviewIdleTimeout,
			PublicHost:    publicHost,
			Anvils:        anvils,
		},
	})
}

// stopPreviews tears down every live preview during shutdown. The reaper stops
// on its own when the run context is cancelled, but the previews it was
// watching do not: their services are daemon-spawned process groups and their
// checkouts are git worktrees, so both leak unless they are stopped explicitly.
//
// It runs on a context detached from the (already cancelled) run context and
// bounded by previewStopTimeout, and is a no-op when previews are disabled.
func (d *Daemon) stopPreviews(ctx context.Context) {
	mgr := d.previews()
	if mgr == nil {
		return
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), previewStopTimeout)
	defer cancel()
	if err := mgr.StopAll(stopCtx); err != nil {
		d.logger.Warn("stopping preview environments on shutdown hit errors", "error", err)
	}
}

// handlePreviewTeardownOnPRClose tears a bead's preview down once its PR
// reaches a terminal state. A preview exists to review a branch; when the PR is
// merged or closed there is nothing left to review, and holding the worktree,
// the ports and one of the few concurrency slots only starves the next bead.
//
// It is a no-op when previews are disabled or the bead has no preview, and runs
// the teardown on its own goroutine: bellows invokes handlers synchronously, and
// a teardown script must not stall the poll cycle.
func (d *Daemon) handlePreviewTeardownOnPRClose(ctx context.Context, event bellows.PREvent) {
	if event.EventType != bellows.EventPRMerged && event.EventType != bellows.EventPRClosed {
		return
	}
	mgr := d.previews()
	if mgr == nil || event.BeadID == "" {
		return
	}
	beadID, eventType := event.BeadID, event.EventType
	go func() {
		// Detached from the bellows poll context, which is cancelled at the end
		// of the cycle and would abort a teardown mid-script.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), previewStopTimeout)
		defer cancel()
		if err := mgr.Stop(stopCtx, beadID); err != nil {
			d.logger.Error("tearing down preview after PR close failed",
				"bead", beadID, "event", eventType, "error", err)
		}
	}()
}
