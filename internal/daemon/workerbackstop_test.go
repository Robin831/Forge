package daemon

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/crucible"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/state"
)

func backstopDaemon(t *testing.T) *Daemon {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &Daemon{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func insertBackstopWorker(t *testing.T, d *Daemon, id string, status state.WorkerStatus) {
	t.Helper()
	require.NoError(t, d.db.InsertWorker(&state.Worker{
		ID:        id,
		BeadID:    "Forge-52bp",
		Anvil:     "repo",
		Status:    status,
		Phase:     "smith",
		StartedAt: time.Now(),
	}))
}

func backstopStatus(t *testing.T, d *Daemon, id string) state.WorkerStatus {
	t.Helper()
	got, err := d.db.GetWorkerStatus(id)
	require.NoError(t, err)
	return got
}

// The backstop's whole job: a row still claiming live work when its dispatch
// exits is failed, and a row an ordinary path already finalised — or one
// deliberately handed off live to Bellows (monitoring/detached) or to a cold
// resume (paused) — is left exactly as it stands.
//
// The exempt half is what makes the defer safe to install unconditionally: the
// success path returns from finalizePipeline with the row on 'monitoring', so a
// backstop that read only "not terminal" would report every successful dispatch
// as a failure and take the PR off Bellows' radar.
func TestTerminateAbandonedWorker(t *testing.T) {
	cases := []struct {
		status     state.WorkerStatus
		wantFailed bool
	}{
		{state.WorkerPending, true},
		{state.WorkerRunning, true},
		{state.WorkerReviewing, true},
		{state.WorkerStalled, true},
		{state.WorkerDone, false},
		{state.WorkerFailed, false},
		{state.WorkerPartial, false},
		{state.WorkerTimeout, false},
		{state.WorkerKilled, false},
		{state.WorkerMonitoring, false},
		{state.WorkerDetached, false},
		{state.WorkerPaused, false},
	}

	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			d := backstopDaemon(t)
			insertBackstopWorker(t, d, "w-1", tc.status)

			d.terminateAbandonedWorker("w-1", "Forge-52bp", "repo")

			want := tc.status
			if tc.wantFailed {
				want = state.WorkerFailed
			}
			assert.Equal(t, want, backstopStatus(t, d, "w-1"))
		})
	}
}

// 'stalled' is a mask the watchdog writes over whatever the row held, so the
// backstop decides it on prev_status. A pipeline sits on 'monitoring' through
// the push and the PR creation that follow warden approval, neither of which
// writes a log line, so the watchdog can stall a row Bellows already owns —
// failing that row would assert an ending over a live handoff and destroy the
// recovery UnstallWorker performs. Stalled over a dispatch status is the
// opposite: nothing will move that row again, and left alone it holds a
// dispatch slot forever.
func TestTerminateAbandonedWorkerReadsTheStatusUnderAStall(t *testing.T) {
	for _, tc := range []struct {
		prev       state.WorkerStatus
		wantFailed bool
	}{
		{state.WorkerMonitoring, false},
		{state.WorkerRunning, true},
		{state.WorkerReviewing, true},
		{state.WorkerPending, true},
	} {
		t.Run(string(tc.prev), func(t *testing.T) {
			d := backstopDaemon(t)
			insertBackstopWorker(t, d, "w-1", tc.prev)
			require.NoError(t, d.db.MarkWorkerStalled("w-1"))

			d.terminateAbandonedWorker("w-1", "Forge-52bp", "repo")

			want := state.WorkerStalled
			if tc.wantFailed {
				want = state.WorkerFailed
			}
			assert.Equal(t, want, backstopStatus(t, d, "w-1"))

			if !tc.wantFailed {
				// The row must still be recoverable, and the run must not have
				// been announced as an abandoned one.
				require.NoError(t, d.db.UnstallWorker("w-1"))
				assert.Equal(t, tc.prev, backstopStatus(t, d, "w-1"))

				events, err := d.db.RecentEvents(20)
				require.NoError(t, err)
				for _, e := range events {
					assert.NotEqual(t, state.EventWorkerAbandoned, e.Type,
						"the backstop announced a row the watchdog had merely masked")
				}
			}
		})
	}
}

// A backstop that fires is a gap in the finalisation, not an outcome of the
// work, so it says so in the activity feed under its own event type — naming
// the status it replaced, which is the only clue to which exit path was missing
// its update.
func TestTerminateAbandonedWorkerRecordsTheGap(t *testing.T) {
	d := backstopDaemon(t)
	insertBackstopWorker(t, d, "w-1", state.WorkerRunning)

	d.terminateAbandonedWorker("w-1", "Forge-52bp", "repo")

	events, err := d.db.RecentEvents(20)
	require.NoError(t, err)
	var found *state.Event
	for i := range events {
		if events[i].Type == state.EventWorkerAbandoned {
			found = &events[i]
			break
		}
	}
	require.NotNil(t, found, "no worker_abandoned event was recorded")
	assert.Equal(t, "Forge-52bp", found.BeadID)
	assert.Contains(t, found.Message, "w-1")
	assert.Contains(t, found.Message, string(state.WorkerRunning))

	// A row that was already terminal is not news.
	d2 := backstopDaemon(t)
	insertBackstopWorker(t, d2, "w-2", state.WorkerDone)
	d2.terminateAbandonedWorker("w-2", "Forge-52bp", "repo")
	quiet, err := d2.db.RecentEvents(20)
	require.NoError(t, err)
	for _, e := range quiet {
		assert.NotEqual(t, state.EventWorkerAbandoned, e.Type,
			"the backstop announced a row it did not touch")
	}
}

