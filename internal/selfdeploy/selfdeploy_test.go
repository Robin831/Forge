package selfdeploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHeadSHA is what the fake `git rev-parse HEAD` reports.
const fakeHeadSHA = "cafebabe0123456789abcdef"

// fakeCommander records the commands it is asked to run and fails any command
// whose name+first-arg (or name+first-two-args, which is what distinguishes
// `git rev-parse HEAD` from the stash probe) matches a key in failOn. For a
// `go build` invocation it writes a stub binary to the -o target so the swap
// step has a real file.
type fakeCommander struct {
	mu     sync.Mutex
	calls  [][]string
	failOn map[string]error // key: "name arg0" or "name arg0 arg1"
	// stash, when set, is what the stash probe reports as the top of the stack.
	// Empty means an empty stack, which is what a clean tree leaves behind and
	// so what every test that is not about stashing wants.
	stash string
}

func (f *fakeCommander) Run(_ context.Context, dir, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))

	// The longer key is tried first so a test can fail one rev-parse without
	// failing the other.
	for n := 2; n >= 0; n-- {
		if len(args) < n {
			continue
		}
		key := strings.Join(append([]string{name}, args[:n]...), " ")
		if err, ok := f.failOn[key]; ok {
			return []byte("boom: " + key), err
		}
	}

	// Emulate the stash probe, which exits 0 either way: empty output is an
	// empty stack, and a non-zero exit is a real failure.
	if name == "git" && len(args) > 0 && args[0] == "for-each-ref" {
		return []byte(f.stash), nil
	}

	// Emulate the unmerged-index probe, which also exits 0 either way: empty
	// output is an index with no conflicts in it, which is what every test that
	// is not about a mid-merge checkout wants. Without this the fake's catch-all
	// "ok" reads as three unmerged entries and every deploy refuses to pull.
	if name == "git" && len(args) > 0 && args[0] == "ls-files" {
		return nil, nil
	}

	// Emulate `git rev-parse HEAD` so the deploy can stamp a build SHA.
	if name == "git" && len(args) > 0 && args[0] == "rev-parse" {
		return []byte(fakeHeadSHA + "\n"), nil
	}

	// Emulate `go build -o <path> <target>` by producing a stub binary.
	if name == "go" && len(args) >= 3 && args[0] == "build" {
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "-o" {
				_ = os.WriteFile(args[i+1], []byte("#!stub\n"), 0o755)
			}
		}
	}
	return []byte("ok"), nil
}

func (f *fakeCommander) ran(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return true
		}
	}
	return false
}

type fakeRestarter struct {
	called int
	err    error
	unit   string
	req    RestartRequest
}

func (f *fakeRestarter) Restart(req RestartRequest) error {
	f.called++
	f.unit = req.Unit
	f.req = req
	return f.err
}

type fakeSink struct {
	mu     sync.Mutex
	events []string // "event: message"
}

func (s *fakeSink) Emit(event, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event+": "+message)
}

func (s *fakeSink) has(event string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if strings.HasPrefix(e, event+":") {
			return true
		}
	}
	return false
}

// setup returns a Deployer wired against a temp dir with an existing "live"
// binary, plus the fakes so tests can assert on them. workers is the number of
// permanently-active workers the drain check reports; a non-zero value means the
// deploy can only ever end in a drain timeout, so the wait budget is kept tiny.
func setup(t *testing.T, workers int, failOn map[string]error) (*Deployer, *fakeCommander, *fakeRestarter, *fakeSink, string) {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "forge")
	if err := os.WriteFile(binPath, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := &fakeCommander{failOn: failOn}
	rest := &fakeRestarter{}
	sink := &fakeSink{}
	ids := make([]string, workers)
	for i := range ids {
		ids[i] = fmt.Sprintf("Forge-w%d", i)
	}
	d := New(
		Config{RepoPath: dir, BinaryPath: binPath, MaxDrainWait: time.Nanosecond},
		cmd, rest, sink,
		func() ([]string, error) { return ids, nil },
	)
	return d, cmd, rest, sink, binPath
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestDeploy_SkipsWhenWorkersNeverDrain(t *testing.T) {
	d, cmd, rest, sink, binPath := setup(t, 2, nil)

	err := d.Deploy(context.Background())
	if !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("want ErrDrainTimeout, got %v", err)
	}
	if !strings.Contains(err.Error(), "Forge-w0") {
		t.Fatalf("drain timeout must name the active workers, got %q", err)
	}
	if cmd.ran("git pull") || cmd.ran("go build") {
		t.Fatalf("no build/pull should run while workers active; calls=%v", cmd.calls)
	}
	if rest.called != 0 {
		t.Fatalf("restart must not be called while workers active")
	}
	if !sink.has(EventSkipped) {
		t.Fatalf("expected a skipped event, got %v", sink.events)
	}
	if got := readFile(t, binPath); got != "OLD" {
		t.Fatalf("live binary must be untouched, got %q", got)
	}
	if _, err := os.Stat(binPath + ".prev"); !os.IsNotExist(err) {
		t.Fatalf("no prev binary should exist on skip")
	}
}

