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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

	"github.com/Robin831/Forge/internal/adventurer"
	"github.com/Robin831/Forge/internal/anvilhealth"
	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/burnish"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/crucible"
	"github.com/Robin831/Forge/internal/depcheck"
	"github.com/Robin831/Forge/internal/diff"
	"github.com/Robin831/Forge/internal/epic"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/forge"
	"github.com/Robin831/Forge/internal/hotreload"
	"github.com/Robin831/Forge/internal/ingot"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/lifecycle"
	"github.com/Robin831/Forge/internal/logrotate"
	"github.com/Robin831/Forge/internal/logsweep"
	"github.com/Robin831/Forge/internal/notify"
	"github.com/Robin831/Forge/internal/pipeline"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/quench"
	"github.com/Robin831/Forge/internal/questgiver"
	"github.com/Robin831/Forge/internal/queueactions"
	"github.com/Robin831/Forge/internal/rebase"
	"github.com/Robin831/Forge/internal/schematic"
	"github.com/Robin831/Forge/internal/selfdeploy"
	"github.com/Robin831/Forge/internal/shutdown"
	"github.com/Robin831/Forge/internal/smelter"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/temper"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/Robin831/Forge/internal/vcs/github"
	"github.com/Robin831/Forge/internal/vulncheck"
	"github.com/Robin831/Forge/internal/warden"
	"github.com/Robin831/Forge/internal/wicket"
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

	// DaemonLogMaxSizeMB is the size threshold (in MB) at which daemon.log is
	// rotated. 0 defers to logrotate's package default (50 MB).
	DaemonLogMaxSizeMB = 0

	// DaemonLogMaxBackups is the number of compressed daemon.log backups kept.
	DaemonLogMaxBackups = 3

	// DefaultLogSweepInterval is how often the preserved bead-log retention
	// sweep runs.
	DefaultLogSweepInterval = 24 * time.Hour

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
	SetAutoMergeHandler(h func(ctx context.Context, anvil string, pr state.PR))
	SetSmelterEnabled(f func() bool)
	SetAssayConfig(f func(anvil string) bellows.AssayGateConfig)
	SetInFlightChecker(f func(beadID string) bool)
	SetCycleHook(f func(ctx context.Context))
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

// anvilPollSnapshot captures the most recent poll outcome for a single anvil.
// Stored in Daemon.lastPollMap so Hearth and `forge status` can read freshness
// without scanning the events table.
type anvilPollSnapshot struct {
	Timestamp time.Time
	OK        bool   // true when the last poll completed without error
	Message   string // human-readable summary, e.g. "5 ready" or the error text
}

// Daemon is the main Forge orchestration daemon.
type Daemon struct {
	cfg atomic.Pointer[config.Config]
	db  *state.DB
	// eventBus is the daemon-owned in-process event Bus, shared by reference
	// with db so that every logged event is fanned out to subscribers. It is
	// nil when settings.bus_enabled is false (legacy polling); consumers must
	// nil-check before subscribing.
	eventBus      *state.Bus
	logger        *slog.Logger
	ipc           *ipc.Server
	shutdownMgr   *shutdown.Manager
	configWatcher *hotreload.Watcher

	// Dispatch state
	activeBeads sync.Map // beadID -> true, currently in-flight
	// controlHandles is the in-memory registry of active pipeline control
	// handles keyed by beadID (beadID string -> *controlHandle). A handle is
	// created with an immutable workerID and registered after a successful
	// claim, just before the dispatchBead goroutine launches. There is a
	// brief window between activeBeads.LoadOrStore and handle registration
	// where no handle exists; IPC lookups return false during this window.
	// Not every activeBeads key has a corresponding control handle (e.g.
	// force_smith/lifecycle lock keys). Cleaned up via releaseBeadSlot.
	// See control.go.
	controlHandles sync.Map
	// pendingActions parks lifecycle actions that arrived while a bead was in
	// flight, to dispatch once it frees. Value is map[lifecycle.Action]ActionRequest
	// (one slot PER ACTION TYPE, latest-wins within a type) so a CI-fix and a
	// review-fix for the same bead no longer clobber each other — the old
	// single-slot latest-wins silently dropped one, stranding the PR in needs_fix
	// (PR #4257 / Fhi.Metadata-hyc4g). Guarded by pendingMu for the read-modify-write.
	pendingActions sync.Map
	pendingMu      sync.Mutex
	wg             sync.WaitGroup // tracks running pipeline goroutines
	pollRunning    atomic.Bool    // true while pollAndDispatch is executing; prevents concurrent overlapping polls
	worktreeMgr    *worktree.Manager
	promptBuilder  *prompt.Builder

	// lifecycleActive counts the lifecycle/bellows fix workers
	// (quench/burnish/rebase/assay) currently running a Claude session. Gated by
	// waitForLifecycleSlot against settings.max_lifecycle_workers so a burst of
	// stuck PRs cannot fan out unbounded Claude sessions and OOM the host
	// (Forge-3m06). These workers are deliberately excluded from the
	// max_total_smiths dispatch cap (see state.ActiveDispatchWorkers), so they
	// need this independent ceiling.
	lifecycleActive atomic.Int64
	lifecycleCond   *sync.Cond

	// PR Monitoring (Bellows)
	bellowsMonitor  bellowsMonitorIface
	lifecycleMgr    *lifecycle.Manager
	depcheckScanner *depcheck.Scanner

	// Vulnerability scanning
	vulnScanner *vulncheck.Scanner

	// Smelter: batches pending warden rules into PRs
	smelterWorker *smelter.Smelter

	// QuestGiver monitor (E2E quest execution)
	questgiverMonitor *questgiver.Monitor

	// Kiln: preview environments for worker branches. previewMgr is nil when
	// previews are disabled (globally, or by every anvil opting out) — see
	// preview.go, where every consumer goes through d.previews() so a disabled
	// Kiln degrades to "no previews" instead of a nil dereference. previewMu
	// guards it against the racing startPreviews/stopPreviews paths.
	previewMu  sync.Mutex
	previewMgr previewManager
	// newPreviewManager builds the Kiln manager. nil selects the real
	// buildPreviewManager; tests replace it with a fake so the wiring can be
	// exercised without worktrees, ports or child processes.
	newPreviewManager func(ctx context.Context, cfg *config.Config, anvils map[string]string) (previewManager, error)

	// questRunStore holds the on-demand quest runs against previews for this
	// daemon lifetime (see preview_quests.go). It is created lazily through
	// d.questRuns(); questRunMu guards that construction.
	questRunMu    sync.Mutex
	questRunStore *questgiver.RunStore
	// previewQuestRunner overrides where a preview quest run is dispatched.
	// nil selects d.questgiverMonitor; tests replace it with a fake so the
	// dispatch path can be exercised without a browser.
	previewQuestRunner previewQuestRunner
	// previewQuestReporter overrides where a finished preview quest run is
	// reported. nil selects a questgiver.Reporter on the real gh CLI; tests
	// replace it so the reporting hand-off can be observed without GitHub.
	previewQuestReporter previewQuestReporter

	// Wicket: GitHub issue triage monitor
	// wicketMu guards wicketMonitor to prevent data races between the
	// hot-reload callback (which may assign a new monitor) and the shutdown
	// path that calls Stop(). Use loadWicketMonitor/storeWicketMonitor helpers.
	wicketMu      sync.Mutex
	wicketMonitor *wicket.Monitor

	// Teams notifications (nil = disabled). Uses atomic.Pointer so the hot-reload
	// callback can swap in a new Notifier without a mutex while concurrent
	// pipeline goroutines safely read via Load().
	notifier atomic.Pointer[notify.Notifier]

	// Generic webhook dispatcher (nil = no generic webhooks configured).
	// Uses atomic.Pointer so the hot-reload callback can swap in a new dispatcher
	// when webhook config changes without a mutex while concurrent goroutines
	// safely read via Load(). WebhookDispatcher methods are nil-safe.
	dispatcher atomic.Pointer[notify.WebhookDispatcher]

	// assayDiffFetch and assayReview override the two external steps of an
	// Assay run — `gh pr diff` and the multi-pass engine. nil selects the real
	// ones; tests replace them so runAssayReview itself can be driven to each
	// of its terminal outcomes without a GitHub CLI or a provider.
	//
	// They exist for one invariant: every path out of runAssayReview emits
	// exactly one terminal event, so the pr_review_needed that opened a review
	// always has a matching resolution in the feed. That is a property of the
	// function, not of emitAssayTerminalEvent, and an early return added above
	// the emit would break it while every unit test of the emitter still
	// passed.
	//
	// Set before Run/IPC serving begins and never mutated afterwards.
	assayDiffFetch func(ctx context.Context, worktreePath string, prNumber int) ([]byte, error)
	assayReview    func(ctx context.Context, req assay.ReviewRequest, db *state.DB, cfg assay.Config) (*assay.ReviewResult, error)

	// assayDeltaFetch overrides the incremental-diff step of a repeat Assay
	// run (`git diff <lastReviewed> <head>` behind an ancestry check). nil
	// selects the real one; tests replace it to drive the delta/fallback
	// branches without git. Errors here are never fatal — every error falls
	// back to a full base..head review. Set before Run/IPC serving begins and
	// never mutated afterwards.
	assayDeltaFetch func(ctx context.Context, worktreePath, sinceSHA, headSHA string) ([]byte, error)

	// lifecycleDispatch overrides where a manually triggered lifecycle action
	// (the pr_action fix verbs) is sent. nil selects the real
	// handleLifecycleAction; tests replace it so the request a handler builds
	// — the rebase base branch above all — can be observed without spawning a
	// worker. Routed through dispatchLifecycleAction.
	//
	// Read from IPC handler goroutines, so it must be set before Run/IPC
	// serving begins and never mutated afterwards; swapping it mid-run is a
	// data race.
	lifecycleDispatch func(context.Context, lifecycle.ActionRequest)

	cancel context.CancelFunc // cancels the Run context for graceful shutdown
	runCtx context.Context    // the live run context; set in Run() after signal/cancel wiring

	forgeDir   string // ~/.forge
	pidFile    string
	configFile string
	logFile    *logrotate.Logger
	startTime  time.Time

	// Cache for last poll results
	lastBeads   []poller.Bead
	lastBeadsMu sync.RWMutex

	// Two-tier bead snapshot, keyed by anvil then bead ID.
	//   labeledSnapshot   — beads matching the anvil's auto_dispatch_tag.
	//                       Refreshed by every poll (fast and slow).
	//   unlabeledSnapshot — beads that do NOT match the anvil's tag but are
	//                       still ready. Refreshed only by unfiltered (slow)
	//                       polls, so they remain visible to Hearth between
	//                       fast polls instead of flickering in and out.
	// For anvils without an auto_dispatch_tag every poll is unfiltered, so
	// all of their beads land in the labeled map and the unlabeled map stays
	// empty.
	snapshotMu        sync.RWMutex
	labeledSnapshot   map[string]map[string]poller.Bead
	unlabeledSnapshot map[string]map[string]poller.Bead

	// Cached Blocks graph from the last slow (unfiltered) poll. Used by
	// fast (label-filtered) polls to detect Crucible parent beads.
	cachedBlocks   poller.BlocksGraph
	cachedBlocksMu sync.RWMutex

	// crucibleTickerResetCh signals the poll loop to reset the crucible
	// ticker when hot-reload detects a crucible_poll_interval change.
	crucibleTickerResetCh chan struct{}

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

	// Per-anvil last-poll snapshot. Updated on every poll completion (success
	// or error) by the OnAnvilDone callback. Replaces the historic
	// EventPoll-row-per-success approach that drowned the events table —
	// successful polls are no longer persisted as events, so Hearth and
	// `forge status` read freshness from this in-memory map via IPC.
	lastPollMu  sync.Mutex
	lastPollMap map[string]anvilPollSnapshot

	// anvilHealth probes anvils for a beads database left mid-merge with
	// unresolved conflicts (a "wedged" anvil). Swappable in tests.
	anvilHealth *anvilhealth.Checker
	// wedgedWarned rate-limits the repeated "still wedged" WARN per anvil.
	// map[anvilName]time.Time of the last emitted warning.
	wedgedWarned sync.Map

	// Cost limit: tracks which date we last logged the cost_limit_hit event
	// to avoid spamming the event log every poll cycle.
	costLimitLoggedDate atomic.Value // stores string (YYYY-MM-DD)

	// authEscalationMu guards authEscalated, which dedupes provider auth-failure
	// escalations to one loud event/notification per provider per day. Every bead
	// that hits the bad credential is still marked needs_human (each needs
	// attention), but the daemon-level "check your API key" alert fires once per
	// provider per day rather than on every affected bead (Forge-d5ns).
	authEscalationMu sync.Mutex
	authEscalated    map[string]bool // key: "YYYY-MM-DD|provider-label"

	// In-flight cost reservation for the daily_cost_limit gate. Recorded daily
	// cost only reflects COMPLETED spend, so without this the gate is blind to
	// the spend of workers that are already running: with N concurrent workers
	// the day could overshoot the limit by roughly N × per-bead cost
	// (Forge-s3w7). At dispatch we reserve a per-worker estimate here; the gate
	// projects recorded + reserved + one more estimate, and re-checks before
	// EACH dispatch. On worker completion the exact reserved amount is released
	// and the actual recorded cost is folded into a rolling average that feeds
	// future estimates. All fields are guarded by costReservationMu.
	costReservationMu sync.Mutex
	reservationSeq    atomic.Uint64      // monotonic key generator for reservations
	costReservations  map[uint64]float64 // reservation key -> reserved USD estimate
	avgBeadCost       float64            // rolling mean of recorded per-bead cost
	avgBeadCostN      int                // number of completed beads folded into avgBeadCost

	// pauseMu serializes pause/resume/restore transitions so the in-memory
	// atomics and persisted daemon_settings stay consistent under concurrent
	// IPC commands.
	pauseMu sync.Mutex

	// dispatchPause is the daemon-wide pause switch. While paused,
	// pollAndDispatch still polls (so the Hearth queue stays current) but
	// returns before claiming/dispatching any new beads — currently running
	// workers are left untouched and finish normally. Manual `forge queue run`
	// dispatch is still allowed (mirrors the cost-limit pause behavior).
	//
	// It carries the pause *reason* (manual vs self-deploy drain) alongside the
	// flag in a single atomic value, so status can say which one it is rather
	// than reporting every pause as an operator action. Only a manual pause is
	// persisted in state.db (daemon_settings) and restored on startup before the
	// first pollAndDispatch; the self-deploy and cost-limit pauses are
	// in-memory only. Mutate it through setDispatchPaused (see pause.go).
	dispatchPause atomic.Pointer[pauseState]

	// pausedSince records when the current manual dispatch pause began. It is
	// stored as a time.Time (zero when not manually paused) and surfaced in
	// StatusPayload.PausedSince so UIs can show "paused since <time>".
	pausedSince atomic.Value

	// selfDeployInFlight guards the self-deploy flow so overlapping merge events
	// (or a re-fired EventPRMerged) cannot launch two concurrent
	// pull/build/restart cycles. Set true while a deploy is draining or running.
	selfDeployInFlight atomic.Bool

	// Per-anvil VCS providers for PR operations (GitHub, GitLab, etc.).
	vcsProviders   map[string]vcs.Provider
	vcsProvidersMu sync.RWMutex

	// prRetryBackoff overrides the inline retry backoff used when wrapping the
	// end-of-pipeline CreatePR in transient-failure retries. nil selects
	// github.DefaultRetryBackoff(); tests set a zero-delay backoff to avoid
	// real sleeps.
	prRetryBackoff *github.RetryBackoff

	// reqTracker tracks async IPC requests so completions can be correlated
	// back to the original command. Store it by value so Daemon instances
	// created via direct struct literals still have a usable tracker.
	reqTracker ipc.RequestTracker

	// labelAdder adds a label to a bead via the bd CLI. Defaults to the real
	// bd-update implementation; may be replaced in tests to avoid exec.Command.
	labelAdder func(anvilPath, beadID, tag string) error

	// beadShower returns the raw JSON output of `bd show <id> --json`.
	// Defaults to exec.Command; may be replaced in tests.
	beadShower func(anvilPath, beadID string) (stdout []byte, stderr string, err error)

	// beadFetcher fetches a full bead by ID (via `bd show`) for the manual
	// create-PR-from-existing-branch recovery. Defaults to crucible.FetchBead;
	// may be replaced in tests to avoid a real bd invocation.
	beadFetcher func(ctx context.Context, beadID, dir string) (poller.Bead, error)

	// parentCloser closes a bead with --force. Defaults to exec.Command;
	// may be replaced in tests.
	parentCloser func(anvilPath, beadID, reason string) error

	// beadCloser runs `bd close`. nil selects the real exec-based
	// implementation; tests replace it to inject transient bd failures.
	beadCloser func(ctx context.Context, beadID, anvilPath, reason string) error

	// bdCloseRetry bounds the close-after-merge retry burst. The zero value
	// selects defaultBdRetryPolicy(); tests set a no-op sleeper.
	bdCloseRetry bdRetryPolicy

	// beadCloseInFlight collapses concurrent close attempts for the same
	// "anvil\x00bead" so the Bellows merge event and the pending-close
	// reconciler cannot run two bursts against one bead.
	beadCloseInFlight sync.Map

	// beadCloseReconciling guards the pending-close reconciler so successive
	// Bellows cycles do not stack reconcilers when one outlives the interval.
	beadCloseReconciling atomic.Bool

	// queueTimestamps holds CreatedAt/UpdatedAt strings (as emitted by
	// `bd ready --json` / `bd list --json`) keyed by "anvil/beadID". It is
	// refreshed alongside the queue_cache rebuild so the IPC "queue" handler
	// can attach timestamps to QueueItem responses without persisting them
	// to SQLite. Missing entries return zero values, which serialise as
	// empty strings on the wire.
	queueTimestampsMu sync.RWMutex
	queueTimestamps   map[string]queueTimestamp
}

// queueTimestamp pairs the two ISO timestamps that bd emits for a bead. The
// strings are passed through verbatim (no parsing) so any timezone or
// precision quirks from bd flow straight to the client.
type queueTimestamp struct {
	CreatedAt string
	UpdatedAt string
}

// configureEventBus constructs the in-process event Bus and wires it into the
// state DB when settings.bus_enabled is true, returning the Bus so the daemon
// can share it (by reference) with real-time SSE/IPC consumers. The Bus is
// non-blocking (drop-oldest), so publishing never stalls LogEvent.
//
// Construction is gated for safe rollout: when the flag is off it returns nil
// and never calls db.SetBus, so LogEvent's Publish path no-ops (see
// DB.logEventAt) and the legacy polling behaviour (consumers re-read via
// EventsSince) is retained. When on, the per-subscriber buffer is sized from
// settings.bus_buffer_size (falling back to config.DefaultBusBufferSize).
func configureEventBus(cfg *config.Config, db *state.DB, logger *slog.Logger) *state.Bus {
	if !cfg.Settings.BusEnabled {
		return nil
	}
	bufSize := cfg.Settings.ResolvedBusBufferSize()
	bus := state.NewBus(bufSize)
	db.SetBus(bus)
	// Wire a dedicated findings-changed Bus alongside the event bus, gated on the
	// same bus_enabled flag. Keeping it separate keeps the PR-findings SSE stream
	// from receiving every logged event (and the activity stream from receiving
	// findings signals). The daemon does not retain a reference: the only consumer
	// is the web PR-findings stream, which reaches it via db.FindingsBus().
	db.SetFindingsBus(state.NewBus(bufSize))
	if logger != nil {
		logger.Info("event bus enabled", "buffer_size", bufSize)
	}
	return bus
}

// New creates a new daemon instance. configPath is the resolved config file the
// daemon was started with (honouring --config); it is used for the hot-reload
// watcher and the web settings API so both read/write the SAME file the daemon
// loaded, not a default-probe guess. An empty configPath falls back to the
// documented default locations.
func New(cfg *config.Config, configPath string) (*Daemon, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("finding home directory: %w", err)
	}

	forgeDir := filepath.Join(home, ".forge")
	if err := os.MkdirAll(filepath.Join(forgeDir, LogDir), 0o755); err != nil {
		return nil, fmt.Errorf("creating log directory: %w", err)
	}

	logPath := filepath.Join(forgeDir, LogDir, LogFileName)
	// Size-based rotation keeps daemon.log bounded: rotate at 50 MB, keep 3
	// compressed backups. An already-oversized file is rotated out on first
	// write. The retention sweep never touches this handle (see logsweep).
	logFile := &logrotate.Logger{
		Filename:   logPath,
		MaxSizeMB:  DaemonLogMaxSizeMB,
		MaxBackups: DaemonLogMaxBackups,
		Compress:   true,
	}
	if err := logFile.Open(); err != nil {
		return nil, fmt.Errorf("opening log file: %w", err)
	}

	// Log to both file and stderr
	multiWriter := io.MultiWriter(logFile, os.Stderr)
	logger := slog.New(slog.NewTextHandler(multiWriter, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	// Make package-level slog.Info/Warn/Error calls (e.g. from worktree.ProbeNodeModules
	// and worktree.unlinkReparsePoints) route through the same handler so their
	// output reaches ~/.forge/logs/daemon.log alongside d.logger.* calls.
	slog.SetDefault(logger)

	db, err := state.Open("")
	if err != nil {
		logFile.Close()
		return nil, fmt.Errorf("opening state database: %w", err)
	}

	// Own the in-process event Bus here and share it (by reference) with the
	// state DB so every logged event is fanned out to subscribers. See
	// configureEventBus for the bus_enabled gating and nil-safety contract.
	eventBus := configureEventBus(cfg, db, logger)

	// One-time reconciliation: repair worker rows whose log_path dangles at a
	// removed worktree but whose preserved copy still exists under ~/.forge/logs.
	// Only dangling rows are touched, so this is cheap on subsequent starts.
	if repaired, berr := db.BackfillDanglingWorkerLogPaths(filepath.Join(forgeDir, LogDir)); berr != nil {
		logger.Warn("worker log-path backfill failed", "error", berr)
	} else if repaired > 0 {
		logger.Info("repaired dangling worker log paths", "count", repaired)
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

	// Create per-anvil VCS providers from each anvil's platform config.
	vcsProviders := buildVCSProviders(cfg, db, logger)

	d := &Daemon{
		db:                    db,
		eventBus:              eventBus,
		logger:                logger,
		forgeDir:              forgeDir,
		pidFile:               filepath.Join(forgeDir, PIDFileName),
		configFile:            config.ConfigFilePath(configPath),
		logFile:               logFile,
		shutdownMgr:           shutdown.NewManager(db, wtMgr, logger, anvilPathMap(cfg)),
		worktreeMgr:           wtMgr,
		promptBuilder:         prompt.NewBuilder(),
		vcsProviders:          vcsProviders,
		reqTracker:            *ipc.NewRequestTracker("forge-"),
		crucibleTickerResetCh: make(chan struct{}, 1),
		lastPollMap:           make(map[string]anvilPollSnapshot),
		queueTimestamps:       make(map[string]queueTimestamp),
		authEscalated:         make(map[string]bool),
		anvilHealth:           anvilhealth.New(),
	}
	d.lifecycleCond = sync.NewCond(&sync.Mutex{})
	d.pausedSince.Store(time.Time{})
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

	d.shutdownMgr.OnNeedsHuman = func(beadID, anvil, title string, failures int, reason string) {
		msg := fmt.Sprintf("Bead %s flagged needs-human after %d recovery failures: %s", beadID, failures, reason)
		if title != "" {
			msg = fmt.Sprintf("Bead %s (%s) flagged needs-human after %d recovery failures: %s", beadID, title, failures, reason)
		}
		disp := d.dispatcher.Load()
		go func(beadID, anvil, msg string, failures int, reason string) {
			notifCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if n := d.notifier.Load(); n != nil {
				n.OrphanRecoveryFailed(notifCtx, anvil, beadID, title, failures, reason)
			}
			if disp != nil {
				disp.Dispatch(notifCtx, notify.EventOrphanRecoveryFailed, beadID, anvil, msg)
			}
		}(beadID, anvil, msg, failures, reason)
	}

	// Initialize costLimitLoggedDate so Load() is always safe (zero atomic.Value
	// returns nil on Load, which is fine for type assertion, but Store("")
	// makes the intent explicit and avoids any future ambiguity).
	d.costLimitLoggedDate.Store("")
	d.cfg.Store(cfg)
	applyWardenFilterConfig(cfg)
	applyPricingConfig(cfg)
	applyWorktreeTimeoutConfig(cfg)
	applyBdTimeoutConfig(cfg)
	d.labelAdder = func(anvilPath, beadID, tag string) error {
		cmd, cancel := executil.BdCommand(d.runCtx, "update", beadID, "--add-label", tag)
		defer cancel()
		cmd.Dir = anvilPath
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	}
	d.beadShower = func(anvilPath, beadID string) ([]byte, string, error) {
		// Use context.Background() so the bd show call succeeds even during
		// graceful shutdown (d.runCtx may already be cancelled at that point).
		cmd, cancel := executil.BdCommand(context.Background(), "show", beadID, "--json")
		defer cancel()
		cmd.Dir = anvilPath
		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf
		out, err := cmd.Output()
		return out, stderrBuf.String(), err
	}
	d.beadFetcher = crucible.FetchBead
	d.parentCloser = func(anvilPath, beadID, reason string) error {
		// Use context.Background() so the bd close call succeeds even during
		// graceful shutdown (d.runCtx may already be cancelled at that point).
		cmd, cancel := executil.BdCommand(context.Background(), "close", beadID,
			"--force", "--reason="+reason, "--json")
		defer cancel()
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

// recordAnvilPoll updates the in-memory last-poll snapshot for the given anvil.
// Called from the OnAnvilDone callback on every poll completion regardless of
// outcome. Replaces the EventPoll event-table writes that used to provide this
// data on the success path.
func (d *Daemon) recordAnvilPoll(anvil string, ok bool, message string) {
	if anvil == "" {
		return
	}
	d.lastPollMu.Lock()
	if d.lastPollMap == nil {
		d.lastPollMap = make(map[string]anvilPollSnapshot)
	}
	d.lastPollMap[anvil] = anvilPollSnapshot{
		Timestamp: time.Now(),
		OK:        ok,
		Message:   message,
	}
	d.lastPollMu.Unlock()
}

// anvilPollSnapshots returns a copy of the per-anvil last-poll map so callers
// (e.g. the IPC status handler) can safely read it without holding the mutex.
func (d *Daemon) anvilPollSnapshots() map[string]anvilPollSnapshot {
	d.lastPollMu.Lock()
	defer d.lastPollMu.Unlock()
	if len(d.lastPollMap) == 0 {
		return nil
	}
	out := make(map[string]anvilPollSnapshot, len(d.lastPollMap))
	for k, v := range d.lastPollMap {
		out[k] = v
	}
	return out
}

// vcsForAnvil returns the VCS provider for the given anvil name.
// Falls back to a GitHub provider if no per-anvil provider is found.
func (d *Daemon) vcsForAnvil(anvil string) vcs.Provider {
	d.vcsProvidersMu.RLock()
	p, ok := d.vcsProviders[anvil]
	d.vcsProvidersMu.RUnlock()
	if ok && p != nil {
		return p
	}
	// Fallback to GitHub with DB access for PR state tracking.
	return github.New(d.db)
}

// quenchLearnConfig builds a warden.LearnConfig for the quench (CI-fix) path
// that routes learned rules into the pending_warden_rules table when smelter is enabled.
func (d *Daemon) quenchLearnConfig(anvilName string) *warden.LearnConfig {
	return &warden.LearnConfig{
		SmelterEnabled: d.cfg.Load().Settings.IsSmelterEnabled(),
		AnvilName:      anvilName,
		InsertPending:  d.db.InsertPendingRule,
	}
}

// ingotMarkFailed is a best-effort helper that sets an ingot's status to failed.
// It guards against a nil DB connection so callers don't need to repeat the pattern.
func (d *Daemon) ingotMarkFailed(beadID, anvil string) {
	if conn := d.db.Conn(); conn != nil {
		if err := ingot.UpdateIngotStatus(conn, beadID, anvil, ingot.StatusFailed); err != nil {
			d.logger.Warn("ingot status update to failed", "bead", beadID, "error", err)
		}
	}
}

// ingotRecordPR is a best-effort helper that records PR details on an ingot
// and transitions its status to pr_open. It resolves the internal PR ID from
// state.db so the ingot row references the correct foreign key.
func (d *Daemon) ingotRecordPR(beadID, anvil string, prNumber int, prURL string) {
	conn := d.db.Conn()
	if conn == nil {
		return
	}
	var prID *int
	if dbPR, err := d.db.GetPRByNumber(anvil, prNumber); err == nil && dbPR != nil {
		id := dbPR.ID
		prID = &id
	}
	if err := ingot.UpdateIngotPR(conn, beadID, anvil, prNumber, prURL, prID); err != nil {
		d.logger.Warn("ingot PR update failed", "bead", beadID, "error", err)
	}
	if err := ingot.UpdateIngotStatus(conn, beadID, anvil, ingot.StatusPROpen); err != nil {
		d.logger.Warn("ingot status update to pr_open failed", "bead", beadID, "error", err)
	}
}

// ingotRecordPRCreateFailed is a best-effort helper that records a failed PR
// creation on an ingot: it persists the pushed branch, head SHA, and classified
// error and transitions the ingot to pr_create_failed so an operator can recover
// the branch via `forge queue create-pr` without re-running Smith.
func (d *Daemon) ingotRecordPRCreateFailed(beadID, anvil, branch, headSHA, classifiedErr string) {
	if conn := d.db.Conn(); conn != nil {
		if err := ingot.UpdateIngotPRCreateFailed(conn, beadID, anvil, branch, headSHA, classifiedErr); err != nil {
			d.logger.Warn("ingot pr_create_failed update failed", "bead", beadID, "error", err)
		}
	}
}

// ingotClearPRCreateError is a best-effort helper that clears a previously
// recorded PR-creation error on an ingot. It is called on the recovery path so a
// successfully reopened PR no longer surfaces a stale failure.
func (d *Daemon) ingotClearPRCreateError(beadID, anvil string) {
	if conn := d.db.Conn(); conn != nil {
		if err := ingot.ClearIngotPRCreateError(conn, beadID, anvil); err != nil {
			d.logger.Warn("ingot pr_create_error clear failed", "bead", beadID, "error", err)
		}
	}
}

// buildPRCreateParams constructs the vcs.CreateParams shared by every PR-creation
// path (end-of-pipeline finalize, stranded-branch auto-recovery, and the manual
// create-PR-from-existing-branch primitive). Centralising it here keeps the PR
// title/body inputs identical across paths so they cannot drift. changeSummary
// and reviewerNotes are the author-written and reviewer-written body sections;
// pass "" for either when not available (the provider generates a default body).
func (d *Daemon) buildPRCreateParams(bead poller.Bead, worktreePath, branch, changeSummary, reviewerNotes, externalRef string) vcs.CreateParams {
	return vcs.CreateParams{
		WorktreePath:    worktreePath,
		BeadID:          bead.ID,
		Title:           fmt.Sprintf("%s (%s)", bead.Title, bead.ID),
		Branch:          branch,
		Base:            bead.EpicBranch, // empty = use provider default base
		AnvilName:       bead.Anvil,
		BeadTitle:       bead.Title,
		BeadDescription: bead.Description,
		BeadType:        bead.IssueType,
		ChangeSummary:   changeSummary,
		ReviewerNotes:   reviewerNotes,
		ExternalRef:     externalRef,
	}
}

// registerPRIfUntracked inserts match into state.db for beadID when the PR is
// not already tracked, then logs the registration event. It is a low-level
// helper called by registerExistingPRByBranch and by code paths that already
// hold a fetched *vcs.OpenPR (avoiding a redundant GetPRByHeadBranch call).
// Returns the PR number on success, 0 on failure.
func (d *Daemon) registerPRIfUntracked(ctx context.Context, anvilName, beadID string, match *vcs.OpenPR, baseBranch string) int {
	if existing, _ := d.db.GetPRByNumber(anvilName, match.Number); existing != nil {
		// Already tracked — nothing to do, HasOpenPRForBead will return true.
		return match.Number
	}
	dbPR := &state.PR{
		Number:     match.Number,
		Anvil:      anvilName,
		BeadID:     beadID,
		Branch:     match.Branch,
		BaseBranch: baseBranch,
		Status:     state.PROpen,
		CreatedAt:  time.Now(),
		Title:      match.Title,
	}
	if err := d.db.InsertPR(dbPR); err != nil {
		d.logger.Warn("failed to insert existing PR record",
			"anvil", anvilName, "pr", match.Number, "bead", beadID, "error", err)
		return 0
	}
	d.logger.Info("registered existing PR",
		"bead", beadID, "anvil", anvilName, "pr", match.Number, "branch", match.Branch)
	if logErr := d.db.LogEvent(state.EventPRCreated,
		fmt.Sprintf("Registered existing PR #%d for branch %s", match.Number, match.Branch),
		beadID, anvilName); logErr != nil {
		d.logger.Warn("failed to log PR registration event", "bead", beadID, "error", logErr)
	}
	return match.Number
}

// registerExistingPRByBranch handles the ErrPRAlreadyExists case from CreatePR
// by looking up the open PR matching the given branch via the VCS provider and
// registering it in state.db when it is not already tracked. Without this
// step, HasOpenPRForBead returns false on the next orphan-recovery sweep and
// the bead is reset to open and re-dispatched, producing a redispatch loop
// that burns Smith tokens to declare "no changes needed" each iteration.
// baseBranch is the PR's target branch (bead.EpicBranch or the repo default);
// storing it prevents rebase/merge actions from defaulting to main for Crucible
// child beads that target a feature branch.
// Returns the PR number on success (0 if the PR could not be located or
// registration failed).
func (d *Daemon) registerExistingPRByBranch(ctx context.Context, anvilName, anvilPath, beadID, branch, baseBranch string) int {
	if branch == "" {
		return 0
	}
	match, err := d.vcsForAnvil(anvilName).GetPRByHeadBranch(ctx, anvilPath, branch)
	if err != nil {
		d.logger.Warn("could not look up open PR by branch to register after ErrPRAlreadyExists",
			"anvil", anvilName, "branch", branch, "bead", beadID, "error", err)
		return 0
	}
	if match == nil {
		d.logger.Warn("ErrPRAlreadyExists but no open PR found by branch — orphan recovery may re-dispatch this bead",
			"anvil", anvilName, "branch", branch, "bead", beadID)
		return 0
	}
	return d.registerPRIfUntracked(ctx, anvilName, beadID, match, baseBranch)
}

// buildVCSProviders creates a VCS provider for each configured anvil based on
// its platform setting. Anvils without a platform default to GitHub.
// The state DB is passed to the GitHub provider so it can record PR metadata.
func buildVCSProviders(cfg *config.Config, db *state.DB, logger *slog.Logger) map[string]vcs.Provider {
	providers := make(map[string]vcs.Provider, len(cfg.Anvils))
	for name, anvil := range cfg.Anvils {
		platform, _ := vcs.ParsePlatform(anvil.Platform)
		switch platform {
		case vcs.GitHub:
			providers[name] = github.New(db)
		default:
			p, err := vcs.ForPlatform(anvil.Platform)
			if err != nil {
				logger.Warn("failed to create VCS provider for anvil, falling back to GitHub",
					"anvil", name, "platform", anvil.Platform, "error", err)
				p = github.New(db)
			}
			providers[name] = p
		}
	}
	return providers
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

// reconcileOpenPRs fetches open PRs from each anvil's VCS platform and
// registers any that are missing from the state DB. This ensures Bellows
// monitors PRs even after a DB reset or if the PR was created outside a
// recorded Forge pipeline session.
//
// Ownership is established by the per-instance forge-managed marker (see
// vcs.MarkerForID and vcs.ForgeID) embedded by Forge when it creates a PR.
// A bare "**Bead**: <id>" reference in the body is NOT sufficient — PR
// templates, "Closes" lines, and manual bead mentions must not cause Forge
// to start pushing review-fix commits or auto-merging unrelated contributors'
// PRs. The legacy generic marker emitted by pre-Forge-i1g7 versions
// (`<!-- forge-managed: true -->`) is also NOT sufficient: in multi-forge
// deployments any instance could have authored such a PR. PRs without the
// current instance's marker are still tracked (so Hearth can display them)
// but are stored with bellows_managed=false.
//
// Already-tracked PRs are also re-checked on every reconcile so that PRs
// adopted under the legacy logic (Forge versions before m1ui) automatically
// have bellows_managed flipped off the next time this runs, without manual
// state.db edits or pod resets. Going the other way (false → true) is also
// supported so an external PR that gains the marker mid-flight gets adopted
// on the next reconcile.
func (d *Daemon) reconcileOpenPRs(ctx context.Context) {
	myForgeID := d.cfg.Load().Settings.ResolvedForgeID()
	for anvilName, anvilCfg := range d.cfg.Load().Anvils {
		if anvilCfg.Path == "" {
			continue
		}
		prs, err := d.vcsForAnvil(anvilName).ListOpenPRs(ctx, anvilCfg.Path)
		if err != nil {
			d.logger.Warn("reconcile: could not list open PRs", "anvil", anvilName, "err", err)
			continue
		}
		for _, pr := range prs {
			forgeManaged := vcs.IsForgeManagedBy(pr.Body, myForgeID)
			existing, _ := d.db.GetPRByNumber(anvilName, pr.Number)
			if existing != nil {
				d.reevaluateTrackedPR(existing, pr, anvilName, forgeManaged, myForgeID)
				continue
			}
			beadID := extractBeadID(pr.Body)
			if !forgeManaged {
				// PR is not created by THIS Forge instance. The body may still
				// reference a bead ID — e.g. a contributor's manual PR, a PR
				// template hint, or a PR authored by a sibling Forge instance
				// pointed at the same anvil. Without OUR marker we have no
				// proof we created it. Track it as external so Bellows leaves
				// it alone.
				if beadID != "" {
					reason := "no forge-managed marker"
					if vcs.IsLegacyForgeManaged(pr.Body) {
						reason = "legacy generic forge-managed marker; ambiguous in multi-forge deployments"
					}
					d.logger.Info("reconcile: PR references a bead but is not managed by this forge; tracking as external",
						"pr", pr.Number, "anvil", anvilName, "referenced_bead", beadID,
						"forge_id", myForgeID, "reason", reason)
				}
				beadID = "ext-" + strconv.Itoa(pr.Number)
			} else if beadID == "" {
				// Marker present (this forge created the PR) but the body carries
				// no parseable "Bead:" reference — e.g. an auto-opened stranded-
				// branch recovery PR, or a body edited after creation. Recover the
				// real bead ID from the canonical forge branch name (forge/<bead-id>)
				// instead of falling back to a synthetic ext-<number> placeholder.
				// This is required for merge-close: handleBeadCloseOnMerge keys off
				// a real bead_id, so an ext-* row would merge without ever closing
				// its bead. Branch recovery is gated on OUR marker so a sibling
				// forge's forge/<id> PR is still tracked as external (Forge-i1g7
				// multi-forge safety); only the marker grants ownership (Forge-wor5).
				if parsed, ok := worktree.BeadIDFromBranch(pr.Branch); ok {
					beadID = parsed
				} else {
					beadID = "ext-" + strconv.Itoa(pr.Number)
				}
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
				d.logger.Info("reconcile: registered untracked PR",
					"pr", pr.Number, "bead", beadID, "anvil", anvilName,
					"forge_managed", forgeManaged, "forge_id", myForgeID)
				// Persist title from upstream
				if pr.Title != "" {
					_ = d.db.UpdatePRTitle(dbPR.ID, pr.Title)
				}
				// Only PRs Forge created (forge-managed marker present) and
				// with a parseable bead ID are eligible for bellows lifecycle
				// management. External PRs and forge-managed-but-unparseable
				// PRs (synthetic ext-* IDs) are display-only.
				if !forgeManaged || strings.HasPrefix(beadID, "ext-") {
					_ = d.db.UpdatePRBellowsManaged(dbPR.ID, false)
				}
			}
		}
	}
}

// reevaluateTrackedPR adjusts the bellows_managed flag on an already-tracked
// PR so it stays in sync with the current ownership rule. This is the second
// half of the Forge-i1g7 fix: PRs that were adopted under the legacy logic
// (anything mentioning a bead) keep bellows_managed=true forever otherwise,
// so disabling bellows on a sibling forge never frees the PR even after the
// per-instance marker rolls out. Re-evaluating on every reconcile makes the
// transition automatic — no manual state.db edits or pod resets required.
//
// Synthetic ext-* PRs are not eligible for bellows management via the legacy
// auto-adoption path, but a user can explicitly assign bellows to them via the
// assign_bellows IPC action; those user-pinned rows carry
// bellows_manually_assigned=1 and must be left alone by reconcile (Forge-l125).
func (d *Daemon) reevaluateTrackedPR(existing *state.PR, pr vcs.OpenPR, anvilName string, forgeManaged bool, myForgeID string) {
	// ext-* rows are synthetic identifiers for PRs Forge did not create.
	// Clear bellows_managed only when it was set by legacy auto-adoption —
	// not when the user explicitly assigned bellows via assign_bellows.
	if strings.HasPrefix(existing.BeadID, "ext-") {
		if existing.BellowsManaged && !existing.BellowsManuallyAssigned {
			_ = d.db.UpdatePRBellowsManaged(existing.ID, false)
			d.logger.Info("reconcile: cleared bellows_managed on auto-adopted ext-* PR",
				"pr", pr.Number, "anvil", anvilName, "bead", existing.BeadID)
		}
		return
	}
	if forgeManaged && !existing.BellowsManaged {
		_ = d.db.UpdatePRBellowsManaged(existing.ID, true)
		d.logger.Info("reconcile: adopted previously-external PR as bellows-managed",
			"pr", pr.Number, "anvil", anvilName, "bead", existing.BeadID, "forge_id", myForgeID)
		return
	}
	if !forgeManaged && existing.BellowsManaged {
		reason := "no forge-managed marker"
		if vcs.IsLegacyForgeManaged(pr.Body) {
			reason = "legacy generic forge-managed marker; ambiguous in multi-forge deployments"
		}
		_ = d.db.UpdatePRBellowsManaged(existing.ID, false)
		d.logger.Info("reconcile: released tracked PR (no longer managed by this forge)",
			"pr", pr.Number, "anvil", anvilName, "bead", existing.BeadID,
			"forge_id", myForgeID, "reason", reason)
	}
}

// extractBeadID parses a bead ID from a PR body. It recognises both the
// PR footer Forge emits ("Bead: Forge-abc | Branch: forge/Forge-abc", see
// vcs.buildPRBody) and the bold markdown form ("**Bead**: Forge-abc") that
// can appear in Smith-authored sections or contributor PRs that mention a
// bead. The bold form is checked first so it wins when both appear.
//
// Note: a successful extraction does NOT imply Forge created the PR — only
// vcs.IsForgeManagedBy answers that for a given instance. extractBeadID is
// used for routing and display.
func extractBeadID(body string) string {
	if id := extractBeadIDWithMarker(body, "**Bead**: "); id != "" {
		return id
	}
	return extractBeadIDWithMarker(body, "Bead: ")
}

func extractBeadIDWithMarker(body, marker string) string {
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

	// Establish the forge instance id used in the per-instance forge-managed
	// marker on every PR Forge creates. This must run before any CreatePR
	// call and before reconcileOpenPRs so PR bodies and ownership checks all
	// agree on the same id.
	forgeID := d.cfg.Load().Settings.ResolvedForgeID()
	vcs.SetForgeID(forgeID)

	d.logger.Info("daemon started",
		"pid", os.Getpid(),
		"anvils", len(d.cfg.Load().Anvils),
		"poll_interval", d.cfg.Load().Settings.PollInterval,
		"forge_id", forgeID,
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

	// Surface beads that were paused before this restart. Their worker rows and
	// worktrees survived, but the parked pipeline goroutines did not, so they
	// cannot be resumed via the live control handle. Log them and record a
	// recovery event; NeedsAttentionBeads already surfaces paused workers with
	// resume/discard actions, and resume_bead cold-resumes them in place.
	if surfaced := d.recoverPausedWorkers(); surfaced > 0 {
		d.logger.Info("startup paused-worker recovery done", "surfaced", surfaced)
	}

	// Set up signal handling and run context BEFORE starting IPC server,
	// so IPC handlers always see a valid, race-free runCtx.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Store cancel so IPC shutdown command can trigger graceful stop.
	// Wrap ctx with a cancel so the IPC handler can cancel independently.
	ctx, d.cancel = context.WithCancel(ctx)
	d.runCtx = ctx

	// Restore a manual dispatch pause persisted from a previous run so a
	// pause set before a restart (e.g. a systemd restart) is not silently
	// undone. Must run before starting IPC/web handlers so all commands
	// observe the restored state immediately. The cost-limit pause is
	// intentionally not persisted (recomputed from daily costs).
	d.restoreDispatchPause()

	// Start Kiln preview environments (no-op when previews are disabled).
	// Reconciliation of previews orphaned by a previous daemon lifetime runs
	// before IPC/web handlers come up, so no caller can observe — or Touch — a
	// preview row whose processes are already gone.
	d.startPreviews(ctx)

	// Start IPC server
	d.ipc = ipc.NewServer()
	d.ipc.OnCommand(d.handleIPC)
	go func() {
		if err := d.ipc.Start(ctx); err != nil {
			d.logger.Error("IPC server error", "error", err)
		}
	}()

	// Bridge the in-process event Bus to IPC subscribers (the Hearth TUI) so its
	// event feed rides pushed events instead of ticker-polling the events table.
	// No-op when the Bus is disabled (settings.bus_enabled off), leaving the
	// legacy poll path intact.
	go d.forwardBusEvents(ctx)

	// Start optional Hearth 2.0 web server (gated by FORGE_WEB_ENABLED).
	if err := d.startWebServer(ctx); err != nil {
		d.logger.Error("web server failed to start", "error", err)
	}

	// Start config hot-reload watcher
	if d.configFile != "" {
		d.configWatcher = hotreload.NewWatcher(d.configFile, d.cfg.Load(), d.logger)
		d.configWatcher.OnChange(func(old, new *config.Config) {
			d.cfg.Store(new)
			applyWardenFilterConfig(new)
			applyPricingConfig(new)
			applyWorktreeTimeoutConfig(new)
			applyBdTimeoutConfig(new)
			// Re-publish the resolved forge id so PR-body builders and the
			// reconciler use the new value immediately. Resolution falls back
			// to os.Hostname() so this normally only changes when the operator
			// edits settings.forge_id explicitly.
			oldID := old.Settings.ResolvedForgeID()
			newID := new.Settings.ResolvedForgeID()
			if oldID != newID {
				vcs.SetForgeID(newID)
				d.logger.Info("forge_id changed via config reload", "old", oldID, "new", newID)
			}
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
			// Always check smelter settings on each reload: interval/enabled can change
			// independently of anvil paths and must not be gated on anvil change detection.
			d.updateSmelterSettings(old, new)
			// Propagate config changes to Wicket monitor (interval, labels, etc.)
			d.updateWicketConfig(new)
			// A per-anvil preview opt-in applies from here on, but only if a
			// preview manager exists to serve it.
			d.warnPreviewOptInWithoutManager(new)
			// Signal the poll loop to reset the crucible ticker when the interval changes.
			if old.Settings.CruciblePollInterval != new.Settings.CruciblePollInterval {
				select {
				case d.crucibleTickerResetCh <- struct{}{}:
				default:
				}
			}
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

	// Initial poll (full/unfiltered to populate the Blocks cache)
	d.pollAndDispatch(ctx, true)

	// Start PR Monitor (Bellows)
	monitorAnvils := make(map[string]string)
	for name, a := range d.cfg.Load().Anvils {
		if a.Path != "" {
			monitorAnvils[name] = a.Path
		}
	}
	d.bellowsMonitor = bellows.New(d.db, d.vcsForAnvil, d.cfg.Load().Settings.BellowsInterval, monitorAnvils, func() bool {
		return d.cfg.Load().Settings.AutoLearnRules
	}, func() int {
		return d.cfg.Load().Settings.MaxCIFixAttempts
	}, func() int {
		return d.cfg.Load().Settings.MaxReviewFixAttempts
	}, func() int {
		return d.cfg.Load().Settings.MaxRebaseAttempts
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
	d.bellowsMonitor.SetAutoMergeHandler(d.handleAutoMerge)
	d.bellowsMonitor.SetSmelterEnabled(func() bool {
		return d.cfg.Load().Settings.IsSmelterEnabled()
	})
	// Provide the Assay trigger gate with the resolved per-anvil Assay config so
	// hot-reloaded changes take effect without a restart. Returning the config
	// each call (rather than capturing it once) keeps the gate live-configured.
	d.bellowsMonitor.SetAssayConfig(func(anvil string) bellows.AssayGateConfig {
		ra := d.cfg.Load().ResolvedAssay(anvil)
		return bellows.AssayGateConfig{
			Enabled:           ra.IsEnabled(),
			SkipDrafts:        ra.IsSkipDrafts(),
			DebounceSeconds:   ra.GetDebounceSeconds(),
			DailyCostLimitUSD: ra.GetDailyCostLimitUSD(),
			MaxRuns:           ra.GetMaxRuns(),
		}
	})
	// Let the still-failing/unresolved retry branches re-dispatch a fix only
	// when no worker is actually running for the bead — keyed off the same
	// activeBeads lock the dispatcher uses — instead of the old
	// `pr.Status != needs_fix` proxy that wedged PRs parked in needs_fix.
	d.bellowsMonitor.SetInFlightChecker(func(beadID string) bool {
		if beadID == "" {
			return false
		}
		_, inFlight := d.activeBeads.Load(beadID)
		return inFlight
	})
	// Re-attempt any close-after-merge that a previous cycle (or a previous
	// daemon lifetime) could not complete. Registered as a cycle hook rather
	// than an event handler because the triggering PR is already merged and
	// will never emit another event.
	d.bellowsMonitor.SetCycleHook(d.kickPendingBeadCloseReconcile)
	d.bellowsMonitor.OnEvent(d.lifecycleMgr.HandleEvent)
	d.bellowsMonitor.OnEvent(d.handleBellowsNotifications)
	d.bellowsMonitor.OnEvent(d.handleBeadCloseOnMerge)
	d.bellowsMonitor.OnEvent(d.handleWicketPRMerged)
	d.bellowsMonitor.OnEvent(d.handleSelfDeploy)
	d.bellowsMonitor.OnEvent(d.handlePreviewTeardownOnPRClose)
	d.bellowsMonitor.OnEvent(d.handlePreviewAutoStart)

	// Reconcile: register any open PRs not yet tracked in the state DB.
	// This handles PRs created before the current DB or after a DB reset.
	d.reconcileOpenPRs(ctx)

	go func() {
		if err := d.bellowsMonitor.Run(ctx); err != nil && err != context.Canceled {
			d.logger.Error("Bellows monitor error", "error", err)
		}
	}()

	// Catch-up pass: close beads whose PRs merged while the daemon was down.
	// Delayed 30s to let bellows complete its first poll cycle.
	go func() {
		t := time.NewTimer(30 * time.Second)
		defer t.Stop()
		select {
		case <-t.C:
			d.reconcileMergedBeads(ctx)
		case <-ctx.Done():
			if !t.Stop() {
				<-t.C
			}
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
		// Provide per-anvil auto-dispatch tags so the scanner uses each
		// anvil's configured label rather than a hard-coded default.
		depcheckTags := make(map[string]string, len(depcheckAnvils))
		for name := range depcheckAnvils {
			if ac, ok := d.cfg.Load().Anvils[name]; ok && ac.AutoDispatchTag != "" {
				depcheckTags[name] = ac.AutoDispatchTag
			}
		}
		d.depcheckScanner.UpdateAnvilTags(depcheckTags)
		// Never sync an anvil's node_modules while a Kiln preview is linked
		// into that checkout — `npm ci` deletes node_modules first, and the
		// delete reaches through the link into the running preview.
		d.depcheckScanner.SetPreviewLiveness(d.previewBeadForAnvil)
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

	// Start preserved bead-log retention sweep. retentionDays is read fresh each
	// pass so config hot-reload takes effect; 0 disables the sweep. This is
	// independent of daemon.log rotation and never touches that live handle.
	logSweepInterval := d.config().Settings.LogSweepInterval
	if logSweepInterval == 0 {
		logSweepInterval = DefaultLogSweepInterval
	}
	logSweep := logsweep.New(
		d.db,
		d.logger,
		filepath.Join(d.forgeDir, LogDir),
		logSweepInterval,
		func() int { return d.config().Settings.LogRetentionDays },
	)
	go logSweep.RunScheduled(ctx)

	// Start Wicket issue triage monitor (if enabled)
	if d.config().Settings.WicketEnabled {
		d.wicketMu.Lock()
		wm := wicket.New(d.cfg.Load(), d.db)
		d.wicketMonitor = wm
		d.wicketMu.Unlock()
		go func() {
			if err := wm.Run(ctx); err != nil && err != context.Canceled {
				d.logger.Error("Wicket monitor error", "error", err)
			}
		}()
	} else {
		d.logger.Info("wicket disabled via configuration (wicket_enabled: false)")
	}

	// Start smelter (batches pending warden rules into PRs on a schedule).
	// Always create the worker when enabled so hot-reload can update interval/paths.
	// Run handles interval <= 0 by pausing the ticker until a positive value arrives.
	if d.config().Settings.IsSmelterEnabled() {
		d.smelterWorker = smelter.New(
			d.db,
			d.config().Settings.SmelterInterval,
			monitorAnvils,
			smelter.WithConsolidator(warden.DefaultConsolidationRunner()),
			smelter.WithDedupThreshold(func() float64 {
				return d.config().Settings.Warden.ResolvedDedupThreshold()
			}),
			smelter.WithArchiveAfterDays(func() int {
				return d.config().Settings.Warden.ResolvedArchiveAfterDays()
			}),
		)
		go func() {
			if err := d.smelterWorker.Run(ctx); err != nil && err != context.Canceled {
				d.logger.Error("Smelter error", "error", err)
			}
		}()
		if d.config().Settings.SmelterInterval <= 0 {
			d.logger.Info("smelter enabled but smelter_interval <= 0; scheduled flushes are paused until interval is updated")
		}
	} else {
		d.logger.Info("smelter disabled via configuration (smelter_enabled: false)")
	}

	// Start QuestGiver. It serves two paths that are enabled independently: the
	// scheduled scan (questgiver_enabled + questgiver_interval) and on-demand
	// runs against a Kiln preview (per-anvil preview_quests). The monitor is
	// therefore constructed when *either* wants it — an anvil that opted into
	// preview quests must be able to run them even with scheduled scanning off,
	// which is the whole point of the two flags being separate — while only the
	// scan starts the polling loop.
	questScanEnabled := d.config().Settings.IsQuestgiverEnabled() && d.config().Settings.QuestgiverInterval > 0
	if d.config().Settings.IsQuestgiverEnabled() && d.config().Settings.QuestgiverInterval <= 0 {
		d.logger.Error("QuestGiver enabled but questgiver_interval <= 0; skipping scheduled quest scans")
	}
	previewQuestPaths := previewQuestAnvils(d.cfg.Load())
	if questScanEnabled || len(previewQuestPaths) > 0 {
		qgAnvils := map[string]string{}
		if questScanEnabled {
			qgAnvils = filterQuestgiverAnvils(monitorAnvils, d.cfg.Load().Anvils)
			for name := range monitorAnvils {
				if _, ok := qgAnvils[name]; !ok {
					d.logger.Info("Skipping anvil for questgiver (questgiver_enabled=false)", "anvil", name)
				}
			}
		}
		adventurerTimeout := d.config().Settings.AdventurerTimeout
		newExec := func() questgiver.QuestExecutor {
			return &adventurerExecutorAdapter{
				exec: adventurer.New(adventurerTimeout, d.logger),
			}
		}
		d.questgiverMonitor = questgiver.New(d.db,
			d.config().Settings.QuestgiverInterval,
			adventurerTimeout,
			qgAnvils, newExec)
		// Quests can also be run on demand against a preview environment, for
		// the anvils that opted in with preview_quests. That path needs its own
		// anvil set (it is not filtered by questgiver_enabled) and a way to
		// check the preview is healthy before driving a browser at it.
		d.questgiverMonitor.SetPreviewQuestAnvils(previewQuestPaths)
		d.questgiverMonitor.SetPreviewLookup(d.previewQuestLookup)
		if questScanEnabled {
			go func() {
				if err := d.questgiverMonitor.Run(ctx); err != nil && err != context.Canceled {
					d.logger.Error("QuestGiver monitor error", "error", err)
				}
			}()
		} else {
			d.logger.Info("QuestGiver scheduled scans are off; monitor is up for preview quest runs only",
				"preview_quest_anvils", len(previewQuestPaths))
		}
	}

	// Two-tier polling: fast ticker uses label-filtered polls for dispatch;
	// slow ticker runs unfiltered polls to rebuild the Crucible Blocks graph.
	// When CruciblePollInterval is 0, all polls are unfiltered (legacy mode).
	crucibleInterval := d.cfg.Load().Settings.CruciblePollInterval
	var crucibleTicker *time.Ticker
	var crucibleC <-chan time.Time
	if crucibleInterval > 0 {
		crucibleTicker = time.NewTicker(crucibleInterval)
		crucibleC = crucibleTicker.C
	}
	defer func() {
		if crucibleTicker != nil {
			crucibleTicker.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			d.logger.Info("daemon shutting down", "reason", ctx.Err())
			d.wicketMu.Lock()
			wm := d.wicketMonitor
			d.wicketMu.Unlock()
			if wm != nil {
				wm.Stop()
			}
			// Cancelling ctx already stopped the idle reaper; the previews it
			// was watching still hold process groups and worktrees, so tear
			// them down explicitly before the daemon exits.
			d.stopPreviews(ctx)
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
			// Fast path when two-tier is active; full poll when disabled.
			d.pollAndDispatch(ctx, crucibleTicker == nil)

		case <-crucibleC:
			// Slow path: unfiltered poll to rebuild the Blocks graph.
			d.pollAndDispatch(ctx, true)

		case <-d.crucibleTickerResetCh:
			newInterval := d.cfg.Load().Settings.CruciblePollInterval
			if newInterval > 0 {
				if crucibleTicker != nil {
					crucibleTicker.Reset(newInterval)
				} else {
					crucibleTicker = time.NewTicker(newInterval)
					crucibleC = crucibleTicker.C
				}
				d.logger.Info("crucible poll interval updated", "interval", newInterval)
			} else {
				if crucibleTicker != nil {
					crucibleTicker.Stop()
					crucibleTicker = nil
					crucibleC = nil
				}
				d.logger.Info("two-tier polling disabled, all polls now unfiltered")
			}
		}
	}
}

// assaySummaryLine renders the one-line tally shown above the severity table in
// Assay's top-level review comment, e.g. "Assay (AI review): 2 important, 4 nit".
// Returns a "no issues" line when the finding set is empty so the summary review
// still reads cleanly on a clean PR.
func assaySummaryLine(findings []assay.Finding) string {
	var imp, nit, pre int
	for _, f := range findings {
		switch f.Severity {
		case assay.SeverityImportant:
			imp++
		case assay.SeverityNit:
			nit++
		case assay.SeverityPreExisting:
			pre++
		}
	}
	if imp == 0 && nit == 0 && pre == 0 {
		return "Assay (AI review): no issues found."
	}
	parts := []string{fmt.Sprintf("%d important", imp), fmt.Sprintf("%d nit", nit)}
	if pre > 0 {
		parts = append(parts, fmt.Sprintf("%d pre-existing", pre))
	}
	return "Assay (AI review): " + strings.Join(parts, ", ")
}

// statePassFailures converts the engine's failed-pass list into the persisted
// form. The two types are deliberately separate — the state package cannot
// import the engine — but they carry the same two fields, so the run record and
// the review result always name the same passes for the same reasons.
// stateAssayStatus converts the engine's run status into the persisted form,
// the same boundary and the same reason as statePassFailures below: the state
// package cannot import the engine, so the two restate one set of values. The
// explicit switch is what makes the three known statuses checked rather than
// assumed by a bare cast — readers downstream compare against the state
// constants (assayWorkerStatus, internal/web's PR fallback), and a rename on
// either side would otherwise fail silently. An unknown status is passed
// through verbatim so a value the engine adds later is still persisted.
func stateAssayStatus(s assay.RunStatus) string {
	switch s {
	case assay.RunStatusComplete:
		return state.AssayStatusComplete
	case assay.RunStatusPartial:
		return state.AssayStatusPartial
	case assay.RunStatusFailed:
		return state.AssayStatusFailed
	default:
		return string(s)
	}
}

func statePassFailures(failed []assay.PassFailure) []state.AssayPassFailure {
	if len(failed) == 0 {
		return nil
	}
	out := make([]state.AssayPassFailure, 0, len(failed))
	for _, f := range failed {
		out = append(out, state.AssayPassFailure{Name: f.Name, Reason: f.Reason})
	}
	return out
}

// assayWorkerStatus maps a finished Assay run onto the worker row's terminal
// status. A partial run gets its own status rather than being flattened into
// failed: findings were produced and posted, so the row must not read as a run
// that reviewed nothing — nor as a clean one, which "done" would imply.
func assayWorkerStatus(run *state.AssayRun, recErr error) state.WorkerStatus {
	switch {
	case recErr != nil || run == nil:
		return state.WorkerFailed
	case run.Status == state.AssayStatusPartial:
		return state.WorkerPartial
	case run.Error != "":
		return state.WorkerFailed
	default:
		return state.WorkerDone
	}
}

// restoreDispatchPause reads the persisted manual dispatch-pause flag from
// state.db and, if it was set, restores the in-memory atomic and pausedSince
// so a pause survives daemon restarts. It logs an event mirroring
// EventDispatchPaused semantics. The cost-limit pause is not persisted.
func (d *Daemon) restoreDispatchPause() {
	d.pauseMu.Lock()
	defer d.pauseMu.Unlock()

	paused, ok, err := d.db.GetSetting(state.SettingDispatchPaused)
	if err != nil {
		d.logger.Error("failed to read persisted dispatch pause", "error", err)
		return
	}
	if !ok || paused != "1" {
		return
	}
	// Only a manual pause is ever persisted, but read the reason back rather
	// than assuming it: a state.db written by an older Forge has no reason at
	// all, and setDispatchPaused normalizes that empty value to manual.
	reason, _, err := d.db.GetSetting(state.SettingDispatchPauseReason)
	if err != nil {
		d.logger.Warn("failed to read dispatch pause reason", "error", err)
		reason = ""
	}
	d.setDispatchPaused(true, PauseReason(reason), "")
	at, ok, err := d.db.GetSetting(state.SettingDispatchPausedAt)
	if err != nil {
		d.logger.Warn("failed to read dispatch pause timestamp", "error", err)
		d.pausedSince.Store(time.Time{})
	} else if ok && at != "" {
		if t, perr := time.Parse(time.RFC3339, at); perr == nil {
			d.pausedSince.Store(t)
		} else {
			d.pausedSince.Store(time.Time{})
		}
	} else {
		d.pausedSince.Store(time.Time{})
	}
	if err := d.db.LogEvent(state.EventDispatchPaused, "Dispatch pause restored from previous run", "", ""); err != nil {
		d.logger.Warn("failed to log dispatch-pause-restored event", "error", err)
	}
	d.logger.Info("dispatch pause restored from previous run")
}

// runAssayReview performs one Assay review of the PR's current head: it fetches
// the diff, runs the multi-pass engine (writing findings to pr_findings), and —
// when the anvil's resolved shadow_mode is false — posts the findings to the PR.
// It records an assay_runs row and returns it along with any record error.
// Worker-row lifecycle is the caller's responsibility, so this is reusable from
// both the ActionAssayReview dispatch and the Burnish pre-fetch coordination
// step. headSHA is recorded on the run and used as the inline-comment anchor.
// assayLogPathRecorder returns the ReviewRequest.OnPassLog callback that points
// the Assay worker row at the first pass log to be spawned (triage, which
// always runs before the concurrent deep passes). Assay previously recorded no
// log path at all, so its Hearth panel sat on "Waiting for log output…" for the
// entire run and read as a missing worker.
//
// sync.Once keeps this to the first pass: the deep passes fan out concurrently,
// so without it the panel would flip between logs mid-run. A nil return (empty
// workerID) disables recording — used on the Burnish-coordination path, where
// the run piggybacks on the Burnish worker and owns no row of its own.
func (d *Daemon) assayLogPathRecorder(workerID string) func(string) {
	if workerID == "" {
		return nil
	}
	var once sync.Once
	return func(logPath string) {
		once.Do(func() {
			if err := d.db.UpdateWorkerLogPath(workerID, logPath); err != nil {
				d.logger.Warn("failed to record Assay worker log path", "worker", workerID, "error", err)
			}
		})
	}
}

// fetchAssayDiff returns the net base..head diff Assay reviews, via
// `gh pr diff <N>` run in the PR's worktree. gh's stderr is logged here rather
// than folded into the returned error: the error is what lands in the run
// record and the feed row, and both are one bounded line.
//
// d.assayDiffFetch replaces it in tests — see the field's doc.
func (d *Daemon) fetchAssayDiff(ctx context.Context, worktreePath string, prNumber int) ([]byte, error) {
	if d.assayDiffFetch != nil {
		return d.assayDiffFetch(ctx, worktreePath, prNumber)
	}
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", "pr", "diff", strconv.Itoa(prNumber)))
	cmd.Dir = worktreePath
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && stderr.Len() > 0 {
		d.logger.Error("gh pr diff failed for Assay", "pr", prNumber, "stderr", stderr.String())
	}
	return out, err
}

// fetchAssayDeltaDiff returns the incremental diff sinceSHA..headSHA for a
// repeat Assay review, via git in the PR's worktree. It first proves sinceSHA
// is an ancestor of headSHA — after a force-push or rebase the last reviewed
// commit is not, and the only honest scope is the full diff again. Any error
// (unknown object, detached history, git missing) is returned for the caller
// to log and fall back on; nothing here is fatal to the run.
//
// d.assayDeltaFetch replaces it in tests — see the field's doc.
func (d *Daemon) fetchAssayDeltaDiff(ctx context.Context, worktreePath, sinceSHA, headSHA string) ([]byte, error) {
	if d.assayDeltaFetch != nil {
		return d.assayDeltaFetch(ctx, worktreePath, sinceSHA, headSHA)
	}
	check := executil.HideWindow(exec.CommandContext(ctx, "git", "-C", worktreePath, "merge-base", "--is-ancestor", sinceSHA, headSHA))
	check.Env = executil.CleanGitEnv()
	if err := check.Run(); err != nil {
		return nil, fmt.Errorf("last reviewed commit %s is not an ancestor of head %s: %w", sinceSHA, headSHA, err)
	}
	cmd := executil.HideWindow(exec.CommandContext(ctx, "git", "-C", worktreePath, "diff", sinceSHA, headSHA))
	cmd.Env = executil.CleanGitEnv()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if stderr.Len() > 0 {
			d.logger.Warn("git diff failed for incremental Assay", "since", sinceSHA, "head", headSHA, "stderr", stderr.String())
		}
		return nil, err
	}
	return out, nil
}

// assayIncrementalScope decides the diff a repeat review actually reads. Given
// the full net base..head diff, it looks up the last successfully reviewed
// commit and, when incremental review applies, returns the delta since that
// commit scoped to files that survive into the net diff (dropping upstream
// merge churn and reverted files). Returns the diff to review, whether it is
// incremental, the baseline SHA, and whether there is anything to review at
// all — a repeat push whose scoped delta is empty changed nothing reviewable,
// and running the passes over the full diff again is exactly the duplicate
// spam this scope exists to prevent.
func (d *Daemon) assayIncrementalScope(ctx context.Context, anvil, worktreePath, headSHA string, prNumber int, fullDiff []byte) (reviewDiff string, incremental bool, baseline string, reviewable bool) {
	reviewDiff = string(fullDiff)
	reviewable = true
	if !d.cfg.Load().ResolvedAssay(anvil).IsIncremental() {
		return
	}
	since, err := d.db.LastReviewedSHA(anvil, prNumber)
	if err != nil || since == "" || since == headSHA {
		return
	}
	delta, derr := d.fetchAssayDeltaDiff(ctx, worktreePath, since, headSHA)
	if derr != nil {
		d.logger.Info("incremental Assay unavailable; reviewing full diff", "pr", prNumber, "anvil", anvil, "since", since, "error", derr)
		return
	}
	scoped := diff.KeepFiles(string(delta), diff.ChangedFiles(string(fullDiff)))
	if strings.TrimSpace(scoped) == "" {
		return "", true, since, false
	}
	return scoped, true, since, true
}

// reviewAssay runs the multi-pass engine over req. d.assayReview replaces it in
// tests — see the field's doc.
func (d *Daemon) reviewAssay(ctx context.Context, req assay.ReviewRequest, db *state.DB, cfg assay.Config) (*assay.ReviewResult, error) {
	if d.assayReview != nil {
		return d.assayReview(ctx, req, db, cfg)
	}
	return assay.Review(ctx, req, db, cfg)
}

func (d *Daemon) runAssayReview(ctx context.Context, anvil, anvilPath, beadID string, prNumber int, headSHA, worktreePath, workerID string) (*state.AssayRun, error) {
	resolved := d.cfg.Load().ResolvedAssay(anvil)
	engineCfg := assay.FromAssayConfig(resolved)

	started := time.Now()
	run := &state.AssayRun{
		Anvil:      anvil,
		PRNumber:   prNumber,
		HeadSHA:    headSHA,
		StartedAt:  started,
		ShadowMode: engineCfg.ShadowMode,
	}

	// Fetch the PR diff — the full net diff base..head, i.e. the cumulative
	// change across every commit. On a repeat review, the diff the passes
	// actually read is then narrowed to the changes since the last reviewed
	// commit (assayIncrementalScope); the full diff is still what the posting
	// layer anchors inline comments against, since GitHub only accepts
	// positions present in the net diff.
	diffBytes, diffErr := d.fetchAssayDiff(ctx, worktreePath, prNumber)
	var reviewDiff, baselineSHA string
	incremental, reviewable := false, false
	if diffErr == nil {
		reviewDiff, incremental, baselineSHA, reviewable = d.assayIncrementalScope(ctx, anvil, worktreePath, headSHA, prNumber, diffBytes)
	}
	if diffErr != nil {
		d.logger.Error("failed to fetch PR diff for Assay", "pr", prNumber, "bead", beadID, "error", diffErr)
		run.SkippedReason = "diff fetch failed"
		run.Error = diffErr.Error()
		run.Status = state.AssayStatusFailed
	} else if !reviewable {
		// The head moved but nothing reviewable changed since the last review
		// (upstream merge, revert). Running the passes over the full diff
		// again would only regenerate comments about already-reviewed code.
		// The run is recorded as skipped-complete: the head counts as
		// reviewed, and a skipped run never consumes the per-PR run cap.
		d.logger.Info("Assay skipped: no reviewable changes since last review",
			"pr", prNumber, "bead", beadID, "head", headSHA, "since", baselineSHA)
		run.SkippedReason = "no reviewable changes since last review"
		run.Status = state.AssayStatusComplete
	} else {
		// Point the worker row at the first pass log to be spawned (triage,
		// which always runs before the concurrent deep passes) so the Hearth
		// live panel has something to stream. Assay previously recorded no
		// log path at all, so its panel sat on "Waiting for log output…" for
		// the entire run and read as a missing worker. sync.Once keeps this to
		// the first pass: the deep passes fan out concurrently, so without it
		// the panel would flip between logs mid-run.
		result, rerr := d.reviewAssay(ctx, assay.ReviewRequest{
			Anvil:       anvil,
			AnvilPath:   anvilPath,
			PRNumber:    prNumber,
			HeadSHA:     headSHA,
			Diff:        reviewDiff,
			Incremental: incremental,
			BaselineSHA: baselineSHA,
			BeadID:      beadID,
			Title:       d.db.BeadTitle(beadID, anvil),
			WorkDir:     worktreePath,
			OnPassLog:   d.assayLogPathRecorder(workerID),
		}, d.db, engineCfg)
		if rerr != nil {
			// A failed run is still a billed run: the sessions it made before
			// it died are charged, so the cost the error carries is recorded
			// exactly like a successful run's. Dropping it would understate
			// the assay spend that daily_cost_limit is measured against.
			run.CostUSD = assay.RunCost(rerr)
			d.logger.Error("Assay review failed", "pr", prNumber, "bead", beadID, "error", rerr, "cost_usd", run.CostUSD)
			run.Error = rerr.Error()
			run.Status = state.AssayStatusFailed
		} else {
			run.CostUSD = result.CostUSD
			run.FindingsCount = len(result.Findings)
			// Coverage is recorded on the run, not re-derived: the status,
			// the pass tally and the named failed passes all come from the
			// engine's single computation, so the worker row's status chip,
			// this log line, the terminal feed event (completed/partial/failed,
			// rendered from this record below), the PR findings panel and the
			// PR summary comment cannot disagree.
			run.Status = stateAssayStatus(result.Status)
			run.CompletedPasses = result.CompletedPasses
			run.TotalPasses = result.TotalPasses
			run.FailedPasses = statePassFailures(result.FailedPasses)
			statusText := result.StatusText()
			if len(result.PassErrors) > 0 {
				for _, pe := range result.PassErrors {
					d.logger.Warn("Assay pass error (partial)", "pr", prNumber, "bead", beadID, "error", pe)
				}
				run.Error = strings.Join(result.PassErrors, "; ")
			}
			if result.Status == assay.RunStatusPartial {
				d.logger.Warn("Assay review partial", "pr", prNumber, "bead", beadID, "head", headSHA, "status", statusText)
			}
			// The per-pass telemetry rides along as its own field: turn
			// counts and termination reasons are what the assay turn budget
			// has to be tuned against, and a budget guessed at without them
			// is what produced max-turns failures on nine-line diffs.
			d.logger.Info("Assay review completed",
				"pr", prNumber, "bead", beadID, "head", headSHA,
				"findings", run.FindingsCount, "pass_errors", len(result.PassErrors),
				"status", statusText,
				"passes", result.PassTelemetryText(),
				"incremental", incremental,
				"total_capped", result.TotalCapped,
				"shadow", engineCfg.ShadowMode, "cost_usd", run.CostUSD,
				"duration_ms", result.Duration.Milliseconds(),
			)

			// Live posting. Post() self-guards on shadow mode, so this only
			// produces public side effects on anvils whose resolved shadow_mode
			// is false. The resolver is the GitHub provider when it satisfies
			// ThreadResolver; other providers fall back to nil (posts comments,
			// skips thread auto-resolution).
			if !engineCfg.ShadowMode {
				var resolver assay.ThreadResolver
				if tr, ok := d.vcsForAnvil(anvil).(assay.ThreadResolver); ok {
					resolver = tr
				}
				postRes, perr := assay.NewPoster(d.db, resolver).Post(ctx, engineCfg, assay.PostRequest{
					Anvil:        anvil,
					PRNumber:     prNumber,
					HeadSHA:      headSHA,
					WorktreePath: worktreePath,
					SummaryLine:  assaySummaryLine(result.Findings),
					FailedPasses: result.FailedPasses,
					Findings:     result.Findings,
					Diff:         string(diffBytes),
				})
				if perr != nil {
					d.logger.Error("Assay posting failed", "pr", prNumber, "bead", beadID, "error", perr)
				} else if postRes != nil {
					run.PostedCount = postRes.Posted
					d.logger.Info("Assay findings posted",
						"pr", prNumber, "bead", beadID,
						"posted", postRes.Posted, "failed", postRes.Failed,
						"resolved", postRes.Resolved, "summary", postRes.SummaryPosted,
						"out_of_diff", postRes.OutOfDiff,
					)
				}
			}
		}
	}

	finished := time.Now()
	run.FinishedAt = &finished
	run.DurationMs = finished.Sub(started).Milliseconds()
	recErr := d.db.RecordAssayRun(run)
	if recErr != nil {
		d.logger.Error("failed to record Assay run", "pr", prNumber, "bead", beadID, "error", recErr)
	}
	// One terminal event per run, from the one place a run ends: this is what
	// closes in the activity feed the pr_review_needed that opened the review.
	// It is emitted even when recording the run failed — the review happened,
	// and a lost row in assay_runs is no reason to also lose the feed's only
	// notice that it did.
	d.emitAssayTerminalEvent(run, beadID)
	return run, recErr
}

// ensureAssayReviewedHead, for an Assay-live anvil, runs an Assay review of the
// PR's current head BEFORE Burnish fetches the review-comment set, so a single
// Burnish pass addresses both Copilot and Assay findings instead of triggering
// two separate fix cycles. It is a no-op when Assay is disabled or in shadow
// mode (nothing is posted, so there is nothing to coordinate) or when the
// current head has already been reviewed. Failures are logged and swallowed —
// coordination is best-effort and must never block the review-fix itself.
func (d *Daemon) ensureAssayReviewedHead(ctx context.Context, anvil, anvilPath, beadID string, prNumber int, worktreePath string) {
	resolved := d.cfg.Load().ResolvedAssay(anvil)
	if !resolved.IsEnabled() || resolved.IsShadowMode() {
		return
	}
	st, err := d.vcsForAnvil(anvil).CheckStatusLight(ctx, anvilPath, prNumber)
	if err != nil || st == nil || st.HeadSHA == "" {
		if err != nil {
			d.logger.Warn("Burnish/Assay coordination: head SHA lookup failed; fixing without Assay sync", "pr", prNumber, "anvil", anvil, "error", err)
		}
		return
	}
	if last, lerr := d.db.LastReviewedSHA(anvil, prNumber); lerr == nil && last == st.HeadSHA {
		return // current head already reviewed; its comments (if any) are posted
	}
	d.logger.Info("Burnish/Assay coordination: running Assay before fix so both reviews land in one pass", "pr", prNumber, "anvil", anvil, "head", st.HeadSHA)
	// No dedicated Assay worker row on this path — the run piggybacks on the
	// Burnish worker — so there is nothing to point at a log file.
	_, _ = d.runAssayReview(ctx, anvil, anvilPath, beadID, prNumber, st.HeadSHA, worktreePath, "")
}

// dispatchLifecycleAction runs a lifecycle action on its own goroutine, through
// the lifecycleDispatch seam so tests can observe the request instead of
// spawning a worker.
func (d *Daemon) dispatchLifecycleAction(req lifecycle.ActionRequest) {
	fn := d.lifecycleDispatch
	if fn == nil {
		fn = d.handleLifecycleAction
	}
	go fn(d.runCtx, req)
}

// handleLifecycleAction handles PR-triggered fixes from Bellows.
func (d *Daemon) handleLifecycleAction(ctx context.Context, req lifecycle.ActionRequest) {
	d.logger.Info("lifecycle action requested", "action", req.Action, "pr", req.PRNumber, "bead", req.BeadID)

	// If there's no bead ID, fall back to the PR number as the worktree/lock
	// key (e.g. warden-learn PRs). Both Bellows-triggered and manual actions
	// are allowed through — the pr-{N} key prevents the .workers dir corruption
	// that the original Forge-6sed guard was protecting against. Only bail out
	// if we have neither a bead ID nor a PR number to derive a key from.
	if req.BeadID == "" && req.PRNumber == 0 {
		d.logger.Info("skipping lifecycle action: no bead ID or PR number", "action", req.Action, "branch", req.Branch)
		return
	}

	anvilCfg, ok := d.cfg.Load().Anvils[req.Anvil]
	if !ok {
		d.logger.Error("unknown anvil in lifecycle action", "anvil", req.Anvil)
		return
	}

	// Use the bead ID as the in-flight lock key; fall back to "pr-{N}" for
	// manual actions on non-bead PRs (e.g. warden-learn PRs triggered by user).
	lockKey := req.BeadID
	if lockKey == "" {
		lockKey = fmt.Sprintf("pr-%d", req.PRNumber)
	}

	// If bead is already in flight, park the action for after it finishes.
	if _, inFlight := d.activeBeads.LoadOrStore(lockKey, true); inFlight {
		// Assay is a best-effort, shadow-mode review pass. Do NOT park it:
		// pendingActions is a single slot per bead (latest-wins), so a parked
		// Assay review would overwrite a parked review-fix/rebase for the same
		// bead — silently dropping the action that actually fixes the PR and
		// stranding it in needs_fix (observed: PR #3545 / Fhi.Metadata-4r1gr,
		// 2026-06-04, where two Assay reviews clobbered the burnish). Skipping is
		// safe: Assay re-fires whenever the PR head changes, so it reviews the
		// fixed head once the more important work completes.
		if req.Action == lifecycle.ActionAssayReview {
			d.logger.Info("bead in flight, skipping best-effort Assay review (re-fires on next head)", "bead", req.BeadID, "pr", req.PRNumber)
			return
		}
		// Mutating actions (review-fix, CI-fix, rebase, ...) park per action type
		// so distinct fix kinds for the same bead coexist instead of clobbering
		// (latest-wins only within a single action type).
		d.parkPendingAction(lockKey, req)
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
				d.drainPendingAction(ctx, lockKey)
			}
		}()
		defer d.activeBeads.Delete(lockKey)

		// Actions that don't need a worktree — handle before worktree creation
		// so they succeed even when the branch/worktree is already cleaned up.
		switch req.Action {
		case lifecycle.ActionCloseBead:
			d.logger.Info("closing bead after PR merge", "bead", req.BeadID)
			d.clearReviewFixDispatch(req)
			if err := d.closeBead(ctx, req.BeadID, anvilCfg.Path, "PR merged"); err != nil {
				d.logger.Warn("failed to close bead after PR merge", "bead", req.BeadID, "error", err)
			}
			return

		case lifecycle.ActionCleanup:
			d.logger.Info("cleaning up PR after close", "pr", req.PRNumber)
			d.clearReviewFixDispatch(req)
			// Optional: delete remote branch etc.
			return
		}

		// Gate the expensive fix workers (quench/burnish/rebase) behind a global
		// concurrency cap. Each spawns a Claude session of comparable length to a
		// Smith, yet they are deliberately excluded from the max_total_smiths
		// dispatch cap (see state.ActiveDispatchWorkers). Without their own ceiling
		// a burst of stuck PRs fans out unbounded Claude sessions and OOM-crashed
		// the host (Forge-3m06). The light-weight no-worktree actions
		// (CloseBead/Cleanup) already returned above and are intentionally not gated.
		//
		// Assay is intentionally NOT gated: it is a fast, best-effort review pass
		// that dedupes per PR head (so it cannot fan out unbounded for a single
		// PR), and we want reviews to start immediately rather than queue behind a
		// long-running Smith/Burnish session holding the only lifecycle slot.
		//
		// We block until a slot becomes available (or ctx is cancelled) so the
		// originally-dispatched attempt runs and per-PR retry counters reflect
		// real work, rather than deferring and re-dispatching which can burn
		// through attempt limits without ever running a fix worker.
		if req.Action != lifecycle.ActionAssayReview {
			maxLifecycle := d.cfg.Load().Settings.MaxLifecycleWorkers
			if !d.waitForLifecycleSlot(ctx, maxLifecycle) {
				d.logger.Warn("lifecycle slot wait cancelled",
					"action", req.Action,
					"pr", req.PRNumber,
					"bead", req.BeadID,
				)
				return
			}
			defer d.releaseLifecycleSlot()
		}

		// Reserve estimated in-flight spend for this fix worker (quench/burnish/
		// rebase/assay) so its cost counts against the daily_cost_limit gate.
		// Unlike Smith dispatch these workers are NOT blocked by the gate — they
		// address already-open PRs and blocking them would strand work — but
		// reserving here makes the poll-loop gate back off *new* Smith dispatch
		// while lifecycle spend is in flight, and the reservation is reconciled
		// when this worker returns (Forge-s3w7).
		lifecycleReservation := d.reserveWorkerCost(d.perWorkerCostEstimate(d.cfg.Load()))
		defer d.releaseWorkerCost(lifecycleReservation)

		// Create/get worktree for the PR branch. Use lockKey (which may be
		// "pr-{N}" for non-bead PRs) so the path is always non-empty/valid.
		wt, err := d.worktreeMgr.Create(ctx, anvilCfg.Path, lockKey, req.Branch)
		if err != nil {
			d.logger.Error("failed to create worktree for lifecycle fix", "error", err)
			// Reset lifecycle and bellows state so the next Bellows poll can
			// re-emit the appropriate event and retry. Without this, CINeedsFix /
			// ReviewNeedsFix / Conflicting stay set after a transient worktree
			// failure (e.g. git fetch error) and the still-failing/still-unresolved/
			// still-conflicting re-emit branches are permanently suppressed.
			switch req.Action {
			case lifecycle.ActionRebase:
				d.lifecycleMgr.NotifyRebaseCompleted(req.Anvil, req.PRNumber)
			case lifecycle.ActionFixCI:
				d.lifecycleMgr.NotifyCIFixCompleted(req.Anvil, req.PRNumber)
			case lifecycle.ActionFixReview:
				d.lifecycleMgr.NotifyReviewFixCompleted(req.Anvil, req.PRNumber)
			}
			if d.bellowsMonitor != nil {
				d.bellowsMonitor.ResetPRState(req.Anvil, req.PRNumber)
			}
			return
		}
		// Preserve this worker's claude logs before the worktree goes away,
		// mirroring the pipeline's teardown. Without this the lifecycle
		// stages (quench/burnish/rebase/assay) left their worker rows
		// pointing into a deleted worktree, so every historical log was
		// unreadable in the web UI and the per-bead Logs browser was empty
		// for them.
		defer func() {
			oldLogDir := filepath.Join(wt.Path, ".forge-logs")
			dstDir, perr := pipeline.PreserveWorktreeLogs(wt.Path, req.BeadID)
			if perr != nil {
				d.logger.Warn("failed to preserve lifecycle logs", "bead", req.BeadID, "error", perr)
			} else if dstDir != "" {
				if n, rerr := d.db.RepointWorkerLogPaths(req.BeadID, oldLogDir, dstDir); rerr != nil {
					d.logger.Warn("failed to repoint lifecycle log paths", "bead", req.BeadID, "error", rerr)
				} else if n > 0 {
					d.logger.Info("preserved lifecycle logs", "bead", req.BeadID, "workers", n, "dir", dstDir)
				}
			}
			d.removeLifecycleWorktree(ctx, req, anvilCfg.Path, wt)
		}()

		workerID := fmt.Sprintf("%s-%s-%d", req.Anvil, req.BeadID, time.Now().UnixNano())

		// Derive a timeout context for the lifecycle worker so it cannot hang
		// indefinitely. Use SmithTimeout as the budget since these workers (quench,
		// burnish, rebase) each spawn a Claude session of comparable length.
		// Mirror the pipeline defaulting: treat SmithTimeout <= 0 as "use 30m"
		// so lifecycle workers always have a deadline unless explicitly configured.
		workerTimeout := d.cfg.Load().Settings.SmithTimeout
		if workerTimeout <= 0 {
			workerTimeout = 30 * time.Minute
		}
		workerCtx, workerCancel := context.WithTimeout(ctx, workerTimeout)
		defer func() {
			// Always cancel the worker context, and log explicitly if the worker
			// exceeded its deadline so timeout-triggered failures are diagnosable.
			workerCancel()
			if workerCtx.Err() == context.DeadlineExceeded {
				d.logger.Error("lifecycle worker timed out",
					"action", req.Action,
					"pr", req.PRNumber,
					"bead", req.BeadID,
					"worker", workerID,
				)
			}
		}()

		switch req.Action {
		case lifecycle.ActionFixCI:
			d.logger.Info("spawning CI fix worker", "pr", req.PRNumber, "bead", req.BeadID)
			_ = d.db.InsertWorker(&state.Worker{
				ID:           workerID,
				BeadID:       req.BeadID,
				Anvil:        req.Anvil,
				Branch:       req.Branch,
				Status:       state.WorkerRunning,
				Phase:        "quench",
				Title:        d.db.BeadTitle(req.BeadID, req.Anvil),
				PRNumber:     req.PRNumber,
				StartedAt:    time.Now(),
				StaleTimeout: workerTimeout / 2,
			})
			cfg := d.config()
			quenchProviders := d.filterCopilotIfLimited(provider.FromConfig(config.ProvidersForStageWithAnvil(cfg.Settings, &anvilCfg, "cifix")))
			// Use batch mode when copilot_batch_ci_fixes is enabled and the primary provider is Copilot.
			var res *quench.FixResult
			useBatch := cfg.Settings.CopilotBatchCIFixes && len(quenchProviders) > 0 && quenchProviders[0].Kind == provider.Copilot
			if useBatch {
				anvilVCS := d.vcsForAnvil(req.Anvil)
				_, failingChecks, fetchErr := anvilVCS.FetchPRChecks(workerCtx, wt.Path, req.PRNumber)
				if fetchErr != nil {
					d.logger.Warn("failed to fetch PR checks for batch CI fix, falling back to single fix", "pr", req.PRNumber, "error", fetchErr)
					useBatch = false
				} else {
					ciLogs, logsErr := anvilVCS.FetchCILogs(workerCtx, wt.Path, failingChecks)
					if logsErr != nil {
						d.logger.Warn("failed to fetch CI logs for batch CI fix, falling back to single fix", "pr", req.PRNumber, "error", logsErr)
						useBatch = false
					}
					if useBatch {
						res = quench.BatchFix(workerCtx, quench.BatchFixParams{
							WorktreePath:  wt.Path,
							BeadID:        req.BeadID,
							AnvilName:     req.Anvil,
							PRNumber:      req.PRNumber,
							Branch:        req.Branch,
							DB:            d.db,
							WorkerID:      workerID,
							ExtraFlags:    cfg.Settings.ClaudeFlags,
							Providers:     quenchProviders,
							FailingChecks: failingChecks,
							CILogs:        ciLogs,
						})
					}
				}
			}
			if !useBatch {
				quenchDetectOpts := temper.DetectOptionsFromAnvilFlag(anvilCfg.GolangciLint)
				res = quench.Fix(workerCtx, quench.FixParams{
					WorktreePath:    wt.Path,
					BeadID:          req.BeadID,
					AnvilName:       req.Anvil,
					AnvilPath:       anvilCfg.Path,
					PRNumber:        req.PRNumber,
					Branch:          req.Branch,
					DB:              d.db,
					WorkerID:        workerID,
					ExtraFlags:      cfg.Settings.ClaudeFlags,
					TemperConfig:    d.resolveTemperConfig(anvilCfg),
					DetectOptions:   quenchDetectOpts,
					GoRaceDetection: d.resolveGoRaceDetection(anvilCfg),
					Providers:       quenchProviders,
					VCS:             d.vcsForAnvil(req.Anvil),
					LearnConfig:     d.quenchLearnConfig(req.Anvil),
					Hooks:           anvilCfg.Hooks,
				})
			}
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
				// checks are still running, burning through quench retries before
				// CI has a chance to complete. The regular poll interval is
				// sufficient to re-detect failure once CI settles.
				d.bellowsMonitor.ResetPRState(req.Anvil, req.PRNumber)
			}

		case lifecycle.ActionFixReview:
			// Refuse a dispatch that would rebuild work an unchanged PR head
			// already carries. ReviewFixCnt bounds review fixes for the PR's
			// whole life and never resets, so it neither stops a
			// non-converging loop early nor lets a genuinely progressing PR
			// keep going; the head SHA is the signal that tells the two apart.
			if !d.reviewFixDispatchAllowed(workerCtx, req, anvilCfg.Path) {
				d.lifecycleMgr.NotifyReviewFixCompleted(req.Anvil, req.PRNumber)
				if d.bellowsMonitor != nil {
					d.bellowsMonitor.ResetPRState(req.Anvil, req.PRNumber)
				}
				return
			}
			d.logger.Info("spawning review fix worker", "pr", req.PRNumber, "bead", req.BeadID)
			// Coordinate with Assay before fetching the comment set: on a live
			// Assay anvil, ensure Assay has reviewed (and posted on) the current
			// head so this single Burnish pass addresses both Copilot and Assay
			// findings rather than running once for Copilot now and again for
			// Assay later. No-op for shadow/disabled anvils or an already-reviewed
			// head; best-effort, never blocks the fix.
			d.ensureAssayReviewedHead(workerCtx, req.Anvil, anvilCfg.Path, req.BeadID, req.PRNumber, wt.Path)
			_ = d.db.InsertWorker(&state.Worker{
				ID:           workerID,
				BeadID:       req.BeadID,
				Anvil:        req.Anvil,
				Branch:       req.Branch,
				Status:       state.WorkerRunning,
				Phase:        "burnish",
				Title:        d.db.BeadTitle(req.BeadID, req.Anvil),
				PRNumber:     req.PRNumber,
				StartedAt:    time.Now(),
				StaleTimeout: workerTimeout / 2,
			})
			burnishCfg := d.cfg.Load()
			burnishProviders := d.filterCopilotIfLimited(provider.FromConfig(config.ProvidersForStageWithAnvil(burnishCfg.Settings, &anvilCfg, "reviewfix")))
			// Use batch mode when copilot_batch_review_fixes is enabled and the primary provider is Copilot.
			var res *burnish.FixResult
			useBurnishBatch := burnishCfg.Settings.CopilotBatchReviewFixes && len(burnishProviders) > 0 && burnishProviders[0].Kind == provider.Copilot
			if useBurnishBatch {
				anvilVCS := d.vcsForAnvil(req.Anvil)
				comments, fetchErr := anvilVCS.FetchReviewComments(workerCtx, wt.Path, req.PRNumber)
				if fetchErr != nil {
					d.logger.Warn("failed to fetch review comments for batch fix, falling back to single fix", "pr", req.PRNumber, "error", fetchErr)
					useBurnishBatch = false
				} else {
					burnishDetectOpts := temper.DetectOptionsFromAnvilFlag(anvilCfg.GolangciLint)
					res = burnish.BatchFix(workerCtx, burnish.BatchFixParams{
						WorktreePath:    wt.Path,
						BeadID:          req.BeadID,
						AnvilName:       req.Anvil,
						AnvilPath:       anvilCfg.Path,
						PRNumber:        req.PRNumber,
						Branch:          req.Branch,
						DB:              d.db,
						WorkerID:        workerID,
						ExtraFlags:      burnishCfg.Settings.ClaudeFlags,
						Providers:       burnishProviders,
						Comments:        comments,
						VCS:             anvilVCS,
						TemperConfig:    d.resolveTemperConfig(anvilCfg),
						DetectOptions:   burnishDetectOpts,
						GoRaceDetection: d.resolveGoRaceDetection(anvilCfg),
						Hooks:           anvilCfg.Hooks,
						VerifyTimeout:   burnishCfg.Settings.BurnishVerifyTimeout,
						VerifyRetries:   burnishCfg.Settings.BurnishVerifyRetries,
					})
				}
			}
			if !useBurnishBatch {
				burnishDetectOpts := temper.DetectOptionsFromAnvilFlag(anvilCfg.GolangciLint)
				res = burnish.Fix(workerCtx, burnish.FixParams{
					WorktreePath:    wt.Path,
					BeadID:          req.BeadID,
					AnvilName:       req.Anvil,
					AnvilPath:       anvilCfg.Path,
					PRNumber:        req.PRNumber,
					Branch:          req.Branch,
					DB:              d.db,
					WorkerID:        workerID,
					MaxAttempts:     burnishCfg.Settings.MaxReviewAttempts,
					ExtraFlags:      burnishCfg.Settings.ClaudeFlags,
					Providers:       burnishProviders,
					VCS:             d.vcsForAnvil(req.Anvil),
					TemperConfig:    d.resolveTemperConfig(anvilCfg),
					DetectOptions:   burnishDetectOpts,
					GoRaceDetection: d.resolveGoRaceDetection(anvilCfg),
					Hooks:           anvilCfg.Hooks,
					VerifyTimeout:   burnishCfg.Settings.BurnishVerifyTimeout,
					VerifyRetries:   burnishCfg.Settings.BurnishVerifyRetries,
				})
			}
			status := state.WorkerDone
			if res.Error != nil {
				status = state.WorkerFailed
			}
			_ = d.db.UpdateWorkerStatus(workerID, status)
			d.recordReviewFixOutcome(req, res)
			// Always notify lifecycle the review-fix cycle has finished and
			// clear the bellows snapshot, regardless of outcome. The earlier
			// guard left both signals untouched on failure, hoping bellows
			// would redispatch from the still-set NeedsFix state — but
			// bellows tracks state in two places (the in-memory lifecycle
			// state and the snapshot cache), and the next 2m poll sees no
			// transition (NeedsFix false→false from its perspective), so
			// no fresh EventReviewChangesRequested is ever emitted and the
			// PR sits stuck. Mirroring the CI-fix path keeps the retry
			// loop alive; runaway is prevented by review_fix_count / the
			// MaxReviewFixAttempts cap, not by withholding reset.
			d.lifecycleMgr.NotifyReviewFixCompleted(req.Anvil, req.PRNumber)
			if d.bellowsMonitor != nil {
				d.bellowsMonitor.ResetPRState(req.Anvil, req.PRNumber)
			}

		case lifecycle.ActionRebase:
			d.logger.Info("rebasing conflicting PR", "pr", req.PRNumber, "bead", req.BeadID)
			_ = d.db.InsertWorker(&state.Worker{
				ID:           workerID,
				BeadID:       req.BeadID,
				Anvil:        req.Anvil,
				Branch:       req.Branch,
				Status:       state.WorkerRunning,
				Phase:        "rebase",
				Title:        d.db.BeadTitle(req.BeadID, req.Anvil),
				PRNumber:     req.PRNumber,
				StartedAt:    time.Now(),
				StaleTimeout: workerTimeout / 2,
			})
			res := rebase.Rebase(workerCtx, rebase.Params{
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
			// Always notify lifecycle that the rebase cycle has completed so it
			// resets pr.Status to open and allows Bellows to re-emit
			// EventPRConflicting on the next poll if the PR is still conflicting.
			// Without this the still-conflicting branch is permanently suppressed
			// by its pr.Status != needs_fix guard. Mirror the CI-fix and
			// review-fix paths.
			d.lifecycleMgr.NotifyRebaseCompleted(req.Anvil, req.PRNumber)
			// Reset the snapshot cache so Bellows re-seeds from the DB on
			// the next poll. Without this, the cached snapshot would still
			// have IsConflicting=true and the seeding guard would preserve
			// that value, preventing the still-conflicting branch from
			// re-emitting EventPRConflicting after the status is reset.
			if d.bellowsMonitor != nil {
				d.bellowsMonitor.ResetPRState(req.Anvil, req.PRNumber)
			}

		case lifecycle.ActionAssayReview:
			d.logger.Info("spawning Assay review worker", "pr", req.PRNumber, "bead", req.BeadID, "head", req.HeadSHA)
			// Insert the worker row FIRST. Per the bead 05/15 lesson, the
			// assay-run "counter" (RecordAssayRun) must only advance after the
			// worker row is successfully inserted — otherwise a failed insert
			// would still mark the head as reviewed and suppress future runs.
			if err := d.db.InsertWorker(&state.Worker{
				ID:           workerID,
				BeadID:       req.BeadID,
				Anvil:        req.Anvil,
				Branch:       req.Branch,
				Status:       state.WorkerRunning,
				Phase:        "assay",
				Title:        d.db.BeadTitle(req.BeadID, req.Anvil),
				PRNumber:     req.PRNumber,
				StartedAt:    time.Now(),
				StaleTimeout: workerTimeout / 2,
			}); err != nil {
				d.logger.Error("failed to insert Assay worker row; skipping run", "pr", req.PRNumber, "bead", req.BeadID, "error", err)
				// Do not record a run: reset the snapshot so the next poll can
				// re-emit EventPRReviewNeeded and retry the dispatch.
				if d.bellowsMonitor != nil {
					d.bellowsMonitor.ResetPRState(req.Anvil, req.PRNumber)
				}
				break
			}

			run, recErr := d.runAssayReview(workerCtx, req.Anvil, anvilCfg.Path, req.BeadID, req.PRNumber, req.HeadSHA, wt.Path, workerID)
			_ = d.db.UpdateWorkerStatus(workerID, assayWorkerStatus(run, recErr))
			// Reset the snapshot so subsequent head pushes are re-detected by
			// the gate on the next poll.
			if d.bellowsMonitor != nil {
				d.bellowsMonitor.ResetPRState(req.Anvil, req.PRNumber)
			}
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
	// Use a detached context with timeout — the bellows polling ctx may be
	// cancelled before the notification HTTP calls complete.
	notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	go func(anvil, beadID string, prNumber int, prURL, title string) {
		defer notifyCancel()
		if n := d.notifier.Load(); n != nil {
			n.PRReadyToMerge(notifyCtx, anvil, beadID, prNumber, prURL, title)
		}
		// Dispatch to generic webhook targets (new webhooks[] config).
		if disp != nil {
			msg := fmt.Sprintf("PR #%d ready to merge — %s (%s, %s) — %s", prNumber, title, beadID, anvil, prURL)
			if title == "" {
				msg = fmt.Sprintf("PR #%d ready to merge (%s, %s) — %s", prNumber, beadID, anvil, prURL)
			}
			disp.Dispatch(notifyCtx, notify.EventPRReadyToMerge, beadID, anvil, msg)
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
				notify.SendGenericPRReadyToMerge(notifyCtx, u, payload, d.logger)
			}
		}
	}(event.Anvil, event.BeadID, event.PRNumber, event.PRURL, title)
}

// handleBeadCloseOnMerge closes the bead when its PR is merged.
// The pipeline always defers bead close until the PR merges, expecting
// bellows to close it. This handler fulfils that contract.
// External PRs (ext-*) are skipped — they don't have real beads to close.
//
// The close runs on its own goroutine with a bounded retry burst: bellows
// invokes handlers synchronously, and a transient dolt/beads failure here is
// not cosmetic — the bead stays open, so every dependent bead behind it stays
// blocked even though the work merged. Anything the burst cannot fix is
// persisted as a pending close and re-attempted on later bellows cycles by
// reconcilePendingBeadCloses.
func (d *Daemon) handleBeadCloseOnMerge(ctx context.Context, event bellows.PREvent) {
	if event.EventType != bellows.EventPRMerged {
		return
	}
	if strings.HasPrefix(event.BeadID, "ext-") {
		return
	}
	anvilCfg, ok := d.cfg.Load().Anvils[event.Anvil]
	if !ok || anvilCfg.Path == "" {
		return
	}
	beadID, anvil, anvilPath, prNumber := event.BeadID, event.Anvil, anvilCfg.Path, event.PRNumber
	reason := fmt.Sprintf("PR #%d merged", prNumber)
	go func() {
		// Detached from the bellows poll context, which is cancelled at the end
		// of the cycle and would abort the burst mid-backoff.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), beadCloseBudget)
		defer cancel()
		_ = d.closeMergedBead(closeCtx, beadID, anvil, anvilPath, reason, prNumber, nil)
	}()
}

// notifyWicketPRCreated informs the Wicket monitor that a PR has been created
// for the given bead, so it can post a follow-up comment on the linked GitHub
// issue. This is a no-op when Wicket is not running or the bead has no linked
// issue. The call runs in a short-lived goroutine so it does not block the
// dispatch path.
func (d *Daemon) notifyWicketPRCreated(beadID, prURL string, prNumber int) {
	d.wicketMu.Lock()
	wm := d.wicketMonitor
	d.wicketMu.Unlock()
	if wm == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		wm.HandlePRCreated(ctx, beadID, prURL, prNumber)
	}()
}

// handleWicketPRMerged bridges bellows EventPRMerged events to the Wicket
// lifecycle handler so that Wicket-sourced GitHub issues are automatically
// closed when their linked PR is merged.
func (d *Daemon) handleWicketPRMerged(ctx context.Context, event bellows.PREvent) {
	if event.EventType != bellows.EventPRMerged {
		return
	}
	d.wicketMu.Lock()
	wm := d.wicketMonitor
	d.wicketMu.Unlock()
	if wm == nil {
		return
	}
	// Derive base branch from the PR record in the DB.
	var baseBranch string
	if pr, err := d.db.GetPRByNumber(event.Anvil, event.PRNumber); err == nil && pr != nil {
		baseBranch = pr.BaseBranch
	}
	go func() {
		lCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		wm.HandlePRMerged(lCtx, event.BeadID, event.PRURL, baseBranch, event.PRNumber)
	}()
}

// handleSelfDeploy triggers Forge's self-deploy flow when a PR merges on Forge's
// own repository. It is gated behind self_deploy.enabled (default off) and only
// fires for the configured self_deploy anvil and base branch. When it accepts an
// event it launches runSelfDeploy in the background so the bellows handler chain
// is never blocked by a drain-and-rebuild that can take many minutes.
func (d *Daemon) handleSelfDeploy(_ context.Context, event bellows.PREvent) {
	if !d.selfDeployAccepts(event) {
		return
	}

	// Single-flight: a second merge event while a deploy is already draining or
	// running is a no-op — the in-flight deploy already pulls the latest tip.
	if !d.selfDeployInFlight.CompareAndSwap(false, true) {
		d.logger.Info("self-deploy already in flight; ignoring merge event",
			"anvil", event.Anvil, "pr", event.PRNumber)
		return
	}

	sd := d.config().SelfDeploy
	go func() {
		defer d.selfDeployInFlight.Store(false)
		d.runSelfDeploy(sd)
	}()
}

// selfDeployAccepts reports whether a bellows event qualifies to trigger a
// self-deploy: it must be a PR-merged event, the feature must be enabled with a
// configured anvil, the event anvil must match, and the merged PR's recorded
// base branch must equal the watched branch.
//
// The base-branch check is deliberately conservative. The bellows PREvent does
// not carry the base branch, so it is read from the PR record. A missing record,
// a lookup error, OR an empty recorded base branch all disqualify the event:
// an empty base branch is ambiguous (it may not be the watched branch at all),
// and silently treating "unknown" as "matches" would let a merge to some other
// branch trigger a production restart. When in doubt, do not deploy.
func (d *Daemon) selfDeployAccepts(event bellows.PREvent) bool {
	if event.EventType != bellows.EventPRMerged {
		return false
	}
	sd := d.config().SelfDeploy
	if !sd.Enabled || sd.Anvil == "" {
		return false
	}
	if event.Anvil != sd.Anvil {
		return false
	}
	pr, err := d.db.GetPRByNumber(event.Anvil, event.PRNumber)
	if err != nil || pr == nil {
		d.logger.Warn("self-deploy: could not resolve merged PR record; skipping",
			"anvil", event.Anvil, "pr", event.PRNumber, "error", err)
		return false
	}
	if pr.BaseBranch == "" {
		d.logger.Warn("self-deploy: merged PR has no recorded base branch; skipping to avoid an unintended restart",
			"anvil", event.Anvil, "pr", event.PRNumber)
		return false
	}
	if pr.BaseBranch != sd.ResolvedBranch() {
		return false
	}
	return true
}

// runSelfDeploy pauses dispatch, drains active workers, then rebuilds and
// restarts the daemon binary. It runs on its own goroutine and uses the daemon's
// root context so a graceful shutdown cancels the drain/pull/build cleanly. The
// final restart step is deliberately exempt: it runs detached and context-free
// so that stopping forge.service cannot kill the process performing the restart
// (see selfdeploy.SystemctlRestarter). On a successful restart the process is
// typically terminated by systemd and this function never returns.
func (d *Daemon) runSelfDeploy(sd config.SelfDeployConfig) {
	anvilCfg, ok := d.config().Anvils[sd.Anvil]
	if !ok || anvilCfg.Path == "" {
		d.logger.Warn("self-deploy: configured anvil not found or missing path; aborting", "anvil", sd.Anvil)
		_ = d.db.LogEvent(state.EventSelfDeployFailed,
			fmt.Sprintf("self-deploy aborted: anvil %q not found or has no path", sd.Anvil), "", sd.Anvil)
		return
	}

	ctx := d.runCtx
	if ctx == nil {
		ctx = context.Background()
	}

	maxDrainWait := sd.ResolvedMaxDrainWait()

	// Pause dispatch so no new workers start while the deploy drains, then arrange
	// the resume up front: every exit path below — drain timeout, build failure,
	// rollback, panic — runs it, so a failed deploy can never leave the daemon
	// permanently paused.
	restorePause := d.pauseForSelfDeploy(maxDrainWait)
	restartRequested := false
	defer func() { restorePause(restartRequested) }()

	d.logger.Info("self-deploy: dispatch paused; draining workers before rebuild",
		"anvil", sd.Anvil, "max_drain_wait", maxDrainWait)

	deployer := selfdeploy.New(
		selfdeploy.Config{
			RepoPath:    sd.ResolvedRepoPath(anvilCfg.Path),
			BinaryPath:  sd.ResolvedBinaryPath(),
			UnitName:    sd.ResolvedUnitName(),
			Branch:      sd.ResolvedBranch(),
			BuildTarget: sd.ResolvedBuildTarget(),
			// The running daemon was built from the binary the deploy preserves
			// for rollback, so its build identifies what a rollback restores.
			CurrentSHA:   forge.Build,
			MaxDrainWait: maxDrainWait,
		},
		selfdeploy.ExecCommander{},
		selfdeploy.SystemctlRestarter{
			Cmd:         sd.ResolvedRestartCommand(),
			PrependArgs: sd.RestartArgs,
			// The restarter logs its intent (build SHA, binary, rollback path,
			// argv) to daemon.log immediately before spawning, so a restart that
			// is killed mid-flight is still diagnosable after the fact.
			Logger: d.logger,
		},
		selfDeployEventSink{db: d.db, anvil: sd.Anvil},
		d.activeWorkerIDs,
		// A failed or rolled-back deploy is otherwise silent — the daemon keeps
		// running the old binary exactly as before — so escalate it into Hearth's
		// Needs Attention list instead of leaving it to be found by diffing
		// `forge version` against origin/main.
		selfdeploy.WithEmitter(selfDeployAttentionSink{
			db:    d.db,
			anvil: sd.Anvil,
			unit:  sd.ResolvedUnitName(),
		}),
		selfdeploy.WithLogger(d.logger),
	)

	// Deploy owns the bounded drain wait: it re-checks activeWorkerIDs on a
	// ticker until the forge is idle or maxDrainWait is spent, and only then
	// pulls, builds and swaps.
	if err := deployer.Deploy(ctx); err != nil {
		switch {
		case errors.Is(err, selfdeploy.ErrDrainTimeout):
			// Deferred, not failed: the deployer already logged a skipped event
			// carrying the elapsed time and the workers that held it up.
			d.logger.Warn("self-deploy: workers did not drain; deferring deploy",
				"anvil", sd.Anvil, "max_drain_wait", maxDrainWait, "error", err)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			d.logger.Info("self-deploy: aborted before the swap", "anvil", sd.Anvil, "error", err)
		default:
			d.logger.Warn("self-deploy failed", "anvil", sd.Anvil, "error", err)
		}
		// Deploy leaves the live binary intact (or rolled back) on failure, so the
		// deferred resume can safely return the daemon to normal operation.
		return
	}
	// On success the restart typically terminates this process; if it returned
	// nil without doing so we still leave dispatch paused pending the restart.
	restartRequested = true
	d.logger.Info("self-deploy: new binary installed and restart requested", "anvil", sd.Anvil)
}

// activeWorkerIDs identifies the workers that would be disrupted by a restart:
// all non-terminal dispatch/lifecycle workers plus operator-paused workers
// (which still hold a worktree and would resume into a running Smith). Workers
// are named by bead where known, since that is what an operator recognises when
// a deploy reports what is holding it up.
func (d *Daemon) activeWorkerIDs() ([]string, error) {
	active, err := d.db.ActiveWorkers()
	if err != nil {
		return nil, err
	}
	paused, err := d.db.PausedWorkers()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(active)+len(paused))
	for _, group := range [][]state.Worker{active, paused} {
		for _, w := range group {
			if w.BeadID != "" {
				ids = append(ids, w.BeadID)
				continue
			}
			ids = append(ids, w.ID)
		}
	}
	return ids, nil
}

// selfDeployEventSink adapts state.DB.LogEvent to the selfdeploy.EventSink
// interface, mapping the package's string event names to state.EventType and
// recording them against the self_deploy anvil.
type selfDeployEventSink struct {
	db    *state.DB
	anvil string
}

func (s selfDeployEventSink) Emit(event, message string) {
	_ = s.db.LogEvent(state.EventType(event), message, "", s.anvil)
}

// selfDeployAttentionSink adapts the state.DB deploy_failures table to the
// selfdeploy.Emitter interface, so a deferred, failed or rolled-back deploy
// becomes a row in Hearth's Needs Attention list rather than a single event-log
// line nobody reads. The row is keyed by anvil + reason and outlives daemon
// restarts, because a rollback leaves the daemon running the old binary and the
// operator has to see that state whenever they next look.
type selfDeployAttentionSink struct {
	db    *state.DB
	anvil string
	unit  string
}

// EmitNeedsAttention records one needs-attention row for the deploy failure.
func (s selfDeployAttentionSink) EmitNeedsAttention(ev selfdeploy.DeployEvent) error {
	unit := ev.Unit
	if unit == "" {
		unit = s.unit
	}
	return s.db.RecordDeployFailure(state.DeployFailure{
		Anvil:        s.anvil,
		Unit:         unit,
		Reason:       string(ev.Reason),
		Detail:       ev.Detail,
		AttemptedSHA: ev.AttemptedSHA,
		RestoredSHA:  ev.RestoredSHA,
		RolledBack:   ev.RolledBack,
		FailedAt:     ev.Timestamp,
	})
}

// ClearNeedsAttention removes the rows a later deploy has superseded. Passing no
// reasons clears every outstanding failure for the anvil.
func (s selfDeployAttentionSink) ClearNeedsAttention(reasons ...selfdeploy.FailureReason) error {
	names := make([]string, 0, len(reasons))
	for _, r := range reasons {
		names = append(names, string(r))
	}
	_, err := s.db.ClearDeployFailures(s.anvil, names...)
	return err
}

// reconcileMergedBeads is a startup catch-up pass that closes beads whose PRs
// merged while the daemon was down (or whose close failed before restart).
// It queries state.db for merged PRs (excluding external and empty bead IDs)
// and attempts to close any corresponding beads that are still open.
// Safe to call multiple times — bd close is idempotent.
func (d *Daemon) reconcileMergedBeads(ctx context.Context) {
	mergedPRs, err := d.db.MergedPRs()
	if err != nil {
		d.logger.Error("reconcileMergedBeads: failed to query merged PRs", "error", err)
		return
	}

	if len(mergedPRs) == 0 {
		return
	}

	var closed int
	for _, pr := range mergedPRs {
		anvilCfg, ok := d.cfg.Load().Anvils[pr.Anvil]
		if !ok || anvilCfg.Path == "" {
			d.logger.Debug("reconcileMergedBeads: skipping PR with unknown/unconfigured anvil",
				"bead", pr.BeadID, "pr", pr.Number, "anvil", pr.Anvil)
			continue
		}

		// Skip beads that bd already shows as closed. bd's `close` command is
		// not a true no-op on already-closed beads — it still updates
		// closed_at and writes an event row, generating Dolt commits for no
		// semantic change. Across many restarts this produces hundreds of
		// spurious commits per anvil per day. Pre-checking via `bd show` is
		// cheap (auto-pull/auto-push are no-ops on reads) and idempotent.
		if status := d.fetchBeadStatus(anvilCfg.Path, pr.BeadID); status == "closed" {
			d.logger.Debug("reconcileMergedBeads: bead already closed, skipping",
				"bead", pr.BeadID, "pr", pr.Number)
			continue
		}

		// Share the close-after-merge retry path so a transient dolt/beads
		// failure here is retried and, if it still fails, left as a pending
		// close for the next Bellows cycle rather than waiting for the next
		// daemon restart.
		closeCtx, cancel := context.WithTimeout(ctx, beadCloseBudget)
		err := d.closeMergedBead(closeCtx, pr.BeadID, pr.Anvil, anvilCfg.Path,
			fmt.Sprintf("PR #%d merged (startup reconciliation)", pr.Number), pr.Number, nil)
		cancel()
		if err == nil {
			closed++
			d.logger.Info("reconcileMergedBeads: closed bead for merged PR",
				"bead", pr.BeadID, "pr", pr.Number, "anvil", pr.Anvil)
		}
	}

	if closed > 0 {
		d.logger.Info("reconcileMergedBeads: startup catch-up complete",
			"closed", closed, "total_merged_prs", len(mergedPRs))
	}
}

// handleAutoMerge is the bellows callback for the ready-to-merge transition.
// It launches the actual merge in a goroutine so bellows' poll loop is not
// blocked by the VCS call. This avoids a race where bellows event handlers
// would stall waiting for the merge RPC to complete.
func (d *Daemon) handleAutoMerge(ctx context.Context, anvil string, pr state.PR) {
	// Never auto-merge external PRs.
	if pr.IsExternal() {
		return
	}

	anvilCfg, ok := d.cfg.Load().Anvils[anvil]
	if !ok || !anvilCfg.AutoMerge {
		return
	}

	if anvilCfg.Path == "" {
		d.logger.Warn("auto-merge skipped: anvil path is empty", "anvil", anvil, "pr_number", pr.Number)
		return
	}

	// Do not launch new auto-merge goroutines once shutdown has been signalled.
	if ctx.Err() != nil {
		return
	}

	// Track the goroutine so d.wg.Wait() during shutdown waits for in-flight
	// auto-merges to complete (avoids orphaned goroutines).
	// IMPORTANT: derive mergeCtx from context.Background(), NOT from the
	// bellows ctx. This ensures that an in-flight merge completes even during
	// graceful shutdown (SIGINT/SIGTERM), avoiding a half-merged state.
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.doAutoMerge(context.Background(), anvil, anvilCfg.Path, pr)
	}()
}

// doAutoMerge performs the actual VCS merge for a PR that has reached the
// ready-to-merge state on an anvil with auto_merge enabled.
func (d *Daemon) doAutoMerge(ctx context.Context, anvil, anvilPath string, pr state.PR) {
	strategy := d.cfg.Load().Settings.MergeStrategy
	if strategy == "" {
		strategy = "squash"
	}

	d.logger.Info("auto-merging PR", "pr_number", pr.Number, "anvil", anvil, "bead", pr.BeadID, "strategy", strategy)
	if err := d.db.LogEvent(state.EventPRMergeRequested,
		fmt.Sprintf("PR #%d auto-merge started (strategy: %s)", pr.Number, strategy),
		pr.BeadID, anvil); err != nil {
		d.logger.Warn("failed to log auto-merge start event", "error", err)
	}

	mergeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	if err := d.vcsForAnvil(anvil).MergePR(mergeCtx, anvilPath, pr.Number, strategy); err != nil {
		if logErr := d.db.LogEvent(state.EventPRMergeFailed,
			fmt.Sprintf("PR #%d auto-merge failed: %v", pr.Number, err),
			pr.BeadID, anvil); logErr != nil {
			d.logger.Warn("failed to log auto-merge failure event", "error", logErr)
		}
		d.logger.Error("auto-merge failed", "pr_number", pr.Number, "anvil", anvil, "error", err)
		return
	}

	if err := d.db.LogEvent(state.EventPRAutoMerged,
		fmt.Sprintf("PR #%d auto-merged successfully (strategy: %s)", pr.Number, strategy),
		pr.BeadID, anvil); err != nil {
		d.logger.Warn("failed to log auto-merge success event", "error", err)
	}
	d.logger.Info("PR auto-merged successfully", "pr_number", pr.Number, "anvil", anvil, "bead", pr.BeadID)
}

// drainActionPriority is the order parked lifecycle actions are dispatched when
// more than one kind is queued for a bead. CI fixes go first (a red build blocks
// everything), then review fixes, then rebase. Any action not listed drains
// after these in unspecified-but-deterministic order (by Action value).
var drainActionPriority = []lifecycle.Action{
	lifecycle.ActionFixCI,
	lifecycle.ActionFixReview,
	lifecycle.ActionRebase,
}

// parkPendingAction stashes a lifecycle action to run once the bead frees,
// keyed by action type so distinct fix kinds for the same bead coexist (the old
// single-slot latest-wins dropped one, stranding PRs in needs_fix). Latest-wins
// only within a single action type.
func (d *Daemon) parkPendingAction(lockKey string, req lifecycle.ActionRequest) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	m, _ := d.pendingActions.Load(lockKey)
	actions, _ := m.(map[lifecycle.Action]lifecycle.ActionRequest)
	if actions == nil {
		actions = make(map[lifecycle.Action]lifecycle.ActionRequest)
	}
	actions[req.Action] = req
	d.pendingActions.Store(lockKey, actions)
}

// drainPendingAction dispatches ONE parked lifecycle action for beadID (highest
// priority first), re-parking the rest. Called after activeBeads.Delete so the
// bead is free. The dispatched action's own completion calls drainPendingAction
// again, so multiple parked actions drain in sequence (one worktree at a time).
func (d *Daemon) drainPendingAction(ctx context.Context, beadID string) {
	d.pendingMu.Lock()
	v, ok := d.pendingActions.Load(beadID)
	if !ok {
		d.pendingMu.Unlock()
		return
	}
	actions, ok := v.(map[lifecycle.Action]lifecycle.ActionRequest)
	if !ok {
		d.pendingActions.Delete(beadID)
		d.pendingMu.Unlock()
		d.logger.Error("pending lifecycle actions have unexpected type", "bead", beadID, "valueType", fmt.Sprintf("%T", v))
		return
	}
	req, found := popPriorityAction(actions)
	if !found {
		d.pendingActions.Delete(beadID)
		d.pendingMu.Unlock()
		return
	}
	delete(actions, req.Action)
	remaining := len(actions)
	if remaining == 0 {
		d.pendingActions.Delete(beadID)
	} else {
		d.pendingActions.Store(beadID, actions)
	}
	d.pendingMu.Unlock()
	d.logger.Info("draining parked lifecycle action", "bead", beadID, "action", req.Action, "remaining", remaining)
	d.handleLifecycleAction(ctx, req)
}

// popPriorityAction selects which parked action to dispatch next: the highest
// priority present (see drainActionPriority), falling back to the lowest Action
// value so the choice is deterministic (Go map iteration order is random).
func popPriorityAction(actions map[lifecycle.Action]lifecycle.ActionRequest) (lifecycle.ActionRequest, bool) {
	if len(actions) == 0 {
		return lifecycle.ActionRequest{}, false
	}
	for _, a := range drainActionPriority {
		if req, ok := actions[a]; ok {
			return req, true
		}
	}
	var best lifecycle.Action
	first := true
	for a := range actions {
		if first || a < best {
			best, first = a, false
		}
	}
	return actions[best], true
}

// effectiveMaxLifecycleWorkers resolves the configured lifecycle concurrency cap,
// falling back to config.DefaultMaxLifecycleWorkers when the setting is unset or
// non-positive.
func effectiveMaxLifecycleWorkers(limit int) int {
	if limit <= 0 {
		return config.DefaultMaxLifecycleWorkers
	}
	return limit
}

// reserveLifecycleSlot atomically tries to reserve a concurrency slot for a
// lifecycle fix worker. Returns true if a slot was acquired (caller MUST call
// releaseLifecycleSlot exactly once), false if the cap is reached.
func (d *Daemon) reserveLifecycleSlot(limit int) bool {
	maxSlots := int64(effectiveMaxLifecycleWorkers(limit))
	for {
		cur := d.lifecycleActive.Load()
		if cur >= maxSlots {
			return false
		}
		if d.lifecycleActive.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// waitForLifecycleSlot blocks until a lifecycle concurrency slot is available or
// ctx is cancelled. Returns true if a slot was acquired (caller MUST call
// releaseLifecycleSlot), false if the context was cancelled before a slot freed.
func (d *Daemon) waitForLifecycleSlot(ctx context.Context, limit int) bool {
	if d.reserveLifecycleSlot(limit) {
		return true
	}

	d.logger.Info("waiting for lifecycle slot",
		"active", d.lifecycleActive.Load(),
		"limit", effectiveMaxLifecycleWorkers(limit),
	)

	// Bridge ctx cancellation into the Cond so Wait() unblocks.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			d.lifecycleCond.Broadcast()
		case <-done:
		}
	}()
	defer close(done)

	d.lifecycleCond.L.Lock()
	defer d.lifecycleCond.L.Unlock()
	for {
		if ctx.Err() != nil {
			return false
		}
		if d.reserveLifecycleSlot(limit) {
			return true
		}
		d.lifecycleCond.Wait()
	}
}

// releaseLifecycleSlot frees a slot previously reserved by
// reserveLifecycleSlot/waitForLifecycleSlot and wakes any goroutines waiting
// for a slot.
func (d *Daemon) releaseLifecycleSlot() {
	d.lifecycleActive.Add(-1)
	if d.lifecycleCond != nil {
		d.lifecycleCond.Broadcast()
	}
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
			d.checkStaleWorkers(interval)

			// Adjust ticker cadence if the stale interval changed.
			if newCheckInterval := checkIntervalFor(interval); newCheckInterval != checkInterval {
				ticker.Reset(newCheckInterval)
				checkInterval = newCheckInterval
			}
		}
	}
}

// checkStaleWorkers runs a single stale-detection pass for the given interval:
// it marks newly-stalled workers and un-stalls any previously-stalled worker
// whose log file has become fresh again (self-healing recovery). It is factored
// out of runStaleDetection's ticker loop so it can be exercised directly in tests.
func (d *Daemon) checkStaleWorkers(interval time.Duration) {
	stalled, err := d.db.StalledWorkers(interval)
	if err != nil {
		d.logger.Warn("stale detection: failed to query workers", "error", err)
		return
	}
	for _, w := range stalled {
		// Defensive skip: a paused (parked) worker legitimately stops producing
		// log output while an operator pause holds it, so it must never be flagged
		// stale. StalledWorkers already excludes 'paused' via its status allowlist;
		// this guard ensures the invariant holds even if that query is later
		// broadened.
		if w.Status == state.WorkerPaused {
			continue
		}
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

	// Recovery pass: a worker previously marked stalled whose log file is fresh
	// again has resumed work and should be un-stalled so it stops showing up as
	// needing attention and is no longer mistakenly counted (or excluded) when it
	// is in fact making progress. This is automatic self-healing — no operator
	// confirmation required.
	recovered, err := d.db.RecoveredStalledWorkers(interval)
	if err != nil {
		d.logger.Warn("stale detection: failed to query recovered workers", "error", err)
		return
	}
	for _, w := range recovered {
		d.logger.Info("un-stalling worker — log activity resumed",
			"worker", w.ID, "bead", w.BeadID, "anvil", w.Anvil,
			"phase", w.Phase, "stale_interval", interval)
		if err := d.db.UnstallWorker(w.ID); err != nil {
			d.logger.Error("failed to un-stall worker", "worker", w.ID, "error", err)
			continue
		}
		_ = d.db.LogEvent(state.EventWorkerRecovered,
			fmt.Sprintf("Worker %s recovered (log activity resumed)", w.ID),
			w.BeadID, w.Anvil)
	}
}

// pollAndDispatch polls all anvils for ready beads and dispatches workers.
// When fullPoll is true, an unfiltered poll is performed and the cached Blocks
// graph is rebuilt (slow path). When false, a label-filtered poll is performed
// and the cached Blocks graph is merged into the results (fast path).
//
// It is serialized via a try-lock: if a poll is already running (e.g. an IPC
// "refresh" overlapping with the ticker), the second caller returns immediately.
// The in-progress poll already holds a consistent capacity snapshot, so
// skipping the duplicate avoids double-dispatching past max_total_smiths.
func (d *Daemon) pollAndDispatch(ctx context.Context, fullPoll bool) {
	if !d.pollRunning.CompareAndSwap(false, true) {
		d.logger.Debug("pollAndDispatch already running, skipping concurrent invocation")
		return
	}
	defer d.pollRunning.Store(false)

	// Snapshot config once so the entire poll cycle sees a consistent view even
	// if hot-reload swaps the pointer concurrently.
	cfg := d.cfg.Load()

	// Legacy mode (Crucible polling disabled) must remain unfiltered even when
	// individual callers request a fast poll.
	effectiveFullPoll := fullPoll || cfg.Settings.CruciblePollInterval <= 0

	if effectiveFullPoll {
		d.logger.Info("polling anvils (full)", "count", len(cfg.Anvils))
	} else {
		d.logger.Info("polling anvils (fast)", "count", len(cfg.Anvils))
	}

	// Periodically recover orphaned in-progress beads (every 10 poll cycles).
	// Recovery also runs once at startup (see Start). Running it here catches
	// beads that become orphaned during normal operation — for example, a
	// worker that crashed between claiming a bead in bd and inserting its row
	// into state.db. A minimum-age guard inside RecoverOrphanedBeads prevents
	// it from reopening legitimately in-flight beads on each periodic check.
	count := d.pollCount.Add(1)
	if count%10 == 0 {
		if recovered := d.shutdownMgr.RecoverOrphanedBeads(); recovered > 0 {
			d.logger.Info("periodic bead recovery", "recovered", recovered)
		}
		// Periodically reconcile open PRs so external PRs appear in Hearth
		d.reconcileOpenPRs(ctx)
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

	// Detect anvils whose beads database is left mid-merge with unresolved
	// conflicts. While wedged, every bd write against the anvil is rolled back,
	// so polling "successfully" and dispatching work there is theatre. Full poll
	// only: a wedge lasts minutes to hours, so there is no value in paying for
	// the check on every fast cycle.
	if effectiveFullPoll {
		d.checkAnvilHealth(ctx, cfg)
	}

	// Always poll so the Hearth TUI queue cache stays current even when all
	// smith slots are occupied. Capacity is checked below before dispatching.
	// Stagger anvil polls to avoid simultaneous bd/git command bursts.
	pollInterval := cfg.Settings.PollInterval
	if pollInterval == 0 {
		pollInterval = DefaultPollInterval
	}
	// When running under a context with a deadline (e.g. short IPC refreshes),
	// avoid using a stagger interval that extends beyond the remaining time
	// budget; otherwise some anvils may never start polling before the
	// context is cancelled.
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 && remaining < pollInterval {
			pollInterval = remaining
		}
	}
	p := poller.NewStaggered(cfg.Anvils, pollInterval)
	p.BdReadyLimit = cfg.Settings.BdReadyLimit
	if !effectiveFullPoll {
		p.UseLabelFilter = true
	}
	pollKind := "full"
	if !effectiveFullPoll {
		pollKind = "fast"
	}
	// Record each anvil's poll completion as soon as it finishes so Hearth and
	// `forge status` can show per-anvil timestamps that reflect the actual
	// stagger, not a single shared timestamp logged after wg.Wait(). Successful
	// polls are tracked in-memory only (see Daemon.lastPollMap); only failures
	// are persisted to the events table to avoid drowning the event log with
	// hundreds of rows per hour.
	p.OnAnvilDone = func(r poller.AnvilResult) {
		if r.Err != nil {
			_ = d.db.LogEvent(state.EventPollError, r.Err.Error(), "", r.Name)
			d.recordAnvilPoll(r.Name, false, r.Err.Error())
		} else {
			d.recordAnvilPoll(r.Name, true, fmt.Sprintf("[%s] %d ready", pollKind, len(r.Beads)))
		}
	}
	beads, results := p.Poll(ctx)

	if effectiveFullPoll {
		// Slow path: rebuild the cached Blocks graph from the unfiltered results.
		graph := poller.BuildBlocksGraph(beads)
		d.cachedBlocksMu.Lock()
		d.cachedBlocks = graph
		d.cachedBlocksMu.Unlock()
	} else {
		// Fast path: merge cached Blocks into label-filtered results so
		// IsCrucibleCandidate still detects parent beads.
		d.cachedBlocksMu.RLock()
		cached := d.cachedBlocks
		d.cachedBlocksMu.RUnlock()
		poller.MergeBlocksFromCache(beads, cached)
	}

	anvilPaths := make(map[string]string, len(cfg.Anvils))
	for name, anvil := range cfg.Anvils {
		anvilPaths[name] = anvil.Path
	}

	// When Crucible is enabled, enrich beads with their blocks (children)
	// so we can detect parent beads that should be dispatched through the
	// Crucible instead of the normal pipeline. This must run before
	// ResolveEpicBranches because epic branch detection now also checks
	// Crucible parent detection now relies entirely on the poller's
	// reconstruction from Parent/Dependencies fields (in pollAnvil).
	// ResolveBlocks was removed because bd show's "dependents" array
	// lists beads that depend on me (i.e. parents I block), NOT children —
	// causing children to be misidentified as crucible parents.

	// Resolve epic branches for beads that belong to an epic. This enriches
	// each bead's EpicBranch field so dispatchBead can branch from and PR to
	// the correct epic branch. Detection works via the parent field (legacy)
	// or via the blocks/blocked_by dependency graph (preferred).
	poller.ResolveEpicBranches(ctx, beads, anvilPaths)

	for _, r := range results {
		if r.Err != nil {
			d.logger.Warn("poll error", "anvil", r.Name, "error", r.Err)
		} else {
			d.logger.Info("poll complete", "anvil", r.Name, "ready", len(r.Beads))
		}
	}

	// Update the two-tier snapshot. Fast (label-filtered) polls only refresh
	// the labeled map for filtered anvils; slow polls refresh both. The merged
	// view feeds lastBeads (used by IPC and title lookups) and the queue_cache
	// rows so unlabeled beads stay visible to Hearth between slow polls.
	d.updateBeadSnapshot(cfg, beads, results, !effectiveFullPoll)

	// Refresh lastBeads with the merged view across all anvils so IPC consumers
	// (run_bead cache lookup, crucibleParentTitle) see unlabeled beads even
	// after a fast poll.
	merged := d.mergedBeadSnapshot()
	d.lastBeadsMu.Lock()
	d.lastBeads = merged
	d.lastBeadsMu.Unlock()

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

		// Filter the already-merged+sorted slice to succeeded anvils to avoid a
		// second merge/sort and extra snapshotMu acquisitions.
		mergedForCache := make([]poller.Bead, 0, len(merged))
		for _, b := range merged {
			if _, ok := succeededSet[b.Anvil]; ok {
				mergedForCache = append(mergedForCache, b)
			}
		}
		var cacheItems []state.QueueItem
		// Collect timestamps alongside the cache rebuild so the IPC "queue"
		// handler can attach created_at/updated_at to QueueItem responses
		// without persisting them in SQLite. The map is rebuilt per-poll for
		// the same set of anvils as the cache itself, so freshness mirrors
		// the cache exactly.
		stamps := make(map[string]queueTimestamp)
		for _, b := range mergedForCache {
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
			stamps[b.Anvil+"/"+b.ID] = queueTimestamp{CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt}
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
			stamps[b.Anvil+"/"+b.ID] = queueTimestamp{CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt}
		}

		if err := d.db.ReplaceQueueCacheForAnvils(succeededAnvils, dedupeCacheItems(cacheItems)); err != nil {
			d.logger.Warn("failed to cache queue", "error", err)
		} else {
			d.replaceQueueTimestamps(succeededSet, stamps)
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

	// Preload wedged anvils once per cycle. Dispatching into an anvil whose beads
	// database is mid-merge cannot succeed — the first bd write is rolled back —
	// so those beads are skipped with a real reason rather than burned through
	// the retry budget.
	wedgedAnvils := d.wedgedAnvilSet()

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

	// Pause switch: skip all new dispatch while paused. Running workers are
	// untouched and finish normally; only new claims/dispatch are skipped.
	// Manual `forge queue run` remains allowed (handled in the run_bead path).
	if ps := d.dispatchPauseState(); ps.Paused {
		d.logger.Debug("dispatch paused, skipping dispatch", "reason", string(ps.Reason))
		return
	}

	// Check daily cost limit before dispatching new work. The gate projects
	// in-flight (not-yet-recorded) spend on top of the recorded daily total —
	// the sum of active workers' reservations plus one per-worker estimate for
	// the worker about to be dispatched — so N concurrent workers cannot blow
	// past the limit by roughly N × per-bead cost (Forge-s3w7). This pre-loop
	// check handles the once-per-day notification and the fully-blocked case;
	// the gate is re-evaluated before EACH dispatch inside the loop below.
	//
	// Capture the date once so the cost lookup, the per-dispatch re-checks, and
	// the event-suppression key all use the same day even if midnight rolls
	// over mid-poll.
	costLimit := cfg.Settings.DailyCostLimit
	today := time.Now().Format("2006-01-02")
	if costLimit > 0 {
		allowed, projected, err := d.costGateAllows(cfg, today)
		if err != nil {
			d.logger.Error("checking daily cost", "error", err)
			return
		}
		if !allowed {
			// Recorded (completed) spend for the message/notification tokens.
			todayCost, terr := d.db.GetTodayCostOn(today)
			if terr != nil {
				d.logger.Error("checking daily cost", "error", terr)
				return
			}
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
					fmt.Sprintf("Daily cost $%.2f (projected $%.2f incl. in-flight reserve) reached limit $%.2f — dispatch paused", todayCost, projected, costLimit),
					"", "")
				d.logger.Warn("daily cost limit reached, dispatch paused",
					"cost", fmt.Sprintf("$%.2f", todayCost),
					"projected", fmt.Sprintf("$%.2f", projected),
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

	// One pre-pass over the whole batch: which beads are a Crucible's to
	// dispatch, not this loop's. The batch is priority-sorted, so a child can
	// precede its parent — the set has to be known before the first dispatch.
	crucibleOwned := d.crucibleOwnedChildren(cfg, beads)

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
			d.releaseBeadSlot(bead.ID)
			continue
		}

		// Skip beads that need human attention (needs_human=1)
		if _, broken := needsHumanSet[bead.ID+"\x00"+bead.Anvil]; broken {
			d.releaseBeadSlot(bead.ID)
			continue
		}

		// Skip children of an orchestrated parent: the Crucible dispatches them
		// itself, on the feature branch, once it reaches them. Dispatching here
		// too would run the same bead twice in one cycle.
		if _, owned := crucibleOwned[bead.Anvil+"\x00"+bead.ID]; owned {
			d.logger.Info("skipping child of an orchestrated parent, the Crucible dispatches it",
				"bead", bead.ID, "anvil", bead.Anvil)
			d.releaseBeadSlot(bead.ID)
			continue
		}

		// Skip every bead in a wedged anvil. The anvil itself is already surfaced
		// in needs-attention with the conflict detail, so this stays at Debug to
		// avoid one line per queued bead per poll.
		if _, wedged := wedgedAnvils[bead.Anvil]; wedged {
			d.logger.Debug("skipping bead in wedged anvil", "bead", bead.ID, "anvil", bead.Anvil)
			d.releaseBeadSlot(bead.ID)
			continue
		}

		// Skip beads that already have an open PR (bellows should handle them)
		if hasPR, err := d.db.HasOpenPRForBead(bead.ID, bead.Anvil); err == nil && hasPR {
			d.logger.Debug("skipping bead with open PR", "bead", bead.ID)
			d.releaseBeadSlot(bead.ID)
			continue
		}

		anvilCfg, ok := cfg.Anvils[bead.Anvil]
		if !ok || anvilCfg.Path == "" {
			d.releaseBeadSlot(bead.ID)
			continue
		}

		// Apply auto-dispatch filtering
		if !shouldDispatch(bead, anvilCfg) {
			d.releaseBeadSlot(bead.ID)
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
				d.releaseBeadSlot(bead.ID)
				anvilActive[bead.Anvil] = maxSmiths // treat as at-capacity for this cycle
				continue
			}
			anvilActive[bead.Anvil] = cnt
		}
		if anvilActive[bead.Anvil]+thisCycleAnvil[bead.Anvil] >= maxSmiths {
			d.releaseBeadSlot(bead.ID)
			continue
		}

		// Check global capacity using snapshot + in-cycle count
		if globalActive+thisCycleTotal >= maxTotal {
			d.releaseBeadSlot(bead.ID)
			break
		}

		// Re-check the daily cost gate before EACH dispatch. Reservations made
		// for beads dispatched earlier in this same loop have grown the
		// projected total, so a batch of ready beads stops once the projection
		// reaches the limit rather than all dispatching off a single stale
		// once-per-poll check (Forge-s3w7). Once blocked, later beads in this
		// cycle stay blocked (recorded cost is fixed and reservations only
		// grow), so break rather than continue.
		if costLimit > 0 {
			allowed, _, gerr := d.costGateAllows(cfg, today)
			if gerr != nil {
				d.logger.Error("checking daily cost before dispatch", "bead", bead.ID, "error", gerr)
				d.releaseBeadSlot(bead.ID)
				break
			}
			if !allowed {
				d.releaseBeadSlot(bead.ID)
				break
			}
		}

		// Insert a pending worker row BEFORE claiming the bead. A cold-start
		// `bd update --status=in_progress` can exceed DefaultBdTimeout and be
		// killed *after* its write has already committed server-side, leaving the
		// bead in_progress with the daemon seeing an error. By pre-inserting the
		// row, orphan recovery's HasWorkerRecord check can still identify the bead
		// as Forge-owned and reclaim it; without it the bead would wedge as a
		// phantom in_progress with no worker row, invisible to recovery forever
		// (Forge-au4z). This also covers the known claim→worktree crash window.
		// The pipeline overwrites this row via INSERT OR REPLACE when it starts.
		claimWorkerID := d.insertPendingWorker(bead.ID, bead.Anvil, bead.Title)

		// Claim the bead before dispatching.
		if err := d.claimBead(ctx, bead.ID, anvilCfg.Path); err != nil {
			d.logger.Warn("failed to claim bead", "bead", bead.ID, "error", err)
			d.abortClaim(bead.ID, bead.Anvil, claimWorkerID, fmt.Sprintf("claim failed: %v", err), err)
			d.releaseBeadSlot(bead.ID)
			continue
		}

		// Create and register the control handle only after workerID is known
		// and the bead is successfully claimed, so the handle is immutable once
		// published and IPC lookups never see a partially initialized handle.
		ctrl := newControlHandle(claimWorkerID)
		d.registerControlHandle(bead.ID, ctrl)

		thisCycleAnvil[bead.Anvil]++
		thisCycleTotal++
		// Reserve this worker's estimated in-flight spend so the next iteration's
		// gate re-check (and future polls) account for it. Released in
		// dispatchBead once the worker completes (Forge-s3w7).
		costReservation := d.reserveWorkerCost(d.perWorkerCostEstimate(cfg))
		d.wg.Add(1)
		go d.dispatchBead(ctx, bead, anvilCfg, claimWorkerID, ctrl, nil, costReservation)
	}

	// Optionally log a summary of this poll cycle's dispatch activity.
	if len(thisCycleAnvil) > 0 {
		d.logger.Debug("poll cycle dispatch summary", "anvil_dispatch_counts", thisCycleAnvil)
	}
}

// reserveWorkerCost records an in-flight cost reservation of `amount` USD and
// returns an opaque key used to release it via releaseWorkerCost when the worker
// finishes. Reservations make the daily_cost_limit gate aware of spend that has
// not yet been recorded in the daily_costs table (Forge-s3w7).
func (d *Daemon) reserveWorkerCost(amount float64) uint64 {
	key := d.reservationSeq.Add(1)
	d.costReservationMu.Lock()
	if d.costReservations == nil {
		d.costReservations = make(map[uint64]float64)
	}
	d.costReservations[key] = amount
	d.costReservationMu.Unlock()
	return key
}

// releaseWorkerCost releases the reservation previously created by
// reserveWorkerCost. It is safe to call with a zero/unknown key (no-op).
func (d *Daemon) releaseWorkerCost(key uint64) {
	if key == 0 {
		return
	}
	d.costReservationMu.Lock()
	delete(d.costReservations, key)
	d.costReservationMu.Unlock()
}

// totalReservedCost returns the sum of all outstanding in-flight cost reservations.
func (d *Daemon) totalReservedCost() float64 {
	d.costReservationMu.Lock()
	defer d.costReservationMu.Unlock()
	var total float64
	for _, v := range d.costReservations {
		total += v
	}
	return total
}

// recordBeadCostSample folds a completed bead's recorded cost into the rolling
// per-bead cost average used to estimate future in-flight spend. Non-positive
// samples are ignored so a bead with no recorded cost does not drag the average
// toward zero.
func (d *Daemon) recordBeadCostSample(cost float64) {
	if cost <= 0 {
		return
	}
	d.costReservationMu.Lock()
	d.avgBeadCostN++
	d.avgBeadCost += (cost - d.avgBeadCost) / float64(d.avgBeadCostN)
	d.costReservationMu.Unlock()
}

// perWorkerCostEstimate returns the estimated in-flight spend (USD) for one
// not-yet-completed worker. It is the larger of the rolling average recorded
// per-bead cost and the configured floor (settings.per_worker_cost_estimate,
// falling back to DefaultPerWorkerCostEstimate) so the estimate is never zero
// before any cost data has accumulated.
func (d *Daemon) perWorkerCostEstimate(cfg *config.Config) float64 {
	floor := cfg.Settings.PerWorkerCostEstimate
	if floor <= 0 {
		floor = config.DefaultPerWorkerCostEstimate
	}
	d.costReservationMu.Lock()
	avg := d.avgBeadCost
	d.costReservationMu.Unlock()
	if avg > floor {
		return avg
	}
	return floor
}

// costGateAllows reports whether dispatching one more worker keeps the projected
// daily spend at or below the configured daily_cost_limit. Projected spend is the
// recorded cost for `today` plus the sum of outstanding in-flight reservations
// plus one per-worker estimate for the worker about to be dispatched. When the
// limit is 0 (disabled) it always allows. The returned projected value is the
// spend the daemon expects once the pending worker is reserved; it is surfaced in
// status output and the cost-limit event message.
func (d *Daemon) costGateAllows(cfg *config.Config, today string) (allowed bool, projected float64, err error) {
	recorded, err := d.db.GetTodayCostOn(today)
	if err != nil {
		return false, 0, err
	}
	projected = recorded + d.totalReservedCost() + d.perWorkerCostEstimate(cfg)
	limit := cfg.Settings.DailyCostLimit
	if limit <= 0 {
		return true, projected, nil
	}
	return projected <= limit, projected, nil
}

// dispatchBead runs the full pipeline for a single bead in a goroutine.
// claimWorkerID is the ID of the pending worker row inserted at claim time;
// it is passed into the pipeline so the running row overwrites the pending one.
// ctrl is the control handle created with an immutable workerID and registered
// by the caller after a successful claim, just before launching this goroutine.
// resume, when non-nil, makes this a restart-resume dispatch: the epic/crucible
// and stranded-remote-branch pre-checks are skipped, the retained worktree is
// reused as-is (no branch reset), and the pipeline is seeded to resume the
// recorded Claude session on its first iteration. Pass nil for a normal fresh
// dispatch.
//
// costReservation is the in-flight cost reservation key created by the caller
// just before launch (0 when no reservation was made). It is released — and the
// bead's actual recorded cost folded into the rolling per-worker estimate — when
// this goroutine returns, so the daily_cost_limit gate reconciles reservation →
// actual on completion or failure (Forge-s3w7).
func (d *Daemon) dispatchBead(ctx context.Context, bead poller.Bead, anvilCfg config.AnvilConfig, claimWorkerID string, ctrl *controlHandle, resume *pipeline.ResumeSession, costReservation uint64) {
	defer d.wg.Done()
	defer func() {
		// Release the in-flight cost reservation and fold the bead's actual
		// recorded cost into the rolling average so future estimates track real
		// spend. Runs on every exit path (success, failure, early abort) so a
		// reservation is never leaked.
		d.releaseWorkerCost(costReservation)
		if cost, err := d.db.GetBeadCost(bead.ID, bead.Anvil); err == nil {
			d.recordBeadCostSample(cost)
		}
		// Use releaseBeadSlotIfOwner so that if stop_bead/handleQueueStop
		// already released our slot and a new dispatch registered a different
		// handle, this cleanup is a no-op instead of deleting the new handle.
		d.releaseBeadSlotIfOwner(bead.ID, ctrl)
		if ctx.Err() == nil {
			d.drainPendingAction(ctx, bead.ID)
		}
	}()

	d.logger.Info("dispatching bead", "bead", bead.ID, "anvil", bead.Anvil, "title", bead.Title)

	// Re-verify the anvil is on main/master immediately before spawning any
	// subprocess. This catches race conditions where the branch changed between
	// the poll-loop check and actual dispatch.
	if err := verifyAnvilOnMain(ctx, d.logger, anvilCfg.Path); err != nil {
		d.logger.Error("anvil branch check failed at dispatch time, aborting bead",
			"bead", bead.ID, "anvil", bead.Anvil, "error", err)
		d.recordDispatchFailure(bead.ID, bead.Anvil,
			fmt.Sprintf("anvil branch check failed: %v", err), true)
		return
	}

	// Force-independent beads skip all epic/crucible logic and go straight
	// to the normal pipeline (e.g. running a child bead standalone).
	if bead.ForceIndependent {
		d.logger.Info("force-independent dispatch, skipping epic/crucible", "bead", bead.ID)
		goto normalPipeline
	}

	// Restart-resume dispatches always take the normal pipeline: a paused bead is
	// mid-flow in a normal Smith→Temper→Warden loop, never an epic/crucible parent,
	// and its worktree already exists.
	if resume != nil {
		d.logger.Info("restart-resume dispatch, skipping epic/crucible", "bead", bead.ID)
		goto normalPipeline
	}

	// An opted-in parent whose children are not in this poll batch (all closed,
	// or all blocked) has nothing to orchestrate. Run it as an ordinary bead
	// rather than creating an empty feature branch and stalling.
	if poller.IsOrchestratedParent(bead) && !crucible.IsCrucibleCandidate(bead) {
		d.logger.Info("orchestrated parent has no ready children, dispatching as a normal bead",
			"bead", bead.ID, "anvil", bead.Anvil)
		goto normalPipeline
	}

	// A parent that opted into orchestration (the "crucible" label, or an
	// explicit "epic-branch:<name>") but cannot be orchestrated is a
	// misconfiguration, not a dispatch: its children are already stamped with
	// its branch, so running the parent as an ordinary bead would leave them
	// basing on a branch nobody creates. Escalate instead of the old legacy
	// path, which created a dangling branch and left the parent in_progress
	// forever with nothing to close it (Forge-fblf).
	//
	// A parent with no opt-in label — including one typed `epic` — never
	// reaches here: it runs the ordinary pipeline like any other bead.
	if poller.IsOrchestratedParent(bead) && !d.cfg.Load().Settings.CrucibleEnabled {
		reason := fmt.Sprintf("bead %s opts into epic orchestration (%q or epic-branch:<name>) but "+
			"settings.crucible_enabled is false — enable it to orchestrate the epic, or remove the label "+
			"so this parent and its children dispatch independently to main",
			bead.ID, epic.CrucibleLabel)
		d.logger.Warn("orchestrated parent dispatched with the Crucible disabled",
			"bead", bead.ID, "anvil", bead.Anvil, "branch", poller.ExtractParentBranch(bead))
		if err := d.db.MarkNeedsHuman(bead.ID, bead.Anvil, reason); err != nil {
			d.logger.Error("failed to mark bead as needs_human", "bead", bead.ID, "error", err)
		}
		d.recordDispatchFailure(bead.ID, bead.Anvil, reason, true)
		return
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
			// Durable session log: without a LogDir the log lands in the
			// check's temp workdir, whose path fails the Hearth allowlist and
			// is deleted with the dir — leaving the worker panel unable to
			// stream or tail anything (Forge-x8ew).
			if home, herr := os.UserHomeDir(); herr == nil {
				schemCfg.LogDir = filepath.Join(home, ".forge", "logs", forge.SanitizeBeadID(bead.ID))
			}
			workerIDForSpawn := claimWorkerID
			schemCfg.OnSpawn = func(pid int, logPath string) {
				if err := d.db.UpdateWorkerPID(workerIDForSpawn, pid); err != nil {
					slog.Warn("failed to record schematic PID", "worker", workerIDForSpawn, "err", err)
				}
				if err := d.db.UpdateWorkerLogPath(workerIDForSpawn, logPath); err != nil {
					slog.Warn("failed to record schematic log path", "worker", workerIDForSpawn, "err", err)
				}
			}

			schematicProviderSpecs := config.ProvidersForStageWithAnvil(d.cfg.Load().Settings, &anvilCfg, "schematic")
			providers := d.filterCopilotIfLimited(provider.FromConfig(schematicProviderSpecs))
			if len(providers) == 0 {
				d.logger.Warn("schematic provider chain exhausted after Copilot quota filtering; falling back to defaults",
					"bead", bead.ID, "anvil", bead.Anvil)
				providers = provider.Defaults()
			}
			if len(providers) == 0 {
				d.logger.Warn("crucible check skipped: no providers available for schematic",
					"bead", bead.ID, "anvil", bead.Anvil)
				_ = d.db.LogEvent(state.EventSchematicDone,
					fmt.Sprintf("Crucible check skipped for %s: no providers available after Copilot quota filtering", bead.ID),
					bead.ID, bead.Anvil)
				goto normalPipeline
			}

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
		_ = d.db.UpdateWorkerStatus(claimWorkerID, state.WorkerRunning)
		d.logger.Info("dispatching crucible", "bead", bead.ID, "children", len(bead.Blocks))

		smithProviderSpecs := config.ProvidersForStageWithAnvil(d.cfg.Load().Settings, &anvilCfg, "smith")
		crucibleParams := crucible.Params{
			DB:                        d.db,
			VCS:                       d.vcsForAnvil(bead.Anvil),
			Logger:                    d.logger,
			WorktreeManager:           d.worktreeMgr,
			PromptBuilder:             d.promptBuilder,
			ParentBead:                bead,
			AnvilName:                 bead.Anvil,
			AnvilConfig:               anvilCfg,
			ExtraFlags:                d.cfg.Load().Settings.ClaudeFlags,
			Providers:                 d.filterCopilotIfLimited(provider.FromConfig(smithProviderSpecs)),
			TemperConfig:              d.resolveTemperConfig(anvilCfg),
			GoRaceDetection:           d.resolveGoRaceDetection(anvilCfg),
			TemperStepTimeout:         d.cfg.Load().Settings.TemperStepTimeout,
			TemperGitTimeout:          d.cfg.Load().Settings.TemperGitTimeout,
			TemperOutputCap:           d.cfg.Load().Settings.TemperOutputCap,
			SmithTimeout:              d.cfg.Load().Settings.SmithTimeout,
			AutoMergeCrucibleChildren: d.cfg.Load().Settings.IsAutoMergeCrucibleChildren(),
			MaxPipelineIterations:     d.cfg.Load().Settings.MaxPipelineIterations,
			WorkerID:                  claimWorkerID,
			WardenModelOverride:       d.cfg.Load().Settings.WardenModelOverride,
			SchematicModelOverride:    d.cfg.Load().Settings.SchematicModelOverride,

			CopilotSkipWardenSmallDiffs: d.cfg.Load().Settings.CopilotSkipWardenSmallDiffs,
			WardenFullRereview:          d.cfg.Load().Settings.WardenFullRereview,
			CopilotCombinedSmithWarden:  d.cfg.Load().Settings.CopilotCombinedSmithWarden,
			CopilotWardenSampleRate:     d.cfg.Load().Settings.CopilotWardenSampleRate,
			EmptyDiffAction:             d.resolveEmptyDiffAction(d.cfg.Load()),

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
					fmt.Sprintf("crucible paused: child %s failed", result.PausedChildID), true)
			} else {
				d.recordDispatchFailure(bead.ID, bead.Anvil,
					fmt.Sprintf("crucible error: %v", result.Error), true)
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
	// Pre-dispatch remote-branch check. If origin already carries unmerged
	// commits for forge/<bead-id> from a prior worker that escalated between
	// `git push` and `gh pr create`, dispatching a fresh Smith produces a
	// parallel implementation that cannot be reconciled without destroying
	// one side's work. Catch the stranded state HERE so the dispatch never
	// burns Smith time on it. A merged stale branch is cleaned up
	// transparently and dispatch proceeds normally.
	// Skip the stranded-remote-branch check on a restart-resume: the bead's
	// forge/<id> branch legitimately exists (its worktree is being reused) and is
	// not a stranded parallel implementation.
	if resume == nil && !d.preDispatchRemoteBranchCheck(ctx, bead, anvilCfg.Path) {
		// The pending worker row inserted at claim time must be terminated so
		// Hearth does not show a permanent pending worker. The pipeline never
		// ran, so we mark it failed here rather than leaving it in limbo.
		_ = d.db.UpdateWorkerStatus(claimWorkerID, state.WorkerFailed)
		return
	}

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
	// Pass a cancellable (deadline-free) context and let the pipeline own the
	// smith-timeout deadline via SmithTimeout. This is required for pause/park/
	// resume: a parked bead must be able to suspend the smith timeout, which is
	// impossible if the deadline is baked into this ctx (a child context can only
	// shorten a parent's deadline, never extend it). The pipeline re-derives its
	// timeout from this cancellable base so interrupt/shutdown still propagate.
	pipelineCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Build pipeline params, optionally enabling Schematic pre-worker.
	// Resolve per-stage providers via stage_providers → smith_providers → providers fallback.
	cfg := d.cfg.Load()
	pipelineParams := pipeline.Params{
		DB:                d.db,
		WorktreeManager:   d.worktreeMgr,
		PromptBuilder:     d.promptBuilder,
		AnvilName:         bead.Anvil,
		AnvilConfig:       anvilCfg,
		Bead:              bead,
		ExtraFlags:        cfg.Settings.ClaudeFlags,
		TemperConfig:      d.resolveTemperConfig(anvilCfg),
		GoRaceDetection:   d.resolveGoRaceDetection(anvilCfg),
		TemperStepTimeout: cfg.Settings.TemperStepTimeout,
		TemperGitTimeout:  cfg.Settings.TemperGitTimeout,
		TemperOutputCap:   cfg.Settings.TemperOutputCap,
		Providers:         d.filterCopilotIfLimited(provider.FromConfig(config.ProvidersForStageWithAnvil(cfg.Settings, &anvilCfg, "smith"))),
		Notifier:          d.notifier.Load(),
		BaseBranch:        bead.EpicBranch,
		WorkerID:          claimWorkerID,
		MaxIterations:     cfg.Settings.MaxPipelineIterations,

		// The pipeline owns its smith-timeout deadline (see pipelineCtx above) so
		// pause/park/resume can suspend it while a bead is parked.
		SmithTimeout: smithTimeout,

		// Let a parked pipeline unblock on daemon shutdown. pipelineCtx is
		// deliberately decoupled from shutdown (so post-smith work completes), and
		// the park wait no longer rides the smith timeout, so without this a parked
		// goroutine would block the shutdown drain (wg.Wait) indefinitely. runCtx
		// is cancelled on shutdown; a parked worker then stays 'paused' for resume
		// after restart rather than being force-failed.
		ShutdownCtx: d.runCtx,

		// Steer mode A: feed the control handle's steer mailbox into the
		// pipeline so a steer message can interrupt a running Smith spawn and
		// resume its session with the steering text (see internal/daemon/control.go).
		SteerCh: ctrl.steer,

		// Pause/park/resume: the control handle also carries the pause and resume
		// signals. A pause_bead request parks the running spawn; a resume_bead
		// request respawns `claude --resume <session>` (see control.go).
		ParkHandle: ctrl,

		// SpawnLive lets the pipeline tell the control handle when a Smith spawn is
		// actively running (and thus interruptible by a steer — mode A) vs not
		// (between spawns / Temper / Warden — mode B). handleSteerBead reads this
		// via ctrl.hasLiveSpawn to label the steer response. Steering itself is
		// driven purely by the steer mailbox (SteerCh above); the pipeline
		// interrupts only the current spawn, never the pipeline context.
		SpawnLive: ctrl.setLiveSpawn,

		WardenModelOverride:         cfg.Settings.WardenModelOverride,
		SchematicModelOverride:      cfg.Settings.SchematicModelOverride,
		CopilotSkipWardenSmallDiffs: cfg.Settings.CopilotSkipWardenSmallDiffs,
		WardenFullRereview:          cfg.Settings.WardenFullRereview,
		CopilotCombinedSmithWarden:  cfg.Settings.CopilotCombinedSmithWarden,
		CopilotWardenSampleRate:     cfg.Settings.CopilotWardenSampleRate,
		EmptyDiffAction:             d.resolveEmptyDiffAction(cfg),
	}

	// Only set WardenProviders/SchematicProviders when explicitly configured in
	// stage_providers; otherwise leave empty so the legacy model-override path runs.
	if wardenSpecs := config.ExplicitStageProvidersWithAnvil(cfg.Settings, &anvilCfg, "warden"); len(wardenSpecs) > 0 {
		pipelineParams.WardenProviders = d.filterCopilotIfLimited(provider.FromConfig(wardenSpecs))
	}
	if schematicSpecs := config.ExplicitStageProvidersWithAnvil(cfg.Settings, &anvilCfg, "schematic"); len(schematicSpecs) > 0 {
		pipelineParams.SchematicProviders = d.filterCopilotIfLimited(provider.FromConfig(schematicSpecs))
	}

	// Restart-resume: seed the pipeline to resume the recorded session in the
	// retained worktree. This must run before the ResetBranch retry logic below
	// so a resume never discards the paused work.
	pipelineParams.ResumeSession = resume

	// If this bead has had previous dispatch failures, reset the worktree
	// branch to the base ref so the retry starts from a clean state. This
	// prevents inheriting junk commits from a failed pipeline run. Skipped on a
	// restart-resume, which must preserve the retained worktree as-is.
	if resume == nil {
		if retry, err := d.db.GetRetry(bead.ID, bead.Anvil); err == nil && retry != nil && retry.DispatchFailures > 0 {
			pipelineParams.ResetBranch = true
			d.logger.Info("resetting worktree branch for retry", "bead", bead.ID, "failures", retry.DispatchFailures)
		}
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
			retryAt := time.Now().Add(backoff)
			d.logger.Warn("all providers rate limited; bead released to open, backing off",
				"bead", bead.ID, "backoff", backoff)
			_ = d.db.LogEvent(state.EventRateLimited,
				fmt.Sprintf("%s rate limited, will retry at %s (in %s)", bead.ID, retryAt.Format("2006-01-02 15:04:05 MST"), backoff.Round(time.Second)),
				bead.ID, bead.Anvil)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
			}
			return
		}
		if outcome.AuthFailed {
			// A provider rejected the credentials. Escalate for human attention
			// immediately — retrying a bad key loops forever (Forge-d5ns). Mark
			// the bead needs_human so it surfaces in the Needs Attention panel and
			// is not re-dispatched. The daemon-level "check your API key" alert is
			// deduped to once per provider per day so many affected beads do not
			// spam the event log / notifications.
			reason := fmt.Sprintf("Provider %s: authentication failed — check API key/credentials", outcome.AuthProvider)
			if err := d.db.MarkNeedsHuman(bead.ID, bead.Anvil, reason); err != nil {
				d.logger.Error("failed to mark bead as needs_human", "bead", bead.ID, "error", err)
			}
			d.recordDispatchFailure(bead.ID, bead.Anvil, reason, true)
			if d.firstAuthFailureToday(outcome.AuthProvider) {
				d.logger.Error("provider authentication failed — check API key/credentials",
					"provider", outcome.AuthProvider, "bead", bead.ID, "anvil", bead.Anvil)
				_ = d.db.LogEvent(state.EventAuthFailed, reason, bead.ID, bead.Anvil)
				if disp := d.dispatcher.Load(); disp != nil {
					go func(beadID, anvil, msg string) {
						notifCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
						defer cancel()
						disp.Dispatch(notifCtx, notify.EventBeadFailed, beadID, anvil, msg)
					}(bead.ID, bead.Anvil, reason)
				}
			}
			holdOff := d.cfg.Load().Settings.PollInterval
			if holdOff <= 0 {
				holdOff = DefaultPollInterval
			}
			d.logger.Warn("provider authentication failed — bead needs human attention", "bead", bead.ID, "provider", outcome.AuthProvider)
			select {
			case <-time.After(holdOff):
			case <-ctx.Done():
			}
			return
		}
		if outcome.NeedsHuman {
			// Warden hit max iterations — surface immediately in Needs Attention
			// rather than cycling through the circuit breaker.
			reason := fmt.Sprintf("Warden rejected after max iterations: %v", outcome.Error)
			if err := d.db.MarkNeedsHuman(bead.ID, bead.Anvil, reason); err != nil {
				d.logger.Error("failed to mark bead as needs_human", "bead", bead.ID, "error", err)
			}
			d.recordDispatchFailure(bead.ID, bead.Anvil, reason, true)
			holdOff := d.cfg.Load().Settings.PollInterval
			if holdOff <= 0 {
				holdOff = DefaultPollInterval
			}
			d.logger.Warn("warden exhausted max iterations — bead needs human attention", "bead", bead.ID)
			select {
			case <-time.After(holdOff):
			case <-ctx.Done():
			}
			return
		}
		d.logger.Error("pipeline error", "bead", bead.ID, "error", outcome.Error)
		d.recordDispatchFailure(bead.ID, bead.Anvil, fmt.Sprintf("pipeline error: %v", outcome.Error), true)
		return
	}

	if !outcome.Success {
		if outcome.EmptyDiff {
			// The branch has no commits vs its base — PR creation would fail and
			// re-running Smith would rebuild the same empty branch. Terminal:
			// resolved here without touching the circuit breaker.
			emptyCtx, emptyCancel := context.WithTimeout(context.Background(), executil.BdTimeout())
			defer emptyCancel()
			d.applyEmptyDiffOutcome(emptyCtx, bead, anvilCfg.Path, outcome)
			return
		}
		if outcome.NoChangesNeeded {
			// Smith determined no changes are needed — close the bead with the
			// reason instead of marking it as failed or needs_human.
			d.logger.Info("no changes needed — closing bead", "bead", bead.ID, "reason", outcome.NoChangesReason)
			closeCtx, closeCancel := context.WithTimeout(context.Background(), executil.BdTimeout())
			defer closeCancel()
			d.applyNoChangesNeededOutcome(closeCtx, bead, anvilCfg.Path, outcome.NoChangesReason)
			return
		}
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
			// Bead needs human attention — mark it immediately so it appears
			// in Hearth's Needs Attention panel without waiting for the
			// circuit breaker to trip after multiple failures.
			reason := needsHumanReason(outcome)

			if err := d.db.MarkNeedsHuman(bead.ID, bead.Anvil, reason); err != nil {
				d.logger.Error("failed to mark bead as needs_human", "bead", bead.ID, "error", err)
			}
			d.recordDispatchFailure(bead.ID, bead.Anvil, reason, true)
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
			d.recordDispatchFailure(bead.ID, bead.Anvil, fmt.Sprintf("pipeline failed: %s", outcome.Verdict), true)
		}
		return
	}

	// Pipeline succeeded — finalize (create PR, notify, close bead).
	d.finalizePipeline(pipelineCtx, outcome, bead, anvilCfg.Path, claimWorkerID)
}

// needsHumanReason selects the operator-facing reason to surface in the Needs
// Attention panel (via retries.last_error) when a pipeline releases a bead for
// human attention. The priority is most-specific-first: an explicit Smith
// escalation reflects a deliberate decision made after looking at the actual
// work, so it must win over the Schematic's decomposition rationale. The
// Schematic reason is the weakest signal — it nearly always exists (the
// Schematic ran and has an opinion), so it is both labelled ("Schematic: ")
// and ranked last so it can never again masquerade as a bare escalation.
func needsHumanReason(outcome *pipeline.Outcome) string {
	if outcome != nil {
		if outcome.SmithResult != nil {
			if r := pipeline.ExtractNeedsHuman(outcome.SmithResult.FullOutput); r != "" {
				return "Smith escalated: " + r
			}
		}
		if outcome.ReviewResult != nil && outcome.ReviewResult.NoDiff && outcome.ReviewResult.Summary != "" {
			return "Warden rejected (no diff): " + outcome.ReviewResult.Summary
		}
		if outcome.SchematicResult != nil && outcome.SchematicResult.Reason != "" {
			return "Schematic: " + outcome.SchematicResult.Reason
		}
	}
	return "Smith produced no diff, needs human attention"
}

// createPRBackoff returns the inline retry backoff for the end-of-pipeline
// CreatePR. It uses the production default unless a test has installed an
// override (typically a zero-delay backoff to avoid real sleeps).
func (d *Daemon) createPRBackoff() github.RetryBackoff {
	if d.prRetryBackoff != nil {
		return *d.prRetryBackoff
	}
	return github.DefaultRetryBackoff()
}

// finalizePipeline handles the post-success pipeline flow: create PR, clear
// retries, send notifications, and close the bead. Both the normal dispatch
// path and the force smith path call this to avoid duplicating PR creation logic.
func (d *Daemon) finalizePipeline(ctx context.Context, outcome *pipeline.Outcome, bead poller.Bead, anvilPath, workerID string) {
	d.logger.Info("pipeline succeeded", "bead", bead.ID, "branch", outcome.Branch, "iterations", outcome.Iterations)

	// Build the PR body's summary sections.
	// ChangeSummary is reserved for the author-written changelog fragment;
	// the warden verdict is routed to ReviewerNotes so it never leaks into
	// the '## Changes' section when no fragment exists.
	var changelogSummary, reviewerNotes string
	if outcome.ChangelogSummary != "" {
		changelogSummary = outcome.ChangelogSummary
	} else if outcome.ReviewResult != nil && outcome.ReviewResult.Summary != "" {
		reviewerNotes = outcome.ReviewResult.Summary
	}

	// Last-chance lookup: fetch the latest external_ref from bd in case it
	// was empty at dispatch time (e.g. GitHub auto-sync hadn't run yet).
	externalRef := bead.ExternalRef
	if externalRef == "" {
		externalRef = d.fetchExternalRef(anvilPath, bead.ID)
	}

	// Wrap CreatePR in transient-failure retry: a momentary gh/GitHub blip
	// (transient 401, rate-limited 403, 5xx, network) is retried with bounded
	// exponential backoff instead of immediately stranding the bead. Permanent
	// errors (422 validation, branch protection, 404) are NOT retried by the
	// classifier and fall straight through to the needs_human path below.
	var pr *vcs.PR
	err := github.RetryTransient(ctx, d.createPRBackoff(),
		func(attempt int, delay time.Duration, e error) {
			d.logger.Warn("PR creation transient failure, retrying",
				"bead", bead.ID, "attempt", attempt, "delay", delay, "error", e)
		},
		func() error {
			var e error
			pr, e = d.vcsForAnvil(bead.Anvil).CreatePR(ctx,
				d.buildPRCreateParams(bead, anvilPath, outcome.Branch, changelogSummary, reviewerNotes, externalRef))
			return e
		})
	if err != nil {
		// If a PR already exists for this branch (duplicate run), log a warning
		// and continue rather than failing — the work is already represented.
		if errors.Is(err, vcs.ErrPRAlreadyExists) {
			d.logger.Warn("PR already exists for branch, skipping creation", "bead", bead.ID, "branch", outcome.Branch, "error", err)
			if logErr := d.db.LogEvent(state.EventPRAlreadyExists, fmt.Sprintf("PR already exists for branch %s (duplicate run)", outcome.Branch), bead.ID, bead.Anvil); logErr != nil {
				d.logger.Error("failed to log duplicate PR event", "bead", bead.ID, "error", logErr)
			}
			// Register the existing PR in state.db so HasOpenPRForBead returns
			// true on the next orphan-recovery sweep. Without this, the bead
			// is reset to open and re-dispatched in a loop.
			d.registerExistingPRByBranch(ctx, bead.Anvil, anvilPath, bead.ID, outcome.Branch, bead.EpicBranch)
			// Update worker state so it doesn't hang in WorkerMonitoring
			// with no PR record for bellows to track.
			if dbErr := d.db.UpdateWorkerStatus(workerID, state.WorkerDone); dbErr != nil {
				d.logger.Error("failed to update worker status to done", "worker", workerID, "error", dbErr)
			}
			if dbErr := d.db.ClearRetry(bead.ID, bead.Anvil); dbErr != nil {
				d.logger.Error("failed to clear retry state", "bead", bead.ID, "error", dbErr)
			}
			return
		}

		d.logger.Error("PR creation failed", "bead", bead.ID, "error", err)
		if logErr := d.db.LogEvent(state.EventPRCreationFailed, fmt.Sprintf("PR creation failed: %v", err), bead.ID, bead.Anvil); logErr != nil {
			d.logger.Error("failed to log PR creation failure event", "bead", bead.ID, "error", logErr)
		}
		reason := fmt.Sprintf("PR creation failed: %v", err)
		if err := d.db.MarkNeedsHuman(bead.ID, bead.Anvil, reason); err != nil {
			d.logger.Error("failed to mark bead as needs_human", "bead", bead.ID, "error", err)
		}
		if dbErr := d.db.UpdateWorkerStatus(workerID, state.WorkerFailed); dbErr != nil {
			d.logger.Error("failed to update worker status to failed", "worker", workerID, "error", dbErr)
		}
		// Record the pushed branch, head SHA, and classified error on the ingot
		// so an operator can recover via `forge queue create-pr <id> --anvil <name>`
		// without re-running Smith. The work is committed and pushed; only the
		// final PR open failed.
		d.ingotRecordPRCreateFailed(bead.ID, bead.Anvil, outcome.Branch, d.localHeadSHA(ctx, anvilPath), err.Error())
		return
	}

	// Clear retry state only after PR creation succeeds, so that a PR
	// creation failure preserves the existing dispatch failure count.
	if dbErr := d.db.ClearRetry(bead.ID, bead.Anvil); dbErr != nil {
		d.logger.Error("failed to clear retry state", "bead", bead.ID, "error", dbErr)
	}
	d.logger.Info("PR created", "bead", bead.ID, "pr", pr.URL)

	d.ingotRecordPR(bead.ID, bead.Anvil, pr.Number, pr.URL)
	d.notifyWicketPRCreated(bead.ID, pr.URL, pr.Number)

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
		if n := d.notifier.Load(); n != nil {
			n.WorkerDone(notifCtx, anvil, beadID, workerID, dur)
		}
		if disp != nil {
			msg := fmt.Sprintf("Worker completed in %s; PR #%d created", dur.Round(time.Second), prNumber)
			disp.Dispatch(notifCtx, notify.EventWorkerDone, beadID, anvil, msg)
		}
	}(bead.Anvil, bead.ID, pr.URL, bead.Title, pr.Number, outcome.Duration)

	d.logger.Info("deferring close until PR merges", "bead", bead.ID, "pr", pr.Number, "pr_url", pr.URL)
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

// abortClaim handles a failed bead claim. It marks the pre-inserted pending
// worker as failed immediately (before any potentially-slow bd calls) so it
// stops counting toward dispatch capacity.
//
// claimErr drives whether the bead claim is released back to open:
//   - Timeout / context-canceled / signal-killed errors are non-atomic — the
//     server-side write may have landed before the client process died. For
//     these, releaseClaim=true reverts the bead to open so it self-heals via
//     `bd ready` instead of wedging as a phantom in_progress.
//   - Other errors (conflict, already in_progress, bd validation) mean the
//     claim almost certainly did NOT land. Releasing here would risk unassigning
//     a bead legitimately owned by another Forge instance or a human.
//
// See Forge-au4z.
func (d *Daemon) abortClaim(beadID, anvil, claimWorkerID, reason string, claimErr error) {
	if claimWorkerID != "" {
		if err := d.db.UpdateWorkerStatus(claimWorkerID, state.WorkerFailed); err != nil {
			d.logger.Warn("failed to mark pending worker failed after claim failure",
				"bead", beadID, "worker", claimWorkerID, "error", err)
		}
	}

	releaseClaim := isNonAtomicClaimFailure(claimErr)
	d.recordDispatchFailure(beadID, anvil, reason, releaseClaim)
}

// isNonAtomicClaimFailure returns true when a claimBead error could indicate
// that the server-side write landed before the client observed the error
// (timeout, context cancellation, signal kill). For these cases the bead
// claim should be released; for clean bd errors (conflict, validation) it
// should not.
func isNonAtomicClaimFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "signal: killed") || strings.Contains(msg, "signal: terminated")
}

// claimBead marks a bead as in_progress via bd update --claim.
func (d *Daemon) claimBead(ctx context.Context, beadID, anvilPath string) error {
	cmd, cancel := executil.BdCommand(ctx, "update", beadID, "--status=in_progress", "--json")
	defer cancel()
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

// applyNoChangesNeededOutcome handles the terminal case where Smith determined
// no changes are needed. It calls closeBead and, on success, clears the retry
// record and logs EventNoChangesNeeded. On failure it immediately marks the
// bead as needs_human so it surfaces in Hearth without waiting for the circuit
// breaker to trip.
//
// Before accepting NO_CHANGES_NEEDED as terminal, we check whether a forge
// branch for this bead exists on origin with commits ahead of main. This can
// happen when a prior dispatch pushed commits but failed before creating a PR
// (e.g. warden rejection). In that case Smith sees the work as already done
// and emits NO_CHANGES_NEEDED, but no PR has been opened. The daemon
// automatically creates the PR rather than flagging needs_human — recovering
// the common "last mile" failure. Only if auto PR creation itself fails does
// the bead escalate to needs_human.
func (d *Daemon) applyNoChangesNeededOutcome(ctx context.Context, bead poller.Bead, anvilPath, reason string) {
	if branch, ok := d.forgeBranchAheadOfMain(ctx, anvilPath, bead.ID, bead.EpicBranch); ok {
		d.logger.Info("orphaned branch detected with commits ahead of main and no PR — auto-creating PR",
			"bead", bead.ID, "branch", branch, "smith_reason", reason)
		_ = d.db.LogEvent(state.EventNoChangesNeeded,
			fmt.Sprintf("Orphaned branch %s detected on NO_CHANGES_NEEDED — attempting auto PR creation (Smith reason: %s)", branch, reason),
			bead.ID, bead.Anvil)

		// Use a dedicated longer timeout for PR creation — the original ctx
		// may carry the 30 s closeCtx deadline, which is one of the main
		// causes of the orphaned-branch scenario in the first place.
		prCtx, prCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer prCancel()
		// Last-chance external_ref lookup for orphaned branch PR creation.
		orphanExtRef := bead.ExternalRef
		if orphanExtRef == "" {
			orphanExtRef = d.fetchExternalRef(anvilPath, bead.ID)
		}
		pr, prErr := d.vcsForAnvil(bead.Anvil).CreatePR(prCtx, vcs.CreateParams{
			WorktreePath:    anvilPath,
			BeadID:          bead.ID,
			Title:           fmt.Sprintf("%s (%s)", bead.Title, bead.ID),
			Branch:          branch,
			Base:            bead.EpicBranch,
			AnvilName:       bead.Anvil,
			BeadTitle:       bead.Title,
			BeadDescription: bead.Description,
			BeadType:        bead.IssueType,
			ExternalRef:     orphanExtRef,
		})
		if prErr != nil {
			if errors.Is(prErr, vcs.ErrPRAlreadyExists) {
				d.logger.Warn("PR already exists for orphaned branch — skipping creation",
					"bead", bead.ID, "branch", branch)
				_ = d.db.LogEvent(state.EventPRAlreadyExists,
					fmt.Sprintf("PR already exists for orphaned branch %s (prior run)", branch),
					bead.ID, bead.Anvil)
				// Register the existing PR in state.db so HasOpenPRForBead
				// returns true on the next orphan-recovery sweep. Without
				// this, the bead is reset to open and re-dispatched, with
				// Smith repeatedly declaring NO_CHANGES_NEEDED in a loop.
				d.registerExistingPRByBranch(prCtx, bead.Anvil, anvilPath, bead.ID, branch, bead.EpicBranch)
				if clearErr := d.db.ClearRetry(bead.ID, bead.Anvil); clearErr != nil {
					d.logger.Error("failed to clear retry record after ErrPRAlreadyExists", "bead", bead.ID, "error", clearErr)
				}
				return
			}
			msg := fmt.Sprintf("NO_CHANGES_NEEDED orphaned branch %s — auto PR creation failed: %v", branch, prErr)
			d.logger.Error("auto PR creation failed for orphaned branch — escalating to needs_human",
				"bead", bead.ID, "branch", branch, "error", prErr)
			_ = d.db.LogEvent(state.EventPRCreationFailed, msg, bead.ID, bead.Anvil)
			if markErr := d.db.MarkNeedsHuman(bead.ID, bead.Anvil, msg); markErr != nil {
				d.logger.Error("failed to mark bead as needs_human", "bead", bead.ID, "error", markErr)
			}
			return
		}

		if pr == nil || pr.URL == "" || pr.Number == 0 {
			msg := fmt.Sprintf("NO_CHANGES_NEEDED orphaned branch %s — auto PR creation returned invalid PR object (nil or missing URL/Number)", branch)
			d.logger.Error("auto PR creation returned invalid PR object — escalating to needs_human",
				"bead", bead.ID, "branch", branch, "pr", pr)
			_ = d.db.LogEvent(state.EventError, msg, bead.ID, bead.Anvil)
			if markErr := d.db.MarkNeedsHuman(bead.ID, bead.Anvil, msg); markErr != nil {
				d.logger.Error("failed to mark bead as needs_human", "bead", bead.ID, "error", markErr)
			}
			return
		}

		d.logger.Info("auto-created PR for orphaned branch", "bead", bead.ID, "branch", branch, "pr", pr.URL)
		d.ingotRecordPR(bead.ID, bead.Anvil, pr.Number, pr.URL)
		d.notifyWicketPRCreated(bead.ID, pr.URL, pr.Number)
		_ = d.db.LogEvent(state.EventPRCreated,
			fmt.Sprintf("Auto-created PR for orphaned branch %s: %s (NO_CHANGES_NEEDED recovery)", branch, pr.URL),
			bead.ID, bead.Anvil)
		if clearErr := d.db.ClearRetry(bead.ID, bead.Anvil); clearErr != nil {
			d.logger.Error("failed to clear retry record after successful PR creation", "bead", bead.ID, "error", clearErr)
		}
		return
	}

	if err := d.closeBead(ctx, bead.ID, anvilPath, reason); err != nil {
		d.logger.Error("failed to close bead after no-changes-needed", "bead", bead.ID, "error", err)
		closeErr := fmt.Sprintf("no changes needed but close failed: %v", err)
		if markErr := d.db.MarkNeedsHuman(bead.ID, bead.Anvil, closeErr); markErr != nil {
			d.logger.Error("failed to mark bead as needs_human", "bead", bead.ID, "error", markErr)
		}
		d.recordDispatchFailure(bead.ID, bead.Anvil, closeErr, true)
	} else {
		_ = d.db.ClearRetry(bead.ID, bead.Anvil)
		_ = d.db.LogEvent(state.EventNoChangesNeeded,
			fmt.Sprintf("Bead closed — no changes needed: %s", reason),
			bead.ID, bead.Anvil)
	}
}

// applyEmptyDiffOutcome handles the terminal case where the pipeline reached
// Warden approval but the branch carries no commits against its base — the work
// is already on the base branch (typically a sibling PR shipped it first). PR
// creation is skipped by the pipeline; this decides what happens to the bead.
//
// Either way the outcome is deterministic: a re-dispatch rebuilds the identical
// empty branch. So the retry record is cleared rather than incremented — an
// empty branch must never feed the dispatch circuit breaker or schedule a retry.
// With empty_diff_action=close the bead is closed with a note; the default
// (attention) leaves it open and raises a Needs Attention entry for the
// operator. A failed auto-close falls back to needs-attention so the bead is
// never silently stranded in_progress.
func (d *Daemon) applyEmptyDiffOutcome(ctx context.Context, bead poller.Bead, anvilPath string, outcome *pipeline.Outcome) {
	base := outcome.EmptyDiffBase
	if base == "" {
		base = "the base branch"
	}
	branch := outcome.Branch
	if branch == "" {
		branch = worktree.BranchName(bead.ID)
	}
	note := fmt.Sprintf("No changes needed — branch %s has no commits vs %s; the work is already on the base branch.", branch, base)

	if err := d.db.ClearRetry(bead.ID, bead.Anvil); err != nil {
		d.logger.Error("failed to clear retry state after empty-diff outcome", "bead", bead.ID, "error", err)
	}

	if outcome.EmptyDiffAction == config.EmptyDiffActionClose {
		if err := d.closeBead(ctx, bead.ID, anvilPath, note); err != nil {
			reason := fmt.Sprintf("%s Auto-close failed: %v", note, err)
			d.logger.Error("failed to close bead after empty-diff outcome", "bead", bead.ID, "error", err)
			if markErr := d.db.MarkNeedsHuman(bead.ID, bead.Anvil, reason); markErr != nil {
				d.logger.Error("failed to mark bead as needs_human", "bead", bead.ID, "error", markErr)
			}
			_ = d.db.LogEvent(state.EventSmithEmptyResult, reason, bead.ID, bead.Anvil)
			d.releaseBeadClaim(bead.ID, bead.Anvil, false)
			return
		}
		d.logger.Info("empty branch — bead closed", "bead", bead.ID, "branch", branch, "base", base)
		_ = d.db.LogEvent(state.EventSmithEmptyResult, "Bead closed — "+note, bead.ID, bead.Anvil)
		return
	}

	d.logger.Warn("empty branch — bead needs attention", "bead", bead.ID, "branch", branch, "base", base)
	if err := d.db.MarkNeedsHuman(bead.ID, bead.Anvil, note); err != nil {
		d.logger.Error("failed to mark bead as needs_human", "bead", bead.ID, "error", err)
	}
	_ = d.db.LogEvent(state.EventSmithEmptyResult, "Needs attention — "+note, bead.ID, bead.Anvil)
	// Release the bd claim so the bead does not sit in_progress with no live
	// worker (which orphan recovery would later have to untangle). needs_human
	// keeps the poller from re-dispatching it in the meantime.
	d.releaseBeadClaim(bead.ID, bead.Anvil, false)
}

// preDispatchRemoteBranchCheck probes origin for the bead's forge branch
// before the pipeline runs. It returns true when dispatch should proceed and
// false when it must abort (the bead is either being handed off to bellows
// because an open PR already covers the branch, or it has been marked
// needs-attention because the branch carries stranded work from a prior
// worker).
//
// The three reachable outcomes:
//
//   - Absent: no branch on origin → proceed normally.
//   - Merged: branch on origin but fully reachable from the base ref → delete
//     the stale branch and proceed normally.
//   - Stranded: branch on origin with commits not reachable from the base ref
//     → cross-check for an existing PR. If a PR exists, log and let bellows
//     own it (return false to skip the pipeline). Otherwise mark needs_human
//     so the operator can decide between accept / reset / merge.
//
// An ls-remote / fetch error is conservative: dispatch proceeds so we don't
// stall the queue on a transient network blip. If the branch really is
// stranded, the prior end-of-pipeline Smith escalation still fires.
func (d *Daemon) preDispatchRemoteBranchCheck(ctx context.Context, bead poller.Bead, anvilPath string) bool {
	branch := worktree.BranchName(bead.ID)
	stateResult, info, err := worktree.CheckRemoteBranchState(ctx, anvilPath, branch, bead.EpicBranch)
	if err != nil {
		d.logger.Warn("pre-dispatch remote branch check failed; proceeding with dispatch",
			"bead", bead.ID, "branch", branch, "error", err)
		return true
	}

	switch stateResult {
	case worktree.RemoteBranchAbsent:
		return true

	case worktree.RemoteBranchMerged:
		d.logger.Info("pre-dispatch: deleting stale merged forge branch on origin",
			"bead", bead.ID, "branch", branch, "sha", info.SHA, "base", info.BaseRef)
		if delErr := worktree.DeleteRemoteBranch(ctx, anvilPath, branch); delErr != nil {
			// Non-fatal: a concurrent process may have deleted it, or the
			// remote may have temporarily refused the push. The next attempt
			// to push (during the pipeline) will surface a clearer error.
			d.logger.Warn("pre-dispatch: failed to delete stale merged branch; continuing dispatch",
				"bead", bead.ID, "branch", branch, "error", delErr)
		}
		return true

	case worktree.RemoteBranchStranded:
		// If a PR already exists for this branch, bellows owns it — never
		// duplicate a worker on top of a tracked PR. This may happen when a
		// prior dispatch created the PR but the daemon crashed before
		// recording it in state.db.
		pr, prErr := d.vcsForAnvil(bead.Anvil).GetPRByHeadBranch(ctx, anvilPath, branch)
		if prErr != nil {
			// A transient gh pr list failure must not cause a false needs_human
			// escalation. Log the error and abort this dispatch cycle; the next
			// poll will retry the PR check before deciding.
			d.logger.Warn("pre-dispatch: PR lookup failed for stranded branch; skipping dispatch until next poll",
				"bead", bead.ID, "branch", branch, "error", prErr)
			return false
		}
		if pr != nil {
			d.logger.Info("pre-dispatch: branch has an existing PR; deferring to bellows",
				"bead", bead.ID, "branch", branch, "pr", pr.Number)
			_ = d.db.LogEvent(state.EventPRAlreadyExists,
				fmt.Sprintf("Pre-dispatch: %s already has open PR #%d; dispatch skipped (bellows takes over)",
					branch, pr.Number),
				bead.ID, bead.Anvil)
			// Use registerPRIfUntracked directly with the already-fetched pr to
			// avoid a redundant GetPRByHeadBranch call inside registerExistingPRByBranch.
			d.registerPRIfUntracked(ctx, bead.Anvil, bead.ID, pr, bead.EpicBranch)
			return false
		}

		// No PR — the prior worker pushed but never opened one. Before
		// escalating to needs_human, check for a completion signal: a
		// changelog fragment for this bead (changelog.d/<bead-id>.md)
		// reachable from the branch tip. Forge requires a fragment per PR, so
		// its presence means the prior worker finished its work and merely
		// failed at the last mile (PR creation). In that case auto-open the PR
		// so bellows can drive it to merge — mirroring the orphaned-branch
		// recovery in applyNoChangesNeededOutcome. The Stranded classification
		// already guarantees the branch is ahead of base by >=1 commit, so no
		// separate ahead-count check is required.
		hasFragment, fragErr := d.branchHasChangelogFragment(ctx, anvilPath, info.SHA, bead.ID)
		if fragErr != nil {
			// Inconclusive probe — never auto-open a PR on a git error. Log and
			// fall through to the needs_human escalation below.
			d.logger.Warn("pre-dispatch: changelog-fragment completion check failed for stranded branch; escalating to needs_human",
				"bead", bead.ID, "branch", branch, "sha", info.SHA, "error", fragErr)
		} else if hasFragment {
			if d.recoverStrandedBranchPR(ctx, bead, anvilPath, branch, info.SHA) {
				return false
			}
			// Recovery failed (PR re-check error, CreatePR error, or invalid
			// PR object). Fall through to the needs_human escalation below so
			// the failure is never silently swallowed.
			d.logger.Warn("pre-dispatch: stranded-branch auto-recovery failed; escalating to needs_human",
				"bead", bead.ID, "branch", branch, "sha", info.SHA)
		}

		// Surface as needs-attention with a message modelled on the Smith
		// escalation from the 2026-05-18 Fhi.Metadata-orjp2 incident so the
		// operator's mental model is consistent regardless of when the check
		// fires.
		shortSHA := info.SHA
		if len(shortSHA) > 12 {
			shortSHA = shortSHA[:12]
		}
		reason := fmt.Sprintf(
			"origin/%s already has commits from a prior worker that were never opened as a PR. "+
				"A fresh Smith would produce a parallel implementation and a non-fast-forward push. "+
				"Resolving this requires a human decision (accept the prior work and open a PR, "+
				"reset the remote branch, or merge with new work). SHA: %s",
			branch, shortSHA,
		)
		d.logger.Warn("pre-dispatch: stranded forge branch on origin — escalating to needs_human",
			"bead", bead.ID, "branch", branch, "sha", info.SHA, "base", info.BaseRef)
		_ = d.db.LogEvent(state.EventDispatchBlockedStrandedBranch, reason, bead.ID, bead.Anvil)
		if markErr := d.db.MarkNeedsHuman(bead.ID, bead.Anvil, reason); markErr != nil {
			d.logger.Error("pre-dispatch: failed to mark bead as needs_human", "bead", bead.ID, "error", markErr)
		}
		d.recordDispatchFailure(bead.ID, bead.Anvil, reason, true)

		// Fire bead-failed notifications immediately. Unlike the normal retry
		// path, the stranded-branch case is a 1-strike escalation: needs_human
		// has already been set above, so we should not wait for the circuit
		// breaker to trip before alerting the operator. Mirrors the async
		// notification block in recordDispatchFailure's circuit-break branch.
		disp := d.dispatcher.Load()
		notifier := d.notifier.Load()
		if disp != nil || notifier != nil {
			beadID, anvil := bead.ID, bead.Anvil
			go func(reason string) {
				notifCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if notifier != nil {
					notifier.BeadFailed(notifCtx, anvil, beadID, 1, reason)
				}
				if disp != nil {
					disp.Dispatch(notifCtx, notify.EventBeadFailed, beadID, anvil, reason)
				}
			}(reason)
		}
		return false
	}

	return true
}

// branchHasChangelogFragment reports whether a changelog fragment for beadID
// (changelog.d/<bead-id>.md or the language-split <bead-id>.<lang>.md) is
// reachable from the given commit SHA. Forge
// requires a fragment per PR, so its presence on a stranded forge branch is a
// completion signal: the prior worker finished its work and merely failed to
// open a PR. The SHA's tree is already local because CheckRemoteBranchState
// fetched the branch before this is called.
//
// It returns (found, nil) on a successful tree read and (false, err) when the
// git command itself fails, so callers can distinguish "no fragment" (a clean
// negative) from "git error" (inconclusive) and avoid auto-opening a PR on an
// indeterminate probe.
func (d *Daemon) branchHasChangelogFragment(ctx context.Context, anvilPath, sha, beadID string) (bool, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "git", "ls-tree", "-r", "--name-only", sha, "--", "changelog.d/"))
	cmd.Dir = anvilPath
	cmd.Env = executil.CleanGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if changelogFragmentMatches(line, beadID) {
			return true, nil
		}
	}
	return false, nil
}

// changelogFragmentMatches reports whether a changelog.d/ path is a changelog
// fragment for beadID. It accepts both the single-file form
// (changelog.d/<bead>.md) and the language-split form some repos use — e.g.
// Munin's changelog.d/<bead>.en.md + <bead>.nb.md. Matching only the .md form
// made recoverStrandedBranchPR/openPRForExistingBranch treat completed
// language-split work as incomplete, stranding it in needs_human
// (Fhi.Metadata-15ed9). It does NOT match a different bead whose id shares a
// prefix (e.g. <bead>1.md), by requiring the extra segment to be dot-delimited.
func changelogFragmentMatches(path, beadID string) bool {
	const dir = "changelog.d/"
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, dir) || !strings.HasSuffix(path, ".md") {
		return false
	}
	stem := strings.TrimSuffix(strings.TrimPrefix(path, dir), ".md")
	return stem == beadID || strings.HasPrefix(stem, beadID+".")
}

// recoverStrandedBranchPR auto-opens a PR for a stranded forge branch that
// carries a completion signal (a changelog fragment for the bead). It first
// re-checks for an existing PR to avoid racing a concurrent open, then mirrors
// the normal end-of-pipeline CreatePR. On success it registers the PR so
// bellows owns it, logs EventDispatchRecoveredStrandedBranch, clears the retry
// record, and returns true so the caller skips dispatch. It returns false on
// any failure (PR re-check error, CreatePR error, or invalid PR object) so the
// caller falls through to the needs_human escalation rather than silently
// swallowing the error.
func (d *Daemon) recoverStrandedBranchPR(ctx context.Context, bead poller.Bead, anvilPath, branch, sha string) bool {
	provider := d.vcsForAnvil(bead.Anvil)

	// Fresh guard: a concurrent dispatch (or bellows) may have opened a PR
	// between the earlier lookup and now. Never duplicate a tracked PR.
	if pr, err := provider.GetPRByHeadBranch(ctx, anvilPath, branch); err != nil {
		d.logger.Warn("pre-dispatch: PR re-check failed before stranded-branch recovery",
			"bead", bead.ID, "branch", branch, "error", err)
		return false
	} else if pr != nil {
		d.logger.Info("pre-dispatch: PR opened concurrently for stranded branch; deferring to bellows",
			"bead", bead.ID, "branch", branch, "pr", pr.Number)
		d.registerPRIfUntracked(ctx, bead.Anvil, bead.ID, pr, bead.EpicBranch)
		if clearErr := d.db.ClearRetry(bead.ID, bead.Anvil); clearErr != nil {
			d.logger.Error("failed to clear retry record after concurrent PR discovery on stranded recovery", "bead", bead.ID, "error", clearErr)
		}
		return true
	}

	// Use a dedicated timeout for PR creation independent of the caller ctx —
	// the original ctx may carry a short poll deadline.
	prCtx, prCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer prCancel()

	// Last-chance external_ref lookup in case it was empty at dispatch time.
	externalRef := bead.ExternalRef
	if externalRef == "" {
		externalRef = d.fetchExternalRef(anvilPath, bead.ID)
	}

	pr, err := provider.CreatePR(prCtx,
		d.buildPRCreateParams(bead, anvilPath, branch, "", "", externalRef))
	if err != nil {
		if errors.Is(err, vcs.ErrPRAlreadyExists) {
			// Lost a race with a concurrent open between the guard and the
			// create — register the existing PR and defer to bellows.
			d.logger.Info("pre-dispatch: PR already exists on stranded-branch recovery create; registering and deferring to bellows",
				"bead", bead.ID, "branch", branch)
			d.registerExistingPRByBranch(prCtx, bead.Anvil, anvilPath, bead.ID, branch, bead.EpicBranch)
			if clearErr := d.db.ClearRetry(bead.ID, bead.Anvil); clearErr != nil {
				d.logger.Error("failed to clear retry record after ErrPRAlreadyExists on stranded recovery", "bead", bead.ID, "error", clearErr)
			}
			return true
		}
		d.logger.Error("pre-dispatch: auto PR creation failed for stranded branch",
			"bead", bead.ID, "branch", branch, "error", err)
		_ = d.db.LogEvent(state.EventPRCreationFailed,
			fmt.Sprintf("Pre-dispatch: auto PR creation failed for stranded branch %s: %v", branch, err),
			bead.ID, bead.Anvil)
		return false
	}
	if pr == nil || pr.URL == "" || pr.Number == 0 {
		d.logger.Error("pre-dispatch: auto PR creation returned invalid PR object for stranded branch",
			"bead", bead.ID, "branch", branch, "pr", pr)
		return false
	}

	// Register so bellows owns the PR and drives it to merge. registerPRIfUntracked
	// is idempotent (CreatePR already records the PR), so this is a safety net.
	d.registerPRIfUntracked(ctx, bead.Anvil, bead.ID, &vcs.OpenPR{
		Number: pr.Number,
		Title:  pr.Title,
		Branch: branch,
	}, bead.EpicBranch)

	shortSHA := sha
	if len(shortSHA) > 12 {
		shortSHA = shortSHA[:12]
	}
	d.logger.Info("pre-dispatch: auto-created PR for stranded forge branch with completion signal",
		"bead", bead.ID, "branch", branch, "pr", pr.URL, "sha", shortSHA)
	d.ingotRecordPR(bead.ID, bead.Anvil, pr.Number, pr.URL)
	d.notifyWicketPRCreated(bead.ID, pr.URL, pr.Number)
	_ = d.db.LogEvent(state.EventDispatchRecoveredStrandedBranch,
		fmt.Sprintf("Pre-dispatch: stranded branch %s had a changelog fragment (completion signal); auto-created PR #%d: %s (SHA %s)",
			branch, pr.Number, pr.URL, shortSHA),
		bead.ID, bead.Anvil)
	if clearErr := d.db.ClearRetry(bead.ID, bead.Anvil); clearErr != nil {
		d.logger.Error("failed to clear retry record after stranded-branch recovery PR creation", "bead", bead.ID, "error", clearErr)
	}
	return true
}

// localHeadSHA returns the tip commit SHA of the given worktree (HEAD), or "" on
// any error. It is a best-effort helper used to record the pushed head SHA when
// PR creation fails — a missing SHA must never block the failure-recording path.
func (d *Daemon) localHeadSHA(ctx context.Context, worktreePath string) string {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "git", "rev-parse", "HEAD"))
	cmd.Dir = worktreePath
	cmd.Env = executil.CleanGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// clearNeedsHumanAfterRecovery clears the needs_human escalation and the stale
// ingot PR-creation error after a successful create-PR-from-existing-branch
// recovery. Errors are logged but not surfaced — the recovery itself succeeded.
func (d *Daemon) clearNeedsHumanAfterRecovery(beadID, anvil string) {
	if err := d.db.ClearRetry(beadID, anvil); err != nil {
		d.logger.Error("create-pr: failed to clear retry/needs_human after recovery", "bead", beadID, "anvil", anvil, "error", err)
	}
	d.ingotClearPRCreateError(beadID, anvil)
}

// openPRForExistingBranch opens a PR for an already-pushed forge branch WITHOUT
// re-running Smith. It is the shared recovery primitive behind the manual
// `forge queue create-pr <id> --anvil <name>` command (and, via the same IPC
// path, the Hearth "Create PR" button). It reuses the pushed branch, its head
// SHA, and the changelog fragment already committed to that branch, then builds
// the same CreateParams the normal pipeline uses (so the PR body cannot drift)
// and registers the resulting PR so bellows drives it to merge.
//
// Preconditions, each surfaced as a distinct error so an operator understands
// why a recovery was refused:
//   - origin/forge/<bead> exists,
//   - it is ahead of the base branch (carries un-merged commits),
//   - no open PR already targets it (an existing PR is registered and treated as
//     a successful recovery rather than an error),
//   - its tip carries the bead's changelog fragment (the per-PR completion
//     signal Forge requires), proving the prior worker finished its work.
//
// On success it clears needs_human, logs EventPRCreateRecovered, and returns the
// PR number and (when known) the PR's web URL. On failure it surfaces the gh
// error and leaves needs_human set so the bead remains in the operator's
// needs-attention view. The URL is returned so callers (the Hearth "Create PR"
// button) can render a clickable link; it is empty for the already-open / raced
// recovery paths where only the PR number is available.
func (d *Daemon) openPRForExistingBranch(ctx context.Context, beadID, anvilName string) (int, string, error) {
	if beadID == "" || anvilName == "" {
		return 0, "", fmt.Errorf("bead_id and anvil are required")
	}
	anvilCfg, ok := d.cfg.Load().Anvils[anvilName]
	if !ok {
		return 0, "", fmt.Errorf("anvil %q not found", anvilName)
	}
	anvilPath := anvilCfg.Path
	if anvilPath == "" {
		return 0, "", fmt.Errorf("anvil %q has no path configured", anvilName)
	}

	branch := worktree.BranchName(beadID)
	// The bead_id used to register the PR is derived from the branch so it stays
	// consistent with the rest of the daemon's branch→bead recovery (ext-<number>
	// fallback handled by registerPRIfUntracked when no forge branch maps back).
	registerID := beadID
	if derived, ok := worktree.BeadIDFromBranch(branch); ok {
		registerID = derived
	}

	// Fetch bead metadata for the PR title/body. The worktree is typically gone
	// by recovery time, so the bead is the only source for title/description.
	fetchBead := d.beadFetcher
	if fetchBead == nil {
		fetchBead = crucible.FetchBead
	}
	bead, err := fetchBead(ctx, beadID, anvilPath)
	if err != nil {
		return 0, "", fmt.Errorf("bd show %s: %w", beadID, err)
	}
	bead.Anvil = anvilName

	// Resolve the epic branch so a Crucible child's recovered PR targets the
	// feature branch rather than the repo default. FetchBead does not populate
	// EpicBranch (it is json:"-" and normally filled by poller.ResolveEpicBranches),
	// so we resolve it explicitly here — mirroring the force_smith/warden_rerun
	// flows. Every downstream use below (base-branch precondition check, CreateParams,
	// and PR registration) reads bead.EpicBranch, so this must run before them.
	beads := []poller.Bead{bead}
	poller.ResolveEpicBranches(ctx, beads, map[string]string{anvilName: anvilPath})
	bead.EpicBranch = beads[0].EpicBranch

	// Precondition: origin/<branch> exists and is ahead of base (stranded).
	branchState, info, err := worktree.CheckRemoteBranchState(ctx, anvilPath, branch, bead.EpicBranch)
	if err != nil {
		return 0, "", fmt.Errorf("checking remote branch %s: %w", branch, err)
	}
	switch branchState {
	case worktree.RemoteBranchAbsent:
		return 0, "", fmt.Errorf("origin/%s does not exist; nothing to open a PR for", branch)
	case worktree.RemoteBranchMerged:
		return 0, "", fmt.Errorf("origin/%s is already merged into the base branch; nothing to open", branch)
	}

	provider := d.vcsForAnvil(anvilName)

	// Precondition: no open PR already. If one exists, register it (idempotent)
	// and treat this as a successful recovery — the goal state is reached.
	if pr, perr := provider.GetPRByHeadBranch(ctx, anvilPath, branch); perr != nil {
		return 0, "", fmt.Errorf("looking up existing PR for %s: %w", branch, perr)
	} else if pr != nil {
		d.registerPRIfUntracked(ctx, anvilName, registerID, pr, bead.EpicBranch)
		d.clearNeedsHumanAfterRecovery(beadID, anvilName)
		d.ingotRecordPR(beadID, anvilName, pr.Number, "")
		d.ingotClearPRCreateError(beadID, anvilName)
		_ = d.db.LogEvent(state.EventPRCreateRecovered,
			fmt.Sprintf("create-pr: %s already has open PR #%d; registered and cleared needs_human", branch, pr.Number),
			beadID, anvilName)
		return pr.Number, "", nil
	}

	// Precondition: branch tip carries the bead's changelog fragment.
	hasFragment, fragErr := d.branchHasChangelogFragment(ctx, anvilPath, info.SHA, beadID)
	if fragErr != nil {
		return 0, "", fmt.Errorf("checking changelog fragment on %s: %w", branch, fragErr)
	}
	if !hasFragment {
		return 0, "", fmt.Errorf("origin/%s does not carry a changelog fragment (changelog.d/%s.md or %s.<lang>.md); refusing to open a PR for incomplete work", branch, beadID, beadID)
	}

	// Last-chance external_ref lookup in case it was empty in the bead record.
	externalRef := bead.ExternalRef
	if externalRef == "" {
		externalRef = d.fetchExternalRef(anvilPath, bead.ID)
	}

	// Dedicated timeout for PR creation independent of the caller ctx, which may
	// carry a short IPC deadline.
	prCtx, prCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer prCancel()

	pr, err := provider.CreatePR(prCtx,
		d.buildPRCreateParams(bead, anvilPath, branch, "", "", externalRef))
	if err != nil {
		if errors.Is(err, vcs.ErrPRAlreadyExists) {
			// Raced a concurrent open between the guard and the create — register
			// the existing PR and treat as recovered.
			n := d.registerExistingPRByBranch(prCtx, anvilName, anvilPath, registerID, branch, bead.EpicBranch)
			if n == 0 {
				_ = d.db.LogEvent(state.EventPRCreationFailed,
					fmt.Sprintf("create-pr: PR already exists for %s but could not locate it to register", branch),
					beadID, anvilName)
				return 0, "", fmt.Errorf("PR already exists for branch %s but could not be located", branch)
			}
			d.clearNeedsHumanAfterRecovery(beadID, anvilName)
			d.ingotRecordPR(beadID, anvilName, n, "")
			d.ingotClearPRCreateError(beadID, anvilName)
			_ = d.db.LogEvent(state.EventPRCreateRecovered,
				fmt.Sprintf("create-pr: PR already existed for %s on create; registered #%d and cleared needs_human", branch, n),
				beadID, anvilName)
			return n, "", nil
		}
		// Surface the gh error and leave needs_human set.
		_ = d.db.LogEvent(state.EventPRCreationFailed,
			fmt.Sprintf("create-pr: PR creation failed for branch %s: %v", branch, err), beadID, anvilName)
		d.ingotRecordPRCreateFailed(beadID, anvilName, branch, info.SHA, err.Error())
		return 0, "", fmt.Errorf("gh pr create for %s failed: %w", branch, err)
	}
	if pr == nil || pr.URL == "" || pr.Number == 0 {
		return 0, "", fmt.Errorf("PR creation for %s returned an invalid PR object", branch)
	}

	// Register so bellows owns the PR. registerPRIfUntracked is idempotent
	// (CreatePR already records the PR), so this is a safety net.
	d.registerPRIfUntracked(prCtx, anvilName, registerID, &vcs.OpenPR{
		Number: pr.Number,
		Title:  pr.Title,
		Branch: branch,
	}, bead.EpicBranch)

	d.ingotRecordPR(beadID, anvilName, pr.Number, pr.URL)
	d.notifyWicketPRCreated(beadID, pr.URL, pr.Number)
	d.clearNeedsHumanAfterRecovery(beadID, anvilName)
	d.logger.Info("create-pr: opened PR for existing forge branch",
		"bead", beadID, "anvil", anvilName, "branch", branch, "pr", pr.URL)
	_ = d.db.LogEvent(state.EventPRCreateRecovered,
		fmt.Sprintf("create-pr: opened PR #%d for existing branch %s: %s", pr.Number, branch, pr.URL),
		beadID, anvilName)
	return pr.Number, pr.URL, nil
}

// forgeBranchAheadOfMain checks whether the origin remote has a forge branch
// for the given bead that contains commits not yet merged into the base branch.
// When epicBranch is set (crucible child beads), it is used as the base instead
// of origin/main or origin/master, so crucible children are not misclassified as
// orphaned when they are already merged into the epic branch. It returns the
// branch name and true when such unmerged commits exist, signalling that work was
// pushed in a prior dispatch but no PR was created.
func (d *Daemon) forgeBranchAheadOfMain(ctx context.Context, anvilPath, beadID, epicBranch string) (string, bool) {
	branchName := worktree.BranchName(beadID)

	gitEnv := executil.CleanGitEnv()

	// ls-remote is a lightweight ref-only query — no object transfer.
	lsCmd := executil.HideWindow(exec.CommandContext(ctx, "git", "ls-remote", "--heads", "origin", "--", branchName))
	lsCmd.Dir = anvilPath
	lsCmd.Env = gitEnv
	lsOut, err := lsCmd.Output()
	if err != nil {
		// Treat ls-remote failures as inconclusive and fail closed to avoid
		// silently discarding potential remote work. The caller should behave
		// conservatively when this returns true.
		d.logger.Error("git ls-remote failed for forge branch ahead-of-main check; treating as potential unmerged work",
			"bead", beadID, "branch", branchName, "error", err)
		return branchName, true
	}
	if len(strings.TrimSpace(string(lsOut))) == 0 {
		return "", false // branch does not exist on remote
	}

	// Branch exists on origin. Fetch it to update the local remote-tracking ref
	// so that "git log origin/<branch>" reflects the latest state.
	if err := d.worktreeMgr.FetchBranch(ctx, anvilPath, branchName); err != nil {
		// Branch confirmed on origin but fetch failed (lock contention, network error, etc.).
		// Fail closed to avoid discarding known remote work.
		d.logger.Warn("could not fetch forge branch for ahead-of-main check; treating as potential unmerged work",
			"bead", beadID, "branch", branchName, "error", err)
		return branchName, true
	}

	// Determine the base ref. Priority order:
	//   1. epicBranch (origin/<epicBranch>) — for crucible children, check against
	//      the actual PR base rather than main to avoid misclassifying merged children.
	//   2. FORGE_BASE_REF env override.
	//   3. origin/main or origin/master.
	baseRef := ""

	if epicBranch != "" {
		epicRef := "origin/" + epicBranch
		verifyCmd := executil.HideWindow(exec.CommandContext(ctx, "git", "rev-parse", "--verify", epicRef))
		verifyCmd.Dir = anvilPath
		verifyCmd.Env = gitEnv
		if verifyCmd.Run() == nil {
			baseRef = epicRef
		}
	}

	if baseRef == "" {
		if explicit := os.Getenv("FORGE_BASE_REF"); explicit != "" {
			verifyCmd := executil.HideWindow(exec.CommandContext(ctx, "git", "rev-parse", "--verify", explicit))
			verifyCmd.Dir = anvilPath
			verifyCmd.Env = gitEnv
			if verifyCmd.Run() == nil {
				baseRef = explicit
			}
		}
	}

	if baseRef == "" {
		for _, candidate := range []string{"origin/main", "origin/master"} {
			verifyCmd := executil.HideWindow(exec.CommandContext(ctx, "git", "rev-parse", "--verify", candidate))
			verifyCmd.Dir = anvilPath
			verifyCmd.Env = gitEnv
			if verifyCmd.Run() == nil {
				baseRef = candidate
				break
			}
		}
	}
	if baseRef == "" {
		d.logger.Warn("cannot determine base ref (origin/main or origin/master) for ahead-of-main check", "bead", beadID)
		return "", false
	}

	// List commits on the forge branch that are not reachable from the base.
	logCmd := executil.HideWindow(exec.CommandContext(ctx, "git", "log", "--oneline",
		"origin/"+branchName, "--not", baseRef))
	logCmd.Dir = anvilPath
	logCmd.Env = gitEnv
	logOut, err := logCmd.Output()
	if err != nil {
		// git log failed after confirming the branch exists — fail closed.
		d.logger.Warn("git log failed for forge branch ahead-of-main check; treating as potential unmerged work",
			"bead", beadID, "branch", branchName, "error", err)
		return branchName, true
	}

	if len(strings.TrimSpace(string(logOut))) > 0 {
		return branchName, true
	}
	return "", false
}

// closeBead marks a bead as closed via bd close. It routes through the
// injectable beadCloser so tests can simulate the transient dolt/beads
// failures the close-after-merge retry path exists to survive.
func (d *Daemon) closeBead(ctx context.Context, beadID, anvilPath, reason string) error {
	if d.beadCloser != nil {
		return d.beadCloser(ctx, beadID, anvilPath, reason)
	}
	return execBdClose(ctx, beadID, anvilPath, reason)
}

// execBdClose is the real `bd close` invocation.
func execBdClose(ctx context.Context, beadID, anvilPath, reason string) error {
	cmd, cancel := executil.BdCommand(ctx, "close", beadID, "--reason="+reason, "--json")
	defer cancel()
	cmd.Dir = anvilPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bd close %s --json: %w\n%s", beadID, err, out)
	}
	return nil
}

// killWorkerProcess performs a 2-phase kill (SIGINT → wait → SIGKILL) for the
// worker identified by workerID. The PID is looked up from state.db — the
// client-supplied PID is never trusted. On Unix the entire process group is
// signaled so child processes (git, node, etc.) are also terminated.
//
// On Windows, process-group signaling is not supported; CTRL_BREAK is sent to
// the direct process only. Child processes may survive — full process-tree
// termination requires job objects, which are not currently implemented.
func (d *Daemon) killWorkerProcess(workerID string) error {
	const gracePeriod = 5 * time.Second
	const pollInterval = 100 * time.Millisecond

	worker, err := d.db.GetWorker(workerID)
	if err != nil {
		return fmt.Errorf("kill_worker: %w", err)
	}
	pid := worker.PID
	if pid <= 0 {
		// No live PID recorded; just mark the worker as failed.
		return d.db.UpdateWorkerStatus(workerID, state.WorkerFailed)
	}

	if runtime.GOOS == "windows" {
		// Phase 1: request graceful exit via CTRL_BREAK.
		proc, findErr := os.FindProcess(pid)
		if findErr != nil {
			// Process not found; mark as failed.
			return d.db.UpdateWorkerStatus(workerID, state.WorkerFailed)
		}
		_ = proc.Signal(os.Interrupt)
		time.Sleep(gracePeriod)
		// Phase 2: force kill.
		d.logger.Warn("worker did not exit after interrupt, sending Kill", "pid", pid, "worker", workerID)
		_ = proc.Kill()
		time.Sleep(200 * time.Millisecond)
		return d.db.UpdateWorkerStatus(workerID, state.WorkerFailed)
	}

	// Resolve the process group ID. If the worker was started with Setpgid,
	// pgid == pid. For older workers or recycled PIDs, Getpgid may return a
	// different value; we verify pgid == pid before using group signaling.
	// On Windows, pgidKnown is always false — see killgroup_windows.go.
	pgid, pgidKnown := processGroup(pid)

	// Phase 1: Send an interrupt to the process group (if known) or the PID
	// directly. signalInterrupt is platform-specific: SIGINT on Unix,
	// CTRL_BREAK_EVENT via os.Process.Signal(os.Interrupt) on Windows.
	signalInterrupt(pid, pgid, pgidKnown)

	// Phase 2: Wait up to gracePeriod for the process to exit.
	deadline := time.Now().Add(gracePeriod)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			// Process is gone.
			if dbErr := d.db.UpdateWorkerStatus(workerID, state.WorkerFailed); dbErr != nil {
				d.logger.Error("failed to update worker status after kill", "worker", workerID, "error", dbErr)
				return dbErr
			}
			return nil
		}
		time.Sleep(pollInterval)
	}

	// Phase 3: Still alive — force-kill. On Unix this is SIGKILL to the
	// process group; on Windows it is TerminateProcess on the individual PID.
	d.logger.Warn("worker did not exit after interrupt, force-killing", "pid", pid, "worker", workerID)
	signalKill(pid, pgid, pgidKnown)

	// Poll to verify the process actually exited after the kill.
	const killVerifyTimeout = 2 * time.Second
	killDeadline := time.Now().Add(killVerifyTimeout)
	for time.Now().Before(killDeadline) {
		if !processAlive(pid) {
			break // Process is gone.
		}
		time.Sleep(pollInterval)
	}
	if processAlive(pid) {
		d.logger.Error("worker process still alive after force-kill", "pid", pid, "worker", workerID)
		if dbErr := d.db.UpdateWorkerStatus(workerID, state.WorkerFailed); dbErr != nil {
			d.logger.Error("failed to update worker status after kill", "worker", workerID, "error", dbErr)
		}
		return fmt.Errorf("worker process %d still alive after force-kill", pid)
	}
	if dbErr := d.db.UpdateWorkerStatus(workerID, state.WorkerFailed); dbErr != nil {
		d.logger.Error("failed to update worker status after kill", "worker", workerID, "error", dbErr)
		return dbErr
	}
	return nil
}

// handleIPC processes incoming IPC commands from CLI/TUI clients.
func (d *Daemon) handleIPC(cmd ipc.Command) ipc.Response {
	switch cmd.Type {
	case "ping":
		// Lightweight liveness probe used by ipc.Ping()/IsRunning(). Answers
		// immediately without touching the DB or shelling out, so it stays
		// cheap even under load and makes the socket the authoritative
		// liveness signal for status/pause/resume.
		data, _ := json.Marshal(map[string]string{"message": "pong"})
		return ipc.Response{Type: "pong", Payload: data}
	case "status":
		workers, _ := d.db.ActiveWorkers()
		prs, _ := d.db.OpenPRs()
		quotas, _ := d.db.GetAllProviderQuotas()
		todayCost, _ := d.db.GetTodayCost()
		costLimit := d.cfg.Load().Settings.DailyCostLimit
		// Reserved in-flight spend and projected total (recorded + reserved) so
		// `forge status` can show why dispatch paused before recorded cost alone
		// reaches the limit (Forge-s3w7).
		reservedCost := d.totalReservedCost()
		projectedCost := todayCost + reservedCost
		copilotReqs, _ := d.db.GetTodayCopilotRequests()
		copilotLimit := d.cfg.Load().Settings.CopilotDailyRequestLimit
		queueCount, _ := d.db.QueueCount()
		lastPoll := "n/a"
		if t, ok := d.lastPollTime.Load().(time.Time); ok && !t.IsZero() {
			lastPoll = time.Since(t).Round(time.Second).String() + " ago"
		}
		// Project the in-memory per-anvil last-poll snapshot into the IPC
		// payload. Sorted by anvil name so the IPC output is deterministic
		// and easy to consume from Hearth / Mezzanine / scripts.
		var anvilLastPoll []ipc.AnvilPollItem
		if snaps := d.anvilPollSnapshots(); len(snaps) > 0 {
			names := make([]string, 0, len(snaps))
			for name := range snaps {
				names = append(names, name)
			}
			sort.Strings(names)
			anvilLastPoll = make([]ipc.AnvilPollItem, 0, len(names))
			for _, name := range names {
				s := snaps[name]
				anvilLastPoll = append(anvilLastPoll, ipc.AnvilPollItem{
					Anvil:     name,
					Timestamp: s.Timestamp,
					OK:        s.OK,
					Message:   s.Message,
				})
			}
		}
		// Wedged anvils (beads database mid-merge with unresolved conflicts).
		// Empty for a healthy forge, so the field is absent from the payload.
		var wedgedAnvils []ipc.WedgedAnvilItem
		if rows, err := d.db.WedgedAnvils(); err != nil {
			d.logger.Debug("status: wedged-anvil lookup failed", "error", err)
		} else {
			for _, r := range rows {
				wedgedAnvils = append(wedgedAnvils, ipc.WedgedAnvilItem{
					Anvil:           r.Anvil,
					ConflictTables:  r.ConflictTables,
					ConflictCount:   r.ConflictCount,
					Branch:          r.Branch,
					Ahead:           r.Ahead,
					Behind:          r.Behind,
					DivergenceKnown: r.DivergenceKnown,
					Detail:          r.Detail,
					DetectedAt:      r.DetectedAt,
				})
			}
		}
		payload := ipc.StatusPayload{
			Running:        true,
			PID:            os.Getpid(),
			Uptime:         time.Since(d.startTime).Round(time.Second).String(),
			Workers:        len(workers),
			QueueSize:      queueCount,
			OpenPRs:        len(prs),
			LastPoll:       lastPoll,
			Quotas:         quotas,
			DailyCost:      todayCost,
			DailyCostLimit: costLimit,
			ReservedCost:   reservedCost,
			// Paused once the projected total (recorded + in-flight reserve)
			// reaches the limit — matching the poll-loop gate, which also adds
			// one per-worker estimate, so status may briefly show paused=false
			// while the gate blocks the very next dispatch. Close enough for a
			// human-facing status line (Forge-s3w7).
			CostLimitPaused:        costLimit > 0 && projectedCost >= costLimit,
			CopilotPremiumRequests: copilotReqs,
			CopilotRequestLimit:    copilotLimit,
			CopilotLimitReached:    copilotLimit > 0 && copilotReqs >= float64(copilotLimit),
			AnvilLastPoll:          anvilLastPoll,
			WedgedAnvils:           wedgedAnvils,
			MaxTotalSmiths:         d.cfg.Load().Settings.MaxTotalSmiths,
		}
		// Report the pause with its cause, so a self-deploy drain does not read
		// as an operator action. The boolean stays authoritative for clients
		// that only know it.
		pause := d.dispatchPauseState()
		payload.DispatchPaused = pause.Paused
		payload.DispatchPauseReason = string(pause.Reason)
		payload.DispatchPauseDetail = pause.Detail
		// The drain's held-up worker count is a live number, so derive it here
		// rather than freezing it into Detail when the pause was taken. It comes
		// from the same set the drain itself waits on (which includes paused
		// workers, unlike the active-worker count above) so status agrees with
		// what is actually holding the deploy up.
		if pause.Paused && pause.Reason == PauseReasonSelfDeploy {
			draining, err := d.activeWorkerIDs()
			if err != nil {
				d.logger.Debug("status: drain worker lookup failed", "error", err)
			}
			if n := len(draining); n > 0 {
				waiting := fmt.Sprintf("waiting on %d worker%s", n, pluralS(n))
				if pause.Detail != "" {
					waiting += ", " + pause.Detail
				}
				payload.DispatchPauseDetail = waiting
			}
		}
		// Surface when the manual dispatch pause began (not the cost-limit pause).
		if payload.DispatchPaused {
			if since, ok := d.pausedSince.Load().(time.Time); ok && !since.IsZero() {
				s := since
				payload.PausedSince = &s
			}
		}
		data, _ := json.Marshal(payload)
		return ipc.Response{Type: "status", Payload: data}

	case "crucibles":
		// Initialise as an empty (non-nil) slice so an environment with no
		// active crucibles serialises as `{"crucibles":[]}` rather than
		// `{"crucibles":null}` — the Hearth 2.0 SPA dereferences this field
		// directly and a null value crashes the dashboard.
		items := []ipc.CrucibleStatusItem{}
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
		return okResponse(resp)

	case "crucible_action":
		var ca ipc.CrucibleActionPayload
		if err := json.Unmarshal(cmd.Payload, &ca); err != nil {
			return errorResponse("invalid crucible_action payload")
		}
		if ca.ParentID == "" || ca.Anvil == "" {
			return errorResponse("parent_id and anvil are required")
		}
		// Canonicalise the user-provided anvil name (case-insensitive) so DB
		// queries and crucibleStatuses keys match the configured form.
		if canonical, _, ok := d.resolveAnvilConfig(ca.Anvil); ok {
			ca.Anvil = canonical
		}

		switch ca.Action {
		case "resume":
			// Resume: retry the parent bead to re-enter the crucible loop.
			// First, reset any failed child's circuit breaker by retrying
			// any retry entry for the parent, then trigger a poll to
			// re-discover it as a crucible candidate.
			anvilCfg, ok := d.cfg.Load().Anvils[ca.Anvil]
			if !ok || anvilCfg.Path == "" {
				return errorResponse(fmt.Sprintf("anvil %q not found", ca.Anvil))
			}

			// Clear circuit breaker / needs_human for the parent bead
			if err := d.db.ResetDispatchFailures(ca.ParentID, ca.Anvil); err != nil {
				d.logger.Warn("failed to reset dispatch failures for crucible parent", "bead", ca.ParentID, "error", err)
			}

			// Reset bead status to open and clear assignee so the poller can
			// rediscover it as a crucible candidate. Re-applies the
			// auto_dispatch_tag (idempotent) — releaseBeadClaim only strips it
			// on circuit-breaker escalation, but the resume path runs after
			// either a transient or escalated failure, so we add the tag
			// unconditionally to guarantee tagged-dispatch anvils see the
			// bead again via bd ready.
			resetCtx, resetCancel := context.WithTimeout(d.runCtx, executil.BdTimeout())
			defer resetCancel()
			output, err := d.restoreBeadAfterPause(resetCtx, ca.ParentID, anvilCfg)
			if err != nil {
				d.logger.Warn("failed to reset crucible parent status", "bead", ca.ParentID, "error", err, "output", string(output))
				return errorResponse(fmt.Sprintf("failed to reset crucible parent status: %v (%s)", err, string(output)))
			}

			// Clear the paused crucible status so it doesn't linger in the UI.
			d.crucibleStatuses.Delete(ca.Anvil + "/" + ca.ParentID)

			_ = d.db.LogEvent(state.EventRetryReset, fmt.Sprintf("Crucible %s resumed (manual)", ca.ParentID), ca.ParentID, ca.Anvil)
			d.logger.Info("crucible resumed", "parent", ca.ParentID, "anvil", ca.Anvil)

			// Trigger poll to rediscover the parent and re-enter crucible loop.
			go d.pollAndDispatch(d.runCtx, false)

			return okResponse(map[string]string{"message": fmt.Sprintf("crucible %s resumed", ca.ParentID)})

		case "stop":
			// Stop: close the parent bead.
			anvilCfg, ok := d.cfg.Load().Anvils[ca.Anvil]
			if !ok || anvilCfg.Path == "" {
				return errorResponse(fmt.Sprintf("anvil %q not found", ca.Anvil))
			}

			closeCmd, closeCancel := executil.BdCommand(d.runCtx, "close", ca.ParentID, "--reason=stopped from Hearth", "--json")
			defer closeCancel()
			closeCmd.Dir = anvilCfg.Path
			if output, err := closeCmd.CombinedOutput(); err != nil {
				return errorResponse(fmt.Sprintf("failed to close crucible parent: %v (%s)", err, string(output)))
			}

			// Clear the crucible status from the UI.
			d.crucibleStatuses.Delete(ca.Anvil + "/" + ca.ParentID)

			_ = d.db.LogEvent(state.EventBeadClosed, fmt.Sprintf("Crucible %s stopped (manual)", ca.ParentID), ca.ParentID, ca.Anvil)
			d.logger.Info("crucible stopped", "parent", ca.ParentID, "anvil", ca.Anvil)

			return okResponse(map[string]string{"message": fmt.Sprintf("crucible %s stopped", ca.ParentID)})

		default:
			return errorResponse(fmt.Sprintf("unknown crucible action %q", ca.Action))
		}

	case "kill_worker":
		var kp ipc.KillWorkerPayload
		if err := json.Unmarshal(cmd.Payload, &kp); err != nil {
			return errorResponse("invalid kill payload")
		}
		// PID is always looked up from state.db inside killWorkerProcess;
		// the client-supplied kp.PID is intentionally ignored.
		// Runs synchronously so the response reflects the actual outcome.
		if err := d.killWorkerProcess(kp.WorkerID); err != nil {
			d.logger.Error("kill_worker failed", "worker", kp.WorkerID, "error", err)
			return errorResponse(err.Error())
		}
		return okResponse(map[string]string{"killed": kp.WorkerID})

	case "shutdown":
		go func() {
			if d.cancel != nil {
				d.cancel()
			}
		}()
		return okResponse(map[string]string{"message": "shutting down"})

	case "refresh":
		go func() {
			d.pollAndDispatch(d.runCtx, false)
			if d.bellowsMonitor != nil {
				d.bellowsMonitor.Refresh()
			}
		}()
		return okResponse(map[string]string{"message": "poll triggered"})

	case "pause_dispatch":
		d.pauseMu.Lock()
		// Idempotent: pausing while already paused is a no-op success.
		if d.dispatchIsPaused() {
			d.pauseMu.Unlock()
			return okResponse(map[string]string{"message": "dispatch paused"})
		}
		now := time.Now()
		// Flip in-memory flag first so concurrent poll cycles see the pause
		// immediately, then persist. Revert if persistence fails.
		d.setDispatchPaused(true, PauseReasonManual, "")
		d.pausedSince.Store(now)
		if err := d.db.SetSetting(state.SettingDispatchPaused, "1"); err != nil {
			d.setDispatchPaused(false, PauseReasonNone, "")
			d.pausedSince.Store(time.Time{})
			d.pauseMu.Unlock()
			d.logger.Error("failed to persist dispatch pause", "error", err)
			return errorResponse("failed to persist dispatch pause: " + err.Error())
		}
		if err := d.db.SetSetting(state.SettingDispatchPauseReason, string(PauseReasonManual)); err != nil {
			d.logger.Warn("failed to persist dispatch pause reason", "error", err)
		}
		if err := d.db.SetSetting(state.SettingDispatchPausedAt, now.Format(time.RFC3339)); err != nil {
			d.logger.Warn("failed to persist dispatch pause timestamp", "error", err)
		}
		if err := d.db.LogEvent(state.EventDispatchPaused, "Dispatch manually paused — running workers continue; no new beads dispatched", "", ""); err != nil {
			d.logger.Warn("failed to log dispatch pause event", "error", err)
		}
		d.logger.Info("dispatch paused (manual)")
		d.pauseMu.Unlock()
		return okResponse(map[string]string{"message": "dispatch paused"})

	case "resume_dispatch":
		d.pauseMu.Lock()
		// Idempotent: resuming while not paused is a no-op success.
		if !d.dispatchIsPaused() {
			d.pauseMu.Unlock()
			return okResponse(map[string]string{"message": "dispatch resumed"})
		}
		if err := d.db.SetSetting(state.SettingDispatchPaused, "0"); err != nil {
			d.pauseMu.Unlock()
			d.logger.Error("failed to persist dispatch resume", "error", err)
			return errorResponse("failed to persist dispatch resume: " + err.Error())
		}
		if err := d.db.SetSetting(state.SettingDispatchPauseReason, ""); err != nil {
			d.logger.Warn("failed to clear dispatch pause reason", "error", err)
		}
		if err := d.db.SetSetting(state.SettingDispatchPausedAt, ""); err != nil {
			d.logger.Warn("failed to clear dispatch pause timestamp", "error", err)
		}
		d.setDispatchPaused(false, PauseReasonNone, "")
		d.pausedSince.Store(time.Time{})
		if err := d.db.LogEvent(state.EventDispatchResumed, "Dispatch manually resumed", "", ""); err != nil {
			d.logger.Warn("failed to log dispatch resume event", "error", err)
		}
		d.logger.Info("dispatch resumed (manual)")
		d.pauseMu.Unlock()
		// Kick a poll so resuming takes effect immediately rather than
		// waiting for the next ticker.
		go d.pollAndDispatch(d.runCtx, false)
		return okResponse(map[string]string{"message": "dispatch resumed"})

	case "reconcile_prs":
		go d.reconcileOpenPRs(d.runCtx)
		return okResponse(map[string]string{"message": "PR reconciliation triggered"})

	case "wicket_scan":
		d.wicketMu.Lock()
		wm := d.wicketMonitor
		d.wicketMu.Unlock()
		if wm == nil {
			return errorResponse("wicket monitor is not running")
		}
		wm.TriggerScan()
		return okResponse(map[string]string{"message": "wicket scan triggered"})

	case "subscribe":
		return okResponse(map[string]string{"message": "subscribed"})

	case "queue":
		items, err := d.db.QueueCache()
		if err != nil {
			return errorResponse(fmt.Sprintf("queue cache: %v", err))
		}
		// Snapshot the anvil dispatch tags once so the loop avoids repeatedly
		// re-reading the config and the Hearth web UI can render an Apply
		// button per row without round-tripping to the registry.
		cfg := d.cfg.Load()
		anvilTags := make(map[string]string, len(cfg.Anvils))
		for name, a := range cfg.Anvils {
			if a.AutoDispatchTag != "" {
				anvilTags[name] = a.AutoDispatchTag
			}
		}
		// Timestamps live in an in-memory map populated alongside the queue
		// cache rebuild (see pollAndDispatch). Sourcing them here avoids a
		// SQLite schema migration on queue_cache; entries missing from the
		// map serialise as empty strings on the wire.
		out := make([]ipc.QueueItem, 0, len(items))
		for _, it := range items {
			labels := parseQueueLabels(it.Labels)
			ts := d.lookupQueueTimestamp(it.Anvil, it.BeadID)
			out = append(out, ipc.QueueItem{
				BeadID:          it.BeadID,
				Anvil:           it.Anvil,
				Title:           it.Title,
				Description:     it.Description,
				Priority:        it.Priority,
				Status:          it.Status,
				Labels:          labels,
				Section:         string(it.Section),
				Assignee:        it.Assignee,
				CreatedAt:       ts.CreatedAt,
				UpdatedAt:       ts.UpdatedAt,
				AutoDispatchTag: anvilTags[it.Anvil],
			})
		}
		return okResponse(ipc.QueueResponse{Items: out})

	case "workers":
		workers, err := d.db.ActiveWorkers()
		if err != nil {
			return errorResponse(fmt.Sprintf("active workers: %v", err))
		}
		// Opt-in recently-finished workers (recent_seconds in the payload):
		// the web dashboard asks for a few minutes of terminal workers so a
		// finished panel lingers with its transcript instead of vanishing on
		// the next poll. Clients that send no payload (the TUI) see only
		// active workers, unchanged.
		if len(cmd.Payload) > 0 {
			var wp struct {
				RecentSeconds int `json:"recent_seconds"`
			}
			if jerr := json.Unmarshal(cmd.Payload, &wp); jerr == nil && wp.RecentSeconds > 0 {
				if wp.RecentSeconds > 3600 {
					wp.RecentSeconds = 3600
				}
				recent, rerr := d.db.RecentlyFinishedWorkers(time.Duration(wp.RecentSeconds) * time.Second)
				if rerr != nil {
					d.logger.Warn("workers IPC: RecentlyFinishedWorkers failed; returning active only", "error", rerr)
				} else {
					workers = append(workers, recent...)
				}
			}
		}
		// Build an index of PRs that currently meet every ready-to-merge
		// condition (CI green, no pending reviews, no unresolved threads, not
		// conflicting, non-terminal status). Bellows synthetic monitor workers
		// whose PR appears in this set are promoted to phase='ready_to_merge'
		// so the Hearth pipeline bar can show them as a distinct stage past
		// the PR/Bellows column.
		readyPRs, err := d.db.ReadyToMergePRs()
		if err != nil {
			d.logger.Warn("workers IPC: ReadyToMergePRs failed; ready-to-merge stage will be empty", "error", err)
			readyPRs = nil
		}
		readyKey := make(map[string]bool, len(readyPRs))
		for _, p := range readyPRs {
			readyKey[fmt.Sprintf("%s/%d", p.Anvil, p.Number)] = true
		}
		out := make([]ipc.WorkerInfo, 0, len(workers))
		for _, w := range workers {
			completedAt := ""
			if w.CompletedAt != nil {
				completedAt = w.CompletedAt.Format(time.RFC3339)
			}
			phase := w.Phase
			if phase == "bellows" && w.PRNumber > 0 && readyKey[fmt.Sprintf("%s/%d", w.Anvil, w.PRNumber)] {
				phase = "ready_to_merge"
			}
			out = append(out, ipc.WorkerInfo{
				ID:          w.ID,
				BeadID:      w.BeadID,
				Anvil:       w.Anvil,
				Branch:      w.Branch,
				Title:       w.Title,
				Status:      string(w.Status),
				Phase:       phase,
				Kind:        ipc.WorkerKindFromPhase(w.Phase),
				PID:         w.PID,
				StartedAt:   w.StartedAt.Format(time.RFC3339),
				CompletedAt: completedAt,
				LogPath:     w.LogPath,
				PRNumber:    w.PRNumber,
				SessionID:   w.SessionID,
				Model:       w.Model,
			})
		}
		return okResponse(ipc.WorkersResponse{Workers: out})

	case "events":
		limit := 50
		if len(cmd.Payload) > 0 {
			var p struct {
				Limit int `json:"limit"`
			}
			if err := json.Unmarshal(cmd.Payload, &p); err == nil && p.Limit > 0 && p.Limit <= 500 {
				limit = p.Limit
			}
		}
		events, err := d.db.RecentEvents(limit)
		if err != nil {
			return errorResponse(fmt.Sprintf("recent events: %v", err))
		}
		out := make([]ipc.EventInfo, 0, len(events))
		for _, e := range events {
			out = append(out, ipc.EventInfo{
				ID:        e.ID,
				Timestamp: e.Timestamp.Format(time.RFC3339),
				Type:      string(e.Type),
				Message:   e.Message,
				BeadID:    e.BeadID,
				Anvil:     e.Anvil,
			})
		}
		return okResponse(ipc.EventsResponse{Events: out})

	case "run_bead":
		var rp ipc.RunBeadPayload
		if err := json.Unmarshal(cmd.Payload, &rp); err != nil {
			return errorResponse("invalid run_bead payload")
		}

		var targetBead *poller.Bead

		if rp.ForceRun {
			// Force-run: fetch bead directly via bd show, bypassing bd ready.
			// This allows running children, blocked beads, etc. independently.
			if rp.Anvil == "" {
				return errorResponse("force-run requires --anvil flag")
			}
			anvilCfg, ok := d.cfg.Load().Anvils[rp.Anvil]
			if !ok {
				return errorResponse(fmt.Sprintf("anvil %q not found", rp.Anvil))
			}
			bead, err := crucible.FetchBead(context.Background(), rp.BeadID, anvilCfg.Path)
			if err != nil {
				return errorResponse(fmt.Sprintf("bd show %s: %v", rp.BeadID, err))
			}
			bead.Anvil = rp.Anvil
			bead.ForceIndependent = true
			targetBead = &bead
		} else {
			// Normal path: search cache first, then poll via bd ready.
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
				currentCfg := d.cfg.Load()
				p := poller.New(currentCfg.Anvils)
				p.BdReadyLimit = currentCfg.Settings.BdReadyLimit
				var beads []poller.Bead
				var pollErrors []string

				if rp.Anvil != "" {
					var err error
					beads, err = p.PollSingle(context.Background(), rp.Anvil)
					if err != nil {
						return errorResponse(fmt.Sprintf("anvil %q not found or poll failed: %v", rp.Anvil, err))
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
					return errorResponse(errorMsg)
				}
			}
		}

		// Skip if bead is already in flight
		if _, inFlight := d.activeBeads.LoadOrStore(targetBead.ID, true); inFlight {
			return errorResponse(fmt.Sprintf("bead %q is already in flight", targetBead.ID))
		}

		// Block beads that need clarification (consistent with auto-dispatch behavior)
		needed, err := d.isBeadClarificationNeeded(targetBead.ID, targetBead.Anvil)
		if err != nil {
			d.releaseBeadSlot(targetBead.ID)
			return errorResponse(fmt.Sprintf("failed to check clarification status for %q: %v", targetBead.ID, err))
		}
		if needed {
			d.releaseBeadSlot(targetBead.ID)
			return errorResponse(fmt.Sprintf("bead %q needs clarification; use 'forge queue unclarify --anvil %s %s' to clear", targetBead.ID, targetBead.Anvil, targetBead.ID))
		}

		// Reject rather than accept-and-discard when the target anvil's beads
		// database is mid-merge. Every bd write there is rolled back, so the
		// dispatch would fail at its first claim; the caller gets the real reason
		// instead of a hollow acknowledgement.
		if reason := d.wedgedAnvilError(targetBead.Anvil); reason != "" {
			d.releaseBeadSlot(targetBead.ID)
			_ = d.db.LogEvent(state.EventDispatchBlockedAnvilWedged,
				fmt.Sprintf("Manual dispatch of %s refused: %s", targetBead.ID, reason),
				targetBead.ID, targetBead.Anvil)
			return errorResponse(fmt.Sprintf("cannot dispatch %q: %s", targetBead.ID, reason))
		}

		// Manual dispatch resets the dispatch circuit breaker so the bead can be retried,
		// but only if the bead has recorded dispatch failures (i.e., the breaker was involved).
		if retry, err := d.db.GetRetry(targetBead.ID, targetBead.Anvil); err == nil && retry != nil && retry.DispatchFailures > 0 {
			_ = d.db.ResetDispatchFailures(targetBead.ID, targetBead.Anvil)
		}

		// Resolve epic branch if not already populated from the poll cache.
		// Skip entirely for force-run beads — they dispatch as standalone.
		if !targetBead.ForceIndependent && targetBead.EpicBranch == "" {
			anvilPath := d.cfg.Load().Anvils[targetBead.Anvil].Path
			if anvilPath != "" {
				paths := map[string]string{targetBead.Anvil: anvilPath}
				single := []poller.Bead{*targetBead}
				// First resolve blocks for this bead so epic relationships can be
				// Crucible detection relies on the poller's reconstruction
				// from Parent/Dependencies (ResolveBlocks was removed).
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
			d.releaseBeadSlot(targetBead.ID)
			return errorResponse(fmt.Sprintf("checking anvil capacity: %v", err))
		}
		if !canSpawnAnvil {
			d.releaseBeadSlot(targetBead.ID)
			return errorResponse(fmt.Sprintf("anvil %q capacity reached (max %d smiths)", targetBead.Anvil, maxSmiths))
		}

		maxTotal := d.cfg.Load().Settings.MaxTotalSmiths
		if maxTotal <= 0 {
			maxTotal = 4
		}
		canSpawnGlobal, err := worker.CanSpawnGlobal(d.db, maxTotal)
		if err != nil {
			d.releaseBeadSlot(targetBead.ID)
			return errorResponse(fmt.Sprintf("checking global capacity: %v", err))
		}
		if !canSpawnGlobal {
			d.releaseBeadSlot(targetBead.ID)
			return errorResponse(fmt.Sprintf("global capacity reached (max %d smiths)", maxTotal))
		}

		// Insert a pending worker row BEFORE claiming so orphan recovery can
		// identify this as Forge-owned even if the claim is killed after
		// committing server-side, and through the claim→worktree window (Forge-au4z).
		claimWorkerID := d.insertPendingWorker(targetBead.ID, targetBead.Anvil, targetBead.Title)

		// Claim the bead
		if err := d.claimBead(context.Background(), targetBead.ID, anvilCfg.Path); err != nil {
			d.abortClaim(targetBead.ID, targetBead.Anvil, claimWorkerID, fmt.Sprintf("claim failed: %v", err), err)
			d.releaseBeadSlot(targetBead.ID)
			return errorResponse(fmt.Sprintf("failed to claim bead: %v", err))
		}

		// Create and register the control handle only after workerID is known
		// and the bead is successfully claimed (see pollAndDispatch).
		ctrl := newControlHandle(claimWorkerID)
		d.registerControlHandle(targetBead.ID, ctrl)

		// Reserve in-flight spend so this manually-dispatched worker still counts
		// against the daily_cost_limit gate for subsequent auto-dispatch, even
		// though the manual path itself bypasses the gate (Forge-s3w7).
		manualReservation := d.reserveWorkerCost(d.perWorkerCostEstimate(d.cfg.Load()))
		d.wg.Add(1)
		go d.dispatchBead(context.Background(), *targetBead, anvilCfg, claimWorkerID, ctrl, nil, manualReservation)

		return okResponse(map[string]string{"message": "dispatched"})

	case "set_clarification":
		var cp ipc.ClarificationPayload
		if err := json.Unmarshal(cmd.Payload, &cp); err != nil {
			return errorResponse("invalid set_clarification payload")
		}
		if err := queueactions.Clarify(context.Background(), d.queueActionsHandle(), queueactions.Params{
			BeadID:    cp.BeadID,
			AnvilName: cp.Anvil,
			Note:      cp.Reason,
		}); err != nil {
			return errorResponse(queueActionsErrorMessage("set clarification", err))
		}
		d.logger.Info("bead marked as clarification_needed", "bead", cp.BeadID, "anvil", cp.Anvil, "reason", strings.TrimSpace(cp.Reason))
		return okResponse(map[string]string{"message": "clarification_needed set"})

	case "append_notes":
		var np ipc.AppendNotesPayload
		if err := json.Unmarshal(cmd.Payload, &np); err != nil {
			return errorResponse("invalid append_notes payload")
		}
		if np.BeadID == "" || np.Anvil == "" {
			return errorResponse("bead_id and anvil are required")
		}

		cfgSnapshot := d.cfg.Load()
		anvilCfg, ok := cfgSnapshot.Anvils[np.Anvil]
		if !ok {
			return errorResponse(fmt.Sprintf("anvil %q not found", np.Anvil))
		}
		if anvilCfg.Path == "" {
			return errorResponse(fmt.Sprintf("anvil %q has no path configured", np.Anvil))
		}

		reqID, _ := d.reqTracker.Track()
		go func() {
			notesCmd, notesCancel := executil.BdCommand(d.runCtx, "update", np.BeadID, "--append-notes", np.Notes)
			defer notesCancel()
			notesCmd.Dir = anvilCfg.Path
			if out, err := notesCmd.CombinedOutput(); err != nil {
				d.completeAsync(reqID, errorResponse(fmt.Sprintf("bd update %s --append-notes: %v: %s", np.BeadID, err, string(out))))
				return
			}
			d.completeAsync(reqID, okResponse(map[string]string{"message": "notes appended"}))
		}()
		resp, _ := ipc.NewQueuedResponse(reqID, "appending notes")
		return resp

	case "tag_bead":
		var tp ipc.TagBeadPayload
		if err := json.Unmarshal(cmd.Payload, &tp); err != nil {
			return errorResponse("invalid tag_bead payload")
		}
		if tp.BeadID == "" || tp.Anvil == "" {
			return errorResponse("bead_id and anvil are required")
		}
		cfgSnapshot := d.cfg.Load()
		anvilCfg, ok := cfgSnapshot.Anvils[tp.Anvil]
		if !ok {
			return errorResponse(fmt.Sprintf("anvil %q not found", tp.Anvil))
		}
		if anvilCfg.Path == "" {
			return errorResponse(fmt.Sprintf("anvil %q has no path configured", tp.Anvil))
		}
		tag := anvilCfg.AutoDispatchTag
		if tag == "" {
			return errorResponse(fmt.Sprintf("anvil %q has no auto_dispatch_tag configured", tp.Anvil))
		}
		reqID, _ := d.reqTracker.Track()
		go func() {
			tagCmd, tagCancel := executil.BdCommand(d.runCtx, "update", tp.BeadID, "--add-label", tag)
			defer tagCancel()
			tagCmd.Dir = anvilCfg.Path
			if out, err := tagCmd.CombinedOutput(); err != nil {
				d.completeAsync(reqID, errorResponse(fmt.Sprintf("bd update failed: %v: %s", err, string(out))))
				return
			}
			d.logger.Info("label added to bead", "bead", tp.BeadID, "anvil", tp.Anvil, "tag", tag)
			_ = d.db.LogEvent(state.EventBeadTagged, fmt.Sprintf("Label %q added to bead %s", tag, tp.BeadID), tp.BeadID, tp.Anvil)
			refreshCtx, refreshCancel := context.WithTimeout(d.runCtx, 30*time.Second)
			defer refreshCancel()
			d.pollAndDispatch(refreshCtx, false)
			d.completeAsync(reqID, okResponse(map[string]string{"message": fmt.Sprintf("label %q added", tag)}))
		}()
		resp, _ := ipc.NewQueuedResponse(reqID, "tagging bead")
		return resp

	case "update_label":
		var up ipc.UpdateLabelPayload
		if err := json.Unmarshal(cmd.Payload, &up); err != nil {
			return errorResponse("invalid update_label payload")
		}
		if up.BeadID == "" || up.Anvil == "" || up.Label == "" {
			return errorResponse("bead_id, anvil, and label are required")
		}
		var bdFlag, pastTense, gerund string
		switch up.Action {
		case "add":
			bdFlag = "--add-label"
			pastTense = "added"
			gerund = "adding"
		case "remove":
			bdFlag = "--remove-label"
			pastTense = "removed"
			gerund = "removing"
		default:
			return errorResponse(fmt.Sprintf("invalid action %q (want add|remove)", up.Action))
		}
		cfgSnapshot := d.cfg.Load()
		anvilCfg, ok := cfgSnapshot.Anvils[up.Anvil]
		if !ok {
			return errorResponse(fmt.Sprintf("anvil %q not found", up.Anvil))
		}
		if anvilCfg.Path == "" {
			return errorResponse(fmt.Sprintf("anvil %q has no path configured", up.Anvil))
		}
		reqID, _ := d.reqTracker.Track()
		go func() {
			labelCmd, labelCancel := executil.BdCommand(d.runCtx, "update", up.BeadID, bdFlag, up.Label)
			defer labelCancel()
			labelCmd.Dir = anvilCfg.Path
			if out, err := labelCmd.CombinedOutput(); err != nil {
				d.completeAsync(reqID, errorResponse(fmt.Sprintf("bd update failed: %v: %s", err, string(out))))
				return
			}
			d.logger.Info("label updated", "bead", up.BeadID, "anvil", up.Anvil, "label", up.Label, "action", up.Action)
			if logErr := d.db.LogEvent(state.EventBeadTagged, fmt.Sprintf("Label %q %s on bead %s", up.Label, pastTense, up.BeadID), up.BeadID, up.Anvil); logErr != nil {
				d.logger.Warn("failed to log label update event", "bead", up.BeadID, "anvil", up.Anvil, "error", logErr)
			}
			refreshCtx, refreshCancel := context.WithTimeout(d.runCtx, 30*time.Second)
			defer refreshCancel()
			d.pollAndDispatch(refreshCtx, false)
			d.completeAsync(reqID, okResponse(map[string]string{"message": fmt.Sprintf("label %q %s", up.Label, pastTense)}))
		}()
		resp, _ := ipc.NewQueuedResponse(reqID, fmt.Sprintf("%s label", gerund))
		return resp

	case "close_bead":
		var cp ipc.CloseBeadPayload
		if err := json.Unmarshal(cmd.Payload, &cp); err != nil {
			return errorResponse("invalid close_bead payload")
		}
		if cp.BeadID == "" || cp.Anvil == "" {
			return errorResponse("bead_id and anvil are required")
		}
		cfgSnapshot := d.cfg.Load()
		anvilCfg, ok := cfgSnapshot.Anvils[cp.Anvil]
		if !ok {
			return errorResponse(fmt.Sprintf("anvil %q not found", cp.Anvil))
		}
		if anvilCfg.Path == "" {
			return errorResponse(fmt.Sprintf("anvil %q has no path configured", cp.Anvil))
		}
		reqID, _ := d.reqTracker.Track()
		go func() {
			closeCmd, closeCancel := executil.BdCommand(d.runCtx, "close", cp.BeadID)
			defer closeCancel()
			closeCmd.Dir = anvilCfg.Path
			if out, err := closeCmd.CombinedOutput(); err != nil {
				d.completeAsync(reqID, errorResponse(fmt.Sprintf("bd close failed: %v: %s", err, string(out))))
				return
			}
			d.logger.Info("bead closed via TUI", "bead", cp.BeadID, "anvil", cp.Anvil)
			_ = d.db.LogEvent(state.EventBeadClosed, fmt.Sprintf("Bead %s closed via TUI", cp.BeadID), cp.BeadID, cp.Anvil)
			refreshCtx, refreshCancel := context.WithTimeout(d.runCtx, 30*time.Second)
			defer refreshCancel()
			d.pollAndDispatch(refreshCtx, false)
			d.completeAsync(reqID, okResponse(map[string]string{"message": fmt.Sprintf("bead %s closed", cp.BeadID)}))
		}()
		resp, _ := ipc.NewQueuedResponse(reqID, "closing bead")
		return resp

	case "stop_bead":
		var sp ipc.StopBeadPayload
		if err := json.Unmarshal(cmd.Payload, &sp); err != nil {
			return errorResponse("invalid stop_bead payload")
		}
		// stop_bead and queue_stop share one implementation; both release the bd
		// claim so a stop from the CLI and from the web GUI behave identically.
		return d.stopBead(stopBeadParams{
			beadID:       sp.BeadID,
			anvil:        sp.Anvil,
			reason:       sp.Reason,
			releaseClaim: true,
		})

	case "create_pr":
		var cp ipc.CreatePRPayload
		if err := json.Unmarshal(cmd.Payload, &cp); err != nil {
			return errorResponse("invalid create_pr payload")
		}
		if cp.BeadID == "" || cp.Anvil == "" {
			return errorResponse("bead_id and anvil are required")
		}
		// Canonicalise the anvil name so the helper's config lookup matches the
		// configured key regardless of how the user typed it on the CLI.
		anvilName := cp.Anvil
		if canonical, _, ok := d.resolveAnvilConfig(anvilName); ok {
			anvilName = canonical
		}
		// Handle synchronously so the CLI receives the real PR number (or gh
		// error). Each IPC connection runs in its own goroutine, so blocking for
		// the duration of the gh call does not stall other clients. The client
		// uses ipc.BdBackedReadTimeout for this command.
		opCtx, opCancel := context.WithTimeout(context.Background(), ipc.BdBackedReadTimeout)
		defer opCancel()
		prNumber, prURL, err := d.openPRForExistingBranch(opCtx, cp.BeadID, anvilName)
		if err != nil {
			return errorResponse(err.Error())
		}
		// Include the PR number and (when known) its web URL as structured
		// fields so the Hearth "Create PR" button can render a clickable link.
		// prURL is empty for the already-open / raced recovery paths, where only
		// the number is available; clients fall back to a number-only label.
		data, _ := json.Marshal(map[string]any{
			"message":   fmt.Sprintf("opened PR #%d for %s", prNumber, cp.BeadID),
			"pr_number": prNumber,
			"pr_url":    prURL,
		})
		return ipc.Response{Type: "ok", Payload: data}

	case "clear_clarification":
		var cp ipc.ClarificationPayload
		if err := json.Unmarshal(cmd.Payload, &cp); err != nil {
			return errorResponse("invalid clear_clarification payload")
		}
		if err := queueactions.Unclarify(context.Background(), d.queueActionsHandle(), queueactions.Params{
			BeadID:    cp.BeadID,
			AnvilName: cp.Anvil,
			Note:      cp.Reason,
		}); err != nil {
			return errorResponse(queueActionsErrorMessage("clear clarification", err))
		}
		d.logger.Info("clarification_needed cleared", "bead", cp.BeadID, "anvil", cp.Anvil)
		return okResponse(map[string]string{"message": "clarification_needed cleared"})

	case "retry_bead":
		var rp ipc.RetryBeadPayload
		if err := json.Unmarshal(cmd.Payload, &rp); err != nil {
			return errorResponse("invalid retry_bead payload")
		}
		if rp.PRID == 0 && (rp.BeadID == "" || rp.Anvil == "") {
			return errorResponse("bead_id and anvil are required")
		}
		// Canonicalise the user-provided anvil name (case-insensitive) so DB
		// queries below match records that were stored under the configured
		// key, regardless of how the user typed it on the CLI.
		if rp.Anvil != "" {
			if canonical, _, ok := d.resolveAnvilConfig(rp.Anvil); ok {
				rp.Anvil = canonical
			}
		}
		// Exhausted PR retry: DB-only, no bd shelling required.
		if rp.PRID > 0 {
			pr, err := d.db.GetPRByID(rp.PRID)
			if err != nil || pr == nil {
				return errorResponse(fmt.Sprintf("PR %d not found", rp.PRID))
			}
			if err := d.db.ResetPRFixCounts(rp.PRID); err != nil {
				return errorResponse(fmt.Sprintf("failed to reset PR fix counts: %v", err))
			}
			// The head-scoped review-fix breaker is part of the same budget:
			// leaving its row would have the retry trip again on its first
			// dispatch, since the head has not moved.
			if err := d.db.DeleteReviewFixDispatch(pr.Anvil, pr.Number); err != nil {
				d.logger.Warn("failed to clear review fix dispatch bookkeeping on retry",
					"pr", pr.Number, "anvil", pr.Anvil, "error", err)
			}
			if d.lifecycleMgr == nil {
				d.logger.Error("lifecycle manager not ready for retry_bead PR reset", "pr_id", rp.PRID, "bead", pr.BeadID, "anvil", pr.Anvil)
				return errorResponse("lifecycle manager not ready")
			}
			d.lifecycleMgr.ResetPRState(pr.Anvil, pr.Number)
			if d.bellowsMonitor != nil {
				d.bellowsMonitor.ResetPRState(pr.Anvil, pr.Number)
				d.bellowsMonitor.Refresh()
			}
			go d.pollAndDispatch(d.runCtx, false)

			_ = d.db.LogEvent(
				state.EventRetryReset,
				fmt.Sprintf("PR fix counts reset for PR %d (manual)", rp.PRID),
				pr.BeadID,
				pr.Anvil,
			)
			d.logger.Info("PR fix counts reset", "pr_id", rp.PRID, "bead", pr.BeadID, "anvil", pr.Anvil)
			return okResponse(map[string]string{"message": "PR fix counts reset, status set to open"})
		}
		// Bead retry: delegate the state mutation + audit event to the
		// shared queueactions.Retry, then handle the daemon-local async work
		// (bd shell, crucible cache, poll dispatch) below.
		hadCircuitBreaker, err := queueactions.Retry(context.Background(), d.queueActionsHandle(), queueactions.Params{
			BeadID:    rp.BeadID,
			AnvilName: rp.Anvil,
		})
		if err != nil {
			return errorResponse(queueActionsErrorMessage("retry bead", err))
		}
		d.logger.Info("retry reset for bead", "bead", rp.BeadID, "anvil", rp.Anvil)
		reqID, _ := d.reqTracker.Track()
		go func() {
			bdUpdateOK := false
			// bdUpdateErr is reported as the request's terminal outcome so a
			// failed bd write cannot present as a successful retry in the UI
			// (Forge-4r2n). It stays nil when there is no bd write to do
			// (anvil without a configured path) — that is not a failure.
			var bdUpdateErr error
			if anvilCfg, ok := d.cfg.Load().Anvils[rp.Anvil]; ok && anvilCfg.Path != "" {
				resetCtx, resetCancel := context.WithTimeout(d.runCtx, executil.BdTimeout())
				defer resetCancel()
				// Mirrors the crucible_action resume path: re-apply the
				// auto_dispatch_tag (idempotent). releaseBeadClaim only strips
				// it on circuit-breaker escalation, but the retry path runs
				// after either a transient or escalated failure, so we add the
				// tag unconditionally to guarantee tagged-dispatch anvils see
				// the bead again via bd ready.
				output, err := d.restoreBeadAfterPause(resetCtx, rp.BeadID, anvilCfg)
				if err != nil {
					d.logger.Warn("failed to reset bead status after retry reset", "bead", rp.BeadID, "error", err, "output", string(output))
					bdUpdateErr = fmt.Errorf("retry state reset but bd update failed: %w", err)
				} else {
					bdUpdateOK = true
				}
			}
			// Clear the in-memory paused crucible status so the parent can
			// re-enter the crucible loop without a daemon restart. Mirrors
			// the crucible_action resume path. Only do this once bd update
			// succeeded so we don't drop UI state on a transient bd failure.
			if bdUpdateOK {
				d.crucibleStatuses.Delete(rp.Anvil + "/" + rp.BeadID)
			}
			d.pollAndDispatch(d.runCtx, false)
			// Preserve the pre-refactor response wording: CLI/web consumers
			// distinguish "retry state reset" (circuit-breaker cleared) from
			// "retry reset" (no circuit-breaker, just a manual nudge).
			retryMsg := "retry reset"
			if hadCircuitBreaker {
				retryMsg = "retry state reset"
			}
			if bdUpdateErr != nil {
				d.completeAsync(reqID, errorResponse(bdUpdateErr.Error()))
				return
			}
			d.completeAsync(reqID, okResponse(map[string]string{"message": retryMsg}))
		}()
		resp, _ := ipc.NewQueuedResponse(reqID, "retrying bead")
		return resp

	case "clear_bead":
		var cp ipc.ClearBeadPayload
		if err := json.Unmarshal(cmd.Payload, &cp); err != nil {
			return errorResponse("invalid clear_bead payload")
		}
		if cp.Anvil != "" {
			if canonical, _, ok := d.resolveAnvilConfig(cp.Anvil); ok {
				cp.Anvil = canonical
			}
		}
		if err := queueactions.Clear(context.Background(), d.queueActionsHandle(), queueactions.Params{
			BeadID:    cp.BeadID,
			AnvilName: cp.Anvil,
		}); err != nil {
			return errorResponse(queueActionsErrorMessage("clear needs-attention flags", err))
		}
		d.logger.Info("needs-attention flags cleared", "bead", cp.BeadID, "anvil", cp.Anvil)
		return okResponse(map[string]string{"message": "needs-attention flags cleared"})

	case "dismiss_bead":
		var dp ipc.DismissBeadPayload
		if err := json.Unmarshal(cmd.Payload, &dp); err != nil {
			return errorResponse("invalid dismiss_bead payload")
		}
		// When targeting an exhausted PR (PRID > 0), bead_id is optional — non-bead
		// PRs (e.g. warden-learn PRs) have no associated bead.
		if dp.PRID == 0 && (dp.BeadID == "" || dp.Anvil == "") {
			return errorResponse("bead_id and anvil are required")
		}
		// Exhausted PR dismiss: set status to closed
		if dp.PRID > 0 {
			pr, err := d.db.GetPRByID(dp.PRID)
			if err != nil || pr == nil {
				return errorResponse(fmt.Sprintf("PR %d not found", dp.PRID))
			}
			if err := d.db.DismissExhaustedPR(dp.PRID); err != nil {
				return errorResponse(fmt.Sprintf("failed to dismiss exhausted PR: %v", err))
			}
			_ = d.db.LogEvent(
				state.EventBeadDismissed,
				fmt.Sprintf("Exhausted PR %d dismissed (manual)", dp.PRID),
				pr.BeadID,
				pr.Anvil,
			)
			d.logger.Info("exhausted PR dismissed", "pr_id", dp.PRID, "bead", pr.BeadID, "anvil", pr.Anvil)
			return okResponse(map[string]string{"message": "exhausted PR dismissed"})
		}
		if err := d.db.DismissRetry(dp.BeadID, dp.Anvil); err != nil {
			return errorResponse(fmt.Sprintf("failed to dismiss: %v", err))
		}
		logMessage := fmt.Sprintf("Bead %s dismissed from needs attention", dp.BeadID)
		_ = d.db.LogEvent(state.EventBeadDismissed, logMessage, dp.BeadID, dp.Anvil)
		d.logger.Info("bead dismissed from needs attention", "bead", dp.BeadID, "anvil", dp.Anvil)
		return okResponse(map[string]string{"message": "dismissed"})

	case "warden_rerun":
		var wp ipc.WardenRerunPayload
		if err := json.Unmarshal(cmd.Payload, &wp); err != nil {
			return errorResponse("invalid warden_rerun payload")
		}
		if wp.BeadID == "" || wp.Anvil == "" {
			return errorResponse("bead_id and anvil are required")
		}
		anvilCfg, ok := d.cfg.Load().Anvils[wp.Anvil]
		if !ok {
			return errorResponse(fmt.Sprintf("anvil %q not found", wp.Anvil))
		}
		branch, err := d.db.LastWorkerBranchForBead(wp.BeadID, wp.Anvil)
		if err != nil || branch == "" {
			return errorResponse(fmt.Sprintf("no branch found for bead %s", wp.BeadID))
		}
		_ = d.db.LogEvent(state.EventWardenRerun, fmt.Sprintf("Warden re-review requested for %s (manual)", wp.BeadID), wp.BeadID, wp.Anvil)
		d.logger.Info("warden re-review requested", "bead", wp.BeadID, "anvil", wp.Anvil, "branch", branch)

		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.handleWardenRerun(wp.BeadID, wp.Anvil, branch, anvilCfg)
		}()

		return okResponse(map[string]string{"message": "warden re-review started"})

	case "assay_rerun":
		var arp ipc.AssayRerunPayload
		if err := json.Unmarshal(cmd.Payload, &arp); err != nil {
			return errorResponse("invalid assay_rerun payload: " + err.Error())
		}
		if arp.Anvil == "" {
			return errorResponse("assay_rerun requires anvil")
		}
		anvilCfg, ok := d.cfg.Load().Anvils[arp.Anvil]
		if !ok {
			return errorResponse("unknown anvil: " + arp.Anvil)
		}
		// The PR may be addressed by its state.db row id (what the web
		// dashboard holds) or by its GitHub number scoped to this anvil (what
		// `forge assay rerun <pr> --anvil` sends); resolvePRTarget is the one
		// place either form becomes a row.
		pr, err := resolvePRTarget(d.db, arp.PR, arp.PRNumber, arp.Anvil)
		if err != nil {
			return errorResponse(err.Error())
		}
		_ = d.db.LogEvent(state.EventPRReviewNeeded, fmt.Sprintf("Assay re-review requested for PR #%d (manual)", pr.Number), pr.BeadID, arp.Anvil)
		d.logger.Info("Assay re-review requested", "pr", pr.Number, "anvil", arp.Anvil, "bead", pr.BeadID)

		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			// Resolve the current head SHA so the run records against the head
			// actually reviewed. A manual rerun deliberately bypasses the
			// Bellows trigger gate's head-SHA debounce, so the stored
			// LastReviewedSHA may already equal head — dispatching
			// ActionAssayReview directly forces the pass regardless.
			headSHA := ""
			statusCtx, cancel := context.WithTimeout(d.runCtx, 30*time.Second)
			if st, serr := d.vcsForAnvil(arp.Anvil).CheckStatusLight(statusCtx, anvilCfg.Path, pr.Number); serr == nil && st != nil {
				headSHA = st.HeadSHA
			} else if serr != nil {
				d.logger.Warn("assay rerun: failed to resolve PR head SHA; recording run without it", "pr", pr.Number, "anvil", arp.Anvil, "error", serr)
			}
			cancel()
			d.handleLifecycleAction(d.runCtx, lifecycle.ActionRequest{
				Action:     lifecycle.ActionAssayReview,
				PRNumber:   pr.Number,
				BeadID:     pr.BeadID,
				Anvil:      arp.Anvil,
				Branch:     pr.Branch,
				BaseBranch: pr.BaseBranch,
				HeadSHA:    headSHA,
				IsManual:   true,
			})
		}()

		return okResponse(map[string]string{"message": fmt.Sprintf("Assay re-review started for PR #%d", pr.Number)})

	case "approve_as_is":
		var ap ipc.ApproveAsIsPayload
		if err := json.Unmarshal(cmd.Payload, &ap); err != nil {
			return errorResponse("invalid approve_as_is payload")
		}
		if ap.BeadID == "" || ap.Anvil == "" {
			return errorResponse("bead_id and anvil are required")
		}
		anvilCfg, ok := d.cfg.Load().Anvils[ap.Anvil]
		if !ok {
			return errorResponse(fmt.Sprintf("anvil %q not found", ap.Anvil))
		}
		branch, err := d.db.LastWorkerBranchForBead(ap.BeadID, ap.Anvil)
		if err != nil || branch == "" {
			return errorResponse(fmt.Sprintf("no branch found for bead %s", ap.BeadID))
		}
		_ = d.db.LogEvent(state.EventApproveAsIs, fmt.Sprintf("Approve as-is requested for %s (manual)", ap.BeadID), ap.BeadID, ap.Anvil)
		d.logger.Info("approve as-is requested", "bead", ap.BeadID, "anvil", ap.Anvil, "branch", branch)

		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.handleApproveAsIs(ap.BeadID, ap.Anvil, branch, anvilCfg)
		}()

		return okResponse(map[string]string{"message": "approve as-is started"})

	case "force_smith":
		var fp ipc.ForceSmithPayload
		if err := json.Unmarshal(cmd.Payload, &fp); err != nil {
			return errorResponse("invalid force_smith payload")
		}
		if fp.BeadID == "" || fp.Anvil == "" {
			return errorResponse("bead_id and anvil are required")
		}
		anvilCfg, ok := d.cfg.Load().Anvils[fp.Anvil]
		if !ok {
			return errorResponse(fmt.Sprintf("anvil %q not found", fp.Anvil))
		}
		branch, err := d.db.LastWorkerBranchForBead(fp.BeadID, fp.Anvil)
		if err != nil || branch == "" {
			return errorResponse(fmt.Sprintf("no branch found for bead %s", fp.BeadID))
		}
		// Same gate as manual dispatch: a forced Smith run against a wedged anvil
		// is guaranteed to lose its bookkeeping at the first bd write, so refuse
		// with the real reason rather than spend a session on it. Checked before
		// the activeBeads slot is claimed, so there is nothing to release.
		if reason := d.wedgedAnvilError(fp.Anvil); reason != "" {
			_ = d.db.LogEvent(state.EventDispatchBlockedAnvilWedged,
				fmt.Sprintf("Force smith on %s refused: %s", fp.BeadID, reason), fp.BeadID, fp.Anvil)
			return errorResponse(fmt.Sprintf("cannot force smith on %q: %s", fp.BeadID, reason))
		}
		_ = d.db.LogEvent(state.EventForceSmith, fmt.Sprintf("Force smith requested for %s (manual)", fp.BeadID), fp.BeadID, fp.Anvil)
		d.logger.Info("force smith requested", "bead", fp.BeadID, "anvil", fp.Anvil, "branch", branch, "user_note", fp.UserNote)

		// Claim the activeBeads slot so the poller doesn't dispatch a normal
		// pipeline run concurrently while force_smith is in flight.
		if _, alreadyInFlight := d.activeBeads.LoadOrStore(fp.BeadID, true); alreadyInFlight {
			return errorResponse(fmt.Sprintf("bead %s is already in flight", fp.BeadID))
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer d.activeBeads.Delete(fp.BeadID)
			d.handleForceSmith(fp.BeadID, fp.Anvil, branch, fp.UserNote, anvilCfg)
		}()

		return okResponse(map[string]string{"message": "force smith started"})

	case "view_logs":
		var vp ipc.ViewLogsPayload
		if err := json.Unmarshal(cmd.Payload, &vp); err != nil {
			return errorResponse("invalid view_logs payload")
		}
		if vp.BeadID == "" {
			return errorResponse("bead_id is required")
		}
		logPath, err := d.db.LastWorkerLogPath(vp.BeadID)
		if err != nil {
			return errorResponse(fmt.Sprintf("failed to find log: %v", err))
		}
		if logPath == "" {
			return errorResponse(fmt.Sprintf("no worker logs found for bead %q", vp.BeadID))
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
		return okResponse(resp)

	case "merge_pr":
		var mp ipc.MergePRPayload
		if err := json.Unmarshal(cmd.Payload, &mp); err != nil {
			return errorResponse("invalid merge_pr payload")
		}
		// Comment 4: allow pr_id-only requests; pr_number can be derived from DB.
		if (mp.PRID <= 0 && mp.PRNumber <= 0) || mp.Anvil == "" {
			return errorResponse("anvil and either pr_id or pr_number are required")
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
			return errorResponse(fmt.Sprintf("failed to load PR from state db: %v", prErr))
		}
		if pr == nil {
			return errorResponse("PR not found in state db; cannot validate merge readiness")
		}
		// Comment 3 & 5: derive authoritative anvil and PR number from the DB record.
		// Validate that the payload's anvil matches what we loaded, to catch stale/buggy clients.
		if mp.PRID > 0 && mp.Anvil != "" && mp.Anvil != pr.Anvil {
			return errorResponse(fmt.Sprintf("anvil mismatch: payload has %q but PR %d belongs to %q", mp.Anvil, mp.PRID, pr.Anvil))
		}
		mergeAnvil := pr.Anvil
		mergeNumber := pr.Number
		cfgSnapshot := d.cfg.Load()
		anvilCfg, ok := cfgSnapshot.Anvils[mergeAnvil]
		if !ok {
			return errorResponse(fmt.Sprintf("anvil %q not found", mergeAnvil))
		}
		// Validate cached readiness from state.db.
		ready, readyErr := d.db.IsPRReadyToMerge(pr.ID)
		if readyErr != nil {
			return errorResponse(fmt.Sprintf("failed to check merge readiness: %v", readyErr))
		}
		if !ready {
			return errorResponse("PR is not ready to merge (not approved, CI failing, conflicting, or has unresolved threads)")
		}
		// Comment 6: re-check live GitHub status immediately before merging to avoid
		// acting on stale cached state from between Bellows polls.
		liveCtx, liveCancel := context.WithTimeout(d.runCtx, 30*time.Second)
		liveStatus, liveErr := d.vcsForAnvil(mergeAnvil).CheckStatus(liveCtx, anvilCfg.Path, mergeNumber)
		liveCancel()
		if liveErr != nil {
			return errorResponse(fmt.Sprintf("could not verify live PR status: %v", liveErr))
		}
		if !liveStatus.CIsPassing() || liveStatus.Mergeable == "CONFLICTING" || liveStatus.UnresolvedThreads > 0 || liveStatus.HasPendingReviewRequests() {
			return errorResponse("PR failed live readiness check (CI failing, conflicts, unresolved threads, or pending reviews)")
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
		if err := d.vcsForAnvil(mergeAnvil).MergePR(mergeCtx, anvilCfg.Path, mergeNumber, strategy); err != nil {
			_ = d.db.LogEvent(state.EventPRMergeFailed,
				fmt.Sprintf("PR #%d merge failed: %v", mergeNumber, err),
				beadID, mergeAnvil)
			d.logger.Error("PR merge failed", "pr_number", mergeNumber, "anvil", mergeAnvil, "error", err)
			// Sanitize error message for IPC: use only the first line to avoid multi-line/huge payloads.
			errSummary := strings.SplitN(err.Error(), "\n", 2)[0]
			return errorResponse(fmt.Sprintf("merge failed: %s", errSummary))
		}
		_ = d.db.LogEvent(state.EventPRMerged,
			fmt.Sprintf("PR #%d merged successfully (strategy: %s)", mergeNumber, strategy),
			beadID, mergeAnvil)
		d.logger.Info("PR merged successfully", "pr_number", mergeNumber, "anvil", mergeAnvil, "strategy", strategy)
		// Immediately update PR status so it disappears from the Ready to Merge panel
		// without waiting for the next bellows poll cycle (up to 2 minutes).
		_ = d.db.UpdatePRStatus(pr.ID, state.PRMerged)
		_ = d.db.CompleteWorkersByBead(beadID)
		// Close the bead and trigger bellows in a goroutine so the IPC response
		// is returned immediately. closeBead (bd close) can take several seconds;
		// running it synchronously would exceed the IPC client's 10-second read
		// deadline, causing Hearth to show a spurious error even though the merge
		// succeeded. bellows cannot detect the merge transition on its own because
		// UpdatePRStatus(PRMerged) above makes lastSnap.IsMerged true before
		// Refresh() fires, so we call closeBead directly here instead.
		if beadID != "" && !strings.HasPrefix(beadID, "ext-") {
			go func() {
				closeCtx, closeCancel := context.WithTimeout(d.runCtx, executil.BdTimeout())
				defer closeCancel()
				if err := d.closeBead(closeCtx, beadID, anvilCfg.Path, fmt.Sprintf("PR #%d merged", mergeNumber)); err != nil {
					d.logger.Warn("failed to close bead after merge", "bead", beadID, "pr", mergeNumber, "error", err)
				}
				if d.bellowsMonitor != nil {
					d.bellowsMonitor.Refresh()
				}
			}()
		} else if d.bellowsMonitor != nil {
			d.bellowsMonitor.Refresh()
		}
		return okResponse(map[string]string{"message": fmt.Sprintf("PR #%d merged", mergeNumber)})

	case "resolve_orphan":
		var rp ipc.ResolveOrphanPayload
		if err := json.Unmarshal(cmd.Payload, &rp); err != nil {
			return errorResponse("invalid resolve_orphan payload")
		}
		if rp.BeadID == "" || rp.Anvil == "" || rp.Action == "" {
			return errorResponse("bead_id, anvil, and action are required")
		}
		cfgSnapshot := d.cfg.Load()
		anvilCfg, ok := cfgSnapshot.Anvils[rp.Anvil]
		if !ok || anvilCfg.Path == "" {
			return errorResponse(fmt.Sprintf("anvil %q not found or has no path", rp.Anvil))
		}
		switch rp.Action {
		case "recover":
			if err := d.shutdownMgr.ResetBead(rp.BeadID, anvilCfg.Path); err != nil {
				return errorResponse(fmt.Sprintf("failed to recover bead: %v", err))
			}
			_ = d.db.RemovePendingOrphan(rp.BeadID, rp.Anvil)
			_ = d.db.LogEvent(state.EventBeadRecovered, fmt.Sprintf("Orphan %s recovered by user via Hearth", rp.BeadID), rp.BeadID, rp.Anvil)
			d.logger.Info("orphan recovered by user", "bead", rp.BeadID, "anvil", rp.Anvil)
			go d.pollAndDispatch(d.runCtx, false)
		case "close":
			// Use context.Background() so this bd close call is not interrupted
			// if the daemon is concurrently shutting down. The user explicitly
			// chose to close this orphan, and the operation must complete.
			closeCmd, closeCancel := executil.BdCommand(context.Background(), "close", rp.BeadID)
			defer closeCancel()
			closeCmd.Dir = anvilCfg.Path
			if out, err := closeCmd.CombinedOutput(); err != nil {
				return errorResponse(fmt.Sprintf("bd close failed: %v: %s", err, string(out)))
			}
			_ = d.db.RemovePendingOrphan(rp.BeadID, rp.Anvil)
			_ = d.db.LogEvent(state.EventBeadClosed, fmt.Sprintf("Orphan %s closed by user (work completed)", rp.BeadID), rp.BeadID, rp.Anvil)
			d.logger.Info("orphan closed by user (completed)", "bead", rp.BeadID, "anvil", rp.Anvil)
			// Refresh queue state so Hearth reflects the closed orphan immediately.
			go d.pollAndDispatch(d.runCtx, false)
		case "discard":
			// Use context.Background() so this bd close call is not interrupted
			// if the daemon is concurrently shutting down. The user explicitly
			// chose to discard this orphan, and the operation must complete.
			discardCmd, discardCancel := executil.BdCommand(context.Background(), "close", rp.BeadID, `--reason=Discarded by user during orphan recovery`)
			defer discardCancel()
			discardCmd.Dir = anvilCfg.Path
			if out, err := discardCmd.CombinedOutput(); err != nil {
				return errorResponse(fmt.Sprintf("bd close failed: %v: %s", err, string(out)))
			}
			_ = d.db.RemovePendingOrphan(rp.BeadID, rp.Anvil)
			_ = d.db.LogEvent(state.EventBeadClosed, fmt.Sprintf("Orphan %s discarded by user", rp.BeadID), rp.BeadID, rp.Anvil)
			d.logger.Info("orphan discarded by user", "bead", rp.BeadID, "anvil", rp.Anvil)
			// Refresh queue state so Hearth reflects the discarded orphan immediately.
			go d.pollAndDispatch(d.runCtx, false)
		default:
			return errorResponse(fmt.Sprintf("unknown orphan action: %q", rp.Action))
		}
		return okResponse(map[string]string{"message": fmt.Sprintf("orphan %s handled: %s", rp.BeadID, rp.Action)})

	case "pr_action":
		var pa ipc.PRActionPayload
		if err := json.Unmarshal(cmd.Payload, &pa); err != nil {
			return errorResponse("invalid pr_action payload: " + err.Error())
		}
		if pa.PRNumber == 0 || pa.Anvil == "" {
			return errorResponse("pr_action requires pr_number and anvil")
		}
		anvilCfg, ok := d.cfg.Load().Anvils[pa.Anvil]
		if !ok {
			return errorResponse("unknown anvil: " + pa.Anvil)
		}

		// Refuse the verbs that spawn a claude session while the anvil's beads
		// database is mid-merge. Their status and label writes are rolled back,
		// so the run burns tokens and consumes a max_ci_fix_attempts /
		// max_review_fix_attempts / max_rebase_attempts budget for nothing. The
		// VCS-only verbs (merge, close, approve, open_browser, bellows
		// assignment) touch no beads state and stay available.
		switch pa.Action {
		case "quench", "cifix", "burnish", "reviewfix", "rebase":
			if reason := d.wedgedAnvilError(pa.Anvil); reason != "" {
				_ = d.db.LogEvent(state.EventDispatchBlockedAnvilWedged,
					fmt.Sprintf("PR #%d %s refused: %s", pa.PRNumber, pa.Action, reason),
					pa.BeadID, pa.Anvil)
				return errorResponse(fmt.Sprintf("cannot run %q on PR #%d: %s", pa.Action, pa.PRNumber, reason))
			}
		}

		switch pa.Action {
		case "close":
			reqID, _ := d.reqTracker.Track()
			go func() {
				closeCtx, closeCancel := context.WithTimeout(d.runCtx, 30*time.Second)
				defer closeCancel()
				closeCmd := executil.HideWindow(exec.CommandContext(closeCtx, "gh", "pr", "close", strconv.Itoa(pa.PRNumber)))
				closeCmd.Dir = anvilCfg.Path
				if out, err := closeCmd.CombinedOutput(); err != nil {
					d.completeAsync(reqID, errorResponse(fmt.Sprintf("gh pr close failed: %v: %s", err, strings.TrimSpace(string(out)))))
					return
				}
				if pa.PRID > 0 {
					_ = d.db.UpdatePRStatus(pa.PRID, state.PRClosed)
				}
				_ = d.db.LogEvent(state.EventPRClosed, fmt.Sprintf("PR #%d closed by user", pa.PRNumber), pa.BeadID, pa.Anvil)
				d.logger.Info("PR closed by user via pr_action", "pr", pa.PRNumber, "anvil", pa.Anvil)
				d.completeAsync(reqID, okResponse(map[string]string{"message": fmt.Sprintf("PR #%d closed", pa.PRNumber)}))
			}()
			resp, _ := ipc.NewQueuedResponse(reqID, "closing PR")
			return resp

		case "open_browser":
			openCtx, openCancel := context.WithTimeout(d.runCtx, 15*time.Second)
			defer openCancel()
			openCmd := exec.CommandContext(openCtx, "gh", "pr", "view", strconv.Itoa(pa.PRNumber), "--web")
			openCmd.Dir = anvilCfg.Path
			executil.HideWindow(openCmd)
			if out, err := openCmd.CombinedOutput(); err != nil {
				return errorResponse(fmt.Sprintf("gh pr view --web failed: %v: %s", err, strings.TrimSpace(string(out))))
			}

		case "merge":
			reqID, _ := d.reqTracker.Track()
			go func() {
				mergeCtx, mergeCancel := context.WithTimeout(d.runCtx, 60*time.Second)
				defer mergeCancel()
				strategy := d.cfg.Load().Settings.MergeStrategy
				if err := d.vcsForAnvil(pa.Anvil).MergePR(mergeCtx, anvilCfg.Path, pa.PRNumber, strategy); err != nil {
					d.completeAsync(reqID, errorResponse(fmt.Sprintf("merge failed: %v", err)))
					return
				}
				if pa.PRID > 0 {
					_ = d.db.UpdatePRStatus(pa.PRID, state.PRMerged)
				}
				_ = d.db.LogEvent(state.EventPRMerged, fmt.Sprintf("PR #%d merged by user", pa.PRNumber), pa.BeadID, pa.Anvil)
				d.logger.Info("PR merged by user via pr_action", "pr", pa.PRNumber, "anvil", pa.Anvil)
				d.completeAsync(reqID, okResponse(map[string]string{"message": fmt.Sprintf("PR #%d merged", pa.PRNumber)}))
			}()
			resp, _ := ipc.NewQueuedResponse(reqID, "merging PR")
			return resp

		case "quench", "cifix":
			if pa.Branch == "" {
				return errorResponse("quench action requires branch")
			}
			req := lifecycle.ActionRequest{
				Action:   lifecycle.ActionFixCI,
				PRNumber: pa.PRNumber,
				BeadID:   pa.BeadID,
				Anvil:    pa.Anvil,
				Branch:   pa.Branch,
				IsManual: true,
			}
			d.dispatchLifecycleAction(req)
			_ = d.db.LogEvent(state.EventQuenchStarted, fmt.Sprintf("PR #%d CI fix triggered by user", pa.PRNumber), pa.BeadID, pa.Anvil)
			d.logger.Info("CI fix triggered by user via pr_action", "pr", pa.PRNumber, "anvil", pa.Anvil)

		case "burnish", "reviewfix":
			if pa.Branch == "" {
				return errorResponse("burnish action requires branch")
			}
			req := lifecycle.ActionRequest{
				Action:   lifecycle.ActionFixReview,
				PRNumber: pa.PRNumber,
				BeadID:   pa.BeadID,
				Anvil:    pa.Anvil,
				Branch:   pa.Branch,
				IsManual: true,
			}
			d.dispatchLifecycleAction(req)
			_ = d.db.LogEvent(state.EventBurnishStarted, fmt.Sprintf("PR #%d review fix triggered by user", pa.PRNumber), pa.BeadID, pa.Anvil)
			d.logger.Info("review fix triggered by user via pr_action", "pr", pa.PRNumber, "anvil", pa.Anvil)

		case "rebase":
			if pa.Branch == "" {
				return errorResponse("rebase action requires branch")
			}
			// Hearth and the web PR rows send the row id and the number
			// together, so the id wins and the number is the fallback for a PR
			// the dashboard knows by number alone (an externally-opened PR).
			// The prologue already guarantees a PR number, so there is always a
			// target to resolve.
			//
			// A PR that will not resolve is refused outright — DB error,
			// missing row, or an id owned by another anvil alike — rather than
			// dispatched with an empty base. rebase.Rebase substitutes "main"
			// for an empty BaseBranch and force-pushes the result, and this
			// repo deliberately opens crucible child PRs based on
			// feature/<parent-id>: proceeding on a guessed base would rewrite
			// such a branch onto main and destroy its old head. A refusal costs
			// the operator a retry; a wrong force-push costs the branch (cf.
			// worktree.RemoveIfPushed — what cannot be proven is not assumed).
			pr, err := resolvePRTargetPreferID(d.db, pa.PRID, pa.PRNumber, pa.Anvil)
			if err != nil {
				d.logger.Warn("rebase refused: could not resolve the PR row",
					"pr", pa.PRNumber, "pr_id", pa.PRID, "anvil", pa.Anvil, "error", err)
				return errorResponse(fmt.Sprintf("cannot rebase PR #%d: %v", pa.PRNumber, err))
			}
			baseBranch := pr.BaseBranch
			req := lifecycle.ActionRequest{
				Action:     lifecycle.ActionRebase,
				PRNumber:   pa.PRNumber,
				BeadID:     pa.BeadID,
				Anvil:      pa.Anvil,
				Branch:     pa.Branch,
				BaseBranch: baseBranch,
				IsManual:   true,
			}
			d.dispatchLifecycleAction(req)
			_ = d.db.LogEvent(state.EventRebaseStarted, fmt.Sprintf("PR #%d rebase triggered by user", pa.PRNumber), pa.BeadID, pa.Anvil)
			d.logger.Info("rebase triggered by user via pr_action", "pr", pa.PRNumber, "anvil", pa.Anvil)

		case "assign_bellows":
			if pa.PRID > 0 {
				// Set both bellows_managed=1 and bellows_manually_assigned=1 so
				// the reconcile loop's defensive ext-* clobber leaves this PR alone.
				if err := d.db.UpdatePRBellowsAssignment(pa.PRID, true, true); err != nil {
					return errorResponse(fmt.Sprintf("failed to assign bellows: %v", err))
				}
			}
			_ = d.db.LogEvent("bellows_assigned", fmt.Sprintf("PR #%d assigned to bellows for lifecycle management", pa.PRNumber), pa.BeadID, pa.Anvil)
			d.logger.Info("bellows assigned to external PR via pr_action", "pr", pa.PRNumber, "anvil", pa.Anvil)

		case "unassign_bellows":
			if pa.PRID > 0 {
				// Clear both flags so reconcile no longer treats this PR as
				// user-pinned and bellows stops running lifecycle workers for it.
				if err := d.db.UpdatePRBellowsAssignment(pa.PRID, false, false); err != nil {
					return errorResponse(fmt.Sprintf("failed to unassign bellows: %v", err))
				}
			}
			_ = d.db.LogEvent("bellows_unassigned", fmt.Sprintf("PR #%d released from bellows lifecycle management", pa.PRNumber), pa.BeadID, pa.Anvil)
			d.logger.Info("bellows unassigned from PR via pr_action", "pr", pa.PRNumber, "anvil", pa.Anvil)

		case "approve":
			reqID, _ := d.reqTracker.Track()
			go func() {
				approveCtx, approveCancel := context.WithTimeout(d.runCtx, 30*time.Second)
				defer approveCancel()
				approveCmd := executil.HideWindow(exec.CommandContext(approveCtx, "gh", "pr", "review", strconv.Itoa(pa.PRNumber), "--approve"))
				approveCmd.Dir = anvilCfg.Path
				if out, err := approveCmd.CombinedOutput(); err != nil {
					d.completeAsync(reqID, errorResponse(fmt.Sprintf("gh pr review --approve failed: %v: %s", err, strings.TrimSpace(string(out)))))
					return
				}
				_ = d.db.LogEvent("review_approved", fmt.Sprintf("PR #%d approved by user", pa.PRNumber), pa.BeadID, pa.Anvil)
				d.logger.Info("PR approved by user via pr_action", "pr", pa.PRNumber, "anvil", pa.Anvil)
				d.completeAsync(reqID, okResponse(map[string]string{"message": fmt.Sprintf("PR #%d approved", pa.PRNumber)}))
			}()
			resp, _ := ipc.NewQueuedResponse(reqID, "approving PR")
			return resp

		default:
			return errorResponse(fmt.Sprintf("unknown pr_action: %q", pa.Action))
		}

		return okResponse(map[string]string{"message": fmt.Sprintf("PR #%d: %s", pa.PRNumber, pa.Action)})

	case "get_ingots":
		var p ipc.GetIngotsPayload
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return errorResponse("invalid payload: " + err.Error())
		}
		conn := d.db.Conn()
		if conn == nil {
			return errorResponse("database not available")
		}
		ingots, err := ingot.GetIngots(conn, p.Anvil, p.Status, p.Limit)
		if err != nil {
			return errorResponse(err.Error())
		}
		return okResponse(ingots)

	case "get_ingot":
		var p ipc.GetIngotPayload
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return errorResponse("invalid payload: " + err.Error())
		}
		if p.BeadID == "" {
			return errorResponse("bead_id is required")
		}
		conn := d.db.Conn()
		if conn == nil {
			return errorResponse("database not available")
		}
		// If anvil is provided, query directly; otherwise search all anvils
		if p.Anvil != "" {
			ig, err := ingot.GetIngot(conn, p.BeadID, p.Anvil)
			if err != nil {
				return errorResponse(err.Error())
			}
			if ig == nil {
				return errorResponse(fmt.Sprintf("ingot %s not found in anvil %s", p.BeadID, p.Anvil))
			}
			return okResponse(ig)
		}
		// No anvil specified — query DB directly across all anvils.
		matches, err := ingot.GetIngotByBeadID(conn, p.BeadID)
		if err != nil {
			return errorResponse(err.Error())
		}
		switch len(matches) {
		case 0:
			return errorResponse(fmt.Sprintf("ingot %s not found", p.BeadID))
		case 1:
			// Exactly one match — fetch with test results eager-loaded.
			ig, err := ingot.GetIngot(conn, p.BeadID, matches[0].Anvil)
			if err != nil {
				return errorResponse(err.Error())
			}
			if ig == nil {
				return errorResponse(fmt.Sprintf("ingot %s not found", p.BeadID))
			}
			return okResponse(ig)
		default:
			// Multiple matches — require --anvil to disambiguate.
			anvils := make([]string, len(matches))
			for i, m := range matches {
				anvils[i] = m.Anvil
			}
			return errorResponse(fmt.Sprintf("ingot %s found in multiple anvils (%s): use --anvil to disambiguate", p.BeadID, strings.Join(anvils, ", ")))
		}

	case "request_status":
		// Resolve the request_id handed back with a "queued" response to its
		// terminal outcome. This is what stops an async failure from being
		// silently discarded: the caller (Hearth 2.0 after a 202) polls here
		// and converts a pending action into success or a visible error.
		var p ipc.RequestStatusPayload
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return errorResponse("invalid payload: " + err.Error())
		}
		if strings.TrimSpace(p.RequestID) == "" {
			return errorResponse("request_id is required")
		}
		out := ipc.RequestStatusResponse{RequestID: p.RequestID, State: ipc.RequestStateUnknown}
		// An unknown or evicted ID is reported as "unknown", not as an error:
		// the record is bounded, so a stale tab must not read a dropped
		// record as a failure.
		if outcome, ok := d.reqTracker.Outcome(p.RequestID); ok {
			out.State = outcome.State
			out.Message = outcome.Message
			if !outcome.UpdatedAt.IsZero() {
				out.UpdatedAt = outcome.UpdatedAt.Format(time.RFC3339)
			}
		}
		return okResponse(out)

	case "preview_start":
		var p ipc.PreviewActionPayload
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return errorResponse("invalid preview_start payload")
		}
		return d.handlePreviewStart(p)

	case "preview_stop":
		var p ipc.PreviewActionPayload
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return errorResponse("invalid preview_stop payload")
		}
		return d.handlePreviewStop(p)

	// "previews" is the web dashboard's name for this read and "preview_list"
	// the CLI's; they are one command with one payload (ipc.PreviewListResponse
	// aliases ipc.PreviewsResponse), not two views of the same state.
	case "previews", "preview_list":
		return d.handlePreviewList()

	case "preview_resolve":
		var p ipc.PreviewResolvePayload
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return errorResponse("invalid preview_resolve payload")
		}
		return d.handlePreviewResolve(p)

	case "preview_quest_run":
		var p ipc.PreviewQuestRunPayload
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return errorResponse("invalid preview_quest_run payload")
		}
		return d.handlePreviewQuestRun(p)

	case "preview_quest_status":
		var p ipc.PreviewQuestStatusPayload
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return errorResponse("invalid preview_quest_status payload")
		}
		return d.handlePreviewQuestStatus(p)

	case "wicket_status":
		cfg := d.cfg.Load()
		enabled := cfg.Settings.WicketEnabled

		// Compute the effective interval, applying the same defaulting logic as
		// the Wicket monitor (interval <= 0 falls back to a 15m default).
		effectiveInterval := cfg.Settings.WicketInterval
		if effectiveInterval <= 0 {
			effectiveInterval = 15 * time.Minute
		}
		interval := effectiveInterval.String()

		// Collect explicitly configured repos from all anvil configs; count
		// anvils that will derive their repo from git remote at runtime.
		repoSet := make(map[string]struct{})
		derivedAnvils := 0
		for _, anvil := range cfg.Anvils {
			if len(anvil.WicketRepos) > 0 {
				for _, r := range anvil.WicketRepos {
					if r != "" {
						repoSet[r] = struct{}{}
					}
				}
			} else {
				derivedAnvils++
			}
		}
		repos := make([]string, 0, len(repoSet))
		for r := range repoSet {
			repos = append(repos, r)
		}
		sort.Strings(repos)

		// Count wicket issues by lifecycle state using cheap DB-side COUNT queries.
		counts := make(map[string]int)
		for _, st := range []string{"pending", "bead_created", "ask_clarify", "needs_human"} {
			n, err := d.db.CountWicketIssues(state.ListWicketIssuesOpts{State: st})
			if err != nil {
				return errorResponse("counting wicket issues: " + err.Error())
			}
			counts[st] = n
		}

		lastScan, err := d.db.LastWicketScanAt()
		if err != nil {
			return errorResponse("querying last scan time: " + err.Error())
		}

		payload := ipc.WicketStatusPayload{
			Enabled:        enabled,
			Interval:       interval,
			MonitoredRepos: repos,
			DerivedAnvils:  derivedAnvils,
			IssueCounts:    counts,
			LastScanAt:     lastScan,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return errorResponse("marshalling wicket status: " + err.Error())
		}
		return ipc.Response{Type: "ok", Payload: data}

	case "queue_clarify":
		return d.handleQueueClarify(cmd)
	case "queue_unclarify":
		return d.handleQueueUnclarify(cmd)
	case "queue_retry":
		return d.handleQueueRetry(cmd)
	case "queue_clear":
		return d.handleQueueClear(cmd)
	case "queue_stop":
		return d.handleQueueStop(cmd)
	case "steer_bead":
		return d.handleSteerBead(cmd)
	case "pause_bead":
		return d.handlePauseBead(cmd)
	case "resume_bead":
		return d.handleResumeBead(cmd)
	case "resume_bead_with_message":
		return d.handleResumeBeadWithMessage(cmd)

	case "revoke_web_sessions":
		// Incident-response escape hatch: drop every web session so all
		// signed-in users must re-authenticate. Safe when the web server is
		// disabled — the table simply has no rows.
		n, err := d.db.DeleteAllWebSessions()
		if err != nil {
			d.logger.Error("revoke web sessions failed", "error", err)
			return errorResponse("failed to revoke web sessions: " + err.Error())
		}
		msg := fmt.Sprintf("revoked %d web session(s)", n)
		d.logger.Info("web sessions revoked", "count", n)
		if logErr := d.db.LogEvent(state.EventWebSessionsRevoked, msg, "", ""); logErr != nil {
			d.logger.Warn("failed to log web session revocation event", "error", logErr)
		}
		return okResponse(map[string]any{
			"revoked": n,
			"message": msg,
		})

	default:
		return errorResponse("unknown command: " + cmd.Type)
	}
}

// errorResponse builds an IPC error response carrying an actionable message
// string in the {"message": ...} envelope every surface (IPC/Web/CLI) unwraps.
// It is the single constructor for the error envelope that was previously
// hand-rolled at ~dozens of sites across the IPC switch.
func errorResponse(msg string) ipc.Response {
	data, _ := json.Marshal(map[string]string{"message": msg})
	return ipc.Response{Type: "error", Payload: data}
}

// okResponse builds an IPC success response by marshalling payload into the
// response envelope. Like the inline json.Marshal it replaces, a marshal error
// yields an empty payload rather than surfacing an error — every payload passed
// here is a plain map/struct that cannot fail to encode.
func okResponse(payload any) ipc.Response {
	data, _ := json.Marshal(payload)
	return ipc.Response{Type: "ok", Payload: data}
}

// steerErrorResponse builds an error IPC response carrying an actionable
// message string, matching the {"message": ...} shape every steer surface
// (IPC/Web/CLI) unwraps.
func steerErrorResponse(msg string) ipc.Response {
	return errorResponse(msg)
}

// workerSessionNonClaude reports whether a worker's recorded session is
// positively NOT a Claude session, i.e. cannot be resumed by a steer message.
// Only Claude reports a session_id (and a claude-* model), so steering any
// other provider would fail to resume and escalate the bead instead.
//
// Because the session_id and model are only persisted to state.db AFTER a spawn
// completes, a just-started Claude spawn has neither recorded yet. To avoid
// falsely rejecting steering of a live Claude spawn (the common steer mode A
// case), this only returns true when there is a positively non-Claude signal: a
// recorded model that is not a claude-* model, with no captured session_id. An
// as-yet-unrecorded session (both empty) is treated as steerable.
func workerSessionNonClaude(w *state.Worker) bool {
	if w == nil {
		return false
	}
	if w.SessionID != "" {
		// Only Claude reports a session_id — a recorded one means Claude.
		return false
	}
	if w.Model == "" {
		// Nothing recorded yet (spawn still starting) — be optimistic.
		return false
	}
	return !strings.Contains(strings.ToLower(w.Model), "claude")
}

// handleSteerBead implements the "steer_bead" IPC verb: it delivers a human
// steering message to a bead's in-flight pipeline. Validation mirrors the
// shared steer semantics used by the Web and CLI surfaces: the bead must have
// an active registered control handle (a running pipeline), the message must be
// non-empty, and the session must be a Claude session (only Claude reports a
// resumable session_id). On success the message is pushed into the control
// handle's steer mailbox and, if a Smith spawn is currently running, that spawn
// is interrupted so the message is consumed immediately (mode A); otherwise the
// message is picked up between spawns (mode B). The bead_steered event and note
// persistence happen inside the pipeline when the message is actually consumed.
func (d *Daemon) handleSteerBead(cmd ipc.Command) ipc.Response {
	var sp ipc.SteerBeadPayload
	if err := json.Unmarshal(cmd.Payload, &sp); err != nil {
		return steerErrorResponse("invalid steer_bead payload")
	}
	beadID := strings.TrimSpace(sp.BeadID)
	message := strings.TrimSpace(sp.Message)
	if beadID == "" {
		return steerErrorResponse("bead_id is required")
	}
	if message == "" {
		return steerErrorResponse("steer message must not be empty")
	}

	ctrl, ok := d.lookupControlHandle(beadID)
	if !ok {
		return steerErrorResponse(fmt.Sprintf("no active pipeline for bead %s; steering requires a running Smith worker", beadID))
	}

	// Reject a positively non-Claude session up front so the caller gets a clear
	// error instead of the pipeline silently escalating an unresumable steer.
	if w, err := d.db.GetWorker(ctrl.workerID); err == nil && workerSessionNonClaude(w) {
		return steerErrorResponse(fmt.Sprintf("bead %s is not running a Claude session (model %q); steering is only supported for Claude sessions", beadID, w.Model))
	}

	// Snapshot the live-spawn state BEFORE pushing to the mailbox: pushSteer
	// feeds waitSmithWithSteer's select, which can consume the message,
	// interrupt the spawn, and clear SpawnLive(false) before we read it back.
	wasLive := ctrl.hasLiveSpawn()

	if !ctrl.pushSteer(message) {
		return steerErrorResponse(fmt.Sprintf("steer mailbox is full for bead %s; try again shortly", beadID))
	}

	mode := "queued for the next spawn (mode B)"
	if wasLive {
		mode = "interrupting the running spawn (mode A)"
	}
	d.logger.Info("steer delivered", "bead", beadID, "worker", ctrl.workerID, "mode", mode)

	data, _ := json.Marshal(map[string]string{
		"message": fmt.Sprintf("steer message delivered to bead %s — %s", beadID, mode),
	})
	return ipc.Response{Type: "ok", Payload: data}
}

// handlePauseBead implements the "pause_bead" IPC verb: it requests that a
// bead's in-flight pipeline park its currently running Claude spawn. Validation
// follows the shared paused-status state machine (state.CanTransitionPause): the
// bead must have an active registered control handle (a running pipeline) whose
// worker row is in the running status. On success the pause request is
// dispatched into the pipeline goroutine via the control handle; that goroutine
// performs the graceful interrupt, records the session/iteration state,
// transitions the worker to paused, and parks awaiting a resume_bead. The
// handler itself does not mutate the worker status — it only validates the
// transition and dispatches the request. Error paths: bead not found (no active
// pipeline / missing worker row) and illegal transition (worker not running).
func (d *Daemon) handlePauseBead(cmd ipc.Command) ipc.Response {
	var pp ipc.PauseBeadPayload
	if err := json.Unmarshal(cmd.Payload, &pp); err != nil {
		return steerErrorResponse("invalid pause_bead payload")
	}
	beadID := strings.TrimSpace(pp.BeadID)
	if beadID == "" {
		return steerErrorResponse("bead_id is required")
	}

	ctrl, ok := d.lookupControlHandle(beadID)
	if !ok {
		return steerErrorResponse(fmt.Sprintf("bead %s not found; pausing requires an active pipeline", beadID))
	}

	w, err := d.db.GetWorker(ctrl.workerID)
	if err != nil {
		return steerErrorResponse(fmt.Sprintf("bead %s not found; no worker row for its pipeline", beadID))
	}
	if !state.CanTransitionPause(w.Status, state.WorkerPaused) {
		return steerErrorResponse(fmt.Sprintf("bead %s cannot be paused from status %q; only a running bead may be paused", beadID, w.Status))
	}

	if !ctrl.requestPause() {
		return steerErrorResponse(fmt.Sprintf("a pause is already pending for bead %s", beadID))
	}
	d.logger.Info("pause requested", "bead", beadID, "worker", ctrl.workerID)

	data, _ := json.Marshal(ipc.PauseBeadResponse{
		BeadID:  beadID,
		Status:  string(state.WorkerPaused),
		Message: fmt.Sprintf("pause requested for bead %s", beadID),
	})
	return ipc.Response{Type: "ok", Payload: data}
}

// handleResumeBead implements the "resume_bead" IPC verb: it requests that a
// paused bead's pipeline resume. The resume message is optional and defaults to
// DefaultResumeMessage when the caller omits it (or supplies only whitespace).
//
// Two paths are supported:
//   - WARM resume: a live pipeline goroutine is still parked (its control handle
//     is registered). Validation follows the shared paused-status state machine
//     (state.CanTransitionPause) — the worker row must be paused — and the resume
//     request is dispatched into the parked goroutine via the handle, which
//     respawns `claude --resume <session>` with the message and continues.
//   - COLD resume (daemon restart): no control handle exists because the parked
//     goroutine did not survive the restart, but the paused worker row and its
//     worktree did. handleResumeBead falls back to coldResumePausedWorker, which
//     re-dispatches the bead into a fresh pipeline seeded to resume the persisted
//     session in the retained worktree.
//
// Error paths: bead not found (no live pipeline AND no paused worker row) and
// illegal transition (a live worker that is not paused).
func (d *Daemon) handleResumeBead(cmd ipc.Command) ipc.Response {
	var rp ipc.ResumeBeadPayload
	if err := json.Unmarshal(cmd.Payload, &rp); err != nil {
		return steerErrorResponse("invalid resume_bead payload")
	}
	beadID := strings.TrimSpace(rp.BeadID)
	if beadID == "" {
		return steerErrorResponse("bead_id is required")
	}
	message := strings.TrimSpace(rp.Message)
	if message == "" {
		message = DefaultResumeMessage
	}

	ctrl, ok := d.lookupControlHandle(beadID)
	if !ok {
		// No live pipeline goroutine is parked for this bead. This is the
		// daemon-restart case: the paused worker row and worktree survived the
		// restart but the parked goroutine did not. Fall back to a COLD resume,
		// which re-dispatches the bead into a fresh pipeline seeded to resume the
		// persisted Claude session in the retained worktree.
		return d.coldResumePausedWorker(beadID, message)
	}

	w, err := d.db.GetWorker(ctrl.workerID)
	if err != nil {
		return steerErrorResponse(fmt.Sprintf("bead %s not found; no worker row for its pipeline", beadID))
	}
	if !state.CanTransitionPause(w.Status, state.WorkerRunning) {
		return steerErrorResponse(fmt.Sprintf("bead %s cannot be resumed from status %q; only a paused bead may be resumed", beadID, w.Status))
	}

	if !ctrl.requestResume(message) {
		return steerErrorResponse(fmt.Sprintf("a resume is already pending for bead %s", beadID))
	}
	d.logger.Info("resume requested", "bead", beadID, "worker", ctrl.workerID)

	data, _ := json.Marshal(ipc.ResumeBeadResponse{
		BeadID:  beadID,
		Status:  string(state.WorkerRunning),
		Message: fmt.Sprintf("resume requested for bead %s", beadID),
	})
	return ipc.Response{Type: "ok", Payload: data}
}

// recoverPausedWorkers surfaces beads that were paused before a daemon restart.
// Their worker rows (status 'paused') and worktrees survive a restart, but the
// parked pipeline goroutines do not, so they can no longer be resumed via a live
// control handle. NeedsAttentionBeads already lists paused workers with
// resume/discard actions and orphan recovery now skips them, so this function's
// job is observability: it logs each surviving paused worker and records a
// bead_recovered event so the restart-while-paused transition is auditable.
// Returns the number of paused workers surfaced.
func (d *Daemon) recoverPausedWorkers() int {
	paused, err := d.db.PausedWorkers()
	if err != nil {
		d.logger.Error("failed to query paused workers on startup", "error", err)
		return 0
	}
	for _, w := range paused {
		d.logger.Info("paused bead survived daemon restart — awaiting resume or discard",
			"bead", w.BeadID, "anvil", w.Anvil, "worker", w.ID, "session", w.SessionID)
		_ = d.db.LogEvent(state.EventBeadRecovered,
			fmt.Sprintf("Paused bead %s survived daemon restart; resume or discard from Needs Attention", w.BeadID),
			w.BeadID, w.Anvil)
	}
	return len(paused)
}

// coldResumePausedWorker resumes a bead whose parked pipeline goroutine did not
// survive a daemon restart. It reconstructs the resume state from the persisted
// paused worker row (session_id + model + anvil), re-registers a control handle
// reusing the existing worker ID, and re-dispatches the bead into a fresh
// pipeline seeded (via pipeline.ResumeSession) to resume the recorded Claude
// session in the RETAINED worktree. The bead is already in_progress in bd (a
// paused bead is never released), so no re-claim is performed.
//
// This is the cold sibling of the warm resume path: a warm resume signals a live
// parked goroutine through its control handle; a cold resume rebuilds that
// goroutine. Both ultimately respawn `claude --resume <session>` via the same
// pipeline machinery.
func (d *Daemon) coldResumePausedWorker(beadID, message string) ipc.Response {
	w, err := d.db.PausedWorkerByBeadID(beadID)
	if err != nil {
		return steerErrorResponse(fmt.Sprintf("failed to look up paused worker for bead %s: %v", beadID, err))
	}
	if w == nil {
		return steerErrorResponse(fmt.Sprintf("bead %s not found; resuming requires a running or paused pipeline", beadID))
	}

	anvilCfg, ok := d.cfg.Load().Anvils[w.Anvil]
	if !ok || anvilCfg.Path == "" {
		return steerErrorResponse(fmt.Sprintf("anvil %q for paused bead %s not found or has no path", w.Anvil, beadID))
	}

	// Reserve the in-flight slot so a concurrent poll/run_bead cannot double
	// dispatch this bead while we set up the resume goroutine.
	if _, inFlight := d.activeBeads.LoadOrStore(beadID, true); inFlight {
		return steerErrorResponse(fmt.Sprintf("bead %s is already in flight", beadID))
	}

	// Reconstruct the bead from bd so the pipeline has the full context it needs
	// for any post-resume iterations (Warden feedback → fresh Smith prompt).
	fetchCtx, cancel := context.WithTimeout(d.runCtx, executil.BdTimeout())
	defer cancel()
	bead, err := crucible.FetchBead(fetchCtx, beadID, anvilCfg.Path)
	if err != nil {
		d.releaseBeadSlot(beadID)
		return steerErrorResponse(fmt.Sprintf("failed to fetch bead %s for resume: %v", beadID, err))
	}
	bead.Anvil = w.Anvil

	// A captured session_id means a Claude session (only Claude reports one), so
	// the resume must respawn with Claude and the recorded model. When empty, the
	// pipeline folds the resume message into a fresh prompt instead.
	resume := &pipeline.ResumeSession{
		SessionID: w.SessionID,
		Provider:  provider.Provider{Kind: provider.Claude, Model: w.Model},
		Message:   message,
	}

	// Re-register a control handle reusing the existing worker ID so the resumed
	// pipeline can be paused/steered again.
	ctrl := newControlHandle(w.ID)
	d.registerControlHandle(beadID, ctrl)

	_ = d.db.LogEvent(state.EventBeadResumed,
		fmt.Sprintf("Cold resume of paused bead %s after daemon restart (session %s)", beadID, w.SessionID),
		beadID, w.Anvil)
	d.logger.Info("cold-resuming paused bead after restart",
		"bead", beadID, "anvil", w.Anvil, "worker", w.ID, "session", w.SessionID)

	// Reserve in-flight spend for the resumed worker so it counts against the
	// daily_cost_limit gate like any other active worker (Forge-s3w7).
	resumeReservation := d.reserveWorkerCost(d.perWorkerCostEstimate(d.cfg.Load()))
	d.wg.Add(1)
	go d.dispatchBead(context.Background(), bead, anvilCfg, w.ID, ctrl, resume, resumeReservation)

	data, _ := json.Marshal(ipc.ResumeBeadResponse{
		BeadID:  beadID,
		Status:  string(state.WorkerRunning),
		Message: fmt.Sprintf("resuming paused bead %s after restart", beadID),
	})
	return ipc.Response{Type: "ok", Payload: data}
}

// handleResumeBeadWithMessage implements the "resume_bead_with_message" IPC
// verb: it resumes a needs-attention bead whose worktree was torn down but whose
// forge/<bead> branch survives, seeding the resumed (or fresh-fallback) session
// with an operator message. It mirrors handleSteerBead's shape — keyed purely by
// bead id, message optional — and delegates the heavy lifting (resumable-worker
// lookup, precondition validation, worktree recreation, dispatch) to the
// ResumeBeadWithMessage entrypoint. The daemon returns an actionable error when
// the bead already has a live pipeline (callers should use resume_bead), has no
// resumable worker row, or its resume preconditions are unmet.
func (d *Daemon) handleResumeBeadWithMessage(cmd ipc.Command) ipc.Response {
	var rp ipc.ResumeBeadWithMessagePayload
	if err := json.Unmarshal(cmd.Payload, &rp); err != nil {
		return steerErrorResponse("invalid resume_bead_with_message payload")
	}
	beadID := strings.TrimSpace(rp.BeadID)
	if beadID == "" {
		return steerErrorResponse("bead_id is required")
	}
	message := strings.TrimSpace(rp.Message)

	workerID, err := d.ResumeBeadWithMessage(beadID, message)
	if err != nil {
		return steerErrorResponse(err.Error())
	}

	d.logger.Info("resume-with-message delivered", "bead", beadID, "worker", workerID)
	data, _ := json.Marshal(ipc.ResumeBeadWithMessageResponse{
		BeadID:   beadID,
		WorkerID: workerID,
		Message:  fmt.Sprintf("resume-with-message dispatched for bead %s (worker %s)", beadID, workerID),
	})
	return ipc.Response{Type: "ok", Payload: data}
}

// ResumeBeadWithMessage resumes a bead whose worktree was torn down but whose
// forge/<bead> branch survives — the needs-attention resume-with-message case.
// It locates the bead's most recent resumable worker row (branch + session_id),
// validates the resume preconditions via state.Worker.ResumeState, and
// re-dispatches the bead into a fresh pipeline seeded (pipeline.ResumeSession
// with RecreateFromBranch) to recreate the worktree at its exact original path
// from the surviving branch (worktree.CreateFromBranch) and resume the recorded
// Claude session with message. If the transcript is missing / the resume errors,
// or the branch is gone, the pipeline falls back to a fresh session seeded with
// bd context + message.
//
// It is the callable entrypoint the resume-with-message IPC verb invokes. The
// bead is left in_progress (a needs-attention bead is not released), so no
// re-claim is performed. message may be empty (defaults to DefaultResumeMessage).
// Returns the reused worker ID on success.
func (d *Daemon) ResumeBeadWithMessage(beadID, message string) (string, error) {
	beadID = strings.TrimSpace(beadID)
	if beadID == "" {
		return "", fmt.Errorf("bead_id is required")
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = DefaultResumeMessage
	}

	// A live pipeline is already running/parked for this bead: its worktree is
	// intact, so this recreate-from-branch path does not apply. The warm/cold
	// resume path (handleResumeBead) owns that case.
	if _, ok := d.lookupControlHandle(beadID); ok {
		return "", fmt.Errorf("bead %s already has a live pipeline; use resume, not resume-with-message", beadID)
	}

	w, err := d.db.ResumableWorkerByBeadID(beadID)
	if err != nil {
		return "", fmt.Errorf("looking up resumable worker for bead %s: %w", beadID, err)
	}
	if w == nil {
		return "", fmt.Errorf("bead %s has no resumable worker row (no recorded branch + session)", beadID)
	}
	rs, err := w.ResumeState()
	if err != nil {
		return "", fmt.Errorf("bead %s cannot be resumed: %w", beadID, err)
	}

	anvilCfg, ok := d.cfg.Load().Anvils[rs.Anvil]
	if !ok || anvilCfg.Path == "" {
		return "", fmt.Errorf("anvil %q for bead %s not found or has no path", rs.Anvil, beadID)
	}

	// Reserve the in-flight slot so a concurrent poll / run_bead cannot double
	// dispatch this bead while we set up the resume goroutine.
	if _, inFlight := d.activeBeads.LoadOrStore(beadID, true); inFlight {
		return "", fmt.Errorf("bead %s is already in flight", beadID)
	}

	// Reconstruct the bead from bd so the pipeline has full context for any
	// post-resume iterations and for the fresh-session fallback prompt.
	fetchCtx, cancel := context.WithTimeout(d.runCtx, executil.BdTimeout())
	defer cancel()
	bead, err := crucible.FetchBead(fetchCtx, beadID, anvilCfg.Path)
	if err != nil {
		d.releaseBeadSlot(beadID)
		return "", fmt.Errorf("failed to fetch bead %s for resume: %w", beadID, err)
	}
	bead.Anvil = rs.Anvil

	// The session_id is Claude's (only Claude reports one), so resume with Claude
	// and the recorded model. RecreateFromBranch tells the pipeline to rebuild the
	// worktree from the surviving branch before resuming.
	resume := &pipeline.ResumeSession{
		SessionID:          rs.SessionID,
		Provider:           provider.Provider{Kind: provider.Claude, Model: w.Model},
		Message:            message,
		RecreateFromBranch: true,
		Branch:             rs.Branch,
		WorktreePath:       d.worktreeMgr.WorktreePath(anvilCfg.Path, beadID),
	}

	// Re-register a control handle reusing the existing worker ID so the resumed
	// pipeline can be paused/steered again, and the pipeline's InsertWorker
	// overwrites the stale row rather than creating a duplicate.
	ctrl := newControlHandle(w.ID)
	d.registerControlHandle(beadID, ctrl)

	_ = d.db.LogEvent(state.EventBeadResumed,
		fmt.Sprintf("Resume-with-message for bead %s: recreating worktree from branch %s (session %s)", beadID, rs.Branch, rs.SessionID),
		beadID, rs.Anvil)
	d.logger.Info("resuming bead with message (recreating worktree from surviving branch)",
		"bead", beadID, "anvil", rs.Anvil, "worker", w.ID, "branch", rs.Branch, "session", rs.SessionID)

	// Reserve in-flight spend for the resumed worker so it counts against the
	// daily_cost_limit gate like any other active worker.
	resumeReservation := d.reserveWorkerCost(d.perWorkerCostEstimate(d.cfg.Load()))
	d.wg.Add(1)
	go d.dispatchBead(context.Background(), bead, anvilCfg, w.ID, ctrl, resume, resumeReservation)

	return w.ID, nil
}

// handleQueueClarify implements the "queue_clarify" IPC verb: marks a bead
// as needing human clarification. Required: bead_id, anvil_name, note.
// Optional: forge_id (multi-forge safety; rejected if mismatched).
func (d *Daemon) handleQueueClarify(cmd ipc.Command) ipc.Response {
	var qp ipc.QueueActionPayload
	if err := json.Unmarshal(cmd.Payload, &qp); err != nil {
		msg, _ := json.Marshal(map[string]string{"message": "invalid queue_clarify payload"})
		return ipc.Response{Type: "error", Payload: msg}
	}
	if qp.AnvilName != "" {
		if canonical, _, ok := d.resolveAnvilConfig(qp.AnvilName); ok {
			qp.AnvilName = canonical
		}
	}
	if err := queueactions.Clarify(context.Background(), d.queueActionsHandle(), queueactions.Params{
		BeadID:    qp.BeadID,
		ForgeID:   qp.ForgeID,
		AnvilName: qp.AnvilName,
		Note:      qp.Note,
	}); err != nil {
		msg, _ := json.Marshal(map[string]string{"message": queueActionsErrorMessage("set clarification", err)})
		return ipc.Response{Type: "error", Payload: msg}
	}
	d.logger.Info("bead marked as clarification_needed via queue_clarify",
		"bead", qp.BeadID, "anvil", qp.AnvilName, "reason", strings.TrimSpace(qp.Note))
	data, _ := json.Marshal(map[string]string{"message": "clarification_needed set"})
	return ipc.Response{Type: "ok", Payload: data}
}

// handleQueueUnclarify implements the "queue_unclarify" IPC verb: clears the
// clarification_needed flag so a bead can be dispatched again. Required:
// bead_id, anvil_name. Note is optional and appears in the audit event.
func (d *Daemon) handleQueueUnclarify(cmd ipc.Command) ipc.Response {
	var qp ipc.QueueActionPayload
	if err := json.Unmarshal(cmd.Payload, &qp); err != nil {
		msg, _ := json.Marshal(map[string]string{"message": "invalid queue_unclarify payload"})
		return ipc.Response{Type: "error", Payload: msg}
	}
	if qp.AnvilName != "" {
		if canonical, _, ok := d.resolveAnvilConfig(qp.AnvilName); ok {
			qp.AnvilName = canonical
		}
	}
	if err := queueactions.Unclarify(context.Background(), d.queueActionsHandle(), queueactions.Params{
		BeadID:    qp.BeadID,
		ForgeID:   qp.ForgeID,
		AnvilName: qp.AnvilName,
		Note:      qp.Note,
	}); err != nil {
		msg, _ := json.Marshal(map[string]string{"message": queueActionsErrorMessage("clear clarification", err)})
		return ipc.Response{Type: "error", Payload: msg}
	}
	d.logger.Info("clarification_needed cleared via queue_unclarify",
		"bead", qp.BeadID, "anvil", qp.AnvilName)
	data, _ := json.Marshal(map[string]string{"message": "clarification_needed cleared"})
	return ipc.Response{Type: "ok", Payload: data}
}

// handleQueueRetry implements the "queue_retry" IPC verb: resets the dispatch
// circuit breaker so the bead re-enters the queue. This is the thin primitive
// — it does not shell out to bd or kick the poller; callers that need those
// orchestration steps continue to use retry_bead.
func (d *Daemon) handleQueueRetry(cmd ipc.Command) ipc.Response {
	var qp ipc.QueueActionPayload
	if err := json.Unmarshal(cmd.Payload, &qp); err != nil {
		msg, _ := json.Marshal(map[string]string{"message": "invalid queue_retry payload"})
		return ipc.Response{Type: "error", Payload: msg}
	}
	if qp.AnvilName != "" {
		if canonical, _, ok := d.resolveAnvilConfig(qp.AnvilName); ok {
			qp.AnvilName = canonical
		}
	}
	hadCircuitBreaker, err := queueactions.Retry(context.Background(), d.queueActionsHandle(), queueactions.Params{
		BeadID:    qp.BeadID,
		ForgeID:   qp.ForgeID,
		AnvilName: qp.AnvilName,
		Note:      qp.Note,
	})
	if err != nil {
		msg, _ := json.Marshal(map[string]string{"message": queueActionsErrorMessage("retry bead", err)})
		return ipc.Response{Type: "error", Payload: msg}
	}
	d.logger.Info("retry reset for bead via queue_retry",
		"bead", qp.BeadID, "anvil", qp.AnvilName, "circuit_breaker_cleared", hadCircuitBreaker)
	respMsg := "retry reset"
	if hadCircuitBreaker {
		respMsg = "retry state reset"
	}
	data, _ := json.Marshal(map[string]string{"message": respMsg})
	return ipc.Response{Type: "ok", Payload: data}
}

// handleQueueClear implements the "queue_clear" IPC verb: drops needs-attention
// flags from a bead's retry row without re-dispatching it. Idempotent.
func (d *Daemon) handleQueueClear(cmd ipc.Command) ipc.Response {
	var qp ipc.QueueActionPayload
	if err := json.Unmarshal(cmd.Payload, &qp); err != nil {
		msg, _ := json.Marshal(map[string]string{"message": "invalid queue_clear payload"})
		return ipc.Response{Type: "error", Payload: msg}
	}
	if qp.AnvilName != "" {
		if canonical, _, ok := d.resolveAnvilConfig(qp.AnvilName); ok {
			qp.AnvilName = canonical
		}
	}
	if err := queueactions.Clear(context.Background(), d.queueActionsHandle(), queueactions.Params{
		BeadID:    qp.BeadID,
		ForgeID:   qp.ForgeID,
		AnvilName: qp.AnvilName,
		Note:      qp.Note,
	}); err != nil {
		msg, _ := json.Marshal(map[string]string{"message": queueActionsErrorMessage("clear needs-attention flags", err)})
		return ipc.Response{Type: "error", Payload: msg}
	}
	d.logger.Info("needs-attention flags cleared via queue_clear",
		"bead", qp.BeadID, "anvil", qp.AnvilName)
	data, _ := json.Marshal(map[string]string{"message": "needs-attention flags cleared"})
	return ipc.Response{Type: "ok", Payload: data}
}

// handleQueueStop implements the "queue_stop" IPC verb — the verb the web GUI
// and Hearth resolve page use. It shares one implementation (stopBead) with the
// stop_bead verb so both release the bd claim: a bead stopped from the UI is
// returned to open/unassigned in bd, becoming visible to `bd ready` again.
// ForgeID is passed through so the multi-forge safety check applies.
func (d *Daemon) handleQueueStop(cmd ipc.Command) ipc.Response {
	var qp ipc.QueueActionPayload
	if err := json.Unmarshal(cmd.Payload, &qp); err != nil {
		return errorResponse("invalid queue_stop payload")
	}
	return d.stopBead(stopBeadParams{
		beadID:       qp.BeadID,
		forgeID:      qp.ForgeID,
		anvil:        qp.AnvilName,
		reason:       qp.Note,
		releaseClaim: true,
	})
}

// stopBeadParams carries the normalized inputs to stopBead. releaseClaim
// controls whether the bd claim is released after the worker is terminated;
// both stop verbs set it true so UI and CLI stops behave identically. It is
// retained as a knob so a future non-releasing caller (kept for compat) can
// stop a worker without touching bd.
type stopBeadParams struct {
	beadID       string
	forgeID      string
	anvil        string
	reason       string
	releaseClaim bool
}

// stopBead is the single implementation behind both stop verbs (stop_bead and
// queue_stop). It terminates any running worker via queueactions.Stop, releases
// the in-memory bead slot, and — when releaseClaim is set — asynchronously
// releases the bd claim (`bd update --status=open --assignee=`) so the poller
// sees the bead again.
//
// The bd release shells out to bd, so it runs in a goroutine and the method
// returns a queued acknowledgement the caller polls for completion. When
// releaseClaim is false the response is returned synchronously.
func (d *Daemon) stopBead(p stopBeadParams) ipc.Response {
	if strings.TrimSpace(p.beadID) == "" || strings.TrimSpace(p.anvil) == "" {
		return errorResponse("bead_id and anvil are required")
	}
	// Canonicalise the anvil name and resolve its config. Releasing the bd
	// claim shells out to `bd update` inside the anvil's checkout, so the path
	// is required even though queueactions.Stop needs only the name.
	anvilName, anvilCfg, ok := d.resolveAnvilConfig(p.anvil)
	if !ok {
		return errorResponse(fmt.Sprintf("anvil %q not found", p.anvil))
	}
	if p.releaseClaim && anvilCfg.Path == "" {
		return errorResponse(fmt.Sprintf("anvil %q has no path configured", anvilName))
	}

	terminatedWorkerID, err := queueactions.Stop(context.Background(), d.queueActionsHandle(), queueactions.Params{
		BeadID:    p.beadID,
		ForgeID:   p.forgeID,
		AnvilName: anvilName,
		Note:      p.reason,
	})
	if err != nil {
		return errorResponse(queueActionsErrorMessage("stop bead", err))
	}
	if terminatedWorkerID != "" {
		d.logger.Info("killed worker for stopped bead", "worker", terminatedWorkerID, "bead", p.beadID, "anvil", anvilName)
	}

	// Use releaseBeadSlotIfOwner (via the shared helper) to avoid deleting a
	// handle registered by a new dispatch if re-dispatch races with cleanup.
	d.releaseStoppedBeadSlot(p.beadID)

	if !p.releaseClaim {
		return okResponse(map[string]string{"message": fmt.Sprintf("bead %s stopped", p.beadID)})
	}

	reqID, _ := d.reqTracker.Track()
	beadID := p.beadID
	anvilPath := anvilCfg.Path
	reason := queueactions.SanitizeControl(strings.TrimSpace(p.reason))
	if reason == "" {
		reason = "manually stopped"
	}
	go func() {
		releaseCmd, releaseCancel := executil.BdCommand(d.runCtx, "update", beadID, "--status=open", "--assignee=", "--json")
		defer releaseCancel()
		releaseCmd.Dir = anvilPath
		if out, err := releaseCmd.CombinedOutput(); err != nil {
			d.logger.Warn("bd update failed when releasing stopped bead", "bead", beadID, "error", err, "output", strings.TrimSpace(string(out)))
			d.completeAsync(reqID, errorResponse(fmt.Sprintf("bead stopped but bd release failed: %v", err)))
			return
		}
		d.logger.Info("bead stopped", "bead", beadID, "anvil", anvilName, "reason", reason)
		d.completeAsync(reqID, okResponse(map[string]string{"message": fmt.Sprintf("bead %s stopped", beadID)}))
	}()
	resp, _ := ipc.NewQueuedResponse(reqID, "stopping bead")
	return resp
}

// forwardBusEvents subscribes to the in-process event Bus and pushes every
// logged event to IPC subscribers (the Hearth TUI) via Broadcast, so the TUI's
// event feed streams in real time instead of ticker-polling the events table.
//
// It mirrors FetchEvents' filtering: poll / poll_error rows are skipped because
// anvil health is shown inline in the Queue panel, not the event feed. An
// overflow gap marker is forwarded as an "events_gap" signal telling the TUI to
// re-sync its feed from the DB once, then resume streaming.
//
// The method returns immediately (leaving the legacy poll path intact) when the
// Bus is disabled — settings.bus_enabled off means d.eventBus is nil.
func (d *Daemon) forwardBusEvents(ctx context.Context) {
	if d.eventBus == nil || d.ipc == nil {
		return
	}
	ch, unsubscribe := d.eventBus.Subscribe()
	defer unsubscribe()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.GapMarker {
				d.BroadcastEvent("events_gap", struct{}{})
				continue
			}
			if ev.Type == state.EventPoll || ev.Type == state.EventPollError {
				continue
			}
			d.BroadcastEvent("event_logged", ipc.EventInfo{
				ID:        ev.ID,
				Timestamp: ev.Timestamp.Format(time.RFC3339),
				Type:      string(ev.Type),
				Message:   ev.Message,
				BeadID:    ev.BeadID,
				Anvil:     ev.Anvil,
			})
		}
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

// completeAsync records a result in the RequestTracker and broadcasts a
// daemon event so IPC subscribers (Hearth TUI) can react to the outcome.
func (d *Daemon) completeAsync(requestID string, resp ipc.Response) {
	if !d.reqTracker.Complete(requestID, ipc.CompletionResult{Response: resp}) {
		slog.Warn("async completion for unknown request id", "request_id", requestID, "response_type", resp.Type)
	}
	d.BroadcastEvent("async_complete", map[string]any{
		"request_id": requestID,
		"type":       resp.Type,
		"response":   resp,
	})
}

// readPIDFile reads and parses the daemon PID file. It returns the PID and
// true only when the file exists and holds a valid positive integer. The
// pidfile is now diagnostic metadata and the SIGINT target for `forge down`;
// the IPC socket is the authoritative liveness signal (see IsRunning).
func readPIDFile() (int, bool) {
	pidPath, err := pidFilePath()
	if err != nil {
		return 0, false
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// IsRunning reports whether a Forge daemon is running. The IPC socket is
// authoritative: a daemon that answers a ping is running regardless of the
// pidfile's state. Only when the socket does not answer does IsRunning fall
// back to pidfile-based liveness detection.
func IsRunning() (int, bool) {
	// The socket is authoritative. A successful ping means a daemon is live
	// and serving, full stop. This survives the failure mode that motivated
	// this change: a crash (SIGKILL/OOM) leaves a stale pidfile, the PID is
	// reused by an unrelated process, the staleness heuristic below deletes
	// the pidfile, and a later legitimately-running daemon would otherwise
	// report "not running" even though ~/.forge/forge.sock answers perfectly.
	// Probing the socket first also guarantees the pidfile-deleting staleness
	// heuristic can only run when the socket is already confirmed dead, so it
	// can never delete a live daemon's pidfile. This mirrors what the Windows
	// branch has always done (socket as liveness proxy), now on all platforms.
	if ipc.Ping() {
		// Report the pidfile PID for diagnostics when available; liveness
		// comes from the socket, not this value.
		pid, _ := readPIDFile()
		return pid, true
	}

	// Socket did not answer: fall back to pidfile-based liveness. Because this
	// is reached only after the ping failed, the pidfile-deleting staleness
	// heuristic inside pidfileProcessAlive can never run against a live daemon.
	return pidfileProcessAlive()
}

// pidfileProcessAlive reports whether the daemon PID file points to a live
// forge process, and returns that PID when it does. It performs the full
// staleness validation used as IsRunning's socket-dead fallback: the process
// must exist, be signalable, be a forge binary, and the pidfile must not
// predate the process. When it finds a stale pidfile — one pointing at a
// non-forge process or a newer forge incarnation — it removes the pidfile so
// the next writePID succeeds cleanly and returns (0, false).
//
// It is the shared arbiter of "is the pidfile the real daemon?": IsRunning
// uses it to decide liveness when the socket is silent, and Stop uses it to
// decide whether the pidfile PID is a safe SIGINT target or whether it must
// instead shut the daemon down over the socket.
//
// On Windows, Signal(0) is unsupported and the pidfile cannot be validated
// this way, so it always returns (0, false); callers there rely on the socket.
func pidfileProcessAlive() (int, bool) {
	pidPath, err := pidFilePath()
	if err != nil {
		return 0, false
	}

	pid, ok := readPIDFile()
	if !ok {
		return 0, false
	}

	// Check if process exists
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}

	// On Windows, Signal(0) is not supported; the pidfile is not a usable
	// liveness signal there and callers fall back to the socket.
	if runtime.GOOS == "windows" {
		_ = proc.Release()
		return 0, false
	}

	// On Unix, FindProcess always succeeds; Signal(0) checks liveness.
	err = proc.Signal(syscall.Signal(0))
	if err != nil {
		return 0, false
	}

	// Liveness alone is not enough. In a container PID namespace, low PIDs
	// like init or its children are almost always alive, so a stale pidfile
	// from a killed pod (helm rollout mid-flight, OOM, drain past grace)
	// would otherwise match an unrelated live process and crashloop every
	// retry pod. Verify the process is actually a forge binary; if not,
	// treat the pidfile as stale and remove it so the next writePID
	// succeeds cleanly.
	isForge, identErr := isForgeProcess(pid)
	if identErr != nil {
		slog.Warn("could not verify forge process identity; assuming alive", "pid", pid, "pidfile", pidPath, "err", identErr)
		return pid, true
	}
	if !isForge {
		slog.Warn("stale pidfile points to non-forge process; ignoring", "pid", pid, "pidfile", pidPath)
		if err := os.Remove(pidPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("failed to remove stale pidfile", "pidfile", pidPath, "err", err)
		}
		return 0, false
	}

	// isForge=true is still ambiguous in a container PID namespace: when
	// a pod is recycled, the new container starts with an empty PID space
	// and the new forge daemon almost always reclaims the previous one's
	// low PID (typically 7). /proc/<pid>/comm equals "forge" because the
	// process IS forge — just a different incarnation than the one that
	// wrote the pidfile. Compare the pidfile mtime against the process's
	// start time (derived from /proc/<pid>/ mtime on Linux); if the
	// pidfile predates the process, it belongs to a dead earlier
	// incarnation. A small skew allowance covers the tiny gap between
	// fork() and the first writePID call.
	procStart, procErr := procStartTime(pid)
	pidFileInfo, statErr := os.Stat(pidPath)
	if procErr == nil && statErr == nil {
		const skew = 5 * time.Second
		if pidFileInfo.ModTime().Before(procStart.Add(-skew)) {
			slog.Warn("stale pidfile predates current PID's process; ignoring",
				"pid", pid, "pidfile", pidPath,
				"pidfile_mtime", pidFileInfo.ModTime(),
				"process_start", procStart)
			if err := os.Remove(pidPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				slog.Warn("failed to remove stale pidfile", "pidfile", pidPath, "err", err)
			}
			return 0, false
		}
	}

	return pid, true
}

// Stop sends a graceful shutdown signal to the running daemon. On Unix we send
// SIGINT to the pidfile PID, but only when the pidfile points at a verified-live
// forge process; if it is missing or present-but-stale (e.g. a reused or dead
// PID left after a crash) we fall back to the IPC "shutdown" command. On
// Windows, syscall.SIGINT is not supported, so the IPC "shutdown" command is
// always used.
func Stop() error {
	_, running := IsRunning()
	if !running {
		return fmt.Errorf("no daemon running")
	}

	// Unix: prefer SIGINT to the pidfile PID for graceful shutdown, but only
	// when pidfileProcessAlive confirms the pidfile actually points at a live
	// forge process. A bare readPIDFile is not enough: a present-but-stale
	// pidfile (reused or dead PID after a crash) would otherwise send SIGINT to
	// an unrelated process while the real daemon — reachable over the socket —
	// keeps running. When the pidfile is absent or stale, fall through to the
	// IPC "shutdown" command like Windows does. Never signal PID 0 — on Unix
	// that targets the whole process group.
	if runtime.GOOS != "windows" {
		if pid, ok := pidfileProcessAlive(); ok {
			proc, err := os.FindProcess(pid)
			if err != nil {
				return fmt.Errorf("finding process %d: %w", pid, err)
			}
			if err := proc.Signal(syscall.SIGINT); err != nil {
				return fmt.Errorf("sending shutdown signal to PID %d: %w", pid, err)
			}
			return nil
		}
	}

	// Windows (no SIGINT), or Unix with a missing/stale pidfile: shut down
	// over the socket.
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
//
// When releaseClaim is true, the bead claim is released immediately via bd
// update so the bead becomes available again without waiting for orphan
// recovery. Pass releaseClaim=false for pre-claim failures (e.g. claimBead
// error) where Forge does not own the bead in bd — releasing in those cases
// risks unassigning a bead legitimately held by another instance or a human.
// If the release fails, a warning is logged and orphan recovery is the
// fallback.
//
// On a transient failure (dispatch_failures < MaxDispatchFailures, broken=false),
// the auto_dispatch_tag is preserved so the bead remains visible to `bd ready`
// on tagged-dispatch anvils and can be re-dispatched on the next poll. Stripping
// the tag is reserved for circuit-breaker escalation (broken=true), at which
// point needs_human=1 has been set and the dispatcher skips the bead anyway —
// removing the tag in that case keeps `bd ready` clean.
func (d *Daemon) recordDispatchFailure(beadID, anvil, reason string, releaseClaim bool) {
	count, broken, err := d.db.IncrementDispatchFailures(beadID, anvil, MaxDispatchFailures, reason)
	if err != nil {
		d.logger.Error("failed to record dispatch failure", "bead", beadID, "error", err)
		return
	}
	_ = d.db.LogEvent(state.EventDispatchFailed,
		fmt.Sprintf("Dispatch attempt %d failed for %s: %s", count, beadID, reason),
		beadID, anvil)

	if releaseClaim {
		d.releaseBeadClaim(beadID, anvil, broken)
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

// resolveAnvilConfig performs a case-insensitive lookup of an anvil by name
// against the current config and returns the canonical key, its config, and
// whether a match was found. Handlers that accept a user-provided anvil name
// (e.g. from the CLI) should canonicalise the name before any DB query so a
// caller passing "Munin" still matches the configured "munin".
func (d *Daemon) resolveAnvilConfig(name string) (string, config.AnvilConfig, bool) {
	if name == "" {
		return "", config.AnvilConfig{}, false
	}
	anvils := d.cfg.Load().Anvils
	if cfg, ok := anvils[name]; ok {
		return name, cfg, true
	}
	lower := strings.ToLower(name)
	var (
		matchedKey string
		matchedCfg config.AnvilConfig
		found      bool
	)
	for k, v := range anvils {
		if strings.ToLower(k) != lower {
			continue
		}
		if found {
			return "", config.AnvilConfig{}, false
		}
		matchedKey = k
		matchedCfg = v
		found = true
	}
	if found {
		return matchedKey, matchedCfg, true
	}
	return "", config.AnvilConfig{}, false
}

// restoreBeadAfterPause sets a bead back to status=open with no assignee and,
// if the anvil uses an auto_dispatch_tag, re-applies that label. This is the
// inverse of releaseBeadClaim and is used by the manual retry/resume paths
// (forge queue retry, Hearth crucible resume) so beads on tagged anvils
// become visible to bd ready again instead of sitting silently un-tagged.
func (d *Daemon) restoreBeadAfterPause(ctx context.Context, beadID string, anvilCfg config.AnvilConfig) ([]byte, error) {
	args := []string{"update", beadID, "--status=open", "--assignee="}
	if anvilCfg.AutoDispatchTag != "" {
		args = append(args, "--add-label="+anvilCfg.AutoDispatchTag)
	}
	args = append(args, "--json")
	cmd, cancel := executil.BdCommand(ctx, args...)
	defer cancel()
	cmd.Dir = anvilCfg.Path
	return cmd.CombinedOutput()
}

// releaseBeadClaim releases a claimed bead back to open status (status=open,
// assignee=""). The anvil path is resolved from the current config. If the
// release fails (bd error, missing anvil config), a warning is logged — orphan
// recovery acts as the fallback.
//
// When stripTag is true, the anvil's auto_dispatch_tag is also removed. This
// is reserved for circuit-breaker escalation (needs_human=1 has been set) where
// we want the bead out of the `bd ready` queue. On a transient failure
// (stripTag=false), the tag is preserved so `bd ready` continues to surface the
// bead on tagged-dispatch anvils and the next poll can re-dispatch it. Stripping
// the tag on every transient failure caused tagged-dispatch beads to be silently
// stranded after a single failure (Forge-dua2).
func (d *Daemon) releaseBeadClaim(beadID, anvil string, stripTag bool) {
	anvilCfg, ok := d.cfg.Load().Anvils[anvil]
	if !ok || anvilCfg.Path == "" {
		d.logger.Warn("cannot release bead claim: anvil not found in config", "bead", beadID, "anvil", anvil)
		return
	}

	baseCtx := d.runCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	args := []string{"update", beadID, "--status=open", "--assignee="}
	if stripTag && anvilCfg.AutoDispatchTag != "" {
		args = append(args, "--remove-label="+anvilCfg.AutoDispatchTag)
	}
	args = append(args, "--json")

	var stderrBuf bytes.Buffer
	cmd, cancel := executil.BdCommand(baseCtx, args...)
	defer cancel()
	cmd.Dir = anvilCfg.Path
	cmd.Stderr = &stderrBuf
	out, err := cmd.Output()
	if err != nil {
		d.logger.Warn("failed to release bead claim, orphan recovery will handle it",
			"bead", beadID, "anvil", anvil,
			"error", err,
			"stdout", strings.TrimSpace(string(out)),
			"stderr", strings.TrimSpace(stderrBuf.String()))
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
					type tagFailure struct {
						childID string
						err     error
					}
					var mu sync.Mutex
					var failures []tagFailure

					sem := make(chan struct{}, 4)
					var wg sync.WaitGroup
					for _, sub := range sr.SubBeads {
						wg.Add(1)
						sem <- struct{}{}
						go func() {
							defer wg.Done()
							defer func() { <-sem }()
							if err := d.labelAdder(anvilCfg.Path, sub.ID, anvilCfg.AutoDispatchTag); err != nil {
								d.logger.Warn("failed to copy auto_dispatch tag to child bead",
									"parent", beadID, "child", sub.ID, "tag", anvilCfg.AutoDispatchTag, "error", err)
								mu.Lock()
								failures = append(failures, tagFailure{childID: sub.ID, err: err})
								mu.Unlock()
							} else {
								d.logger.Info("copied auto_dispatch tag to child bead",
									"parent", beadID, "child", sub.ID, "tag", anvilCfg.AutoDispatchTag)
								_ = d.db.LogEvent(state.EventBeadTagged,
									fmt.Sprintf("Label %q propagated to child bead %s from decomposed parent %s", anvilCfg.AutoDispatchTag, sub.ID, beadID),
									sub.ID, anvil)
							}
						}()
					}
					wg.Wait()
					for _, f := range failures {
						reason := fmt.Sprintf("failed to propagate auto_dispatch tag %q to child bead %s: %v",
							anvilCfg.AutoDispatchTag, f.childID, f.err)
						d.recordDispatchFailure(beadID, anvil, reason, true)
					}
				}
			}
		}

		// Auto-close the parent if nothing else depends on it, since the
		// children ARE the work now. If it has dependents, keep it open so
		// those dependents stay blocked until the children complete.
		d.maybeCloseDecomposedParent(bead, anvilCfg, childCount)
		return
	}
	// Decomposition produced no children — preserve the retry record so the bead
	// surfaces in Needs Attention rather than silently disappearing.
	reason := "decomposition produced no child beads"
	if sr != nil && sr.Reason != "" {
		reason = reason + ": " + sr.Reason
	}
	d.logger.Warn("bead decomposition produced no children; recording as dispatch failure", "bead", beadID, "reason", reason)
	d.recordDispatchFailure(beadID, anvil, reason, true)
}

// fetchBeadStatus does a one-shot bd show lookup for the bead's status field.
// Returns the status string ("open", "in_progress", "closed", etc.) or "" if
// the lookup fails. Used by reconcileMergedBeads to skip already-closed beads
// before invoking `bd close`, which would otherwise generate spurious Dolt
// commits on every daemon restart.
func (d *Daemon) fetchBeadStatus(anvilPath, beadID string) string {
	if d.beadShower == nil {
		return ""
	}
	output, stderrStr, err := d.beadShower(anvilPath, beadID)
	if err != nil {
		d.logger.Debug("fetchBeadStatus: bd show failed", "bead", beadID, "error", err, "stderr", stderrStr)
		return ""
	}

	type beadShowResponse struct {
		Status string `json:"status"`
	}

	var resp beadShowResponse
	if err := executil.DecodeJSON(output, &resp); err != nil {
		var resps []beadShowResponse
		if arrayErr := executil.DecodeJSON(output, &resps); arrayErr != nil {
			d.logger.Debug("fetchBeadStatus: failed to parse status from bd show", "bead", beadID, "error", err)
			return ""
		}
		if len(resps) > 0 {
			resp = resps[0]
		}
	}
	return resp.Status
}

// fetchExternalRef does a one-shot bd show lookup for the bead's external_ref.
// Returns the external_ref value, or "" if the lookup fails or the field is empty.
func (d *Daemon) fetchExternalRef(anvilPath, beadID string) string {
	if d.beadShower == nil {
		return ""
	}
	output, stderrStr, err := d.beadShower(anvilPath, beadID)
	if err != nil {
		d.logger.Debug("last-chance external_ref lookup failed", "bead", beadID, "error", err, "stderr", stderrStr)
		return ""
	}

	type beadShowResponse struct {
		ExternalRef string `json:"external_ref"`
	}

	// Try object form first (tolerates leading/trailing diagnostic noise).
	var resp beadShowResponse
	if err := executil.DecodeJSON(output, &resp); err != nil {
		// Fall back to array form.
		var resps []beadShowResponse
		if arrayErr := executil.DecodeJSON(output, &resps); arrayErr != nil {
			d.logger.Debug("failed to parse external_ref from bd show", "bead", beadID, "error", err)
			return ""
		}
		if len(resps) > 0 {
			resp = resps[0]
		}
	}

	if resp.ExternalRef != "" {
		d.logger.Info("last-chance lookup found external_ref", "bead", beadID, "external_ref", resp.ExternalRef)
	}
	return resp.ExternalRef
}

// maybeCloseDecomposedParent auto-closes a decomposed parent bead when it has
// no dependents (nothing has depends_on pointing to it). If the parent has
// dependents those beads stay blocked until someone closes the parent manually.
func (d *Daemon) maybeCloseDecomposedParent(bead poller.Bead, anvilCfg config.AnvilConfig, childCount int) {
	beadID := bead.ID
	anvil := bead.Anvil

	// Query the parent's dependents via bd show --json.
	output, stderrStr, err := d.beadShower(anvilCfg.Path, beadID)
	if err != nil {
		d.logger.Warn("failed to query parent bead dependents; leaving open",
			"bead", beadID, "error", err, "stderr", stderrStr)
		return
	}

	// bd show --json may return [{...}]; unwrap.
	output = bytes.TrimSpace(output)
	if len(output) > 1 && output[0] == '[' {
		start := bytes.IndexByte(output, '{')
		end := bytes.LastIndexByte(output, '}')
		if start >= 0 && end > start {
			output = output[start : end+1]
		}
	}

	var resp struct {
		Dependents []struct {
			ID             string `json:"id"`
			DependencyType string `json:"dependency_type"`
		} `json:"dependents"`
	}
	if err := json.Unmarshal(output, &resp); err != nil {
		d.logger.Warn("failed to parse parent bead dependents; leaving open",
			"bead", beadID, "error", err, "output", string(output), "stderr", stderrStr)
		return
	}

	if len(resp.Dependents) > 0 {
		d.logger.Info("keeping decomposed parent open (has dependents)",
			"bead", beadID, "dependents", len(resp.Dependents))
		// Tag the parent so that when it is re-dispatched after its dependents
		// complete, schematic recognises it and returns ActionAlreadyDecomposed
		// instead of spawning smith or re-decomposing.
		if d.labelAdder != nil {
			if err := d.labelAdder(anvilCfg.Path, beadID, schematic.LabelDecomposed); err != nil {
				d.logger.Warn("failed to tag decomposed parent; it may be re-decomposed on next dispatch",
					"bead", beadID, "error", err)
			} else {
				d.logger.Info("tagged decomposed parent to prevent re-dispatch",
					"bead", beadID, "label", schematic.LabelDecomposed)
			}
		}
		return
	}

	// No dependents — safe to auto-close.
	reason := fmt.Sprintf("Decomposed into %d children", childCount)
	if err := d.parentCloser(anvilCfg.Path, beadID, reason); err != nil {
		d.logger.Warn("failed to auto-close decomposed parent",
			"bead", beadID, "error", err)
		return
	}

	d.logger.Info("auto-closed decomposed parent (no dependents)",
		"bead", beadID, "children", childCount)
	_ = d.db.LogEvent(state.EventBeadAutoClosed,
		fmt.Sprintf("Parent auto-closed after decomposition into %d children (no dependents)", childCount),
		beadID, anvil)
}

// dedupeCacheItems collapses duplicate (BeadID, Anvil) pairs into a single
// row, preferring the entry that appears later in the slice. The cache writer
// concatenates ready beads (from the merged snapshot) with in-progress beads
// (from a fresh PollInProgress) and a bead can briefly appear in both during
// bd's claim/release transitions. Without this dedupe, the second INSERT
// inside ReplaceQueueCacheForAnvils violates the (bead_id, anvil) UNIQUE
// constraint, the surrounding transaction rolls back, and queue_cache stays
// frozen on stale rows until the race resolves on its own.
//
// Callers append in-progress entries after ready entries, so "last write
// wins" naturally preserves the in-progress section when both exist for the
// same bead — that's the more current state.
func dedupeCacheItems(items []state.QueueItem) []state.QueueItem {
	if len(items) == 0 {
		return items
	}
	type key struct {
		beadID, anvil string
	}
	seen := make(map[key]int, len(items))
	for i, it := range items {
		seen[key{it.BeadID, it.Anvil}] = i
	}
	if len(seen) == len(items) {
		return items
	}
	out := make([]state.QueueItem, 0, len(seen))
	for i, it := range items {
		if seen[key{it.BeadID, it.Anvil}] == i {
			out = append(out, it)
		}
	}
	return out
}

// updateBeadSnapshot refreshes the daemon's two-tier bead snapshot from a poll
// result. The fast (label-filtered) path may only refresh the labeled map for
// anvils that effectively used the label filter; the unlabeled map stays put
// so beads picked up by a previous slow poll remain visible to Hearth.
//
// fastPoll mirrors pollAndDispatch's effectiveFullPoll inversion: when
// fastPoll is true, the cycle requested label filtering. Per-anvil filtering
// is enabled only when the anvil also has a non-empty AutoDispatchTag — anvils
// without a tag are polled unfiltered even on the fast path, so we treat them
// as a slow refresh and update both maps.
//
// Anvils whose poll returned an error are left untouched so a transient bd
// failure does not wipe the cached snapshot.
func (d *Daemon) updateBeadSnapshot(cfg *config.Config, beads []poller.Bead, results []poller.AnvilResult, fastPoll bool) {
	if cfg == nil {
		return
	}

	beadsByAnvil := make(map[string][]poller.Bead, len(results))
	for _, b := range beads {
		beadsByAnvil[b.Anvil] = append(beadsByAnvil[b.Anvil], b)
	}

	d.snapshotMu.Lock()
	defer d.snapshotMu.Unlock()
	if d.labeledSnapshot == nil {
		d.labeledSnapshot = map[string]map[string]poller.Bead{}
	}
	if d.unlabeledSnapshot == nil {
		d.unlabeledSnapshot = map[string]map[string]poller.Bead{}
	}

	for _, r := range results {
		if r.Err != nil {
			continue
		}
		anvilCfg, ok := cfg.Anvils[r.Name]
		if !ok {
			continue
		}
		tag := anvilCfg.AutoDispatchTag
		// Was this anvil's poll filtered? Only when the global cycle ran in
		// fast mode AND this anvil had a tag for the poller to filter on.
		filtered := fastPoll && tag != ""

		labeled := make(map[string]poller.Bead, len(beadsByAnvil[r.Name]))
		unlabeled := make(map[string]poller.Bead)
		for _, b := range beadsByAnvil[r.Name] {
			if tag == "" || beadHasLabel(b, tag) {
				labeled[b.ID] = b
			} else {
				unlabeled[b.ID] = b
			}
		}
		d.labeledSnapshot[r.Name] = labeled
		if !filtered {
			// Slow (or effectively slow) refresh: replace the unlabeled map
			// wholesale so beads that have transitioned out of ready (closed,
			// claimed, lost a dependency) are evicted.
			d.unlabeledSnapshot[r.Name] = unlabeled
		}
	}
}

// beadHasLabel reports whether the bead carries the given label (case-insensitive).
func beadHasLabel(b poller.Bead, label string) bool {
	for _, l := range b.Labels {
		if strings.EqualFold(l, label) {
			return true
		}
	}
	return false
}

// mergedBeadSnapshot returns the union of labeled and unlabeled snapshots
// across all currently configured anvils, sorted by priority (ascending)
// then bead ID. Labeled entries take precedence on collision because they
// reflect the freshest poll.
func (d *Daemon) mergedBeadSnapshot() []poller.Bead {
	cfg := d.cfg.Load()
	names := make([]string, 0, len(cfg.Anvils))
	for name := range cfg.Anvils {
		names = append(names, name)
	}
	sort.Strings(names)

	d.snapshotMu.RLock()
	defer d.snapshotMu.RUnlock()
	return d.mergedBeadSnapshotLocked(names)
}

// mergedBeadSnapshotForAnvils returns the union of labeled and unlabeled
// snapshots restricted to the given anvil names.
func (d *Daemon) mergedBeadSnapshotForAnvils(anvils []string) []poller.Bead {
	d.snapshotMu.RLock()
	defer d.snapshotMu.RUnlock()
	return d.mergedBeadSnapshotLocked(anvils)
}

// mergedBeadSnapshotLocked is the lock-free merge implementation. The caller
// must hold snapshotMu (read or write).
func (d *Daemon) mergedBeadSnapshotLocked(anvils []string) []poller.Bead {
	var out []poller.Bead
	for _, a := range anvils {
		seen := make(map[string]struct{})
		if labeled, ok := d.labeledSnapshot[a]; ok {
			for id, b := range labeled {
				seen[id] = struct{}{}
				out = append(out, b)
			}
		}
		if unlabeled, ok := d.unlabeledSnapshot[a]; ok {
			for id, b := range unlabeled {
				if _, dup := seen[id]; dup {
					continue
				}
				out = append(out, b)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Anvil < out[j].Anvil
	})
	return out
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

// resolveTemperConfig returns a *temper.Config built from per-anvil custom
// commands (AnvilConfig.Temper), or nil when no custom commands are set so that
// callers fall back to auto-detection.
func (d *Daemon) resolveTemperConfig(anvilCfg config.AnvilConfig) *temper.Config {
	if anvilCfg.Temper == nil || anvilCfg.Temper.IsEmpty() {
		return nil
	}
	var cfg *temper.Config
	if len(anvilCfg.Temper.Steps) > 0 {
		if anvilCfg.Temper.Build != "" || anvilCfg.Temper.Test != "" || anvilCfg.Temper.Lint != "" {
			d.logger.Warn("temper.steps overrides temper.build/test/lint", "path", anvilCfg.Path)
		}
		cfg = temper.ConfigFromSteps(anvilCfg.Temper.Steps)
	} else {
		cfg = temper.ConfigFromCommands(anvilCfg.Temper.Build, anvilCfg.Temper.Test, anvilCfg.Temper.Lint, anvilCfg.Temper.LintRequired)
	}
	if cfg != nil {
		if dc := d.cfg.Load(); dc != nil {
			s := dc.Settings
			cfg.StepTimeout = s.TemperStepTimeout
			cfg.GitTimeout = s.TemperGitTimeout
			cfg.OutputCap = s.TemperOutputCap
		}
	}
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

// resolveEmptyDiffAction resolves settings.empty_diff_action for a dispatch.
// An unrecognised value is not fatal — it falls back to "attention" (the
// conservative choice: surface it rather than auto-close a bead) and warns so
// the typo is visible in the daemon log.
func (d *Daemon) resolveEmptyDiffAction(cfg *config.Config) string {
	action, ok := config.ResolveEmptyDiffAction(cfg.Settings.EmptyDiffAction)
	if !ok {
		d.logger.Warn("unrecognised settings.empty_diff_action — falling back",
			"configured", cfg.Settings.EmptyDiffAction, "using", action)
	}
	return action
}

// filterCopilotIfLimited removes copilot providers from the list when the
// daily copilot premium request limit has been reached. If the limit is 0
// (unlimited) or not yet reached, the list is returned unchanged.
//
// Fail policy: on a DB error reading the current usage this gate fails CLOSED —
// it assumes the limit is reached and filters copilot out — mirroring the
// daily-cost gate (costGateAllows), which also fails closed on a DB error. A
// gate that failed open here would silently ignore the configured limit
// whenever the DB hiccuped (Forge-d5ns).
//
// Empty-list guard: if filtering would remove every provider (a copilot-only
// list over the limit), the original list is returned unchanged rather than
// handing the caller zero providers — which would silently fall back to the
// built-in defaults. An explicit warning is logged and a copilot_limit_hit event
// is recorded so the situation is visible instead of surprising. The soft daily
// cap is deliberately overshot in this edge case because stalling all work is
// worse.
func (d *Daemon) filterCopilotIfLimited(providers []provider.Provider) []provider.Provider {
	limit := d.cfg.Load().Settings.CopilotDailyRequestLimit
	if limit <= 0 {
		return providers
	}
	used, err := d.db.GetTodayCopilotRequests()
	if err != nil {
		// Fail closed: treat the limit as reached so a DB error cannot silently
		// disable the configured cap. Fall through to the filter below.
		d.logger.Warn("checking copilot premium requests failed; failing closed and filtering copilot for safety", "error", err)
	} else if used < float64(limit) {
		return providers
	}
	// Filter out copilot providers.
	filtered := make([]provider.Provider, 0, len(providers))
	for _, pv := range providers {
		if pv.Kind != provider.Copilot {
			filtered = append(filtered, pv)
		}
	}
	if len(filtered) == len(providers) {
		// Nothing was copilot — no change.
		return providers
	}
	if len(filtered) == 0 {
		// Copilot was the only provider. Do NOT hand back zero providers.
		msg := fmt.Sprintf("copilot daily request limit reached (%.1f/%d) but copilot is the only configured provider; proceeding with copilot despite the limit", used, limit)
		d.logger.Error("copilot daily request limit reached but copilot is the only configured provider; proceeding with copilot despite the limit",
			"used", fmt.Sprintf("%.1f", used), "limit", limit)
		if err := d.db.LogEvent(state.EventCopilotLimitHit, msg, "", ""); err != nil {
			d.logger.Error("failed to log copilot_limit_hit event", "error", err)
		}
		return providers
	}
	d.logger.Info("copilot daily request limit reached, skipping copilot provider",
		"used", fmt.Sprintf("%.1f", used), "limit", limit)
	return filtered
}

// firstAuthFailureToday reports whether this is the first observed
// authentication failure for the given provider today. It records the
// (date, provider) pair so subsequent calls the same day return false, letting
// the caller emit the loud "check your API key" alert once per provider per day
// instead of on every affected bead (Forge-d5ns).
func (d *Daemon) firstAuthFailureToday(providerLabel string) bool {
	key := time.Now().Format("2006-01-02") + "|" + providerLabel
	d.authEscalationMu.Lock()
	defer d.authEscalationMu.Unlock()
	if d.authEscalated == nil {
		d.authEscalated = make(map[string]bool)
	}
	if d.authEscalated[key] {
		return false
	}
	d.authEscalated[key] = true
	return true
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
			if !ok || oldAnvil.Path != newAnvil.Path || oldAnvil.Platform != newAnvil.Platform {
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
			// Also detect questgiver_enabled toggle
			oldQG := oldAnvil.QuestgiverEnabled
			newQG := newAnvil.QuestgiverEnabled
			if (oldQG == nil) != (newQG == nil) || (oldQG != nil && newQG != nil && *oldQG != *newQG) {
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

	// Prune poll snapshots for anvils that were removed or renamed so the IPC
	// status response never reports stale per-anvil data after a hot-reload.
	d.lastPollMu.Lock()
	for name := range d.lastPollMap {
		if _, ok := new.Anvils[name]; !ok {
			delete(d.lastPollMap, name)
		}
	}
	d.lastPollMu.Unlock()

	// Same for the wedged-anvil WARN rate limiter. The persisted anvil_health
	// rows are pruned by the poll-time check itself.
	d.wedgedWarned.Range(func(key, _ any) bool {
		name, ok := key.(string)
		if !ok {
			return true
		}
		if _, still := new.Anvils[name]; !still {
			d.wedgedWarned.Delete(name)
		}
		return true
	})

	// Rebuild per-anvil VCS providers
	newProviders := buildVCSProviders(new, d.db, d.logger)
	d.vcsProvidersMu.Lock()
	d.vcsProviders = newProviders
	d.vcsProvidersMu.Unlock()
	d.logger.Info("rebuilt per-anvil VCS providers", "count", len(newProviders))

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

	// Update questgiver monitor (respect global questgiver_enabled)
	if d.questgiverMonitor != nil {
		if !new.Settings.IsQuestgiverEnabled() {
			// Questgiver globally disabled: clear anvil paths so the monitor stops polling.
			d.questgiverMonitor.UpdateAnvilPaths(map[string]string{})
			d.logger.Info("disabled questgiver monitor via config; cleared anvil paths")
		} else {
			qgPaths := filterQuestgiverAnvils(paths, new.Anvils)
			d.questgiverMonitor.UpdateAnvilPaths(qgPaths)
			d.logger.Info("updated questgiver anvil paths", "count", len(qgPaths))
		}
		// Preview quest runs are gated on preview_quests rather than on
		// questgiver_enabled: they are asked for on a specific branch, not
		// polled for, so they stay available even when scheduled scanning is
		// off for the anvil.
		pqPaths := previewQuestAnvils(new)
		d.questgiverMonitor.SetPreviewQuestAnvils(pqPaths)
		d.logger.Info("updated preview quest anvils", "count", len(pqPaths))
	}
}

// updateWicketConfig propagates a new configuration to the Wicket monitor so
// that triage settings (such as labels and provider) take effect without a
// daemon restart where supported. Called from the hot-reload callback on every
// config reload. It also handles runtime enable/disable: if the monitor was not
// running and is now enabled it is started; if running and now disabled the
// config update prevents future scans without a restart.
// Note: WicketInterval changes require a monitor restart to take effect because
// the poll ticker is created once when Run starts and is not dynamically reset.
func (d *Daemon) updateWicketConfig(cfg *config.Config) {
	d.wicketMu.Lock()
	defer d.wicketMu.Unlock()

	if d.wicketMonitor != nil {
		// Monitor already running — propagate new config so the next scan cycle
		// picks up the changes (including a possible disable via WicketEnabled=false).
		d.wicketMonitor.UpdateConfig(cfg)
		d.logger.Info("updated wicket configuration")
		return
	}
	// Monitor was never started (daemon launched with wicket_enabled: false).
	// Start it now if the config has been switched on at runtime.
	if cfg.Settings.WicketEnabled {
		m := wicket.New(cfg, d.db)
		d.wicketMonitor = m
		go func() {
			if err := m.Run(d.runCtx); err != nil && err != context.Canceled {
				d.logger.Error("Wicket monitor error", "error", err)
			}
		}()
		d.logger.Info("started wicket monitor via hot-reload")
	}
}

// updateSmelterSettings is called from the hot-reload callback on every config
// reload. It updates the smelter's anvil paths and interval independently of
// whether the anvil set changed, ensuring that smelter_enabled and
// smelter_interval changes take effect without a daemon restart.
func (d *Daemon) updateSmelterSettings(old, new *config.Config) {
	if d.smelterWorker == nil {
		return
	}

	// Build current anvil paths for smelter.
	paths := make(map[string]string, len(new.Anvils))
	for name, a := range new.Anvils {
		if a.Path != "" {
			paths[name] = a.Path
		}
	}

	if !new.Settings.IsSmelterEnabled() {
		// Smelter globally disabled: clear anvil paths and pause its ticker.
		d.smelterWorker.UpdateAnvilPaths(map[string]string{})
		d.smelterWorker.UpdateInterval(0)
		d.logger.Info("disabled smelter via config; cleared anvil paths and paused ticker")
	} else {
		d.smelterWorker.UpdateAnvilPaths(paths)
		d.logger.Info("updated smelter anvil paths", "count", len(paths))
		// Restore/update the interval when it changes, or when transitioning
		// from disabled to enabled (to re-enable a previously paused ticker).
		if old.Settings.SmelterInterval != new.Settings.SmelterInterval ||
			!old.Settings.IsSmelterEnabled() {
			d.smelterWorker.UpdateInterval(new.Settings.SmelterInterval)
			d.logger.Info("updated smelter interval",
				"old", old.Settings.SmelterInterval,
				"new", new.Settings.SmelterInterval)
		}
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

// replaceQueueTimestamps refreshes the in-memory timestamp map for the given
// set of successfully-polled anvils. Entries belonging to other anvils are
// kept verbatim, mirroring ReplaceQueueCacheForAnvils so a failed anvil poll
// does not wipe its last-known timestamps. `fresh` is the full set of new
// timestamps from this poll cycle (its keys must use the "anvil/beadID"
// format).
func (d *Daemon) replaceQueueTimestamps(succeeded map[string]struct{}, fresh map[string]queueTimestamp) {
	d.queueTimestampsMu.Lock()
	defer d.queueTimestampsMu.Unlock()
	next := make(map[string]queueTimestamp, len(fresh)+len(d.queueTimestamps))
	// Keep entries from anvils that did not poll successfully this cycle so
	// their timestamps remain available alongside their cached queue rows.
	for k, v := range d.queueTimestamps {
		anvil, _, ok := strings.Cut(k, "/")
		if !ok {
			continue
		}
		if _, replaced := succeeded[anvil]; replaced {
			continue
		}
		next[k] = v
	}
	for k, v := range fresh {
		next[k] = v
	}
	d.queueTimestamps = next
}

// lookupQueueTimestamp returns the CreatedAt/UpdatedAt pair recorded for the
// given (anvil, beadID) during the most recent poll. Callers receive a
// zero-valued queueTimestamp (empty strings) when no entry is present.
func (d *Daemon) lookupQueueTimestamp(anvil, beadID string) queueTimestamp {
	d.queueTimestampsMu.RLock()
	defer d.queueTimestampsMu.RUnlock()
	return d.queueTimestamps[anvil+"/"+beadID]
}

// parseQueueLabels decodes a JSON-encoded labels string from the queue_cache
// table. Returns an empty slice (not nil) on any decode error so JSON
// callers see [] instead of null.
func parseQueueLabels(raw string) []string {
	out := []string{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	if out == nil {
		out = []string{}
	}
	return out
}

// adventurerExecutorAdapter wraps an adventurer.Executor to implement the
// questgiver.QuestExecutor interface, bridging the two packages without an
// import cycle (adventurer imports questgiver, not the other way around).
type adventurerExecutorAdapter struct {
	exec *adventurer.Executor
}

func (a *adventurerExecutorAdapter) Execute(ctx context.Context, quest *questgiver.Quest) *questgiver.QuestResult {
	r := a.exec.Execute(ctx, quest)
	return &questgiver.QuestResult{
		Passed:       r.Passed,
		FailedStep:   r.FailedStep,
		ErrorMessage: r.ErrorMessage,
		Duration:     r.Duration,
		Screenshots:  r.Screenshots,
	}
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

// filterQuestgiverAnvils returns the subset of anvils that should be monitored
// by the QuestGiver. Anvils with QuestgiverEnabled explicitly set to false are
// excluded. When the per-anvil setting is nil (unset), the anvil is included
// (the global IsQuestgiverEnabled gate has already been checked by the caller).
func filterQuestgiverAnvils(anvils map[string]string, anvilCfgs map[string]config.AnvilConfig) map[string]string {
	result := make(map[string]string, len(anvils))
	for name, path := range anvils {
		if ac, ok := anvilCfgs[name]; ok && ac.QuestgiverEnabled != nil && !*ac.QuestgiverEnabled {
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

		// Invalidate any stale worktree directory that matches the recovered
		// branch. If the previous run left a .workers/<bead-id>/ directory
		// without a valid .git file, it must be removed so the next dispatch
		// doesn't reuse it and accidentally edit the main checkout.
		if strings.HasPrefix(originalBranch, "forge/") {
			beadDir := strings.TrimPrefix(originalBranch, "forge/")
			stalePath := filepath.Join(anvilPath, ".workers", beadDir)
			if err := worktree.ValidateWorktreeDir(stalePath); err != nil {
				if _, statErr := os.Stat(stalePath); statErr == nil {
					logger.Warn("removing stale worktree directory after branch recovery",
						"path", stalePath, "validation_error", err)
					if rmErr := worktree.RemoveWithRetry(ctx, stalePath); rmErr != nil {
						logger.Error("failed to remove stale worktree directory",
							"path", stalePath, "error", rmErr)
					}
				}
			}
		}
	}

	return nil
}

// handleWardenRerun re-runs warden on the existing worktree branch.
// If warden approves, creates a PR. Otherwise returns the bead to needs attention.
func (d *Daemon) handleWardenRerun(beadID, anvil, branch string, anvilCfg config.AnvilConfig) {
	ctx, cancel := context.WithTimeout(d.runCtx, 30*time.Minute)
	defer cancel()

	wt, err := d.worktreeMgr.Create(ctx, anvilCfg.Path, beadID, branch)
	if err != nil {
		d.logger.Error("warden_rerun: failed to create worktree", "bead", beadID, "error", err)
		return
	}
	defer d.worktreeMgr.Remove(context.Background(), anvilCfg.Path, wt)

	workerID := fmt.Sprintf("%s-%s-%d", anvil, beadID, time.Now().UnixNano())
	_ = d.db.InsertWorker(&state.Worker{
		ID:        workerID,
		BeadID:    beadID,
		Anvil:     anvil,
		Branch:    branch,
		Status:    state.WorkerRunning,
		Phase:     "warden_rerun",
		Title:     d.db.BeadTitle(beadID, anvil),
		StartedAt: time.Now(),
	})

	providers := d.filterCopilotIfLimited(provider.FromConfig(config.ProvidersForStageWithAnvil(d.cfg.Load().Settings, &anvilCfg, "warden")))

	title := d.db.BeadTitle(beadID, anvil)
	var description string
	var baseBranch string
	var rerunExternalRef string
	if bead, err := crucible.FetchBead(ctx, beadID, anvilCfg.Path); err == nil {
		description = bead.SpecForPrompt()
		rerunExternalRef = bead.ExternalRef
		// Resolve epic branch so PRs target the correct base for crucible children.
		beads := []poller.Bead{bead}
		poller.ResolveEpicBranches(ctx, beads, map[string]string{anvil: anvilCfg.Path})
		baseBranch = beads[0].EpicBranch
	}

	result, err := warden.Review(ctx, wt.Path, beadID, title, description, anvilCfg.Path, d.db, "", workerID, providers...)
	if err != nil {
		d.logger.Error("warden_rerun: review failed", "bead", beadID, "error", err)
		_ = d.db.UpdateWorkerStatus(workerID, state.WorkerFailed)
		return
	}

	if result.Verdict == warden.VerdictApprove {
		d.logger.Info("warden_rerun: approved", "bead", beadID)
		_ = d.db.LogEvent(state.EventWardenPass, result.Summary, beadID, anvil)

		// Ensure branch is pushed before creating PR.
		pushCmd := executil.HideWindow(exec.CommandContext(ctx, "git", "push", "-u", "origin", "--", branch))
		pushCmd.Dir = wt.Path
		if pushErr := pushCmd.Run(); pushErr != nil {
			d.logger.Warn("warden_rerun: push failed (may already be up-to-date)", "error", pushErr)
		}

		// Prefer a changelog fragment (if the rerun worktree has one) for the
		// '## Changes' section and keep the warden verdict in ReviewerNotes so
		// review-speak never appears under '## Changes'.
		var changelogSummary, reviewerNotes string
		if wt != nil {
			changelogSummary = pipeline.ExtractChangelogSummary(wt.Path, beadID)
		}
		if changelogSummary == "" && result.Summary != "" {
			reviewerNotes = result.Summary
		}

		pr, err := d.vcsForAnvil(anvil).CreatePR(ctx, vcs.CreateParams{
			WorktreePath:    anvilCfg.Path,
			BeadID:          beadID,
			Title:           fmt.Sprintf("%s (%s)", title, beadID),
			Branch:          branch,
			Base:            baseBranch,
			AnvilName:       anvil,
			BeadTitle:       title,
			BeadDescription: description,
			ChangeSummary:   changelogSummary,
			ReviewerNotes:   reviewerNotes,
			ExternalRef:     rerunExternalRef,
		})
		if err != nil {
			d.logger.Error("warden_rerun: PR creation failed", "bead", beadID, "error", err)
			_ = d.db.UpdateWorkerStatus(workerID, state.WorkerFailed)
			_ = d.db.MarkNeedsHuman(beadID, anvil, fmt.Sprintf("warden_rerun: PR creation failed: %v", err))
			d.ingotMarkFailed(beadID, anvil)
			return
		}
		// Clear needs_human only after PR is successfully created.
		_ = d.db.ResetRetry(beadID, anvil)
		d.logger.Info("warden_rerun: PR created", "bead", beadID, "pr", pr.URL)
		_ = d.db.UpdateWorkerStatus(workerID, state.WorkerDone)
		_ = d.db.LogEvent(state.EventPRCreated, fmt.Sprintf("PR #%d created: %s", pr.Number, pr.URL), beadID, anvil)
		d.ingotRecordPR(beadID, anvil, pr.Number, pr.URL)
		d.notifyWicketPRCreated(beadID, pr.URL, pr.Number)
	} else {
		d.logger.Info("warden_rerun: not approved", "bead", beadID, "verdict", result.Verdict, "summary", result.Summary)
		_ = d.db.LogEvent(state.EventWardenReject, fmt.Sprintf("Warden re-review: %s — %s", result.Verdict, result.Summary), beadID, anvil)
		_ = d.db.UpdateWorkerStatus(workerID, state.WorkerFailed)
		// Bead stays in needs attention — record the updated feedback.
		d.recordDispatchFailure(beadID, anvil, fmt.Sprintf("warden re-review: %s", result.Summary), true)
	}
}

// handleApproveAsIs bypasses warden and creates a PR from the current branch state.
func (d *Daemon) handleApproveAsIs(beadID, anvil, branch string, anvilCfg config.AnvilConfig) {
	ctx, cancel := context.WithTimeout(d.runCtx, 5*time.Minute)
	defer cancel()

	workerID := fmt.Sprintf("%s-%s-%d", anvil, beadID, time.Now().UnixNano())
	_ = d.db.InsertWorker(&state.Worker{
		ID:        workerID,
		BeadID:    beadID,
		Anvil:     anvil,
		Branch:    branch,
		Status:    state.WorkerRunning,
		Phase:     "approve_as_is",
		Title:     d.db.BeadTitle(beadID, anvil),
		StartedAt: time.Now(),
	})

	// Ensure branch is pushed.
	wt, err := d.worktreeMgr.Create(ctx, anvilCfg.Path, beadID, branch)
	if err != nil {
		d.logger.Error("approve_as_is: failed to create worktree", "bead", beadID, "error", err)
		_ = d.db.UpdateWorkerStatus(workerID, state.WorkerFailed)
		return
	}
	defer d.worktreeMgr.Remove(context.Background(), anvilCfg.Path, wt)

	pushCmd := executil.HideWindow(exec.CommandContext(ctx, "git", "push", "-u", "origin", "--", branch))
	pushCmd.Dir = wt.Path
	if pushErr := pushCmd.Run(); pushErr != nil {
		d.logger.Warn("approve_as_is: push failed (may already be up-to-date)", "error", pushErr)
	}

	title := d.db.BeadTitle(beadID, anvil)
	var description string
	var baseBranch string
	var approveExtRef string
	if bead, err := crucible.FetchBead(ctx, beadID, anvilCfg.Path); err == nil {
		description = bead.Description
		approveExtRef = bead.ExternalRef
		// Resolve epic branch so PRs target the correct base for crucible children.
		beads := []poller.Bead{bead}
		poller.ResolveEpicBranches(ctx, beads, map[string]string{anvil: anvilCfg.Path})
		baseBranch = beads[0].EpicBranch
	}

	// Prefer a changelog fragment for '## Changes' when the worktree has one;
	// otherwise leave the section empty and record the manual-bypass note as
	// a reviewer note instead of polluting the changelog section.
	var approveChangelogSummary, approveReviewerNotes string
	if approveChangelogSummary = pipeline.ExtractChangelogSummary(wt.Path, beadID); approveChangelogSummary == "" {
		approveReviewerNotes = "Approved as-is (manual bypass)"
	}

	pr, err := d.vcsForAnvil(anvil).CreatePR(ctx, vcs.CreateParams{
		WorktreePath:    anvilCfg.Path,
		BeadID:          beadID,
		Title:           fmt.Sprintf("%s (%s)", title, beadID),
		Branch:          branch,
		Base:            baseBranch,
		AnvilName:       anvil,
		BeadTitle:       title,
		BeadDescription: description,
		ChangeSummary:   approveChangelogSummary,
		ReviewerNotes:   approveReviewerNotes,
		ExternalRef:     approveExtRef,
	})
	if err != nil {
		d.logger.Error("approve_as_is: PR creation failed", "bead", beadID, "error", err)
		_ = d.db.UpdateWorkerStatus(workerID, state.WorkerFailed)
		_ = d.db.MarkNeedsHuman(beadID, anvil, fmt.Sprintf("approve_as_is: PR creation failed: %v", err))
		d.ingotMarkFailed(beadID, anvil)
		return
	}

	// Clear needs_human only after PR is successfully created.
	_ = d.db.ResetRetry(beadID, anvil)
	d.logger.Info("approve_as_is: PR created", "bead", beadID, "pr", pr.URL)
	_ = d.db.UpdateWorkerStatus(workerID, state.WorkerDone)
	_ = d.db.LogEvent(state.EventPRCreated, fmt.Sprintf("PR #%d created (approved as-is): %s", pr.Number, pr.URL), beadID, anvil)
	d.ingotRecordPR(beadID, anvil, pr.Number, pr.URL)
	d.notifyWicketPRCreated(beadID, pr.URL, pr.Number)
}

// handleForceSmith re-invokes smith on the same branch with existing warden
// feedback attached. If userNote is non-empty, it is prepended to the prompt.
func (d *Daemon) handleForceSmith(beadID, anvil, branch, userNote string, anvilCfg config.AnvilConfig) {
	ctx, cancel := context.WithTimeout(d.runCtx, 30*time.Minute)
	defer cancel()

	// Clear needs_human immediately so the bead leaves the Needs Attention panel
	// as soon as force smith begins, not only after it completes.
	_ = d.db.ResetRetry(beadID, anvil)

	wt, err := d.worktreeMgr.Create(ctx, anvilCfg.Path, beadID, branch)
	if err != nil {
		d.logger.Error("force_smith: failed to create worktree", "bead", beadID, "error", err)
		return
	}
	wtRemoved := false
	defer func() {
		if !wtRemoved {
			d.worktreeMgr.Remove(context.Background(), anvilCfg.Path, wt)
		}
	}()

	workerID := fmt.Sprintf("%s-%s-%d", anvil, beadID, time.Now().UnixNano())
	_ = d.db.InsertWorker(&state.Worker{
		ID:        workerID,
		BeadID:    beadID,
		Anvil:     anvil,
		Branch:    branch,
		Status:    state.WorkerRunning,
		Phase:     "force_smith",
		Title:     d.db.BeadTitle(beadID, anvil),
		StartedAt: time.Now(),
	})

	// Build the smith prompt with warden feedback context.
	title := d.db.BeadTitle(beadID, anvil)
	var description, notes string
	if bead, err := crucible.FetchBead(ctx, beadID, anvilCfg.Path); err == nil {
		description = bead.SpecForPrompt()
		notes = bead.Notes
	}

	// Build prior feedback from the retry reason (which contains warden feedback).
	var priorFeedback string
	iteration := 2 // minimum iteration for a forced re-smith
	if retry, err := d.db.GetRetry(beadID, anvil); err == nil && retry != nil {
		if retry.LastError != "" {
			priorFeedback = retry.LastError
		}
		if retry.RetryCount+1 > iteration {
			iteration = retry.RetryCount + 1
		}
	}

	feedbackContext := priorFeedback
	if userNote != "" {
		feedbackContext = fmt.Sprintf("Human note: %s\n\n%s", userNote, feedbackContext)
	}

	promptText, err := d.promptBuilder.Build(prompt.BeadContext{
		BeadID:              beadID,
		Title:               title,
		Description:         description,
		Notes:               notes,
		AnvilName:           anvil,
		AnvilPath:           anvilCfg.Path,
		WorktreePath:        wt.Path,
		Branch:              branch,
		Iteration:           iteration,
		PriorFeedback:       feedbackContext,
		PriorFeedbackSource: "Warden review (force retry)",
	})
	if err != nil {
		d.logger.Error("force_smith: prompt build failed", "bead", beadID, "error", err)
		_ = d.db.UpdateWorkerStatus(workerID, state.WorkerFailed)
		return
	}

	logDir := wt.Path + "/.forge-logs"
	providers := d.filterCopilotIfLimited(provider.FromConfig(config.ProvidersForStageWithAnvil(d.cfg.Load().Settings, &anvilCfg, "smith")))

	var lastExitCode int
	for _, pv := range providers {
		process, err := smith.SpawnWithProvider(ctx, wt.Path, promptText, logDir, pv, d.cfg.Load().Settings.ClaudeFlags)
		if err != nil {
			d.logger.Warn("force_smith: spawn failed, trying next provider", "provider", pv.Label(), "error", err)
			continue
		}
		// Record the spawned process details in the worker row.
		_ = d.db.UpdateWorkerPID(workerID, process.PID)
		_ = d.db.UpdateWorkerLogPath(workerID, process.LogPath)

		smithResult := process.Wait()
		if smithResult != nil {
			_ = d.db.UpdateWorkerSession(workerID, smithResult.SessionID, smith.SessionModel(smithResult, pv))
		}
		if smithResult != nil && smithResult.ExitCode == 0 {
			d.logger.Info("force_smith: smith completed", "bead", beadID, "provider", pv.Label())
			// Push changes before removing the force_smith worktree.
			pushCmd := executil.HideWindow(exec.CommandContext(ctx, "git", "push", "-u", "origin", "--", branch))
			pushCmd.Dir = wt.Path
			_ = pushCmd.Run()

			_ = d.db.UpdateWorkerStatus(workerID, state.WorkerDone)
			_ = d.db.LogEvent(state.EventSmithDone, "Force smith completed", beadID, anvil)
			_ = d.db.ResetRetry(beadID, anvil)

			// Remove the force_smith worktree before pipeline.Run creates
			// its own — git does not allow two worktrees on the same branch.
			d.worktreeMgr.Remove(context.Background(), anvilCfg.Path, wt)
			wtRemoved = true // prevent defer from double-removing

			// Continue with temper → warden → PR via the normal pipeline,
			// skipping the smith phase (already completed above). The bead
			// stays in_progress throughout — no reset to open needed.
			d.runPostForceSmithPipeline(ctx, beadID, anvil, anvilCfg)
			return
		}
		if smithResult != nil {
			lastExitCode = smithResult.ExitCode
		}
	}

	d.logger.Error("force_smith: all providers failed", "bead", beadID, "exit_code", lastExitCode)
	_ = d.db.UpdateWorkerStatus(workerID, state.WorkerFailed)
	_ = d.db.LogEvent(state.EventSmithFailed, fmt.Sprintf("Force smith failed: exit code %d", lastExitCode), beadID, anvil)
}

// runPostForceSmithPipeline runs the temper → warden → PR phases via
// pipeline.Run with SkipSmith=true after force smith has completed.
func (d *Daemon) runPostForceSmithPipeline(ctx context.Context, beadID, anvil string, anvilCfg config.AnvilConfig) {
	// Fetch full bead metadata so the pipeline has title/description.
	bead, err := crucible.FetchBead(ctx, beadID, anvilCfg.Path)
	if err != nil {
		d.logger.Error("force_smith: failed to fetch bead for pipeline", "bead", beadID, "error", err)
		return
	}
	// FetchBead parses JSON; Anvil is json:"-" so we must set it manually.
	bead.Anvil = anvil

	// Resolve epic branch so that Crucible children target the correct base
	// branch for PR creation. FetchBead does not populate EpicBranch (it is
	// json:"-" and normally filled by poller.ResolveEpicBranches), so we
	// resolve it explicitly here — mirroring the warden_rerun/approve_as_is flows.
	beads := []poller.Bead{bead}
	poller.ResolveEpicBranches(ctx, beads, map[string]string{anvil: anvilCfg.Path})
	bead.EpicBranch = beads[0].EpicBranch

	smithProviderSpecs := config.ProvidersForStageWithAnvil(d.cfg.Load().Settings, &anvilCfg, "smith")

	// Derive from context.Background() (not d.runCtx) so that a graceful
	// shutdown does not cancel Temper/Warden/PR creation mid-flight — matching
	// the same pattern used for normal dispatch pipelines.
	pipelineCtx, cancel := context.WithTimeout(context.Background(), d.cfg.Load().Settings.SmithTimeout)
	defer cancel()

	postPipelineParams := pipeline.Params{
		DB:                d.db,
		WorktreeManager:   d.worktreeMgr,
		PromptBuilder:     d.promptBuilder,
		AnvilName:         anvil,
		AnvilConfig:       anvilCfg,
		Bead:              bead,
		BaseBranch:        bead.EpicBranch, // empty for non-Crucible beads; set for children
		ExtraFlags:        d.cfg.Load().Settings.ClaudeFlags,
		TemperConfig:      d.resolveTemperConfig(anvilCfg),
		GoRaceDetection:   d.resolveGoRaceDetection(anvilCfg),
		TemperStepTimeout: d.cfg.Load().Settings.TemperStepTimeout,
		TemperGitTimeout:  d.cfg.Load().Settings.TemperGitTimeout,
		TemperOutputCap:   d.cfg.Load().Settings.TemperOutputCap,
		Providers:         d.filterCopilotIfLimited(provider.FromConfig(smithProviderSpecs)),
		Notifier:          d.notifier.Load(),
		MaxIterations:     d.cfg.Load().Settings.MaxPipelineIterations,
		SkipSmith:         true,

		WardenModelOverride:         d.cfg.Load().Settings.WardenModelOverride,
		SchematicModelOverride:      d.cfg.Load().Settings.SchematicModelOverride,
		CopilotSkipWardenSmallDiffs: d.cfg.Load().Settings.CopilotSkipWardenSmallDiffs,
		WardenFullRereview:          d.cfg.Load().Settings.WardenFullRereview,
		CopilotCombinedSmithWarden:  d.cfg.Load().Settings.CopilotCombinedSmithWarden,
		CopilotWardenSampleRate:     d.cfg.Load().Settings.CopilotWardenSampleRate,
		EmptyDiffAction:             d.resolveEmptyDiffAction(d.cfg.Load()),
	}
	// Only set WardenProviders/SchematicProviders when explicitly configured in
	// stage_providers; otherwise leave empty so the legacy model-override path runs.
	if wardenSpecs := config.ExplicitStageProvidersWithAnvil(d.cfg.Load().Settings, &anvilCfg, "warden"); len(wardenSpecs) > 0 {
		postPipelineParams.WardenProviders = d.filterCopilotIfLimited(provider.FromConfig(wardenSpecs))
	}
	if schematicSpecs := config.ExplicitStageProvidersWithAnvil(d.cfg.Load().Settings, &anvilCfg, "schematic"); len(schematicSpecs) > 0 {
		postPipelineParams.SchematicProviders = d.filterCopilotIfLimited(provider.FromConfig(schematicSpecs))
	}
	outcome := pipeline.Run(pipelineCtx, postPipelineParams)

	if outcome.Error != nil {
		reason := fmt.Sprintf("Force smith post-pipeline failed: %v", outcome.Error)
		d.logger.Error("force_smith: post-smith pipeline failed", "bead", beadID, "error", outcome.Error)
		_ = d.db.MarkNeedsHuman(beadID, anvil, reason)
		return
	}

	if !outcome.Success {
		if outcome.EmptyDiff {
			// Nothing to open a PR for — the work is already on the base branch.
			// Resolve it the same way a normal dispatch would.
			emptyCtx, emptyCancel := context.WithTimeout(context.Background(), executil.BdTimeout())
			defer emptyCancel()
			d.applyEmptyDiffOutcome(emptyCtx, bead, anvilCfg.Path, outcome)
			return
		}
		reason := "Force smith: warden rejected, needs human attention"
		if outcome.ReviewResult != nil && outcome.ReviewResult.Summary != "" {
			reason = "Force smith warden: " + outcome.ReviewResult.Summary
		}
		d.logger.Warn("force_smith: post-smith pipeline did not succeed", "bead", beadID, "verdict", outcome.Verdict)
		_ = d.db.MarkNeedsHuman(beadID, anvil, reason)
		return
	}

	// Pipeline succeeded — use shared finalize path (PR + notify + close).
	d.finalizePipeline(pipelineCtx, outcome, bead, anvilCfg.Path, outcome.WorkerID)
}

// applyWardenFilterConfig pushes settings.warden into the warden package so
// future warden reviews use the configured filter cap and toggles. Called at
// daemon startup and again whenever the config hot-reloads.
func applyWardenFilterConfig(cfg *config.Config) {
	if cfg == nil {
		warden.SetActiveFilterConfig(warden.DefaultReviewFilterConfig())
		return
	}
	w := cfg.Settings.Warden
	warden.SetActiveFilterConfig(warden.ReviewFilterConfig{
		MaxRules:          w.ResolvedMaxRulesPerReview(),
		UseAllRules:       w.UseAllRules,
		FilterPathGlob:    w.IsFilterPathGlobEnabled(),
		FilterCategory:    w.IsFilterCategoryEnabled(),
		FilterPatternGrep: w.IsFilterPatternGrepEnabled(),
	})
}

// applyWorktreeTimeoutConfig pushes settings.worktree_git_timeout into the
// worktree package so checkout-heavy git commands (worktree add, fetch, push,
// reset, clean) run under the configured deadline instead of the built-in
// default. Cheap metadata commands keep their own tight bound. Called at daemon
// startup and again whenever the config hot-reloads.
func applyWorktreeTimeoutConfig(cfg *config.Config) {
	if cfg == nil {
		worktree.SetGitCheckoutTimeout(0)
		return
	}
	worktree.SetGitCheckoutTimeout(cfg.Settings.WorktreeGitTimeout)
}

// applyBdTimeoutConfig pushes settings.bd_timeout into the executil package so
// every bd invocation (ready, show, create, update, close, sql) runs under the
// configured deadline instead of the built-in default. Called at daemon startup
// and again whenever the config hot-reloads.
func applyBdTimeoutConfig(cfg *config.Config) {
	if cfg == nil {
		executil.SetBdTimeout(0)
		return
	}
	executil.SetBdTimeout(cfg.Settings.BdTimeout)
}

// applyPricingConfig pushes settings.pricing and
// settings.copilot_premium_multipliers into the cost package so fallback cost
// estimates (Copilot/Gemini/OpenAI) and Copilot premium-request weighting use
// the configured rates. Both are overlaid on top of the built-in defaults, so
// an operator can override a single model without restating the rest. Called
// at daemon startup and again whenever the config hot-reloads.
func applyPricingConfig(cfg *config.Config) {
	if cfg == nil {
		cost.SetPricingTable(nil)
		cost.SetCopilotPremiumMultipliers(nil)
		return
	}
	var overrides map[string]cost.Pricing
	if len(cfg.Settings.Pricing) > 0 {
		overrides = make(map[string]cost.Pricing, len(cfg.Settings.Pricing))
		for model, p := range cfg.Settings.Pricing {
			overrides[model] = cost.Pricing{
				InputPerM:      p.InputPerM,
				OutputPerM:     p.OutputPerM,
				CacheReadPerM:  p.CacheReadPerM,
				CacheWritePerM: p.CacheWritePerM,
			}
		}
	}
	cost.SetPricingTable(overrides)
	cost.SetCopilotPremiumMultipliers(cfg.Settings.CopilotPremiumMultipliers)
}
