// Package daemon implements The Forge's background daemon process.
//
// The daemon runs the main orchestration loop:
//   - Polls anvils for ready beads (via poller)
//   - Spawns Smith workers (via worker pool)
//   - Monitors PRs (via Bellows)
//   - Writes a PID file for lifecycle management
//   - Logs to ~/.forge/logs/daemon.log
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/cifix"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/crucible"
	"github.com/Robin831/Forge/internal/depcheck"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/ghpr"
	"github.com/Robin831/Forge/internal/hotreload"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/lifecycle"
	"github.com/Robin831/Forge/internal/notify"
	"github.com/Robin831/Forge/internal/pipeline"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/rebase"
	"github.com/Robin831/Forge/internal/reviewfix"
	"github.com/Robin831/Forge/internal/schematic"
	"github.com/Robin831/Forge/internal/shutdown"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/vulncheck"
	"github.com/Robin831/Forge/internal/worker"
	"github.com/Robin831/Forge/internal/worktree"
)

const (
	// PIDFileName is the name of the PID file within ~/.forge/.
	PIDFileName = "forge.pid"

	// LogDir is the directory for daemon logs within ~/.forge/.
	LogDir = "logs"

	// LogFileName is the daemon log filename.
	LogFileName = "daemon.log"

	// DefaultPollInterval is the default interval between bead polls.
	DefaultPollInterval = 30 * time.Second

	// GracefulTimeout is how long to wait for workers to finish on shutdown.
	GracefulTimeout = 60 * time.Second

	// MaxDispatchFailures is the number of consecutive dispatch failures before
	// a bead is circuit-broken (marked needs_human). This prevents a single
	// poison bead from consuming capacity every poll cycle.
	MaxDispatchFailures = 3
)

// bellowsMonitorIface defines the subset of *bellows.Monitor used by the daemon,
// allowing a test double to be injected in unit tests.
type bellowsMonitorIface interface {
	OnEvent(h bellows.Handler)
	UpdateAnvilPaths(paths map[string]string)
	Refresh()
	Run(ctx context.Context) error
	ResetPRState(anvil string, prNumber int)
}

// temperCacheEntry caches a parsed per-anvil temper.yaml along with the file's
// modification time so the file is only re-read when it changes on disk.
// statErr is non-empty when the last os.Stat call returned a non-ENOENT error;
// it is used to suppress repeated log spam when the file is unreadable.
type temperCacheEntry struct {
	cfg     *temper.TemperYAML
	mtime   time.Time
	statErr string // last non-ENOENT stat error message; empty on success
}

// Daemon is the main Forge orchestration daemon.
type Daemon struct {
	cfg           atomic.Pointer[config.Config]
	db            *state.DB
	logger        *slog.Logger
	ipc           *ipc.Server
	shutdownMgr   *shutdown.Manager
	configWatcher *hotreload.Watcher

	// Dispatch state
	activeBeads    sync.Map       // beadID -> true, currently in-flight
	pendingActions sync.Map       // beadID -> lifecycle.ActionRequest; single parked action per bead, latest-wins
	wg             sync.WaitGroup // tracks running pipeline goroutines
	pollRunning    atomic.Bool    // true while pollAndDispatch is executing; prevents concurrent overlapping polls
	worktreeMgr    *worktree.Manager
	promptBuilder  *prompt.Builder

	// PR Monitoring (Bellows)
	bellowsMonitor  bellowsMonitorIface
	lifecycleMgr    *lifecycle.Manager
	depcheckScanner *depcheck.Scanner

	// Vulnerability scanning
	vulnScanner *vulncheck.Scanner

	// Teams notifications (nil = disabled). Uses atomic.Pointer so the hot-reload
	// callback can swap in a new Notifier without a mutex while concurrent
	// pipeline goroutines safely read via Load().
	notifier atomic.Pointer[notify.Notifier]

	// Generic webhook dispatcher (nil = no generic webhooks configured).
	// Uses atomic.Pointer so the hot-reload callback can swap in a new dispatcher
	// when webhook config changes without a mutex while concurrent goroutines
	// safely read via Load(). WebhookDispatcher methods are nil-safe.
	dispatcher atomic.Pointer[notify.WebhookDispatcher]

	cancel context.CancelFunc // cancels the Run context for graceful shutdown
	runCtx context.Context    // the live run context; set in Run() after signal/cancel wiring

	forgeDir   string // ~/.forge
	pidFile    string
	configFile string
	logFile    *os.File
	startTime  time.Time

	// Cache for last poll results
	lastBeads   []poller.Bead
	lastBeadsMu sync.RWMutex

	// Per-anvil temper.yaml cache keyed by anvil path.
	// Avoids repeated filesystem I/O on every dispatch and de-duplicates
	// log spam when the file is invalid or unreadable.
	temperCache sync.Map // map[string]*temperCacheEntry

	// Active Crucible statuses (parentBeadID -> crucible.Status)
	crucibleStatuses sync.Map

	// Periodic bead recovery counter (runs every N poll cycles)
	pollCount atomic.Int64

	// Last successful poll timestamp
	lastPollTime atomic.Value // stores time.Time

	// Cost limit: tracks which date we last logged the cost_limit_hit event
	// to avoid spamming the event log every poll cycle.
	costLimitLoggedDate atomic.Value // stores string (YYYY-MM-DD)

	// labelAdder adds a label to a bead via the bd CLI. Defaults to the real
	// bd-update implementation; may be replaced in tests to avoid exec.Command.
	labelAdder func(anvilPath, beadID, tag string) error
}

