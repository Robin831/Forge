package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/prompt"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/worktree"
)

// The shared daemon test fixture.
//
// What it exists for is one fixture rather than three: the sibling sub-tasks of
// this epic each need a daemon standing over a real database with rows planted
// before it starts, a real process to kill out from under it, and an assertion
// that waits for a transition instead of sleeping past it. Written per test
// those three come out subtly different — one seeds through InsertWorker and so
// cannot plant a prior generation at all, one greps a bytes.Buffer the daemon's
// own goroutine is writing to, one sleeps 200ms and passes on a slow host by
// luck — and the differences are exactly where the evidence lives.
//
// Three properties are deliberate:
//
//   - The database is a real on-disk SQLite file opened through state.Open, so
//     the schema, the migrations and the queries under test are the production
//     ones. Not `:memory:`, whose per-connection lifetime is a different
//     database from the one the daemon runs against, and not a mock, which
//     would assert the reaper's SQL against a second idea of what it says.
//   - Seeding is raw SQL. The ownership columns are the whole subject here, and
//     every production insert path stamps them itself (state.InsertWorker takes
//     them from db.generation()), so a row from a daemon lifetime that ended —
//     the leak this epic is about — is not expressible through them.
//   - The child process is real and killable. A stub PID proves nothing about a
//     reaper written not to read pids; a genuine process that can be SIGKILLed
//     and reaped is what lets a test say a row outlived its process.

const (
	// defaultWaitTimeout bounds every poll-and-assert helper. Long enough that
	// a loaded CI host does not fail a correct daemon, short enough that a
	// wrong one fails rather than hangs until the package's own test timeout.
	defaultWaitTimeout = 5 * time.Second

	// waitTick is how often a condition is re-checked. Short, because the
	// helpers exist so a test never has to pick a sleep that is long enough.
	waitTick = 5 * time.Millisecond

	// testPollInterval is the harness default poll interval — milliseconds
	// against the shipped 5m, so a test that drives the poll loop observes
	// several cycles inside defaultWaitTimeout.
	testPollInterval = 20 * time.Millisecond

	// testMaxWorkers mirrors the shipped settings.max_total_smiths default, so
	// a test that does not care about the limit runs against the production
	// number rather than one invented here.
	testMaxWorkers = 4

	// testAnvil is the anvil name every seeding helper defaults to. One name,
	// so a seeded worker row and a seeded bead are about the same anvil unless
	// a test deliberately says otherwise.
	testAnvil = "repo"

	// dbTimeFormat mirrors state's canonical (unexported) column layout:
	// fixed-width, so the lexicographic ORDER BY the production queries use is
	// chronological. state.parseTime also accepts RFC3339Nano, but a seeded row
	// should be byte-identical to one the daemon wrote, not merely parseable.
	dbTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

	// helperProcessEnv gates TestHelperSleep, which is a subprocess and not a
	// test. Without the gate `go test` would run it and block for helperSleep.
	helperProcessEnv = "GO_WANT_HELPER_PROCESS"

	// helperSleep bounds a dummy worker that nothing kills. Every spawn
	// registers a cleanup that kills and reaps it, so this is only the backstop
	// for a harness bug — long enough to outlive any test, finite so a leaked
	// child cannot sit on the host forever.
	helperSleep = 10 * time.Minute
)

// testCfg holds the knobs newTestDaemon exposes. It is a struct behind
// functional options rather than a widening parameter list so a sibling
// sub-task can add one without touching every existing call.
type testCfg struct {
	pollInterval time.Duration
	maxWorkers   int
	logLevel     slog.Level
	generation   string
	dbPath       string
}

type testOpt func(*testCfg)

// withPollInterval overrides settings.poll_interval for the daemon under test.
func withPollInterval(d time.Duration) testOpt {
	return func(c *testCfg) { c.pollInterval = d }
}

// withWorkerLimit overrides settings.max_total_smiths — the global dispatch
// ceiling — for the daemon under test.
func withWorkerLimit(n int) testOpt {
	return func(c *testCfg) { c.maxWorkers = n }
}

// withLogLevel sets the level below which the capture sink drops records.
// Debug by default, so a test asserting on a WARN never has to raise it.
func withLogLevel(l slog.Level) testOpt {
	return func(c *testCfg) { c.logLevel = l }
}

