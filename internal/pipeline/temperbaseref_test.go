package pipeline

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitIn runs a git command in dir with the repo-override env vars stripped, so
// the helper itself is immune to the ambient GIT_DIR these tests set.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = cleanGitEnv()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// decoyRepoEnv points GIT_DIR/GIT_WORK_TREE at a second repository, reproducing
// a daemon (or test binary) started from inside a Forge worker worktree. Any
// git command that fails to strip these answers from the decoy instead of from
// the path it was handed with -C.
func decoyRepoEnv(t *testing.T, withOriginMain bool) string {
	t.Helper()
	decoy, _ := initGitRepo(t)
	if withOriginMain {
		gitIn(t, decoy, "update-ref", "refs/remotes/origin/main", "HEAD")
	}
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	return decoy
}

// TestResolveTemperBaseRef_IgnoresAmbientGitDir verifies that base-ref detection
// probes the worktree it was given and not the repository an inherited GIT_DIR
// points at. The decoy has origin/main; the target does not, so a leaked GIT_DIR
// shows up as a confidently wrong "origin/main".
func TestResolveTemperBaseRef_IgnoresAmbientGitDir(t *testing.T) {
	target, _ := initGitRepo(t)
	decoyRepoEnv(t, true)

	ref := resolveTemperBaseRef(context.Background(), target, "")

	assert.Empty(t, ref, "target repo has no origin/main; the decoy's must not answer for it")
}

// TestResolveTemperBaseRef_FindsTargetRefUnderAmbientGitDir is the same leak in
// the other direction: the target owns origin/main and the decoy does not, so an
// inherited GIT_DIR suppresses a base ref that really exists.
func TestResolveTemperBaseRef_FindsTargetRefUnderAmbientGitDir(t *testing.T) {
	target, _ := initGitRepo(t)
	gitIn(t, target, "update-ref", "refs/remotes/origin/main", "HEAD")
	decoyRepoEnv(t, false)

	ref := resolveTemperBaseRef(context.Background(), target, "")

	assert.Equal(t, "origin/main", ref, "origin/main exists in the target worktree")
}

// TestResolveTemperBaseRef_PrefersOriginMasterFallback pins the documented
// candidate order when only origin/master exists.
func TestResolveTemperBaseRef_PrefersOriginMasterFallback(t *testing.T) {
	target, _ := initGitRepo(t)
	gitIn(t, target, "update-ref", "refs/remotes/origin/master", "HEAD")
	decoyRepoEnv(t, true)

	ref := resolveTemperBaseRef(context.Background(), target, "")

	assert.Equal(t, "origin/master", ref)
}

// TestResolveTemperBaseRef_ExplicitBaseBranchSkipsProbe documents that a
// configured base branch (e.g. a Crucible child targeting its epic branch) is
// used verbatim without touching git at all.
func TestResolveTemperBaseRef_ExplicitBaseBranchSkipsProbe(t *testing.T) {
	target, _ := initGitRepo(t)
	decoyRepoEnv(t, true)

	ref := resolveTemperBaseRef(context.Background(), target, "feature/Forge-epic")

	assert.Equal(t, "origin/feature/Forge-epic", ref)
}
