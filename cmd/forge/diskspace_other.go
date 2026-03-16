//go:build !windows

package main

import (
	"fmt"
	"syscall"
)

// diskFreeBytes returns the number of free bytes available on the filesystem
// containing the given path.
func diskFreeBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	// Bavail = blocks available to unprivileged users.
	return stat.Bavail * uint64(stat.Bsize), nil
}

// filesystemKey returns a string that uniquely identifies the filesystem
// containing path. On Unix this is the Fsid from statfs, which correctly
// distinguishes separate mounts even when they share the same root prefix.
// Falls back to the path itself if statfs fails.
func filesystemKey(path string) string {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return path
	}
	return fmt.Sprintf("%d:%d", stat.Fsid.Val[0], stat.Fsid.Val[1])
}
