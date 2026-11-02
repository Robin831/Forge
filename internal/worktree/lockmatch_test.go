package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const lockV2 = `{"lockfileVersion":3,"packages":{"":{"name":"client"},
"node_modules/@eslint-react/eslint-plugin":{"version":"%s"},
"node_modules/@esbuild/darwin-arm64":{"version":"0.25.0","optional":true},
"node_modules/local":{"resolved":"../local","link":true}}}`

const hidden = `{"lockfileVersion":3,"packages":{
"node_modules/@eslint-react/eslint-plugin":{"version":"%s"},
"node_modules/left-over":{"version":"1.0.0"}}}`

// setup makes an anvil whose client/node_modules was installed at installed, and a worktree whose
// client/package-lock.json locks locked (DevInfra-rjb.3).
func setup(t *testing.T, installed, locked string) (anvil, wt string) {
	t.Helper()
	anvil, wt = t.TempDir(), t.TempDir()
	nm := filepath.Join(anvil, "client", "node_modules")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(nm, ".package-lock.json"), sprintf(hidden, installed))
	write(t, filepath.Join(nm, "marker.txt"), "anvil")
	if err := os.MkdirAll(filepath.Join(wt, "client"), 0o755); err != nil {
		t.Fatal(err)
	}
	if locked != "" {
		write(t, filepath.Join(wt, "client", "package-lock.json"), sprintf(lockV2, locked))
	}
	return anvil, wt
}

func TestLinkNodeModules_LinksWhenTheInstallMatchesTheLockfile(t *testing.T) {
	anvil, wt := setup(t, "5.28.8", "5.28.8")
	if err := linkNodeModules(anvil, wt); err != nil {
		t.Fatal(err)
	}
	if linked, _ := isReparsePoint(filepath.Join(wt, "client", "node_modules")); !linked {
		t.Error("a matching install (an absent optional package, a link entry and a left-over package included) is shared")
	}
}

func TestLinkNodeModules_DoesNotLinkAStaleInstall(t *testing.T) {
	anvil, wt := setup(t, "5.27.3", "5.28.8")
	if err := linkNodeModules(anvil, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(wt, "client", "node_modules")); !os.IsNotExist(err) {
		t.Errorf("a stale install must not be linked, so npm ci installs in the worktree (err=%v)", err)
	}
}

func TestLinkNodeModules_RemovesAReusedWorktreesStaleLinkButNotItsTarget(t *testing.T) {
	anvil, wt := setup(t, "5.27.3", "5.27.3")
	if err := linkNodeModules(anvil, wt); err != nil {
		t.Fatal(err)
	}
	// The branch moves the lockfile on; the worktree is reused.
	write(t, filepath.Join(wt, "client", "package-lock.json"), sprintf(lockV2, "5.28.8"))
	if err := linkNodeModules(anvil, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(wt, "client", "node_modules")); !os.IsNotExist(err) {
		t.Errorf("the stale link must go (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(anvil, "client", "node_modules", "marker.txt")); err != nil {
		t.Errorf("the anvil's own tree must stay: %v", err)
	}
}

func TestLinkNodeModules_LeavesAWorktreesOwnDirectoryAlone(t *testing.T) {
	anvil, wt := setup(t, "5.27.3", "5.28.8")
	own := filepath.Join(wt, "client", "node_modules")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(own, "mine.txt"), "worktree")
	if err := linkNodeModules(anvil, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(own, "mine.txt")); err != nil {
		t.Errorf("the worktree's own install must stay: %v", err)
	}
}

func TestLinkNodeModules_LinksAsBeforeWhenItCannotTell(t *testing.T) {
	anvil, wt := setup(t, "5.27.3", "")
	if err := linkNodeModules(anvil, wt); err != nil {
		t.Fatal(err)
	}
	if linked, _ := isReparsePoint(filepath.Join(wt, "client", "node_modules")); !linked {
		t.Error("without a lockfile to compare, the old sharing stays")
	}
}

func TestInstalledMatchesLock(t *testing.T) {
	anvil, wt := setup(t, "5.27.3", "5.28.8")
	match, known, why := installedMatchesLock(filepath.Join(anvil, "client", "node_modules"), filepath.Join(wt, "client", "package-lock.json"))
	if match || !known || why == "" {
		t.Errorf("match=%v known=%v why=%q", match, known, why)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sprintf(format, v string) string { return fmt.Sprintf(format, v) }

// A preview has no install step: it keeps sharing even a stale install rather than start without node_modules.
func TestLinkNodeModules_PreviewSharesAStaleInstall(t *testing.T) {
	anvil, wt := setup(t, "5.27.3", "5.28.8")
	if err := linkNodeModulesWith(anvil, wt, false); err != nil {
		t.Fatal(err)
	}
	if linked, _ := isReparsePoint(filepath.Join(wt, "client", "node_modules")); !linked {
		t.Error("a preview must still get the shared tree")
	}
}
