// Package gitguard keeps a worker from repointing its anvil's `origin`.
//
// # The fault
//
// Git stores remotes in the config file of the repository a worktree belongs
// to, never in the worktree. A Forge worker is a linked worktree under the
// anvil's `.workers/`, so `git remote set-url origin <path>` typed inside one
// does not write anything the worker owns: it writes `<anvil>/.git/config`, for
// the daemon and for every other worker, and the value survives the worktree
// being deleted at teardown. Measured directly (Linux, git 2.43; Windows, git
// 2.55): the write lands on the anvil with no GIT_DIR set at all, so it is a
// property of `git worktree` rather than of the environment Forge exports.
//
// An anvil found that way — `origin` pointing at `<anvil>/.workers/<bead>/.git`
// after that worker finished — spent a day with every fetch, dependency scan
// and PR reconcile failing against a directory that no longer existed.
//
// # Why a wrapper on PATH
//
// Nothing inside git prevents this. `GIT_DIR` confines refs, objects and the
// index and never config; `GIT_COMMON_DIR` is where the config the worker must
// read lives; a value pinned in the anvil's `config.worktree` does not shadow a
// corrupted one, because git accumulates `remote.origin.url` across config
// files as a LIST rather than overriding (measured: `git remote -v` then
// reports the corrupted URL for fetch and both for push). Prevention therefore
// has to stop the command, and the only interception point that holds for every
// provider Forge can spawn — none of which share a permission model — is the
// PATH the agent's shell resolves `git` on.
//
// So the guard is a small POSIX script installed ahead of git for the agent
// process alone. The daemon's own git calls, the deployment's bootstrap and the
// operator's repair command never see it.
//
// # What it costs when it is wrong
//
// A guard that blocks real work is worse than the fault it prevents, so the
// script fails open at every step it cannot answer, and denies one command
// shape rather than a category: writes to the remote named `origin`, the one
// every Forge fetch resolves. Reads pass. Other remote names pass — `gh pr
// checkout` of a fork adds one. Every non-`remote`, non-`config` invocation
// leaves the script without a probe having been run, which is the whole of a
// worker's git traffic. And the guard is a guardrail, not a sandbox: an
// absolute path to git walks past it, which is the correct depth for something
// standing between an agent and a tool it uses hundreds of times a session.
package gitguard

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

//go:embed shim.sh
var shimSource string

// realGitPlaceholder is replaced with the absolute path of the real git when
// the script is installed. The script must not resolve `git` from PATH itself:
// the first entry on that PATH is the script.
const realGitPlaceholder = "@@FORGE_REAL_GIT@@"

// GuardedGitDirEnv names the anvil's git directory to the installed script.
// Without it the script still refuses the write that lands on the anvil by
// accident (any linked worktree's), and with it also the one that names the
// anvil outright.
const GuardedGitDirEnv = "FORGE_GUARDED_GIT_DIR"

// DisableEnv switches the guard off for the daemon that reads it, for an
// operator who needs a worker to do something the guard refuses. It is read
// from the DAEMON's environment, so a worker cannot set it for itself.
const DisableEnv = "FORGE_DISABLE_GIT_GUARD"

// Install writes the guard script and returns the directory to put in front of
// a worker's PATH. An empty directory with a nil error means the guard is
// switched off; an error means it could not be installed, and every caller
// treats that as "run the agent with plain git", which is where we were.
func Install() (string, error) {
	if switchedOff(os.Getenv(DisableEnv)) {
		return "", nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("gitguard: resolving home directory: %w", err)
	}
	dir := filepath.Join(home, ".forge", "gitguard")

	gitPath, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("gitguard: locating git: %w", err)
	}
	if abs, err := filepath.Abs(gitPath); err == nil {
		gitPath = abs
	}
	// A git resolved from inside the guard's own directory would be the script,
	// which would then exec itself. It can only happen if the daemon is already
	// running with the guard on its PATH, and the recursion is unbounded, so it
	// is refused rather than repaired.
	if within(dir, gitPath) {
		return "", fmt.Errorf("gitguard: git resolves to the guard itself (%s) — refusing to install a script that would exec itself", gitPath)
	}

	script, err := render(gitPath)
	if err != nil {
		return "", err
	}
	if err := writeScript(dir, script); err != nil {
		return "", err
	}
	return dir, nil
}

