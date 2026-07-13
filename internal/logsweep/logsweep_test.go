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

// makeBeadDir creates logsRoot/<name>/file.log and back-dates it by age.
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