// New creates a new daemon instance.
func New(cfg *config.Config) (*Daemon, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("finding home directory: %w", err)
	}

	forgeDir := filepath.Join(home, ".forge")
	if err := os.MkdirAll(filepath.Join(forgeDir, LogDir), 0o755); err != nil {
		return nil, fmt.Errorf("creating log directory: %w", err)
	}

	logPath := filepath.Join(forgeDir, LogDir, LogFileName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening log file: %w", err)
	}

	// Log to both file and stderr
	multiWriter := io.MultiWriter(logFile, os.Stderr)
	logger := slog.New(slog.NewTextHandler(multiWriter, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	db, err := state.Open("")
	if err != nil {
		logFile.Close()
		return nil, fmt.Errorf("opening state database: %w", err)
	}

	wtMgr := worktree.NewManager()

	notifier, err := newNotifierFromConfig(cfg, logger)
	if err != nil {
		db.Close()
		logFile.Close()
		return nil, fmt.Errorf("invalid Teams webhook URL: %w", err)
	}

	// Build generic webhook dispatcher from the new webhooks config.
	// Respects the global notifications.enabled flag — no targets are built
	// when notifications are disabled, so the dispatcher returns nil (no-op).
	var dispatcher *notify.WebhookDispatcher
	if cfg.Notifications.Enabled {
		var webhookTargets []notify.WebhookTarget
		for _, w := range cfg.Notifications.Webhooks {
			trimmedURL := strings.TrimSpace(w.URL)
			if trimmedURL == "" {
				continue
			}

			var trimmedEvents []string
			for _, ev := range w.Events {
				tEv := strings.TrimSpace(ev)
				if tEv != "" {
					trimmedEvents = append(trimmedEvents, tEv)
				}
			}

			webhookTargets = append(webhookTargets, notify.WebhookTarget{
				Name:   w.Name,
				URL:    trimmedURL,
				Events: trimmedEvents,
			})
		}
		dispatcher = notify.NewWebhookDispatcher(webhookTargets, logger)
	}

	d := &Daemon{
		db:            db,
		logger:        logger,
		forgeDir:      forgeDir,
		pidFile:       filepath.Join(forgeDir, PIDFileName),
		configFile:    config.ConfigFilePath(""),
		logFile:       logFile,
		shutdownMgr:   shutdown.NewManager(db, wtMgr, logger, anvilPathMap(cfg)),
		worktreeMgr:   wtMgr,
		promptBuilder: prompt.NewBuilder(),
	}
	d.notifier.Store(notifier)
	d.dispatcher.Store(dispatcher)
	// Wire up the crucible-active check so orphan recovery skips parent beads
	// that are currently being orchestrated by an in-process Crucible run.
	// The key is "anvil/beadID" to avoid false positives when two anvils share
	// the same bead ID.
	d.shutdownMgr.SetCrucibleActiveCheck(func(beadID, anvil string) bool {
		_, active := d.crucibleStatuses.Load(anvil + "/" + beadID)
		return active
	})

	// Wire up the orphan-found callback to defer recovery to the Hearth dialog
	// when a TUI client is connected. When no client is connected (headless/CI),
	// the callback returns false and auto-recovery proceeds as before.
	d.shutdownMgr.OnOrphanFound = func(beadID, anvil, title, branch string) bool {
		if d.ipc == nil || !d.ipc.HasClients() {
			return false // no Hearth client — auto-recover
		}
		// Avoid duplicate entries if the bead is already pending a user decision.
		if already, err := d.db.IsPendingOrphan(beadID, anvil); err == nil && already {
			return true // already queued in the dialog, skip
		}
		// Record the orphan so Hearth's polling loop can show the dialog.
		if err := d.db.AddPendingOrphan(beadID, anvil, title, branch); err != nil {
			d.logger.Warn("failed to record pending orphan", "bead", beadID, "error", err)
			return false // fall back to auto-recover on DB error
		}
		_ = d.db.LogEvent(state.EventOrphanCleanup,
			fmt.Sprintf("Orphan %s deferred to Hearth dialog", beadID), beadID, anvil)
		return true
	}

	// Initialize costLimitLoggedDate so Load() is always safe (zero atomic.Value
	// returns nil on Load, which is fine for type assertion, but Store("")
	// makes the intent explicit and avoids any future ambiguity).
	d.costLimitLoggedDate.Store("")
	d.cfg.Store(cfg)
	d.labelAdder = func(anvilPath, beadID, tag string) error {
		ctx, cancel := context.WithTimeout(d.runCtx, 30*time.Second)
		defer cancel()
		cmd := executil.HideWindow(exec.CommandContext(ctx, "bd", "update", beadID, "--add-label", tag))
		cmd.Dir = anvilPath
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	}
	// Default runCtx to context.Background() so IPC handlers that access it
	// before Run() wires up the real context (e.g. early tag_bead commands)
	// never receive a nil context.
	d.runCtx = context.Background()
	return d, nil
}

// config returns the current daemon configuration atomically.
// Use this instead of accessing d.cfg directly to avoid data races with
// the hot-reload goroutine that updates the config pointer.
func (d *Daemon) config() *config.Config {
	return d.cfg.Load()
}

// anvilPathMap extracts directory paths from all configured anvils.
func anvilPathMap(cfg *config.Config) map[string]string {
	m := make(map[string]string)
	for name, a := range cfg.Anvils {
		if a.Path != "" {
			m[name] = a.Path
		}
	}
	return m
}

// reconcileGitHubPRs fetches open PRs from GitHub and registers any that are
// missing from the state DB. This ensures Bellows monitors PRs even after a
// DB reset or if the PR was created outside a recorded Forge pipeline session.
func (d *Daemon) reconcileGitHubPRs(ctx context.Context) {
	for anvilName, anvilCfg := range d.cfg.Load().Anvils {
		if anvilCfg.Path == "" {
			continue
		}
		prs, err := ghpr.ListOpen(ctx, anvilCfg.Path)
		if err != nil {
			d.logger.Warn("reconcile: could not list GitHub PRs", "anvil", anvilName, "err", err)
			continue
		}
		for _, pr := range prs {
			existing, _ := d.db.PRByNumber(pr.Number)
			if existing != nil {
				continue // already tracked
			}
			beadID := extractBeadID(pr.Body)
			if beadID == "" {
				continue // not a Forge PR
			}
			dbPR := &state.PR{
				Number:    pr.Number,
				Anvil:     anvilName,
				BeadID:    beadID,
				Branch:    pr.Branch,
				Status:    state.PROpen,
				CreatedAt: time.Now(),
			}
			if err := d.db.InsertPR(dbPR); err == nil {
				d.logger.Info("reconcile: registered untracked GitHub PR",
					"pr", pr.Number, "bead", beadID, "anvil", anvilName)
			}
		}
	}
}

// extractBeadID parses a bead ID from a Forge PR body (e.g. "**Bead**: Forge-abc").
func extractBeadID(body string) string {
	const marker = "**Bead**: "
	idx := strings.Index(body, marker)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(marker):]
	end := strings.IndexAny(rest, "\n\r ")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

// Run starts the daemon's main loop. It blocks until ctx is cancelled
// or a shutdown signal is received.
func (d *Daemon) Run(ctx context.Context) error {
	// Write PID file
	if err := d.writePID(); err != nil {
		return fmt.Errorf("writing PID file: %w", err)
	}
	defer d.removePID()
	defer d.cleanup()

	d.startTime = time.Now()

	d.logger.Info("daemon started",
		"pid", os.Getpid(),
		"anvils", len(d.cfg.Load().Anvils),
		"poll_interval", d.cfg.Load().Settings.PollInterval,
	)
	d.db.LogEvent(state.EventDaemonStarted, "Forge daemon started", "", "")

	// Clean up orphans from any previous crash
	if cleaned := d.shutdownMgr.CleanupOrphans(); cleaned > 0 {
		d.logger.Info("startup orphan cleanup done", "cleaned", cleaned)
	}

	// Recover orphaned in-progress beads (claimed but no active worker or open PR)
	if recovered := d.shutdownMgr.RecoverOrphanedBeads(); recovered > 0 {
		d.logger.Info("startup bead recovery done", "recovered", recovered)
	}

	// Set up signal handling and run context BEFORE starting IPC server,
	// so IPC handlers always see a valid, race-free runCtx.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Store cancel so IPC shutdown command can trigger graceful stop.
	// Wrap ctx with a cancel so the IPC handler can cancel independently.
	ctx, d.cancel = context.WithCancel(ctx)
	d.runCtx = ctx

	// Start IPC server
	d.ipc = ipc.NewServer()
	d.ipc.OnCommand(d.handleIPC)
	go func() {
		if err := d.ipc.Start(ctx); err != nil {
			d.logger.Error("IPC server error", "error", err)
		}
	}()

	// Start config hot-reload watcher
	if d.configFile != "" {
		d.configWatcher = hotreload.NewWatcher(d.configFile, d.cfg.Load(), d.logger)
		d.configWatcher.OnChange(func(old, new *config.Config) {
			d.cfg.Store(new)
			if d.lifecycleMgr != nil {
				d.lifecycleMgr.SetThresholds(
					new.Settings.MaxCIFixAttempts,
					new.Settings.MaxReviewFixAttempts,
					new.Settings.MaxRebaseAttempts,
				)
			}
			// Recreate the notifier when any notification setting changes so
			// that webhook URL, enabled flag, or event filters take effect
			// immediately without a daemon restart.
			if old.Notifications.Enabled != new.Notifications.Enabled ||
				old.Notifications.ResolvedTeamsURL() != new.Notifications.ResolvedTeamsURL() ||
				!slices.Equal(old.Notifications.ResolvedTeamsEvents(), new.Notifications.ResolvedTeamsEvents()) {
				if n := d.buildNotifier(new); n != nil {
					d.notifier.Store(n)
				}
			}
			// Recreate the generic webhook dispatcher when the webhooks[] list or
			// enabled flag changes so that new/removed targets and event filters
			// take effect without a daemon restart.
			oldWhJSON, _ := json.Marshal(old.Notifications.Webhooks)
			newWhJSON, _ := json.Marshal(new.Notifications.Webhooks)
			if old.Notifications.Enabled != new.Notifications.Enabled ||
				string(oldWhJSON) != string(newWhJSON) {
				d.dispatcher.Store(d.buildDispatcher(new))
				d.logger.Info("webhook dispatcher reloaded")
			}
			// Update bellows and depcheck when anvils change
			d.updateAnvilPaths(old, new)
			d.db.LogEvent(state.EventConfigReload, "Configuration reloaded", "", "")
		})
		go func() {
			if err := d.configWatcher.Start(); err != nil {
				d.logger.Error("config watcher error", "error", err)
			}
		}()
	}

	// Main poll loop
	pollInterval := d.cfg.Load().Settings.PollInterval
	if pollInterval == 0 {
		pollInterval = DefaultPollInterval
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Initial poll
	d.pollAndDispatch(ctx)

	// Start PR Monitor (Bellows)
	monitorAnvils := make(map[string]string)
	for name, a := range d.cfg.Load().Anvils {
		if a.Path != "" {
			monitorAnvils[name] = a.Path
		}
	}
	d.bellowsMonitor = bellows.New(d.db, d.cfg.Load().Settings.BellowsInterval, monitorAnvils, func() bool {
		return d.cfg.Load().Settings.AutoLearnRules
	}, func() int {
		return d.cfg.Load().Settings.MaxCIFixAttempts
	})
	d.lifecycleMgr = lifecycle.New(d.db, d.logger, d.handleLifecycleAction)
	d.lifecycleMgr.SetThresholds(
		d.cfg.Load().Settings.MaxCIFixAttempts,
		d.cfg.Load().Settings.MaxReviewFixAttempts,
		d.cfg.Load().Settings.MaxRebaseAttempts,
	)
	if err := d.lifecycleMgr.Load(ctx); err != nil {
		d.logger.Error("failed to load lifecycle states", "error", err)
		return fmt.Errorf("daemon initialization failed: %w", err)
	}
	d.bellowsMonitor.OnEvent(d.lifecycleMgr.HandleEvent)
	d.bellowsMonitor.OnEvent(d.handleBellowsNotifications)

	// Reconcile: register any GitHub PRs not yet tracked in the state DB.
	// This handles PRs created before the current DB or after a DB reset.
	d.reconcileGitHubPRs(ctx)

	go func() {
		if err := d.bellowsMonitor.Run(ctx); err != nil && err != context.Canceled {
			d.logger.Error("Bellows monitor error", "error", err)
		}
	}()

	// Start stale worker detection loop (always running; respects current config)
	go d.runStaleDetection(ctx)

	// Start dependency update checker (if enabled)
	if d.config().Settings.DepcheckInterval > 0 {
		depcheckAnvils := filterDepcheckAnvils(monitorAnvils, d.cfg.Load().Anvils)
		for name := range monitorAnvils {
			if _, ok := depcheckAnvils[name]; !ok {
				d.logger.Info("Skipping anvil for depcheck (depcheck_enabled=false)", "anvil", name)
			}
		}
		d.depcheckScanner = depcheck.New(d.db,
			d.config().Settings.DepcheckInterval,
			d.config().Settings.DepcheckTimeout,
			depcheckAnvils)
		go func() {
			if err := d.depcheckScanner.Run(ctx); err != nil && err != context.Canceled {
				d.logger.Error("Depcheck scanner error", "error", err)
			}
		}()
	}

	// Start vulnerability scanning loop (respects vulncheck_enabled config)
	if d.config().Settings.IsVulncheckEnabled() {
		d.vulnScanner = vulncheck.New(d.db, d.logger, d.config().Anvils, d.config().Settings.VulncheckTimeout)
		go d.vulnScanner.RunScheduled(ctx, d.config().Settings.VulncheckInterval)
	} else {
		d.logger.Info("vulncheck disabled via configuration (vulncheck_enabled: false)")
	}

	for {
		select {
		case <-ctx.Done():
			d.logger.Info("daemon shutting down", "reason", ctx.Err())
			killed := d.shutdownMgr.GracefulShutdown()
			d.shutdownMgr.CleanupWorktrees()
			d.wg.Wait() // wait for all dispatch goroutines
			// Wait for any in-flight generic webhook deliveries so that a graceful
			// shutdown does not drop pr_ready_to_merge or other notifications that
			// were started by Bellows just before the shutdown signal arrived.
			d.dispatcher.Load().Wait()
			d.db.LogEvent(state.EventDaemonStopped,
				fmt.Sprintf("Forge daemon stopped (killed %d workers)", killed), "", "")
			return nil

		case <-ticker.C:
			d.pollAndDispatch(ctx)
		}
	}
}

// handleLifecycleAction handles PR-triggered fixes from Bellows.
func (d *Daemon) handleLifecycleAction(ctx context.Context, req lifecycle.ActionRequest) {
	d.logger.Info("lifecycle action requested", "action", req.Action, "pr", req.PRNumber, "bead", req.BeadID)

	anvilCfg, ok := d.cfg.Load().Anvils[req.Anvil]
	if !ok {
		d.logger.Error("unknown anvil in lifecycle action", "anvil", req.Anvil)
		return
	}

	// If bead is already in flight, park the action for after it finishes.
	if _, inFlight := d.activeBeads.LoadOrStore(req.BeadID, true); inFlight {
		d.pendingActions.Store(req.BeadID, req)
		d.logger.Info("bead in flight, queued lifecycle action for later", "bead", req.BeadID, "action", req.Action)
		return
	}

	d.wg.Add(1)

	go func() {
		defer d.wg.Done()
		// Drain order: activeBeads.Delete runs first (registered last → LIFO),
		// then drainPendingAction fires so any parked action sees the bead as free.
		// Skip draining during shutdown to avoid wg.Add after wg.Wait.
		defer func() {
			if ctx.Err() == nil {
				d.drainPendingAction(ctx, req.BeadID)
			}
		}()
		defer d.activeBeads.Delete(req.BeadID)

		// Create/get worktree for the PR branch
		wt, err := d.worktreeMgr.Create(ctx, anvilCfg.Path, req.BeadID, req.Branch)
		if err != nil {
			d.logger.Error("failed to create worktree for lifecycle fix", "error", err)
			return
		}
		defer d.worktreeMgr.Remove(ctx, anvilCfg.Path, wt)

		workerID := fmt.Sprintf("%s-%s-%d", req.Anvil, req.BeadID, time.Now().UnixNano())

		switch req.Action {
		case lifecycle.ActionFixCI:
			d.logger.Info("spawning CI fix worker", "pr", req.PRNumber, "bead", req.BeadID)
			_ = d.db.InsertWorker(&state.Worker{
				ID:        workerID,
				BeadID:    req.BeadID,
				Anvil:     req.Anvil,
				Branch:    req.Branch,
				Status:    state.WorkerRunning,
				Phase:     "cifix",
				Title:     d.db.BeadTitle(req.BeadID, req.Anvil),
				PRNumber:  req.PRNumber,
				StartedAt: time.Now(),
			})
			cifixDetectOpts := temper.DetectOptionsFromAnvilFlag(anvilCfg.GolangciLint)
			res := cifix.Fix(ctx, cifix.FixParams{
				WorktreePath:    wt.Path,
				BeadID:          req.BeadID,
				AnvilName:       req.Anvil,
				AnvilPath:       anvilCfg.Path,
				PRNumber:        req.PRNumber,
				Branch:          req.Branch,
				DB:              d.db,
				WorkerID:        workerID,
				ExtraFlags:      d.config().Settings.ClaudeFlags,
				DetectOptions:   cifixDetectOpts,
				GoRaceDetection: d.resolveGoRaceDetection(anvilCfg),
				Providers:       d.filterCopilotIfLimited(provider.FromConfig(d.config().Settings.Providers)),
			})
			status := state.WorkerDone
			if res.Error != nil {
				status = state.WorkerFailed
			}
			_ = d.db.UpdateWorkerStatus(workerID, status)
			// Always notify lifecycle that the CI fix cycle has completed so it
			// can reset any suppression state and allow future CI-failure
			// detection to trigger additional attempts as needed.
			d.lifecycleMgr.NotifyCIFixCompleted(req.Anvil, req.PRNumber)
			// Reset the bellows snapshot cache so the next poll sees a fresh
			// state transition. Without this, bellows sees CI still failing
			// (same as last snapshot) and never re-emits EventCIFailed,
			// preventing the lifecycle manager from dispatching retries.
			if d.bellowsMonitor != nil {
				// Reset the snapshot only; do not trigger an immediate Refresh()
				// here because CI checks may still be pending. An immediate poll
				// would see "not yet passing" and could emit EventCIFailed while
				// checks are still running, burning through cifix retries before
				// CI has a chance to complete. The regular poll interval is
				// sufficient to re-detect failure once CI settles.
				d.bellowsMonitor.ResetPRState(req.Anvil, req.PRNumber)
			}

		case lifecycle.ActionFixReview:
			d.logger.Info("spawning review fix worker", "pr", req.PRNumber, "bead", req.BeadID)
			_ = d.db.InsertWorker(&state.Worker{
				ID:        workerID,
				BeadID:    req.BeadID,
				Anvil:     req.Anvil,
				Branch:    req.Branch,
				Status:    state.WorkerRunning,
				Phase:     "reviewfix",
				Title:     d.db.BeadTitle(req.BeadID, req.Anvil),
				PRNumber:  req.PRNumber,
				StartedAt: time.Now(),
			})
			res := reviewfix.Fix(ctx, reviewfix.FixParams{
				WorktreePath: wt.Path,
				BeadID:       req.BeadID,
				AnvilName:    req.Anvil,
				AnvilPath:    anvilCfg.Path,
				PRNumber:     req.PRNumber,
				Branch:       req.Branch,
				DB:           d.db,
				WorkerID:     workerID,
				MaxAttempts:  d.cfg.Load().Settings.MaxReviewAttempts,
				ExtraFlags:   d.cfg.Load().Settings.ClaudeFlags,
				Providers:    d.filterCopilotIfLimited(provider.FromConfig(d.cfg.Load().Settings.Providers)),
			})
			status := state.WorkerDone
			if res.Error != nil {
				status = state.WorkerFailed
			}
			_ = d.db.UpdateWorkerStatus(workerID, status)
			// Clear NeedsFix only when the fix cycle completed without error.
			// If the fix failed (res.Error != nil), leave NeedsFix set so bellows
			// can detect and dispatch another attempt rather than silently
			// clearing a state that still needs attention.
			if res.Error == nil {
				d.lifecycleMgr.NotifyReviewFixCompleted(req.Anvil, req.PRNumber)
			}

		case lifecycle.ActionCloseBead:
			d.logger.Info("closing bead after merge", "bead", req.BeadID)
			_ = d.closeBead(ctx, req.BeadID, anvilCfg.Path)

		case lifecycle.ActionCleanup:
			d.logger.Info("cleaning up PR after close", "pr", req.PRNumber)
			// Optional: delete remote branch etc.

		case lifecycle.ActionRebase:
			d.logger.Info("rebasing conflicting PR", "pr", req.PRNumber, "bead", req.BeadID)
			_ = d.db.InsertWorker(&state.Worker{
				ID:        workerID,
				BeadID:    req.BeadID,
				Anvil:     req.Anvil,
				Branch:    req.Branch,
				Status:    state.WorkerRunning,
				Phase:     "rebase",
				Title:     d.db.BeadTitle(req.BeadID, req.Anvil),
				PRNumber:  req.PRNumber,
				StartedAt: time.Now(),
			})
			res := rebase.Rebase(ctx, rebase.Params{
				WorktreePath: wt.Path,
				Branch:       req.Branch,
				BaseBranch:   req.BaseBranch,
				BeadID:       req.BeadID,
				AnvilName:    req.Anvil,
				PRNumber:     req.PRNumber,
				DB:           d.db,
				WorkerID:     workerID,
				ExtraFlags:   d.cfg.Load().Settings.ClaudeFlags,
				Providers:    d.filterCopilotIfLimited(provider.FromConfig(d.cfg.Load().Settings.Providers)),
			})
			status := state.WorkerDone
			if !res.Success {
				status = state.WorkerFailed
				d.logger.Error("rebase failed", "pr", req.PRNumber, "bead", req.BeadID, "error", res.Output)
			} else {
				d.logger.Info("rebase succeeded", "pr", req.PRNumber, "bead", req.BeadID)
			}
			_ = d.db.UpdateWorkerStatus(workerID, status)
		}
	}()
}

// handleBellowsNotifications sends webhook notifications for PR status events.
// It is registered as a second bellows event handler alongside lifecycleMgr.HandleEvent.
// Notifications are dispatched asynchronously to avoid blocking Bellows polling.
func (d *Daemon) handleBellowsNotifications(ctx context.Context, event bellows.PREvent) {
	if event.EventType != bellows.EventPRReadyToMerge {
		return
	}
	cfg := d.cfg.Load()
	hasLegacyURLs := cfg != nil && len(cfg.Notifications.PRReadyWebhookURLs) > 0
	disp := d.dispatcher.Load()
	if d.notifier.Load() == nil && disp == nil && !hasLegacyURLs {
		return
	}
	title := d.db.BeadTitle(event.BeadID, event.Anvil)
	go func(anvil, beadID string, prNumber int, prURL, title string) {
		if n := d.notifier.Load(); n != nil {
			n.PRReadyToMerge(ctx, anvil, beadID, prNumber, prURL, title)
		}
		// Dispatch to generic webhook targets (new webhooks[] config).
		if disp != nil {
			msg := fmt.Sprintf("PR #%d ready to merge: %s", prNumber, prURL)
			disp.Dispatch(ctx, notify.EventPRReadyToMerge, beadID, anvil, msg)
		}
		// Legacy pr_ready_webhook_urls support.
		if cfg != nil && cfg.Notifications.Enabled {
			summary := fmt.Sprintf("PR #%d ready to merge: %s (%s)", prNumber, title, anvil)
			if title == "" {
				summary = fmt.Sprintf("PR #%d ready to merge (%s)", prNumber, anvil)
			}
			payload := notify.WebhookPayload{
				Source:  "forge",
				Summary: summary,
				Event:   "pr_ready_to_merge",
				URL:     prURL,
				Repo:    anvil,
				Bead:    beadID,
				PR:      prNumber,
			}
			for _, u := range cfg.Notifications.PRReadyWebhookURLs {
				notify.SendGenericPRReadyToMerge(ctx, u, payload, d.logger)
			}
		}
	}(event.Anvil, event.BeadID, event.PRNumber, event.PRURL, title)
}

// drainPendingAction checks whether a lifecycle action was parked for beadID
// while it was in flight, and if so dispatches it now. Called after
// activeBeads.Delete so the bead is considered free before the action runs.
func (d *Daemon) drainPendingAction(ctx context.Context, beadID string) {
	v, ok := d.pendingActions.LoadAndDelete(beadID)
	if !ok {
		return
	}
	req, ok := v.(lifecycle.ActionRequest)
	if !ok {
		d.logger.Error("pending lifecycle action has unexpected type", "bead", beadID, "valueType", fmt.Sprintf("%T", v))
		return
	}
	d.logger.Info("draining parked lifecycle action", "bead", beadID, "action", req.Action)
	d.handleLifecycleAction(ctx, req)
}

// runStaleDetection periodically checks active workers for stale log files.
// A worker is marked as stalled if its log file has not been modified for longer
// than the configured stale_interval. This does not kill the process — it warns
// the operator via the Needs Attention panel. The check runs approximately at
// half the stale interval, with a minimum of 30s. When stale_interval is 0
// (disabled), the goroutine idles at the 30s default rate so it can react if
// the config is hot-reloaded to a positive value.
func (d *Daemon) runStaleDetection(ctx context.Context) {
	const defaultCheckInterval = 30 * time.Second

	checkIntervalFor := func(staleInterval time.Duration) time.Duration {
		if staleInterval <= 0 {
			return defaultCheckInterval
		}
		if half := staleInterval / 2; half > defaultCheckInterval {
			return half
		}
		return defaultCheckInterval
	}

	checkInterval := checkIntervalFor(d.config().Settings.StaleInterval)
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Re-read stale interval in case config was hot-reloaded
			interval := d.config().Settings.StaleInterval
			if interval <= 0 {
				// Disabled; idle at the default rate to detect re-enablement.
				if checkInterval != defaultCheckInterval {
					ticker.Reset(defaultCheckInterval)
					checkInterval = defaultCheckInterval
				}
				continue
			}
			stalled, err := d.db.StalledWorkers(interval)
			if err != nil {
				d.logger.Warn("stale detection: failed to query workers", "error", err)
				continue
			}
			for _, w := range stalled {
				d.logger.Warn("marking worker as stalled — no log activity",
					"worker", w.ID, "bead", w.BeadID, "anvil", w.Anvil,
					"phase", w.Phase, "stale_interval", interval)
				if err := d.db.MarkWorkerStalled(w.ID); err != nil {
					d.logger.Error("failed to mark worker stalled", "worker", w.ID, "error", err)
					continue
				}
				_ = d.db.LogEvent(state.EventWorkerStalled,
					fmt.Sprintf("Worker %s stalled (no log activity for %s)", w.ID, interval),
					w.BeadID, w.Anvil)
			}

			// Adjust ticker cadence if the stale interval changed.
			if newCheckInterval := checkIntervalFor(interval); newCheckInterval != checkInterval {
				ticker.Reset(newCheckInterval)
				checkInterval = newCheckInterval
			}
		}
	}
}

