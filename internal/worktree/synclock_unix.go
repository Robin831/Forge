//go:build !windows

package worktree

import (
	"os"
	"syscall"
)

// tryLockFile takes an exclusive lock without waiting; the OS drops it when the holder exits, however it exits.
func tryLockFile(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}

func unlockFile(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
