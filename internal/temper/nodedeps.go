package temper

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/Robin831/Forge/internal/worktree"
)

// SkipReasonFreshInstall: Temper installed this folder's dependencies itself just before, from the same lockfile.
const SkipReasonFreshInstall SkipReason = "fresh_install"

// ensureNodeDeps makes each Node project's node_modules match its lockfile before verification (DevInfra-rjb.3):
// worktree creation linked the anvil's install only if it matched then, but the smith may have changed the lockfile
// since, and an anvil's install step runs only when the branch touches it. A stale link is removed (the link only)
// and the folder installed with npm ci; so is a stale own install, and a missing one where the anvil has one (the
// project has dependencies). Unknown (no lockfile, another package manager) is left as it is. It returns the steps
// it ran and the folders it installed; ok is false when an install failed, and verification then stops.
func ensureNodeDeps(ctx context.Context, worktreePath string, defaultTimeout time.Duration, outputCap int) (steps []StepResult, installed map[string]bool, ok bool) {
	installed = map[string]bool{}
	anvil := worktree.InferAnvilPath(worktreePath)
	for _, sub := range detectNodeDirs(worktreePath) {
		dir := filepath.Join(worktreePath, sub)
		lock := filepath.Join(dir, "package-lock.json")
		if !fileExists(dir, "package-lock.json") {
			continue
		}
		nm := filepath.Join(dir, "node_modules")
		need, why := false, ""
		if _, err := os.Lstat(nm); os.IsNotExist(err) {
			if anvil != "" {
				if info, err := os.Stat(filepath.Join(anvil, sub, "node_modules")); err == nil && info.IsDir() {
					need, why = true, "no node_modules, and the anvil's project has one"
				}
			}
		} else if err != nil {
			continue
		} else if isNodeModulesLinked(dir) {
			target, err := filepath.EvalSymlinks(nm)
			if err != nil {
				continue
			}
			if match, known, reason := worktree.InstalledMatchesLock(target, lock); known && !match {
				// os.Remove takes the link itself, never the shared tree behind it.
				if err := os.Remove(nm); err != nil {
					log.Printf("[temper] node_modules in %q is a stale link and could not be removed: %v", sub, err)
					continue
				}
				need, why = true, "the linked install does not match the lockfile: "+reason
			}
		} else if match, known, reason := worktree.InstalledMatchesLock(nm, lock); known && !match {
			need, why = true, "the install does not match the lockfile: "+reason
		}
		if !need {
			continue
		}
		log.Printf("[temper] Installing %q before verification: %s", sub, why)
		name := "node-install"
		if sub != "" {
			name += ":" + sub
		}
		result := runStep(ctx, worktreePath, Step{Name: name, Command: "npm", Args: []string{"ci", "--prefer-offline", "--no-audit", "--no-fund"},
			Dir: sub, Timeout: 10 * time.Minute}, defaultTimeout, outputCap)
		steps = append(steps, result)
		if !result.Passed {
			return steps, installed, false
		}
		installed[dir] = true
	}
	return steps, installed, true
}
