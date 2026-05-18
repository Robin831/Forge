package worktree

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// gitCommandTimeout matches the 60s deadline used by gitCmd in worktree.go so
// every git invocation in this package shares the same shutdown bound.
const gitCommandTimeout = 60 * time.Second

// RemoteBranchState describes whether a worker branch exists on origin and, if
// so, whether the commits it carries are already reachable from the base ref.
type RemoteBranchState int

const (
	// RemoteBranchAbsent — the branch does not exist on origin. Safe to
	// dispatch a fresh worker.
	RemoteBranchAbsent RemoteBranchState = iota
	// RemoteBranchMerged — the branch exists on origin but every commit is
	// reachable from the base ref (e.g. origin/main). The branch is a stale
	// pointer and can be deleted.
	RemoteBranchMerged
	// RemoteBranchStranded — the branch exists on origin and carries commits
	// that are NOT reachable from the base ref. A prior worker pushed work
	// without opening a PR. Dispatching a fresh worker would produce a
	// parallel implementation and a non-fast-forward push.
	RemoteBranchStranded
)

// String returns a human-readable label for the state.
func (s RemoteBranchState) String() string {
	switch s {
	case RemoteBranchAbsent:
		return "absent"
	case RemoteBranchMerged:
		return "merged"
	case RemoteBranchStranded:
		return "stranded"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// RemoteBranchInfo carries supplementary detail discovered by
// CheckRemoteBranchState — most importantly the remote SHA, which callers log
// in needs-attention messages so an operator can inspect the prior work.
type RemoteBranchInfo struct {
	// Branch is the branch name probed on origin.
	Branch string
	// SHA is the tip commit of origin/<Branch>, or empty when the branch is
	// absent.
	SHA string
	// BaseRef is the ref used as the reachability base (e.g. "origin/main"),
	// or empty when reachability was not evaluated (Absent).
	BaseRef string
}

// CheckRemoteBranchState reports whether origin/<branch> exists and, if so,
// whether its tip is reachable from the resolved base ref. anvilPath must be a
// non-bare git repo with an "origin" remote configured.
//
// baseBranch overrides the reachability target — pass the epic branch for
// crucible children so commits already merged into the epic are correctly
// classified as merged. Pass "" to use origin/main or origin/master.
//
// The check is lightweight: a single git ls-remote followed (only when the
// branch exists) by a targeted fetch and a merge-base ancestry test. It does
// NOT mutate state — callers that want to clean up a merged stale branch
// should call DeleteRemoteBranch separately.
func CheckRemoteBranchState(ctx context.Context, anvilPath, branch, baseBranch string) (RemoteBranchState, RemoteBranchInfo, error) {
	info := RemoteBranchInfo{Branch: branch}

	sha, err := lsRemoteBranchSHA(ctx, anvilPath, branch)
	if err != nil {
		return RemoteBranchAbsent, info, fmt.Errorf("ls-remote origin %s: %w", branch, err)
	}
	if sha == "" {
		return RemoteBranchAbsent, info, nil
	}
	info.SHA = sha

	// Fetch the branch so the local remote-tracking ref reflects the SHA we
	// just observed via ls-remote. Without this, a stale local origin/<branch>
	// (or no local ref at all) would make the ancestry check below incorrect.
	if err := gitCmd(ctx, anvilPath, "fetch", "origin", "--", branch); err != nil {
		return RemoteBranchAbsent, info, fmt.Errorf("fetching origin %s: %w", branch, err)
	}

	baseRef, err := resolveBaseRefWithOverride(ctx, anvilPath, baseBranch)
	if err != nil {
		return RemoteBranchAbsent, info, fmt.Errorf("resolving base ref: %w", err)
	}
	info.BaseRef = baseRef

	// merge-base --is-ancestor exits 0 when the first arg is an ancestor of
	// the second (i.e. fully reachable from the base) and 1 otherwise.
	if isAncestor(ctx, anvilPath, sha, baseRef) {
		return RemoteBranchMerged, info, nil
	}
	return RemoteBranchStranded, info, nil
}

// resolveBaseRefWithOverride returns "origin/<baseBranch>" when baseBranch is
// non-empty and present on origin, falling back to resolveBaseRef (origin/main
// or origin/master) otherwise.
func resolveBaseRefWithOverride(ctx context.Context, anvilPath, baseBranch string) (string, error) {
	if baseBranch != "" {
		candidate := "origin/" + baseBranch
		if err := gitCmd(ctx, anvilPath, "rev-parse", "--verify", candidate); err == nil {
			return candidate, nil
		}
	}
	return resolveBaseRef(ctx, anvilPath)
}

// DeleteRemoteBranch removes branch from origin. Used to clean up merged
// stranded branches before dispatching a fresh worker. A "remote ref does not
// exist" failure (another process beat us to the delete) is treated as
// success so concurrent forges do not collide.
func DeleteRemoteBranch(ctx context.Context, anvilPath, branch string) error {
	cmdCtx, cancel := contextWithGitTimeout(ctx)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", "push", "origin", "--delete", "--", branch))
	cmd.Dir = anvilPath
	cmd.Env = localGitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if strings.Contains(msg, "remote ref does not exist") ||
			strings.Contains(msg, "unable to delete") && strings.Contains(msg, "remote ref does not exist") {
			return nil
		}
		return fmt.Errorf("git push origin --delete %s: %w: %s", branch, err, strings.TrimSpace(msg))
	}
	return nil
}

// lsRemoteBranchSHA returns the SHA of origin/<branch> as reported by
// ls-remote, or "" when the branch does not exist on origin.
func lsRemoteBranchSHA(ctx context.Context, anvilPath, branch string) (string, error) {
	cmdCtx, cancel := contextWithGitTimeout(ctx)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", "ls-remote", "--heads", "origin", "--", branch))
	cmd.Dir = anvilPath
	cmd.Env = localGitEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return "", nil
	}
	// Output is "<sha>\t<ref>" per matching head; we asked for a single branch
	// so there is at most one line.
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

// isAncestor returns true when ancestor is reachable from descendant in the
// git history (i.e. every commit reachable from ancestor is reachable from
// descendant). Errors from git (including non-zero exit when not an ancestor)
// produce a false result; callers should treat that as "not merged" and
// proceed conservatively.
func isAncestor(ctx context.Context, anvilPath, ancestor, descendant string) bool {
	cmdCtx, cancel := contextWithGitTimeout(ctx)
	defer cancel()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", "merge-base", "--is-ancestor", ancestor, descendant))
	cmd.Dir = anvilPath
	cmd.Env = localGitEnv()
	return cmd.Run() == nil
}

// contextWithGitTimeout wraps ctx with the gitCommandTimeout bound so the
// helper commands in this file inherit consistent shutdown behaviour.
func contextWithGitTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, gitCommandTimeout)
}
