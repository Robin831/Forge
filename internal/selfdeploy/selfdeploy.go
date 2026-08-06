// Package selfdeploy rebuilds and restarts the Forge daemon from source after a
// merge lands on Forge's own repository.
//
// The production daemon binary (~/bin/forge) is normally built by hand, so
// merged daemon-side fixes routinely fail to run until someone remembers to
// rebuild — version skew that has caused several "phantom" regressions where a
// fix was merged but the running binary was weeks behind main. When enabled,
// the Deployer closes that gap automatically: on a qualifying merge the daemon
// drains its workers, then this package pulls the repo, rebuilds the binary,
// verifies it, atomically swaps it into place (keeping the previous binary for
// rollback), and restarts the systemd unit.
//
// The flow is deliberately conservative:
//   - it refuses to run while any worker is active: the caller pauses dispatch,
//     then Deploy waits out Config.MaxDrainWait, re-checking on a ticker until
//     the forge is idle (the last check is the one guarding the binary swap) or
//     the budget is spent, in which case the deploy is deferred, not failed;
//   - the freshly built binary must pass `forge version` and `forge --help`
//     (exit 0) before it is allowed to replace the live one;
//   - the previous binary is preserved at <binary>.prev so a bad restart can be
//     rolled back manually or automatically;
//   - the restart itself is spawned detached, outside the daemon's unit cgroup
//     and free of any caller context, so stopping forge.service cannot kill the
//     process performing the restart (see SystemctlRestarter).
//
// All external interactions (git/go/systemctl) sit behind small interfaces so
// the drain/verify/swap/rollback logic is unit-testable without touching the
// real host.
package selfdeploy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Event names emitted over an EventSink during a deploy. They mirror the
// state.EventType constants of the same string value so the daemon's event log
// and TUI can categorise them.
const (
	EventStarted  = "self_deploy_started"
	EventSuccess  = "self_deploy_success"
	EventRollback = "self_deploy_rollback"
	EventFailed   = "self_deploy_failed"
	EventSkipped  = "self_deploy_skipped"
)

// Commander runs an external command in a working directory and returns its
// combined output. dir may be empty to inherit the current process directory.
type Commander interface {
	Run(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

// Restarter restarts a systemd unit. The production implementation may cause
// the calling process to be terminated (self-restart); callers must treat a nil
// return as "restart requested" rather than "restart completed".
//
// Restart takes no context.Context on purpose. The restart child must outlive
// both the caller and the daemon's own lifetime: it is the thing that stops
// forge.service, so binding it to the deploy context means a cancellation or
// deadline SIGKILLs the restart mid-flight, which Deploy would then read as a
// failed restart and roll a perfectly good binary back. Keeping the parameter
// out of the signature makes that mistake unrepresentable rather than merely
// discouraged — see SystemctlRestarter for the full rationale.
type Restarter interface {
	Restart(req RestartRequest) error
}

// EventSink records deploy lifecycle events. Implementations should not block.
type EventSink interface {
	Emit(event, message string)
}

// Config holds the resolved settings for a single deploy. Zero-value optional
// fields are filled in by New.
type Config struct {
	// RepoPath is the source checkout to `git pull` and build from.
	RepoPath string
	// BinaryPath is the live binary that gets replaced (e.g. ~/bin/forge).
	BinaryPath string
	// PrevPath is where the outgoing binary is preserved for rollback.
	// Defaults to BinaryPath + ".prev".
	PrevPath string
	// UnitName is the systemd unit restarted after the swap. Defaults to "forge".
	UnitName string
	// Branch is the branch pulled before building. Defaults to "main".
	Branch string
	// BuildTarget is the `go build` package target. Defaults to "./cmd/forge".
	BuildTarget string
	// CurrentSHA identifies the build of the binary that is live when the deploy
	// starts — the one preserved at PrevPath and put back by a rollback. It is
	// reported as DeployEvent.RestoredSHA so a rollback says what the host is
	// running now, not just what it failed to run. Optional: an empty value only
	// costs the needs-attention item that detail.
	CurrentSHA string
	// MaxDrainWait bounds how long Deploy waits for active workers to drain
	// before deferring the deploy. Defaults to DefaultMaxDrainWait.
	MaxDrainWait time.Duration
	// DrainInterval is how often the drain check is retried while waiting.
	// Defaults to DefaultDrainInterval, and is clamped to MaxDrainWait.
	DrainInterval time.Duration
}

// ActiveWorkersFunc reports the workers that would be disrupted by a restart,
// identified for a human (bead or worker id). An empty result means the forge
// has fully drained. A nil ActiveWorkersFunc disables the drain wait entirely.
type ActiveWorkersFunc func() ([]string, error)

// Deployer performs the drain-verify-swap-restart flow.
type Deployer struct {
	cfg     Config
	cmd     Commander
	restart Restarter
	events  EventSink
	// attention raises the operator-facing needs-attention item for a failed or
	// rolled-back deploy. nil disables the escalation (the event log still gets
	// the rollback/failed event).
	attention Emitter
	// logger records emission failures. Never nil after New.
	logger *slog.Logger
	// activeWorkers reports the workers currently active. While it returns a
	// non-empty set the deploy waits; nil disables the check (tests).
	activeWorkers ActiveWorkersFunc
	// now and newTicker indirect the drain wait's timing so tests can drive polls
	// with a fake clock instead of sleeping. Default to time.Now and realTicker.
	now       func() time.Time
	newTicker func(time.Duration) (<-chan time.Time, func())
	// remove and rename indirect the filesystem swap so tests can inject
	// failures. Default to os.Remove / os.Rename.
	remove func(string) error
	rename func(string, string) error
	stat   func(string) (os.FileInfo, error)
}

// Option customises a Deployer beyond the collaborators every deploy needs.
type Option func(*Deployer)

// WithEmitter attaches the needs-attention emitter. Without it a failed deploy
// is only visible in the event log, which is what made silent rollbacks so easy
// to miss.
func WithEmitter(e Emitter) Option {
	return func(d *Deployer) { d.attention = e }
}

// WithLogger sets the logger used for the escalation path's own failures. It
// defaults to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(d *Deployer) {
		if l != nil {
			d.logger = l
		}
	}
}

