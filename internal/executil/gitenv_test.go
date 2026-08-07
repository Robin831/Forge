package executil

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStripGitEnv_RemovesTheWholeRepoLocationFamily(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"GIT_DIR=/decoy/.git",
		"GIT_WORK_TREE=/decoy",
		"GIT_COMMON_DIR=/decoy/.git",
		"GIT_INDEX_FILE=/decoy/.git/index",
		"GIT_OBJECT_DIRECTORY=/decoy/.git/objects",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=/other/objects",
		"GIT_NAMESPACE=refs/namespaces/x",
		"GIT_GRAFT_FILE=/decoy/.git/info/grafts",
		"GIT_SHALLOW_FILE=/decoy/.git/shallow",
		"GIT_CEILING_DIRECTORIES=/",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM=1",
		"HOME=/home/test",
	}

	out := StripGitEnv(in)

	assert.Equal(t, []string{"PATH=/usr/bin", "HOME=/home/test"}, out,
		"every repo-location override must go; unrelated vars must stay")
}

// Variables that merely configure git's behaviour (rather than pointing it at a
// different repository) must survive: stripping them would change committer
// identity, editor choice, or SSH transport for no security benefit.
func TestStripGitEnv_KeepsNonLocationGitVars(t *testing.T) {
	in := []string{
		"GIT_AUTHOR_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_SSH_COMMAND=ssh -i key",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}

	assert.Equal(t, in, StripGitEnv(in))
}

func TestStripGitEnv_DoesNotMutateInput(t *testing.T) {
	in := []string{"GIT_DIR=/decoy/.git", "PATH=/usr/bin"}

	StripGitEnv(in)

	assert.Equal(t, []string{"GIT_DIR=/decoy/.git", "PATH=/usr/bin"}, in)
}

// An entry with no "=" (which os.Environ never produces but a hand-built env
// slice might) is passed through rather than dropped or panicking.
func TestStripGitEnv_HandlesMalformedEntries(t *testing.T) {
	assert.Equal(t, []string{"NOEQUALS"}, StripGitEnv([]string{"NOEQUALS"}))
}

func TestCleanGitEnv_StripsAmbientOverrides(t *testing.T) {
	t.Setenv("GIT_DIR", "/decoy/.git")
	t.Setenv("GIT_WORK_TREE", "/decoy")
	t.Setenv("GIT_INDEX_FILE", "/decoy/.git/index")
	t.Setenv("FORGE_GITENV_MARKER", "kept")

	out := CleanGitEnv()

	assert.NotContains(t, out, "GIT_DIR=/decoy/.git")
	assert.NotContains(t, out, "GIT_WORK_TREE=/decoy")
	assert.NotContains(t, out, "GIT_INDEX_FILE=/decoy/.git/index")
	assert.Contains(t, out, "FORGE_GITENV_MARKER=kept")
	for _, e := range out {
		key, _, _ := strings.Cut(e, "=")
		assert.False(t, IsGitRepoEnvVar(key), "%s survived CleanGitEnv", key)
	}
}

func TestIsGitRepoEnvVar(t *testing.T) {
	assert.True(t, IsGitRepoEnvVar("GIT_DIR"))
	assert.True(t, IsGitRepoEnvVar("GIT_INDEX_FILE"))
	assert.True(t, IsGitRepoEnvVar("GIT_NAMESPACE"))
	assert.False(t, IsGitRepoEnvVar("GIT_AUTHOR_NAME"))
	assert.False(t, IsGitRepoEnvVar("git_dir"), "env var names are case-sensitive on POSIX")
	assert.False(t, IsGitRepoEnvVar("PATH"))
}