// render substitutes the real git path into the script. The path is
// single-quoted in the script, so a path containing a single quote would end
// the quoting and change what the script runs; there is no such path on a
// normal install, and the honest answer to one is to decline rather than to
// invent an escaping rule for a shell the script may not be running under.
func render(gitPath string) (string, error) {
	if strings.ContainsAny(gitPath, "'\n") {
		return "", fmt.Errorf("gitguard: git path %q contains a quote or newline and cannot be embedded safely", gitPath)
	}
	// The script runs under sh — Git Bash's on Windows, where a backslash
	// inside single quotes is a literal backslash and would not survive as a
	// path separator.
	return strings.Replace(shimSource, realGitPlaceholder, filepath.ToSlash(gitPath), 1), nil
}

// installMu serialises the install within this process. Every worker is a
// goroutine of one daemon and they spawn concurrently, so without it two of
// them rename over the same path at the same moment — which on Windows is not
// a lost write but an error: a rename onto a file another handle has open
// fails with "Access is denied", measured by TestInstall_SurvivesConcurrentSpawns.
var installMu sync.Mutex

// writeScript materialises the script at <dir>/git, rewriting it only when its
// content has changed — a rewrite that is not needed is a window in which a
// spawn resolves a half-written file.
func writeScript(dir, script string) error {
	installMu.Lock()
	defer installMu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("gitguard: creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, "git")
	if existing, err := os.ReadFile(path); err == nil && string(existing) == script {
		return nil
	}

	tmp, err := os.CreateTemp(dir, "git-*.tmp")
	if err != nil {
		return fmt.Errorf("gitguard: creating a temporary file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.WriteString(script); err != nil {
		tmp.Close()
		return fmt.Errorf("gitguard: writing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("gitguard: closing %s: %w", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("gitguard: making %s executable: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		// Windows refuses a rename over a file another process has open for
		// execution — a second daemon sharing this home, or a worker's own git
		// call mid-flight. The file already there is a guard, so an identical
		// one failing to replace it changes nothing.
		if runtime.GOOS == "windows" && sameContent(path, script) {
			return nil
		}
		return fmt.Errorf("gitguard: installing %s: %w", path, err)
	}
	return nil
}

func sameContent(path, want string) bool {
	got, err := os.ReadFile(path)
	return err == nil && string(got) == want
}

// AnvilGitDir returns the git directory shared by every worktree of the
// repository worktreePath belongs to — the anvil's `.git` for a worker. It
// returns "" when that cannot be established, which leaves the guard on its
// linked-worktree rule alone rather than on a guess.
func AnvilGitDir(ctx context.Context, worktreePath string) string {
	if worktreePath == "" {
		return ""
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git",
		"rev-parse", "--path-format=absolute", "--git-common-dir"))
	cmd.Dir = worktreePath
	// The daemon may itself have been started inside a repository, and an
	// inherited GIT_DIR would have this answer come back about that one.
	cmd.Env = executil.CleanGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Env returns the environment additions that arm the guard for a worker: the
// guard directory in front of PATH, and the anvil's git directory. Callers hand
// it the environment they have already assembled and use the result verbatim.
func Env(env []string, guardDir, anvilGitDir string) []string {
	if guardDir == "" {
		return env
	}
	out := prependPath(env, guardDir)
	if anvilGitDir != "" {
		out = append(out, GuardedGitDirEnv+"="+anvilGitDir)
	}
	return out
}

// prependPath puts dir at the front of the PATH entry in env, adding one when
// env carries none.
func prependPath(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	prepended := false
	for _, e := range env {
		key, value, ok := strings.Cut(e, "=")
		if !ok || prepended || !isPathKey(key) {
			out = append(out, e)
			continue
		}
		if value == "" {
			out = append(out, key+"="+dir)
		} else {
			out = append(out, key+"="+dir+string(os.PathListSeparator)+value)
		}
		prepended = true
	}
	if !prepended {
		out = append(out, "PATH="+dir)
	}
	return out
}

// isPathKey reports whether key names the search path. Windows environment
// names are case-insensitive and os.Environ there reports "Path"; elsewhere
// "Path" and "PATH" are two different variables and only one of them is it.
func isPathKey(key string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(key, "PATH")
	}
	return key == "PATH"
}

// switchedOff reads the disable variable. Anything set to a recognised false
// value, or unset, leaves the guard on: a variable set to "0" reads as an
// operator turning the guard OFF at a glance and must not turn it on.
func switchedOff(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// within reports whether path is inside dir.
func within(dir, path string) bool {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
