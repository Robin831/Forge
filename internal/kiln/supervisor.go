package kiln

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/forge"
	"github.com/Robin831/Forge/internal/hooks"
)

const (
	// DefaultStopTimeout is how long a service gets to exit after a polite
	// termination signal before its whole process group is killed.
	DefaultStopTimeout = 10 * time.Second
	// forceKillGrace bounds how long Stop waits for the process group to
	// disappear after the forced kill, so teardown cannot hang forever on an
	// unkillable (e.g. uninterruptible-sleep) process.
	forceKillGrace = 5 * time.Second
)

// ServiceSpec is everything needed to spawn one preview service. The service
// must already have been expanded (see Manifest.Expand): the supervisor runs
// the command line verbatim.
type ServiceSpec struct {
	// Service is the expanded manifest service.
	Service Service
	// BeadID is the bead the preview belongs to; it selects the log directory.
	BeadID string
	// WorktreePath is the preview checkout the service's Dir is relative to.
	WorktreePath string
	// Env is the full environment for the process (see BuildEnv).
	Env []string
	// Logger receives supervision diagnostics. Optional.
	Logger *slog.Logger
}

// ServiceProcess is one supervised preview service.
//
// The process is started in its own process group (executil.SetProcessGroup) so
// the entire tree can be killed: a manifest command is a shell line that
// routinely forks (`npm run dev` → node → esbuild), and killing only the leader
// would leave those children holding the allocated port and the worktree files.
type ServiceProcess struct {
	// Name is the manifest service name.
	Name string
	// LogPath is the file stdout and stderr are appended to.
	LogPath string
	// StartedAt is when the process was spawned.
	StartedAt time.Time

	cmd     *exec.Cmd
	logFile *os.File
	logger  *slog.Logger

	// done is closed once cmd.Wait has returned.
	done chan struct{}

	mu      sync.Mutex
	waitErr error

	stopOnce sync.Once
	stopErr  error
}

// StartService spawns one preview service and returns a handle to it. The
// returned process is running but not yet known to be healthy — see
// HealthCheck.
//
// ctx bounds the process's lifetime, not the start call: cancelling it kills
// the whole process group. Callers therefore pass a preview-scoped context, not
// a request-scoped one.
func StartService(ctx context.Context, spec ServiceSpec) (*ServiceProcess, error) {
	logger := spec.Logger
	if logger == nil {
		logger = slog.Default()
	}

	dir := spec.WorktreePath
	if spec.Service.Dir != "" {
		dir = filepath.Join(spec.WorktreePath, spec.Service.Dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("service %q: working directory %s: %w", spec.Service.Name, dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("service %q: working directory %s is not a directory", spec.Service.Name, dir)
	}

	logPath, err := ServiceLogPath(spec.BeadID, spec.Service.Name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, fmt.Errorf("service %q: creating log directory: %w", spec.Service.Name, err)
	}
	// Append rather than truncate: restarting a service inside the same bead's
	// preview must not erase the log of the run that made it necessary.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("service %q: opening log %s: %w", spec.Service.Name, logPath, err)
	}

	startedAt := time.Now()
	fmt.Fprintf(logFile, "\n=== kiln preview %s: service %q started %s ===\n%s\n",
		spec.BeadID, spec.Service.Name, startedAt.Format(time.RFC3339), spec.Service.Command)

	shell, flag := hooks.ShellArgs()
	cmd := executil.HideWindow(exec.CommandContext(ctx, shell, flag, spec.Service.Command))
	executil.SetProcessGroup(cmd)
	cmd.Dir = dir
	cmd.Env = spec.Env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// exec's default cancel kills only the leader; a preview service's children
	// are the ones holding the port, so cancel the whole group instead.
	cmd.Cancel = func() error { return executil.KillProcessTree(cmd) }

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("service %q: starting %q in %s: %w", spec.Service.Name, spec.Service.Command, dir, err)
	}
	// Best-effort containment so a daemon crash cannot leave previews running
	// (Windows job object; a no-op on Unix).
	if err := executil.ContainProcess(cmd); err != nil {
		logger.Warn("kiln: failed to contain preview service process",
			"bead", spec.BeadID, "service", spec.Service.Name, "error", err)
	}

	p := &ServiceProcess{
		Name:      spec.Service.Name,
		LogPath:   logPath,
		StartedAt: startedAt,
		cmd:       cmd,
		logFile:   logFile,
		logger:    logger,
		done:      make(chan struct{}),
	}
	go p.reap()
	return p, nil
}

