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
	// Touch resets a preview's idle clock; a bead without one is a no-op.
	Touch(beadID string)
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

// warnPreviewOptInWithoutManager covers the one per-anvil preview edit a reload
// cannot honour on its own: the manager is built at startup and only when some
// anvil already opts in (startPreviews), so a Forge that started with previews
// off for every anvil has no manager to hand the newly opted-in anvil to.
//
// The config swap itself is correct — IsPreviewEnabledForAnvil answers true
// from the next reload on — but every entry point goes through the nil-safe
// previews() first and answers "preview environments are disabled", which reads
// like the edit was ignored. Naming the anvil and the restart is the whole
// difference between a two-minute fix and an afternoon.
func (d *Daemon) warnPreviewOptInWithoutManager(cfg *config.Config) {
	if d.previews() != nil || cfg == nil || !cfg.Settings.PreviewEnabled {
		return
	}
	anvils := previewAnvils(cfg)
	if len(anvils) == 0 {
		return
	}
	names := make([]string, 0, len(anvils))
	for name := range anvils {
		names = append(names, name)
	}
	sort.Strings(names)
	d.logger.Warn("anvil(s) opted into Kiln previews but no preview manager is running; "+
		"the manager is built at startup, so a daemon restart is required",
		"anvils", strings.Join(names, ", "))
}

// previewBeadForAnvil returns the bead holding a live preview on the named
// anvil, or "" when it has none — the depcheck.PreviewLivenessFunc the daemon
// injects so a dependency scan never runs `npm ci` in a checkout a preview is
// linked into. The registry is the one Kiln already answers `previews` from, so
// there is a single source of truth for "what is live".
//
// Ties are resolved by bead id (List is sorted): the caller only needs a name
// for the log line, and any live preview is reason enough to skip.
//
// A preview whose start is still in flight is not in List and so does not
// block — the same accepted race depcheck documents on its own side.
func (d *Daemon) previewBeadForAnvil(anvil string) string {
	mgr := d.previews()
	if mgr == nil || anvil == "" {
		return ""
	}
	for _, env := range mgr.List() {
		if env != nil && env.Anvil == anvil {
			return env.BeadID
		}
	}
	return ""
}

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

	d.warnPreviewPortRange(cfg)

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

// warnPreviewPortRange logs a WARN when the configured preview_port_range
// overlaps the range the host kernel assigns ephemeral (source) ports from.
//
// Kiln bind-tests a port when it allocates it, but the service only binds it
// minutes later, once its restore/build finishes; in that window the kernel can
// hand the same port to an outbound connection and the service then dies with
// "address already in use". The overlap is the precondition for that race, and
// it is invisible after the fact — hence naming both ranges up front.
//
// It warns and never rejects: an operator may have narrowed or moved the kernel
// range, and a probabilistic risk is no reason to refuse to start.
func (d *Daemon) warnPreviewPortRange(cfg *config.Config) {
	lo, hi, err := cfg.Settings.PreviewPortRangeBounds()
	if err != nil {
		// An unparseable range is reported by config validation and again by
		// buildPreviewManager; there is nothing to compare here.
		return
	}
	if msg := kiln.EphemeralOverlapWarning(lo, hi, config.DefaultPreviewPortRange); msg != "" {
		d.logger.Warn(msg)
	}
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
		// Kiln owns the state transition; the daemon owns telling anybody about
		// it. Without this the demotion would be honest and invisible — correct
		// on a panel nobody has open.
		OnServiceExit: d.handlePreviewServiceExit,
		Logger:        d.logger,
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
			ProxyBase:     cfg.Settings.ResolvedPreviewProxyBase(),
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
			"branch", opts.Branch, "status", env.Status(),
			"entry_url", previewEntryURL(d.cfg.Load(), env.Record()))
		if d.db != nil {
			_ = d.db.LogEvent(state.EventPreviewAutoStarted,
				fmt.Sprintf("preview auto-started for PR #%d (%s)", prNumber, env.Status()),
				opts.BeadID, opts.Anvil)
		}
	}()
}

