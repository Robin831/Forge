package selfdeploy

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// recorder is a single ordered event log shared by the capturing logger and the
// fake process starter, so tests can assert not just *that* the intent line was
// emitted but that it was emitted strictly *before* the exec.
type recorder struct {
	mu      sync.Mutex
	entries []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, s)
}

// Write makes the recorder usable as an slog sink.
func (r *recorder) Write(p []byte) (int, error) {
	r.add("log " + strings.TrimSpace(string(p)))
	return len(p), nil
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.entries...)
}

// indexOf returns the position of the first entry containing sub, or -1.
func (r *recorder) indexOf(sub string) int {
	for i, e := range r.snapshot() {
		if strings.Contains(e, sub) {
			return i
		}
	}
	return -1
}

// testRestarter wires a SystemctlRestarter to a recorder, a fake PATH lookup and
// a fake starter, so no real process is ever spawned. Both the intent log and
// the exec land in rec, in order.
func testRestarter(rec *recorder, systemdRun bool, cmdName string, prepend []string) SystemctlRestarter {
	return SystemctlRestarter{
		Cmd:         cmdName,
		PrependArgs: prepend,
		Logger:      slog.New(slog.NewTextHandler(rec, &slog.HandlerOptions{Level: slog.LevelDebug})),
		lookPath: func(string) (string, error) {
			if systemdRun {
				return "/usr/bin/systemd-run", nil
			}
			return "", errors.New("executable file not found in $PATH")
		},
		start: func(c *exec.Cmd) error {
			rec.add("exec " + strings.Join(c.Args, " "))
			return nil
		},
		pid: func() int { return 4242 },
	}
}

func sampleRequest() RestartRequest {
	return RestartRequest{
		Unit:         "forge",
		BuildSHA:     "cafebabe0123456789abcdef",
		BinaryPath:   "/home/robin/bin/forge",
		RollbackPath: "/home/robin/bin/forge.prev",
	}
}

// TestRestart_ArgvPerMode covers both escape routes out of the forge.service
// cgroup: a transient systemd scope when systemd-run exists, and a non-blocking
// systemctl job handed to PID 1 when it does not.
func TestRestart_ArgvPerMode(t *testing.T) {
	tests := []struct {
		name       string
		systemdRun bool
		cmd        string
		prepend    []string
		wantArgv   string
		wantMode   string
	}{
		{
			name:       "systemd-run available, plain systemctl",
			systemdRun: true,
			wantArgv:   "systemd-run --scope --collect --unit=forge-selfdeploy-cafebabe0123-4242 systemctl restart forge",
			wantMode:   ModeSystemdRun,
		},
		{
			name:       "systemd-run available, sudo wrapper stays in front",
			systemdRun: true,
			cmd:        "sudo",
			prepend:    []string{"systemctl"},
			wantArgv:   "sudo systemd-run --scope --collect --unit=forge-selfdeploy-cafebabe0123-4242 systemctl restart forge",
			wantMode:   ModeSystemdRun,
		},
		{
			name:       "systemd-run available, user manager",
			systemdRun: true,
			cmd:        "systemctl",
			prepend:    []string{"--user"},
			wantArgv:   "systemd-run --user --scope --collect --unit=forge-selfdeploy-cafebabe0123-4242 systemctl --user restart forge",
			wantMode:   ModeSystemdRun,
		},
		{
			name:       "systemd-run missing, fall back to --no-block",
			systemdRun: false,
			wantArgv:   "systemctl restart --no-block forge",
			wantMode:   ModeNoBlock,
		},
		{
			name:       "systemd-run missing, sudo fallback",
			systemdRun: false,
			cmd:        "sudo",
			prepend:    []string{"systemctl"},
			wantArgv:   "sudo systemctl restart --no-block forge",
			wantMode:   ModeNoBlock,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			s := testRestarter(rec, tc.systemdRun, tc.cmd, tc.prepend)

			cmd, mode := s.buildRestartCmd(sampleRequest())
			if got := strings.Join(cmd.Args, " "); got != tc.wantArgv {
				t.Errorf("argv:\n got %q\nwant %q", got, tc.wantArgv)
			}
			if mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", mode, tc.wantMode)
			}

			if err := s.Restart(sampleRequest()); err != nil {
				t.Fatalf("Restart: %v", err)
			}
			if i := rec.indexOf("exec " + tc.wantArgv); i < 0 {
				t.Errorf("expected exec of %q, recorded %v", tc.wantArgv, rec.snapshot())
			}
		})
	}
}

// TestRestart_NoCancellableContext is the core regression guard: the restart
// child must not be bound to any caller context. exec.CommandContext is what
// wires a context into a command (it populates Cmd.Cancel), so an unset Cancel
// proves no cancellable context reached the process. Restart itself takes no
// context.Context parameter at all, so this cannot regress by accident.
func TestRestart_NoCancellableContext(t *testing.T) {
	rec := &recorder{}
	s := testRestarter(rec, true, "", nil)

	cmd, _ := s.buildRestartCmd(sampleRequest())
	if cmd.Cancel != nil {
		t.Error("restart command has a Cancel func: a cancellable context was wired in")
	}
	if cmd.WaitDelay != 0 {
		t.Errorf("restart command has WaitDelay %v: a context deadline was wired in", cmd.WaitDelay)
	}

	// Cancelling a caller context around the call changes nothing: the restart
	// still runs and reports no cancellation error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	<-ctx.Done()

	if err := s.Restart(sampleRequest()); err != nil {
		t.Fatalf("Restart after caller cancellation: %v", err)
	}
	if rec.indexOf("exec ") < 0 {
		t.Fatalf("restart was not started, recorded %v", rec.snapshot())
	}
}

