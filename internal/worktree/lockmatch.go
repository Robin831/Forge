package worktree

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type lockPackage struct {
	Version     string   `json:"version"`
	Optional    bool     `json:"optional"`
	DevOptional bool     `json:"devOptional"`
	Link        bool     `json:"link"`
	OS          []string `json:"os"`
	CPU         []string `json:"cpu"`
}

type lockFile struct {
	Packages map[string]lockPackage `json:"packages"`
}

func readLock(path string) (map[string]lockPackage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
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
// Every package the lockfile names must be installed at its version, except one npm may skip (optional,
// devOptional, or tied to an os or cpu); packages installed but no longer locked are ignored.
func installedMatchesLock(installedNM, lockPath string) (match, known bool, why string) {
	want, err := readLock(lockPath)
	if err != nil {
		return false, false, err.Error()
	}
	have, err := readLock(filepath.Join(installedNM, ".package-lock.json"))
	if err != nil {
		return false, false, err.Error()
	}
	for key, w := range want {
		if key == "" || w.Link {
			continue
		}
		h, ok := have[key]
		switch {
		case !ok && (w.Optional || w.DevOptional || len(w.OS) > 0 || len(w.CPU) > 0):
			continue
		case !ok:
			return false, true, key + " is locked at " + w.Version + " but not installed"
		case h.Version != w.Version:
			return false, true, key + " is installed at " + h.Version + " but locked at " + w.Version
		}
	}
	return true, true, ""
}
