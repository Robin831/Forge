package selfdeploy

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Restart invocation modes, reported in the intent log line and returned by
// restartArgv so callers can tell which path was taken.
const (
	// ModeSystemdRun is the preferred path: the restart runs inside its own
	// transient systemd scope, outside the daemon's unit cgroup.
	ModeSystemdRun = "systemd-run-scope"
	// ModeNoBlock is the fallback when systemd-run is unavailable: systemctl
	// enqueues the restart job with PID 1 and returns immediately, so the job
	// survives even if the client is killed.
	ModeNoBlock = "systemctl-no-block"
)

// systemdRunBin is the binary used to open a transient scope for the restart.
const systemdRunBin = "systemd-run"

// scopeUnitPrefix names the transient scope created for a self-deploy restart.
// The suffix is made unique per deploy so back-to-back deploys never collide on
// a unit name that systemd has not yet garbage-collected.
const scopeUnitPrefix = "forge-selfdeploy-"

// maxSHAInUnitName bounds how much of the build SHA is embedded in the scope
// unit name — enough to identify the deploy, short enough to stay readable in
// `systemctl list-units`.
const maxSHAInUnitName = 12

// privilegeWrappers are command names that only elevate the command following
// them rather than being the restart command themselves. When the configured
// restart command is one of these, systemd-run has to be inserted *after* it
// (`sudo systemd-run … systemctl restart forge`), because creating a system
// scope requires the elevated privileges the wrapper provides.
var privilegeWrappers = map[string]bool{
	"sudo":   true,
	"doas":   true,
	"pkexec": true,
}

// RestartRequest carries everything the restarter needs to invoke a restart and
// to log a diagnosable intent line before doing so. When a restart is killed
// mid-flight the intent line in daemon.log is the only surviving evidence of
// which binary was live and what could be rolled back, so every field here is
// chosen to answer "what state was the host left in?".
type RestartRequest struct {
	// Unit is the systemd unit to restart (e.g. "forge").
	Unit string
	// BuildSHA is the commit the new binary was built from. Empty when the SHA
	// could not be resolved; it is diagnostic only and never gates the restart.
	BuildSHA string
	// BinaryPath is the live binary that was just swapped in.
	BinaryPath string
	// RollbackPath is where the outgoing binary was preserved, or empty when
	// there was no previous binary to preserve.
	RollbackPath string
}

// SystemctlRestarter restarts a unit via systemctl, deliberately detaching the
// restart child from the daemon's lifetime and cgroup.
//
// Why the detach is mandatory: the daemon runs as forge.service, so any child it
// spawns is placed in that unit's cgroup. `systemctl restart forge` stops the
// unit, and stopping a unit SIGKILLs everything left in its cgroup — including
// the very `systemctl` client that asked for the restart. The observed failure
// was `sudo [systemctl restart forge]: signal: killed`: the restart client died
// before the job completed, Deploy read that as a failed restart, rolled the
// binary back, and the host was left running the stale binary. A context
// deadline (or the daemon's own root context being cancelled during shutdown)
// produced the same kill from the other direction.
//
// Three independent measures remove that failure mode:
//
//   - No context. Restart takes no context.Context at all, so no caller
//     deadline or cancellation can reach the child. The absence of the
//     parameter is the enforcement: it cannot be threaded in by accident.
//   - Setsid. detachProcess starts the child in its own session and process
//     group, so a process-group signal aimed at the daemon does not reach it.
//   - Its own cgroup. `systemd-run --scope` moves the child into a transient
//     scope unit, outside forge.service's cgroup, so stopping forge.service
//     leaves it running. When systemd-run is unavailable the fallback is
//     `systemctl restart --no-block`, which hands the job to PID 1 and returns
//     immediately, so the job completes even if the client is killed.
//
// Because the child is released rather than waited on, a nil return means
// "restart requested", not "restart completed" — see the Restarter interface.
type SystemctlRestarter struct {
	// Cmd is the command name (default "systemctl") and PrependArgs are inserted
	// before "restart" (e.g. nil for a root daemon, ["--user"] for a user unit,
	// or ["systemctl"] when Cmd is "sudo").
	Cmd         string
	PrependArgs []string
	// Logger receives the pre-exec intent line. Defaults to slog.Default().
	Logger *slog.Logger

	// lookPath resolves an executable in PATH; nil uses exec.LookPath. Injected
	// by tests to exercise both the systemd-run and --no-block branches.
	lookPath func(string) (string, error)
	// start runs the assembled command detached; nil uses startDetached.
	start func(*exec.Cmd) error
	// pid supplies the suffix that makes the transient scope unit name unique;
	// nil uses os.Getpid.
	pid func() int
}