// pollAndDispatch polls all anvils for ready beads and dispatches workers.
// It is serialized via a try-lock: if a poll is already running (e.g. an IPC
// "refresh" overlapping with the ticker), the second caller returns immediately.
// The in-progress poll already holds a consistent capacity snapshot, so
// skipping the duplicate avoids double-dispatching past max_total_smiths.
func (d *Daemon) pollAndDispatch(ctx context.Context) {
	if !d.pollRunning.CompareAndSwap(false, true) {
		d.logger.Debug("pollAndDispatch already running, skipping concurrent invocation")
		return
	}
	defer d.pollRunning.Store(false)

	// Snapshot config once so the entire poll cycle sees a consistent view even
	// if hot-reload swaps the pointer concurrently.
	cfg := d.cfg.Load()

	d.logger.Info("polling anvils", "count", len(cfg.Anvils))

	// Periodically recover orphaned in-progress beads (every 10 poll cycles).
	// Recovery also runs once at startup (see Start). Running it here catches
	// beads that become orphaned during normal operation — for example, a
	// worker that crashed between claiming a bead in bd and inserting its row
	// into state.db. A minimum-age guard inside RecoverOrphanedBeads prevents
	// it from reopening legitimately in-flight beads on each periodic check.
	if d.pollCount.Add(1)%10 == 0 {
		if recovered := d.shutdownMgr.RecoverOrphanedBeads(); recovered > 0 {
			d.logger.Info("periodic bead recovery", "recovered", recovered)
		}
	}

	maxTotal := cfg.Settings.MaxTotalSmiths
	if maxTotal <= 0 {
		maxTotal = 4
	}

	// Verify each anvil is checked out to main/master before polling.
	// A smith subprocess running git commands in the parent directory can
	// corrupt the working environment for all subsequent workers.
	for name, anvil := range cfg.Anvils {
		if err := verifyAnvilOnMain(ctx, d.logger, anvil.Path); err != nil {
			d.logger.Error("anvil branch check failed — polling will continue but dispatch may be affected",
				"anvil", name, "error", err)
			_ = d.db.LogEvent(state.EventPollError,
				fmt.Sprintf("anvil branch check failed: %v", err), "", name)
		}
	}

	// Always poll so the Hearth TUI queue cache stays current even when all
	// smith slots are occupied. Capacity is checked below before dispatching.
	p := poller.New(cfg.Anvils)
	beads, results := p.Poll(ctx)

	anvilPaths := make(map[string]string, len(cfg.Anvils))
	for name, anvil := range cfg.Anvils {
		anvilPaths[name] = anvil.Path
	}

	// When Crucible is enabled, enrich beads with their blocks (children)
	// so we can detect parent beads that should be dispatched through the
	// Crucible instead of the normal pipeline. This must run before
	// ResolveEpicBranches because epic branch detection now also checks
	// whether a bead blocks an epic-type bead.
	if cfg.Settings.CrucibleEnabled {
		poller.ResolveBlocks(ctx, beads, anvilPaths)
	}

	// Resolve epic branches for beads that belong to an epic. This enriches
	// each bead's EpicBranch field so dispatchBead can branch from and PR to
	// the correct epic branch. Detection works via the parent field (legacy)
	// or via the blocks/blocked_by dependency graph (preferred).
	poller.ResolveEpicBranches(ctx, beads, anvilPaths)

	// Update cache
	d.lastBeadsMu.Lock()
	d.lastBeads = beads
	d.lastBeadsMu.Unlock()

	for _, r := range results {
		if r.Err != nil {
			d.logger.Warn("poll error", "anvil", r.Name, "error", r.Err)
			_ = d.db.LogEvent(state.EventPollError, r.Err.Error(), "", r.Name)
		} else {
			d.logger.Info("poll complete", "anvil", r.Name, "ready", len(r.Beads))
			_ = d.db.LogEvent(state.EventPoll, fmt.Sprintf("Polled anvil: %s (%d ready)", r.Name, len(r.Beads)), "", r.Name)
		}
	}

	// Cache queue in SQLite so the Hearth TUI can read it without polling independently.
	// Only update cache rows for anvils that polled successfully, so failed anvils
	// retain their last-known cached data instead of appearing empty.
	var succeededAnvils []string
	for _, r := range results {
		if r.Err == nil {
			succeededAnvils = append(succeededAnvils, r.Name)
		}
	}
	if len(succeededAnvils) > 0 {
		// Build a set of succeeded anvils for O(1) membership checks.
		succeededSet := make(map[string]struct{}, len(succeededAnvils))
		for _, a := range succeededAnvils {
			succeededSet[a] = struct{}{}
		}

		var cacheItems []state.QueueItem
		for _, b := range beads {
			if b.Labels == nil {
				b.Labels = []string{}
			}
			labelsJSON, _ := json.Marshal(b.Labels)
			section := d.classifyBeadSection(b)
			cacheItems = append(cacheItems, state.QueueItem{
				BeadID:      b.ID,
				Anvil:       b.Anvil,
				Title:       b.Title,
				Description: b.Description,
				Priority:    b.Priority,
				Status:      b.Status,
				Labels:      string(labelsJSON),
				Section:     section,
				Assignee:    b.Assignee,
			})
		}

		// Also include in-progress beads from successful anvils.
		// Build a sub-poller covering only the anvils that polled successfully to
		// avoid running extra bd commands for anvils whose primary poll failed.
		succeededConfigs := make(map[string]config.AnvilConfig, len(succeededAnvils))
		for _, name := range succeededAnvils {
			succeededConfigs[name] = cfg.Anvils[name]
		}
		inProgress, inProgressResults := poller.New(succeededConfigs).PollInProgress(ctx)
		for _, r := range inProgressResults {
			if r.Err != nil {
				d.logger.Warn("poll in-progress error", "anvil", r.Name, "error", r.Err)
			}
		}
		for _, b := range inProgress {
			// Only include in-progress beads from anvils that polled successfully.
			if _, ok := succeededSet[b.Anvil]; !ok {
				continue
			}
			if b.Labels == nil {
				b.Labels = []string{}
			}
			labelsJSON, _ := json.Marshal(b.Labels)
			cacheItems = append(cacheItems, state.QueueItem{
				BeadID:      b.ID,
				Anvil:       b.Anvil,
				Title:       b.Title,
				Description: b.Description,
				Priority:    b.Priority,
				Status:      b.Status,
				Labels:      string(labelsJSON),
				Section:     state.QueueSectionInProgress,
				Assignee:    b.Assignee,
			})
		}

		if err := d.db.ReplaceQueueCacheForAnvils(succeededAnvils, cacheItems); err != nil {
			d.logger.Warn("failed to cache queue", "error", err)
		}
	}

	// Record poll completion time
	d.lastPollTime.Store(time.Now())

	// Preload all clarification-needed bead IDs once per poll cycle to avoid N+1 queries.
	// Fail-closed: if the DB query fails, skip dispatch this cycle so beads that need
	// clarification are not accidentally started during a transient DB error.
	clarSet, clarErr := d.db.ClarificationNeededBeadIDSet()
	if clarErr != nil {
		d.logger.Error("loading clarification-needed set; skipping dispatch this poll cycle", "error", clarErr)
		return
	}

	// Preload needs-human beads (needs_human=1) to avoid dispatching them automatically.
	needsHumanSet, needsHumanErr := d.db.NeedsHumanBeadIDSet()
	if needsHumanErr != nil {
		d.logger.Error("loading needs-human set; skipping dispatch this poll cycle", "error", needsHumanErr)
		return
	}

	// We snapshot DB counts ONCE here and track in-cycle dispatches separately.
	// Previously, the loop re-queried the DB each iteration and subtracted the
	// in-cycle count from the max. This double-counted workers whose goroutines
	// had already called InsertWorker: the DB count included them AND the
	// thisCycle counter reduced the max for them, causing only one dispatch per
	// cycle after the initial batch completed.
	globalActive, err := worker.DispatchTotalActiveCount(d.db)
	if err != nil {
		d.logger.Error("checking global capacity", "error", err)
		return
	}
	if globalActive >= maxTotal {
		d.logger.Info("global smith limit reached, skipping dispatch", "max", maxTotal)
		return
	}

	// Check daily cost limit before dispatching new work.
	costLimit := cfg.Settings.DailyCostLimit
	if costLimit > 0 {
		// Capture date once so both the cost lookup and the event-suppression
		// key use the same day even if midnight rolls over between calls.
		today := time.Now().Format("2006-01-02")
		todayCost, err := d.db.GetTodayCostOn(today)
		if err != nil {
			d.logger.Error("checking daily cost", "error", err)
			return
		}
		if todayCost >= costLimit {
			// Notify once per calendar day — even across daemon restarts.
			// Use a DB-backed check (persists across restarts) with an
			// in-memory fast-path to avoid a DB query on every poll cycle.
			prev, _ := d.costLimitLoggedDate.Load().(string)
			alreadyNotified := prev == today
			if !alreadyNotified {
				// Check DB in case the daemon restarted after notifying today.
				notified, err := d.db.HasEventForDate(state.EventCostLimitHit, today)
				if err != nil {
					// Fail closed: avoid spamming notifications when the DB is unhealthy.
					d.logger.Error("checking cost limit event deduplication", "error", err, "date", today)
					alreadyNotified = true
				} else {
					alreadyNotified = notified
				}
			}
			if !alreadyNotified {
				d.costLimitLoggedDate.Store(today)
				_ = d.db.LogEvent(state.EventCostLimitHit,
					fmt.Sprintf("Daily cost $%.2f reached limit $%.2f — dispatch paused", todayCost, costLimit),
					"", "")
				d.logger.Warn("daily cost limit reached, dispatch paused",
					"cost", fmt.Sprintf("$%.2f", todayCost),
					"limit", fmt.Sprintf("$%.2f", costLimit))

				// Fire daily_cost notifications — once per day when the limit is hit.
				inTokens, outTokens, _, _, _, _, err := d.db.GetDailyCost(today)
				if err != nil {
					d.logger.Error("failed to get daily cost for notification", "error", err, "date", today)
					// Proceed with zero counts rather than skipping the notification entirely
					inTokens, outTokens = 0, 0
				}
				disp := d.dispatcher.Load()
				go func(date string, cost, limit float64, inT, outT int) {
					notifCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					if n := d.notifier.Load(); n != nil {
						n.DailyCost(notifCtx, date, cost, limit, int64(inT), int64(outT))
					}
					if disp != nil {
						msg := fmt.Sprintf("Daily cost $%.2f reached limit $%.2f", cost, limit)
						disp.Dispatch(notifCtx, notify.EventDailyCost, "", "", msg)
					}
				}(today, todayCost, costLimit, inTokens, outTokens)
			} else if prev != today {
				// Update in-memory cache to skip the DB query on future poll cycles.
				d.costLimitLoggedDate.Store(today)
			}
			return
		}
	}

	// Snapshot per-anvil active counts so we don't re-query the DB each
	// iteration (avoids double-counting with the thisCycleAnvil counter).
	anvilActive := make(map[string]int)
	thisCycleTotal := 0
	thisCycleAnvil := make(map[string]int)

	for _, bead := range beads {
		// Atomically reserve this bead's slot; skip if another goroutine already
		// claimed it (e.g. a concurrent manual run_bead dispatch). Using
		// LoadOrStore closes the race that existed between Load and the later
		// Store: a concurrent run_bead could slip in between those two calls.
		if _, alreadyInFlight := d.activeBeads.LoadOrStore(bead.ID, true); alreadyInFlight {
			continue
		}

		// Skip beads that need clarification (analogous to needs_human)
		if _, needed := clarSet[bead.ID+"\x00"+bead.Anvil]; needed {
			d.activeBeads.Delete(bead.ID)
			continue
		}

		// Skip beads that need human attention (needs_human=1)
		if _, broken := needsHumanSet[bead.ID+"\x00"+bead.Anvil]; broken {
			d.activeBeads.Delete(bead.ID)
			continue
		}

		// Skip beads that already have an open PR (bellows should handle them)
		if hasPR, err := d.db.HasOpenPRForBead(bead.ID, bead.Anvil); err == nil && hasPR {
			d.logger.Debug("skipping bead with open PR", "bead", bead.ID)
			d.activeBeads.Delete(bead.ID)
			continue
		}

		anvilCfg, ok := cfg.Anvils[bead.Anvil]
		if !ok || anvilCfg.Path == "" {
			d.activeBeads.Delete(bead.ID)
			continue
		}

		// Apply auto-dispatch filtering
		if !shouldDispatch(bead, anvilCfg) {
			d.activeBeads.Delete(bead.ID)
			continue
		}

		maxSmiths := anvilCfg.MaxSmiths
		if maxSmiths <= 0 {
			maxSmiths = 1
		}

		// Check per-anvil capacity using snapshot + in-cycle count
		if _, ok := anvilActive[bead.Anvil]; !ok {
			cnt, err := worker.DispatchActiveCount(d.db, bead.Anvil)
			if err != nil {
				d.logger.Error("checking per-anvil capacity", "anvil", bead.Anvil, "error", err)
				d.activeBeads.Delete(bead.ID)
				anvilActive[bead.Anvil] = maxSmiths // treat as at-capacity for this cycle
				continue
			}
			anvilActive[bead.Anvil] = cnt
		}
		if anvilActive[bead.Anvil]+thisCycleAnvil[bead.Anvil] >= maxSmiths {
			d.activeBeads.Delete(bead.ID)
			continue
		}

		// Check global capacity using snapshot + in-cycle count
		if globalActive+thisCycleTotal >= maxTotal {
			d.activeBeads.Delete(bead.ID)
			break
		}

		// Claim the bead atomically before dispatching
		if err := d.claimBead(ctx, bead.ID, anvilCfg.Path); err != nil {
			d.logger.Warn("failed to claim bead", "bead", bead.ID, "error", err)
			d.recordDispatchFailure(bead.ID, bead.Anvil, fmt.Sprintf("claim failed: %v", err))
			d.activeBeads.Delete(bead.ID)
			continue
		}

		// Insert a pending worker row immediately after claiming so that orphan
		// recovery can identify this as a Forge-owned bead even if the process
		// crashes before the pipeline inserts the running row (the known
		// claim→worktree crash window). The pipeline overwrites this row via
		// INSERT OR REPLACE when it starts.
		claimWorkerID := d.insertPendingWorker(bead.ID, bead.Anvil, bead.Title)

		thisCycleAnvil[bead.Anvil]++
		thisCycleTotal++
		d.wg.Add(1)
		go d.dispatchBead(ctx, bead, anvilCfg, claimWorkerID)
	}

	// Optionally log a summary of this poll cycle's dispatch activity.
	if len(thisCycleAnvil) > 0 {
		d.logger.Debug("poll cycle dispatch summary", "anvil_dispatch_counts", thisCycleAnvil)
	}
}

