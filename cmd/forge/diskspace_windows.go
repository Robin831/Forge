//go:build windows

package main

import (
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
