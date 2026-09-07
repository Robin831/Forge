package gitguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/executil"
)

// The guard is a POSIX script because that is what every provider's shell
// resolves `git` through. These tests run it under that shell — Git Bash's on
// Windows — so the behaviour asserted here is the pod's behaviour, not a Go
// re-implementation of it.

const denial = "forge: refusing to rewrite `origin`"

type anvilFixture struct {
	anvil  string // the main checkout, as a Forge anvil is
	worker string // a linked worktree under .workers/, as a Forge worker is
	clone  string // a repository of its own, which owns its config
	script string // the rendered guard
}

// TestTheFaultTheGuardExistsFor is the measurement the guard is built on: a
// remote written from inside a linked worktree lands on the SHARED config of
// the repository behind it. No GIT_DIR is set here — the write is a property of
// `git worktree`, not of the environment Forge exports into a worker.
func TestTheFaultTheGuardExistsFor(t *testing.T) {
	f := newAnvil(t)

	runGit(t, f.worker, "remote", "set-url", "origin", "/somewhere/else")

	if got := anvilOrigin(t, f.anvil); got != "/somewhere/else" {
		t.Fatalf("anvil origin is %q — the fault this guard prevents did not reproduce", got)
	}
}

func TestGuard_RefusesOriginWritesFromAWorker(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"remote set-url", []string{"remote", "set-url", "origin", "/tmp/bad"}},
		{"remote add", []string{"remote", "add", "origin", "/tmp/bad"}},
		{"remote rename", []string{"remote", "rename", "origin", "upstream"}},
		{"remote remove", []string{"remote", "remove", "origin"}},
		{"remote rm", []string{"remote", "rm", "origin"}},
		{"remote set-url --push", []string{"remote", "set-url", "--push", "origin", "/tmp/bad"}},
		{"config key value", []string{"config", "remote.origin.url", "/tmp/bad"}},
		{"config --unset", []string{"config", "--unset", "remote.origin.url"}},
		{"config --replace-all", []string{"config", "--replace-all", "remote.origin.url", "/tmp/bad"}},
		{"config pushurl", []string{"config", "remote.origin.pushurl", "/tmp/bad"}},
		// Git folds a config key's section and variable to lower case and
		// leaves the subsection alone, so these are the same key it resolves
		// for `remote.origin.url` — a literal comparison let them through.
		{"config mixed-case variable", []string{"config", "remote.origin.URL", "/tmp/bad"}},
		{"config mixed-case section", []string{"config", "Remote.origin.url", "/tmp/bad"}},
		{"config shouting", []string{"config", "REMOTE.origin.PUSHURL", "/tmp/bad"}},
		{"config set mixed-case", []string{"config", "set", "remote.origin.Url", "/tmp/bad"}},
		// git 2.46's subcommand spelling reaches the same file as the old one.
		{"config set", []string{"config", "set", "remote.origin.url", "/tmp/bad"}},
		{"config unset", []string{"config", "unset", "remote.origin.url"}},
		// Global options in front of the subcommand must not hide it.
		{"-c in front", []string{"-c", "core.pager=cat", "remote", "set-url", "origin", "/tmp/bad"}},
		{"--no-pager in front", []string{"--no-pager", "remote", "set-url", "origin", "/tmp/bad"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAnvil(t)
			before := anvilOrigin(t, f.anvil)

			out, err := f.run(t, f.worker, nil, tc.args...)
			if err == nil {
				t.Fatalf("git %s was allowed", strings.Join(tc.args, " "))
			}
			if !strings.Contains(out, denial) {
				t.Errorf("output does not explain the refusal:\n%s", out)
			}
			if after := anvilOrigin(t, f.anvil); after != before {
				t.Errorf("the anvil's origin changed to %q", after)
			}
		})
	}
}

// The guard denies one command shape. Everything a worker legitimately does —
// including every read of the very remote it protects — passes through.
func TestGuard_AllowsEverythingElseFromAWorker(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"remote -v", []string{"remote", "-v"}},
		{"remote get-url", []string{"remote", "get-url", "origin"}},
		{"remote show", []string{"remote", "show", "-n", "origin"}},
		{"config --get", []string{"config", "--get", "remote.origin.url"}},
		{"config read", []string{"config", "remote.origin.url"}},
		// A fork remote under another name is what `gh pr checkout` adds, and
		// no other remote's URL is one Forge resolves.
		{"remote add upstream", []string{"remote", "add", "upstream", "https://example.com/fork.git"}},
		{"config unrelated key", []string{"config", "user.email", "worker@example.com"}},
		// The remote NAME is a config subsection, which git compares
		// case-sensitively — `ORIGIN` is a different remote, and not one
		// Forge resolves.
		{"config other-cased remote name", []string{"config", "remote.ORIGIN.url", "/tmp/whatever"}},
		// The refspec under `origin` is the bootstrap's to widen, and does not
		// decide where a fetch goes.
		{"config origin fetch refspec", []string{"config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"}},
		{"config --global origin", []string{"config", "--global", "remote.origin.url", "/tmp/whatever"}},
		{"status", []string{"status", "--porcelain"}},
		{"rev-parse", []string{"rev-parse", "--abbrev-ref", "HEAD"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAnvil(t)
			// A global config write must not touch the developer's own.
			env := []string{"GIT_CONFIG_GLOBAL=" + filepath.Join(t.TempDir(), "gitconfig")}

			if out, err := f.run(t, f.worker, env, tc.args...); err != nil {
				t.Fatalf("git %s was refused:\n%s", strings.Join(tc.args, " "), out)
			}
		})
	}
}

