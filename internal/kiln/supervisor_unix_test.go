//go:build !windows

package kiln

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeHome points ~/.forge/logs at a temporary directory so preview logs never
// touch the developer's real home.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// waitFor polls cond until it holds or the deadline expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func TestStartServiceLogsOutputAndInjectsEnv(t *testing.T) {
	home := fakeHome(t)
	worktree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(worktree, "client"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := PreviewEnv{
		PreviewID: "forge_ir70", BeadID: "Forge-ir70", Branch: "forge/Forge-ir70",
		WorktreePath: worktree, AnvilName: "forge", AnvilPath: "/anvil",
		Ports: map[string]int{"client": 42002},
	}
	spec := ServiceSpec{
		Service: Service{
			Name:    "client",
			Dir:     "client",
			Command: `echo "id=$FORGE_PREVIEW_ID port=$FORGE_PREVIEW_PORT_CLIENT cwd=$(pwd)"; sleep 60`,
		},
		BeadID:       "Forge-ir70",
		WorktreePath: worktree,
		Env:          BuildEnv(os.Environ(), env, nil),
	}

	proc, err := StartService(context.Background(), spec)
	if err != nil {
		t.Fatalf("StartService: %v", err)
	}
	t.Cleanup(func() { _ = proc.Stop(2 * time.Second) })

	wantPath := filepath.Join(home, ".forge", "logs", "Forge-ir70", "preview-client.log")
	if proc.LogPath != wantPath {
		t.Errorf("LogPath = %q, want %q", proc.LogPath, wantPath)
	}
	if proc.PID() <= 0 {
		t.Errorf("PID = %d, want a real pid", proc.PID())
	}

	waitFor(t, 5*time.Second, "the service to log its line", func() bool {
		return strings.Contains(readFile(t, wantPath), "id=forge_ir70")
	})
	log := readFile(t, wantPath)
	if !strings.Contains(log, "port=42002") {
		t.Errorf("port env var missing from log:\n%s", log)
	}
	// The service must run in its manifest dir, not the worktree root. macOS
	// resolves /var → /private/var, so compare the suffix.
	if !strings.Contains(log, "cwd=") || !strings.Contains(log, string(filepath.Separator)+"client") {
		t.Errorf("service did not run in its service directory:\n%s", log)
	}
}

func TestStopKillsTheWholeProcessGroup(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()
	pidFile := filepath.Join(worktree, "child.pid")

	// The leader backgrounds a child and keeps running: killing only the
	// leader would leave the child holding the port.
	spec := ServiceSpec{
		Service: Service{
			Name:    "api",
			Command: "sleep 300 & echo $! > " + pidFile + "; sleep 300",
		},
		BeadID:       "Forge-group",
		WorktreePath: worktree,
		Env:          os.Environ(),
	}
	proc, err := StartService(context.Background(), spec)
	if err != nil {
		t.Fatalf("StartService: %v", err)
	}

	var childPID int
	waitFor(t, 5*time.Second, "the backgrounded child to report its pid", func() bool {
		pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, pidFile)))
		if err != nil || pid <= 0 {
			return false
		}
		childPID = pid
		return true
	})

	if err := proc.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !proc.Exited() {
		t.Error("leader still reported as running after Stop")
	}
	waitFor(t, 5*time.Second, "the backgrounded child to be reaped", func() bool {
		return syscall.Kill(childPID, 0) == syscall.ESRCH
	})

	// Stop is idempotent: teardown paths call it from more than one place.
	if err := proc.Stop(2 * time.Second); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

func TestStopIsSafeAfterTheServiceExitedOnItsOwn(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()

	proc, err := StartService(context.Background(), ServiceSpec{
		Service:      Service{Name: "api", Command: "exit 3"},
		BeadID:       "Forge-exit",
		WorktreePath: worktree,
		Env:          os.Environ(),
	})
	if err != nil {
		t.Fatalf("StartService: %v", err)
	}

	<-proc.Done()
	if proc.ExitErr() == nil {
		t.Error("ExitErr = nil, want the non-zero exit to be reported")
	}
	if err := proc.Stop(time.Second); err != nil {
		t.Errorf("Stop after a self-inflicted exit: %v", err)
	}
}

func TestStartServiceRejectsMissingDir(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()

	_, err := StartService(context.Background(), ServiceSpec{
		Service:      Service{Name: "api", Dir: "does-not-exist", Command: "true"},
		BeadID:       "Forge-nodir",
		WorktreePath: worktree,
		Env:          os.Environ(),
	})
	if err == nil {
		t.Fatal("StartService accepted a missing service directory")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the missing directory: %v", err)
	}
}

