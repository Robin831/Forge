package temper

import (
	"context"
	"log"
	"os/exec"

	"github.com/Robin831/Forge/internal/executil"
)

// baseRefCandidates are the remote-tracking refs probed, in order, when no
// base branch is known. Mirrors the worktree manager's auto-detection.
var baseRefCandidates = []string{"origin/main", "origin/master"}

// ResolveBaseRef returns the git ref to use as the base for computing the
// changed-file list that Temper's per-step `paths` globs are matched against.
// When baseBranch is set explicitly it is used directly (as
// "origin/<baseBranch>"); otherwise origin/main is probed, then origin/master.
// Returns an empty string when no ref resolves — callers must then leave
// Config.ChangedFiles nil, which disables path filtering entirely.
//
// The candidate probes run with the git repo-location env vars stripped (see
// executil.CleanGitEnv) so a daemon that itself lives in a git worktree cannot
// have its own repository answer for the anvil's — an inherited GIT_DIR makes
// `git -C <worktree> rev-parse` resolve origin/main from the ambient repo, and
// Temper would then filter paths against a base ref belonging to a different
// repository.
//
// This lives here rather than in one of its callers because the pipeline,
// burnish and quench must all derive the same base ref for the same worktree:
// a second copy that resolved differently would gate the same diff two ways.
func ResolveBaseRef(ctx context.Context, worktreePath, baseBranch string) string {
	if baseBranch != "" {
		return "origin/" + baseBranch
	}
	for _, candidate := range baseRefCandidates {
		cmd := executil.HideWindow(exec.CommandContext(ctx, "git", "-C", worktreePath, "rev-parse", "--verify", candidate))
		cmd.Env = executil.CleanGitEnv()
		if err := cmd.Run(); err == nil {
			return candidate
		}
	}
	return ""
}

// ChangedFilesForBase resolves the base ref for the worktree (see
// ResolveBaseRef) and returns the files the branch changed against it, ready
// to assign to Config.ChangedFiles.
//
// A worktree whose base ref cannot be resolved yields (nil, nil): there is no
// diff to gate against, and nil means "unknown", which runs every step. That
// is deliberately not an error — the caller cannot do anything about it, and
// failing verification over it would be worse than running the full suite.
func ChangedFilesForBase(ctx context.Context, worktreePath, baseBranch string) ([]string, error) {
	baseRef := ResolveBaseRef(ctx, worktreePath, baseBranch)
	if baseRef == "" {
		return nil, nil
	}
	return ChangedFilesFromGit(ctx, worktreePath, baseRef)
}

// ChangedFilesOrNil is ChangedFilesForBase plus the fail-open handling every
// caller outside the pipeline applies to it identically: an error is logged
// under logPrefix and answered with nil.
//
// Nil means "unknown" to Temper, which runs every step. A git failure says
// nothing about the diff, so gating on a list that could not be read would
// skip steps that should have run; running everything only costs time. This
// lives here for the same reason ResolveBaseRef does — burnish and quench had
// a copy each, and two copies of a fail-open rule are two chances to fail
// closed.
func ChangedFilesOrNil(ctx context.Context, worktreePath, baseBranch, logPrefix string) []string {
	files, err := ChangedFilesForBase(ctx, worktreePath, baseBranch)
	if err != nil {
		log.Printf("%s WARN could not compute changed files for step gating (%v) — running all steps", logPrefix, err)
		return nil
	}
	return files
}
