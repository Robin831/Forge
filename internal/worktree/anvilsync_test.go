package worktree

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// origin, an anvil cloned from it, and a second clone that pushes to it.
func gitTrio(t *testing.T) (anvil, other string) {
	t.Helper()
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	run(t, base, "init", "--bare", "-b", "main", origin)
	seed := filepath.Join(base, "seed")
	run(t, base, "clone", origin, seed)
	writeLockFile(t, filepath.Join(seed, "a.txt"), "1")
	run(t, seed, "add", ".")
	run(t, seed, "commit", "-m", "one")
	run(t, seed, "push", "origin", "HEAD:main")
	anvil, other = filepath.Join(base, "anvil"), filepath.Join(base, "other")
	run(t, base, "clone", origin, anvil)
	run(t, base, "clone", origin, other)
	return anvil, other
}

func push(t *testing.T, other, name string) {
	writeLockFile(t, filepath.Join(other, name), name)
	run(t, other, "add", ".")
	run(t, other, "commit", "-m", name)
	run(t, other, "push", "origin", "HEAD:main")
}

func TestFastForwardAnvil(t *testing.T) {
	ctx := context.Background()
	anvil, other := gitTrio(t)
	push(t, other, "b.txt")
	run(t, anvil, "fetch", "origin")
	fastForwardAnvil(ctx, anvil)
	if run(t, anvil, "rev-parse", "HEAD") != run(t, anvil, "rev-parse", "origin/main") {
		t.Fatal("a clean anvil behind origin is fast-forwarded")
	}
	push(t, other, "c.txt")
	run(t, anvil, "fetch", "origin")
	writeLockFile(t, filepath.Join(anvil, "a.txt"), "local edit")
	before := run(t, anvil, "rev-parse", "HEAD")
	fastForwardAnvil(ctx, anvil)
	if run(t, anvil, "rev-parse", "HEAD") != before {
		t.Error("a dirty anvil is left as it is")
	}
	run(t, anvil, "checkout", "--", "a.txt")
	writeLockFile(t, filepath.Join(anvil, "mine.txt"), "x")
	run(t, anvil, "add", "mine.txt")
	run(t, anvil, "commit", "-m", "local")
	diverged := run(t, anvil, "rev-parse", "HEAD")
	fastForwardAnvil(ctx, anvil)
	if run(t, anvil, "rev-parse", "HEAD") != diverged {
		t.Error("a diverged anvil is left as it is")
	}
}

// fakeNpm "installs" by writing npm's hidden lockfile from the folder's package-lock.json; fail makes it fail.
func fakeNpm(t *testing.T, calls *int, fail *bool) {
	t.Helper()
	orig := npmCommand
	t.Cleanup(func() { npmCommand = orig })
	npmCommand = func(_ context.Context, dir string) ([]byte, error) {
		*calls++
		if fail != nil && *fail {
			return []byte("npm ERR! boom"), fmt.Errorf("exit status 1")
		}
		raw, err := os.ReadFile(filepath.Join(dir, "package-lock.json"))
		if err != nil {
			return nil, err
		}
		var l map[string]any
		_ = json.Unmarshal(raw, &l)
		pk := l["packages"].(map[string]any)
		delete(pk, "")
		out, _ := json.Marshal(map[string]any{"packages": pk})
		return nil, os.WriteFile(filepath.Join(dir, "node_modules", ".package-lock.json"), out, 0o644)
	}
}

// A git anvil with a client project whose shared install is at installed while its lockfile locks locked.
func gitAnvilProject(t *testing.T, installed, locked string) string {
	t.Helper()
	anvil, _ := gitTrio(t)
	nm := filepath.Join(anvil, "client", "node_modules")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLockFile(t, filepath.Join(nm, ".package-lock.json"), fmt.Sprintf(`{"packages":{"node_modules/a":{"version":"%s"}}}`, installed))
	writeLockFile(t, filepath.Join(anvil, "client", "package-lock.json"), fmt.Sprintf(`{"packages":{"":{},"node_modules/a":{"version":"%s"}}}`, locked))
	return anvil
}

func installedVersion(t *testing.T, anvil string) string {
	raw, _ := os.ReadFile(filepath.Join(anvil, "client", "node_modules", ".package-lock.json"))
	return string(raw)
}

func TestRefreshSharedInstallRunsInPlaceWhenNothingUsesIt(t *testing.T) {
	calls := 0
	fakeNpm(t, &calls, nil)
	anvil := gitAnvilProject(t, "1.0.0", "2.0.0")
	refreshSharedInstall(context.Background(), anvil, "client")
	if calls != 1 || !strings.Contains(installedVersion(t, anvil), "2.0.0") {
		t.Fatalf("calls=%d installed=%s", calls, installedVersion(t, anvil))
	}
	refreshSharedInstall(context.Background(), anvil, "client")
	if calls != 1 {
		t.Error("a matching install is not installed again")
	}
}

