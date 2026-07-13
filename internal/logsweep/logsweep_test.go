package logsweep

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// fakeDB is a lightweight workerLister double.
type fakeDB struct {
	workers []state.Worker
	nulled  []string
	events  []state.EventType
}

func (f *fakeDB) ActiveWorkers() ([]state.Worker, error) { return f.workers, nil }

func (f *fakeDB) NullWorkerLogPathsUnder(dir string) (int, error) {
	f.nulled = append(f.nulled, dir)
	return 0, nil
}

func (f *fakeDB) LogEvent(typ state.EventType, message, beadID, anvil string) error {
	f.events = append(f.events, typ)
	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestShouldSweep(t *testing.T) {
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-30 * 24 * time.Hour) // retention = 30 days

	tests := []struct {
		name             string
		newest           time.Time
		retentionDays    int
		hasRunningWorker bool
		want             bool
	}{
		{"disabled retention 0", now.Add(-100 * 24 * time.Hour), 0, false, false},
		{"disabled retention negative", now.Add(-100 * 24 * time.Hour), -5, false, false},
		{"running worker blocks deletion", cutoff.Add(-time.Hour), 30, true, false},
		{"exactly at cutoff retained", cutoff, 30, false, false},
		{"just under retention retained", cutoff.Add(time.Second), 30, false, false},
		{"just over retention removed", cutoff.Add(-time.Second), 30, false, true},
		{"far older removed", now.Add(-90 * 24 * time.Hour), 30, false, true},
		{"fresh retained", now.Add(-time.Hour), 30, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldSweep(tt.newest, tt.retentionDays, tt.hasRunningWorker, now)
			if got != tt.want {
				t.Fatalf("ShouldSweep(%v, %d, %v, now) = %v, want %v",
					tt.newest, tt.retentionDays, tt.hasRunningWorker, got, tt.want)
			}
		})
	}
}