// dispatchBead runs the full pipeline for a single bead in a goroutine.
// claimWorkerID is the ID of the pending worker row inserted at claim time;
// it is passed into the pipeline so the running row overwrites the pending one.
func (d *Daemon) dispatchBead(ctx context.Context, bead poller.Bead, anvilCfg config.AnvilConfig, claimWorkerID string) {
	defer d.wg.Done()
	// Drain order: activeBeads.Delete runs first (registered last → LIFO),
	// then drainPendingAction fires so any parked lifecycle action sees the bead as free.
	// Skip draining during shutdown to avoid wg.Add after wg.Wait.
	defer func() {
		if ctx.Err() == nil {
			d.drainPendingAction(ctx, bead.ID)
		}
	}()
	defer d.activeBeads.Delete(bead.ID)

	d.logger.Info("dispatching bead", "bead", bead.ID, "anvil", bead.Anvil, "title", bead.Title)

	// Re-verify the anvil is on main/master immediately before spawning any
	// subprocess. This catches race conditions where the branch changed between
	// the poll-loop check and actual dispatch.
	if err := verifyAnvilOnMain(ctx, d.logger, anvilCfg.Path); err != nil {
		d.logger.Error("anvil branch check failed at dispatch time, aborting bead",
			"bead", bead.ID, "anvil", bead.Anvil, "error", err)
		d.recordDispatchFailure(bead.ID, bead.Anvil,
			fmt.Sprintf("anvil branch check failed: %v", err))
		return
	}

	// Handle epic beads: when the Crucible is enabled and the bead has children,
	// fall through to the Crucible path which handles branch creation, child
	// orchestration, and final PR. The legacy path only applies to epics that
	// either have no children or when the Crucible is disabled.
	if poller.IsEpicBead(bead) && !(d.cfg.Load().Settings.CrucibleEnabled && crucible.IsCrucibleCandidate(bead)) {
		epicBranch := poller.ExtractParentBranch(bead)
		if epicBranch != "" {
			d.logger.Info("creating epic branch", "bead", bead.ID, "branch", epicBranch)
			if err := d.worktreeMgr.CreateEpicBranch(ctx, anvilCfg.Path, epicBranch); err != nil {
				d.logger.Error("failed to create epic branch", "bead", bead.ID, "branch", epicBranch, "error", err)
				d.recordDispatchFailure(bead.ID, bead.Anvil, fmt.Sprintf("epic branch creation failed: %v", err))
				return
			}
			_ = d.db.LogEvent(state.EventBeadClaimed,
				fmt.Sprintf("Epic branch %q created for %s", epicBranch, bead.ID),
				bead.ID, bead.Anvil)
			d.logger.Info("epic branch created", "bead", bead.ID, "branch", epicBranch)
			// Epic bead stays in_progress — child beads will work on the branch.
			// Do not close the epic or run the pipeline.
			return
		}
	}

	// Handle Crucible beads: parent beads that block children. The Crucible
	// orchestrates all children on a feature branch, merging each child's PR
	// before dispatching the next, then creates a final PR to main.
	//
	// When the schematic is enabled, we ask it to inspect the parent+children
	// relationship before committing to crucible mode. This prevents simple
	// sequencing dependencies (bd dep add) from triggering a full feature-branch
	// orchestration when the beads are actually independent.
	if d.cfg.Load().Settings.CrucibleEnabled && crucible.IsCrucibleCandidate(bead) {
		// Run schematic crucible check if enabled — determines whether the
		// children genuinely need orchestration or are just sequenced.
		if d.cfg.Load().Settings.SchematicEnabled {
			_ = d.db.UpdateWorkerPhase(claimWorkerID, "schematic")
			_ = d.db.LogEvent(state.EventSchematicStarted,
				fmt.Sprintf("Crucible check: inspecting %s with %d children", bead.ID, len(bead.Blocks)),
				bead.ID, bead.Anvil)

			schemCfg := schematic.DefaultConfig()
			schemCfg.Enabled = true
			schemCfg.ExtraFlags = d.cfg.Load().Settings.ClaudeFlags

			smithProviders := d.cfg.Load().Settings.SmithProviders
			if len(smithProviders) == 0 {
				smithProviders = d.cfg.Load().Settings.Providers
			}
			providers := d.filterCopilotIfLimited(provider.FromConfig(smithProviders))

			// Fetch child details for the prompt
			var children []schematic.ChildBead
			for _, childID := range bead.Blocks {
				child, err := crucible.FetchBead(ctx, childID, anvilCfg.Path)
				if err != nil {
					d.logger.Warn("crucible check: failed to fetch child", "child", childID, "error", err)
					continue
				}
				children = append(children, schematic.ChildBead{
					ID:          child.ID,
					Title:       child.Title,
					Description: child.Description,
				})
			}

			checkResult := schematic.RunCrucibleCheck(ctx, schemCfg, bead, children, anvilCfg.Path, providers[0])

			if checkResult.Quota != nil {
				if err := d.db.UpsertProviderQuota(string(providers[0].Kind), checkResult.Quota); err != nil {
					d.logger.Warn("failed to update provider quota from crucible check", "error", err)
				}
			}

			_ = d.db.LogEvent(state.EventSchematicDone,
				fmt.Sprintf("Crucible check: %s → needs_crucible=%v (%s)",
					bead.ID, checkResult.NeedsCrucible, checkResult.Reason),
				bead.ID, bead.Anvil)

			if !checkResult.NeedsCrucible {
				d.logger.Info("schematic says standalone dispatch", "bead", bead.ID, "reason", checkResult.Reason)
				// Clear epic branch so the normal pipeline creates a worktree
				// from main instead of a non-existent feature branch.
				bead.EpicBranch = ""
				goto normalPipeline
			}
			d.logger.Info("schematic confirms crucible needed", "bead", bead.ID, "reason", checkResult.Reason)
		}

		_ = d.db.UpdateWorkerPhase(claimWorkerID, "crucible")
		d.logger.Info("dispatching crucible", "bead", bead.ID, "children", len(bead.Blocks))

		smithProviderSpecs := d.cfg.Load().Settings.SmithProviders
		if len(smithProviderSpecs) == 0 {
			smithProviderSpecs = d.cfg.Load().Settings.Providers
		}
		crucibleParams := crucible.Params{
			DB:                        d.db,
			Logger:                    d.logger,
			WorktreeManager:           d.worktreeMgr,
			PromptBuilder:             d.promptBuilder,
			ParentBead:                bead,
			AnvilName:                 bead.Anvil,
			AnvilConfig:               anvilCfg,
			ExtraFlags:                d.cfg.Load().Settings.ClaudeFlags,
			Providers:                 d.filterCopilotIfLimited(provider.FromConfig(smithProviderSpecs)),
			GoRaceDetection:           d.resolveGoRaceDetection(anvilCfg),
			SmithTimeout:              d.cfg.Load().Settings.SmithTimeout,
			AutoMergeCrucibleChildren: d.cfg.Load().Settings.IsAutoMergeCrucibleChildren(),
			MaxPipelineIterations:     d.cfg.Load().Settings.MaxPipelineIterations,
			StatusCallback: func(s crucible.Status) {
				d.crucibleStatuses.Store(bead.Anvil+"/"+bead.ID, s)
			},
		}
		if d.cfg.Load().Settings.SchematicEnabled {
			wordThreshold := d.cfg.Load().Settings.SchematicWordThreshold
			if wordThreshold <= 0 {
				wordThreshold = 100
			}
			schemCfg := schematic.DefaultConfig()
			schemCfg.Enabled = true
			schemCfg.WordThreshold = wordThreshold
			schemCfg.ExtraFlags = d.cfg.Load().Settings.ClaudeFlags
			crucibleParams.SchematicConfig = &schemCfg
		}

		// IMPORTANT: derive crucibleCtx from context.Background(), NOT from the
		// daemon's ctx. This ensures that a graceful shutdown (SIGINT/SIGTERM)
		// does not cancel in-flight Crucible children mid-pipeline, which could
		// result in lost partially-completed child PRs. Each child pipeline
		// manages its own smith timeout internally.
		result := crucible.Run(context.Background(), crucibleParams)
		// Clean up completed crucible status after a short delay so
		// the TUI can observe the terminal "complete" state before removal.
		defer func() {
			if result.Error == nil {
				crucibleKey := bead.Anvil + "/" + bead.ID
				time.AfterFunc(2*time.Second, func() {
					d.crucibleStatuses.Delete(crucibleKey)
				})
			}
			// On error/pause, keep the status visible so the TUI shows it.
		}()
		if result.Error != nil {
			d.logger.Error("crucible failed", "bead", bead.ID, "error", result.Error)
			if result.PausedChildID != "" {
				d.recordDispatchFailure(bead.ID, bead.Anvil,
					fmt.Sprintf("crucible paused: child %s failed", result.PausedChildID))
			} else {
				d.recordDispatchFailure(bead.ID, bead.Anvil,
					fmt.Sprintf("crucible error: %v", result.Error))
			}
			return
		}
		if result.Success {
			_ = d.db.ClearRetry(bead.ID, bead.Anvil)
			finalPRURL := ""
			if result.FinalPR != nil {
				finalPRURL = result.FinalPR.URL
			}
			d.logger.Info("crucible completed",
				"bead", bead.ID,
				"children", result.ChildrenDone,
				"final_pr", finalPRURL)
			// Parent bead stays open — bellows will close it when the final PR merges.
			_ = d.db.LogEvent(state.EventCrucibleComplete,
				fmt.Sprintf("Crucible %s complete: %d children merged, final PR created",
					bead.ID, result.ChildrenDone),
				bead.ID, bead.Anvil)
		}
		return
	}

normalPipeline:
	// Apply smith timeout.
	// IMPORTANT: derive pipelineCtx from context.Background(), NOT from the
	// daemon's ctx. This ensures that a graceful shutdown (SIGINT/SIGTERM)
	// does not cancel in-flight pipelines mid-run. The smith subprocess is
	// killed explicitly by GracefulShutdown(); post-smith work (warden, PR
	// creation, bead closing) should be allowed to complete so PRs are not
	// lost. The smith timeout still provides the outer deadline.
	smithTimeout := d.cfg.Load().Settings.SmithTimeout
	if smithTimeout <= 0 {
		smithTimeout = 30 * time.Minute
	}
	pipelineCtx, cancel := context.WithTimeout(context.Background(), smithTimeout)
	defer cancel()

	// Build pipeline params, optionally enabling Schematic pre-worker.
	// Use smith_providers for the dispatch pipeline when configured; fall back
	// to the main providers list. This lets smiths run a more capable model
	// while lifecycle workers (cifix, reviewfix) use a lighter model.
	smithProviderSpecs := d.cfg.Load().Settings.SmithProviders
	if len(smithProviderSpecs) == 0 {
		smithProviderSpecs = d.cfg.Load().Settings.Providers
	}
	pipelineParams := pipeline.Params{
		DB:              d.db,
		WorktreeManager: d.worktreeMgr,
		PromptBuilder:   d.promptBuilder,
		AnvilName:       bead.Anvil,
		AnvilConfig:     anvilCfg,
		Bead:            bead,
		ExtraFlags:      d.cfg.Load().Settings.ClaudeFlags,
		GoRaceDetection: d.resolveGoRaceDetection(anvilCfg),
		Providers:       d.filterCopilotIfLimited(provider.FromConfig(smithProviderSpecs)),
		Notifier:        d.notifier.Load(),
		BaseBranch:      bead.EpicBranch,
		WorkerID:        claimWorkerID,
		MaxIterations:   d.cfg.Load().Settings.MaxPipelineIterations,
	}

	// If this bead has had previous dispatch failures, reset the worktree
	// branch to the base ref so the retry starts from a clean state. This
	// prevents inheriting junk commits from a failed pipeline run.
	if retry, err := d.db.GetRetry(bead.ID, bead.Anvil); err == nil && retry != nil && retry.DispatchFailures > 0 {
		pipelineParams.ResetBranch = true
		d.logger.Info("resetting worktree branch for retry", "bead", bead.ID, "failures", retry.DispatchFailures)
	}
	if d.cfg.Load().Settings.SchematicEnabled {
		wordThreshold := d.cfg.Load().Settings.SchematicWordThreshold
		if wordThreshold <= 0 {
			wordThreshold = 100
		}
		schemCfg := schematic.DefaultConfig()
		schemCfg.Enabled = true
		schemCfg.WordThreshold = wordThreshold
		schemCfg.ExtraFlags = d.cfg.Load().Settings.ClaudeFlags
		pipelineParams.SchematicConfig = &schemCfg
	}

	outcome := pipeline.Run(pipelineCtx, pipelineParams)

	if outcome.Error != nil {
		if outcome.RateLimited {
			// Bead was released back to open by the pipeline. Wait for the
			// configured backoff so this goroutine holds the activeBeads slot
			// and prevents an immediate re-dispatch by the next poll tick.
			backoff := d.cfg.Load().Settings.RateLimitBackoff
			if backoff <= 0 {
				backoff = 5 * time.Minute
			}
			d.logger.Warn("all providers rate limited; bead released to open, backing off",
				"bead", bead.ID, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
			}
			return
		}
		d.logger.Error("pipeline error", "bead", bead.ID, "error", outcome.Error)
		d.recordDispatchFailure(bead.ID, bead.Anvil, fmt.Sprintf("pipeline error: %v", outcome.Error))
		return
	}

	if !outcome.Success {
		if outcome.Decomposed {
			// Dispatch bead_decomposed to generic webhook targets.
			if disp := d.dispatcher.Load(); disp != nil {
				childCount := 0
				if outcome.SchematicResult != nil {
					childCount = len(outcome.SchematicResult.SubBeads)
				}
				dispatchMsg := fmt.Sprintf("Bead decomposed into %d sub-beads", childCount)
				go func(beadID, anvil, msg string) {
					notifCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					disp.Dispatch(notifCtx, notify.EventBeadDecomposed, beadID, anvil, msg)
				}(bead.ID, bead.Anvil, dispatchMsg)
			}
			d.applyDecomposedOutcome(bead, anvilCfg, outcome.SchematicResult)
			return
		}
		if outcome.NeedsHuman {
			// Bead was released back to open. Record as dispatch failure so the
			// circuit breaker can trip after repeated attempts.
			reason := "Smith produced no diff, needs human attention"
			if outcome.SchematicResult != nil && outcome.SchematicResult.Reason != "" {
				reason = outcome.SchematicResult.Reason
			} else if outcome.ReviewResult != nil && outcome.ReviewResult.Summary != "" && outcome.ReviewResult.NoDiff {
				reason = "Warden rejected (no diff): " + outcome.ReviewResult.Summary
			} else if outcome.SmithResult != nil {
				if r := pipeline.ExtractNeedsHuman(outcome.SmithResult.FullOutput); r != "" {
					reason = "Smith escalated: " + r
				}
			}

			d.recordDispatchFailure(bead.ID, bead.Anvil, reason)
			// Hold the activeBeads slot for a full poll interval so the bead is not
			// immediately re-dispatched before a human can investigate.
			holdOff := d.cfg.Load().Settings.PollInterval
			if holdOff <= 0 {
				holdOff = DefaultPollInterval
			}
			d.logger.Warn("bead released to open — Smith produced no diff, needs human attention; holding off re-dispatch",
				"bead", bead.ID, "holdoff", holdOff)
			select {
			case <-time.After(holdOff):
			case <-ctx.Done():
			}
		} else {
			d.logger.Warn("pipeline did not succeed", "bead", bead.ID, "verdict", outcome.Verdict)
			d.recordDispatchFailure(bead.ID, bead.Anvil, fmt.Sprintf("pipeline failed: %s", outcome.Verdict))
		}
		return
	}

	// Pipeline succeeded — clear any prior dispatch failures.
	_ = d.db.ClearRetry(bead.ID, bead.Anvil)
	d.logger.Info("pipeline succeeded", "bead", bead.ID, "branch", outcome.Branch, "iterations", outcome.Iterations)

	// Create PR — run gh from the main repo dir since the branch is already pushed.
	// Use pipelineCtx (background-derived) so PR creation succeeds even during
	// graceful shutdown.
	// Build a change summary for the PR description.
	// Priority:
	// 1. ChangelogSummary (bullets from Smith's changelog fragment)
	// 2. ReviewResult.Summary (Warden's one-line summary)
	var changeSummary string
	if outcome.ChangelogSummary != "" {
		changeSummary = outcome.ChangelogSummary
	} else if outcome.ReviewResult != nil && outcome.ReviewResult.Summary != "" {
		changeSummary = outcome.ReviewResult.Summary
	}

	pr, err := ghpr.Create(pipelineCtx, ghpr.CreateParams{
		WorktreePath:    anvilCfg.Path,
		BeadID:          bead.ID,
		Title:           fmt.Sprintf("%s (%s)", bead.Title, bead.ID),
		Branch:          outcome.Branch,
		Base:            bead.EpicBranch, // empty = use ghpr.Create default base (currently "main", still passes --base)
		AnvilName:       bead.Anvil,
		DB:              d.db,
		BeadTitle:       bead.Title,
		BeadDescription: bead.Description,
		BeadType:        bead.IssueType,
		ChangeSummary:   changeSummary,
	})
	if err != nil {
		d.logger.Error("PR creation failed", "bead", bead.ID, "error", err)
		return
	}

	d.logger.Info("PR created", "bead", bead.ID, "pr", pr.URL)

	// Send PR-created notifications.
	disp := d.dispatcher.Load()
	go func(anvil, beadID, prURL, prTitle string, prNumber int, dur time.Duration) {
		notifCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if n := d.notifier.Load(); n != nil {
			n.PRCreated(notifCtx, anvil, beadID, prNumber, prURL, prTitle)
		}
		if disp != nil {
			msg := fmt.Sprintf("PR #%d created: %s", prNumber, prURL)
			disp.Dispatch(notifCtx, notify.EventPRCreated, beadID, anvil, msg)
		}
		// Worker-done notification fires here alongside PR creation since
		// the pipeline worker is complete at this point.
		if n := d.notifier.Load(); n != nil {
			n.WorkerDone(notifCtx, anvil, beadID, claimWorkerID, dur)
		}
		if disp != nil {
			msg := fmt.Sprintf("Worker completed in %s; PR #%d created", dur.Round(time.Second), prNumber)
			disp.Dispatch(notifCtx, notify.EventWorkerDone, beadID, anvil, msg)
		}
	}(bead.Anvil, bead.ID, pr.URL, bead.Title, pr.Number, outcome.Duration)

	// Close the bead — unless other beads depend on it. When dependents exist,
	// closing now would unblock them before this PR is merged, causing them to
	// build on stale main. Bellows will close the bead after the PR merges.
	if len(bead.Blocks) > 0 {
		d.logger.Info("bead has dependents, deferring close until PR merges", "bead", bead.ID, "dependents", len(bead.Blocks))
	} else if err := d.closeBead(pipelineCtx, bead.ID, anvilCfg.Path); err != nil {
		d.logger.Warn("failed to close bead", "bead", bead.ID, "error", err)
	}
}

