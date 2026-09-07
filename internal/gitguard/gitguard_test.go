package gitguard

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestRender_SubstitutesTheRealGitPath(t *testing.T) {
	script, err := render(filepath.FromSlash("/usr/bin/git"))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(script, realGitPlaceholder) {
		t.Error("the placeholder survived rendering")
	}
	if !strings.Contains(script, "REAL_GIT='/usr/bin/git'") {
		t.Error("the rendered script does not name the real git")
	}
}

// A Windows path reaches the script as forward slashes: it runs under Git
// Bash's sh, where a backslash inside single quotes stays a literal backslash
// rather than a path separator.
func TestRender_UsesForwardSlashes(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows produces a backslash path")
	}
	script, err := render(`C:\Program Files\Git\cmd\git.exe`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(script, "REAL_GIT='C:/Program Files/Git/cmd/git.exe'") {
		t.Error("the Windows path was not converted to forward slashes")
	}
}

// A quote in the path would end the script's own quoting and change what it
// runs, so the guard declines to install rather than inventing an escaping rule.
func TestRender_RefusesAQuotedPath(t *testing.T) {
	if _, err := render("/opt/o'brien/git"); err == nil {
		t.Error("a path containing a single quote must not be embedded")
	}
}

func TestInstall_WritesARunnableScript(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)

	dir, err := Install()
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if dir != filepath.Join(home, ".forge", "gitguard") {
		t.Errorf("guard installed at %q", dir)
	}
	info, err := os.Stat(filepath.Join(dir, "git"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Errorf("mode %v is not executable", info.Mode().Perm())
	}
}

// A second spawn must not rewrite a file another worker may be executing.
func TestInstall_IsIdempotent(t *testing.T) {
	setHome(t, t.TempDir())

	dir, err := Install()
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	path := filepath.Join(dir, "git")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if _, err := Install(); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("an unchanged script was rewritten")
	}
	// Nothing is left behind for the next spawn to resolve as `git`.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("guard directory holds %d entries, want only the script", len(entries))
	}
}

// Workers spawn concurrently and share one guard directory, so the install has
// to survive several at once without leaving a partial file where the next
// spawn will resolve `git`.
func TestInstall_SurvivesConcurrentSpawns(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)

	var wg sync.WaitGroup
	errs := make([]error, 32)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = Install()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Install %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(home, ".forge", "gitguard"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("guard directory holds %d entries, want only the script", len(entries))
	}
}

func TestInstall_DisabledInstallsNothing(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv(DisableEnv, "1")

	dir, err := Install()
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if dir != "" {
		t.Errorf("got %q, want the guard switched off", dir)
	}
	if _, err := os.Stat(filepath.Join(home, ".forge", "gitguard")); !os.IsNotExist(err) {
		t.Error("a disabled guard still wrote its directory")
	}
}

// "0" reads as an operator turning the guard OFF at a glance, and must not turn
// it on.
func TestSwitchedOff(t *testing.T) {
	for _, on := range []string{"", "0", "false", "no", "off", "  FALSE  "} {
		if switchedOff(on) {
			t.Errorf("%q switched the guard off", on)
		}
	}
	for _, off := range []string{"1", "true", "yes", "anything"} {
		if !switchedOff(off) {
			t.Errorf("%q left the guard on", off)
		}
	}
}

func TestEnv_PutsTheGuardFirstOnPath(t *testing.T) {
	sep := string(os.PathListSeparator)
	got := Env([]string{"PATH=/usr/bin" + sep + "/bin", "HOME=/home/forge"}, "/guard", "/anvil/.git")

	wantPath := "PATH=/guard" + sep + "/usr/bin" + sep + "/bin"
	if got[0] != wantPath {
		t.Errorf("got %q, want %q", got[0], wantPath)
	}
	if !contains(got, GuardedGitDirEnv+"=/anvil/.git") {
		t.Error("the anvil's git directory was not passed to the guard")
	}
}

// An anvil that could not be resolved leaves the guard on its linked-worktree
// rule rather than carrying an empty path it would compare against.
func TestEnv_OmitsAnUnresolvedAnvil(t *testing.T) {
	for _, e := range Env([]string{"PATH=/usr/bin"}, "/guard", "") {
		if strings.HasPrefix(e, GuardedGitDirEnv+"=") {
			t.Errorf("%s was set with no anvil to name", GuardedGitDirEnv)
		}
	}
}

func TestEnv_AddsAPathWhenThereIsNone(t *testing.T) {
	got := Env([]string{"HOME=/home/forge"}, "/guard", "")
	if !contains(got, "PATH=/guard") {
		t.Errorf("got %v, want a PATH entry", got)
	}
}

// An empty PATH must not become a bare separator, which some shells read as
// "the current directory".
func TestEnv_DoesNotAppendToAnEmptyPath(t *testing.T) {
	got := Env([]string{"PATH="}, "/guard", "")
	if got[0] != "PATH=/guard" {
		t.Errorf("got %q, want %q", got[0], "PATH=/guard")
	}
}

// os.Environ reports "Path" on Windows, where environment names are
// case-insensitive: matched exactly, the guard would be appended as a second,
// ignored variable and never reached.
func TestEnv_MatchesWindowsPathCasing(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PATH casing only collapses on Windows")
	}
	got := Env([]string{"Path=C:\\bin"}, "C:\\guard", "")
	want := "Path=C:\\guard" + string(os.PathListSeparator) + "C:\\bin"
	if got[0] != want {
		t.Errorf("got %q, want %q", got[0], want)
	}
	if len(got) != 1 {
		t.Errorf("got %d entries, want the one PATH rewritten in place", len(got))
	}
}

func TestEnv_WithNoGuardIsUnchanged(t *testing.T) {
	in := []string{"PATH=/usr/bin"}
	got := Env(in, "", "/anvil/.git")
	if len(got) != 1 || got[0] != in[0] {
		t.Errorf("got %v, want the environment untouched", got)
	}
}

func TestAnvilGitDir_NamesTheSharedRepository(t *testing.T) {
	fixture := newAnvil(t)

	got := AnvilGitDir(context.Background(), fixture.worker)
	if got == "" {
		t.Fatal("no anvil git directory resolved from a worker worktree")
	}
	if !samePath(got, filepath.Join(fixture.anvil, ".git")) {
		t.Errorf("got %q, want the anvil's .git (%q)", got, filepath.Join(fixture.anvil, ".git"))
	}
}

func TestAnvilGitDir_UnresolvableIsEmpty(t *testing.T) {
	if got := AnvilGitDir(context.Background(), t.TempDir()); got != "" {
		t.Errorf("got %q, want empty for a directory that is not a repository", got)
	}
	if got := AnvilGitDir(context.Background(), ""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// setHome points os.UserHomeDir at a temporary directory. It reads a different
// variable per platform, and setting only one of them leaves the test writing
// into the developer's real ~/.forge.
func setHome(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", dir)
		return
	}
	t.Setenv("HOME", dir)
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool {
	ra, erra := filepath.EvalSymlinks(a)
	rb, errb := filepath.EvalSymlinks(b)
	if erra != nil || errb != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return filepath.Clean(ra) == filepath.Clean(rb)
}
