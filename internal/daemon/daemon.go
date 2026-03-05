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
	"syscall"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/cifix"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/ghpr"
	"github.com/Robin831/Forge/internal/hotreload"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/lifecycle"
	"github.com/Robin831/Forge/internal/pipeline"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/reviewfix"
	"github.com/Robin831/Forge/internal/shutdown"
	"github.com/Robin831/Forge/internal/state"
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
)

// Daemon is the main Forge orchestration daemon.
type Daemon struct {
	cfg           *config.Config
	db            *state.DB
	logger        *slog.Logger
	ipc           *ipc.Server
	shutdownMgr   *shutdown.Manager
	configWatcher *hotreload.Watcher

	// Dispatch state
	activeBeads  sync.Map       // beadID -> true, currently in-flight
	wg           sync.WaitGroup // tracks running pipeline goroutines
	worktreeMgr  *worktree.Manager
	promptBuilder *prompt.Builder

	// PR Monitoring (Bellows)
	bellowsMonitor *bellows.Monitor
	lifecycleMgr   *lifecycle.Manager

	forgeDir   string // ~/.forge
	pidFile    string
	configFile string
	logFile    *os.File
	startTime  time.Time
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

	return &Daemon{
		cfg:           cfg,
		db:            db,
		logger:        logger,
		forgeDir:      forgeDir,
		pidFile:       filepath.Join(forgeDir, PIDFileName),
		configFile:    config.ConfigFilePath(""),
		logFile:       logFile,
		shutdownMgr:   shutdown.NewManager(db, wtMgr, logger, anvilPathMap(cfg)),
		worktreeMgr:   wtMgr,
		promptBuilder: prompt.NewBuilder(),
	}, nil
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
		"anvils", len(d.cfg.Anvils),
		"poll_interval", d.cfg.Settings.PollInterval,
	)
	d.db.LogEvent(state.EventDaemonStarted, "Forge daemon started", "", "")

	// Clean up orphans from any previous crash
	if cleaned := d.shutdownMgr.CleanupOrphans(); cleaned > 0 {
		d.logger.Info("startup orphan cleanup done", "cleaned", cleaned)
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
		d.configWatcher = hotreload.NewWatcher(d.configFile, d.cfg, d.logger)
		d.configWatcher.OnChange(func(old, new *config.Config) {
			d.cfg = new
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

	// Main poll loop
	pollInterval := d.cfg.Settings.PollInterval
	if pollInterval == 0 {
		pollInterval = DefaultPollInterval
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Initial poll
	d.pollAndDispatch(ctx)

	// Start PR Monitor (Bellows)
	monitorAnvils := make(map[string]string)
	for name, a := range d.cfg.Anvils {
		if a.Path != "" {
			monitorAnvils[name] = a.Path
		}
	}
	monitorInterval := d.cfg.Settings.PollInterval
	if monitorInterval < 2*time.Minute {
		monitorInterval = 2 * time.Minute // don't poll GitHub too fast
	}
	d.bellowsMonitor = bellows.New(d.db, monitorInterval, monitorAnvils)
	d.lifecycleMgr = lifecycle.New(d.db, d.handleLifecycleAction)
	d.bellowsMonitor.OnEvent(d.lifecycleMgr.HandleEvent)

	go func() {
		if err := d.bellowsMonitor.Run(ctx); err != nil && err != context.Canceled {
			d.logger.Error("Bellows monitor error", "error", err)
		}
	}()

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

	anvilCfg, ok := d.cfg.Anvils[req.Anvil]
	if !ok {
		d.logger.Error("unknown anvil in lifecycle action", "anvil", req.Anvil)
		return
	}

	// Skip if bead is already in flight (either being implemented or another fix)
	if _, inFlight := d.activeBeads.LoadOrStore(req.BeadID, true); inFlight {
		d.logger.Info("bead already in flight, skipping lifecycle action", "bead", req.BeadID)
		return
	}

	d.wg.Add(1)

	go func() {
		defer d.wg.Done()
		defer d.activeBeads.Delete(req.BeadID)

		// Create/get worktree for the PR branch
		wt, err := d.worktreeMgr.Create(ctx, anvilCfg.Path, req.BeadID, req.Branch)
		if err != nil {
			d.logger.Error("failed to create worktree for lifecycle fix", "error", err)
			return
		}
		defer d.worktreeMgr.Remove(ctx, anvilCfg.Path, wt)

		switch req.Action {
		case lifecycle.ActionFixCI:
			d.logger.Info("spawning CI fix worker", "pr", req.PRNumber, "bead", req.BeadID)
			cifix.Fix(ctx, cifix.FixParams{
				WorktreePath: wt.Path,
				BeadID:       req.BeadID,
				AnvilName:    req.Anvil,
				AnvilPath:    anvilCfg.Path,
				PRNumber:     req.PRNumber,
				Branch:       req.Branch,
				DB:           d.db,
				ExtraFlags:   d.cfg.Settings.ClaudeFlags,
			})

		case lifecycle.ActionFixReview:
			d.logger.Info("spawning review fix worker", "pr", req.PRNumber, "bead", req.BeadID)
			reviewfix.Fix(ctx, reviewfix.FixParams{
				WorktreePath: wt.Path,
				BeadID:       req.BeadID,
				AnvilName:    req.Anvil,
				AnvilPath:    anvilCfg.Path,
				PRNumber:     req.PRNumber,
				Branch:       req.Branch,
				DB:           d.db,
				ExtraFlags:   d.cfg.Settings.ClaudeFlags,
			})

		case lifecycle.ActionCloseBead:
			d.logger.Info("closing bead after merge", "bead", req.BeadID)
			_ = d.closeBead(ctx, req.BeadID, anvilCfg.Path)

		case lifecycle.ActionCleanup:
			d.logger.Info("cleaning up PR after close", "pr", req.PRNumber)
			// Optional: delete remote branch etc.
		}
	}()
}

// pollAndDispatch polls all anvils for ready beads and dispatches workers.
func (d *Daemon) pollAndDispatch(ctx context.Context) {
	d.logger.Info("polling anvils", "count", len(d.cfg.Anvils))

	// Check global capacity first
	maxTotal := d.cfg.Settings.MaxTotalSmiths
	if maxTotal <= 0 {
		maxTotal = 4
	}
	canSpawn, err := worker.CanSpawnGlobal(d.db, maxTotal)
	if err != nil {
		d.logger.Error("checking global capacity", "error", err)
		return
	}
	if !canSpawn {
		d.logger.Info("global smith limit reached, skipping poll", "max", maxTotal)
		return
	}

	// Poll all anvils for ready beads
	p := poller.New(d.cfg.Anvils)
	beads, results := p.Poll(ctx)

	for _, r := range results {
		if r.Err != nil {
			d.logger.Warn("poll error", "anvil", r.Name, "error", r.Err)
			_ = d.db.LogEvent(state.EventPollError, r.Err.Error(), "", r.Name)
		} else {
			d.logger.Info("poll complete", "anvil", r.Name, "ready", len(r.Beads))
			_ = d.db.LogEvent(state.EventPoll, fmt.Sprintf("Polled anvil: %s (%d ready)", r.Name, len(r.Beads)), "", r.Name)
		}
	}

	// Track beads dispatched this poll cycle but not yet inserted into the DB.
	// Without this, the DB-based capacity checks see stale counts and can
	// over-dispatch before the first goroutine's InsertWorker call commits.
	thisCycleTotal := 0
	thisCycleAnvil := make(map[string]int)

	for _, bead := range beads {
		// Skip beads already in flight
		if _, inFlight := d.activeBeads.Load(bead.ID); inFlight {
			continue
		}

		// Check per-anvil capacity, accounting for beads dispatched this cycle
		// that haven't been written to the DB yet.
		anvilCfg := d.cfg.Anvils[bead.Anvil]

		// Apply auto-dispatch filtering
		if !shouldDispatch(bead, anvilCfg) {
			continue
		}

		maxSmiths := anvilCfg.MaxSmiths
		if maxSmiths <= 0 {
			maxSmiths = 1
		}
		effectiveAnvilMax := maxSmiths - thisCycleAnvil[bead.Anvil]
		if effectiveAnvilMax <= 0 {
			continue
		}
		canSpawnAnvil, err := worker.CanSpawn(d.db, bead.Anvil, effectiveAnvilMax)
		if err != nil || !canSpawnAnvil {
			continue
		}

		// Re-check global capacity (may have filled since the check above)
		effectiveGlobalMax := maxTotal - thisCycleTotal
		if effectiveGlobalMax <= 0 {
			break
		}
		canSpawn, err = worker.CanSpawnGlobal(d.db, effectiveGlobalMax)
		if err != nil || !canSpawn {
			break
		}

		// Claim the bead atomically before dispatching
		if err := d.claimBead(ctx, bead.ID, anvilCfg.Path); err != nil {
			d.logger.Warn("failed to claim bead", "bead", bead.ID, "error", err)
			continue
		}

		d.activeBeads.Store(bead.ID, true)
		thisCycleAnvil[bead.Anvil]++
		thisCycleTotal++
		d.wg.Add(1)
		go d.dispatchBead(ctx, bead, anvilCfg)
	}
}

// dispatchBead runs the full pipeline for a single bead in a goroutine.
func (d *Daemon) dispatchBead(ctx context.Context, bead poller.Bead, anvilCfg config.AnvilConfig) {
	defer d.wg.Done()
	defer d.activeBeads.Delete(bead.ID)

	d.logger.Info("dispatching bead", "bead", bead.ID, "anvil", bead.Anvil, "title", bead.Title)

	// Apply smith timeout
	smithTimeout := d.cfg.Settings.SmithTimeout
	if smithTimeout <= 0 {
		smithTimeout = 30 * time.Minute
	}
	pipelineCtx, cancel := context.WithTimeout(ctx, smithTimeout)
	defer cancel()

	outcome := pipeline.Run(pipelineCtx, pipeline.Params{
		DB:              d.db,
		WorktreeManager: d.worktreeMgr,
		PromptBuilder:   d.promptBuilder,
		AnvilName:       bead.Anvil,
		AnvilConfig:     anvilCfg,
		Bead:            bead,
		ExtraFlags:      d.cfg.Settings.ClaudeFlags,
		Providers:       provider.FromConfig(d.cfg.Settings.Providers),
	})

	if outcome.Error != nil {
		d.logger.Error("pipeline error", "bead", bead.ID, "error", outcome.Error)
		return
	}

	if !outcome.Success {
		d.logger.Warn("pipeline did not succeed", "bead", bead.ID, "verdict", outcome.Verdict)
		return
	}

	d.logger.Info("pipeline succeeded", "bead", bead.ID, "branch", outcome.Branch, "iterations", outcome.Iterations)

	// Create PR — run gh from the main repo dir since the branch is already pushed
	pr, err := ghpr.Create(ctx, ghpr.CreateParams{
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

	// Close the bead
	if err := d.closeBead(ctx, bead.ID, anvilCfg.Path); err != nil {
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
		payload := ipc.StatusPayload{
			Running:   true,
			PID:       os.Getpid(),
			Uptime:    time.Since(d.startTime).Round(time.Second).String(),
			Workers:   len(workers),
			QueueSize: 0, // Updated during poll
			OpenPRs:   len(prs),
			LastPoll:  "n/a",
			Quotas:    quotas,
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
				_ = proc.Signal(syscall.SIGINT)
			}
		}
		_ = d.db.UpdateWorkerStatus(kp.WorkerID, state.WorkerFailed)
		data, _ := json.Marshal(map[string]string{"killed": kp.WorkerID})
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
func Stop() error {
	pid, running := IsRunning()
	if !running {
		return fmt.Errorf("no daemon running")
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding process %d: %w", pid, err)
	}

	// Send interrupt signal for graceful shutdown
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

// shouldDispatch determines if a bead should be automatically dispatched based on anvil configuration.
func shouldDispatch(bead poller.Bead, anvilCfg config.AnvilConfig) bool {
	switch anvilCfg.AutoDispatch {
	case "off":
		return false
	case "tagged":
		if anvilCfg.AutoDispatchTag == "" {
			return false
		}
		for _, t := range bead.Tags {
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