// insertPendingWorker writes a minimal WorkerPending row to state.db immediately
// after claiming a bead. This closes the crash window between bd marking the
// bead in_progress and the pipeline inserting its running worker row: orphan
// recovery uses HasWorkerRecord to distinguish Forge-claimed beads from beads
// owned by humans or other tools, so without this row a crash during worktree
// creation would leave the bead stuck in_progress forever.
//
// Returns the generated worker ID so the caller can pass it to the pipeline,
// which will overwrite this row (via INSERT OR REPLACE) once the full running
// record is available.
func (d *Daemon) insertPendingWorker(beadID, anvilName, title string) string {
	workerID := fmt.Sprintf("%s-%s-%d", anvilName, beadID, time.Now().Unix())
	w := &state.Worker{
		ID:        workerID,
		BeadID:    beadID,
		Anvil:     anvilName,
		Status:    state.WorkerPending,
		Title:     title,
		StartedAt: time.Now(),
	}
	if err := d.db.InsertWorker(w); err != nil {
		d.logger.Warn("failed to insert pending worker row at claim time", "bead", beadID, "error", err)
	}
	return workerID
}

// claimBead marks a bead as in_progress via bd update --claim.
func (d *Daemon) claimBead(ctx context.Context, beadID, anvilPath string) error {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "bd", "update", beadID, "--status=in_progress", "--json"))
	cmd.Dir = anvilPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bd update %s --status=in_progress --json: %w\n%s", beadID, err, out)
	}
	return nil
}

// crucibleParentTitle looks up the title for a crucible's parent bead
// from the last polled beads. Returns the bead ID if not found.
func (d *Daemon) crucibleParentTitle(parentID string) string {
	d.lastBeadsMu.RLock()
	defer d.lastBeadsMu.RUnlock()
	for _, b := range d.lastBeads {
		if b.ID == parentID {
			return b.Title
		}
	}
	return parentID
}

// closeBead marks a bead as closed via bd close.
func (d *Daemon) closeBead(ctx context.Context, beadID, anvilPath string) error {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "bd", "close", beadID, "--reason=Implemented by Forge", "--json"))
	cmd.Dir = anvilPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bd close %s --json: %w\n%s", beadID, err, out)
	}
	return nil
}