// New constructs a Deployer, applying defaults to optional Config fields.
func New(cfg Config, cmd Commander, restart Restarter, events EventSink, activeWorkers ActiveWorkersFunc, opts ...Option) *Deployer {
	if cfg.PrevPath == "" {
		cfg.PrevPath = cfg.BinaryPath + ".prev"
	}
	if cfg.UnitName == "" {
		cfg.UnitName = "forge"
	}
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	if cfg.BuildTarget == "" {
		cfg.BuildTarget = "./cmd/forge"
	}
	if cfg.MaxDrainWait <= 0 {
		cfg.MaxDrainWait = DefaultMaxDrainWait
	}
	if cfg.DrainInterval <= 0 {
		cfg.DrainInterval = DefaultDrainInterval
	}
	d := &Deployer{
		cfg:           cfg,
		cmd:           cmd,
		restart:       restart,
		events:        events,
		logger:        slog.Default(),
		activeWorkers: activeWorkers,
		now:           time.Now,
		newTicker:     realTicker,
		remove:        os.Remove,
		rename:        os.Rename,
		stat:          os.Stat,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Deploy runs the full flow. It returns nil once a restart has been requested
// (which, for a self-restart, means the process will imminently be terminated),
// an error unwrapping to ErrDrainTimeout when workers never drained, a wrapped
// ctx error when the wait is cancelled, or a wrapped error at the first failing
// step. On any failure before the binary is swapped, the live binary is left
// untouched; on a swap or restart failure the previous binary is restored.
//
// Deploy can block for up to Config.MaxDrainWait before doing any work — the
// caller is expected to run it off the hot path (the daemon gives it its own
// goroutine) with dispatch already paused.
func (d *Deployer) Deploy(ctx context.Context) error {
	d.emit(EventStarted, fmt.Sprintf("self-deploy starting (repo=%s binary=%s unit=%s branch=%s)",
		d.cfg.RepoPath, d.cfg.BinaryPath, d.cfg.UnitName, d.cfg.Branch))

	// Drain guard: refuse to touch the binary while workers run. The caller has
	// paused dispatch, so the active set can only shrink — wait it out rather
	// than giving up on the first busy sample, and let the final (successful)
	// check double as the atomic guard immediately before the swap.
	if err := d.waitForDrain(ctx, d.cfg.MaxDrainWait); err != nil {
		return err
	}
	// The forge drained, so an earlier deferral is history: resolve it now
	// rather than leaving a stale "workers did not drain" item in the list for
	// as long as it takes the next deploy to fail some other way.
	d.resolveAttention(ReasonDrainTimeout)

	// 1. Pull the latest source (fast-forward only — a diverged tree is an
	//    operator problem the manual restart.sh path is better suited to).
	if out, err := d.cmd.Run(ctx, d.cfg.RepoPath, "git", "pull", "--ff-only", "origin", d.cfg.Branch); err != nil {
		return d.fail("git pull failed: %v: %s", err, trim(out))
	}

	// 1b. Resolve the commit being deployed. Best-effort: it only feeds the
	//     restart intent log and the transient scope unit name, so a failure here
	//     must not abort a deploy.
	buildSHA := d.headSHA(ctx)

	// 2. Build to a temp path on the same filesystem as the target so the final
	//    swap is an atomic rename.
	tmpPath := d.cfg.BinaryPath + ".new"
	if out, err := d.cmd.Run(ctx, d.cfg.RepoPath, "go", "build", "-o", tmpPath, d.cfg.BuildTarget); err != nil {
		_ = d.remove(tmpPath)
		return d.fail("go build failed: %v: %s", err, trim(out))
	}

	// 3. Verify the freshly built binary before it is allowed to go live.
	if err := d.verify(ctx, tmpPath); err != nil {
		_ = d.remove(tmpPath)
		return d.fail("new binary verification failed: %v", err)
	}

	// 4. Preserve the outgoing binary for rollback (if one exists).
	hadPrev := false
	if _, err := d.stat(d.cfg.BinaryPath); err == nil {
		if err := d.rename(d.cfg.BinaryPath, d.cfg.PrevPath); err != nil {
			_ = d.remove(tmpPath)
			return d.fail("preserving previous binary failed: %v", err)
		}
		hadPrev = true
	}

	// 5. Atomic swap: temp -> live binary.
	if err := d.rename(tmpPath, d.cfg.BinaryPath); err != nil {
		restored := false
		if hadPrev {
			restored = d.rename(d.cfg.PrevPath, d.cfg.BinaryPath) == nil // restore
		}
		_ = d.remove(tmpPath)
		// The swap is the first step that can leave the host without its binary,
		// so escalate here too: a restore that puts the old build back is exactly
		// the silent rollback this escalation exists to surface.
		d.raiseAttention(DeployEvent{
			Reason:       ReasonSwapFailed,
			AttemptedSHA: buildSHA,
			RolledBack:   restored,
			Detail:       fmt.Sprintf("swapping in new binary failed: %v", err),
		})
		return d.fail("swapping in new binary failed: %v", err)
	}

	rollbackPath := ""
	if hadPrev {
		rollbackPath = d.cfg.PrevPath
	}

	d.emit(EventSuccess, fmt.Sprintf("new binary installed at %s (build %s, previous kept at %s); restarting unit %s",
		d.cfg.BinaryPath, shaOrUnknown(buildSHA), d.cfg.PrevPath, d.cfg.UnitName))

	// 6. Restart. On a self-restart this call typically does not return (the
	//    process is terminated by systemd). The deploy ctx is deliberately NOT
	//    passed: the restart must survive this process, and Restarter's signature
	//    enforces that. If it returns an error the restart never started, so roll
	//    back to the known-good previous binary.
	if err := d.restart.Restart(RestartRequest{
		Unit:         d.cfg.UnitName,
		BuildSHA:     buildSHA,
		BinaryPath:   d.cfg.BinaryPath,
		RollbackPath: rollbackPath,
	}); err != nil {
		// Exactly one needs-attention item per failed restart, whatever the
		// rollback did: the reason names the originating failure and the payload
		// says whether the previous binary is back. Emitting again from the
		// rollback branch would double-list the same incident.
		ev := DeployEvent{
			Reason:       ReasonRestartFailed,
			AttemptedSHA: buildSHA,
			Detail:       fmt.Sprintf("restart failed: %v", err),
		}
		switch {
		case !hadPrev:
			ev.Detail = fmt.Sprintf("restart failed and no previous binary to roll back to: %v", err)
			d.events.Emit(EventFailed, ev.Detail)
		default:
			if rbErr := d.rename(d.cfg.PrevPath, d.cfg.BinaryPath); rbErr == nil {
				ev.RolledBack = true
				ev.RestoredSHA = d.cfg.CurrentSHA
				d.events.Emit(EventRollback, fmt.Sprintf("restart failed (%v); rolled back to previous binary", err))
			} else {
				ev.Reason = ReasonRollbackFailed
				ev.Detail = fmt.Sprintf("restart failed (%v) and rollback failed (%v)", err, rbErr)
				d.events.Emit(EventFailed, ev.Detail)
			}
		}
		d.raiseAttention(ev)
		return fmt.Errorf("selfdeploy restart: %w", err)
	}
	// The restart is under way, so every earlier deferral, failure or rollback
	// for this forge is stale — nothing else clears them, and a resolved item
	// left in Needs Attention trains operators to ignore the list.
	d.resolveAttention()
	return nil
}

// raiseAttention records ev as a needs-attention item, filling in the fields
// that are the same for every failure. Escalation is best-effort by design: it
// runs on the rollback path, where the deploy has already gone wrong and the
// one thing that must still happen is restoring the previous binary. A failure
// to record is logged and swallowed.
func (d *Deployer) raiseAttention(ev DeployEvent) {
	if d.attention == nil {
		return
	}
	ev.Unit = d.cfg.UnitName
	ev.BinaryPath = d.cfg.BinaryPath
	if ev.Timestamp.IsZero() {
		ev.Timestamp = d.now()
	}
	if err := d.attention.EmitNeedsAttention(ev); err != nil {
		d.logger.Warn("self-deploy: could not record the needs-attention item",
			"reason", string(ev.Reason), "attempted_sha", ev.AttemptedSHA,
			"rolled_back", ev.RolledBack, "error", err)
	}
}

// resolveAttention clears outstanding needs-attention items for the given
// reasons (all of them when called with none). Like raiseAttention it never
// fails a deploy.
func (d *Deployer) resolveAttention(reasons ...FailureReason) {
	if d.attention == nil {
		return
	}
	if err := d.attention.ClearNeedsAttention(reasons...); err != nil {
		d.logger.Warn("self-deploy: could not clear needs-attention items",
			"reasons", reasons, "error", err)
	}
}

// verify runs the new binary's `version` and `--help` subcommands, requiring
// both to exit 0. This catches a build that links but is fundamentally broken
// (missing shared lib, panics on startup, corrupt binary) before it replaces
// the live one.
func (d *Deployer) verify(ctx context.Context, path string) error {
	if out, err := d.cmd.Run(ctx, "", path, "version"); err != nil {
		return fmt.Errorf("`%s version` did not exit 0: %v: %s", path, err, trim(out))
	}
	if out, err := d.cmd.Run(ctx, "", path, "--help"); err != nil {
		return fmt.Errorf("`%s --help` did not exit 0: %v: %s", path, err, trim(out))
	}
	return nil
}

// headSHA resolves the commit the build will be produced from. It is purely
// diagnostic — it identifies the deploy in the restart intent log and in the
// transient scope unit name — so any failure yields an empty string rather than
// aborting the deploy.
func (d *Deployer) headSHA(ctx context.Context) string {
	out, err := d.cmd.Run(ctx, d.cfg.RepoPath, "git", "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// shaOrUnknown renders a possibly-empty build SHA for human-facing messages.
func shaOrUnknown(sha string) string {
	if sha == "" {
		return "unknown"
	}
	return sha
}

func (d *Deployer) emit(event, message string) {
	if d.events != nil {
		d.events.Emit(event, message)
	}
}

// fail emits an EventFailed and returns the formatted error.
func (d *Deployer) fail(format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	d.emit(EventFailed, err.Error())
	return err
}

// trim bounds command output embedded in error/event messages so a noisy build
// log cannot balloon the event row.
func trim(out []byte) string {
	const max = 2000
	s := string(out)
	if len(s) > max {
		return s[:max] + "…(truncated)"
	}
	return s
}

// --- production implementations of the injectable interfaces ---

// ExecCommander runs commands via os/exec, capturing combined output.
type ExecCommander struct{}

// Run executes name with args in dir (empty dir inherits the process cwd).
func (ExecCommander) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	return cmd.CombinedOutput()
}
