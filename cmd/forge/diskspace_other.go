//go:build !windows

package main

import "syscall"

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
