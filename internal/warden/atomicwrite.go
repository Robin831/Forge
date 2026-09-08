package warden

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path via a temp file in the same directory
// followed by a rename, so no reader ever observes a partially written file.
//
// It is one helper for both stores this package owns — the anvil's rules file
// and its archive — because a truncated read of either is silent: a plain
// os.WriteFile truncates and then writes, and a YAML sequence cut at a rule
// boundary is still valid YAML with fewer rules in it. Every reader here
// (LoadRules, LoadArchive, the smelter's copyIntoWorktree, which copies the
// live rules file into the worktree it then commits) would parse that short
// file as a smaller rule set and persist it, so the loss reads exactly like an
// intentional archive sweep and nothing detects it.
//
// The window is not notional and it is now entered on every Warden review:
// learnedRulesSection stamps the rules it emitted, and the files this package
// writes run to four figures of rules. The rename closes it for every caller
// at once — a reader sees the whole old file or the whole new one — and the
// Sync before it means a host that loses power mid-write is in the same
// position rather than holding a file of zeros.
//
// The temp file is created in the destination's own directory because a rename
// is only atomic within a filesystem, and it is removed on every failure path
// so a failed write leaves neither a partial target nor a stray sibling.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	// Preserve the existing mode where there is one, so a file an operator has
	// tightened is not widened by being rewritten.
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming temp file: %w", err)
	}
	cleanup = false
	return nil
}
