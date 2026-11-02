package worktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// syncAnvil keeps the anvil's own checkout and its shared Node installs current before a worktree is made from it
// (DevInfra-rjb.4), so worktrees can share them again (rjb.3 shares only a matching install). Best effort and never
// waited on: when another sync holds the anvil, or anything fails, it is skipped and the worktree installs its own.
func syncAnvil(ctx context.Context, anvilPath string) {
	unlock, ok := lockAnvil(anvilPath)
	if !ok {
		return
	}
	defer unlock()
	// One deadline for the whole sync, under the lock's stale age, so no other process takes the lock over mid-run.
	ctx, cancel := context.WithTimeout(ctx, syncDeadline)
	defer cancel()
	fastForwardAnvil(ctx, anvilPath)
	for _, sub := range nodeModulesDirs {
		if ctx.Err() != nil {
			return
		}
		refreshSharedInstall(ctx, anvilPath, sub)
	}
}

const (
	syncLockName   = ".forge-sync.lock"
	installTimeout = 10 * time.Minute
	syncDeadline   = 25 * time.Minute
	failureBackoff = 30 * time.Minute
)

// lockAnvil takes the anvil for one sync across processes, with an OS file lock (flock, LockFileEx) that the OS drops
// when its holder exits: no stale lock to reclaim, so no two syncs can both win. It never waits.
func lockAnvil(anvilPath string) (func(), bool) {
	f, err := os.OpenFile(filepath.Join(gitDir(anvilPath), syncLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		slog.Warn("anvil sync: cannot open the sync lock; not syncing", "anvil", anvilPath, "error", err)
		return nil, false
	}
	if !tryLockFile(f) {
		_ = f.Close()
		return nil, false
	}
	return func() { unlockFile(f); _ = f.Close() }, true
}

// syncInProgress reports that a sync holds the anvil's lock right now.
func syncInProgress(anvilPath string) bool {
	f, err := os.OpenFile(filepath.Join(gitDir(anvilPath), syncLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	if !tryLockFile(f) {
		return true
	}
	unlockFile(f)
	return false
}

// gitDir is git's own directory for the anvil, where the lock lives (never tracked, never in a worktree); .git can be
// a file pointing elsewhere (a submodule or a worktree anvil).
func gitDir(anvilPath string) string {
	cmd := exec.Command("git", "-C", anvilPath, "rev-parse", "--git-common-dir")
	cmd.Env = localGitEnv() // an inherited GIT_DIR would point the lock at another repository
	out, err := cmd.Output()
	if dir := strings.TrimSpace(string(out)); err == nil && dir != "" {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(anvilPath, dir)
		}
		return dir
	}
	return filepath.Join(anvilPath, ".git")
}

// fastForwardAnvil moves the anvil to origin/<branch> when it is on its main branch, its tracked files are clean and
// it is only behind; anything else (a diverged or dirty checkout) is logged and left as it is.
func fastForwardAnvil(ctx context.Context, anvilPath string) {
	branch, err := CurrentBranch(ctx, anvilPath)
	if err != nil || !isMainBranch(branch) || branch == "HEAD" {
		return
	}
	var status bytes.Buffer
	if err := gitCmdOut(ctx, anvilPath, &status, "status", "--porcelain", "--untracked-files=no"); err != nil {
		slog.Warn("anvil sync: git status failed; not fast-forwarding", "anvil", anvilPath, "error", err)
		return
	}
	if status.Len() > 0 {
		slog.Warn("anvil sync: not fast-forwarding, the anvil has local changes", "anvil", anvilPath)
		return
	}
	upstream := "origin/" + branch
	if gitCmdQuiet(ctx, anvilPath, "rev-parse", "--verify", "--quiet", upstream) != nil {
		slog.Warn("anvil sync: not fast-forwarding, no upstream to follow", "anvil", anvilPath, "upstream", upstream)
		return
	}
	if gitCmdQuiet(ctx, anvilPath, "merge-base", "--is-ancestor", "HEAD", upstream) != nil {
		slog.Warn("anvil sync: not fast-forwarding, the anvil is not behind its upstream only", "anvil", anvilPath, "upstream", upstream)
		return
	}
	if err := gitCmd(ctx, anvilPath, "merge", "--ff-only", "--quiet", upstream); err != nil {
		slog.Warn("anvil sync: fast-forward failed", "anvil", anvilPath, "error", err)
	}
}

// npmCommand runs npm ci in dir; tests replace it. In its own process group, cancelled and reaped as a whole (as
// Temper's steps are), so no postinstall child goes on changing the install after the lock is released.
var npmCommand = func(ctx context.Context, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "npm", "ci", "--prefer-offline", "--no-audit", "--no-fund")
	cmd.Dir = dir
	executil.SetProcessGroup(cmd)
	cmd.Cancel = func() error { return executil.KillProcessTree(cmd) }
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	_ = executil.KillProcessTree(cmd)
	return out, err
}

var failures sync.Map // anvil|sub|lockfile hash -> time of a failed install: not retried before failureBackoff

// refreshSharedInstall runs npm ci in the anvil's own <sub> when its shared install no longer matches its lockfile,
// in place, so the project's own scripts, patches and .npmrc apply. Only when no worktree or preview links to it:
// npm ci empties node_modules first. Otherwise it waits for a later sync, and the worktrees install their own.
func refreshSharedInstall(ctx context.Context, anvilPath, sub string) {
	dir := filepath.Join(anvilPath, sub)
	nm := filepath.Join(dir, "node_modules")
	lockPath := filepath.Join(dir, "package-lock.json")
	if info, err := os.Lstat(nm); err != nil || !info.IsDir() {
		return // no shared install here, or it is a link (not ours to rewrite)
	}
	if match, known, _ := installedMatchesLock(nm, lockPath); match || !known {
		return
	}
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		return
	}
	sum := sha256.Sum256(raw)
	key := anvilPath + "|" + sub + "|" + hex.EncodeToString(sum[:8])
	if at, ok := failures.Load(key); ok && time.Since(at.(time.Time)) < failureBackoff {
		return
	}
	if users := linksTo(ctx, anvilPath, nm); len(users) > 0 {
		slog.Info("anvil sync: shared node_modules is stale but in use; worktrees install their own until it is free", "anvil", anvilPath, "dir", sub, "users", len(users))
		return
	}
	ictx, cancel := context.WithTimeout(ctx, installTimeout)
	out, err := npmCommand(ictx, dir)
	cancel()
	if err == nil {
		if match, _, why := installedMatchesLock(nm, lockPath); !match {
			err = errors.New("npm ci did not produce the locked tree: " + why)
		}
	}
	if err != nil {
		failures.Range(func(k, v any) bool {
			if time.Since(v.(time.Time)) > failureBackoff {
				failures.Delete(k)
			}
			return true
		})
		failures.Store(key, time.Now())
		slog.Warn("anvil sync: refreshing the shared node_modules failed; retried after the back-off", "anvil", anvilPath, "dir", sub, "error", err, "output", tail(out))
		return
	}
	failures.Delete(key)
	slog.Info("anvil sync: shared node_modules refreshed", "anvil", anvilPath, "dir", sub)
}

// linksTo lists every worktree of the anvil (git worktree list: bead worktrees and previews alike) whose node_modules
// link resolves to nm.
func linksTo(ctx context.Context, anvilPath, nm string) []string {
	real, err := filepath.EvalSymlinks(nm)
	if err != nil {
		return nil
	}
	var list bytes.Buffer
	// -z: paths as they are, not quoted, so a path with a newline is still the path.
	if gitCmdOut(ctx, anvilPath, &list, "worktree", "list", "--porcelain", "-z") != nil {
		return []string{"(git worktree list failed)"} // cannot tell: treat as in use
	}
	var users []string
	for _, field := range strings.Split(list.String(), "\x00") {
		path, ok := strings.CutPrefix(field, "worktree ")
		if !ok || filepath.Clean(path) == filepath.Clean(anvilPath) {
			continue
		}
		for _, sub := range nodeModulesDirs {
			link := filepath.Join(path, sub, "node_modules")
			if linked, _ := isReparsePoint(link); !linked {
				continue
			}
			if target, err := filepath.EvalSymlinks(link); err == nil && target == real {
				users = append(users, path)
			}
		}
	}
	return users
}

func tail(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > 300 {
		s = s[len(s)-300:]
	}
	return s
}
