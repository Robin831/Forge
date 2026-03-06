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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/cifix"
	"github.com/Robin831/Forge/internal/config"
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
	bellowsMonitor  *bellows.Monitor
	lifecycleMgr    *lifecycle.Manager
	depcheckMonitor *depcheck.Monitor

	// Teams notifications (nil = disabled)
	notifier *notify.Notifier

	cancel     context.CancelFunc // cancels the Run context for graceful shutdown
	runCtx     context.Context    // the live run context; set in Run() after signal/cancel wiring

	forgeDir   string // ~/.forge
	pidFile    string
	configFile string
	logFile    *os.File
	startTime  time.Time

	// Cache for last poll results
	lastBeads   []poller.Bead
	lastBeadsMu sync.RWMutex

	// Periodic bead recovery counter (runs every N poll cycles)
	pollCount atomic.Int64

	// Cost limit: tracks which date we last logged the cost_limit_hit event
	// to avoid spamming the event log every poll cycle.
	costLimitLoggedDate atomic.Value // stores string (YYYY-MM-DD)
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

	webhookURL := cfg.Notifications.TeamsWebhookURL
	trimmedWebhookURL := strings.TrimSpace(webhookURL)
	if cfg.Notifications.Enabled && trimmedWebhookURL != "" {
		formatted, err := notify.FormatWebhookURL(trimmedWebhookURL)
		if err != nil {
			db.Close()
			logFile.Close()
			return nil, fmt.Errorf("invalid Teams webhook URL: %w", err)
		}
		webhookURL = formatted
	} else if !cfg.Notifications.Enabled && trimmedWebhookURL != "" {
		logger.Warn("Teams webhook URL is set but notifications are disabled; skipping URL validation")
	}

	notifier := notify.NewNotifier(notify.Config{
		WebhookURL: webhookURL,
		Enabled:    cfg.Notifications.Enabled,
		Events:     cfg.Notifications.Events,
	}, logger)

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
		notifier:      notifier,
	}
	// Initialize costLimitLoggedDate so Load() is always safe (zero atomic.Value
	// returns nil on Load, which is fine for type assertion, but Store("")
	// makes the intent explicit and avoids any future ambiguity).
	d.costLimitLoggedDate.Store("")
	d.cfg.Store(cfg)
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
			d.db.LogEvent(state.EventConfigReload, "Configuration reloaded", "", "")
		})
		go func() {
			if err := d.configWatcher.Start(); err != nil {
				d.logger.Error("config watcher error", "error", err)
			}
		}()
	}

	// Set up signal handling
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Store cancel so IPC shutdown command can trigger graceful stop.
	// Wrap ctx with a cancel so the IPC handler can cancel independently.
	ctx, d.cancel = context.WithCancel(ctx)
	d.runCtx = ctx

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
	d.bellowsMonitor = bellows.New(d.db, d.cfg.Load().Settings.BellowsInterval, monitorAnvils)
	d.lifecycleMgr = lifecycle.New(d.db, d.logger, d.handleLifecycleAction)
	d.lifecycleMgr.SetThresholds(
		d.config().Settings.MaxCIFixAttempts,
		d.config().Settings.MaxReviewFixAttempts,
		d.config().Settings.MaxRebaseAttempts,
	)
	if err := d.lifecycleMgr.Load(ctx); err != nil {
		d.logger.Error("failed to load lifecycle states", "error", err)
		return fmt.Errorf("daemon initialization failed: %w", err)
	}
	d.bellowsMonitor.OnEvent(d.lifecycleMgr.HandleEvent)

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
		d.depcheckMonitor = depcheck.New(d.db,
			d.config().Settings.DepcheckInterval,
			d.config().Settings.DepcheckTimeout,
			monitorAnvils)
		go func() {
			if err := d.depcheckMonitor.Run(ctx); err != nil && err != context.Canceled {
				d.logger.Error("Depcheck monitor error", "error", err)
			}
		}()
	}

	for {
		select {
		case <-ctx.Done():
			d.logger.Info("daemon shutting down", "reason", ctx.Err())
			killed := d.shutdownMgr.GracefulShutdown()
			d.shutdownMgr.CleanupWorktrees()
			d.wg.Wait() // wait for all dispatch goroutines
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
				StartedAt: time.Now(),
			})
			cifixDetectOpts := temper.DetectOptionsFromAnvilFlag(anvilCfg.GolangciLint)
			res := cifix.Fix(ctx, cifix.FixParams{
				WorktreePath:  wt.Path,
				BeadID:        req.BeadID,
				AnvilName:     req.Anvil,
				AnvilPath:     anvilCfg.Path,
				PRNumber:      req.PRNumber,
				Branch:        req.Branch,
				DB:            d.db,
				WorkerID:      workerID,
				ExtraFlags:    d.config().Settings.ClaudeFlags,
				DetectOptions: cifixDetectOpts,
				Providers:     provider.FromConfig(d.config().Settings.Providers),
			})
			status := state.WorkerDone
			if res.Error != nil {
				status = state.WorkerFailed
			}
			_ = d.db.UpdateWorkerStatus(workerID, status)

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
				Providers:    provider.FromConfig(d.cfg.Load().Settings.Providers),
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
				StartedAt: time.Now(),
			})
			res := rebase.Rebase(ctx, rebase.Params{
				WorktreePath: wt.Path,
				Branch:       req.Branch,
				BeadID:       req.BeadID,
				AnvilName:    req.Anvil,
				PRNumber:     req.PRNumber,
				DB:           d.db,
				WorkerID:     workerID,
				ExtraFlags:   d.cfg.Load().Settings.ClaudeFlags,
				Providers:    provider.FromConfig(d.cfg.Load().Settings.Providers),
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

	// Always poll so the Hearth TUI queue cache stays current even when all
	// smith slots are occupied. Capacity is checked below before dispatching.
	p := poller.New(cfg.Anvils)
	beads, results := p.Poll(ctx)

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
				BeadID:   b.ID,
				Anvil:    b.Anvil,
				Title:    b.Title,
				Priority: b.Priority,
				Status:   b.Status,
				Labels:   string(labelsJSON),
				Section:  section,
			})
		}

		// Also include in-progress beads from successful anvils.
		inProgress, inProgressResults := p.PollInProgress(ctx)
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
				BeadID:   b.ID,
				Anvil:    b.Anvil,
				Title:    b.Title,
				Priority: b.Priority,
				Status:   b.Status,
				Labels:   string(labelsJSON),
				Section:  state.QueueSectionInProgress,
			})
		}

		if err := d.db.ReplaceQueueCacheForAnvils(succeededAnvils, cacheItems); err != nil {
			d.logger.Warn("failed to cache queue", "error", err)
		}
	}

	// Preload all clarification-needed bead IDs once per poll cycle to avoid N+1 queries.
	// Fail-closed: if the DB query fails, skip dispatch this cycle so beads that need
	// clarification are not accidentally started during a transient DB error.
	clarSet, clarErr := d.db.ClarificationNeededBeadIDSet()
	if clarErr != nil {
		d.logger.Error("loading clarification-needed set; skipping dispatch this poll cycle", "error", clarErr)
		return
	}

	// Preload circuit-broken beads (needs_human=1) to avoid dispatching poison beads.
	circuitBrokenSet, circuitBrokenErr := d.db.DispatchCircuitBrokenBeadIDSet()
	if circuitBrokenErr != nil {
		d.logger.Error("loading circuit-broken set; skipping dispatch this poll cycle", "error", circuitBrokenErr)
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
	costLimit := d.cfg.Load().Settings.DailyCostLimit
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
			// Log the event only once per day to avoid spamming.
			prev, _ := d.costLimitLoggedDate.Load().(string)
			if prev != today {
				d.costLimitLoggedDate.Store(today)
				_ = d.db.LogEvent(state.EventCostLimitHit,
					fmt.Sprintf("Daily cost $%.2f reached limit $%.2f — dispatch paused", todayCost, costLimit),
					"", "")
				d.logger.Warn("daily cost limit reached, dispatch paused",
					"cost", fmt.Sprintf("$%.2f", todayCost),
					"limit", fmt.Sprintf("$%.2f", costLimit))
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
		// Skip beads already in flight
		if _, inFlight := d.activeBeads.Load(bead.ID); inFlight {
			continue
		}

		// Skip beads that need clarification (analogous to needs_human)
		if _, needed := clarSet[bead.ID+"\x00"+bead.Anvil]; needed {
			continue
		}

		// Skip beads that are circuit-broken (needs_human=1 from dispatch failures or retries)
		if _, broken := cbSet[bead.ID+"\x00"+bead.Anvil]; broken {
			continue
		}

		// Skip beads that already have an open PR (bellows should handle them)
		if hasPR, err := d.db.HasOpenPRForBead(bead.ID, bead.Anvil); err == nil && hasPR {
			d.logger.Debug("skipping bead with open PR", "bead", bead.ID)
			continue
		}

		anvilCfg := cfg.Anvils[bead.Anvil]

		// Apply auto-dispatch filtering
		if !shouldDispatch(bead, anvilCfg) {
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
				continue
			}
			anvilActive[bead.Anvil] = cnt
		}
		if anvilActive[bead.Anvil]+thisCycleAnvil[bead.Anvil] >= maxSmiths {
			continue
		}

		// Check global capacity using snapshot + in-cycle count
		if globalActive+thisCycleTotal >= maxTotal {
			break
		}

		// Claim the bead atomically before dispatching
		if err := d.claimBead(ctx, bead.ID, anvilCfg.Path); err != nil {
			d.logger.Warn("failed to claim bead", "bead", bead.ID, "error", err)
			d.recordDispatchFailure(bead.ID, bead.Anvil, fmt.Sprintf("claim failed: %v", err))
			continue
		}

		d.activeBeads.Store(bead.ID, true)
		thisCycleAnvil[bead.Anvil]++
		thisCycleTotal++
		d.wg.Add(1)
		go d.dispatchBead(ctx, bead, anvilCfg)
	}

	// Optionally log a summary of this poll cycle's dispatch activity.
	if len(thisCycleAnvil) > 0 {
		d.logger.Debug("poll cycle dispatch summary", "anvil_dispatch_counts", thisCycleAnvil)
	}
}

