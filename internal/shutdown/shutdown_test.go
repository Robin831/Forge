package shutdown

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/worktree"
)

// openTestDB opens a fresh state.DB in a temp directory and registers cleanup.
func openTestDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// makeWorktreeDir creates a fake worktree directory under <anvil>/.workers/<name>
// containing a marker file, and returns its path.
func makeWorktreeDir(t *testing.T, anvil, name string) string {
	t.Helper()
	p := filepath.Join(anvil, ".workers", name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir worktree %q: %v", p, err)
	}
	if err := os.WriteFile(filepath.Join(p, "marker"), []byte("work"), 0o644); err != nil {
		t.Fatalf("write marker in %q: %v", p, err)
	}
	return p
}

// TestCleanupWorktreesRetainsPaused verifies the shutdown-retention contract:
// a worktree belonging to a paused (parked) worker is preserved across a full
// shutdown cleanup — treated like a drained pipeline whose state persists — while
// every non-paused worktree is removed. This is the load-bearing path that lets a
// bead paused before a daemon restart be resumed in place afterwards.
//
// The anvil is a plain temp dir (not a git repo); worktree.Manager.Remove falls
// back to os.RemoveAll when `git worktree remove` fails, which is exactly the
// removal we assert on here.
func TestCleanupWorktreesRetainsPaused(t *testing.T) {
	db := openTestDB(t)
	anvil := t.TempDir()

	pausedDir := makeWorktreeDir(t, anvil, "Forge-paused")
	// A second paused worker with a plain (non-forge/) branch, to prove the
	// forge/ prefix is stripped correctly when building the retention set.
	pausedPlainDir := makeWorktreeDir(t, anvil, "Forge-plain")
	goneDir := makeWorktreeDir(t, anvil, "Forge-gone")

	if err := db.InsertWorker(&state.Worker{
		ID: "w-paused", BeadID: "Forge-paused", Anvil: "anvil-1", Branch: "forge/Forge-paused",
		Status: state.WorkerPaused, Phase: "smith", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertWorker(&state.Worker{
		ID: "w-plain", BeadID: "Forge-plain", Anvil: "anvil-1", Branch: "Forge-plain",
		Status: state.WorkerPaused, Phase: "smith", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// A running worker must NOT contribute to the retention set — its worktree is
	// cleaned up like any other on shutdown.
	if err := db.InsertWorker(&state.Worker{
		ID: "w-gone", BeadID: "Forge-gone", Anvil: "anvil-1", Branch: "forge/Forge-gone",
		Status: state.WorkerRunning, Phase: "smith", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	m := NewManager(db, worktree.NewManager(), discardLogger(), map[string]string{"anvil-1": anvil})
	m.CleanupWorktrees()

	if _, err := os.Stat(pausedDir); err != nil {
		t.Errorf("paused worktree must be retained across shutdown, but it was removed: %v", err)
	}
	if _, err := os.Stat(pausedPlainDir); err != nil {
		t.Errorf("paused worktree with plain branch must be retained, but it was removed: %v", err)
	}
	if _, err := os.Stat(goneDir); !os.IsNotExist(err) {
		t.Errorf("non-paused worktree must be removed on shutdown, but it survived (stat err=%v)", err)
	}
}

// TestCleanupWorktreesRemovesAllWhenNonePaused verifies that with no paused
// workers the retention set is empty and every worktree is removed — the retain
// logic must not accidentally preserve worktrees when nothing is parked.
func TestCleanupWorktreesRemovesAllWhenNonePaused(t *testing.T) {
	db := openTestDB(t)
	anvil := t.TempDir()

	a := makeWorktreeDir(t, anvil, "Forge-a")
	b := makeWorktreeDir(t, anvil, "Forge-b")

	// Only a running worker exists; PausedWorkers() returns empty.
	if err := db.InsertWorker(&state.Worker{
		ID: "w-a", BeadID: "Forge-a", Anvil: "anvil-1", Branch: "forge/Forge-a",
		Status: state.WorkerRunning, Phase: "smith", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	m := NewManager(db, worktree.NewManager(), discardLogger(), map[string]string{"anvil-1": anvil})
	m.CleanupWorktrees()

	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("worktree %q must be removed when no workers are paused (stat err=%v)", p, err)
		}
	}
}

// TestCleanupWorktreesNilManager verifies CleanupWorktrees is a no-op (and does
// not panic) when no worktree manager is configured.
func TestCleanupWorktreesNilManager(t *testing.T) {
	m := NewManager(openTestDB(t), nil, discardLogger(), map[string]string{"anvil-1": t.TempDir()})
	m.CleanupWorktrees() // must not panic
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ownedPIDs is a test helper that returns the set of PIDs identified as
// Forge-owned by identifyForgeOwnedProcesses, exercising the cwd-required (Unix)
// verification path.
func ownedPIDs(workers []state.Worker, procs []procInfo, roots []string) map[int]bool {
	return ownedPIDsWithCwd(workers, procs, roots, true)
}

// ownedPIDsWithCwd lets a test choose whether the cwd-in-worktree check is
// required, mirroring the platformSupportsProcessCwd flag (true on Unix, false
// on Windows).
func ownedPIDsWithCwd(workers []state.Worker, procs []procInfo, roots []string, requireWorktreeCwd bool) map[int]bool {
	owned := identifyForgeOwnedProcesses(workers, procs, roots, requireWorktreeCwd, discardLogger())
	set := make(map[int]bool, len(owned))
	for _, op := range owned {
		set[op.pid] = true
	}
	return set
}

// TestIdentifyForgeOwnedProcessesNoCwd covers the Windows verification path,
// where process working directories are unavailable (requireWorktreeCwd=false).
// Ownership rests solely on a PID + creation-time match against a recorded
// worker row; the unrecorded-PID secondary sweep is disabled so a stray process
// whose exe basename is "claude.exe" is never reaped on that basis alone.
func TestIdentifyForgeOwnedProcessesNoCwd(t *testing.T) {
	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	workers := []state.Worker{
		// Tracked worker: PID 1001 recorded, spawned ~5s after the row.
		{ID: "w-tracked", BeadID: "Forge-abc1", PID: 1001, StartedAt: base},
		// A worker row whose PID was recycled by an unrelated process.
		{ID: "w-recycled", BeadID: "Forge-def2", PID: 1002, StartedAt: base},
	}

	procs := []procInfo{
		// Tracked worker: PID + creation time match the row. No cwd available.
		{pid: 1001, argv: []string{"claude.exe"}, startTime: base.Add(5 * time.Second)},
		// Recycled PID: same PID as w-recycled but started 3h later — inconsistent.
		{pid: 1002, argv: []string{"deploy.exe"}, startTime: base.Add(3 * time.Hour)},
		// Operator's own Claude session: exe basename matches a worker binary but
		// its PID was never recorded. Without a cwd check, it must NOT be reaped.
		{pid: 2001, argv: []string{"claude.exe"}, startTime: base.Add(-time.Hour)},
	}

	got := ownedPIDsWithCwd(workers, procs, nil, false)

	if !got[1001] {
		t.Errorf("tracked worker PID 1001 should be reaped via PID + creation-time match")
	}
	for _, pid := range []int{1002, 2001} {
		if got[pid] {
			t.Errorf("PID %d must NOT be reaped on Windows (recycled PID / unrecorded operator session)", pid)
		}
	}
	if len(got) != 1 {
		t.Errorf("identified %d processes, want 1: %v", len(got), got)
	}
}

// TestIdentifyForgeOwnedProcesses covers the four canonical scenarios from the
// bead: operator session spared, tracked worker reaped, recycled PID spared,
// and a lost worker inside a worktree reaped.
func TestIdentifyForgeOwnedProcesses(t *testing.T) {
	anvil := filepath.FromSlash("/home/robin/source/Forge")
	root := filepath.Join(anvil, ".workers")
	worktree := filepath.Join(root, "Forge-abc1")

	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	roots := []string{root}

	workers := []state.Worker{
		// Tracked worker: PID 1001 recorded, spawned ~5s after the row.
		{ID: "w-tracked", BeadID: "Forge-abc1", PID: 1001, StartedAt: base},
		// A worker row whose PID was recycled by an unrelated process.
		{ID: "w-recycled", BeadID: "Forge-def2", PID: 1002, StartedAt: base},
	}

	procs := []procInfo{
		// Tracked worker's live claude process, cwd inside the worktree.
		{pid: 1001, pgid: 1001, argv: []string{"claude", "--flag"}, startTime: base.Add(5 * time.Second), cwd: worktree},
		// Recycled PID: same PID as w-recycled but started 3h later, running an
		// unrelated program outside any worktree.
		{pid: 1002, pgid: 1002, argv: []string{"/usr/bin/bash", "deploy.sh"}, startTime: base.Add(3 * time.Hour), cwd: filepath.FromSlash("/home/robin")},
		// Operator's own Claude Code session — argv matches "claude" but cwd is
		// outside every worktree.
		{pid: 2001, pgid: 2001, argv: []string{filepath.FromSlash("/home/robin/.local/share/claude/versions/1.2.3/claude"), "daemon"}, startTime: base.Add(-time.Hour), cwd: filepath.FromSlash("/home/robin")},
		// Operator's helper process (claude bg-pty-host) whose group leader is
		// the operator session (also outside worktrees).
		{pid: 2002, pgid: 2001, argv: []string{"claude", "bg-pty-host"}, startTime: base.Add(-time.Hour), cwd: filepath.FromSlash("/home/robin")},
		// Unrelated shell script whose argv contains a ".claude" path but whose
		// basename is not a worker binary, running outside any worktree.
		{pid: 2003, pgid: 2003, argv: []string{"bash", filepath.FromSlash("/home/robin/.claude/jobs/x/tmp/deploy.sh")}, startTime: base, cwd: filepath.FromSlash("/home/robin/.claude/jobs/x")},
		// Genuinely lost worker: PID never recorded, but argv[0] basename is
		// "claude" and cwd is inside a worktree.
		{pid: 3001, pgid: 3001, argv: []string{"claude"}, startTime: base, cwd: filepath.Join(root, "Forge-lost9")},
	}

	got := ownedPIDs(workers, procs, roots)

	want := map[int]bool{
		1001: true, // tracked worker
		3001: true, // lost worker inside worktree
	}

	for pid := range want {
		if !got[pid] {
			t.Errorf("expected PID %d to be identified as Forge-owned, but it was not", pid)
		}
	}
	spared := []int{1002, 2001, 2002, 2003}
	for _, pid := range spared {
		if got[pid] {
			t.Errorf("PID %d must NOT be killed but was identified as Forge-owned", pid)
		}
	}
	if len(got) != len(want) {
		t.Errorf("identified %d processes, want %d: %v", len(got), len(want), got)
	}
}

// TestPrimarySourceRequiresWorktreeCwd verifies that a PID matching a worker row
// with a consistent start time is still spared when it is not running inside a
// worktree — ownership cannot be confirmed from the PID alone.
func TestPrimarySourceRequiresWorktreeCwd(t *testing.T) {
	root := filepath.FromSlash("/anvil/.workers")
	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	workers := []state.Worker{{ID: "w1", BeadID: "B-1", PID: 500, StartedAt: base}}
	procs := []procInfo{
		{pid: 500, pgid: 500, argv: []string{"claude"}, startTime: base.Add(time.Second), cwd: filepath.FromSlash("/home/robin")},
	}

	if got := ownedPIDs(workers, procs, []string{root}); got[500] {
		t.Errorf("PID 500 matched a worker row but runs outside any worktree; it must not be reaped")
	}
}

// TestPgidLeaderCwdFallback verifies a child process (cwd outside a worktree)
// is reaped when its process-group leader is inside a worktree.
func TestPgidLeaderCwdFallback(t *testing.T) {
	root := filepath.FromSlash("/anvil/.workers")
	worktree := filepath.Join(root, "Forge-xyz")
	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	procs := []procInfo{
		// Group leader claude, inside the worktree.
		{pid: 700, pgid: 700, argv: []string{"claude"}, startTime: base, cwd: worktree},
		// Child process that chdir'd elsewhere but shares the group.
		{pid: 701, pgid: 700, argv: []string{"claude", "agents"}, startTime: base, cwd: filepath.FromSlash("/tmp")},
	}

	got := ownedPIDs(nil, procs, []string{root})
	if !got[700] || !got[701] {
		t.Errorf("expected both group leader (700) and child (701) to be reaped, got %v", got)
	}
}

func TestStartTimeConsistent(t *testing.T) {
	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		procStart   time.Time
		workerStart time.Time
		want        bool
	}{
		{"exact", base, base, true},
		{"shortly after", base.Add(10 * time.Second), base, true},
		{"within spawn delay", base.Add(20 * time.Minute), base, true},
		{"small negative skew", base.Add(-30 * time.Second), base, true},
		{"recycled newer", base.Add(2 * time.Hour), base, false},
		{"started well before", base.Add(-10 * time.Minute), base, false},
		{"zero worker start", base, time.Time{}, false},
		{"zero proc start", time.Time{}, base, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := startTimeConsistent(tc.procStart, tc.workerStart); got != tc.want {
				t.Errorf("startTimeConsistent = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPathUnderAny(t *testing.T) {
	root := filepath.FromSlash("/anvil/.workers")
	roots := []string{root}
	cases := []struct {
		name string
		dir  string
		want bool
	}{
		{"exact root", root, true},
		{"nested", filepath.Join(root, "Forge-abc"), true},
		{"deep nested", filepath.Join(root, "Forge-abc", "sub"), true},
		{"deleted suffix", filepath.Join(root, "Forge-abc") + " (deleted)", true},
		{"outside", filepath.FromSlash("/home/robin"), false},
		{"sibling prefix trap", filepath.FromSlash("/anvil/.workers-other/x"), false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := pathUnderAny(tc.dir, roots)
			if got != tc.want {
				t.Errorf("pathUnderAny(%q) = %v, want %v", tc.dir, got, tc.want)
			}
		})
	}
}

func TestArgvBasename(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"claude"}, "claude"},
		{[]string{"/usr/local/bin/claude", "--flag"}, "claude"},
		{[]string{filepath.FromSlash("/home/robin/.local/share/claude/versions/1/claude")}, "claude"},
		{[]string{`C:\tools\claude.exe`}, "claude.exe"},
		{[]string{"bash", "/home/robin/.claude/x.sh"}, "bash"},
		{nil, ""},
		{[]string{""}, ""},
	}
	for _, tc := range cases {
		if got := argvBasename(tc.argv); got != tc.want {
			t.Errorf("argvBasename(%v) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

// TestListProcessesInjectable verifies findOrphanedClaude uses the injectable
// process lister and honors the worktree-ownership contract end-to-end.
func TestListProcessesInjectable(t *testing.T) {
	anvil := t.TempDir()
	root := filepath.Join(anvil, ".workers")
	base := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	orig := listProcesses
	t.Cleanup(func() { listProcesses = orig })
	listProcesses = func() ([]procInfo, error) {
		return []procInfo{
			{pid: 4001, pgid: 4001, argv: []string{"claude"}, startTime: base, cwd: filepath.Join(root, "Forge-a")},
			{pid: 4002, pgid: 4002, argv: []string{"claude"}, startTime: base, cwd: filepath.FromSlash("/home/robin")},
		}, nil
	}

	m := &Manager{
		logger: discardLogger(),
		anvils: map[string]string{"test": anvil},
	}

	owned := m.findOrphanedClaude(nil)
	if len(owned) != 1 || owned[0].pid != 4001 {
		t.Fatalf("expected only PID 4001 to be reaped, got %+v", owned)
	}
	if owned[0].evidence == "" {
		t.Errorf("expected non-empty evidence for reaped process")
	}
}
