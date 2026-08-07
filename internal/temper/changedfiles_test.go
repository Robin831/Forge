package temper

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commitFileOn creates a repo in a fresh temp dir holding an origin/main
// remote-tracking ref plus one later commit adding the named file, i.e. exactly
// the shape ChangedFilesFromGit is asked to diff.
func commitFileOn(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	initGitRepo(t, dir)

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = executil.CleanGitEnv()
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}

	git("update-ref", "refs/remotes/origin/main", "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644))
	git("add", "-A")
	git("-c", "commit.gpgsign=false", "commit", "-m", "add "+name)
	return dir
}

// TestChangedFilesFromGit_IgnoresAmbientGitDir verifies the changed-file list
// describes the worktree passed in, not the repository an inherited GIT_DIR
// points at. Without stripping the git repo-override env vars the daemon (which
// may itself run inside a Forge worker worktree) would hand Temper another
// repo's file list and path filters would match the wrong files.
func TestChangedFilesFromGit_IgnoresAmbientGitDir(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available on PATH")
	}
	target := commitFileOn(t, "target.go")
	decoy := commitFileOn(t, "decoy.go")

	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)

	files, err := ChangedFilesFromGit(context.Background(), target, "origin/main")

	require.NoError(t, err)
	assert.Equal(t, []string{"target.go"}, files, "diff must come from the target worktree")
}