// handleIPC processes incoming IPC commands from CLI/TUI clients.
func (d *Daemon) handleIPC(cmd ipc.Command) ipc.Response {
	switch cmd.Type {
	case "status":
		workers, _ := d.db.ActiveWorkers()
		prs, _ := d.db.OpenPRs()
		quotas, _ := d.db.GetAllProviderQuotas()
		todayCost, _ := d.db.GetTodayCost()
		costLimit := d.cfg.Load().Settings.DailyCostLimit
		copilotReqs, _ := d.db.GetTodayCopilotRequests()
		copilotLimit := d.cfg.Load().Settings.CopilotDailyRequestLimit
		queueCount, _ := d.db.QueueCount()
		lastPoll := "n/a"
		if t, ok := d.lastPollTime.Load().(time.Time); ok && !t.IsZero() {
			lastPoll = time.Since(t).Round(time.Second).String() + " ago"
		}
		payload := ipc.StatusPayload{
			Running:                true,
			PID:                    os.Getpid(),
			Uptime:                 time.Since(d.startTime).Round(time.Second).String(),
			Workers:                len(workers),
			QueueSize:              queueCount,
			OpenPRs:                len(prs),
			LastPoll:               lastPoll,
			Quotas:                 quotas,
			DailyCost:              todayCost,
			DailyCostLimit:         costLimit,
			CostLimitPaused:        costLimit > 0 && todayCost >= costLimit,
			CopilotPremiumRequests: copilotReqs,
			CopilotRequestLimit:    copilotLimit,
			CopilotLimitReached:    copilotLimit > 0 && copilotReqs >= float64(copilotLimit),
		}
		data, _ := json.Marshal(payload)
		return ipc.Response{Type: "status", Payload: data}

	case "crucibles":
		var items []ipc.CrucibleStatusItem
		d.crucibleStatuses.Range(func(key, value any) bool {
			s := value.(crucible.Status)
			items = append(items, ipc.CrucibleStatusItem{
				ParentID:          s.ParentID,
				ParentTitle:       d.crucibleParentTitle(s.ParentID),
				Anvil:             s.Anvil,
				Branch:            s.Branch,
				Phase:             s.Phase,
				TotalChildren:     s.TotalChildren,
				CompletedChildren: s.CompletedChildren,
				CurrentChild:      s.CurrentChild,
				StartedAt:         s.StartedAt.Format("15:04:05"),
			})
			return true
		})
		sort.Slice(items, func(i, j int) bool {
			if items[i].StartedAt != items[j].StartedAt {
				return items[i].StartedAt < items[j].StartedAt
			}
			return items[i].ParentID < items[j].ParentID
		})
		resp := ipc.CruciblesResponse{Crucibles: items}
		data, _ := json.Marshal(resp)
		return ipc.Response{Type: "ok", Payload: data}

	case "kill_worker":
		var kp ipc.KillWorkerPayload
		if err := json.Unmarshal(cmd.Payload, &kp); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": "invalid kill payload"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if kp.PID > 0 {
			proc, err := os.FindProcess(kp.PID)
			if err == nil {
				if runtime.GOOS == "windows" {
					// Windows does not support SIGINT via Signal; use Kill instead.
					_ = proc.Kill()
				} else {
					_ = proc.Signal(syscall.SIGINT)
				}
			}
		}
		_ = d.db.UpdateWorkerStatus(kp.WorkerID, state.WorkerFailed)
		data, _ := json.Marshal(map[string]string{"killed": kp.WorkerID})
		return ipc.Response{Type: "ok", Payload: data}

	case "shutdown":
		go func() {
			if d.cancel != nil {
				d.cancel()
			}
		}()
		data, _ := json.Marshal(map[string]string{"message": "shutting down"})
		return ipc.Response{Type: "ok", Payload: data}

	case "refresh":
		go func() {
			d.pollAndDispatch(d.runCtx)
			if d.bellowsMonitor != nil {
				d.bellowsMonitor.Refresh()
			}
		}()
		data, _ := json.Marshal(map[string]string{"message": "poll triggered"})
		return ipc.Response{Type: "ok", Payload: data}

	case "subscribe":
		data, _ := json.Marshal(map[string]string{"message": "subscribed"})
		return ipc.Response{Type: "ok", Payload: data}

	case "queue":
		data, _ := json.Marshal(map[string]string{"message": "not yet implemented"})
		return ipc.Response{Type: "ok", Payload: data}

	case "run_bead":
		var rp ipc.RunBeadPayload
		if err := json.Unmarshal(cmd.Payload, &rp); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": "invalid run_bead payload"})
			return ipc.Response{Type: "error", Payload: msg}
		}

		// Search for the bead in cache first
		var targetBead *poller.Bead
		d.lastBeadsMu.RLock()
		for _, b := range d.lastBeads {
			if b.ID == rp.BeadID && (rp.Anvil == "" || b.Anvil == rp.Anvil) {
				tb := b // copy
				targetBead = &tb
				break
			}
		}
		d.lastBeadsMu.RUnlock()

		// If not in cache, poll as fallback
		if targetBead == nil {
			d.logger.Info("bead not in cache, polling anvils", "bead", rp.BeadID)
			p := poller.New(d.cfg.Load().Anvils)
			var beads []poller.Bead
			var pollErrors []string

			if rp.Anvil != "" {
				var err error
				beads, err = p.PollSingle(context.Background(), rp.Anvil)
				if err != nil {
					msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("anvil %q not found or poll failed: %v", rp.Anvil, err)})
					return ipc.Response{Type: "error", Payload: msg}
				}
			} else {
				var results []poller.AnvilResult
				beads, results = p.Poll(context.Background())
				for _, r := range results {
					if r.Err != nil {
						pollErrors = append(pollErrors, fmt.Sprintf("%s: %v", r.Name, r.Err))
					}
				}
			}

			for _, b := range beads {
				if b.ID == rp.BeadID {
					tb := b
					targetBead = &tb
					break
				}
			}

			if targetBead == nil {
				errorMsg := fmt.Sprintf("bead %q not found or not ready", rp.BeadID)
				if len(pollErrors) > 0 {
					errorMsg += fmt.Sprintf(" (also %d anvils failed to poll: %v)", len(pollErrors), pollErrors)
				}
				msg, _ := json.Marshal(map[string]string{"message": errorMsg})
				return ipc.Response{Type: "error", Payload: msg}
			}
		}

		// Skip if bead is already in flight
		if _, inFlight := d.activeBeads.LoadOrStore(targetBead.ID, true); inFlight {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("bead %q is already in flight", targetBead.ID)})
			return ipc.Response{Type: "error", Payload: msg}
		}

		// Block beads that need clarification (consistent with auto-dispatch behavior)
		needed, err := d.isBeadClarificationNeeded(targetBead.ID, targetBead.Anvil)
		if err != nil {
			d.activeBeads.Delete(targetBead.ID)
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("failed to check clarification status for %q: %v", targetBead.ID, err)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if needed {
			d.activeBeads.Delete(targetBead.ID)
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("bead %q needs clarification; use 'forge queue unclarify --anvil %s %s' to clear", targetBead.ID, targetBead.Anvil, targetBead.ID)})
			return ipc.Response{Type: "error", Payload: msg}
		}

		// Manual dispatch resets the dispatch circuit breaker so the bead can be retried,
		// but only if the bead has recorded dispatch failures (i.e., the breaker was involved).
		if retry, err := d.db.GetRetry(targetBead.ID, targetBead.Anvil); err == nil && retry != nil && retry.DispatchFailures > 0 {
			_ = d.db.ResetDispatchFailures(targetBead.ID, targetBead.Anvil)
		}

		// Resolve epic branch if not already populated from the poll cache.
		// Detection works via parent field (legacy) or blocks (preferred).
		// When Crucible is enabled we first resolve blocks so that epic
		// relationships can be derived even when the original ready JSON
		// omitted the blocks field.
		if targetBead.EpicBranch == "" {
			anvilPath := d.cfg.Load().Anvils[targetBead.Anvil].Path
			if anvilPath != "" {
				paths := map[string]string{targetBead.Anvil: anvilPath}
				single := []poller.Bead{*targetBead}
				// First resolve blocks for this bead so epic relationships can be
				// derived even when the original ready JSON omitted `blocks`.
				if d.cfg.Load().Settings.CrucibleEnabled {
					poller.ResolveBlocks(context.Background(), single, paths)
				}
				poller.ResolveEpicBranches(context.Background(), single, paths)
				targetBead.EpicBranch = single[0].EpicBranch
			}
		}

		// Dispatch immediately regardless of auto_dispatch setting (but respect capacity)
		anvilCfg := d.cfg.Load().Anvils[targetBead.Anvil]

		// Check capacity
		maxSmiths := anvilCfg.MaxSmiths
		if maxSmiths <= 0 {
			maxSmiths = 1
		}
		canSpawnAnvil, err := worker.CanSpawn(d.db, targetBead.Anvil, maxSmiths)
		if err != nil {
			d.activeBeads.Delete(targetBead.ID)
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("checking anvil capacity: %v", err)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if !canSpawnAnvil {
			d.activeBeads.Delete(targetBead.ID)
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("anvil %q capacity reached (max %d smiths)", targetBead.Anvil, maxSmiths)})
			return ipc.Response{Type: "error", Payload: msg}
		}

		maxTotal := d.cfg.Load().Settings.MaxTotalSmiths
		if maxTotal <= 0 {
			maxTotal = 4
		}
		canSpawnGlobal, err := worker.CanSpawnGlobal(d.db, maxTotal)
		if err != nil {
			d.activeBeads.Delete(targetBead.ID)
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("checking global capacity: %v", err)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if !canSpawnGlobal {
			d.activeBeads.Delete(targetBead.ID)
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("global capacity reached (max %d smiths)", maxTotal)})
			return ipc.Response{Type: "error", Payload: msg}
		}

		// Claim the bead
		if err := d.claimBead(context.Background(), targetBead.ID, anvilCfg.Path); err != nil {
			d.activeBeads.Delete(targetBead.ID)
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("failed to claim bead: %v", err)})
			return ipc.Response{Type: "error", Payload: msg}
		}

		// Insert a pending worker row immediately after claiming so orphan
		// recovery can identify this as Forge-owned in the claim→worktree window.
		claimWorkerID := d.insertPendingWorker(targetBead.ID, targetBead.Anvil, targetBead.Title)

		d.wg.Add(1)
		go d.dispatchBead(context.Background(), *targetBead, anvilCfg, claimWorkerID)

		data, _ := json.Marshal(map[string]string{"message": "dispatched"})
		return ipc.Response{Type: "ok", Payload: data}

	case "set_clarification":
		var cp ipc.ClarificationPayload
		if err := json.Unmarshal(cmd.Payload, &cp); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": "invalid set_clarification payload"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if cp.BeadID == "" || cp.Anvil == "" {
			msg, _ := json.Marshal(map[string]string{"message": "bead_id and anvil are required"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		reason := strings.TrimSpace(cp.Reason)
		if reason == "" {
			msg, _ := json.Marshal(map[string]string{"message": "reason is required"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if err := d.db.SetClarificationNeeded(cp.BeadID, cp.Anvil, true, reason); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("failed to set clarification: %v", err)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		_ = d.db.LogEvent(state.EventClarificationNeeded, fmt.Sprintf("Bead %s needs clarification: %s", cp.BeadID, reason), cp.BeadID, cp.Anvil)
		d.logger.Info("bead marked as clarification_needed", "bead", cp.BeadID, "anvil", cp.Anvil, "reason", reason)
		data, _ := json.Marshal(map[string]string{"message": "clarification_needed set"})
		return ipc.Response{Type: "ok", Payload: data}

	case "append_notes":
		var np ipc.AppendNotesPayload
		if err := json.Unmarshal(cmd.Payload, &np); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": "invalid append_notes payload"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if np.BeadID == "" || np.Anvil == "" {
			msg, _ := json.Marshal(map[string]string{"message": "bead_id and anvil are required"})
			return ipc.Response{Type: "error", Payload: msg}
		}

		cfgSnapshot := d.cfg.Load()
		anvilCfg, ok := cfgSnapshot.Anvils[np.Anvil]
		if !ok {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("anvil %q not found", np.Anvil)})
			return ipc.Response{Type: "error", Payload: msg}
		}

		notesCtx, notesCancel := context.WithTimeout(d.runCtx, 30*time.Second)
		defer notesCancel()

		// bd update does not support reading notes from a file or stdin, so we must
		// pass them as an argument. Routing through the daemon IPC still ensures
		// the notes are not visible on the long-lived Hearth CLI process command line.
		notesCmd := executil.HideWindow(exec.CommandContext(notesCtx, "bd", "update", np.BeadID, "--append-notes", np.Notes))
		notesCmd.Dir = anvilCfg.Path
		out, err := notesCmd.CombinedOutput()
		if err != nil {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("bd update %s --notes-file: %v: %s", np.BeadID, err, string(out))})
			return ipc.Response{Type: "error", Payload: msg}
		}

		data, _ := json.Marshal(map[string]string{"message": "notes appended"})
		return ipc.Response{Type: "ok", Payload: data}

	case "tag_bead":
		var tp ipc.TagBeadPayload
		if err := json.Unmarshal(cmd.Payload, &tp); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": "invalid tag_bead payload"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if tp.BeadID == "" || tp.Anvil == "" {
			msg, _ := json.Marshal(map[string]string{"message": "bead_id and anvil are required"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		cfgSnapshot := d.cfg.Load()
		anvilCfg, ok := cfgSnapshot.Anvils[tp.Anvil]
		if !ok {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("anvil %q not found", tp.Anvil)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if anvilCfg.Path == "" {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("anvil %q has no path configured", tp.Anvil)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		// Derive the tag from the daemon's authoritative config so the client
		// cannot inject arbitrary labels and hot-reload is respected.
		tag := anvilCfg.AutoDispatchTag
		if tag == "" {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("anvil %q has no auto_dispatch_tag configured", tp.Anvil)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		tagCtx, tagCancel := context.WithTimeout(d.runCtx, 30*time.Second)
		defer tagCancel()
		tagCmd := executil.HideWindow(exec.CommandContext(tagCtx, "bd", "update", tp.BeadID, "--add-label", tag))
		tagCmd.Dir = anvilCfg.Path
		if out, err := tagCmd.CombinedOutput(); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("bd update failed: %v: %s", err, string(out))})
			return ipc.Response{Type: "error", Payload: msg}
		}
		d.logger.Info("label added to bead", "bead", tp.BeadID, "anvil", tp.Anvil, "tag", tag)
		_ = d.db.LogEvent(state.EventBeadTagged, fmt.Sprintf("Label %q added to bead %s", tag, tp.BeadID), tp.BeadID, tp.Anvil)
		// Trigger a refresh so the queue cache updates promptly, but bound the
		// context so the refresh participates in graceful shutdown.
		refreshCtx, refreshCancel := context.WithTimeout(d.runCtx, 30*time.Second)
		go func() {
			defer refreshCancel()
			d.pollAndDispatch(refreshCtx)
		}()
		data, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("label %q added", tag)})
		return ipc.Response{Type: "ok", Payload: data}

	case "close_bead":
		var cp ipc.CloseBeadPayload
		if err := json.Unmarshal(cmd.Payload, &cp); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": "invalid close_bead payload"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if cp.BeadID == "" || cp.Anvil == "" {
			msg, _ := json.Marshal(map[string]string{"message": "bead_id and anvil are required"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		cfgSnapshot := d.cfg.Load()
		anvilCfg, ok := cfgSnapshot.Anvils[cp.Anvil]
		if !ok {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("anvil %q not found", cp.Anvil)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if anvilCfg.Path == "" {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("anvil %q has no path configured", cp.Anvil)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		closeCtx, closeCancel := context.WithTimeout(d.runCtx, 30*time.Second)
		defer closeCancel()
		closeCmd := executil.HideWindow(exec.CommandContext(closeCtx, "bd", "close", cp.BeadID))
		closeCmd.Dir = anvilCfg.Path
		if out, err := closeCmd.CombinedOutput(); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("bd close failed: %v: %s", err, string(out))})
			return ipc.Response{Type: "error", Payload: msg}
		}
		d.logger.Info("bead closed via TUI", "bead", cp.BeadID, "anvil", cp.Anvil)
		_ = d.db.LogEvent(state.EventBeadClosed, fmt.Sprintf("Bead %s closed via TUI", cp.BeadID), cp.BeadID, cp.Anvil)
		refreshCtx, refreshCancel := context.WithTimeout(d.runCtx, 30*time.Second)
		go func() {
			defer refreshCancel()
			d.pollAndDispatch(refreshCtx)
		}()
		data, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("bead %s closed", cp.BeadID)})
		return ipc.Response{Type: "ok", Payload: data}

	case "stop_bead":
		var sp ipc.StopBeadPayload
		if err := json.Unmarshal(cmd.Payload, &sp); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": "invalid stop_bead payload"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if sp.BeadID == "" || sp.Anvil == "" {
			msg, _ := json.Marshal(map[string]string{"message": "bead_id and anvil are required"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		reason := strings.TrimSpace(sp.Reason)
		if reason == "" {
			reason = "manually stopped"
		}
		// Sanitize reason to prevent terminal escape injection in event log.
		reason = strings.Map(func(r rune) rune {
			if r < 32 && r != '\n' {
				return -1
			}
			return r
		}, reason)

		// Validate the anvil exists and has a path, matching tag_bead/close_bead.
		anvilCfg, ok := d.cfg.Load().Anvils[sp.Anvil]
		if !ok {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("anvil %q not found", sp.Anvil)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if anvilCfg.Path == "" {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("anvil %q has no path configured", sp.Anvil)})
			return ipc.Response{Type: "error", Payload: msg}
		}

		// Kill any running worker for this bead.
		if w, err := d.db.ActiveWorkerByBeadAndAnvil(sp.BeadID, sp.Anvil); err == nil && w != nil {
			if w.PID > 0 {
				if proc, err := os.FindProcess(w.PID); err == nil {
					if runtime.GOOS == "windows" {
						_ = proc.Kill()
					} else {
						_ = proc.Signal(syscall.SIGINT)
					}
				}
			}
			_ = d.db.UpdateWorkerStatus(w.ID, state.WorkerFailed)
			d.logger.Info("killed worker for stopped bead", "worker", w.ID, "bead", sp.BeadID)
		}

		// Mark clarification_needed first so the poller will skip this bead
		// before we free the active slot; prevents a run_bead race between
		// the Delete and the DB write.
		if err := d.db.SetClarificationNeeded(sp.BeadID, sp.Anvil, true, reason); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("failed to set clarification: %v", err)})
			return ipc.Response{Type: "error", Payload: msg}
		}

		// Remove from active beads so the slot is freed.
		d.activeBeads.Delete(sp.BeadID)

		// Release bead back to open so it's visible but not dispatched.
		releaseCtx, releaseCancel := context.WithTimeout(d.runCtx, 30*time.Second)
		defer releaseCancel()
		releaseCmd := executil.HideWindow(exec.CommandContext(releaseCtx, "bd", "update", sp.BeadID, "--status=open", "--assignee=", "--json"))
		releaseCmd.Dir = anvilCfg.Path
		if out, err := releaseCmd.CombinedOutput(); err != nil {
			d.logger.Warn("bd update failed when releasing stopped bead", "bead", sp.BeadID, "error", err, "output", strings.TrimSpace(string(out)))
			errMsg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("bead stopped but bd release failed: %v", err)})
			return ipc.Response{Type: "error", Payload: errMsg}
		}

		_ = d.db.LogEvent(state.EventBeadStopped, fmt.Sprintf("Bead %s stopped: %s", sp.BeadID, reason), sp.BeadID, sp.Anvil)
		d.logger.Info("bead stopped", "bead", sp.BeadID, "anvil", sp.Anvil, "reason", reason)
		data, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("bead %s stopped", sp.BeadID)})
		return ipc.Response{Type: "ok", Payload: data}

	case "clear_clarification":
		var cp ipc.ClarificationPayload
		if err := json.Unmarshal(cmd.Payload, &cp); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": "invalid clear_clarification payload"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if cp.BeadID == "" || cp.Anvil == "" {
			msg, _ := json.Marshal(map[string]string{"message": "bead_id and anvil are required"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if err := d.db.SetClarificationNeeded(cp.BeadID, cp.Anvil, false, ""); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("failed to clear clarification: %v", err)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		_ = d.db.LogEvent(state.EventClarificationCleared, fmt.Sprintf("Clarification cleared for bead %s", cp.BeadID), cp.BeadID, cp.Anvil)
		d.logger.Info("clarification_needed cleared", "bead", cp.BeadID, "anvil", cp.Anvil)
		data, _ := json.Marshal(map[string]string{"message": "clarification_needed cleared"})
		return ipc.Response{Type: "ok", Payload: data}

	case "retry_bead":
		var rp ipc.RetryBeadPayload
		if err := json.Unmarshal(cmd.Payload, &rp); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": "invalid retry_bead payload"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if rp.BeadID == "" || rp.Anvil == "" {
			msg, _ := json.Marshal(map[string]string{"message": "bead_id and anvil are required"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		// Exhausted PR retry: reset fix counters and status back to open
		if rp.PRID > 0 {
			pr, err := d.db.GetPRByID(rp.PRID)
			if err != nil || pr == nil {
				msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("PR %d not found", rp.PRID)})
				return ipc.Response{Type: "error", Payload: msg}
			}
			if err := d.db.ResetPRFixCounts(rp.PRID); err != nil {
				msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("failed to reset PR fix counts: %v", err)})
				return ipc.Response{Type: "error", Payload: msg}
			}
			// Reset the lifecycle manager's in-memory state so new Bellows
			// events dispatch fresh fix/rebase workers instead of being
			// silently dropped as "already exhausted".
			if d.lifecycleMgr == nil {
				d.logger.Error("lifecycle manager not ready for retry_bead PR reset", "pr_id", rp.PRID, "bead", pr.BeadID, "anvil", pr.Anvil)
				msg, _ := json.Marshal(map[string]string{"message": "lifecycle manager not ready"})
				return ipc.Response{Type: "error", Payload: msg}
			}
			d.lifecycleMgr.ResetPRState(pr.Anvil, pr.Number)
			if d.bellowsMonitor != nil {
				d.bellowsMonitor.ResetPRState(pr.Anvil, pr.Number)
				d.bellowsMonitor.Refresh()
			}
			// Trigger a poll as well to catch any other ready work or updates.
			go d.pollAndDispatch(d.runCtx)

			_ = d.db.LogEvent(
				state.EventRetryReset,
				fmt.Sprintf("PR fix counts reset for PR %d (manual)", rp.PRID),
				pr.BeadID,
				pr.Anvil,
			)
			d.logger.Info("PR fix counts reset", "pr_id", rp.PRID, "bead", pr.BeadID, "anvil", pr.Anvil)
			data, _ := json.Marshal(map[string]string{"message": "PR fix counts reset, status set to open"})
			return ipc.Response{Type: "ok", Payload: data}
		}
		retry, err := d.db.GetRetry(rp.BeadID, rp.Anvil)
		if err != nil {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("failed to get retry state: %v", err)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if retry != nil && retry.DispatchFailures > 0 {
			if err := d.db.ResetDispatchFailures(rp.BeadID, rp.Anvil); err != nil {
				msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("failed to reset circuit breaker: %v", err)})
				return ipc.Response{Type: "error", Payload: msg}
			}
			_ = d.db.LogEvent(state.EventRetryReset, fmt.Sprintf("Circuit breaker reset for bead %s (manual)", rp.BeadID), rp.BeadID, rp.Anvil)
			d.logger.Info("circuit breaker reset for bead", "bead", rp.BeadID, "anvil", rp.Anvil)

			// Clear the bead assignee so the poller can re-dispatch it.
			// The poller filters out beads with a non-empty assignee, so if the
			// previous pipeline failure left the assignee set the bead would
			// remain permanently invisible after the circuit breaker reset.
			if anvilCfg, ok := d.cfg.Load().Anvils[rp.Anvil]; ok && anvilCfg.Path != "" {
				clearCtx, clearCancel := context.WithTimeout(d.runCtx, 15*time.Second)
				defer clearCancel()
				clearCmd := executil.HideWindow(exec.CommandContext(clearCtx, "bd", "update", rp.BeadID, "--assignee=", "--json"))
				clearCmd.Dir = anvilCfg.Path
				if output, clearErr := clearCmd.CombinedOutput(); clearErr != nil {
					d.logger.Warn("failed to clear bead assignee after circuit breaker reset", "bead", rp.BeadID, "error", clearErr, "output", string(output))
				}
			}

			// Trigger a poll immediately after resetting the circuit breaker.
			go d.pollAndDispatch(d.runCtx)

			data, _ := json.Marshal(map[string]string{"message": "circuit breaker reset"})
			return ipc.Response{Type: "ok", Payload: data}
		}
		// Regular retry: clear needs_human and clarification_needed flags
		if err := d.db.ResetRetry(rp.BeadID, rp.Anvil); err != nil {
			// If no retry record exists, it might be a stalled worker being
			// retried. We don't return an error here so the TUI can still
			// show success and the user can proceed.
			d.logger.Warn("ResetRetry failed (might not have a retry record)", "bead", rp.BeadID, "anvil", rp.Anvil, "error", err)
		}
		_ = d.db.LogEvent(state.EventRetryReset, fmt.Sprintf("Retry reset for bead %s (manual)", rp.BeadID), rp.BeadID, rp.Anvil)
		d.logger.Info("retry reset for bead", "bead", rp.BeadID, "anvil", rp.Anvil)

		// Clear the bead assignee so the poller can re-dispatch it.
		// The poller filters out beads with a non-empty assignee, so if the
		// previous pipeline failure left the assignee set the bead would
		// remain permanently invisible after the retry reset.
		if anvilCfg, ok := d.cfg.Load().Anvils[rp.Anvil]; ok && anvilCfg.Path != "" {
			clearCtx, clearCancel := context.WithTimeout(d.runCtx, 15*time.Second)
			defer clearCancel()
			clearCmd := executil.HideWindow(exec.CommandContext(clearCtx, "bd", "update", rp.BeadID, "--assignee=", "--json"))
			clearCmd.Dir = anvilCfg.Path
			if output, clearErr := clearCmd.CombinedOutput(); clearErr != nil {
				d.logger.Warn("failed to clear bead assignee after retry reset", "bead", rp.BeadID, "error", clearErr, "output", string(output))
			}
		}

		// Trigger a poll immediately after resetting retry state.
		go d.pollAndDispatch(d.runCtx)

		data, _ := json.Marshal(map[string]string{"message": "retry reset"})
		return ipc.Response{Type: "ok", Payload: data}

	case "dismiss_bead":
		var dp ipc.DismissBeadPayload
		if err := json.Unmarshal(cmd.Payload, &dp); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": "invalid dismiss_bead payload"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if dp.BeadID == "" || dp.Anvil == "" {
			msg, _ := json.Marshal(map[string]string{"message": "bead_id and anvil are required"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		// Exhausted PR dismiss: set status to closed
		if dp.PRID > 0 {
			pr, err := d.db.GetPRByID(dp.PRID)
			if err != nil || pr == nil {
				msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("PR %d not found", dp.PRID)})
				return ipc.Response{Type: "error", Payload: msg}
			}
			if err := d.db.DismissExhaustedPR(dp.PRID); err != nil {
				msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("failed to dismiss exhausted PR: %v", err)})
				return ipc.Response{Type: "error", Payload: msg}
			}
			_ = d.db.LogEvent(
				state.EventBeadDismissed,
				fmt.Sprintf("Exhausted PR %d dismissed (manual)", dp.PRID),
				pr.BeadID,
				pr.Anvil,
			)
			d.logger.Info("exhausted PR dismissed", "pr_id", dp.PRID, "bead", pr.BeadID, "anvil", pr.Anvil)
			data, _ := json.Marshal(map[string]string{"message": "exhausted PR dismissed"})
			return ipc.Response{Type: "ok", Payload: data}
		}
		if err := d.db.DismissRetry(dp.BeadID, dp.Anvil); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("failed to dismiss: %v", err)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		logMessage := fmt.Sprintf("Bead %s dismissed from needs attention", dp.BeadID)
		_ = d.db.LogEvent(state.EventBeadDismissed, logMessage, dp.BeadID, dp.Anvil)
		d.logger.Info("bead dismissed from needs attention", "bead", dp.BeadID, "anvil", dp.Anvil)
		data, _ := json.Marshal(map[string]string{"message": "dismissed"})
		return ipc.Response{Type: "ok", Payload: data}

	case "view_logs":
		var vp ipc.ViewLogsPayload
		if err := json.Unmarshal(cmd.Payload, &vp); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": "invalid view_logs payload"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if vp.BeadID == "" {
			msg, _ := json.Marshal(map[string]string{"message": "bead_id is required"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		logPath, err := d.db.LastWorkerLogPath(vp.BeadID)
		if err != nil {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("failed to find log: %v", err)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if logPath == "" {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("no worker logs found for bead %q", vp.BeadID)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		// Read last 50 lines of the log without loading the entire file into memory.
		const maxLines = 50
		lastLines, err := func(path string, n int) ([]string, error) {
			if n <= 0 {
				return nil, nil
			}
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			defer f.Close()

			info, err := f.Stat()
			if err != nil {
				return nil, err
			}
			size := info.Size()
			if size == 0 {
				return nil, nil
			}

			const readBlockSize = 8192
			var (
				buf          []byte
				remaining    = size
				newlineCount int
			)

			for remaining > 0 && newlineCount <= n {
				toRead := int64(readBlockSize)
				if remaining < toRead {
					toRead = remaining
				}
				remaining -= toRead

				chunk := make([]byte, toRead)
				if _, err := f.ReadAt(chunk, remaining); err != nil && err != io.EOF {
					return nil, err
				}

				for _, b := range chunk {
					if b == '\n' {
						newlineCount++
					}
				}

				buf = append(chunk, buf...)
			}

			if len(buf) == 0 {
				return nil, nil
			}

			lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
			if len(lines) <= n {
				return lines, nil
			}
			return lines[len(lines)-n:], nil
		}(logPath, maxLines)
		if err != nil {
			lastLines = nil
		}
		resp := ipc.ViewLogsResponse{LogPath: logPath, LastLines: lastLines}
		data, _ := json.Marshal(resp)
		return ipc.Response{Type: "ok", Payload: data}

	case "merge_pr":
		var mp ipc.MergePRPayload
		if err := json.Unmarshal(cmd.Payload, &mp); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": "invalid merge_pr payload"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		// Comment 4: allow pr_id-only requests; pr_number can be derived from DB.
		if (mp.PRID <= 0 && mp.PRNumber <= 0) || mp.Anvil == "" {
			msg, _ := json.Marshal(map[string]string{"message": "anvil and either pr_id or pr_number are required"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		// Load the PR record first so we can derive authoritative anvil and number.
		var pr *state.PR
		var prErr error
		if mp.PRID > 0 {
			pr, prErr = d.db.GetPRByID(mp.PRID)
		} else {
			pr, prErr = d.db.GetPRByNumber(mp.Anvil, mp.PRNumber)
		}
		if prErr != nil {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("failed to load PR from state db: %v", prErr)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if pr == nil {
			msg, _ := json.Marshal(map[string]string{"message": "PR not found in state db; cannot validate merge readiness"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		// Comment 3 & 5: derive authoritative anvil and PR number from the DB record.
		// Validate that the payload's anvil matches what we loaded, to catch stale/buggy clients.
		if mp.PRID > 0 && mp.Anvil != "" && mp.Anvil != pr.Anvil {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("anvil mismatch: payload has %q but PR %d belongs to %q", mp.Anvil, mp.PRID, pr.Anvil)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		mergeAnvil := pr.Anvil
		mergeNumber := pr.Number
		cfgSnapshot := d.cfg.Load()
		anvilCfg, ok := cfgSnapshot.Anvils[mergeAnvil]
		if !ok {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("anvil %q not found", mergeAnvil)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		// Validate cached readiness from state.db.
		ready, readyErr := d.db.IsPRReadyToMerge(pr.ID)
		if readyErr != nil {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("failed to check merge readiness: %v", readyErr)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if !ready {
			msg, _ := json.Marshal(map[string]string{"message": "PR is not ready to merge (not approved, CI failing, conflicting, or has unresolved threads)"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		// Comment 6: re-check live GitHub status immediately before merging to avoid
		// acting on stale cached state from between Bellows polls.
		liveCtx, liveCancel := context.WithTimeout(d.runCtx, 30*time.Second)
		liveStatus, liveErr := ghpr.CheckStatus(liveCtx, anvilCfg.Path, mergeNumber)
		liveCancel()
		if liveErr != nil {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("could not verify live PR status: %v", liveErr)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if !liveStatus.CIsPassing() || liveStatus.Mergeable == "CONFLICTING" || liveStatus.UnresolvedThreads > 0 || liveStatus.HasPendingReviewRequests() {
			msg, _ := json.Marshal(map[string]string{"message": "PR failed live readiness check (CI failing, conflicts, unresolved threads, or pending reviews)"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		beadID := pr.BeadID
		strategy := cfgSnapshot.Settings.MergeStrategy
		if strategy == "" {
			strategy = "squash"
		}
		// Comment 7: log the merge request before attempting, so the event is always recorded.
		_ = d.db.LogEvent(state.EventPRMergeRequested,
			fmt.Sprintf("PR #%d merge requested (strategy: %s)", mergeNumber, strategy),
			beadID, mergeAnvil)
		d.logger.Info("PR merge requested", "pr_number", mergeNumber, "anvil", mergeAnvil, "strategy", strategy)
		mergeCtx, mergeCancel := context.WithTimeout(d.runCtx, 60*time.Second)
		defer mergeCancel()
		if err := ghpr.Merge(mergeCtx, anvilCfg.Path, mergeNumber, strategy); err != nil {
			_ = d.db.LogEvent(state.EventPRMergeFailed,
				fmt.Sprintf("PR #%d merge failed: %v", mergeNumber, err),
				beadID, mergeAnvil)
			d.logger.Error("PR merge failed", "pr_number", mergeNumber, "anvil", mergeAnvil, "error", err)
			// Sanitize error message for IPC: use only the first line to avoid multi-line/huge payloads.
			errSummary := strings.SplitN(err.Error(), "\n", 2)[0]
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("merge failed: %s", errSummary)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		_ = d.db.LogEvent(state.EventPRMerged,
			fmt.Sprintf("PR #%d merged successfully (strategy: %s)", mergeNumber, strategy),
			beadID, mergeAnvil)
		d.logger.Info("PR merged successfully", "pr_number", mergeNumber, "anvil", mergeAnvil, "strategy", strategy)
		// Immediately update PR status so it disappears from the Ready to Merge panel
		// without waiting for the next bellows poll cycle (up to 2 minutes).
		_ = d.db.UpdatePRStatus(pr.ID, state.PRMerged)
		_ = d.db.CompleteWorkersByBead(beadID)
		// Trigger an immediate bellows poll so downstream lifecycle effects
		// (bead close, worktree cleanup) happen promptly.
		if d.bellowsMonitor != nil {
			d.bellowsMonitor.Refresh()
		}
		data, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("PR #%d merged", mergeNumber)})
		return ipc.Response{Type: "ok", Payload: data}

	case "resolve_orphan":
		var rp ipc.ResolveOrphanPayload
		if err := json.Unmarshal(cmd.Payload, &rp); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": "invalid resolve_orphan payload"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if rp.BeadID == "" || rp.Anvil == "" || rp.Action == "" {
			msg, _ := json.Marshal(map[string]string{"message": "bead_id, anvil, and action are required"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		cfgSnapshot := d.cfg.Load()
		anvilCfg, ok := cfgSnapshot.Anvils[rp.Anvil]
		if !ok || anvilCfg.Path == "" {
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("anvil %q not found or has no path", rp.Anvil)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		switch rp.Action {
		case "recover":
			if err := d.shutdownMgr.ResetBead(rp.BeadID, anvilCfg.Path); err != nil {
				msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("failed to recover bead: %v", err)})
				return ipc.Response{Type: "error", Payload: msg}
			}
			_ = d.db.RemovePendingOrphan(rp.BeadID, rp.Anvil)
			_ = d.db.LogEvent(state.EventBeadRecovered, fmt.Sprintf("Orphan %s recovered by user via Hearth", rp.BeadID), rp.BeadID, rp.Anvil)
			d.logger.Info("orphan recovered by user", "bead", rp.BeadID, "anvil", rp.Anvil)
			go d.pollAndDispatch(d.runCtx)
		case "close":
			// Use context.Background() so this bd close call is not interrupted
			// if the daemon is concurrently shutting down. The user explicitly
			// chose to close this orphan, and the operation must complete.
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer closeCancel()
			closeCmd := executil.HideWindow(exec.CommandContext(closeCtx, "bd", "close", rp.BeadID))
			closeCmd.Dir = anvilCfg.Path
			if out, err := closeCmd.CombinedOutput(); err != nil {
				msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("bd close failed: %v: %s", err, string(out))})
				return ipc.Response{Type: "error", Payload: msg}
			}
			_ = d.db.RemovePendingOrphan(rp.BeadID, rp.Anvil)
			_ = d.db.LogEvent(state.EventBeadClosed, fmt.Sprintf("Orphan %s closed by user (work completed)", rp.BeadID), rp.BeadID, rp.Anvil)
			d.logger.Info("orphan closed by user (completed)", "bead", rp.BeadID, "anvil", rp.Anvil)
			// Refresh queue state so Hearth reflects the closed orphan immediately.
			go d.pollAndDispatch(d.runCtx)
		case "discard":
			// Use context.Background() so this bd close call is not interrupted
			// if the daemon is concurrently shutting down. The user explicitly
			// chose to discard this orphan, and the operation must complete.
			discardCtx, discardCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer discardCancel()
			discardCmd := executil.HideWindow(exec.CommandContext(discardCtx, "bd", "close", rp.BeadID, `--reason=Discarded by user during orphan recovery`))
			discardCmd.Dir = anvilCfg.Path
			if out, err := discardCmd.CombinedOutput(); err != nil {
				msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("bd close failed: %v: %s", err, string(out))})
				return ipc.Response{Type: "error", Payload: msg}
			}
			_ = d.db.RemovePendingOrphan(rp.BeadID, rp.Anvil)
			_ = d.db.LogEvent(state.EventBeadClosed, fmt.Sprintf("Orphan %s discarded by user", rp.BeadID), rp.BeadID, rp.Anvil)
			d.logger.Info("orphan discarded by user", "bead", rp.BeadID, "anvil", rp.Anvil)
			// Refresh queue state so Hearth reflects the discarded orphan immediately.
			go d.pollAndDispatch(d.runCtx)
		default:
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("unknown orphan action: %q", rp.Action)})
			return ipc.Response{Type: "error", Payload: msg}
		}
		data, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("orphan %s handled: %s", rp.BeadID, rp.Action)})
		return ipc.Response{Type: "ok", Payload: data}

	case "pr_action":
		var pa ipc.PRActionPayload
		if err := json.Unmarshal(cmd.Payload, &pa); err != nil {
			msg, _ := json.Marshal(map[string]string{"message": "invalid pr_action payload: " + err.Error()})
			return ipc.Response{Type: "error", Payload: msg}
		}
		if pa.PRNumber == 0 || pa.Anvil == "" {
			msg, _ := json.Marshal(map[string]string{"message": "pr_action requires pr_number and anvil"})
			return ipc.Response{Type: "error", Payload: msg}
		}
		anvilCfg, ok := d.cfg.Load().Anvils[pa.Anvil]
		if !ok {
			msg, _ := json.Marshal(map[string]string{"message": "unknown anvil: " + pa.Anvil})
			return ipc.Response{Type: "error", Payload: msg}
		}

		switch pa.Action {
		case "close":
			closeCmd := exec.CommandContext(d.runCtx, "gh", "pr", "close", strconv.Itoa(pa.PRNumber))
			closeCmd.Dir = anvilCfg.Path
			if out, err := closeCmd.CombinedOutput(); err != nil {
				msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("gh pr close failed: %v: %s", err, string(out))})
				return ipc.Response{Type: "error", Payload: msg}
			}
			if pa.PRID > 0 {
				_ = d.db.UpdatePRStatus(pa.PRID, state.PRClosed)
			}
			_ = d.db.LogEvent(state.EventPRClosed, fmt.Sprintf("PR #%d closed by user", pa.PRNumber), pa.BeadID, pa.Anvil)
			d.logger.Info("PR closed by user via pr_action", "pr", pa.PRNumber, "anvil", pa.Anvil)

		case "open_browser":
			openCmd := exec.CommandContext(d.runCtx, "gh", "pr", "view", strconv.Itoa(pa.PRNumber), "--web")
			openCmd.Dir = anvilCfg.Path
			if out, err := openCmd.CombinedOutput(); err != nil {
				msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("gh pr view --web failed: %v: %s", err, string(out))})
				return ipc.Response{Type: "error", Payload: msg}
			}

		case "reviewfix":
			if pa.BeadID == "" || pa.Branch == "" {
				msg, _ := json.Marshal(map[string]string{"message": "reviewfix action requires bead_id and branch"})
				return ipc.Response{Type: "error", Payload: msg}
			}
			req := lifecycle.ActionRequest{
				Action:   lifecycle.ActionFixReview,
				PRNumber: pa.PRNumber,
				BeadID:   pa.BeadID,
				Anvil:    pa.Anvil,
				Branch:   pa.Branch,
			}
			go d.handleLifecycleAction(d.runCtx, req)
			_ = d.db.LogEvent(state.EventReviewFixStarted, fmt.Sprintf("PR #%d review fix triggered by user", pa.PRNumber), pa.BeadID, pa.Anvil)
			d.logger.Info("review fix triggered by user via pr_action", "pr", pa.PRNumber, "anvil", pa.Anvil)

		case "rebase":
			if pa.BeadID == "" || pa.Branch == "" {
				msg, _ := json.Marshal(map[string]string{"message": "rebase action requires bead_id and branch"})
				return ipc.Response{Type: "error", Payload: msg}
			}
			pr, _ := d.db.GetPRByID(pa.PRID)
			baseBranch := ""
			if pr != nil {
				baseBranch = pr.BaseBranch
			}
			req := lifecycle.ActionRequest{
				Action:     lifecycle.ActionRebase,
				PRNumber:   pa.PRNumber,
				BeadID:     pa.BeadID,
				Anvil:      pa.Anvil,
				Branch:     pa.Branch,
				BaseBranch: baseBranch,
			}
			go d.handleLifecycleAction(d.runCtx, req)
			_ = d.db.LogEvent(state.EventRebaseStarted, fmt.Sprintf("PR #%d rebase triggered by user", pa.PRNumber), pa.BeadID, pa.Anvil)
			d.logger.Info("rebase triggered by user via pr_action", "pr", pa.PRNumber, "anvil", pa.Anvil)

		default:
			msg, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("unknown pr_action: %q", pa.Action)})
			return ipc.Response{Type: "error", Payload: msg}
		}

		respData, _ := json.Marshal(map[string]string{"message": fmt.Sprintf("PR #%d: %s", pa.PRNumber, pa.Action)})
		return ipc.Response{Type: "ok", Payload: respData}

	default:
		msg, _ := json.Marshal(map[string]string{"message": "unknown command: " + cmd.Type})
		return ipc.Response{Type: "error", Payload: msg}
	}
}

// BroadcastEvent sends an event to all connected IPC clients.
func (d *Daemon) BroadcastEvent(eventType string, data any) {
	if d.ipc == nil {
		return
	}
	raw, _ := json.Marshal(data)
	d.ipc.Broadcast(ipc.Event{
		Type:      eventType,
		Timestamp: time.Now(),
		Data:      raw,
	})
}

// IsRunning checks whether a daemon process is already running by reading
// the PID file and checking if the process is alive.
func IsRunning() (int, bool) {
	pidPath, err := pidFilePath()
	if err != nil {
		return 0, false
	}

	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, false
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}

	// Check if process exists
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}

	// On Windows, Signal(0) is not supported. Use the named pipe as a
	// liveness proxy — the OS destroys it automatically when the process exits.
	if runtime.GOOS == "windows" {
		alive := ipc.SocketExists()
		if !alive {
			_ = proc.Release()
		}
		return pid, alive
	}

	// On Unix, FindProcess always succeeds; Signal(0) checks liveness.
	err = proc.Signal(syscall.Signal(0))
	if err != nil {
		return 0, false
	}

	return pid, true
}

// Stop sends a graceful shutdown signal to the running daemon.
// On Windows, syscall.SIGINT is not supported, so we use an IPC "shutdown"
// command instead. On Unix we send SIGINT directly.
func Stop() error {
	_, running := IsRunning()
	if !running {
		return fmt.Errorf("no daemon running")
	}

	if runtime.GOOS == "windows" {
		client, err := ipc.NewClient()
		if err != nil {
			return fmt.Errorf("connecting to daemon: %w", err)
		}
		defer client.Close()
		resp, err := client.Send(ipc.Command{Type: "shutdown"})
		if err != nil {
			return fmt.Errorf("sending shutdown command: %w", err)
		}
		if resp.Type == "error" {
			return fmt.Errorf("daemon rejected shutdown: %s", resp.Payload)
		}
		return nil
	}

	// Unix: find the process and send SIGINT for graceful shutdown.
	pid, _ := IsRunning()
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding process %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGINT); err != nil {
		return fmt.Errorf("sending shutdown signal to PID %d: %w", pid, err)
	}
	return nil
}

// writePID writes the current process PID to the PID file.
func (d *Daemon) writePID() error {
	pid := os.Getpid()
	return os.WriteFile(d.pidFile, []byte(strconv.Itoa(pid)), 0o644)
}

// removePID removes the PID file.
func (d *Daemon) removePID() {
	os.Remove(d.pidFile)
}

// cleanup closes resources.
func (d *Daemon) cleanup() {
	if d.configWatcher != nil {
		d.configWatcher.Stop()
	}
	if d.ipc != nil {
		d.ipc.Close()
	}
	if d.db != nil {
		d.db.Close()
	}
	if d.logFile != nil {
		d.logFile.Close()
	}
}

// pidFilePath returns the path to the PID file.
func pidFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".forge", PIDFileName), nil
}

