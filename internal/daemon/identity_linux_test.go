//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withProcFS swaps the package-level procFS root for the duration of the test.
func withProcFS(t *testing.T, root string) {
	t.Helper()
	prev := procFS
	procFS = root
	t.Cleanup(func() { procFS = prev })
}

// writeFakeProcEntry sets up a fake /proc/<pid>/comm (and optionally an
// /proc/<pid>/exe symlink) under root, mimicking the real procfs layout.
func writeFakeProcEntry(t *testing.T, root string, pid int, comm string, exeTarget string) {
	t.Helper()
	pidDir := filepath.Join(root, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(pidDir, 0o755))
	if comm != "" {
		require.NoError(t, os.WriteFile(filepath.Join(pidDir, "comm"), []byte(comm+"\n"), 0o644))
	}
	if exeTarget != "" {
		require.NoError(t, os.Symlink(exeTarget, filepath.Join(pidDir, "exe")))
	}
}

func TestIsForgeProcess_CommMatchesForge(t *testing.T) {
	root := t.TempDir()
	withProcFS(t, root)
	writeFakeProcEntry(t, root, 12345, "forge", "")

	ok, err := isForgeProcess(12345)
	require.NoError(t, err)
	assert.True(t, ok, "comm == forge should be identified as a forge process")
}

func TestIsForgeProcess_CommIsDifferentBinary(t *testing.T) {
	root := t.TempDir()
	withProcFS(t, root)
	// Mimics the production failure mode: container PID 1 is "tini" or
	// some other init, not forge, but a stale pidfile points at it.
	writeFakeProcEntry(t, root, 7, "tini", "")

	ok, err := isForgeProcess(7)
	require.NoError(t, err)
	assert.False(t, ok, "non-forge comm must not be treated as a forge process")
}

func TestIsForgeProcess_PidGoneReturnsFalse(t *testing.T) {
	root := t.TempDir()
	withProcFS(t, root)

	ok, err := isForgeProcess(99999)
	require.NoError(t, err, "ENOENT must be treated as 'process gone', not an error")
	assert.False(t, ok)
}

func TestIsForgeProcess_CommBasenameOnPath(t *testing.T) {
	// comm normally contains a bare basename, but defensively handle the
	// case where the kernel reports a path-like value — only the basename
	// should be compared.
	root := t.TempDir()
	withProcFS(t, root)
	writeFakeProcEntry(t, root, 555, "/usr/local/bin/forge", "")

	ok, err := isForgeProcess(555)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestIsForgeProcess_LookalikeBinaryIsNotForge(t *testing.T) {
	// "forge-worker" must not be mistaken for the forge daemon — basename
	// comparison requires an exact match.
	root := t.TempDir()
	withProcFS(t, root)
	writeFakeProcEntry(t, root, 777, "forge-worker", "")

	ok, err := isForgeProcess(777)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestIsForgeProcess_RealTestProcessIsNotForge(t *testing.T) {
	// Sanity check against the real procfs: the running test binary is
	// "<pkg>.test" (or similar), never "forge". This guards against the
	// original bug where any live PID was treated as a forge process.
	ok, err := isForgeProcess(os.Getpid())
	require.NoError(t, err)
	assert.False(t, ok, "test binary should not be identified as forge")
}

func TestIsForgeProcess_ExeFallbackForgeProcess(t *testing.T) {
	// When comm is unreadable (EACCES, e.g. hidepid) but exe points to forge,
	// the function should fall back to exe and correctly identify the process.
	if os.Getuid() == 0 {
		t.Skip("permission-based tests cannot run as root")
	}
	root := t.TempDir()
	withProcFS(t, root)
	pid := 12345
	pidDir := filepath.Join(root, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(pidDir, 0o755))

	commPath := filepath.Join(pidDir, "comm")
	require.NoError(t, os.WriteFile(commPath, []byte("something\n"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(commPath, 0o644) })

	require.NoError(t, os.Symlink("/usr/local/bin/forge", filepath.Join(pidDir, "exe")))

	ok, err := isForgeProcess(pid)
	require.NoError(t, err)
	assert.True(t, ok, "exe fallback should identify forge when comm is unreadable")
}

func TestIsForgeProcess_ExeFallbackNonForgeProcess(t *testing.T) {
	// When comm is unreadable but exe points to a non-forge binary, the
	// function should correctly return false via the exe fallback.
	if os.Getuid() == 0 {
		t.Skip("permission-based tests cannot run as root")
	}
	root := t.TempDir()
	withProcFS(t, root)
	pid := 12346
	pidDir := filepath.Join(root, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(pidDir, 0o755))

	commPath := filepath.Join(pidDir, "comm")
	require.NoError(t, os.WriteFile(commPath, []byte("something\n"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(commPath, 0o644) })

	require.NoError(t, os.Symlink("/usr/bin/python3", filepath.Join(pidDir, "exe")))

	ok, err := isForgeProcess(pid)
	require.NoError(t, err)
	assert.False(t, ok, "exe fallback should not identify non-forge binary as forge")
}

func TestIsForgeProcess_PermissionDeniedOnBothReturnsError(t *testing.T) {
	// On a hidepid procfs mount, both comm and exe may be inaccessible.
	// The function must return an error (not false, nil) so IsRunning()
	// assumes the process is alive rather than deleting the pidfile.
	if os.Getuid() == 0 {
		t.Skip("permission-based tests cannot run as root")
	}
	root := t.TempDir()
	withProcFS(t, root)
	pid := 12347
	pidDir := filepath.Join(root, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(pidDir, 0o755))

	commPath := filepath.Join(pidDir, "comm")
	require.NoError(t, os.WriteFile(commPath, []byte("forge\n"), 0o644))

	// Make the directory non-executable so all entries (comm, exe) become
	// inaccessible — mimicking hidepid=2 behaviour.
	// Register pidDir cleanup first so it runs LAST (LIFO), after comm cleanup.
	t.Cleanup(func() { _ = os.Chmod(commPath, 0o644) })
	t.Cleanup(func() { _ = os.Chmod(pidDir, 0o755) })
	require.NoError(t, os.Chmod(pidDir, 0o000))

	_, err := isForgeProcess(pid)
	assert.Error(t, err, "permission denied on both comm and exe should return an error, not (false, nil)")
}

func TestIsRunning_StalePidfileToNonForgeProcessIsCleanedUp(t *testing.T) {
	// Reproduces the production scenario: a pidfile left on a PVC after a
	// kill -9 points at a PID (the test process here) which is alive but
	// is not a forge binary. IsRunning() must return (0, false) and remove
	// the stale pidfile so the next writePID() can claim it.
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".forge"), 0o755))

	pidPath := filepath.Join(home, ".forge", PIDFileName)
	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644))

	pid, running := IsRunning()
	assert.Equal(t, 0, pid)
	assert.False(t, running, "non-forge PID must not be treated as a running forge daemon")

	_, err := os.Stat(pidPath)
	assert.True(t, os.IsNotExist(err), "stale pidfile should have been removed")
}

