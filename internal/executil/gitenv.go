package executil

import (
	"os"
	"strings"
)

// gitRepoEnvVars is the set of environment variables git honors to decide
// *which* repository (and which of its object/index/ref stores) a command
// operates on. Every one of them can make a `git -C <dir> ...` invocation
// answer from somewhere other than <dir>:
//
//   - GIT_DIR / GIT_WORK_TREE / GIT_COMMON_DIR — the repository and its
//     working tree, and the shared dir a linked worktree borrows refs from.
//   - GIT_INDEX_FILE — the staging area read by status/add/commit/diff.
//   - GIT_OBJECT_DIRECTORY / GIT_ALTERNATE_OBJECT_DIRECTORIES — object lookup.
//   - GIT_NAMESPACE — rewrites ref resolution, so `rev-parse origin/main`
//     resolves under a different ref hierarchy.
//   - GIT_GRAFT_FILE / GIT_SHALLOW_FILE — the history git believes it has.
//   - GIT_CEILING_DIRECTORIES / GIT_DISCOVERY_ACROSS_FILESYSTEM — repository
//     discovery, which can make git fail to find the target repo at all.
//
// Forge runs git commands from a daemon that may itself have been started
// inside a git worktree, or from a process spawned by a git hook (which
// exports GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE). Any inherited value here
// silently retargets the command at the ambient repository, so the whole
// family is stripped and repository location is left to cmd.Dir / `git -C`.
var gitRepoEnvVars = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_COMMON_DIR":                   true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_NAMESPACE":                    true,
	"GIT_GRAFT_FILE":                   true,
	"GIT_SHALLOW_FILE":                 true,
	"GIT_CEILING_DIRECTORIES":          true,
	"GIT_DISCOVERY_ACROSS_FILESYSTEM":  true,
}

// IsGitRepoEnvVar reports whether name is one of the git environment variables
// that redirect where a git command finds its repository. Callers that build a
// child environment for other reasons (see smith.buildChildEnv) use this to
// apply the same strip set as StripGitEnv.
func IsGitRepoEnvVar(name string) bool {
	return gitRepoEnvVars[name]
}

// StripGitEnv returns env with every git repo-location override removed. The
// input slice is not modified.
func StripGitEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		key, _, _ := strings.Cut(e, "=")
		if gitRepoEnvVars[key] {
			continue
		}
		out = append(out, e)
	}
	return out
}

// CleanGitEnv returns os.Environ() with every git repo-location override
// removed. It is the environment to hand any git subprocess whose repository
// is determined by cmd.Dir or an explicit `-C <path>`.
func CleanGitEnv() []string {
	return StripGitEnv(os.Environ())
}
