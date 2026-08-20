package temper

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitIn runs a git command in dir with the repo-override env vars stripped, so
// the helper itself is immune to the ambient GIT_DIR these tests set.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = executil.CleanGitEnv()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// newRepo creates a temp repo with one commit and returns its path.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	initGitRepo(t, dir)
	return dir
}

// decoyRepoEnv points GIT_DIR/GIT_WORK_TREE at a second repository, reproducing
// a daemon (or test binary) started from inside a Forge worker worktree. Any
// git command that fails to strip these answers from the decoy instead of from
// the path it was handed with -C.
func decoyRepoEnv(t *testing.T, withOriginMain bool) {
	t.Helper()
	decoy := newRepo(t)
	if withOriginMain {
		gitIn(t, decoy, "update-ref", "refs/remotes/origin/main", "HEAD")
	}
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
}

// TestResolveBaseRef_IgnoresAmbientGitDir verifies that base-ref detection
// probes the worktree it was given and not the repository an inherited GIT_DIR
// points at. The decoy has origin/main; the target does not, so a leaked GIT_DIR
// shows up as a confidently wrong "origin/main".
func TestResolveBaseRef_IgnoresAmbientGitDir(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available on PATH")
	}
	target := newRepo(t)
	decoyRepoEnv(t, true)

	ref := ResolveBaseRef(context.Background(), target, "")

	assert.Empty(t, ref, "target repo has no origin/main; the decoy's must not answer for it")
}

// TestResolveBaseRef_FindsTargetRefUnderAmbientGitDir is the same leak in the
// other direction: the target owns origin/main and the decoy does not, so an
// inherited GIT_DIR suppresses a base ref that really exists.
func TestResolveBaseRef_FindsTargetRefUnderAmbientGitDir(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available on PATH")
	}
	target := newRepo(t)
	gitIn(t, target, "update-ref", "refs/remotes/origin/main", "HEAD")
	decoyRepoEnv(t, false)

	ref := ResolveBaseRef(context.Background(), target, "")

	assert.Equal(t, "origin/main", ref, "origin/main exists in the target worktree")
}

// TestResolveBaseRef_PrefersOriginMasterFallback pins the documented candidate
// order when only origin/master exists.
func TestResolveBaseRef_PrefersOriginMasterFallback(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available on PATH")
	}
	target := newRepo(t)
	gitIn(t, target, "update-ref", "refs/remotes/origin/master", "HEAD")
	decoyRepoEnv(t, true)

	ref := ResolveBaseRef(context.Background(), target, "")

	assert.Equal(t, "origin/master", ref)
}

// TestResolveBaseRef_ExplicitBaseBranchSkipsProbe documents that a configured
// base branch (e.g. a Crucible child targeting its epic branch) is used
// verbatim without touching git at all.
func TestResolveBaseRef_ExplicitBaseBranchSkipsProbe(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available on PATH")
	}
	target := newRepo(t)
	decoyRepoEnv(t, true)

	ref := ResolveBaseRef(context.Background(), target, "feature/Forge-epic")

	assert.Equal(t, "origin/feature/Forge-epic", ref)
}

// TestChangedFilesForBase_NoResolvableBaseIsNotAnError pins the fail-open
// contract every caller relies on: a worktree with no base ref yields a nil
// list and no error, which Temper reads as "unknown" and runs every step.
func TestChangedFilesForBase_NoResolvableBaseIsNotAnError(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available on PATH")
	}
	target := newRepo(t)

	files, err := ChangedFilesForBase(context.Background(), target, "")

	require.NoError(t, err)
	assert.Nil(t, files)
}

// TestChangedFilesForBase_ResolvesAndDiffs covers the ordinary path: the base
// ref is auto-detected and the branch's own files come back.
func TestChangedFilesForBase_ResolvesAndDiffs(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available on PATH")
	}
	dir := commitFileOn(t, "api/handler.go")

	files, err := ChangedFilesForBase(context.Background(), dir, "")

	require.NoError(t, err)
	assert.Equal(t, []string{"api/handler.go"}, files)
}

// TestChangedFilesOrNil_GitFailureIsNilNotEmpty is the fail-open decision the
// helper exists to hold in one place, and the distinction is the whole point:
// Temper consults its `paths` globs only when Config.ChangedFiles is non-nil,
// so an EMPTY non-nil slice matches nothing and skips every gated step — a
// git failure would silently turn verification into a no-op. Nil means
// "unknown", which runs everything.
//
// assert.Nil, never assert.Empty: assert.Empty passes for both, which is
// exactly the difference under test.
func TestChangedFilesOrNil_GitFailureIsNilNotEmpty(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available on PATH")
	}

	t.Run("unresolvable explicit base", func(t *testing.T) {
		// An explicit base skips the probe, so the ref reaches git diff and
		// fails there — the error path ChangedFilesForBase forwards.
		dir := commitFileOn(t, "api/handler.go")

		files := ChangedFilesOrNil(context.Background(), dir, "no-such-branch", "[test]")

		assert.Nil(t, files, "a failed diff must be nil (unknown), not an empty list (nothing changed)")
	})

	t.Run("not a git repository", func(t *testing.T) {
		dir := t.TempDir()

		files := ChangedFilesOrNil(context.Background(), dir, "main", "[test]")

		assert.Nil(t, files)
	})

	t.Run("no base ref resolves", func(t *testing.T) {
		// Not an error at all (ChangedFilesForBase returns nil, nil), but the
		// answer has to be the same nil the error path produces.
		dir := newRepo(t)

		files := ChangedFilesOrNil(context.Background(), dir, "", "[test]")

		assert.Nil(t, files)
	})
}

// TestChangedFilesOrNil_ForwardsFileList is the other half: when the diff does
// resolve, the helper is a pass-through and the gate really is applied.
func TestChangedFilesOrNil_ForwardsFileList(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available on PATH")
	}
	dir := commitFileOn(t, "api/handler.go")

	files := ChangedFilesOrNil(context.Background(), dir, "", "[test]")

	assert.Equal(t, []string{"api/handler.go"}, files)
}