// withGeneration names the daemon lifetime the env runs as. Tests that plant a
// row from another lifetime do not need it — they set workerRow.Generation —
// but a test driving two generations against one database does.
func withGeneration(gen string) testOpt {
	return func(c *testCfg) { c.generation = gen }
}

// withDBPath points the env at an existing state.db instead of creating one in
// its own temp directory. It is what makes a RESTART expressible: the leak the
// generation column exists to close is a row written by a lifetime that ended,
// and the only faithful way to produce one is a second daemon over the first
// one's database. Planting a made-up generation string states the same
// condition; opening the same file proves the two daemons disagree about
// ownership of a row neither of them was told about.
func withDBPath(path string) testOpt {
	return func(c *testCfg) { c.dbPath = path }
}

// testEnv is one daemon, one database and one temp directory, torn down
// together.
type testEnv struct {
	t      *testing.T
	dir    string
	dbPath string
	db     *state.DB
	d      *Daemon
	sink   *captureHandler

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	stopOnce sync.Once

	// mu guards children, which spawnDummyWorker writes and killWorker reads.
	mu       sync.Mutex
	children map[int]*dummyWorker

	seedSeq int
}

// newTestDaemon builds a daemon over a real, migrated state.db in a fresh temp
// directory. It does NOT start anything: the fixture exists so leaked, paused
// and non-terminal rows can be planted BEFORE the daemon's loops observe them,
// which is only possible if construction and start are two steps. Call
// e.start() (or e.startLoop) once the database says what the test needs it to.
//
// Teardown is registered here and is idempotent, so a test may stop the daemon
// early to assert on what it left behind.
func newTestDaemon(t *testing.T, opts ...testOpt) *testEnv {
	t.Helper()

	cfg := testCfg{
		pollInterval: testPollInterval,
		maxWorkers:   testMaxWorkers,
		logLevel:     slog.LevelDebug,
		generation:   "gen-test-running",
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	dir := t.TempDir()
	dbPath := cfg.dbPath
	if dbPath == "" {
		dbPath = filepath.Join(dir, "state.db")
	}
	db, err := state.Open(dbPath)
	require.NoError(t, err, "opening state.db at %s", dbPath)

	sink := newCaptureHandler(cfg.logLevel)

	d := &Daemon{
		db:            db,
		logger:        slog.New(sink),
		forgeDir:      dir,
		worktreeMgr:   worktree.NewManager(),
		promptBuilder: prompt.NewBuilder(),
		reqTracker:    *ipc.NewRequestTracker("test-"),
	}
	d.cfg.Store(&config.Config{
		Settings: config.SettingsConfig{
			PollInterval:   cfg.pollInterval,
			MaxTotalSmiths: cfg.maxWorkers,
		},
		Anvils: map[string]config.AnvilConfig{},
	})

	// The generation is set on the daemon and on the DB together, exactly as
	// Daemon.Run does: the daemon compares against it and every InsertWorker
	// stamps it, and a fixture where the two disagree would have the daemon
	// reap the rows it had just written itself.
	d.workerGeneration = cfg.generation
	db.SetDaemonGeneration(cfg.generation)

	ctx, cancel := context.WithCancel(context.Background())
	// IPC handlers read runCtx, and the shutdown command cancels through
	// d.cancel; wiring both to the env's context means either route ends the
	// env's goroutines rather than leaving them running past the test.
	d.runCtx = ctx
	d.cancel = cancel

	e := &testEnv{
		t:        t,
		dir:      dir,
		dbPath:   dbPath,
		db:       db,
		d:        d,
		sink:     sink,
		ctx:      ctx,
		cancel:   cancel,
		children: make(map[int]*dummyWorker),
	}
	t.Cleanup(e.stop)
	return e
}

// generation returns the daemon lifetime this env runs as — the value a seeded
// row must carry to read as owned by it.
func (e *testEnv) generation() string { return e.d.workerGeneration }

// conn is the raw database handle, for a test that needs a query this file does
// not wrap.
func (e *testEnv) conn() *sql.DB { return e.db.Conn() }

// reconfigure applies a mutation to the daemon's live configuration, in place
// of a widening set of construction options.
//
// It exists because the settings a test needs are not all knowable before the
// env has a temp directory: an anvil's Path is e.dir, and an anvil is what a
// test driving the poll loop must configure. The current config is copied and
// the copy stored, which is how the daemon's own hot reload swaps one in — so a
// goroutine holding the pointer it loaded keeps reading a consistent view
// rather than one mutated underneath it.
func (e *testEnv) reconfigure(mutate func(*config.Config)) {
	e.t.Helper()
	next := *e.d.config()
	mutate(&next)
	e.d.cfg.Store(&next)
}

// start launches the daemon's worker-ownership loops — the heartbeat and the
// periodic leaked-worker reaper — under the env's context and waitgroup, in the
// order and with the wiring Daemon.Run uses.
//
// It is deliberately those two and not Daemon.Run: Run writes a pid file, opens
// an IPC socket, shells out to bd and reaches the real ~/.forge, none of which a
// package test may do. A sibling sub-task needing another loop starts it with
// startLoop rather than widening this.
func (e *testEnv) start() {
	e.t.Helper()
	e.startLoop(e.d.runWorkerHeartbeat)
	e.startLoop(e.d.runLeakedWorkerReaper)
}

// startLoop runs one daemon loop under the env's context and waitgroup, so stop
// both ends it and waits for it. The loop must return when its context is done
// — which is what a test asserting on shutdown gets for free.
func (e *testEnv) startLoop(fn func(context.Context)) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		fn(e.ctx)
	}()
}