// Called twice — as it is whenever an explicit termination and the defer both
// run — the second call is a no-op rather than a second event.
func TestTerminateAbandonedWorkerIsIdempotent(t *testing.T) {
	d := backstopDaemon(t)
	insertBackstopWorker(t, d, "w-1", state.WorkerRunning)

	d.terminateAbandonedWorker("w-1", "Forge-52bp", "repo")
	d.terminateAbandonedWorker("w-1", "Forge-52bp", "repo")

	assert.Equal(t, state.WorkerFailed, backstopStatus(t, d, "w-1"))

	events, err := d.db.RecentEvents(20)
	require.NoError(t, err)
	count := 0
	for _, e := range events {
		if e.Type == state.EventWorkerAbandoned {
			count++
		}
	}
	assert.Equal(t, 1, count, "the second call announced the same gap again")
}

// An empty worker ID (no claim row was ever inserted) and a row the bellows
// sweep has already deleted are both nothing to finalise. Neither may create a
// row or raise an event.
func TestTerminateAbandonedWorkerToleratesAMissingRow(t *testing.T) {
	d := backstopDaemon(t)

	d.terminateAbandonedWorker("", "Forge-52bp", "repo")
	d.terminateAbandonedWorker("gone", "Forge-52bp", "repo")

	events, err := d.db.RecentEvents(20)
	require.NoError(t, err)
	assert.Empty(t, events)

	_, err = d.db.GetWorkerStatus("gone")
	assert.ErrorIs(t, err, state.ErrWorkerNotFound)
}

// Installed as a defer it also covers the exit no explicit update can: a panic
// unwinding the dispatch goroutine.
func TestTerminateAbandonedWorkerRunsOnAPanicUnwind(t *testing.T) {
	d := backstopDaemon(t)
	insertBackstopWorker(t, d, "w-1", state.WorkerRunning)

	func() {
		defer func() { _ = recover() }()
		defer d.terminateAbandonedWorker("w-1", "Forge-52bp", "repo")
		panic("smith exploded mid-pipeline")
	}()

	assert.Equal(t, state.WorkerFailed, backstopStatus(t, d, "w-1"))
}

// dispatchBead has roughly two dozen exits — early aborts, gotos, the pipeline
// error handler, the success path — and cannot be driven end to end from a test
// (it spawns claude in a real worktree). What CAN be pinned is the one property
// that makes the backstop cover all of them: the defer is registered before the
// function's first exit, so no return, goto or panic can precede it.
func TestDispatchBeadRegistersTheBackstopBeforeItsFirstExit(t *testing.T) {
	src, err := os.ReadFile("daemon.go")
	require.NoError(t, err)

	const signature = "func (d *Daemon) dispatchBead("
	start := strings.Index(string(src), signature)
	require.GreaterOrEqual(t, start, 0, "dispatchBead not found — has it been renamed?")
	body := string(src)[start:]

	deferAt := strings.Index(body, "defer d.terminateAbandonedWorker(claimWorkerID")
	require.GreaterOrEqual(t, deferAt, 0,
		"dispatchBead no longer installs the abandoned-worker backstop; every exit it has can leak a worker row claiming a dead Smith")

	for _, exit := range []string{"\n\t\treturn", "\n\t\tgoto ", "\n\treturn"} {
		if at := strings.Index(body, exit); at >= 0 {
			assert.Less(t, deferAt, at,
				"the backstop defer is registered after an exit (%q) in dispatchBead", strings.TrimSpace(exit))
		}
	}
}

// The Crucible's own exits finalise the parent claim row rather than leaving it
// to the backstop. dispatchBead sets that row 'running' before crucible.Run and
// the Crucible never moves it, so on the SUCCESS path the backstop would have
// recorded a completed epic as 'failed' — and, worse, emitted worker_abandoned
// for it, which is an event about a missing finalisation and not about work.
func TestFinalizeCrucibleWorker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status state.WorkerStatus
	}{
		{"crucible completed", state.WorkerDone},
		{"crucible failed or paused", state.WorkerFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := backstopDaemon(t)
			insertBackstopWorker(t, d, "w-1", state.WorkerRunning)

			bead := poller.Bead{ID: "Forge-52bp", Anvil: "repo"}
			d.finalizeCrucibleWorker("w-1", bead, tc.status)
			assert.Equal(t, tc.status, backstopStatus(t, d, "w-1"))

			// The dispatch defer still runs after it: the row is terminal, so
			// the backstop leaves it alone and raises nothing.
			d.terminateAbandonedWorker("w-1", bead.ID, bead.Anvil)
			assert.Equal(t, tc.status, backstopStatus(t, d, "w-1"))

			events, err := d.db.RecentEvents(10)
			require.NoError(t, err)
			for _, e := range events {
				assert.NotEqual(t, state.EventWorkerAbandoned, e.Type,
					"a Crucible exit that finalises its own row must not also be reported as abandoned")
			}
		})
	}

	t.Run("no claim row", func(t *testing.T) {
		d := backstopDaemon(t)
		d.finalizeCrucibleWorker("", poller.Bead{ID: "Forge-52bp", Anvil: "repo"}, state.WorkerDone)
	})
}

