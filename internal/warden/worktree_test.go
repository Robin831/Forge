package warden

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/worktree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitRepoFixture builds a main checkout with one commit and returns its
// path. It is a MAIN checkout on purpose: that is the shape an anvil has,
// and the shape Smith's pre-flight refuses.
func gitRepoFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
	dir := t.TempDir()
	// CleanGitEnv, not os.Environ: these tests run inside a worker worktree
	// whose GIT_DIR/GIT_WORK_TREE are exported, and an inherited GIT_DIR
	// answers for THAT repository — `git -C <tmp> init` reinitializes the
	// worker's repo and the fixture silently is not a repository at all.
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(executil.CleanGitEnv(), "LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
	}
	run("init", "-b", "main")
	run("config", "user.email", "forge@example.com")
	run("config", "user.name", "Forge Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("committed\n"), 0o644))
	run("add", "README.md")
	run("commit", "-m", "initial")
	return dir
}

// worktreeList reports `git worktree list` for the repo, which is what says
// whether the administrative entry survived the run.
func worktreeList(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "worktree", "list")
	cmd.Env = append(executil.CleanGitEnv(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git worktree list: %s", out)
	return string(out)
}

func TestWithEphemeralWorktree(t *testing.T) {
	t.Run("fn sees a linked worktree checked out at HEAD", func(t *testing.T) {
		repo := gitRepoFixture(t)

		var seen string
		require.NoError(t, WithEphemeralWorktree(context.Background(), repo, func(wt string) error {
			seen = wt
			// The tree is materialized: the committed file is there.
			body, err := os.ReadFile(filepath.Join(wt, "README.md"))
			require.NoError(t, err)
			assert.Equal(t, "committed\n", string(body))
			// And it is a LINKED worktree, not a second clone: .git is the
			// file pointer the pre-flight requires, not a directory.
			info, err := os.Stat(filepath.Join(wt, ".git"))
			require.NoError(t, err)
			assert.False(t, info.IsDir(), ".git must be a worktree file pointer")
			return nil
		}))

		assert.NotEqual(t, repo, seen, "the session must not run in the main checkout")
		assert.NotContains(t, worktreeList(t, repo), seen)
		assert.NoDirExists(t, seen)
	})

	t.Run("the pre-flight accepts the directory fn is handed", func(t *testing.T) {
		repo := gitRepoFixture(t)

		// The whole point of the helper: the anvil itself is refused, the
		// directory fn receives is not.
		require.Error(t, worktree.ValidateWorktreeDir(repo))
		require.NoError(t, WithEphemeralWorktree(context.Background(), repo, func(wt string) error {
			return worktree.ValidateWorktreeDir(wt)
		}))
	})

	t.Run("cleanup runs when fn fails", func(t *testing.T) {
		repo := gitRepoFixture(t)

		var seen string
		err := WithEphemeralWorktree(context.Background(), repo, func(wt string) error {
			seen = wt
			return assert.AnError
		})
		require.ErrorIs(t, err, assert.AnError, "fn's error must reach the caller unwrapped by cleanup")
		assert.NotContains(t, worktreeList(t, repo), seen)
		assert.NoDirExists(t, seen)
	})

	t.Run("uncommitted rules files are visible inside the worktree", func(t *testing.T) {
		repo := gitRepoFixture(t)
		// Never committed: a detached checkout would carry none of this.
		require.NoError(t, SaveRules(repo, &RulesFile{Rules: []Rule{
			{ID: "live-rule", Category: "style", Pattern: "p", Check: "c"},
		}}))

		require.NoError(t, WithEphemeralWorktree(context.Background(), repo, func(wt string) error {
			rf, err := LoadRules(wt)
			require.NoError(t, err)
			require.Len(t, rf.Rules, 1)
			assert.Equal(t, "live-rule", rf.Rules[0].ID)
			return nil
		}))
	})

	t.Run("edits inside the worktree never reach the anvil", func(t *testing.T) {
		repo := gitRepoFixture(t)
		require.NoError(t, SaveRules(repo, &RulesFile{Rules: []Rule{
			{ID: "live-rule", Category: "style", Pattern: "p", Check: "c"},
		}}))

		// The merged rule comes back to the caller as JSON from the session;
		// the checkout is scratch space, so whatever a session writes there
		// is discarded with it.
		require.NoError(t, WithEphemeralWorktree(context.Background(), repo, func(wt string) error {
			return SaveRules(wt, &RulesFile{Rules: []Rule{{ID: "scribbled", Category: "style", Pattern: "x", Check: "y"}}})
		}))

		rf, err := LoadRules(repo)
		require.NoError(t, err)
		require.Len(t, rf.Rules, 1)
		assert.Equal(t, "live-rule", rf.Rules[0].ID)
	})

	t.Run("a directory outside any repository is passed through", func(t *testing.T) {
		dir := t.TempDir()

		var seen string
		require.NoError(t, WithEphemeralWorktree(context.Background(), dir, func(wt string) error {
			seen = wt
			return nil
		}))
		// Nothing to isolate from, and no repository to add a worktree to:
		// the pre-flight accepts such a directory as it stands.
		assert.Equal(t, dir, seen)
	})

	t.Run("a repository with no commits is an error, not a pass-through", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available on PATH")
		}
		dir := t.TempDir()
		cmd := exec.Command("git", "-C", dir, "init", "-b", "main")
		cmd.Env = append(executil.CleanGitEnv(), "LC_ALL=C")
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git init: %s", out)

		called := false
		err = WithEphemeralWorktree(context.Background(), dir, func(string) error {
			called = true
			return nil
		})
		// Running fn in the main checkout is exactly what the pre-flight
		// refuses, so an unresolvable HEAD must not degrade to it.
		require.Error(t, err)
		assert.False(t, called)
	})
}

