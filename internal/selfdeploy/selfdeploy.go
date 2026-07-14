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
//   - it refuses to run while any worker is active (the caller drains first, and
//     Deploy re-checks atomically before touching the binary);
//   - the freshly built binary must pass `forge version` and `forge --help`
//     (exit 0) before it is allowed to replace the live one;
//   - the previous binary is preserved at <binary>.prev so a bad restart can be
//     rolled back manually or automatically.
//
// All external interactions (git/go/systemctl) sit behind small interfaces so
// the drain/verify/swap/rollback logic is unit-testable without touching the
// real host.
package selfdeploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// ErrWorkersActive is returned (and an EventSkipped emitted) when Deploy is
// called while one or more workers are still active. The caller is expected to
// have drained already; this is the final safety re-check.
var ErrWorkersActive = errors.New("selfdeploy: workers still active, deploy deferred")

// Commander runs an external command in a working directory and returns its
// combined output. dir may be empty to inherit the current process directory.
type Commander interface {
	Run(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

// Restarter restarts a systemd unit. The production implementation may cause
// the calling process to be terminated (self-restart); callers must treat a nil
// return as "restart requested" rather than "restart completed".
type Restarter interface {
	Restart(ctx context.Context, unit string) error
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
}

// Deployer performs the drain-verify-swap-restart flow.
type Deployer struct {
	cfg     Config
	cmd     Commander
	restart Restarter
	events  EventSink
	// activeWorkers reports how many workers are currently active. When it
	// returns > 0 the deploy is skipped. nil disables the check (tests).
	activeWorkers func() (int, error)
	// remove and rename indirect the filesystem swap so tests can inject
	// failures. Default to os.Remove / os.Rename.
	remove func(string) error
	rename func(string, string) error
	stat   func(string) (os.FileInfo, error)
}

// New constructs a Deployer, applying defaults to optional Config fields.
func New(cfg Config, cmd Commander, restart Restarter, events EventSink, activeWorkers func() (int, error)) *Deployer {
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
	return &Deployer{
		cfg:           cfg,
		cmd:           cmd,
		restart:       restart,
		events:        events,
		activeWorkers: activeWorkers,
		remove:        os.Remove,
		rename:        os.Rename,
		stat:          os.Stat,
	}
}

// Deploy runs the full flow. It returns nil once a restart has been requested
// (which, for a self-restart, means the process will imminently be terminated),
// ErrWorkersActive when the drain guard trips, or a wrapped error at the first
// failing step. On any failure before the binary is swapped, the live binary is
// left untouched; on a swap or restart failure the previous binary is restored.
func (d *Deployer) Deploy(ctx context.Context) error {
	d.emit(EventStarted, fmt.Sprintf("self-deploy starting (repo=%s binary=%s unit=%s branch=%s)",
		d.cfg.RepoPath, d.cfg.BinaryPath, d.cfg.UnitName, d.cfg.Branch))

	// Final drain guard: refuse to touch the binary while workers run. The
	// caller pauses dispatch and waits, but a worker could have started in the
	// gap between the wait loop and here, so re-check atomically.
	if d.activeWorkers != nil {
		n, err := d.activeWorkers()
		if err != nil {
			return d.fail("worker drain check failed: %v", err)
		}
		if n > 0 {
			d.events.Emit(EventSkipped, fmt.Sprintf("deploy deferred: %d worker(s) still active", n))
			return ErrWorkersActive
		}
	}

	// 1. Pull the latest source (fast-forward only — a diverged tree is an
	//    operator problem the manual restart.sh path is better suited to).
	if out, err := d.cmd.Run(ctx, d.cfg.RepoPath, "git", "pull", "--ff-only", "origin", d.cfg.Branch); err != nil {
		return d.fail("git pull failed: %v: %s", err, trim(out))
	}

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
		if hadPrev {
			_ = d.rename(d.cfg.PrevPath, d.cfg.BinaryPath) // restore
		}
		_ = d.remove(tmpPath)
		return d.fail("swapping in new binary failed: %v", err)
	}

	d.emit(EventSuccess, fmt.Sprintf("new binary installed at %s (previous kept at %s); restarting unit %s",
		d.cfg.BinaryPath, d.cfg.PrevPath, d.cfg.UnitName))

	// 6. Restart. On a self-restart this call typically does not return (the
	//    process is terminated by systemd). If it returns an error the restart
	//    did not take, so roll back to the known-good previous binary.
	if err := d.restart.Restart(ctx, d.cfg.UnitName); err != nil {
		if hadPrev {
			if rbErr := d.rename(d.cfg.PrevPath, d.cfg.BinaryPath); rbErr == nil {
				d.events.Emit(EventRollback, fmt.Sprintf("restart failed (%v); rolled back to previous binary", err))
			} else {
				d.events.Emit(EventFailed, fmt.Sprintf("restart failed (%v) and rollback failed (%v)", err, rbErr))
			}
		} else {
			d.events.Emit(EventFailed, fmt.Sprintf("restart failed and no previous binary to roll back to: %v", err))
		}
		return fmt.Errorf("selfdeploy restart: %w", err)
	}
	return nil
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

// SystemctlRestarter restarts a unit via `systemctl restart <unit>`.
type SystemctlRestarter struct {
	// Cmd is the command name (default "systemctl") and PrependArgs are inserted
	// before "restart" (e.g. nil for a root daemon, or ["--user"] elsewhere).
	Cmd         string
	PrependArgs []string
}

// Restart invokes systemctl restart for the unit.
func (s SystemctlRestarter) Restart(ctx context.Context, unit string) error {
	bin := s.Cmd
	if bin == "" {
		bin = "systemctl"
	}
	args := append(append([]string{}, s.PrependArgs...), "restart", unit)
	cmd := exec.CommandContext(ctx, bin, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v: %w: %s", bin, args, err, trim(out))
	}
	return nil
}
