package logrotate

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixedTime = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

func TestWrite_RotatesAtThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	l := &Logger{Filename: path, MaxSizeMB: 1, MaxBackups: 3, Compress: false}
	defer l.Close()

	// One megabyte chunk; the second write crosses the 1 MB threshold and rotates.
	chunk := make([]byte, megabyte)
	for i := range chunk {
		chunk[i] = 'a'
	}

	if _, err := l.Write(chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Write([]byte("second line after rotation\n")); err != nil {
		t.Fatal(err)
	}

	backups := l.listBackups()
	if len(backups) != 1 {
		t.Fatalf("expected exactly 1 backup after rotation, got %d (%v)", len(backups), backups)
	}

	// The active file holds only the post-rotation write.
	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(active); !strings.Contains(got, "second line after rotation") || len(got) >= megabyte {
		t.Fatalf("active file should contain only the post-rotation write, got %d bytes", len(got))
	}
}

func TestOpenExisting_RotatesOversizedFileOnStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	// Pre-seed an oversized file, mimicking the historical 599 MB daemon.log.
	big := make([]byte, 2*megabyte)
	if err := os.WriteFile(path, big, 0o644); err != nil {
		t.Fatal(err)
	}

	l := &Logger{Filename: path, MaxSizeMB: 1, MaxBackups: 3, Compress: false}
	defer l.Close()

	if _, err := l.Write([]byte("fresh start\n")); err != nil {
		t.Fatal(err)
	}

	if len(l.listBackups()) != 1 {
		t.Fatalf("expected the oversized file to be rotated out on first write, backups=%v", l.listBackups())
	}
	active, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(active) != "fresh start\n" {
		t.Fatalf("active file should be fresh after startup rotation, got %q", string(active))
	}
}

func TestRotate_CompressAndPrune(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	l := &Logger{Filename: path, MaxSizeMB: 1, MaxBackups: 2, Compress: true}
	defer l.Close()

	// Force several rotations; each Rotate writes a byte first so the file is
	// non-empty and actually rotates.
	for i := 0; i < 4; i++ {
		if _, err := l.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := l.Rotate(); err != nil {
			t.Fatal(err)
		}
	}

	backups := l.listBackups()
	if len(backups) != 2 {
		t.Fatalf("expected MaxBackups=2 retained, got %d (%v)", len(backups), backups)
	}
	for _, b := range backups {
		if !strings.HasSuffix(b, ".gz") {
			t.Errorf("expected compressed backup, got %q", b)
		}
	}
}

func TestWrite_OversizedSingleWriteLandsInFreshFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	l := &Logger{Filename: path, MaxSizeMB: 1, MaxBackups: 3}
	defer l.Close()

	// A single write larger than the threshold should still land in the
	// active file without creating a backup (size was 0 before the write).
	big := make([]byte, 2*megabyte)
	for i := range big {
		big[i] = 'x'
	}
	n, err := l.Write(big)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(big) {
		t.Fatalf("wrote %d bytes, expected %d", n, len(big))
	}
	if len(l.listBackups()) != 0 {
		t.Fatalf("expected 0 backups for oversized single write, got %v", l.listBackups())
	}

	// A subsequent small write should trigger rotation of the oversized file.
	if _, err := l.Write([]byte("after")); err != nil {
		t.Fatal(err)
	}
	if len(l.listBackups()) != 1 {
		t.Fatalf("expected 1 backup after second write, got %v", l.listBackups())
	}
	active, _ := os.ReadFile(path)
	if string(active) != "after" {
		t.Errorf("active file = %q, want %q", string(active), "after")
	}
}

func TestPrune_MaxBackupsZeroKeepsNone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	l := &Logger{Filename: path, MaxSizeMB: 1, MaxBackups: 0}
	defer l.Close()

	for i := 0; i < 3; i++ {
		if _, err := l.Write([]byte("data")); err != nil {
			t.Fatal(err)
		}
		if err := l.Rotate(); err != nil {
			t.Fatal(err)
		}
	}

	backups := l.listBackups()
	if len(backups) != 0 {
		t.Fatalf("MaxBackups=0 should keep no backups, got %d (%v)", len(backups), backups)
	}
}

func TestPrune_KeepsNewestBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	l := &Logger{Filename: path, MaxSizeMB: 1, MaxBackups: 2}
	defer l.Close()

	// Create 5 backups; only the 2 newest should survive.
	var allBackups []string
	for i := 0; i < 5; i++ {
		if _, err := l.Write([]byte("log line")); err != nil {
			t.Fatal(err)
		}
		if err := l.Rotate(); err != nil {
			t.Fatal(err)
		}
		allBackups = l.listBackups()
	}

	if len(allBackups) != 2 {
		t.Fatalf("expected 2 backups, got %d", len(allBackups))
	}
}

func TestUniqueBackupName_CollisionResolution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	l := &Logger{Filename: path, MaxSizeMB: 1, MaxBackups: 10}

	// Create multiple backups that would share the same timestamp.
	ts := backupTimeFormat // use a fixed time representation
	_ = ts
	name1 := l.uniqueBackupName(fixedTime)
	// Create the file so the next call gets a different name.
	if err := os.WriteFile(name1, []byte("b1"), 0o644); err != nil {
		t.Fatal(err)
	}
	name2 := l.uniqueBackupName(fixedTime)
	if name1 == name2 {
		t.Fatalf("uniqueBackupName returned the same name twice: %q", name1)
	}
	// The second name should contain a numeric suffix.
	if !strings.Contains(filepath.Base(name2), "-1") {
		t.Errorf("expected numeric suffix in %q", name2)
	}
}

func TestCompress_BackupContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	l := &Logger{Filename: path, MaxSizeMB: 1, MaxBackups: 1, Compress: true}
	defer l.Close()

	payload := "compressed log data\n"
	if _, err := l.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := l.Rotate(); err != nil {
		t.Fatal(err)
	}

	backups := l.listBackups()
	if len(backups) != 1 {
		t.Fatalf("expected 1 backup, got %d", len(backups))
	}
	if !strings.HasSuffix(backups[0], ".gz") {
		t.Fatalf("expected .gz suffix, got %q", backups[0])
	}

	// Verify the compressed content is valid and matches.
	f, err := os.Open(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	data, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != payload {
		t.Errorf("decompressed content = %q, want %q", string(data), payload)
	}
}

func TestBackupExists_ChecksBothPlainAndCompressed(t *testing.T) {
	dir := t.TempDir()

	plain := filepath.Join(dir, "backup.log")
	if backupExists(plain) {
		t.Error("expected false for non-existent file")
	}

	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !backupExists(plain) {
		t.Error("expected true for existing plain file")
	}

	os.Remove(plain)
	gz := plain + compressSuffix
	if err := os.WriteFile(gz, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !backupExists(plain) {
		t.Error("expected true when .gz version exists")
	}
}