func TestRefreshSharedInstallLeavesAnInstallAWorktreeOrPreviewLinksTo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	calls := 0
	fakeNpm(t, &calls, nil)
	anvil := gitAnvilProject(t, "1.0.0", "2.0.0")
	// A worktree anywhere git knows it (a preview lives outside .workers) linking the shared install.
	wt := filepath.Join(t.TempDir(), "preview-1")
	run(t, anvil, "worktree", "add", "--detach", wt)
	if err := os.MkdirAll(filepath.Join(wt, "client"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(anvil, "client", "node_modules"), filepath.Join(wt, "client", "node_modules")); err != nil {
		t.Fatal(err)
	}
	refreshSharedInstall(context.Background(), anvil, "client")
	if calls != 0 || !strings.Contains(installedVersion(t, anvil), "1.0.0") {
		t.Errorf("an install in use is not touched: calls=%d", calls)
	}
}

func TestRefreshSharedInstallBacksOffAfterAFailure(t *testing.T) {
	calls, fail := 0, true
	fakeNpm(t, &calls, &fail)
	anvil := gitAnvilProject(t, "1.0.0", "2.0.0")
	refreshSharedInstall(context.Background(), anvil, "client")
	refreshSharedInstall(context.Background(), anvil, "client")
	if calls != 1 {
		t.Errorf("a failed install is not retried within the back-off: calls=%d", calls)
	}
	writeLockFile(t, filepath.Join(anvil, "client", "package-lock.json"), `{"packages":{"":{},"node_modules/a":{"version":"3.0.0"}}}`)
	fail = false
	refreshSharedInstall(context.Background(), anvil, "client")
	if calls != 2 || !strings.Contains(installedVersion(t, anvil), "3.0.0") {
		t.Errorf("a new lockfile is tried at once: calls=%d", calls)
	}
}

func TestSyncAnvilSkipsWhenAnotherSyncHoldsIt(t *testing.T) {
	calls := 0
	fakeNpm(t, &calls, nil)
	anvil := gitAnvilProject(t, "1.0.0", "2.0.0")
	unlock, ok := lockAnvil(anvil)
	if !ok {
		t.Fatal("first lock")
	}
	syncAnvil(context.Background(), anvil)
	if calls != 0 {
		t.Error("a held anvil is skipped, never waited on")
	}
	unlock()
	syncAnvil(context.Background(), anvil)
	if calls != 1 {
		t.Errorf("free again, it syncs: calls=%d", calls)
	}
}

func TestLockAnvilTakesOverAStaleLock(t *testing.T) {
	anvil, _ := gitTrio(t)
	path := filepath.Join(anvil, ".git", syncLockName)
	writeLockFile(t, path, "")
	old := time.Now().Add(-2 * syncLockStale)
	_ = os.Chtimes(path, old, old)
	unlock, ok := lockAnvil(anvil)
	if !ok {
		t.Fatal("a lock from a dead sync is taken over")
	}
	unlock()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("unlock removes the lock")
	}
}

// While a sync refreshes the anvil's install, a worktree made meanwhile does not link the half-built tree.
func TestLinkNodeModulesDoesNotLinkDuringASync(t *testing.T) {
	anvil := gitAnvilProject(t, "2.0.0", "2.0.0")
	wt := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(filepath.Join(wt, "client"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeLockFile(t, filepath.Join(wt, "client", "package-lock.json"), `{"packages":{"":{},"node_modules/a":{"version":"2.0.0"}}}`)
	unlock, _ := lockAnvil(anvil)
	if err := linkNodeModules(anvil, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(wt, "client", "node_modules")); !os.IsNotExist(err) {
		t.Error("no link while the anvil is being refreshed")
	}
	unlock()
	if err := linkNodeModules(anvil, wt); err != nil {
		t.Fatal(err)
	}
	if linked, _ := isReparsePoint(filepath.Join(wt, "client", "node_modules")); !linked {
		t.Error("linked once the sync is done")
	}
}

// A preview has no install step, but it too stays off a tree a sync is rebuilding (round 2 of the review).
func TestPreviewLinkSkipsDuringASync(t *testing.T) {
	anvil := gitAnvilProject(t, "2.0.0", "2.0.0")
	wt := filepath.Join(t.TempDir(), "preview")
	if err := os.MkdirAll(filepath.Join(wt, "client"), 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, _ := lockAnvil(anvil)
	defer unlock()
	if err := linkNodeModulesWith(anvil, wt, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(wt, "client", "node_modules")); !os.IsNotExist(err) {
		t.Error("a preview does not link a tree being rebuilt")
	}
}

func TestGitDirFollowsAGitFile(t *testing.T) {
	anvil, _ := gitTrio(t)
	wt := filepath.Join(t.TempDir(), "linked")
	run(t, anvil, "worktree", "add", "--detach", wt)
	if got, want := gitDir(wt), filepath.Join(anvil, ".git"); filepath.Clean(got) != filepath.Clean(want) {
		t.Errorf("gitDir(%s) = %s, want the common dir %s", wt, got, want)
	}
}
