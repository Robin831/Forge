package worktree

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Robin831/Forge/internal/executil"
)

// UnpushedHeadError reports that a worktree was left on disk because its HEAD
// carries commits that exist nowhere but that checkout. Removing the worktree
// would have made them unreachable — the commit objects survive in the shared
// object store, but nothing references them, so recovering one means going
// spelunking through `git fsck --lost-found`.
//
// The fields carry everything an operator needs to recover the work by hand:
// the SHA to cherry-pick or push, and the checkout it still lives in.
type UnpushedHeadError struct {
	// Path is the worktree that was preserved.
	Path string
	// Branch is the branch the worktree has checked out.
	Branch string
	// LocalHead is the commit the worktree's HEAD points at — the SHA to
	// recover.
	LocalHead string
	// RemoteHead is the tip of origin/<Branch>, or "" when the remote branch
	// could not be resolved at all.
	RemoteHead string
}

func (e *UnpushedHeadError) Error() string {
	remote := e.RemoteHead
	if remote == "" {
		remote = "<unresolved>"
	}
	return fmt.Sprintf("worktree %s kept: HEAD %s on branch %s is not on the remote (origin tip %s)",
		e.Path, shortSHA(e.LocalHead), e.Branch, shortSHA(remote))
}

// RemoveIfPushed is Remove with one added invariant: a worktree whose HEAD is
// not reachable from the remote is never deleted.
//
// Burnish (and any other fix worker) commits into the worktree and only pushes
// once verification confirms the fix. When verification cannot confirm it, the
// old teardown deleted the worktree anyway and a finished, correct fix commit
// became an unreferenced object — 25 minutes of Opus thrown away with nothing
// but a WARN line to show for it (Forge-xl50). Treat that like data loss
// anywhere else in the pipeline: keep the checkout and let the caller escalate.
//
// "Pushed" is proven, not assumed: HEAD must be an ancestor of origin/<branch>,
// or reachable from some other remote-tracking branch (the branch may have been
// renamed or already merged with a fast-forward). Anything the check cannot
// prove is treated as unpushed — a preserved worktree costs one directory and
// one Needs Attention entry; a wrong removal costs the work.
//
// A worktree whose HEAD cannot be resolved at all (empty checkout, corrupted
// gitdir) has nothing to protect and is removed normally.
func (m *Manager) RemoveIfPushed(ctx context.Context, anvilPath string, wt *Worktree) error {
	local, remote, pushed := headPushState(ctx, wt.Path, wt.Branch)
	if !pushed && local != "" {
		return &UnpushedHeadError{
			Path:       wt.Path,
			Branch:     wt.Branch,
			LocalHead:  local,
			RemoteHead: remote,
		}
	}
	return m.Remove(ctx, anvilPath, wt)
}

// headPushState resolves the worktree's HEAD and reports whether it is provably
// on the remote. local is "" when HEAD cannot be resolved (nothing to protect);
// remote is "" when origin/<branch> cannot be resolved.
func headPushState(ctx context.Context, worktreePath, branch string) (local, remote string, pushed bool) {
	local, err := gitCapture(ctx, worktreePath, "rev-parse", "HEAD")
	if err != nil || local == "" {
		// No resolvable HEAD — an empty or broken checkout holds no commits
		// worth keeping.
		return "", "", true
	}

	if branch != "" {
		remote, _ = gitCapture(ctx, worktreePath, "rev-parse", "--verify", "origin/"+branch)
		if remote != "" {
			// --is-ancestor exits 0 when local is contained in the remote tip,
			// which covers both "identical" and "the remote moved ahead".
			if err := gitCmdQuiet(ctx, worktreePath, "merge-base", "--is-ancestor", local, "origin/"+branch); err == nil {
				return local, remote, true
			}
			return local, remote, false
		}
	}

	// No usable origin/<branch> ref: fall back to asking whether ANY
	// remote-tracking branch contains the commit. This is what keeps a renamed
	// or already-deleted branch from stranding a worktree whose work is in fact
	// safe on the remote.
	containing, err := gitCapture(ctx, worktreePath, "branch", "-r", "--contains", local)
	if err == nil && strings.TrimSpace(containing) != "" {
		return local, remote, true
	}
	return local, remote, false
}

// gitCapture runs a git command and returns its trimmed stdout. Errors carry
// the command's own stderr so a caller logging them says something useful.
func gitCapture(ctx context.Context, dir string, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, DefaultGitTimeout)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", append([]string{"-C", dir}, args...)...))
	cmd.Env = localGitEnv()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), msg, err)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitCmdQuiet runs a git command purely for its exit status, discarding output.
// Used for predicates like `merge-base --is-ancestor` where a non-zero exit is
// the answer, not a failure worth logging.
func gitCmdQuiet(ctx context.Context, dir string, args ...string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, DefaultGitTimeout)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", append([]string{"-C", dir}, args...)...))
	cmd.Env = localGitEnv()
	return cmd.Run()
}

// shortSHA abbreviates a commit id for operator-facing messages, leaving
// anything that is not a full SHA untouched.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
