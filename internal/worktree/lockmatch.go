package worktree

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type lockPackage struct {
	Version     string `json:"version"`
	Resolved    string `json:"resolved"`
	Integrity   string `json:"integrity"`
	Optional    bool   `json:"optional"`
	DevOptional bool   `json:"devOptional"`
	Link        bool   `json:"link"`
}

// maxLockBytes bounds a lockfile read: the worktree's comes from the branch, which could make it anything.
const maxLockBytes = 64 << 20

type lockFile struct {
	Packages map[string]lockPackage `json:"packages"`
}

func readLock(path string) (map[string]lockPackage, error) {
	// Only a regular file of sane size: a tracked symlink to /dev/zero or a fifo must not hang or exhaust the daemon.
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxLockBytes {
		return nil, fmt.Errorf("%s is not a regular lockfile under %d bytes", path, maxLockBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxLockBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxLockBytes {
		return nil, fmt.Errorf("%s grew past %d bytes", path, maxLockBytes)
	}
	var l lockFile
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, err
	}
	if l.Packages == nil {
		return nil, fmt.Errorf("%s has no packages section (lockfile v1?)", path)
	}
	return l.Packages, nil
}

// installedMatchesLock compares what is installed in installedNM (npm's hidden lockfile,
// node_modules/.package-lock.json, written on every install) with the worktree's package-lock.json
// (DevInfra-rjb.3). known is false when either file is missing or unreadable: then nobody can tell.
// Every package the lockfile names must be installed from the same source (version, resolved, integrity; a link
// to the same target), except an optional or devOptional one npm may skip; and nothing may be installed that the
// lockfile no longer names (a left-over nested copy would shadow the hoisted one).
func installedMatchesLock(installedNM, lockPath string) (match, known bool, why string) {
	want, err := readLock(lockPath)
	if err != nil {
		return false, false, err.Error()
	}
	have, err := readLock(filepath.Join(installedNM, ".package-lock.json"))
	if os.IsNotExist(err) {
		// A lockfile, but no record of what is installed: an interrupted or foreign install, never shared as it is.
		return false, true, "nothing records what is installed (node_modules/.package-lock.json is missing)"
	}
	if err != nil {
		return false, false, err.Error()
	}
	for key, w := range want {
		if key == "" {
			continue
		}
		h, ok := have[key]
		switch {
		case !ok && (w.Optional || w.DevOptional):
			continue
		case !ok:
			return false, true, key + " is locked but not installed"
		case w.Link != h.Link || (w.Link && w.Resolved != h.Resolved):
			return false, true, key + " links elsewhere than locked"
		case h.Version != w.Version:
			return false, true, key + " is installed at " + h.Version + " but locked at " + w.Version
		case differs(h.Resolved, w.Resolved) || differs(h.Integrity, w.Integrity):
			return false, true, key + " is installed from another source than locked"
		}
	}
	for key := range have {
		if _, ok := want[key]; !ok && key != "" {
			return false, true, key + " is installed but no longer locked"
		}
	}
	return true, true, ""
}

// differs: both sides say something, and not the same thing (a field npm left out is not a difference).
func differs(a, b string) bool { return a != "" && b != "" && a != b }

// InstalledMatchesLock is installedMatchesLock for Temper, which checks again right before verification.
func InstalledMatchesLock(installedNM, lockPath string) (match, known bool, why string) {
	return installedMatchesLock(installedNM, lockPath)
}