// The guard is transparent: what comes back is the real git's answer, not the
// script's.
func TestGuard_PassesGitsOwnOutputThrough(t *testing.T) {
	f := newAnvil(t)

	out, err := f.run(t, f.worker, nil, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "forge/bead" {
		t.Errorf("got %q, want the worker's branch", strings.TrimSpace(out))
	}
}

// A repository that owns its config is the agent's to repoint. This is the case
// the guard must not break: a scratch clone is what an agent is told to use
// when it needs an upstream of its own.
func TestGuard_AllowsAClonesOwnOrigin(t *testing.T) {
	f := newAnvil(t)

	if out, err := f.run(t, f.clone, nil, "remote", "set-url", "origin", "https://example.com/other.git"); err != nil {
		t.Fatalf("a clone's own origin was refused:\n%s", out)
	}
}

// The refusal prints a way out, and the way out has to work. It says to unset
// GIT_DIR and GIT_WORK_TREE FIRST, which is the half that is easy to leave out:
// Forge exports them into a worker, they retarget git from any directory, and a
// clone taken without dropping them is refused again from inside itself.
func TestGuard_TheRemedyItPrintsWorks(t *testing.T) {
	f := newAnvil(t)
	worker := []string{
		"GIT_DIR=" + filepath.Join(f.anvil, ".git", "worktrees", "bead"),
		"GIT_WORK_TREE=" + f.worker,
	}
	scratch := filepath.Join(t.TempDir(), "scratch")

	// The clone itself is taken with the variables dropped, as the message says.
	if out, err := f.run(t, f.worker, nil, "clone", "--quiet", f.worker, scratch); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	// Still inside a worker's environment, but the variables are gone, so the
	// clone is what git and the guard both see.
	if out, err := f.run(t, scratch, nil, "remote", "set-url", "origin", "https://example.com/mine.git"); err != nil {
		t.Fatalf("the remedy the guard prints does not work:\n%s", out)
	}

	// And the trap the message warns about: keep the variables and the write is
	// refused from inside the clone, because git never looked at the clone.
	if _, err := f.run(t, scratch, worker, "remote", "set-url", "origin", "https://example.com/mine.git"); err == nil {
		t.Error("a write with GIT_DIR still set reached the clone — the warning in the message is wrong")
	}
}

// The main checkout owns its config too, so the rule that catches a worker does
// not catch it. This is what keeps the deployment's bootstrap and the
// operator's `git -C <anvil> remote set-url origin` working — neither has the
// guard on its PATH, and neither would be stopped if it did.
func TestGuard_AllowsTheAnvilsOwnOriginWhenNoAnvilIsNamed(t *testing.T) {
	f := newAnvil(t)

	if out, err := f.run(t, f.anvil, nil, "remote", "set-url", "origin", "https://example.com/real.git"); err != nil {
		t.Fatalf("the main checkout's own origin was refused:\n%s", out)
	}
}

// Naming the anvil is the write the worktree rule cannot see: the anvil is a
// main checkout, so it owns its config, and only knowing which repository is
// the anvil separates it from the scratch clone above.
func TestGuard_RefusesTheAnvilNamedOutright(t *testing.T) {
	f := newAnvil(t)
	guarded := []string{GuardedGitDirEnv + "=" + AnvilGitDir(t.Context(), f.worker)}
	before := anvilOrigin(t, f.anvil)

	out, err := f.run(t, f.worker, guarded, "-C", f.anvil, "remote", "set-url", "origin", "/tmp/bad")
	if err == nil {
		t.Fatal("`git -C <anvil> remote set-url origin` was allowed")
	}
	if !strings.Contains(out, denial) {
		t.Errorf("output does not explain the refusal:\n%s", out)
	}
	if after := anvilOrigin(t, f.anvil); after != before {
		t.Errorf("the anvil's origin changed to %q", after)
	}
}

// A clone is not the anvil, so naming the anvil changes nothing for it.
func TestGuard_AllowsACloneWhileTheAnvilIsNamed(t *testing.T) {
	f := newAnvil(t)
	guarded := []string{GuardedGitDirEnv + "=" + AnvilGitDir(t.Context(), f.worker)}

	if out, err := f.run(t, f.clone, guarded, "remote", "set-url", "origin", "https://example.com/other.git"); err != nil {
		t.Fatalf("a clone's own origin was refused while the anvil was named:\n%s", out)
	}
}

// Forge exports GIT_DIR and GIT_WORK_TREE into a worker so a stray `cd ..`
// cannot commit to the anvil's main branch. They retarget a command run from
// anywhere at all, including outside the anvil, and the guard has to see
// through them.
func TestGuard_RefusesThroughAnExportedGitDir(t *testing.T) {
	f := newAnvil(t)
	elsewhere := t.TempDir()
	env := []string{
		"GIT_DIR=" + filepath.Join(f.anvil, ".git", "worktrees", "bead"),
		"GIT_WORK_TREE=" + f.worker,
	}
	before := anvilOrigin(t, f.anvil)

	out, err := f.run(t, elsewhere, env, "remote", "set-url", "origin", "/tmp/bad")
	if err == nil {
		t.Fatal("a write retargeted at the worker by GIT_DIR was allowed")
	}
	if !strings.Contains(out, denial) {
		t.Errorf("output does not explain the refusal:\n%s", out)
	}
	if after := anvilOrigin(t, f.anvil); after != before {
		t.Errorf("the anvil's origin changed to %q", after)
	}
}

// Outside a repository the guard has nothing to compare, and a guard that
// cannot tell what it is looking at defers to git.
func TestGuard_OutsideARepositoryDefersToGit(t *testing.T) {
	f := newAnvil(t)

	out, err := f.run(t, t.TempDir(), nil, "remote", "set-url", "origin", "/tmp/bad")
	if err == nil {
		t.Fatal("git itself should have failed here")
	}
	if strings.Contains(out, denial) {
		t.Errorf("the guard answered for a directory that is not a repository:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "not a git repository") {
		t.Errorf("git's own error did not reach the caller:\n%s", out)
	}
}

// run executes the guard under sh, the way an agent's shell reaches it.
func (f anvilFixture) run(t *testing.T, dir string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(shPath(t), append([]string{f.script}, args...)...)
	cmd.Dir = dir
	// CleanGitEnv, not os.Environ: this test binary is itself routinely run
	// inside a Forge worker, whose inherited GIT_DIR would retarget every
	// fixture command at the anvil under test.
	cmd.Env = append(executil.CleanGitEnv(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// newAnvil builds the shape the guard reasons about: a main checkout with a
// linked worktree under .workers/, plus a clone that owns its own config.
func newAnvil(t *testing.T) anvilFixture {
	t.Helper()
	requireGit(t)

	root := t.TempDir()
	anvil := filepath.Join(root, "anvil")
	if err := os.MkdirAll(anvil, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	runGit(t, anvil, "init", "--initial-branch=main", ".")
	runGit(t, anvil, "config", "user.email", "forge@example.com")
	runGit(t, anvil, "config", "user.name", "Forge")
	if err := os.WriteFile(filepath.Join(anvil, "file.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGit(t, anvil, "add", "file.txt")
	runGit(t, anvil, "commit", "-m", "initial")
	runGit(t, anvil, "remote", "add", "origin", "https://example.com/real.git")

	worker := filepath.Join(anvil, ".workers", "bead")
	runGit(t, anvil, "worktree", "add", "-b", "forge/bead", worker)

	clone := filepath.Join(root, "clone")
	runGit(t, root, "clone", "--quiet", anvil, clone)

	return anvilFixture{anvil: anvil, worker: worker, clone: clone, script: installTestScript(t)}
}

// installTestScript renders the embedded shim against the real git, the way
// Install does, without writing into the developer's home directory.
func installTestScript(t *testing.T) string {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git is not available: %v", err)
	}
	script, err := render(gitPath)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	path := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func anvilOrigin(t *testing.T, anvil string) string {
	t.Helper()
	out := runGit(t, anvil, "config", "--file", filepath.Join(anvil, ".git", "config"), "--get", "remote.origin.url")
	return strings.TrimSpace(out)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = executil.CleanGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not available: %v", err)
	}
}

// shPath finds the POSIX shell the guard runs under. On Windows that is Git
// Bash's, which ships with the git the guard wraps.
func shPath(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("sh"); err == nil {
		return path
	}
	gitPath, err := exec.LookPath("git")
	if err == nil {
		// <install>/cmd/git.exe → <install>/usr/bin/sh.exe
		candidate := filepath.Join(filepath.Dir(filepath.Dir(gitPath)), "usr", "bin", "sh.exe")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Skip("no POSIX shell available to run the guard")
	return ""
}