// Every exit of dispatchBead's crucible block must reach that finaliser. The
// block cannot be driven from a test (crucible.Run spawns pipelines against a
// real anvil), but which exits terminate the row is a source-level property and
// the one that decides whether a normal epic emits worker_abandoned.
//
// It is asserted structurally rather than by looking for the calls: the block
// had a finalisation on each of two branches and a third, unbranched exit that
// reached neither — a Result carrying no error and not claiming success — so a
// guard that asks whether the calls are PRESENT is exactly the shape that
// cannot see the exit which has none. What is pinned instead is that the block
// finalises the row ONCE, unconditionally, above every exit it has: any return
// added below it is covered by construction, and any attempt to move the call
// back under a branch fails here.
func TestCrucibleBlockFinalisesItsWorkerRowOnEveryExit(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "daemon.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err)

	block, runAt := crucibleRunBlock(t, file)
	require.NotNil(t, block, "the crucible dispatch call was not found — has it moved?")

	// The finalisation must be a statement of the block itself, not of a branch
	// inside it: a call nested under an `if` finalises only the exits that if
	// governs, which is the bug.
	finalizeAt := -1
	for i := runAt + 1; i < len(block.List); i++ {
		if callsFinalizeCrucibleWorker(block.List[i]) {
			finalizeAt = i
			break
		}
	}
	require.GreaterOrEqual(t, finalizeAt, 0,
		"the crucible block does not finalise its parent claim row unconditionally after crucible.Run; "+
			"left running, the dispatch backstop reports an ordinary epic as abandoned")

	// Everything the block does after crucible.Run — every return, every branch
	// that ends in one — must sit below that call.
	finalizePos := block.List[finalizeAt].Pos()
	for i := runAt; i < len(block.List); i++ {
		stmt := block.List[i]
		ast.Inspect(stmt, func(n ast.Node) bool {
			ret, ok := n.(*ast.ReturnStmt)
			if !ok {
				return true
			}
			assert.Greater(t, ret.Pos(), finalizePos,
				"an exit of the crucible block at %s is reached without finalising the parent claim row",
				fset.Position(ret.Pos()))
			return true
		})
	}
}

// crucibleRunBlock returns the statement list directly containing
// `result := crucible.Run(...)` inside dispatchBead, and that statement's index
// in it.
func crucibleRunBlock(t *testing.T, file *ast.File) (*ast.BlockStmt, int) {
	t.Helper()
	var found *ast.BlockStmt
	var at int
	ast.Inspect(file, func(n ast.Node) bool {
		block, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}
		for i, stmt := range block.List {
			assign, ok := stmt.(*ast.AssignStmt)
			if !ok || len(assign.Rhs) != 1 {
				continue
			}
			call, ok := assign.Rhs[0].(*ast.CallExpr)
			if !ok {
				continue
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "crucible" && sel.Sel.Name == "Run" {
					found, at = block, i
					return false
				}
			}
		}
		return true
	})
	return found, at
}

func callsFinalizeCrucibleWorker(stmt ast.Stmt) bool {
	expr, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "finalizeCrucibleWorker"
}

// The status that unconditional call passes is the only part of the decision
// that ever depended on which branch the block took, so it is derived from the
// result — including for the shape neither branch describes.
func TestCrucibleWorkerStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *crucible.Result
		want   state.WorkerStatus
	}{
		{"success", &crucible.Result{Success: true}, state.WorkerDone},
		{"error", &crucible.Result{Error: errors.New("boom")}, state.WorkerFailed},
		{"paused on a child", &crucible.Result{Error: errors.New("boom"), PausedChildID: "Forge-child"}, state.WorkerFailed},
		// Neither an error nor a success: no epic reached its final PR, so the
		// row did not succeed. Nothing returns this today, which is precisely
		// why the branches below the call must not be what decides it.
		{"neither", &crucible.Result{}, state.WorkerFailed},
		// A success flag that arrives with an error is not a success.
		{"success with an error", &crucible.Result{Success: true, Error: errors.New("boom")}, state.WorkerFailed},
		{"no result at all", nil, state.WorkerFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, crucibleWorkerStatus(tc.result))
		})
	}
}