// stop cancels the daemon, waits for its goroutines and closes the database.
//
// Registered as the cleanup and safe to call early; the second call is a no-op,
// so a test that stops the daemon to assert on what it left behind does not
// double-close. The wait is bounded and REPORTED rather than left to hang: a
// loop that ignores its context is a defect worth naming, and a cleanup that
// blocks forever names nothing.
func (e *testEnv) stop() {
	e.stopOnce.Do(func() {
		e.cancel()

		done := make(chan struct{})
		go func() {
			e.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(defaultWaitTimeout):
			e.t.Errorf("test daemon goroutines did not exit within %s of cancellation", defaultWaitTimeout)
		}

		if err := e.db.Close(); err != nil {
			e.t.Errorf("closing state.db: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Log capture
// ---------------------------------------------------------------------------

// logRecord is one captured log line: the two fields an assertion is made on,
// plus the attributes, since a WARN naming the wrong worker is not the WARN
// under test.
type logRecord struct {
	Level   slog.Level
	Message string
	Attrs   map[string]any
}

// Attr returns one attribute value and whether it was present.
func (r logRecord) Attr(key string) (any, bool) {
	v, ok := r.Attrs[key]
	return v, ok
}

// captureHandler is the slog.Handler behind e.logs(). It exists because the
// alternative in this package is a bytes.Buffer read from the test goroutine
// while the daemon's own goroutine writes it — a data race `-race` will fail
// on, and, when it does not, a substring match against a formatted line rather
// than an assertion about a level and a message.
//
// records and mu are shared by POINTER with every handler WithAttrs/WithGroup
// derives, so a logger the daemon decorated still appends to the one log the
// test reads.
type captureHandler struct {
	level   slog.Level
	mu      *sync.Mutex
	records *[]logRecord
	attrs   []slog.Attr
	groups  []string
}

func newCaptureHandler(level slog.Level) *captureHandler {
	return &captureHandler{
		level:   level,
		mu:      &sync.Mutex{},
		records: &[]logRecord{},
	}
}

func (h *captureHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *captureHandler) Handle(_ context.Context, rec slog.Record) error {
	attrs := make(map[string]any, rec.NumAttrs()+len(h.attrs))
	for _, a := range h.attrs {
		h.put(attrs, a)
	}
	rec.Attrs(func(a slog.Attr) bool {
		h.put(attrs, a)
		return true
	})

	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, logRecord{
		Level:   rec.Level,
		Message: rec.Message,
		Attrs:   attrs,
	})
	return nil
}

// put writes one attribute under its group-qualified key, resolving a LogValuer
// so a lazily-formatted value is captured as what it renders to rather than as
// the wrapper.
func (h *captureHandler) put(dst map[string]any, a slog.Attr) {
	a.Value = a.Value.Resolve()
	key := a.Key
	if len(h.groups) > 0 {
		key = strings.Join(append(append([]string{}, h.groups...), a.Key), ".")
	}
	dst[key] = a.Value.Any()
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &next
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.groups = append(append([]string{}, h.groups...), name)
	return &next
}

// logs returns a snapshot of everything logged so far, in order.
//
// The slice is a copy, so the daemon appending to the log while a test walks
// the result is not a race. Each record's Attrs map is built once in Handle and
// never written again, so it is shared rather than cloned — a test must not
// mutate it.
func (e *testEnv) logs() []logRecord {
	e.sink.mu.Lock()
	defer e.sink.mu.Unlock()
	out := make([]logRecord, len(*e.sink.records))
	copy(out, *e.sink.records)
	return out
}

// findLog returns the first record at the given level whose message contains
// substr, so an assertion can go on to read the attributes it carried.
func (e *testEnv) findLog(level slog.Level, substr string) (logRecord, bool) {
	for _, rec := range e.logs() {
		if rec.Level == level && strings.Contains(rec.Message, substr) {
			return rec, true
		}
	}
	return logRecord{}, false
}

// hasLog reports whether anything was logged at the given level with a message
// containing substr.
func (e *testEnv) hasLog(level slog.Level, substr string) bool {
	_, ok := e.findLog(level, substr)
	return ok
}

// waitForLog blocks until hasLog is satisfied, for an assertion about a line a
// background loop has yet to write.
func (e *testEnv) waitForLog(t *testing.T, level slog.Level, substr string) {
	t.Helper()
	e.waitFor(t, func() bool { return e.hasLog(level, substr) },
		fmt.Sprintf("a %s log containing %q", level, substr))
}

// ---------------------------------------------------------------------------
// Real, killable child processes
// ---------------------------------------------------------------------------

// TestHelperSleep is not a test: it is the process spawnDummyWorker starts.
// The test binary re-executes itself because a long-lived child has to be a
// real process — a reaper written not to read pids is only shown to be right by
// a row whose process genuinely outlives, or genuinely predeceases, it — and
// `sleep` is not portable to every host this suite runs on.
//
// It runs only when GO_WANT_HELPER_PROCESS=1, which spawnDummyWorker sets;
// under an ordinary `go test` it skips immediately.
func TestHelperSleep(t *testing.T) {
	if os.Getenv(helperProcessEnv) != "1" {
		t.Skip("helper process; only runs when spawned by spawnDummyWorker")
	}
	time.Sleep(helperSleep)
}

// dummyWorker is one spawned child and the once that reaps it. The once is what
// keeps killWorker and the spawn's own cleanup from both calling Wait, whose
// second call reports an error about a process that was already collected.
type dummyWorker struct {
	cmd  *exec.Cmd
	once sync.Once
}

// kill ends the process and reaps it.
//
// Both halves matter, and the second is the one that is easy to omit: on Linux
// a killed child that nothing has waited for is a ZOMBIE, and a zombie still
// answers kill(pid, 0) — so processAlive, and any production liveness probe
// shaped like it, reports a process the test believes it has destroyed as
// running. Wait is what makes "gone" true.
func (w *dummyWorker) kill() {
	if w.cmd.Process != nil {
		// Process.Kill is SIGKILL on Unix and TerminateProcess on Windows: the
		// unblockable ending this fixture is for, without a signal constant
		// that does not exist on every platform this package builds for. An
		// error here means the process is already finished, which is the state
		// being asked for.
		_ = w.cmd.Process.Kill()
	}
	w.once.Do(func() { _ = w.cmd.Wait() })
}

// spawnDummyWorker starts a real long-lived child process and returns its pid.
//
// The process is killed and reaped by a registered cleanup whether or not the
// test kills it itself, so a package run with -count=N leaks nothing.
//
// It starts in its OWN process group, which is not a detail: the daemon's
// killWorkerProcess resolves a worker's group from its pid and signals the
// whole group, so a child left in the test binary's group would have any test
// that plants this pid on a worker row and drives kill_worker, stop_bead or
// detach_bellows deliver SIGINT and then SIGKILL to `go test` itself. The call
// is executil's rather than a bare SysProcAttr because Setpgid does not exist
// on Windows, where the same helper sets CREATE_NEW_PROCESS_GROUP — and it is
// what production spawns use, so the child this fixture hands a test is
// grouped exactly as the process a real worker row names.
func (e *testEnv) spawnDummyWorker(t *testing.T) (int, *exec.Cmd) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperSleep$")
	cmd.Env = append(os.Environ(), helperProcessEnv+"=1")
	executil.SetProcessGroup(cmd)
	require.NoError(t, cmd.Start(), "starting dummy worker process")

	pid := cmd.Process.Pid
	w := &dummyWorker{cmd: cmd}

	e.mu.Lock()
	e.children[pid] = w
	e.mu.Unlock()

	t.Cleanup(w.kill)
	return pid, cmd
}

// killWorker kills the process with the given pid and returns once it is
// genuinely gone rather than a zombie.
//
// A pid this harness spawned is reaped through its own dummyWorker (see
// dummyWorker.kill for why the reap is not optional). A pid it did not spawn
// can only be signalled and then POLLED — nothing here can collect it, since
// only its real parent may wait on it — so on such a pid the wait below runs
// until the child's own parent reaps it or the timeout fires: a zombie still
// answers processAlive's kill(pid, 0), which is the reap dummyWorker.kill
// exists to perform.
func (e *testEnv) killWorker(pid int) {
	e.t.Helper()

	e.mu.Lock()
	w := e.children[pid]
	e.mu.Unlock()

	if w != nil {
		w.kill()
	} else if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}

	e.waitFor(e.t, func() bool { return !processAlive(pid) },
		fmt.Sprintf("process %d to be gone", pid))
}

// ---------------------------------------------------------------------------
// Direct database seeding
// ---------------------------------------------------------------------------

// workerRow is a worker row as a test wants to plant it. It mirrors the columns
// rather than state.Worker because it carries the two the production insert
// path refuses to take from a caller — see seedWorkerRow.
type workerRow struct {
	ID       string
	BeadID   string
	Anvil    string
	Branch   string
	PID      int
	Status   state.WorkerStatus
	Phase    string
	Title    string
	PRNumber int

	StartedAt   time.Time
	CompletedAt time.Time
	LogPath     string

	// PrevStatus is the status the watchdog captured when it stalled the row.
	// It is part of the reaper's exemption test (a row stalled over a
	// monitoring one is a handoff, not a leak), so it is plantable.
	PrevStatus state.WorkerStatus
	SessionID  string

	// Generation and Heartbeat are the ownership columns, and they are written
	// EXACTLY as given — no default, no fallback to the running daemon's. The
	// empty string and the zero time are meaningful values (a row from a build
	// that predates the columns, which every reader treats as "not this
	// daemon's"), and a helper that quietly filled them in would make the one
	// row this fixture exists to plant — one a daemon lifetime that has ended
	// left behind — inexpressible.
	//
	// A row meant to read as owned by the running daemon carries
	// e.generation() and a recent Heartbeat.
	Generation string
	Heartbeat  time.Time
}

// seedWorkerRow writes a worker row straight into state.db.
//
// It bypasses state.InsertWorker deliberately: that path stamps
// daemon_generation and heartbeat_at from the DB's own current generation,
// which is right for production (a leak is created by whichever insert site
// forgets) and is exactly what makes a leaked row impossible to plant through
// it.
//
// The identity and display fields are defaulted so a test states only what it
// is about; the ownership columns are not (see workerRow).
func (e *testEnv) seedWorkerRow(row workerRow) workerRow {
	e.t.Helper()

	e.seedSeq++
	if row.ID == "" {
		row.ID = fmt.Sprintf("worker-%d", e.seedSeq)
	}
	if row.BeadID == "" {
		row.BeadID = fmt.Sprintf("Forge-seed%d", e.seedSeq)
	}
	if row.Anvil == "" {
		row.Anvil = testAnvil
	}
	if row.Status == "" {
		row.Status = state.WorkerRunning
	}
	if row.StartedAt.IsZero() {
		row.StartedAt = time.Now()
	}

	var completedAt any
	if !row.CompletedAt.IsZero() {
		completedAt = row.CompletedAt.Format(dbTimeFormat)
	}
	heartbeat := ""
	if !row.Heartbeat.IsZero() {
		heartbeat = row.Heartbeat.Format(dbTimeFormat)
	}

	_, err := e.conn().Exec(
		`INSERT OR REPLACE INTO workers
		   (id, bead_id, anvil, branch, pid, status, phase, title, pr_number,
		    started_at, completed_at, log_path, prev_status, session_id,
		    daemon_generation, heartbeat_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.BeadID, row.Anvil, row.Branch, row.PID, string(row.Status),
		row.Phase, row.Title, row.PRNumber,
		row.StartedAt.Format(dbTimeFormat), completedAt, row.LogPath,
		string(row.PrevStatus), row.SessionID,
		row.Generation, heartbeat,
	)
	require.NoError(e.t, err, "seeding worker row %s", row.ID)
	return row
}

// seedBead writes a bead into the queue cache — the daemon's own record of a
// bead, since beads themselves live in bd and not in state.db.
func (e *testEnv) seedBead(id, status string) {
	e.t.Helper()
	e.seedBeadInAnvil(id, testAnvil, status)
}

// seedBeadInAnvil is seedBead for a test running more than one anvil, which the
// (bead_id, anvil) primary key makes a different row rather than the same one.
func (e *testEnv) seedBeadInAnvil(id, anvil, status string) {
	e.t.Helper()
	_, err := e.conn().Exec(
		`INSERT OR REPLACE INTO queue_cache
		   (bead_id, anvil, title, priority, status, labels, section, updated_at)
		 VALUES (?, ?, ?, ?, ?, '[]', 'ready', ?)`,
		id, anvil, "seeded bead "+id, 2, status, time.Now().Format(dbTimeFormat),
	)
	require.NoError(e.t, err, "seeding bead %s", id)
}

// ---------------------------------------------------------------------------
// Polling assertions
// ---------------------------------------------------------------------------

// waitFor blocks until cond holds, or fails the test at defaultWaitTimeout.
//
// It is a tick against a hard deadline rather than a sleep because the two fail
// differently: a sleep sized for a fast host is flaky on a slow one and a sleep
// sized for a slow one makes every passing test pay for it, while this returns
// as soon as the condition holds and names what it was waiting for when it does
// not. cond is evaluated once before the first tick, so a condition that is
// already true costs nothing.
func (e *testEnv) waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	if e.waitUntil(cond, defaultWaitTimeout) {
		return
	}
	t.Fatalf("timed out after %s waiting for %s", defaultWaitTimeout, msg)
}

// waitUntil is waitFor without the assertion: it reports whether cond became
// true within timeout, for a caller that wants to say something of its own
// about the failure.
func (e *testEnv) waitUntil(cond func() bool, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = defaultWaitTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(waitTick)
	}
}

// waitForStatus blocks until the bead's worker row reaches want.
//
// A bead's state in state.db IS its worker row — the daemon records nothing
// else about work in flight — so this reads the most recent row for the bead,
// which is the one every transition under test moves. The failure names the
// last status actually observed, since "wanted failed" on its own does not say
// whether the row was still running, already done, or never written at all.
func (e *testEnv) waitForStatus(t *testing.T, beadID, want string, timeout time.Duration) {
	t.Helper()

	var last string
	ok := e.waitUntil(func() bool {
		got, err := e.beadWorkerStatus(beadID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			last = "<no worker row>"
		case err != nil:
			last = fmt.Sprintf("<query error: %v>", err)
		default:
			last = got
		}
		return last == want
	}, timeout)
	if ok {
		return
	}
	t.Fatalf("bead %s: waited %s for worker status %q, last observed %q",
		beadID, timeout, want, last)
}

// waitForWorkerStatus is waitForStatus addressed by worker row id, for a test
// holding several rows for one bead.
func (e *testEnv) waitForWorkerStatus(t *testing.T, workerID string, want state.WorkerStatus, timeout time.Duration) {
	t.Helper()

	var last state.WorkerStatus
	ok := e.waitUntil(func() bool {
		got, err := e.db.GetWorkerStatus(workerID)
		if err != nil {
			last = state.WorkerStatus(fmt.Sprintf("<query error: %v>", err))
			return false
		}
		last = got
		return got == want
	}, timeout)
	if ok {
		return
	}
	t.Fatalf("worker %s: waited %s for status %q, last observed %q",
		workerID, timeout, want, last)
}

// beadWorkerStatus returns the status of the most recent worker row for a bead,
// or sql.ErrNoRows when the bead has none. started_at is the fixed-width
// canonical layout, so ordering it lexicographically is ordering it in time;
// rowid breaks a tie between two rows written in the same instant.
func (e *testEnv) beadWorkerStatus(beadID string) (string, error) {
	var status string
	err := e.conn().QueryRow(
		`SELECT status FROM workers WHERE bead_id = ?
		 ORDER BY started_at DESC, rowid DESC LIMIT 1`, beadID,
	).Scan(&status)
	return status, err
}
