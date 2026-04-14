//go:build !windows

package temper

import "os"

// isReparsePoint returns false on Unix; directory links are symlinks and are
// already detected via os.ModeSymlink.
func isReparsePoint(_ os.FileInfo) bool {
	return false
}