func TestIsRunning_PidfileWithDeadPidReturnsNotRunning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".forge"), 0o755))

	// PID well above the typical pid_max for transient processes — almost
	// certainly does not exist.
	require.NoError(t, os.WriteFile(filepath.Join(home, ".forge", PIDFileName), []byte("4194303"), 0o644))

	pid, running := IsRunning()
	assert.Equal(t, 0, pid)
	assert.False(t, running)
}

func TestProcStartTime_ReadsPidDirModTime(t *testing.T) {
	// procStartTime should return the mtime of /proc/<pid>/ which on real
	// Linux equals the process creation time. Fake it under a temp procfs
	// root and assert the stat readback matches.
	root := t.TempDir()
	withProcFS(t, root)

	pid := 12345
	pidDir := filepath.Join(root, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(pidDir, 0o755))

	want := time.Date(2026, 5, 12, 6, 30, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(pidDir, want, want))

	got, err := procStartTime(pid)
	require.NoError(t, err)
	assert.True(t, got.Equal(want), "got %v, want %v", got, want)
}

func TestProcStartTime_MissingPidReturnsError(t *testing.T) {
	root := t.TempDir()
	withProcFS(t, root)

	_, err := procStartTime(99999)
	assert.Error(t, err, "non-existent pid dir must surface an error so IsRunning falls back to other checks")
}

func TestIsRunning_PidfilePredatesProcessStartIsCleanedUp(t *testing.T) {
	// The container-recycle scenario: previous incarnation wrote a pidfile
	// at T1, was killed, the pod restarted, the new forge binary now sits
	// at the same low PID with a process-start time T2 > T1. comm matches
	// (both are "forge") so isForgeProcess returns true, but the pidfile
	// is genuinely stale. The mtime-vs-procstart comparison should catch it.
	root := t.TempDir()
	withProcFS(t, root)
	pid := 7
	pidDir := filepath.Join(root, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(pidDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pidDir, "comm"), []byte("forge\n"), 0o644))

	// Process started 1 hour ago.
	procStart := time.Now().Add(-1 * time.Hour)
	require.NoError(t, os.Chtimes(pidDir, procStart, procStart))

	// Pidfile predates the process by 2 hours — well outside the 5s skew
	// allowance.
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".forge"), 0o755))
	pidPath := filepath.Join(home, ".forge", PIDFileName)
	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0o644))
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(pidPath, old, old))

	// Note: the real /proc/<pid>/ exists for PID 7 on the test host too,
	// but our withProcFS override redirects procStartTime to the temp
	// root, so the fake mtime is what counts. Liveness via Signal(0) is
	// also checked against the real host — since PID 7 may or may not
	// exist on the test host, this test exercises the staleness branch
	// only when the liveness check passes; otherwise IsRunning bails
	// earlier with (0, false), which is also a correct outcome for the
	// caller. We accept either, and only assert the post-condition: no
	// pidfile remains.
	_, _ = IsRunning()
	_, statErr := os.Stat(pidPath)
	assert.True(t, os.IsNotExist(statErr),
		"pidfile predating process start must be removed regardless of which "+
			"branch IsRunning takes (signal-0 dead or mtime-stale)")
}