// Restart logs the restart intent and then starts a detached restart of the
// unit. It returns as soon as the child has been spawned; the child is released,
// never waited on, and outlives this process by design.
func (s SystemctlRestarter) Restart(req RestartRequest) error {
	cmd, mode := s.buildRestartCmd(req)

	// Log BEFORE spawning. Everything after this point can be killed by the
	// restart itself, so this line is the last thing guaranteed to reach
	// daemon.log — it has to carry enough state (which build, which binary,
	// what to roll back to) to reconstruct the host afterwards. slog writes
	// synchronously to the daemon's log sink, so the record is durable by the
	// time Info returns.
	s.logger().Info("self-deploy: restarting unit (detached)",
		"unit", req.Unit,
		"build_sha", req.BuildSHA,
		"binary", req.BinaryPath,
		"rollback", req.RollbackPath,
		"mode", mode,
		"argv", strings.Join(cmd.Args, " "))

	if err := s.starter()(cmd); err != nil {
		return fmt.Errorf("starting detached restart %v: %w", cmd.Args, err)
	}
	return nil
}

// buildRestartCmd assembles the detached restart command and reports which mode
// it used. It is separated from Restart so the argv and the detach flags can be
// asserted in unit tests without spawning systemctl.
func (s SystemctlRestarter) buildRestartCmd(req RestartRequest) (*exec.Cmd, string) {
	argv, mode := s.restartArgv(req)
	// exec.Command, never exec.CommandContext: binding the child to any context
	// re-introduces the deadline/cancellation kill this type exists to avoid.
	cmd := exec.Command(argv[0], argv[1:]...)
	// Detach from the daemon's session/process group.
	detachProcess(cmd)
	// The child outlives us and is never waited on, so it has nowhere to write:
	// leave stdio nil (/dev/null) rather than inheriting handles that close when
	// the daemon dies.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	return cmd, mode
}

// restartArgv builds the full argv for the restart, preferring a transient
// systemd scope and falling back to a non-blocking systemctl call.
func (s SystemctlRestarter) restartArgv(req RestartRequest) ([]string, string) {
	bin := s.Cmd
	if bin == "" {
		bin = "systemctl"
	}
	prefix := append([]string{bin}, s.PrependArgs...)

	if _, err := s.lookPathFn()(systemdRunBin); err != nil {
		// systemd-run unavailable: hand the job to PID 1 and return immediately.
		return append(append([]string{}, prefix...), "restart", "--no-block", req.Unit), ModeNoBlock
	}

	// A privilege wrapper has to stay in front — creating a *system* scope needs
	// the privileges it grants — so systemd-run is inserted just after it.
	head, rest := []string{}, prefix
	if privilegeWrappers[filepath.Base(bin)] {
		head, rest = prefix[:1], prefix[1:]
	}

	run := []string{systemdRunBin}
	if s.userMode() {
		// A `systemctl --user` unit lives in the user manager, so the scope must
		// be created there too.
		run = append(run, "--user")
	}
	run = append(run, "--scope", "--collect", "--unit="+s.scopeUnit(req))

	argv := append([]string{}, head...)
	argv = append(argv, run...)
	argv = append(argv, rest...)
	argv = append(argv, "restart", req.Unit)
	return argv, ModeSystemdRun
}

// userMode reports whether the restart targets the per-user systemd manager.
func (s SystemctlRestarter) userMode() bool {
	for _, a := range s.PrependArgs {
		if a == "--user" {
			return true
		}
	}
	return false
}

// scopeUnit names the transient scope for this deploy. The build SHA identifies
// which deploy it belongs to and the PID keeps repeat deploys from colliding on
// a name systemd has not collected yet.
func (s SystemctlRestarter) scopeUnit(req RestartRequest) string {
	sha := sanitizeUnitToken(req.BuildSHA)
	if len(sha) > maxSHAInUnitName {
		sha = sha[:maxSHAInUnitName]
	}
	if sha == "" {
		return fmt.Sprintf("%s%d", scopeUnitPrefix, s.pidFn()())
	}
	return fmt.Sprintf("%s%s-%d", scopeUnitPrefix, sha, s.pidFn()())
}

// sanitizeUnitToken strips anything that is not valid in a systemd unit name.
func sanitizeUnitToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s SystemctlRestarter) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s SystemctlRestarter) lookPathFn() func(string) (string, error) {
	if s.lookPath != nil {
		return s.lookPath
	}
	return exec.LookPath
}

func (s SystemctlRestarter) starter() func(*exec.Cmd) error {
	if s.start != nil {
		return s.start
	}
	return startDetached
}

func (s SystemctlRestarter) pidFn() func() int {
	if s.pid != nil {
		return s.pid
	}
	return os.Getpid
}

// startDetached spawns cmd and immediately releases it. Release (rather than
// Wait) is deliberate: the child is expected to outlive this process — indeed to
// terminate it — so waiting would either block until the daemon is killed or
// report a spurious "signal: killed" failure that triggers a rollback of a
// perfectly good binary. Start still surfaces immediate spawn failures
// (executable missing, permission denied), which is the error class a caller can
// actually act on.
func startDetached(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