// dispatchBead runs the full pipeline for a single bead in a goroutine.
func (d *Daemon) dispatchBead(ctx context.Context, bead poller.Bead, anvilCfg config.AnvilConfig) {
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
		Providers:       provider.FromConfig(smithProviderSpecs),
		Notifier:        d.notifier,
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
			d.logger.Info("bead decomposed into sub-beads", "bead", bead.ID)
			// Decomposition is intentional, not a failure — clear any prior dispatch failures.
			_ = d.db.ClearRetry(bead.ID, bead.Anvil)
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
	pr, err := ghpr.Create(pipelineCtx, ghpr.CreateParams{
		WorktreePath: anvilCfg.Path,
		BeadID:       bead.ID,
		Title:        fmt.Sprintf("%s (%s)", bead.Title, bead.ID),
		Branch:       outcome.Branch,
		AnvilName:    bead.Anvil,
		DB:           d.db,
	})
	if err != nil {
		d.logger.Error("PR creation failed", "bead", bead.ID, "error", err)
		return
	}

	d.logger.Info("PR created", "bead", bead.ID, "pr", pr.URL)

	// Close the bead — also use pipelineCtx for the same reason.
	if err := d.closeBead(pipelineCtx, bead.ID, anvilCfg.Path); err != nil {
		d.logger.Warn("failed to close bead", "bead", bead.ID, "error", err)
	}
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
		payload := ipc.StatusPayload{
			Running:         true,
			PID:             os.Getpid(),
			Uptime:          time.Since(d.startTime).Round(time.Second).String(),
			Workers:         len(workers),
			QueueSize:       0, // Updated during poll
			OpenPRs:         len(prs),
			LastPoll:        "n/a",
			Quotas:          quotas,
			DailyCost:       todayCost,
			DailyCostLimit:  costLimit,
			CostLimitPaused: costLimit > 0 && todayCost >= costLimit,
		}
		data, _ := json.Marshal(payload)
		return ipc.Response{Type: "status", Payload: data}

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
		go d.pollAndDispatch(context.Background())
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

		d.wg.Add(1)
		go d.dispatchBead(context.Background(), *targetBead, anvilCfg)

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
			data, _ := json.Marshal(map[string]string{"message": "circuit breaker reset"})
			return ipc.Response{Type: "ok", Payload: data}
		}
		_ = d.db.LogEvent(state.EventRetryReset, fmt.Sprintf("Retry reset for bead %s (manual)", rp.BeadID), rp.BeadID, rp.Anvil)
		d.logger.Info("retry reset for bead", "bead", rp.BeadID, "anvil", rp.Anvil)
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
// logs a circuit-break event if the threshold is reached.
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
	}
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
