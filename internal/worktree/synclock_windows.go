//go:build windows

package worktree

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile takes an exclusive lock without waiting; the OS drops it when the holder exits, however it exits.
func tryLockFile(f *os.File) bool {
	var o windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &o) == nil
}

func unlockFile(f *os.File) {
	var o windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &o)
}