// The doc comment's other half: with fn successful, a teardown failure IS
// the returned error, since nothing else would report it — and it is
// returned as a *WorktreeCleanupError so a caller can tell it from the
// checkout never having been created at all. ConsolidateAnvil branches on
// exactly that distinction, and without this case the assignment on the
// named return is exercised by nothing.
//
// The failure is forced without mocking git: fn drops the write bit on the
// temp directory holding the checkout, which is what a Windows file lock
// amounts to here — neither `git worktree remove` nor the RemoveAll behind
// it can unlink anything inside.
func TestWithEphemeralWorktree_CleanupFailure(t *testing.T) {
	// Root ignores the permission bits the failure is built on.
	if os.Geteuid() == 0 {
		t.Skip("cleanup cannot be made to fail as root")
	}

	// sealTmpParent makes the checkout's temp parent undeletable and
	// arranges for the test to unseal and remove it afterwards: it lives
	// under os.MkdirTemp rather than t.TempDir, so nothing else would.
	sealTmpParent := func(t *testing.T, wtPath string) error {
		t.Helper()
		parent := filepath.Dir(wtPath)
		t.Cleanup(func() {
			_ = os.Chmod(parent, 0o700)
			_ = os.RemoveAll(parent)
		})
		return os.Chmod(parent, 0o500)
	}

	t.Run("a cleanup failure after a successful fn is reported as one", func(t *testing.T) {
		repo := gitRepoFixture(t)

		var seen string
		err := WithEphemeralWorktree(context.Background(), repo, func(wt string) error {
			seen = wt
			return sealTmpParent(t, wt)
		})
		require.Error(t, err, "a cleanup failure must not be swallowed on a successful run")

		var cleanupErr *WorktreeCleanupError
		require.ErrorAs(t, err, &cleanupErr,
			"the caller must be able to tell a teardown failure from a checkout that never existed")
		assert.Equal(t, seen, cleanupErr.WorktreePath)
	})

	t.Run("fn's error still wins over a cleanup failure", func(t *testing.T) {
		repo := gitRepoFixture(t)

		err := WithEphemeralWorktree(context.Background(), repo, func(wt string) error {
			require.NoError(t, sealTmpParent(t, wt))
			return assert.AnError
		})
		require.ErrorIs(t, err, assert.AnError,
			"the reason the run failed is worth more than the reason a temp dir survived")

		var cleanupErr *WorktreeCleanupError
		assert.False(t, errors.As(err, &cleanupErr),
			"a cleanup failure must never displace fn's own error")
	})
}