func TestDeploy_Success(t *testing.T) {
	d, cmd, rest, sink, binPath := setup(t, 0, nil)

	if err := d.Deploy(context.Background()); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !cmd.ran("git pull --ff-only origin main") {
		t.Fatalf("expected ff-only pull of main; calls=%v", cmd.calls)
	}
	if !cmd.ran("go build -o") {
		t.Fatalf("expected go build; calls=%v", cmd.calls)
	}
	// Verification must have run both subcommands on the temp binary.
	if !cmd.ran(binPath+".new version") || !cmd.ran(binPath+".new --help") {
		t.Fatalf("expected verify version+--help on new binary; calls=%v", cmd.calls)
	}
	if rest.called != 1 || rest.unit != "forge" {
		t.Fatalf("expected one restart of unit forge, got called=%d unit=%q", rest.called, rest.unit)
	}
	// New binary is live, previous kept for rollback, temp gone.
	if got := readFile(t, binPath); got != "#!stub\n" {
		t.Fatalf("live binary should be the newly built stub, got %q", got)
	}
	if got := readFile(t, binPath+".prev"); got != "OLD" {
		t.Fatalf("prev binary should hold the old binary, got %q", got)
	}
	if _, err := os.Stat(binPath + ".new"); !os.IsNotExist(err) {
		t.Fatalf("temp binary should be renamed away")
	}
	if !sink.has(EventStarted) || !sink.has(EventSuccess) {
		t.Fatalf("expected started+success events, got %v", sink.events)
	}
}

func TestDeploy_VerifyFailureAbortsSwap(t *testing.T) {
	// The new binary's `version` check exits non-zero → no swap, no restart.
	failOn := map[string]error{}
	d, cmd, rest, sink, binPath := setup(t, 0, failOn)
	// Make verify fail by failing the version subcommand keyed by full path.
	cmd.failOn[binPath+".new version"] = errors.New("crashes on start")

	err := d.Deploy(context.Background())
	if err == nil {
		t.Fatal("expected error when verification fails")
	}
	if rest.called != 0 {
		t.Fatalf("restart must not run when verify fails")
	}
	if got := readFile(t, binPath); got != "OLD" {
		t.Fatalf("live binary must be untouched on verify failure, got %q", got)
	}
	if _, err := os.Stat(binPath + ".prev"); !os.IsNotExist(err) {
		t.Fatalf("no prev binary should be created when verify fails")
	}
	if _, err := os.Stat(binPath + ".new"); !os.IsNotExist(err) {
		t.Fatalf("temp binary should be cleaned up on verify failure")
	}
	if !sink.has(EventFailed) {
		t.Fatalf("expected failed event, got %v", sink.events)
	}
}

func TestDeploy_GitPullFailureAborts(t *testing.T) {
	d, cmd, rest, _, binPath := setup(t, 0, map[string]error{"git pull": errors.New("diverged")})

	if err := d.Deploy(context.Background()); err == nil {
		t.Fatal("expected error when git pull fails")
	}
	if cmd.ran("go build") {
		t.Fatalf("build must not run after pull failure")
	}
	if rest.called != 0 {
		t.Fatalf("restart must not run after pull failure")
	}
	if got := readFile(t, binPath); got != "OLD" {
		t.Fatalf("live binary must be untouched, got %q", got)
	}
}

func TestDeploy_BuildFailureAborts(t *testing.T) {
	d, _, rest, _, binPath := setup(t, 0, map[string]error{"go build": errors.New("compile error")})

	if err := d.Deploy(context.Background()); err == nil {
		t.Fatal("expected error when build fails")
	}
	if rest.called != 0 {
		t.Fatalf("restart must not run after build failure")
	}
	if got := readFile(t, binPath); got != "OLD" {
		t.Fatalf("live binary must be untouched, got %q", got)
	}
}

func TestDeploy_RestartFailureRollsBack(t *testing.T) {
	d, _, rest, sink, binPath := setup(t, 0, nil)
	rest.err = errors.New("systemctl restart failed")

	err := d.Deploy(context.Background())
	if err == nil {
		t.Fatal("expected error when restart fails")
	}
	if rest.called != 1 {
		t.Fatalf("expected one restart attempt, got %d", rest.called)
	}
	// Rollback: the live binary must be restored to the previous content.
	if got := readFile(t, binPath); got != "OLD" {
		t.Fatalf("expected rollback to restore OLD binary, got %q", got)
	}
	if !sink.has(EventRollback) {
		t.Fatalf("expected rollback event, got %v", sink.events)
	}
}

func TestDeploy_DefaultsApplied(t *testing.T) {
	cfg := Config{RepoPath: "/repo", BinaryPath: "/opt/forge"}
	d := New(cfg, &fakeCommander{}, &fakeRestarter{}, &fakeSink{}, nil)
	if d.cfg.PrevPath != "/opt/forge.prev" {
		t.Errorf("PrevPath default = %q", d.cfg.PrevPath)
	}
	if d.cfg.UnitName != "forge" {
		t.Errorf("UnitName default = %q", d.cfg.UnitName)
	}
	if d.cfg.Branch != "main" {
		t.Errorf("Branch default = %q", d.cfg.Branch)
	}
	if d.cfg.BuildTarget != "./cmd/forge" {
		t.Errorf("BuildTarget default = %q", d.cfg.BuildTarget)
	}
	if d.cfg.MaxDrainWait != DefaultMaxDrainWait {
		t.Errorf("MaxDrainWait default = %s", d.cfg.MaxDrainWait)
	}
	if d.cfg.DrainInterval != DefaultDrainInterval {
		t.Errorf("DrainInterval default = %s", d.cfg.DrainInterval)
	}
}

func TestDeploy_NoActiveWorkerCheck(t *testing.T) {
	// A nil activeWorkers func disables the guard entirely (deploy proceeds).
	dir := t.TempDir()
	binPath := filepath.Join(dir, "forge")
	if err := os.WriteFile(binPath, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	rest := &fakeRestarter{}
	d := New(Config{RepoPath: dir, BinaryPath: binPath}, &fakeCommander{}, rest, &fakeSink{}, nil)
	if err := d.Deploy(context.Background()); err != nil {
		t.Fatalf("Deploy with nil worker check: %v", err)
	}
	if rest.called != 1 {
		t.Fatalf("expected restart to run when worker check disabled")
	}
}
