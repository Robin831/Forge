//go:build !windows

package worktree

// createJunction is a no-op on Unix — the caller uses os.Symlink directly.
// This stub exists only to satisfy the compiler on non-Windows platforms;
// createDirLink never calls it when runtime.GOOS != "windows".
func createJunction(src, dst string) error {
	panic("createJunction called on non-Windows platform")
}
