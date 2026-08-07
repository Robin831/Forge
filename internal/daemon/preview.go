package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/kiln"
	"github.com/Robin831/Forge/internal/questgiver"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/worktree"
)

// previewStopTimeout bounds the teardown of all live previews on shutdown, so a
// wedged teardown script cannot hold the daemon open indefinitely.
const previewStopTimeout = 2 * time.Minute

// previewStartTimeout bounds one preview start. It has to cover the manifest's
// setup command, spawning every service and waiting out their health checks,
// which for a .NET restore + npm install is minutes rather than seconds.
const previewStartTimeout = 15 * time.Minute

// previewManager is the slice of *kiln.Manager the daemon drives. It is an
// interface rather than the concrete type so the wiring can be exercised
// without real worktrees, ports or child processes.
type previewManager interface {
	// Reconcile clears previews left behind by a previous daemon lifetime.
	Reconcile(ctx context.Context) error
	// RunReaper tears down idle previews until ctx is cancelled.
	RunReaper(ctx context.Context)
	// Start brings a bead's preview up, or returns the existing one.
	Start(ctx context.Context, opts kiln.StartOptions) (*kiln.Environment, error)
	// Stop tears down one bead's preview; a bead without one is a no-op.
	Stop(ctx context.Context, beadID string) error
	// StopAll tears down every live preview.
	StopAll(ctx context.Context) error
	// List returns every live preview, ordered by bead id.
	List() []*kiln.Environment
	// Get returns one bead's preview; the bool is false when it has none.
	Get(beadID string) (*kiln.Environment, bool)
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

// previewableAnvils returns the sorted names of the anvils a preview can
// actually be started for: previews are enabled for them (previewAnvils) and
// their main checkout declares a manifest.
//
// The manifest check is a stat per anvil rather than a cached flag: an operator
// who adds `.forge/preview.yaml` expects the Preview button to appear on the
// next poll, not after a daemon restart, and a handful of stats per list call
// is cheaper than the reload plumbing that would avoid them.
func previewableAnvils(cfg *config.Config) []string {
	enabled := previewAnvils(cfg)
	names := make([]string, 0, len(enabled))
	for name, path := range enabled {
		if kiln.Exists(path) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// previewQuestAnvils returns anvil name → main checkout path for every anvil
// that opted into running its E2E quests against a preview
// (`preview_quests: true`, with previews enabled for it).
//
// It is deliberately not filtered by questgiver_enabled: that flag governs the
// scheduled scan, while a preview quest run is asked for on one branch at one
// commit.
func previewQuestAnvils(cfg *config.Config) map[string]string {
	if cfg == nil {
		return nil
	}
	opted := make(map[string]string)
	for name, anvil := range cfg.Anvils {
		if anvil.Path == "" || !cfg.IsPreviewQuestsEnabledForAnvil(name) {
			continue
		}
		opted[name] = anvil.Path
	}
	if len(opted) == 0 {
		return nil
	}
	return opted
}

// previewQuestLookup resolves the live preview serving an anvil at a given head
// commit, for QuestGiver's preview run path. It reports the preview's id and
// status; QuestGiver decides what to do with a preview that is not healthy.
//
// A match is a live preview of the same anvil whose checkout is at headSHA. An
// empty headSHA matches the anvil's only preview, which is what a caller with
// no commit in hand (a manual "run quests" on a single-preview anvil) gets.
func (d *Daemon) previewQuestLookup(ctx context.Context, anvil, headSHA string) (questgiver.PreviewInfo, bool) {
	mgr := d.previews()
	if mgr == nil {
		return questgiver.PreviewInfo{}, false
	}
	for _, env := range mgr.List() {
		if env == nil || !strings.EqualFold(env.Anvil, anvil) {
			continue
		}
		if headSHA != "" && !d.previewServesHead(ctx, env, headSHA) {
			continue
		}
		return questgiver.PreviewInfo{
			PreviewID: kiln.SanitizePreviewID(env.BeadID),
			Status:    env.Status(),
		}, true
	}
	return questgiver.PreviewInfo{}, false
}

// previewServesHead reports whether a preview's checkout is at headSHA. The
// checkout is detached at the branch tip it was started from, so its HEAD is
// the commit the running services were built from — the honest answer to "is
// this preview showing that commit?", where the branch name is not (the branch
// may have moved since the preview started).
func (d *Daemon) previewServesHead(ctx context.Context, env *kiln.Environment, headSHA string) bool {
	if env.WorktreePath == "" {
		return false
	}
	head, err := worktree.HeadSHA(ctx, env.WorktreePath)
	if err != nil {
		d.logger.Debug("could not resolve preview checkout HEAD",
			"bead", env.BeadID, "path", env.WorktreePath, "error", err)
		return false
	}
	return shaMatches(head, headSHA)
}

// shaMatches compares a full commit SHA against a possibly abbreviated one.
// Abbreviations shorter than 7 characters must match exactly rather than by
// prefix, so a stray fragment cannot silently select the wrong preview.
func shaMatches(full, want string) bool {
	full = strings.ToLower(strings.TrimSpace(full))
	want = strings.ToLower(strings.TrimSpace(want))
	if full == "" || want == "" {
		return false
	}
	if len(want) < 7 {
		return full == want
	}
	return strings.HasPrefix(full, want)
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
		"max_concurrent", cfg.Settings.ResolvedPreviewMaxConcurrent(),
		"evict_lru", cfg.Settings.PreviewEvictLRU)
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
			EvictLRU:      cfg.Settings.PreviewEvictLRU,
			IdleTimeout:   cfg.Settings.PreviewIdleTimeout,
			PublicHost:    publicHost,
			BindHost:      bindHost,
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

// handlePreviewAutoStart starts a preview when Bellows announces a PR ready to
// merge, for anvils that opted in with `preview_auto: ready_to_merge`. It is
// the automatic half of the Preview button: the ready-to-merge moment is
// exactly when a human is about to decide on the branch, so having it already
// running (and, with the QuestGiver integration, already exercised) is worth
// the memory for the few minutes a review takes.
//
// Bellows emits EventPRReadyToMerge on the rising edge only, so this fires once
// per transition rather than every poll. Everything else is the same
// bounded resource a manual start gets: the concurrency cap still rejects, the
// idle reaper still collects, and a bead whose preview is already up just gets
// its idle clock touched.
//
// Skips are silent by design (log only, no Needs Attention): an auto-preview
// nobody asked for must not turn into an operator task when the cap is full.
func (d *Daemon) handlePreviewAutoStart(ctx context.Context, event bellows.PREvent) {
	if event.EventType != bellows.EventPRReadyToMerge {
		return
	}
	mgr := d.previews()
	if mgr == nil || event.BeadID == "" {
		return
	}
	// External PRs carry a synthetic ext-<number> bead id and a branch Forge
	// never created — there is nothing to check out under our own naming.
	if strings.HasPrefix(event.BeadID, "ext-") {
		return
	}
	cfg := d.cfg.Load()
	anvilName, anvilCfg, ok := d.resolveAnvilConfig(event.Anvil)
	if !ok || anvilCfg.Path == "" {
		return
	}
	// Covers the global gate, the per-anvil preview_enabled opt-out and the
	// preview_auto mode in one call.
	if !cfg.IsPreviewAutoReadyToMerge(anvilName) {
		return
	}
	// An anvil with no manifest could only ever fail the start; skipping here
	// keeps a warning per ready-to-merge PR out of the log.
	if !kiln.Exists(anvilCfg.Path) {
		d.logger.Debug("auto-preview skipped: anvil has no .forge/preview.yaml",
			"anvil", anvilName, "bead", event.BeadID)
		return
	}

	branch := strings.TrimSpace(event.Branch)
	if branch == "" {
		branch = worktree.BranchName(event.BeadID)
	}
	opts := kiln.StartOptions{
		BeadID:    event.BeadID,
		Anvil:     anvilName,
		AnvilPath: anvilCfg.Path,
		Branch:    branch,
		// Nobody asked for this one: a full box skips it rather than evicting
		// a preview an operator started, even with preview_evict_lru on.
		NoEvict: true,
	}
	prNumber := event.PRNumber

	// Detached from the bellows poll context (cancelled at the end of the
	// cycle, which would abort the setup script mid-run) and off the poll
	// goroutine, because a start is minutes of git checkout, setup and health
	// checks that must not stall PR monitoring.
	go func() {
		startCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), previewStartTimeout)
		defer cancel()
		env, err := mgr.Start(startCtx, opts)
		if err != nil {
			if errors.Is(err, kiln.ErrTooManyPreviews) {
				d.logger.Info("auto-preview skipped: preview_max_concurrent reached",
					"bead", opts.BeadID, "anvil", opts.Anvil, "pr", prNumber, "reason", err)
				return
			}
			d.logger.Warn("auto-preview on ready-to-merge failed",
				"bead", opts.BeadID, "anvil", opts.Anvil, "pr", prNumber, "error", err)
			return
		}
		d.logger.Info("auto-preview started on ready-to-merge",
			"bead", opts.BeadID, "anvil", opts.Anvil, "pr", prNumber,
			"branch", opts.Branch, "status", env.Status(), "entry_url", env.EntryURL())
		if d.db != nil {
			_ = d.db.LogEvent(state.EventPreviewAutoStarted,
				fmt.Sprintf("preview auto-started for PR #%d (%s)", prNumber, env.Status()),
				opts.BeadID, opts.Anvil)
		}
	}()
}

// handlePreviewStart serves the "preview_start" IPC command.
//
// Starting a preview is slow enough (git checkout, setup script, service spawn,
// health checks) that it cannot be answered synchronously, so it follows the
// daemon's queued-command pattern: the work runs on its own goroutine and the
// caller gets a request id it resolves through "request_status".
func (d *Daemon) handlePreviewStart(p ipc.PreviewActionPayload) ipc.Response {
	mgr := d.previews()
	if mgr == nil {
		return errorResponse("preview environments are disabled (settings.preview_enabled)")
	}
	beadID := strings.TrimSpace(p.BeadID)
	if beadID == "" {
		return errorResponse("bead_id is required")
	}
	if strings.TrimSpace(p.Anvil) == "" {
		return errorResponse("anvil is required")
	}
	anvilName, anvilCfg, ok := d.resolveAnvilConfig(p.Anvil)
	if !ok {
		return errorResponse(fmt.Sprintf("anvil %q not found", p.Anvil))
	}
	if anvilCfg.Path == "" {
		return errorResponse(fmt.Sprintf("anvil %q has no path configured", anvilName))
	}
	// The global gate is already implied by mgr being non-nil; this catches the
	// anvil that opted out via its own tri-state while its siblings did not.
	if !d.cfg.Load().IsPreviewEnabledForAnvil(anvilName) {
		return errorResponse(fmt.Sprintf("previews are disabled for anvil %q", anvilName))
	}

	branch := strings.TrimSpace(p.Branch)
	if branch == "" {
		branch = worktree.BranchName(beadID)
	}
	opts := kiln.StartOptions{
		BeadID:    beadID,
		Anvil:     anvilName,
		AnvilPath: anvilCfg.Path,
		Branch:    branch,
	}

	reqID, _ := d.reqTracker.Track()
	go func() {
		startCtx, cancel := context.WithTimeout(d.runCtx, previewStartTimeout)
		defer cancel()
		env, err := mgr.Start(startCtx, opts)
		if err != nil {
			d.logger.Warn("starting preview failed", "bead", beadID, "anvil", anvilName, "error", err)
			d.completeAsync(reqID, errorResponse(fmt.Sprintf("starting preview for %s failed: %v", beadID, err)))
			return
		}
		d.logger.Info("preview started", "bead", beadID, "anvil", anvilName,
			"branch", branch, "status", env.Status())
		d.completeAsync(reqID, okResponse(map[string]string{
			"message":   fmt.Sprintf("preview for %s is %s", beadID, env.Status()),
			"status":    env.Status(),
			"entry_url": env.EntryURL(),
		}))
	}()
	resp, _ := ipc.NewQueuedResponse(reqID, "starting preview")
	return resp
}

// handlePreviewStop serves the "preview_stop" IPC command. Like the start it is
// queued: teardown kills process groups, runs the manifest's teardown command
// and removes a git worktree.
//
// A bead with no preview is rejected up front rather than answered with a
// silent success. An operator typing `forge preview stop <bead>` at the wrong
// id deserves to hear about it, and the caller cannot tell the two apart once
// the work is queued. The automatic teardown paths (PR merge/close, shutdown)
// call the manager directly, where stopping something already gone stays the
// no-op it needs to be.
func (d *Daemon) handlePreviewStop(p ipc.PreviewActionPayload) ipc.Response {
	mgr := d.previews()
	if mgr == nil {
		return errorResponse("preview environments are disabled (settings.preview_enabled)")
	}
	beadID := strings.TrimSpace(p.BeadID)
	if beadID == "" {
		return errorResponse("bead_id is required")
	}
	if _, ok := mgr.Get(beadID); !ok {
		return errorResponse(fmt.Sprintf("no preview running for bead %s", beadID))
	}

	reqID, _ := d.reqTracker.Track()
	go func() {
		// Detached from the run context: a shutdown mid-teardown would otherwise
		// abandon the very worktree and process group the teardown exists to
		// release.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(d.runCtx), previewStopTimeout)
		defer cancel()
		if err := mgr.Stop(stopCtx, beadID); err != nil {
			d.logger.Warn("stopping preview failed", "bead", beadID, "error", err)
			d.completeAsync(reqID, errorResponse(fmt.Sprintf("stopping preview for %s failed: %v", beadID, err)))
			return
		}
		d.logger.Info("preview stopped", "bead", beadID)
		d.completeAsync(reqID, okResponse(ipc.PreviewStopResponse{
			Stopped: true,
			BeadID:  beadID,
			Message: fmt.Sprintf("preview for %s stopped", beadID),
		}))
	}()
	resp, _ := ipc.NewQueuedResponse(reqID, "stopping preview")
	return resp
}

// handlePreviewList serves the "previews" and "preview_list" IPC commands:
// every live preview with its per-service ports and health, plus the two
// settings a client needs to render links and idle deadlines itself.
//
// Listing deliberately does not Touch the previews it reports. The dashboard
// polls this endpoint, so counting a poll as activity would keep every preview
// alive forever and turn the idle reaper off in practice.
func (d *Daemon) handlePreviewList() ipc.Response {
	out := ipc.PreviewListResponse{
		Anvils:      []string{},
		QuestAnvils: []string{},
		Previews:    []ipc.PreviewInfo{},
	}
	cfg := d.cfg.Load()
	var idle time.Duration
	if cfg != nil {
		out.PublicHost = cfg.Settings.PreviewPublicHost
		idle = cfg.Settings.PreviewIdleTimeout
		out.IdleTimeoutSeconds = int64(idle / time.Second)
	}
	mgr := d.previews()
	if mgr == nil {
		return okResponse(out)
	}
	out.Enabled = true
	out.Anvils = previewableAnvils(cfg)
	out.QuestAnvils = previewQuestAnvilNames(cfg)
	now := time.Now()
	for _, env := range mgr.List() {
		out.Previews = append(out.Previews, previewInfo(env.Record(), env.EntryURL(), idle, now))
	}
	return okResponse(out)
}

// previewInfo maps one preview's persisted record onto the IPC payload. It
// takes the record and the entry URL rather than the *kiln.Environment so the
// mapping — the entry port, the idle countdown and the resource note derived
// here — is exercisable without a live preview behind it.
func previewInfo(rec state.Preview, entryURL string, idle time.Duration, now time.Time) ipc.PreviewInfo {
	info := ipc.PreviewInfo{
		BeadID:               rec.BeadID,
		Anvil:                rec.Anvil,
		Branch:               rec.Branch,
		Status:               rec.Status,
		CreatedAt:            rec.CreatedAt,
		LastActiveAt:         rec.LastActiveAt,
		EntryURL:             entryURL,
		Port:                 previewEntryPort(rec),
		IdleRemainingSeconds: previewIdleRemaining(rec, idle, now),
		ResourceNote:         previewResourceNote(rec),
	}
	for _, svc := range rec.Services {
		info.Services = append(info.Services, ipc.PreviewServiceInfo{
			Name:   svc.Name,
			Port:   svc.Port,
			Health: svc.Health,
			Entry:  svc.Entry,
			Error:  svc.Error,
		})
	}
	return info
}

// previewEntryPort returns the port the preview's entry link points at: the
// service flagged as the entry, falling back to the first service that has a
// port so a single-service preview still reports one. 0 means ports have not
// been allocated yet.
func previewEntryPort(rec state.Preview) int {
	for _, svc := range rec.Services {
		if svc.Entry && svc.Port > 0 {
			return svc.Port
		}
	}
	for _, svc := range rec.Services {
		if svc.Port > 0 {
			return svc.Port
		}
	}
	return 0
}

// previewIdleRemaining is the countdown to the idle reaper: how long is left of
// preview_idle_timeout from the preview's last activity. It clamps at 0 (a
// preview past its deadline is waiting for the next reaper tick, not overdue by
// a negative number) and returns nil when the reaper is disabled or the preview
// has never been touched, so a client renders "no deadline" rather than "due
// now".
func previewIdleRemaining(rec state.Preview, idle time.Duration, now time.Time) *int64 {
	if idle <= 0 || rec.LastActiveAt.IsZero() {
		return nil
	}
	secs := int64(rec.LastActiveAt.Add(idle).Sub(now) / time.Second)
	if secs < 0 {
		secs = 0
	}
	return &secs
}

// previewResourceNote summarises what a preview costs while it is up: the
// supervised process groups and the ports they hold. Kiln applies no memory or
// CPU limits, so the honest note is the footprint itself rather than a limit
// that was never set.
func previewResourceNote(rec state.Preview) string {
	if len(rec.Services) == 0 {
		return "no services"
	}
	ports := make([]string, 0, len(rec.Services))
	for _, svc := range rec.Services {
		if svc.Port > 0 {
			ports = append(ports, strconv.Itoa(svc.Port))
		}
	}
	note := fmt.Sprintf("%d service", len(rec.Services))
	if len(rec.Services) != 1 {
		note += "s"
	}
	if len(ports) == 0 {
		return note + ", no ports allocated"
	}
	return note + ", ports " + strings.Join(ports, ", ")
}
