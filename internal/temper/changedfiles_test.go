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
	writeRepoFile(t, dir, name, "x")
	git("add", "-A")
	git("-c", "commit.gpgsign=false", "commit", "-m", "add "+name)
	return dir
}

// writeRepoFile writes a file (creating parent directories) inside a repo.
func writeRepoFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
}

// TestChangedFilesFromGit_IncludesDeletions pins the invariant that a file the
// branch DELETED still appears in the changed-file list. A deletion changes
// what builds exactly as much as an edit does, so a step gated on "**/*.cs"
// must still run for a diff that only removes .cs files — the alternative is a
// verification that reports PASS having compiled nothing.
func TestChangedFilesFromGit_IncludesDeletions(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available on PATH")
	}
	dir := commitFileOn(t, "api/Service.cs")
	gitIn(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitIn(t, dir, "rm", "-q", filepath.FromSlash("api/Service.cs"))
	gitIn(t, dir, "-c", "commit.gpgsign=false", "commit", "-m", "delete service")

	files, err := ChangedFilesFromGit(context.Background(), dir, "origin/main")

	require.NoError(t, err)
	assert.Equal(t, []string{"api/Service.cs"}, files)
	assert.True(t, matchesChangedFiles([]string{"**/*.cs"}, files),
		"a deletion must still trigger the steps gated on that file type")
}

// TestChangedFilesFromGit_RenameNamesBothSides covers the other half of the
// same invariant. With rename detection on, git reports a move as the
// DESTINATION path alone, so moving the last Go file out of a gated directory
// would look like nothing happened there. --no-renames reports the move as a
// delete plus an add, naming both sides.
func TestChangedFilesFromGit_RenameNamesBothSides(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not available on PATH")
	}
	dir := commitFileOn(t, "api/handler.go")
	gitIn(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "service"), 0o755))
	gitIn(t, dir, "mv", filepath.FromSlash("api/handler.go"), filepath.FromSlash("service/handler.go"))
	gitIn(t, dir, "-c", "commit.gpgsign=false", "commit", "-m", "move handler")

	files, err := ChangedFilesFromGit(context.Background(), dir, "origin/main")

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"api/handler.go", "service/handler.go"}, files)
	assert.True(t, matchesChangedFiles([]string{"api/**"}, files),
		"the source directory of a move must still be seen as changed")
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