// handlePreviewServiceExit records a preview service that became healthy and
// then died, as one activity-feed event against the bead.
//
// The event exists for the same reason the Assay terminal events do: a limb
// dying is something the operator should hear about without opening the panel
// that would have shown it. The message names what an operator needs to decide
// whether to act — which service, why it died, how long it lived, and what the
// preview can still serve — because the alternative is inferring all four from
// a status chip that changed colour.
func (d *Daemon) handlePreviewServiceExit(exit kiln.ServiceExit) {
	if d == nil || d.db == nil {
		return
	}
	msg := fmt.Sprintf("preview service %q %s — preview is now %s",
		exit.Service, exit.Detail, exit.Status)
	if exit.Entry {
		// The entry service is the preview's address, so its death is the
		// difference between "part of this is broken" and "the link is dead".
		msg += " (entry service — no preview URL)"
	}
	if err := d.db.LogEvent(state.EventPreviewServiceExited, msg, exit.BeadID, exit.Anvil); err != nil {
		d.logger.Warn("logging a preview service exit failed",
			"bead", exit.BeadID, "service", exit.Service, "error", err)
	}
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
		d.completeAsync(reqID, okResponse(ipc.PreviewStartResponse{
			BeadID:   beadID,
			Status:   env.Status(),
			Message:  fmt.Sprintf("preview for %s is %s", beadID, env.Status()),
			EntryURL: previewEntryURL(d.cfg.Load(), env.Record()),
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
		rec := env.Record()
		out.Previews = append(out.Previews, previewInfo(rec, previewEntryURL(cfg, rec), idle, now))
	}
	return okResponse(out)
}

// handlePreviewResolve serves the "preview_resolve" IPC command: the host-based
// preview proxy in internal/web hands over the label it parsed out of a Host
// header and gets back the loopback address to forward to.
//
// The lookup lives here rather than in the web layer because the Kiln registry
// does: the proxy sees a hostname, the daemon owns the bead → preview mapping,
// and doing both in one call means the resolve can Touch the preview. Unlike
// "previews" — which the dashboard polls, so counting it as activity would
// disable the idle reaper in practice — a proxied request *is* someone using
// the preview, and must reset the idle clock.
//
// Every refusal is an ok response carrying a PreviewResolve* reason. The proxy
// turns each into its own 404 body, so "no preview by that name" and "that
// preview is stopped" stay distinguishable to whoever typed the URL.
func (d *Daemon) handlePreviewResolve(p ipc.PreviewResolvePayload) ipc.Response {
	label := kiln.NormalizeHostname(p.Label)
	service := kiln.NormalizeHostname(p.Service)
	if label == "" {
		return errorResponse("label is required")
	}
	out := ipc.PreviewResolveResponse{Service: service}

	mgr := d.previews()
	if mgr == nil {
		out.Reason = ipc.PreviewResolveDisabled
		return okResponse(out)
	}
	var match *kiln.Environment
	for _, env := range mgr.List() {
		if env != nil && kiln.PreviewLabel(env.BeadID) == label {
			match = env
			break
		}
	}
	if match == nil {
		out.Reason = ipc.PreviewResolveNoPreview
		return okResponse(out)
	}

	rec := match.Record()
	out.BeadID = rec.BeadID
	out.Status = rec.Status
	// A stopped or failed preview is still in the registry for a moment (and a
	// failed one may never have served anything), so answering with its ports
	// would forward to a dead process group.
	if rec.Status == state.PreviewStopped || rec.Status == state.PreviewFailed {
		out.Reason = ipc.PreviewResolveStopped
		return okResponse(out)
	}

	var target state.PreviewService
	if service == "" {
		target, _ = previewEntryService(rec)
	} else {
		svc, ok := previewServiceByLabel(rec, service)
		if !ok {
			out.Reason = ipc.PreviewResolveNoService
			return okResponse(out)
		}
		target = svc
	}
	out.Service = target.Name
	if target.Port <= 0 {
		out.Reason = ipc.PreviewResolveNoPort
		return okResponse(out)
	}
	// Forwarding to the port of a service that failed or has exited produces a
	// connection error the browser reports as a network fault. Answering
	// "not serving" instead points at the log, which is where the answer is.
	if !state.PreviewServiceServing(target.Health) {
		out.Reason = ipc.PreviewResolveNotServing
		return okResponse(out)
	}
	port := target.Port

	out.Host = previewDialHost(d.cfg.Load())
	out.Port = port
	out.Found = true
	// Proxied traffic is activity — the whole point of resolving through the
	// daemon rather than reading the ports out of the previews payload.
	mgr.Touch(rec.BeadID)
	return okResponse(out)
}

// previewServiceByLabel finds the service a `<label>--<service>` host names.
//
// A hostname label is lowercase and carries neither '.' nor '_', both of which
// a manifest service name may contain, so an exact (case-insensitive) match is
// tried first and a match on the name folded to a DNS label second. The fold is
// not injective — services named "api_v1" and "api.v1" both fold to "api-v1" —
// so the first one declared wins, the same way the manifest's own
// FORGE_PREVIEW_PORT_<NAME> collision is a manifest bug rather than something
// resolved at request time.
func previewServiceByLabel(rec state.Preview, label string) (state.PreviewService, bool) {
	for _, svc := range rec.Services {
		if strings.EqualFold(svc.Name, label) {
			return svc, true
		}
	}
	for _, svc := range rec.Services {
		if kiln.ServiceLabel(svc.Name) == label {
			return svc, true
		}
	}
	return state.PreviewService{}, false
}

// previewDialHost is the address a proxy connects to a preview service on.
//
// It is settings.preview_bind_host, except that a wildcard bind names no
// address to dial — "listening everywhere" is not somewhere to connect — so it
// is reported as loopback, which a wildcard listener also answers on.
func previewDialHost(cfg *config.Config) string {
	host := config.DefaultPreviewBindHost
	if cfg != nil {
		host = cfg.Settings.ResolvedPreviewBindHost()
	}
	switch strings.Trim(strings.TrimSpace(host), "[]") {
	case "", "0.0.0.0", "::", "*":
		return "127.0.0.1"
	}
	return host
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
		EntryNote:            previewEntryNote(rec),
		Port:                 previewEntryPort(rec),
		IdleRemainingSeconds: previewIdleRemaining(rec, idle, now),
		ResourceNote:         previewResourceNote(rec),
	}
	for _, svc := range rec.Services {
		info.Services = append(info.Services, ipc.PreviewServiceInfo{
			Name:      svc.Name,
			Port:      svc.Port,
			Health:    svc.Health,
			Entry:     svc.Entry,
			Error:     svc.Error,
			StartedAt: svc.StartedAt,
			ExitedAt:  svc.ExitedAt,
			ExitCode:  svc.ExitCode,
		})
	}
	return info
}

// previewEntryURL is the operator-facing link for a preview: what the previews
// payload carries, what `forge preview list` prints and what a finished
// preview_start reports.
//
// It is built from the settings rather than taken from the running preview
// (kiln.Environment.EntryURL, which is always the direct host:port the service
// binds) because which form is *the* link is a daemon-level decision: with
// settings.preview_proxy_base configured previews are addressed by hostname and
// the loopback port is frequently not reachable by whoever reads the link.
// Reading the settings here also means a hot-reloaded preview_public_host is
// picked up by previews that were already running when it changed.
//
// It mints no access token. The token belongs to the web layer's auth gate,
// which has the secret and knows whether the caller's session cookie already
// reaches the preview host; the token-carrying link is built there (see
// internal/web.previewEntryURL) on top of this same builder.
func previewEntryURL(cfg *config.Config, rec state.Preview) string {
	opts := kiln.EntryURLOptions{BeadID: rec.BeadID, Port: previewEntryPort(rec)}
	if cfg != nil {
		opts.ProxyBase = cfg.Settings.ResolvedPreviewProxyBase()
		opts.Host = cfg.Settings.ResolvedPreviewPublicHost()
	}
	return kiln.EntryURL(opts)
}

// previewEntryService returns the service a preview's link points at: the one
// flagged as the manifest's entry, falling back to the first service with a port
// so a single-service preview (which needs no `entry: true`) still has one.
//
// It answers regardless of that service's health — the caller decides what a
// dead entry service means, and every caller needs to be able to name it.
func previewEntryService(rec state.Preview) (state.PreviewService, bool) {
	for _, svc := range rec.Services {
		if svc.Entry && svc.Port > 0 {
			return svc, true
		}
	}
	for _, svc := range rec.Services {
		if svc.Port > 0 {
			return svc, true
		}
	}
	return state.PreviewService{}, false
}

// previewEntryPort returns the port the preview's entry link points at, or 0
// when there is no link to give: no ports allocated yet, or an entry service
// that is not serving.
//
// The second case is the point. A preview whose entry service died still holds
// its port allocation, so the old "first port that exists" rule kept handing out
// a URL that answered ERR_EMPTY_RESPONSE — which reads as a tunnel or network
// fault, not as a dead process, and cost an afternoon of misdirected triage. It
// never falls back to a healthy *sibling's* port either: that would produce a
// link that works and shows the wrong application.
func previewEntryPort(rec state.Preview) int {
	svc, ok := previewEntryService(rec)
	if !ok || !state.PreviewServiceServing(svc.Health) {
		return 0
	}
	return svc.Port
}

// previewEntryNote explains a withheld entry URL, and is empty whenever there is
// nothing to explain — including when the URL is missing simply because ports
// have not been allocated yet, which is a preview still starting rather than a
// preview with a problem.
func previewEntryNote(rec state.Preview) string {
	svc, ok := previewEntryService(rec)
	if !ok || state.PreviewServiceServing(svc.Health) {
		return ""
	}
	detail := svc.Error
	if detail == "" {
		detail = svc.Health
	}
	return fmt.Sprintf("entry service %q is not serving: %s", svc.Name, detail)
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