func TestServiceLogsAppendAcrossRestarts(t *testing.T) {
	home := fakeHome(t)
	worktree := t.TempDir()

	spec := ServiceSpec{
		Service:      Service{Name: "api", Command: `echo ran-once`},
		BeadID:       "Forge-append",
		WorktreePath: worktree,
		Env:          os.Environ(),
	}
	for i := 0; i < 2; i++ {
		proc, err := StartService(context.Background(), spec)
		if err != nil {
			t.Fatalf("StartService #%d: %v", i, err)
		}
		<-proc.Done()
	}

	// Count output lines only: the start banner echoes the command line too.
	logPath := filepath.Join(home, ".forge", "logs", "Forge-append", "preview-api.log")
	runs := 0
	for _, line := range strings.Split(readFile(t, logPath), "\n") {
		if strings.TrimSpace(line) == "ran-once" {
			runs++
		}
	}
	if runs != 2 {
		t.Errorf("log holds %d runs, want 2 (the log must append, not truncate)", runs)
	}
}

func TestCancellingTheLifetimeContextKillsTheService(t *testing.T) {
	fakeHome(t)
	worktree := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	proc, err := StartService(ctx, ServiceSpec{
		Service:      Service{Name: "api", Command: "sleep 300"},
		BeadID:       "Forge-cancel",
		WorktreePath: worktree,
		Env:          os.Environ(),
	})
	if err != nil {
		t.Fatalf("StartService: %v", err)
	}
	cancel()

	select {
	case <-proc.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("service survived cancellation of its lifetime context")
	}
}

// TestExitCodeReportsAChosenStatus: a process that exited on its own terms has
// an exit status, and that status is what every surface prints.
func TestExitCodeReportsAChosenStatus(t *testing.T) {
	fakeHome(t)

	proc, err := StartService(context.Background(), ServiceSpec{
		Service:      Service{Name: "api", Command: "exit 3"},
		BeadID:       "Forge-code",
		WorktreePath: t.TempDir(),
		Env:          os.Environ(),
	})
	if err != nil {
		t.Fatalf("StartService: %v", err)
	}
	<-proc.Done()

	code, ok := proc.ExitCode()
	if !ok || code != 3 {
		t.Fatalf("ExitCode() = (%d, %v), want (3, true)", code, ok)
	}
}

// TestExitCodeRefusesToInventOneForASignalledProcess is the guard's whole
// reason for existing: exec reports -1 for a process killed by a signal, and
// -1 is not a status the program chose. Reporting it anyway would put
// `exited (exit -1, ...)` on every surface — a number that reads as the
// service's own decision — instead of the signal FormatExitCause names.
func TestExitCodeRefusesToInventOneForASignalledProcess(t *testing.T) {
	fakeHome(t)

	proc, err := StartService(context.Background(), ServiceSpec{
		Service:      Service{Name: "api", Command: "sleep 300"},
		BeadID:       "Forge-signal",
		WorktreePath: t.TempDir(),
		Env:          os.Environ(),
	})
	if err != nil {
		t.Fatalf("StartService: %v", err)
	}

	// Still running: there is no status to report yet, which is the same
	// refusal for a different reason.
	if code, ok := proc.ExitCode(); ok {
		t.Errorf("ExitCode() = (%d, true) while running, want no code", code)
	}

	if err := proc.Stop(2 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	<-proc.Done()

	exitErr := proc.ExitErr()
	if exitErr == nil {
		t.Fatal("ExitErr = nil after a signalled death, want the signal")
	}
	if code, ok := proc.ExitCode(); ok {
		t.Fatalf("ExitCode() = (%d, true) for a signalled process, want no code", code)
	}

	// The extraction and the renderer, end to end: nil code means the cause
	// comes from the wait error rather than an invented status.
	if got := FormatServiceExit(nil, exitErr, 90*time.Second); !strings.Contains(got, "signal") {
		t.Errorf("FormatServiceExit = %q, want the signal named", got)
	}
}

// TestExitCodeOnANilProcess: the accessor is called from the exit watcher, which
// runs against services that may never have spawned.
func TestExitCodeOnANilProcess(t *testing.T) {
	var proc *ServiceProcess
	if code, ok := proc.ExitCode(); ok {
		t.Errorf("ExitCode() = (%d, true) on a nil process, want no code", code)
	}
}

func TestServiceLogPath(t *testing.T) {
	home := fakeHome(t)

	got, err := ServiceLogPath("forge/Forge-ir70", "api")
	if err != nil {
		t.Fatalf("ServiceLogPath: %v", err)
	}
	// The bead id is sanitized the same way the pipeline's preserved logs are,
	// so both land in one directory per bead.
	want := filepath.Join(home, ".forge", "logs", "forge_Forge-ir70", "preview-api.log")
	if got != want {
		t.Errorf("ServiceLogPath = %q, want %q", got, want)
	}
}
