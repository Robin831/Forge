package logrotate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