// TestRestart_LogsIntentBeforeExec asserts the intent line lands in the log
// before the process is spawned — once the restart starts, this daemon can be
// SIGKILLed at any moment, so a line written afterwards may never exist.
func TestRestart_LogsIntentBeforeExec(t *testing.T) {
	rec := &recorder{}
	s := testRestarter(rec, true, "", nil)
	req := sampleRequest()

	if err := s.Restart(req); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	logIdx := rec.indexOf("self-deploy: restarting unit")
	execIdx := rec.indexOf("exec ")
	if logIdx < 0 {
		t.Fatalf("no intent log line recorded: %v", rec.snapshot())
	}
	if execIdx < 0 {
		t.Fatalf("no exec recorded: %v", rec.snapshot())
	}
	if logIdx >= execIdx {
		t.Fatalf("intent log must precede the exec (log=%d exec=%d): %v", logIdx, execIdx, rec.snapshot())
	}

	line := rec.snapshot()[logIdx]
	for _, want := range []string{req.BuildSHA, req.BinaryPath, req.RollbackPath, req.Unit, ModeSystemdRun} {
		if !strings.Contains(line, want) {
			t.Errorf("intent log line missing %q: %s", want, line)
		}
	}
}

// TestRestart_StartFailureIsReported keeps the rollback path working: a restart
// that never even spawned must surface as an error so Deploy restores the
// previous binary.
func TestRestart_StartFailureIsReported(t *testing.T) {
	rec := &recorder{}
	s := testRestarter(rec, true, "", nil)
	s.start = func(*exec.Cmd) error { return errors.New("permission denied") }

	err := s.Restart(sampleRequest())
	if err == nil {
		t.Fatal("expected an error when the restart fails to spawn")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error should wrap the spawn failure, got %v", err)
	}
	if rec.indexOf("self-deploy: restarting unit") < 0 {
		t.Errorf("intent must still be logged when the spawn fails: %v", rec.snapshot())
	}
}

// TestRestart_ScopeUnitNaming checks the transient scope name stays unique and
// systemd-legal even with a missing or awkward build SHA.
func TestRestart_ScopeUnitNaming(t *testing.T) {
	tests := []struct {
		name string
		sha  string
		want string
	}{
		{name: "short sha", sha: "abc123", want: "forge-selfdeploy-abc123-4242"},
		{name: "long sha truncated", sha: "cafebabe0123456789abcdef", want: "forge-selfdeploy-cafebabe0123-4242"},
		{name: "empty sha", sha: "", want: "forge-selfdeploy-4242"},
		{name: "illegal chars stripped", sha: "ab/c 1:2*3", want: "forge-selfdeploy-abc123-4242"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := testRestarter(&recorder{}, true, "", nil)
			if got := s.scopeUnit(RestartRequest{Unit: "forge", BuildSHA: tc.sha}); got != tc.want {
				t.Errorf("scopeUnit = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRestart_StdioIsDetached guards against inheriting the daemon's handles:
// the child outlives us, so anything it wrote to an inherited pipe would hit a
// closed descriptor once the daemon dies.
func TestRestart_StdioIsDetached(t *testing.T) {
	s := testRestarter(&recorder{}, true, "", nil)
	cmd, _ := s.buildRestartCmd(sampleRequest())
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Error("restart command must not inherit the daemon's stdio")
	}
}

// TestDeploy_PassesRestartRequest checks the deploy hands the restarter the
// state a post-mortem needs: which commit was built, which binary went live and
// where the rollback copy lives.
func TestDeploy_PassesRestartRequest(t *testing.T) {
	d, _, rest, _, binPath := setup(t, 0, nil)

	// A cancelled caller context must not stop the restart from being requested.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := d.Deploy(ctx); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if rest.called != 1 {
		t.Fatalf("expected one restart, got %d", rest.called)
	}
	if rest.req.Unit != "forge" {
		t.Errorf("Unit = %q", rest.req.Unit)
	}
	if rest.req.BuildSHA != fakeHeadSHA {
		t.Errorf("BuildSHA = %q, want %q", rest.req.BuildSHA, fakeHeadSHA)
	}
	if rest.req.BinaryPath != binPath {
		t.Errorf("BinaryPath = %q, want %q", rest.req.BinaryPath, binPath)
	}
	if rest.req.RollbackPath != binPath+".prev" {
		t.Errorf("RollbackPath = %q, want %q", rest.req.RollbackPath, binPath+".prev")
	}
}

// TestDeploy_MissingSHADoesNotAbort keeps the SHA lookup best-effort: it is
// diagnostic metadata, not a gate on deploying.
func TestDeploy_MissingSHADoesNotAbort(t *testing.T) {
	d, _, rest, _, _ := setup(t, 0, map[string]error{"git rev-parse HEAD": errors.New("not a git repo")})

	if err := d.Deploy(context.Background()); err != nil {
		t.Fatalf("Deploy should tolerate a failed rev-parse: %v", err)
	}
	if rest.called != 1 {
		t.Fatalf("expected the restart to still run, got %d calls", rest.called)
	}
	if rest.req.BuildSHA != "" {
		t.Errorf("BuildSHA = %q, want empty when rev-parse fails", rest.req.BuildSHA)
	}
}