// reap waits for the process to exit, records the outcome and releases the log
// file. Descendants that outlive the leader keep their inherited handle, so
// closing here cannot cut their output short.
func (p *ServiceProcess) reap() {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.waitErr = err
	p.mu.Unlock()
	if err != nil {
		fmt.Fprintf(p.logFile, "\n=== kiln: service %q exited: %v ===\n", p.Name, err)
	} else {
		fmt.Fprintf(p.logFile, "\n=== kiln: service %q exited cleanly ===\n", p.Name)
	}
	p.logFile.Close()
	close(p.done)
}

// PID returns the process group leader's PID, or 0 when the process never
// started.
func (p *ServiceProcess) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Done is closed once the process has exited.
func (p *ServiceProcess) Done() <-chan struct{} { return p.done }

// Exited reports whether the process has already exited.
func (p *ServiceProcess) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// ExitErr returns the process's exit error, or nil while it is still running or
// if it exited cleanly.
func (p *ServiceProcess) ExitErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

// ExitCode returns the exit status of a process that has exited.
//
// The bool is false while it is still running, and also when it was killed by a
// signal: a signalled process has no exit status, and reporting one anyway
// (exec's -1) would read as an exit code the program chose. The cause in that
// case is in ExitErr, which names the signal.
func (p *ServiceProcess) ExitCode() (int, bool) {
	if p == nil || !p.Exited() {
		return 0, false
	}
	err := p.ExitErr()
	if err == nil {
		return 0, true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code, true
		}
	}
	return 0, false
}

// Stop terminates the service and everything it spawned.
//
// It first asks the leader to exit (SIGTERM; on Windows the signal is not
// deliverable and the step is skipped), waits up to timeout, then kills the
// whole process group. The final group kill runs even when the leader has
// already exited, because that is exactly the case where a backgrounded child
// is left holding the port.
//
// Stop is idempotent: repeated calls return the first call's result without
// re-signalling.
func (p *ServiceProcess) Stop(timeout time.Duration) error {
	if p == nil {
		return nil
	}
	p.stopOnce.Do(func() { p.stopErr = p.stop(timeout) })
	return p.stopErr
}

func (p *ServiceProcess) stop(timeout time.Duration) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = DefaultStopTimeout
	}

	if !p.Exited() {
		// Politely first: a dev server that traps SIGTERM flushes and closes
		// its listening socket, which is what makes the port immediately
		// reusable by the next preview.
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			p.logger.Debug("kiln: graceful stop signal not delivered",
				"service", p.Name, "pid", p.PID(), "error", err)
		}
		select {
		case <-p.done:
		case <-time.After(timeout):
			p.logger.Warn("kiln: preview service did not exit in time, killing process group",
				"service", p.Name, "pid", p.PID(), "timeout", timeout)
		}
	}

	// Always reap the group: stragglers survive their leader.
	if err := executil.KillProcessTree(p.cmd); err != nil {
		p.logger.Warn("kiln: killing preview service process group failed",
			"service", p.Name, "pid", p.PID(), "error", err)
	}
	select {
	case <-p.done:
		return nil
	case <-time.After(forceKillGrace):
		return fmt.Errorf("service %q (pid %d) still running after kill", p.Name, p.PID())
	}
}

// LogDir returns the per-bead log directory previews write into:
// ~/.forge/logs/<beadID>/. It is the same directory the pipeline preserves
// worker logs in, so preview logs show up in the Hearth bead-log browser and
// are covered by the logsweep retention policy for free.
func LogDir(beadID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("kiln: resolving home directory: %w", err)
	}
	return filepath.Join(home, ".forge", "logs", forge.SanitizeBeadID(beadID)), nil
}

// ServiceLogPath returns the log file for one service of a bead's preview:
// ~/.forge/logs/<beadID>/preview-<service>.log. Service names are constrained
// by manifest validation to characters that are safe in a file name.
func ServiceLogPath(beadID, service string) (string, error) {
	dir, err := LogDir(beadID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "preview-"+service+".log"), nil
}