// isBeadClarificationNeeded checks the state DB for a clarification_needed flag on a bead.
// Returns (needed, error) so callers can distinguish "clarification needed" from "DB error".
func (d *Daemon) isBeadClarificationNeeded(beadID, anvil string) (bool, error) {
	r, err := d.db.GetRetry(beadID, anvil)
	if err != nil {
		return false, fmt.Errorf("checking clarification status for %s: %w", beadID, err)
	}
	if r == nil {
		return false, nil
	}
	return r.ClarificationNeeded, nil
}

// recordDispatchFailure increments the dispatch failure counter for a bead and
// logs a circuit-break event if the threshold is reached. When the circuit
// breaker trips, a bead_failed notification is sent to configured webhooks.
func (d *Daemon) recordDispatchFailure(beadID, anvil, reason string) {
	count, broken, err := d.db.IncrementDispatchFailures(beadID, anvil, MaxDispatchFailures, reason)
	if err != nil {
		d.logger.Error("failed to record dispatch failure", "bead", beadID, "error", err)
		return
	}
	if broken {
		msg := fmt.Sprintf("Bead %s circuit-broken after %d consecutive dispatch failures: %s", beadID, count, reason)
		d.logger.Warn(msg, "bead", beadID, "anvil", anvil)
		_ = d.db.LogEvent(state.EventDispatchCircuitBreak, msg, beadID, anvil)

		// Fire bead-failed notifications asynchronously.
		disp := d.dispatcher.Load()
		go func(beadID, anvil, reason string, count int) {
			notifCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if n := d.notifier.Load(); n != nil {
				n.BeadFailed(notifCtx, anvil, beadID, count, reason)
			}
			if disp != nil {
				failMsg := fmt.Sprintf("Bead failed after %d dispatch attempts: %s", count, reason)
				disp.Dispatch(notifCtx, notify.EventBeadFailed, beadID, anvil, failMsg)
			}
		}(beadID, anvil, reason, count)
	}
}

