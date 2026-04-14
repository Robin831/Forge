//go:build !windows

package temper

import (
	"os"
	"testing"
)

// createTestDirLink creates a symlink from dst pointing to src for use in
// tests. On Unix, production code also uses symlinks, so this matches real
// behavior exactly.
func createTestDirLink(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.Symlink(src, dst); err != nil {
		t.Fatalf("os.Symlink(%s, %s): %v", src, dst, err)
	}
}
