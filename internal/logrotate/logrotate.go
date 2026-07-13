// Package logrotate provides a minimal, dependency-free size-based log rotator
// that implements io.Writer. It is used as the sink for the daemon's slog
// handler so ~/.forge/logs/daemon.log cannot grow unbounded.
//
// Behaviour mirrors the subset of gopkg.in/natefinch/lumberjack.v2 that Forge
// needs: rotate when the active file would exceed MaxSizeMB, keep at most
// MaxBackups rotated files, and optionally gzip-compress the backups. Backups
// are named with a sortable timestamp (base-2006-01-02T15-04-05.000.ext) so the
// newest survive pruning. Writes are serialised with a mutex, so a single
// Logger is safe for concurrent use by multiple goroutines.
package logrotate

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// backupTimeFormat is the timestamp embedded in rotated file names. It is
// filesystem-safe (no colons) and lexically sortable so a plain string sort
// orders backups oldest-to-newest.
const backupTimeFormat = "2006-01-02T15-04-05.000"

// compressSuffix is appended to a backup's name once it has been gzip'd.
const compressSuffix = ".gz"

// megabyte is the number of bytes in one MB, used to convert MaxSizeMB.
const megabyte = 1024 * 1024

// Logger is an io.Writer that rotates the file at Filename once it reaches
// MaxSizeMB. The zero value is not usable; construct one with the exported
// fields set. All fields are read-only after the first Write.
type Logger struct {
	// Filename is the path of the active log file.
	Filename string
	// MaxSizeMB is the size threshold in megabytes at which the active file is
	// rotated. Values <= 0 fall back to defaultMaxSizeMB.
	MaxSizeMB int
	// MaxBackups is the maximum number of rotated files to retain. Older
	// backups beyond this count are deleted after each rotation. 0 keeps none.
	MaxBackups int
	// Compress gzip-compresses rotated files when true.
	Compress bool

	mu   sync.Mutex
	file *os.File
	size int64
}

const defaultMaxSizeMB = 50

// maxBytes returns the configured rotation threshold in bytes.
func (l *Logger) maxBytes() int64 {
	size := l.MaxSizeMB
	if size <= 0 {
		size = defaultMaxSizeMB
	}
	return int64(size) * megabyte
}

// Write implements io.Writer. It opens the active file on first use, rotates
// when the incoming write would push the file past the size threshold, and
// then appends the bytes.
func (l *Logger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		if err := l.openExisting(); err != nil {
			return 0, err
		}
	}

	// Rotate before writing when appending would exceed the threshold, but only
	// if the file already holds data — a single write larger than the threshold
	// still lands in a fresh file rather than an empty one.
	if l.size > 0 && l.size+int64(len(p)) > l.maxBytes() {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}

	n, err := l.file.Write(p)
	l.size += int64(n)
	return n, err
}

// Rotate forces an immediate rotation of the active file, regardless of size.
// It is safe to call before any Write; a missing active file is created first.
func (l *Logger) Rotate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		if err := l.openExisting(); err != nil {
			return err
		}
	}
	return l.rotate()
}

// Close closes the active file. The Logger may be reused afterwards; the next
// Write reopens the file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// openExisting opens (or creates) the active file for appending and records its
// current size. If the existing file already exceeds the threshold it is
// rotated immediately so an oversized pre-existing log is bounded on startup.
func (l *Logger) openExisting() error {
	if err := os.MkdirAll(filepath.Dir(l.Filename), 0o755); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}
	f, err := os.OpenFile(l.Filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}
	l.file = f
	if info, err := f.Stat(); err == nil {
		l.size = info.Size()
	}
	// An already-oversized file (e.g. the historical unrotated daemon.log) is
	// rotated out on startup rather than waiting for the next threshold cross.
	if l.size >= l.maxBytes() {
		if err := l.rotate(); err != nil {
			return err
		}
	}
	return nil
}

