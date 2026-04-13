//go:build windows

package worktree

import (
	"fmt"
	"os/exec"

	"github.com/Robin831/Forge/internal/executil"
)

// createJunction creates an NTFS junction from dst pointing to src.
// Junctions do not require elevated privileges (unlike symlinks on Windows)
// and are transparent to all file I/O.
func createJunction(src, dst string) error {
	// mklink /J creates a directory junction — no admin rights needed.
	cmd := executil.HideWindow(exec.Command("cmd", "/C", "mklink", "/J", dst, src))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mklink /J %s %s: %s: %w", dst, src, out, err)
	}
	return nil
}