// makeBeadDir creates logsRoot/<name>/file.log and back-dates both the file
// and the directory itself by age.
func makeBeadDir(t *testing.T, logsRoot, name string, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(logsRoot, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "smith.log")
	if err := os.WriteFile(f, []byte("log contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-age)
	if err := os.Chtimes(f, mt, mt); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, mt, mt); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunOnce_SelectsCorrectDirs(t *testing.T) {
	logsRoot := t.TempDir()

	oldDir := makeBeadDir(t, logsRoot, "old-bead", 40*24*time.Hour)
	freshDir := makeBeadDir(t, logsRoot, "fresh-bead", 24*time.Hour)
	activeDir := makeBeadDir(t, logsRoot, "active-bead", 40*24*time.Hour)

	// A stray file at the logs root (mimicking daemon.log) must never be touched.
	daemonLog := filepath.Join(logsRoot, "daemon.log")
	if err := os.WriteFile(daemonLog, []byte("daemon"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldLog := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(daemonLog, oldLog, oldLog); err != nil {
		t.Fatal(err)
	}

	db := &fakeDB{
		workers: []state.Worker{{BeadID: "active-bead", Status: "running"}},
	}

	m := New(db, discardLogger(), logsRoot, 24*time.Hour, func() int { return 30 })
	m.runOnce(context.Background())

	// old-bead is older than retention with no running worker: removed.
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("expected old-bead dir to be removed, stat err=%v", err)
	}
	// fresh-bead is within retention: kept.
	if _, err := os.Stat(freshDir); err != nil {
		t.Errorf("expected fresh-bead dir to remain, got err=%v", err)
	}
	// active-bead is old but has a running worker: kept.
	if _, err := os.Stat(activeDir); err != nil {
		t.Errorf("expected active-bead dir to remain (running worker), got err=%v", err)
	}
	// daemon.log at the root must remain untouched.
	if _, err := os.Stat(daemonLog); err != nil {
		t.Errorf("expected daemon.log to remain untouched, got err=%v", err)
	}

	// Exactly the removed dir should have had its log paths nulled.
	if len(db.nulled) != 1 || db.nulled[0] != oldDir {
		t.Errorf("expected NullWorkerLogPathsUnder called once for %q, got %v", oldDir, db.nulled)
	}

	// A summary event must always be emitted.
	if len(db.events) != 1 || db.events[0] != state.EventLogSweepDone {
		t.Errorf("expected one EventLogSweepDone, got %v", db.events)
	}
}

func TestRunOnce_SkipsNonBeadDirs(t *testing.T) {
	logsRoot := t.TempDir()

	// A directory whose name doesn't look like a bead ID (no hyphen) should
	// be left untouched even when old enough to be swept.
	weirdDir := filepath.Join(logsRoot, "randomstuff")
	if err := os.MkdirAll(weirdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(weirdDir, old, old); err != nil {
		t.Fatal(err)
	}

	// A bead-like dir that IS old enough — should be removed.
	oldDir := makeBeadDir(t, logsRoot, "Forge-old1", 40*24*time.Hour)

	db := &fakeDB{}
	m := New(db, discardLogger(), logsRoot, 24*time.Hour, func() int { return 30 })
	m.runOnce(context.Background())

	if _, err := os.Stat(weirdDir); os.IsNotExist(err) {
		t.Errorf("expected non-bead dir %q to remain, but it was removed", weirdDir)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("expected bead dir %q to be removed, stat err=%v", oldDir, err)
	}
}

func TestRunOnce_EmptyDirSwept(t *testing.T) {
	logsRoot := t.TempDir()

	emptyDir := filepath.Join(logsRoot, "Forge-empty")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(emptyDir, old, old); err != nil {
		t.Fatal(err)
	}

	db := &fakeDB{}
	m := New(db, discardLogger(), logsRoot, 24*time.Hour, func() int { return 30 })
	m.runOnce(context.Background())

	if _, err := os.Stat(emptyDir); !os.IsNotExist(err) {
		t.Errorf("expected empty old bead dir to be swept, stat err=%v", err)
	}
}

func TestDirNewestAndSize_UsesDirMtime(t *testing.T) {
	dir := t.TempDir()

	// Write a file and backdate it far into the past.
	f := filepath.Join(dir, "old.log")
	if err := os.WriteFile(f, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(f, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	// The directory itself was just created (recent mtime). dirNewestAndSize
	// should return a time at least as recent as the directory, not the old
	// file time.
	newest, size, err := dirNewestAndSize(dir)
	if err != nil {
		t.Fatalf("dirNewestAndSize: %v", err)
	}
	if size != 4 {
		t.Errorf("size = %d, want 4", size)
	}
	if newest.Before(time.Now().Add(-5 * time.Second)) {
		t.Errorf("newest = %v, expected recent (dir was just created)", newest)
	}
}

func TestLooksLikeBeadDir(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"Forge-abc1", true},
		{"Hytte-xyz9", true},
		{"org_repo-bead", true},
		{"BD-1", true},
		{"my-project_sub-task", true},
		{"nohyphen", false},
		{"has space-id", false},
		{".hidden-dir", false},
		{"path/slash-bad", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeBeadDir(tt.name); got != tt.want {
				t.Errorf("looksLikeBeadDir(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestRunOnce_DisabledWhenRetentionZero(t *testing.T) {
	logsRoot := t.TempDir()
	oldDir := makeBeadDir(t, logsRoot, "old-bead", 90*24*time.Hour)

	db := &fakeDB{}
	m := New(db, discardLogger(), logsRoot, 24*time.Hour, func() int { return 0 })
	m.runOnce(context.Background())

	if _, err := os.Stat(oldDir); err != nil {
		t.Errorf("expected old-bead dir to remain when retention disabled, got err=%v", err)
	}
	if len(db.nulled) != 0 {
		t.Errorf("expected no log-path nulling when disabled, got %v", db.nulled)
	}
	if len(db.events) != 0 {
		t.Errorf("expected no sweep event when disabled, got %v", db.events)
	}
}