// applyDecomposedOutcome updates the retry record after a Schematic decompose
// result. When real sub-beads were created it clears any prior dispatch
// failures and propagates the parent's auto_dispatch tag (if any) to each
// child so they are picked up by the poller immediately; when none were
// produced it records a failure so the bead surfaces in Needs Attention and
// can reach the circuit breaker.
func (d *Daemon) applyDecomposedOutcome(bead poller.Bead, anvilCfg config.AnvilConfig, sr *schematic.Result) {
	beadID := bead.ID
	anvil := bead.Anvil
	childCount := 0
	if sr != nil {
		childCount = len(sr.SubBeads)
	}
	if childCount > 0 {
		d.logger.Info("bead decomposed into sub-beads", "bead", beadID, "count", childCount)
		// Decomposition is intentional, not a failure — clear any prior dispatch failures.
		_ = d.db.ClearRetry(beadID, anvil)

		// When the anvil uses tagged auto_dispatch and the parent has the
		// dispatch tag, copy that tag to each child so they are eligible for
		// immediate dispatch by the poller.
		if anvilCfg.AutoDispatch == "tagged" && anvilCfg.AutoDispatchTag != "" {
			if d.labelAdder == nil {
				d.logger.Warn("labelAdder is nil; skipping auto_dispatch tag propagation to child beads",
					"parent", beadID, "tag", anvilCfg.AutoDispatchTag)
			} else {
				parentHasTag := false
				for _, lbl := range bead.Labels {
					if strings.EqualFold(lbl, anvilCfg.AutoDispatchTag) {
						parentHasTag = true
						break
					}
				}
				if parentHasTag {
					for _, sub := range sr.SubBeads {
						if err := d.labelAdder(anvilCfg.Path, sub.ID, anvilCfg.AutoDispatchTag); err != nil {
							d.logger.Warn("failed to copy auto_dispatch tag to child bead",
								"parent", beadID, "child", sub.ID, "tag", anvilCfg.AutoDispatchTag, "error", err)
							reason := fmt.Sprintf("failed to propagate auto_dispatch tag %q to child bead %s: %v",
								anvilCfg.AutoDispatchTag, sub.ID, err)
							d.recordDispatchFailure(beadID, anvil, reason)
						} else {
							d.logger.Info("copied auto_dispatch tag to child bead",
								"parent", beadID, "child", sub.ID, "tag", anvilCfg.AutoDispatchTag)
							_ = d.db.LogEvent(state.EventBeadTagged,
								fmt.Sprintf("Label %q propagated to child bead %s from decomposed parent %s", anvilCfg.AutoDispatchTag, sub.ID, beadID),
								sub.ID, anvil)
						}
					}
				}
			}
		}
		return
	}
	// Decomposition produced no children — preserve the retry record so the bead
	// surfaces in Needs Attention rather than silently disappearing.
	reason := "decomposition produced no child beads"
	if sr != nil && sr.Reason != "" {
		reason = reason + ": " + sr.Reason
	}
	d.logger.Warn("bead decomposition produced no children; recording as dispatch failure", "bead", beadID, "reason", reason)
	d.recordDispatchFailure(beadID, anvil, reason)
}

// classifyBeadSection determines which queue section a bead belongs to based
// on the anvil's auto_dispatch configuration.
func (d *Daemon) classifyBeadSection(bead poller.Bead) state.QueueSection {
	if bead.Status == "in_progress" {
		return state.QueueSectionInProgress
	}
	cfgSnapshot := d.cfg.Load()
	anvilCfg, ok := cfgSnapshot.Anvils[bead.Anvil]
	if !ok {
		return state.QueueSectionReady
	}
	// Only split ready vs unlabeled when auto_dispatch mode is "tagged"
	if anvilCfg.AutoDispatch == "tagged" && anvilCfg.AutoDispatchTag != "" {
		for _, t := range bead.Labels {
			if strings.EqualFold(t, anvilCfg.AutoDispatchTag) {
				return state.QueueSectionReady
			}
		}
		return state.QueueSectionUnlabeled
	}
	return state.QueueSectionReady
}

// loadAnvilTemperCached returns the parsed .forge/temper.yaml for the given anvil path,
// using a per-path cache keyed on the file's modification time to avoid repeated I/O
// on every dispatch. Errors are logged once per unique error message rather than on
// every call; non-ENOENT stat errors are cached as sentinels so log spam is suppressed
// even when the file is unreadable (e.g. permission denied).
func (d *Daemon) loadAnvilTemperCached(anvilPath string) *temper.TemperYAML {
	yamlPath := filepath.Join(anvilPath, ".forge", "temper.yaml")
	info, statErr := os.Stat(yamlPath)

	if statErr != nil {
		if !os.IsNotExist(statErr) {
			// Cache the error as a sentinel so we only log when it changes.
			errMsg := statErr.Error()
			if entry, ok := d.temperCache.Load(anvilPath); !ok || entry.(*temperCacheEntry).statErr != errMsg {
				d.logger.Warn("temper: cannot stat per-anvil config", "path", yamlPath, "error", statErr)
				d.temperCache.Store(anvilPath, &temperCacheEntry{statErr: errMsg})
			}
		} else {
			d.temperCache.Delete(anvilPath)
		}
		return nil
	}

	mtime := info.ModTime()
	if entry, ok := d.temperCache.Load(anvilPath); ok {
		cached := entry.(*temperCacheEntry)
		if cached.statErr == "" && cached.mtime.Equal(mtime) {
			return cached.cfg
		}
	}

	cfg, err := temper.LoadAnvilConfig(anvilPath)
	if err != nil {
		d.logger.Warn("temper: failed to load per-anvil config", "path", yamlPath, "error", err)
		// Cache the failed load at this mtime so we don't spam logs on every dispatch.
		d.temperCache.Store(anvilPath, &temperCacheEntry{cfg: nil, mtime: mtime})
		return nil
	}

	d.temperCache.Store(anvilPath, &temperCacheEntry{cfg: cfg, mtime: mtime})
	return cfg
}

// resolveGoRaceDetection resolves the effective Go race detection setting.
// Priority: per-anvil .forge/temper.yaml > per-anvil forge.yaml config > global setting.
// The .forge/temper.yaml is cached by mtime to avoid repeated filesystem I/O.
func (d *Daemon) resolveGoRaceDetection(anvilCfg config.AnvilConfig) bool {
	goRace := d.cfg.Load().Settings.GoRaceDetection
	if anvilCfg.GoRaceDetection != nil {
		goRace = *anvilCfg.GoRaceDetection
	}
	if anvilTemper := d.loadAnvilTemperCached(anvilCfg.Path); anvilTemper != nil && anvilTemper.GoRaceDetection != nil {
		goRace = *anvilTemper.GoRaceDetection
	}
	return goRace
}

// filterCopilotIfLimited removes copilot providers from the list when the
// daily copilot premium request limit has been reached. If the limit is 0
// (unlimited) or not yet reached, the list is returned unchanged.
func (d *Daemon) filterCopilotIfLimited(providers []provider.Provider) []provider.Provider {
	limit := d.cfg.Load().Settings.CopilotDailyRequestLimit
	if limit <= 0 {
		return providers
	}
	used, err := d.db.GetTodayCopilotRequests()
	if err != nil {
		d.logger.Error("checking copilot premium requests", "error", err)
		return providers
	}
	if used < float64(limit) {
		return providers
	}
	// Filter out copilot providers.
	filtered := make([]provider.Provider, 0, len(providers))
	for _, pv := range providers {
		if pv.Kind != provider.Copilot {
			filtered = append(filtered, pv)
		}
	}
	if len(filtered) < len(providers) {
		d.logger.Info("copilot daily request limit reached, skipping copilot provider",
			"used", fmt.Sprintf("%.1f", used), "limit", limit)
	}
	return filtered
}

// shouldDispatch determines if a bead should be automatically dispatched based on anvil configuration.
func shouldDispatch(bead poller.Bead, anvilCfg config.AnvilConfig) bool {
	switch anvilCfg.AutoDispatch {
	case "off":
		return false
	case "tagged":
		if anvilCfg.AutoDispatchTag == "" {
			return false
		}
		for _, t := range bead.Labels {
			if strings.EqualFold(t, anvilCfg.AutoDispatchTag) {
				return true
			}
		}
		return false
	case "priority":
		return bead.Priority <= anvilCfg.AutoDispatchMinPriority
	case "all", "":
		return true
	default:
		// Unknown mode — fail safe rather than dispatch everything.
		// Validate() prevents this in practice but guard against runtime surprises.
		slog.Warn("unknown auto_dispatch mode; disabling auto-dispatch for safety", "mode", anvilCfg.AutoDispatch)
		return false
	}
}

// updateAnvilPaths is called from the hot-reload callback when the set of
// configured anvils changes. It pushes updated path maps into bellows and
// depcheck so they pick up additions, removals, and path changes without a
// daemon restart.
func (d *Daemon) updateAnvilPaths(old, new *config.Config) {
	// Quick check: did anvils actually change?
	changed := len(old.Anvils) != len(new.Anvils)
	if !changed {
		for name, newAnvil := range new.Anvils {
			oldAnvil, ok := old.Anvils[name]
			if !ok || oldAnvil.Path != newAnvil.Path {
				changed = true
				break
			}
			// Also detect depcheck_enabled toggle
			oldDE := oldAnvil.DepcheckEnabled
			newDE := newAnvil.DepcheckEnabled
			if (oldDE == nil) != (newDE == nil) || (oldDE != nil && newDE != nil && *oldDE != *newDE) {
				changed = true
				break
			}
		}
	}
	if !changed {
		return
	}

	// Build new anvil path map
	paths := make(map[string]string, len(new.Anvils))
	for name, a := range new.Anvils {
		if a.Path != "" {
			paths[name] = a.Path
		}
	}

	// Update bellows monitor
	if d.bellowsMonitor != nil {
		d.bellowsMonitor.UpdateAnvilPaths(paths)
		d.logger.Info("updated bellows anvil paths", "count", len(paths))
	}

	// Update depcheck scanner (filter by depcheck_enabled)
	if d.depcheckScanner != nil {
		depcheckPaths := filterDepcheckAnvils(paths, new.Anvils)
		d.depcheckScanner.UpdateAnvilPaths(depcheckPaths)
		d.logger.Info("updated depcheck anvil paths", "count", len(depcheckPaths))
	}
}

// buildDispatcher constructs a new *notify.WebhookDispatcher from the given
// config. Returns nil when notifications are disabled or no webhook targets are
// configured. Safe to call from the hot-reload goroutine; the result is stored
// via d.dispatcher.Store() which is race-free.
func (d *Daemon) buildDispatcher(cfg *config.Config) *notify.WebhookDispatcher {
	if !cfg.Notifications.Enabled {
		return nil
	}
	var webhookTargets []notify.WebhookTarget
	for _, w := range cfg.Notifications.Webhooks {
		trimmedURL := strings.TrimSpace(w.URL)
		if trimmedURL == "" {
			continue
		}
		var trimmedEvents []string
		for _, ev := range w.Events {
			tEv := strings.TrimSpace(ev)
			if tEv != "" {
				trimmedEvents = append(trimmedEvents, tEv)
			}
		}
		webhookTargets = append(webhookTargets, notify.WebhookTarget{
			Name:   w.Name,
			URL:    trimmedURL,
			Events: trimmedEvents,
		})
	}
	return notify.NewWebhookDispatcher(webhookTargets, d.logger)
}

// buildNotifier constructs a new *notify.Notifier from the given config.
// On URL validation failure it falls back to the raw (unformatted) URL rather
// than returning nil, so a config typo during hot-reload cannot accidentally
// disable notifications that were previously working.
func (d *Daemon) buildNotifier(cfg *config.Config) *notify.Notifier {
	n, err := newNotifierFromConfig(cfg, d.logger)
	if err != nil {
		// URL validation failed; build with the raw URL to keep the notifier
		// non-nil and avoid silently disabling an otherwise valid notification
		// setup due to a transient config typo.
		d.logger.Error("invalid Teams webhook URL in reloaded config; using raw URL", "error", err)
		n = notify.NewNotifier(notify.Config{
			WebhookURL: strings.TrimSpace(cfg.Notifications.ResolvedTeamsURL()),
			Enabled:    cfg.Notifications.Enabled,
			Events:     trimStrings(cfg.Notifications.ResolvedTeamsEvents()),
		}, d.logger)
	}
	if n != nil {
		d.logger.Info("notifications config reloaded", "enabled", cfg.Notifications.Enabled)
	} else {
		d.logger.Info("notifications disabled by reloaded config")
	}
	return n
}

// newNotifierFromConfig constructs a *notify.Notifier from cfg, validating and
// normalising the Teams webhook URL. It is the shared implementation used by
// both the startup path (New) and the hot-reload path (buildNotifier) so that
// the two cannot drift in behaviour or logging over time.
func newNotifierFromConfig(cfg *config.Config, logger *slog.Logger) (*notify.Notifier, error) {
	webhookURL := cfg.Notifications.ResolvedTeamsURL()
	trimmedURL := strings.TrimSpace(webhookURL)
	if cfg.Notifications.Enabled && trimmedURL != "" {
		formatted, err := notify.FormatWebhookURL(trimmedURL)
		if err != nil {
			return nil, err
		}
		webhookURL = formatted
	} else if !cfg.Notifications.Enabled && trimmedURL != "" {
		logger.Warn("Teams webhook URL is set but notifications are disabled; skipping URL validation")
	}
	n := notify.NewNotifier(notify.Config{
		WebhookURL: webhookURL,
		Enabled:    cfg.Notifications.Enabled,
		Events:     trimStrings(cfg.Notifications.ResolvedTeamsEvents()),
	}, logger)
	return n, nil
}

func trimStrings(ss []string) []string {
	var res []string
	for _, s := range ss {
		t := strings.TrimSpace(s)
		if t != "" {
			res = append(res, t)
		}
	}
	return res
}

// filterDepcheckAnvils returns the subset of anvils that should be scanned by
// depcheck. Anvils with DepcheckEnabled explicitly set to false are excluded.
func filterDepcheckAnvils(anvils map[string]string, anvilCfgs map[string]config.AnvilConfig) map[string]string {
	result := make(map[string]string, len(anvils))
	for name, path := range anvils {
		if ac, ok := anvilCfgs[name]; ok && ac.DepcheckEnabled != nil && !*ac.DepcheckEnabled {
			continue
		}
		result[name] = path
	}
	return result
}

// verifyAnvilOnMain checks that the anvil root directory is checked out to
// main or master. If the repo is on a different branch (e.g. because a
// smith subprocess ran git checkout in the parent directory), it logs a
// warning and attempts to recover by checking out main/master.
// Returns an error only if recovery is attempted and fails. If the current
// branch cannot be determined, the function is a no-op (non-fatal).
func verifyAnvilOnMain(ctx context.Context, logger *slog.Logger, anvilPath string) error {
	if strings.TrimSpace(anvilPath) == "" {
		logger.Warn("verifyAnvilOnMain: empty anvil path; skipping branch verification")
		return nil
	}

	recovered, originalBranch, err := worktree.VerifyAndRecoverMain(ctx, anvilPath)
	if err != nil {
		if originalBranch == "" {
			// Cannot determine current branch — non-fatal, just warn.
			logger.Warn("verifyAnvilOnMain: could not determine current branch",
				"anvil", anvilPath, "error", err)
			return nil
		}
		return fmt.Errorf("anvil %q is checked out to %q instead of main/master and checkout recovery failed: %w",
			anvilPath, originalBranch, err)
	}

	if recovered {
		logger.Warn("anvil repo was not on main/master — performed recovery checkout",
			"anvil", anvilPath, "original_branch", originalBranch)
	}

	return nil
}
