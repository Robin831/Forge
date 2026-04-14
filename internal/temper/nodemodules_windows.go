//go:build windows

package temper

import (
	"os"
	"syscall"
)

// isReparsePoint returns true if info has the FILE_ATTRIBUTE_REPARSE_POINT
// Windows attribute set. NTFS junctions (created by mklink /J) are reparse
// points but do NOT set os.ModeSymlink in Go's os.FileInfo, so this check
// is needed in addition to the ModeSymlink bit.
func isReparsePoint(info os.FileInfo) bool {
	sys, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return false
	}
	const fileAttributeReparsePoint = 0x400
	return sys.FileAttributes&fileAttributeReparsePoint != 0
}
