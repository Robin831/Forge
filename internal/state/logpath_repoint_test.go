package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newRepointTestDB opens a fresh temp-backed state DB for the log-path tests.
func newRepointTestDB(t *testing.T) *DB {
	t.Helper()
	tmpDir := t.TempDir()
	db, err := Open(filepath.Join(tmpDir, "state.db"))
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func getLogPath(t *testing.T, db *DB, id string) string {
	t.Helper()
	w, err := db.GetWorker(id)
	if err != nil {
		t.Fatalf("GetWorker(%s): %v", id, err)
	}
	return w.LogPath
}

func TestRepointWorkerLogPaths_BasenameMatch(t *testing.T) {
	db := newRepointTestDB(t)

	oldLogDir := filepath.Join("/anvil", ".workers", "BD-1", ".forge-logs")
	newDir := filepath.Join("/home/user", ".forge", "logs", "BD-1")

	// Worker whose log lives under the worktree .forge-logs — should be repointed.
	if err := db.InsertWorker(&Worker{
		ID:        "w-1",
		BeadID:    "BD-1",
		Anvil:     "anvil",
		Status:    WorkerDone,
		LogPath:   filepath.Join(oldLogDir, "1700000000.log"),
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// Worker for a different bead — must not be touched.
	otherPath := filepath.Join("/anvil", ".workers", "BD-2", ".forge-logs", "9.log")
	if err := db.InsertWorker(&Worker{
		ID:        "w-other",
		BeadID:    "BD-2",
		Anvil:     "anvil",
		Status:    WorkerDone,
		LogPath:   otherPath,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// Worker for BD-1 whose path is NOT under oldLogDir — must not be touched.
	elsewhere := filepath.Join("/home/user", ".forge", "logs", "BD-1", "old.log")
	if err := db.InsertWorker(&Worker{
		ID:        "w-elsewhere",
		BeadID:    "BD-1",
		Anvil:     "anvil",
		Status:    WorkerDone,
		LogPath:   elsewhere,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	n, err := db.RepointWorkerLogPaths("BD-1", oldLogDir, newDir)
	if err != nil {
		t.Fatalf("RepointWorkerLogPaths: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 repoint, got %d", n)
	}

	if got, want := getLogPath(t, db, "w-1"), filepath.Join(newDir, "1700000000.log"); got != want {
		t.Errorf("w-1 log_path = %q, want %q", got, want)
	}
	if got := getLogPath(t, db, "w-other"); got != otherPath {
		t.Errorf("w-other log_path changed to %q, want %q", got, otherPath)
	}
	if got := getLogPath(t, db, "w-elsewhere"); got != elsewhere {
		t.Errorf("w-elsewhere log_path changed to %q, want %q", got, elsewhere)
	}
}

func TestRepointWorkerLogPaths_MissingDestinationFileStillRepoints(t *testing.T) {
	// The repoint mapping is purely by basename and does not require the
	// destination file to exist on disk (preserveWorktreeLogs is best-effort
	// per-file). A row under oldLogDir is repointed regardless.
	db := newRepointTestDB(t)

	oldLogDir := filepath.Join("/anvil", ".workers", "BD-9", ".forge-logs")
	newDir := filepath.Join("/tmp", "does-not-exist", "BD-9")

	if err := db.InsertWorker(&Worker{
		ID:        "w-9",
		BeadID:    "BD-9",
		Anvil:     "anvil",
		Status:    WorkerDone,
		LogPath:   filepath.Join(oldLogDir, "42.log"),
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	n, err := db.RepointWorkerLogPaths("BD-9", oldLogDir, newDir)
	if err != nil {
		t.Fatalf("RepointWorkerLogPaths: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 repoint, got %d", n)
	}
	if got, want := getLogPath(t, db, "w-9"), filepath.Join(newDir, "42.log"); got != want {
		t.Errorf("w-9 log_path = %q, want %q", got, want)
	}
}

func TestRepointWorkerLogPaths_Collision(t *testing.T) {
	db := newRepointTestDB(t)

	oldLogDir := filepath.Join("/anvil", ".workers", "BD-5", ".forge-logs")
	newDir := filepath.Join("/home/user", ".forge", "logs", "BD-5")

	// Two workers whose logs share the same basename under oldLogDir. Only the
	// first should claim the target; the second is left untouched.
	firstPath := filepath.Join(oldLogDir, "dup.log")
	secondPath := filepath.Join(oldLogDir, "dup.log")
	if err := db.InsertWorker(&Worker{
		ID:        "w-first",
		BeadID:    "BD-5",
		Anvil:     "anvil",
		Status:    WorkerDone,
		LogPath:   firstPath,
		StartedAt: time.Now().Add(-2 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertWorker(&Worker{
		ID:        "w-second",
		BeadID:    "BD-5",
		Anvil:     "anvil",
		Status:    WorkerDone,
		LogPath:   secondPath,
		StartedAt: time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	n, err := db.RepointWorkerLogPaths("BD-5", oldLogDir, newDir)
	if err != nil {
		t.Fatalf("RepointWorkerLogPaths: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 repoint on collision, got %d", n)
	}

	// Exactly one row wins the basename and is repointed; the other keeps its
	// original (dangling) path. Query order is not guaranteed, so assert on the
	// pair rather than a specific worker ID.
	repointedTarget := filepath.Join(newDir, "dup.log")
	first := getLogPath(t, db, "w-first")
	second := getLogPath(t, db, "w-second")
	newCount := 0
	oldCount := 0
	for _, p := range []string{first, second} {
		switch p {
		case repointedTarget:
			newCount++
		case firstPath: // firstPath == secondPath (same dangling path)
			oldCount++
		default:
			t.Errorf("unexpected log_path %q", p)
		}
	}
	if newCount != 1 || oldCount != 1 {
		t.Errorf("collision resolution wrong: got %d repointed and %d unchanged (first=%q second=%q)", newCount, oldCount, first, second)
	}
}

func TestNullWorkerLogPathsUnder_BoundaryMatching(t *testing.T) {
	db := newRepointTestDB(t)

	bdDir := filepath.Join("/home/user", ".forge", "logs", "BD-1")
	bdExtraDir := filepath.Join("/home/user", ".forge", "logs", "BD-1-extra")
	bdOtherDir := filepath.Join("/home/user", ".forge", "logs", "BD-2")

	// Worker under BD-1 — should be nulled.
	if err := db.InsertWorker(&Worker{
		ID:        "w-bd1",
		BeadID:    "BD-1",
		Anvil:     "anvil",
		Status:    WorkerDone,
		LogPath:   filepath.Join(bdDir, "smith.log"),
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// Worker under BD-1-extra (sibling prefix) — must NOT be nulled.
	if err := db.InsertWorker(&Worker{
		ID:        "w-bd1extra",
		BeadID:    "BD-1-extra",
		Anvil:     "anvil",
		Status:    WorkerDone,
		LogPath:   filepath.Join(bdExtraDir, "smith.log"),
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// Worker under BD-2 — must NOT be nulled.
	if err := db.InsertWorker(&Worker{
		ID:        "w-bd2",
		BeadID:    "BD-2",
		Anvil:     "anvil",
		Status:    WorkerDone,
		LogPath:   filepath.Join(bdOtherDir, "smith.log"),
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// Worker with empty log_path — must NOT be affected.
	if err := db.InsertWorker(&Worker{
		ID:        "w-empty",
		BeadID:    "BD-1",
		Anvil:     "anvil",
		Status:    WorkerDone,
		LogPath:   "",
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	n, err := db.NullWorkerLogPathsUnder(bdDir)
	if err != nil {
		t.Fatalf("NullWorkerLogPathsUnder: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 row nulled, got %d", n)
	}

	// BD-1 worker should now have empty log_path.
	if got := getLogPath(t, db, "w-bd1"); got != "" {
		t.Errorf("w-bd1 log_path = %q, want empty", got)
	}
	// BD-1-extra must be untouched.
	if got := getLogPath(t, db, "w-bd1extra"); got != filepath.Join(bdExtraDir, "smith.log") {
		t.Errorf("w-bd1extra log_path = %q, want %q", got, filepath.Join(bdExtraDir, "smith.log"))
	}
	// BD-2 must be untouched.
	if got := getLogPath(t, db, "w-bd2"); got != filepath.Join(bdOtherDir, "smith.log") {
		t.Errorf("w-bd2 log_path = %q, want %q", got, filepath.Join(bdOtherDir, "smith.log"))
	}
}

func TestNullWorkerLogPathsUnder_EmptyDir(t *testing.T) {
	db := newRepointTestDB(t)

	n, err := db.NullWorkerLogPathsUnder("")
	if err != nil {
		t.Fatalf("NullWorkerLogPathsUnder(empty): %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 for empty dir, got %d", n)
	}
}

func TestBackfillDanglingWorkerLogPaths(t *testing.T) {
	db := newRepointTestDB(t)

	logsRoot := t.TempDir()

	// Row 1: dangling path with a preserved copy present → repaired.
	danglingSrc := filepath.Join("/anvil", ".workers", "BD-A", ".forge-logs", "100.log")
	preservedA := filepath.Join(logsRoot, "BD-A", "100.log")
	if err := os.MkdirAll(filepath.Dir(preservedA), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preservedA, []byte("preserved"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertWorker(&Worker{
		ID:        "w-a",
		BeadID:    "BD-A",
		Anvil:     "anvil",
		Status:    WorkerDone,
		LogPath:   danglingSrc,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Row 2: log_path still resolves to an existing file → left untouched.
	existing := filepath.Join(logsRoot, "existing.log")
	if err := os.WriteFile(existing, []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertWorker(&Worker{
		ID:        "w-b",
		BeadID:    "BD-B",
		Anvil:     "anvil",
		Status:    WorkerDone,
		LogPath:   existing,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Row 3: dangling path with NO preserved copy → left untouched.
	danglingNoCopy := filepath.Join("/anvil", ".workers", "BD-C", ".forge-logs", "200.log")
	if err := db.InsertWorker(&Worker{
		ID:        "w-c",
		BeadID:    "BD-C",
		Anvil:     "anvil",
		Status:    WorkerDone,
		LogPath:   danglingNoCopy,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	n, err := db.BackfillDanglingWorkerLogPaths(logsRoot)
	if err != nil {
		t.Fatalf("BackfillDanglingWorkerLogPaths: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 repair, got %d", n)
	}

	if got := getLogPath(t, db, "w-a"); got != preservedA {
		t.Errorf("w-a log_path = %q, want %q", got, preservedA)
	}
	if got := getLogPath(t, db, "w-b"); got != existing {
		t.Errorf("w-b log_path changed to %q, want %q", got, existing)
	}
	if got := getLogPath(t, db, "w-c"); got != danglingNoCopy {
		t.Errorf("w-c log_path changed to %q, want %q", got, danglingNoCopy)
	}

	// A second run must be a no-op now that the only dangling row was repaired.
	n2, err := db.BackfillDanglingWorkerLogPaths(logsRoot)
	if err != nil {
		t.Fatalf("second BackfillDanglingWorkerLogPaths: %v", err)
	}
	if n2 != 0 {
		t.Errorf("expected 0 repairs on second run, got %d", n2)
	}
}

func TestBackfillDanglingWorkerLogPaths_BackslashPaths(t *testing.T) {
	db := newRepointTestDB(t)
	logsRoot := t.TempDir()

	// Simulate a Windows-style path where filepath.Join produced backslashes.
	// The SQL LIKE must match both separators.
	dangling := `C:\anvil\.workers\BD-W\.forge-logs\500.log`
	preserved := filepath.Join(logsRoot, "BD-W", "500.log")
	if err := os.MkdirAll(filepath.Dir(preserved), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preserved, []byte("win"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertWorker(&Worker{
		ID:        "w-win",
		BeadID:    "BD-W",
		Anvil:     "anvil",
		Status:    WorkerDone,
		LogPath:   dangling,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	n, err := db.BackfillDanglingWorkerLogPaths(logsRoot)
	if err != nil {
		t.Fatalf("BackfillDanglingWorkerLogPaths: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 repair for backslash path, got %d", n)
	}
	if got := getLogPath(t, db, "w-win"); got != preserved {
		t.Errorf("w-win log_path = %q, want %q", got, preserved)
	}
}

func TestBackfillDanglingWorkerLogPaths_SanitizesBeadID(t *testing.T) {
	db := newRepointTestDB(t)
	logsRoot := t.TempDir()

	// Bead ID with a slash — preserveWorktreeLogs sanitizes "/" → "_",
	// so the on-disk directory is "org_repo" not "org/repo".
	beadID := "org/repo"
	sanitizedID := "org_repo"

	dangling := filepath.Join("/anvil", ".workers", "x", ".forge-logs", "300.log")
	preserved := filepath.Join(logsRoot, sanitizedID, "300.log")
	if err := os.MkdirAll(filepath.Dir(preserved), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preserved, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertWorker(&Worker{
		ID:        "w-slash",
		BeadID:    beadID,
		Anvil:     "anvil",
		Status:    WorkerDone,
		LogPath:   dangling,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	n, err := db.BackfillDanglingWorkerLogPaths(logsRoot)
	if err != nil {
		t.Fatalf("BackfillDanglingWorkerLogPaths: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 repair, got %d", n)
	}
	if got := getLogPath(t, db, "w-slash"); got != preserved {
		t.Errorf("w-slash log_path = %q, want %q", got, preserved)
	}
}
