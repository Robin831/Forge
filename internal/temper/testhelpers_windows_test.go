//go:build windows

package temper

import (
	"os/exec"
	"testing"
)

// createTestDirLink creates an NTFS junction from dst pointing to src for use
// in tests. On Windows, production code uses mklink /J junctions (not
// symlinks) because symlinks require elevated privileges. This matches real
// behavior so the junction-detection path in isNodeModulesLinked is exercised.
func createTestDirLink(t *testing.T, src, dst string) {
	t.Helper()
	cmd := exec.Command("cmd", "/C", "mklink", "/J", dst, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %s: %v", dst, src, out, err)
	}
}
