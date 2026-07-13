package shutdown

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ownedPIDs is a test helper that returns the set of PIDs identified as
// Forge-owned by identifyForgeOwnedProcesses.
func ownedPIDs(workers []state.Worker, procs []procInfo, roots []string) map[int]bool {
	owned := identifyForgeOwnedProcesses(workers, procs, roots, discardLogger())
	set := make(map[int]bool, len(owned))
	for _, op := range owned {
		set[op.pid] = true
	}
	return set
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

func TestParseStat(t *testing.T) {
	// comm contains spaces and parentheses to exercise the last-')' logic.
	// Fields after comm: state ppid pgrp(=5) ... starttime(=22).
	// index within post-')' fields: pgrp=2, starttime=19.
	line := "1234 (weird )name) S 1 4321 4321 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 987654 0 0"
	pgid, start := parseStat([]byte(line))
	if pgid != 4321 {
		t.Errorf("pgid = %d, want 4321", pgid)
	}
	if start != 987654 {
		t.Errorf("starttime = %d, want 987654", start)
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
