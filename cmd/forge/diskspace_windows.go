//go:build windows

package main

import (
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// diskFreeBytes returns the number of free bytes available on the volume
// containing the given path.
func diskFreeBytes(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free uint64
	err = windows.GetDiskFreeSpaceEx(p, (*uint64)(unsafe.Pointer(&free)), nil, nil)
	if err != nil {
		return 0, err
	}
	return free, nil
}

// filesystemKey returns a string that uniquely identifies the volume
// containing path. On Windows this is the volume name (e.g. "C:\").
func filesystemKey(path string) string {
	vol := filepath.VolumeName(path)
	if vol != "" {
		return vol + `\`
	}
	return `\`
}
