package worktree

import (
	"bufio"
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
		touchLock(anvilPath)
		refreshSharedInstall(ctx, anvilPath, sub)
	}
}

const (
	syncLockName   = ".forge-sync.lock"
	syncLockStale  = 30 * time.Minute
	installTimeout = 10 * time.Minute
	syncDeadline   = 25 * time.Minute
	failureBackoff = 30 * time.Minute
)

// lockAnvil takes the anvil for one sync across processes (a lock file made with O_EXCL); a lock older than
// syncLockStale is from a sync that died and is taken over. It never waits.
func lockAnvil(anvilPath string) (func(), bool) {
	path := filepath.Join(gitDir(anvilPath), syncLockName)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }, true
		}
		if !errors.Is(err, os.ErrExist) {
			slog.Warn("anvil sync: cannot take the sync lock; not syncing", "anvil", anvilPath, "error", err)
			return nil, false
		}
		info, statErr := os.Stat(path)
		if statErr != nil || time.Since(info.ModTime()) < syncLockStale {
			return nil, false
		}
		_ = os.Remove(path)
	}
	return nil, false
}

// syncInProgress reports a live sync lock on the anvil (one older than syncLockStale is from a sync that died).
func syncInProgress(anvilPath string) bool {
	info, err := os.Stat(filepath.Join(gitDir(anvilPath), syncLockName))
	return err == nil && time.Since(info.ModTime()) < syncLockStale
}

// touchLock marks the sync as alive, so its lock never looks stale while it runs.
func touchLock(anvilPath string) {
	now := time.Now()
	_ = os.Chtimes(filepath.Join(gitDir(anvilPath), syncLockName), now, now)
}

// gitDir is git's own directory for the anvil, where the lock lives (never tracked, never in a worktree); .git can be
// a file pointing elsewhere (a submodule or a worktree anvil).
func gitDir(anvilPath string) string {
	out, err := exec.Command("git", "-C", anvilPath, "rev-parse", "--git-common-dir").Output()
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

// npmCommand runs npm ci in dir; tests replace it. WaitDelay: a postinstall child holding the output pipe must not
// keep the call past its timeout.
var npmCommand = func(ctx context.Context, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "npm", "ci", "--prefer-offline", "--no-audit", "--no-fund")
	cmd.Dir = dir
	cmd.WaitDelay = 10 * time.Second
	return cmd.CombinedOutput()
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
	if gitCmdOut(ctx, anvilPath, &list, "worktree", "list", "--porcelain") != nil {
		return []string{"(git worktree list failed)"} // cannot tell: treat as in use
	}
	var users []string
	sc := bufio.NewScanner(&list)
	for sc.Scan() {
		path, ok := strings.CutPrefix(sc.Text(), "worktree ")
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
