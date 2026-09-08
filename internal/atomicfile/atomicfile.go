// Package atomicfile is the one definition of "replace a file so no reader
// ever sees it half-written" — a temp file in the destination's own directory,
// written, synced, and renamed over the target.
//
// It is one package rather than one copy per store because it was two: the web
// layer wrote forge.yaml this way for its fsnotify watcher, warden wrote its
// rules file and archive this way for its own readers, and the two had already
// come apart on the question the primitive exists to answer — only one of them
// synced before the rename, so the packages disagreed about whether an
// "atomically written" file survives a power loss, with nothing in either to
// say which behaviour was meant. Same argument as internal/textfmt and
// internal/gitfail: the copies did not disagree when they were written, which
// is why each was written instead of one of them being found.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// defaultMode is the permission a newly created file is given. An existing
// file's own mode wins, so this applies only on first write.
const defaultMode = os.FileMode(0o644)

// Write replaces path with data via a temp file in the same directory followed
// by a rename, so no reader ever observes a partially written file. The parent
// directory is created when missing.
//
// The failure it closes is silent in both of this repository's stores: a plain
// os.WriteFile truncates and then writes, and both a YAML sequence cut at an
// element boundary and a truncated config file are still valid YAML with less
// in them. Warden's readers (LoadRules, LoadArchive, the smelter's
// copyIntoWorktree) would parse a short rules file as a smaller rule set and
// persist it, so the loss reads exactly like an intentional archive sweep;
// forge.yaml's fsnotify watcher would hot-reload the truncated config.
//
// Neither window is notional. Warden's is entered on every review now that
// learnedRulesSection stamps the rules it emitted, over files that run to four
// figures of rules, and the config one is entered by every dashboard edit.
// The rename closes it for every reader at once — each sees the whole old file
// or the whole new one — and the Sync before it means a host that loses power
// mid-write is in that same position rather than holding a file of zeros.
//
// The temp file is created in the destination's own directory because a rename
// is only atomic within a filesystem, and it is removed on every failure path
// so a failed write leaves neither a partial target nor a stray sibling.
func Write(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	// Preserve the existing mode where there is one, so a file an operator has
	// tightened is not widened by being rewritten.
	mode := defaultMode
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