// rotate closes the active file, moves it aside to a timestamped backup,
// optionally compresses that backup, prunes old backups, and reopens a fresh
// active file. The caller must hold l.mu.
func (l *Logger) rotate() error {
	if l.file != nil {
		if err := l.file.Close(); err != nil {
			return err
		}
		l.file = nil
	}

	// Nothing to move if the active file is absent or empty.
	if info, err := os.Stat(l.Filename); err == nil && info.Size() > 0 {
		backup := l.uniqueBackupName(time.Now())
		if err := os.Rename(l.Filename, backup); err != nil {
			return fmt.Errorf("rotating log file: %w", err)
		}
		if l.Compress {
			// Best-effort: on failure the uncompressed backup is left in place so
			// no log data is lost. There is no logger to report to from here.
			_ = compressFile(backup)
		}
	}

	// Pruning failure must not prevent logging from resuming.
	_ = l.pruneBackups()

	f, err := os.OpenFile(l.Filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("reopening log file after rotation: %w", err)
	}
	l.file = f
	l.size = 0
	return nil
}

// backupName builds the rotated file name for the active file at time t, e.g.
// daemon.log -> daemon-2026-07-13T09-30-00.000.log.
func (l *Logger) backupName(t time.Time) string {
	dir := filepath.Dir(l.Filename)
	base := filepath.Base(l.Filename)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext)
	return filepath.Join(dir, fmt.Sprintf("%s-%s%s", prefix, t.UTC().Format(backupTimeFormat), ext))
}

// uniqueBackupName returns backupName(t), disambiguated with a numeric suffix if
// a backup with that name (compressed or not) already exists. Rapid successive
// rotations can share a millisecond-precision timestamp, so this guards against
// a rename silently clobbering the previous backup.
func (l *Logger) uniqueBackupName(t time.Time) string {
	candidate := l.backupName(t)
	dir := filepath.Dir(candidate)
	base := filepath.Base(candidate)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; ; i++ {
		if !backupExists(candidate) {
			return candidate
		}
		candidate = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
	}
}

// backupExists reports whether path exists either as-is or with the compress
// suffix appended.
func backupExists(path string) bool {
	if _, err := os.Lstat(path); err == nil {
		return true
	}
	if _, err := os.Lstat(path + compressSuffix); err == nil {
		return true
	}
	return false
}

// pruneBackups deletes the oldest rotated files so that at most MaxBackups
// remain. Both compressed and uncompressed backups are considered.
func (l *Logger) pruneBackups() error {
	if l.MaxBackups <= 0 {
		// Keep nothing beyond the active file.
		backups := l.listBackups()
		var firstErr error
		for _, b := range backups {
			if err := os.Remove(b); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}

	backups := l.listBackups()
	if len(backups) <= l.MaxBackups {
		return nil
	}
	// listBackups returns names sorted oldest-first; delete everything except
	// the newest MaxBackups entries.
	var firstErr error
	for _, b := range backups[:len(backups)-l.MaxBackups] {
		if err := os.Remove(b); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// listBackups returns the full paths of rotated files for this logger, sorted
// oldest-first (lexical order matches chronological order via the timestamp).
func (l *Logger) listBackups() []string {
	dir := filepath.Dir(l.Filename)
	base := filepath.Base(l.Filename)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext) + "-"

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var backups []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == base {
			continue // the active file, never a backup
		}
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// Must end with the original extension or that extension plus .gz.
		if !strings.HasSuffix(name, ext) && !strings.HasSuffix(name, ext+compressSuffix) {
			continue
		}
		backups = append(backups, filepath.Join(dir, name))
	}
	sort.Strings(backups)
	return backups
}

// compressFile gzip-compresses src into src+".gz" and removes src on success.
func compressFile(src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	dst := src + compressSuffix
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(out)

	if _, err := io.Copy(gz, in); err != nil {
		gz.Close()
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := gz.Close(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	// Remove the uncompressed original only after the compressed copy is durable.
	return os.Remove(src)
}
