package worktree

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/gitfail"
)

// CheckAnvilOrigin reports whether the anvil's `origin` has been repointed at a
// path inside the anvil itself — a worker's worktree under `.workers/`, in the
// case this exists for. It returns the offending URL, or "" when the remote is
// fine or could not be read.
//
// Why it is a CHECK and not a repair. Forge writes no remote URL anywhere, and
// cannot: an anvil's config carries a `path`, not a repository address, so
// there is no correct value to restore to. Only the deployment's bootstrap
// knows the address, which is why a pod restart heals this and a running daemon
// does not. Stating the invariant — a repository is never its own upstream — is
// the part that can be done without that knowledge.
//
// Why HERE. The remote is checked when a worker's worktree is torn down, next
// to CleanStaleCoreWorktree, which exists for the same class of damage: a
// config key on the anvil's SHARED .git/config pointing at a worker path that
// is about to stop existing. A worker runs with GIT_DIR set into that shared
// repository, and git writes `remote.*` to the common config from inside a
// linked worktree — GIT_DIR confines refs, objects and the index, never config
// — so a `git remote set-url` run in a worktree lands on the anvil. Checking at
// teardown is what turns "the remote was wrong by the time anyone looked" into
// a log line naming the bead whose worker was the last to hold that worktree.
//
// It is deliberately not fatal to the caller: the worktree removal that just
// happened was correct, and refusing to finish it would leave the worktree on
// disk as well as the bad remote.
func CheckAnvilOrigin(ctx context.Context, anvilPath, beadID string) string {
	origin, err := readAnvilOrigin(ctx, anvilPath)
	if err != nil {
		slog.Warn("worktree: could not read the anvil's origin", "anvil", anvilPath, "error", err)
		return ""
	}
	if !gitfail.SelfReferentialRemote(origin, anvilPath) {
		return ""
	}
	slog.Error("worktree: the anvil's origin points inside its own checkout — nothing in Forge writes this, "+
		"and every fetch, PR reconcile and dependency scan for this anvil will fail until it is repointed with "+
		"`git remote set-url`",
		"anvil", anvilPath, "origin", origin, "bead", beadID)
	return origin
}

// readAnvilOrigin reads remote.origin.url out of the anvil's own config FILE.
//
// The file, not `git -C <anvil> config --get`, for the same reason
// readCoreWorktree does it: the question is being asked precisely when the
// checkout may be in a state that makes an ordinary git command fail, and
// naming the file removes every way an ambient GIT_DIR could have the answer
// come back from a different repository — which is the mechanism under
// suspicion here.
func readAnvilOrigin(ctx context.Context, anvilPath string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git",
		"config", "--file", anvilGitConfigFile(anvilPath), "--get", "remote.origin.url"))
	cmd.Dir = anvilPath
	cmd.Env = anvilGitEnv(anvilPath)
	out, err := cmd.Output()
	if err != nil {
		// Exit 1 from `git config --get` means the key is unset. An anvil with
		// no origin at all is a different fault and not this one's to report.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return "", nil
		}
		return "", fmt.Errorf("reading remote.origin.url: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
