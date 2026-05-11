//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

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
