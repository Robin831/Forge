package depupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/depcheck"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildFragmentContent_English(t *testing.T) {
	groups := []UpdateGroup{
		{
			Name: "vite ecosystem",
			Kind: "minor",
			Updates: []depcheck.ModuleUpdate{
				{Path: "vite", Current: "4.0.0", Latest: "5.0.0", Kind: "major"},
				{Path: "vite-plugin-foo", Current: "1.2.0", Latest: "1.3.0", Kind: "minor"},
			},
		},
	}

	got := buildFragmentContent(groups, "deps-batch-test", false)

	assert.True(t, strings.HasPrefix(got, "category: Changed\n"), "expected 'category: Changed' header, got: %q", got)
	assert.Contains(t, got, "**Updated vite** - Bumped from 4.0.0 to 5.0.0. (deps-batch-test)")
	assert.Contains(t, got, "**Updated vite-plugin-foo** - Bumped from 1.2.0 to 1.3.0. (deps-batch-test)")
}

func TestBuildFragmentContent_Norwegian(t *testing.T) {
	groups := []UpdateGroup{
		{
			Name: "react",
			Kind: "minor",
			Updates: []depcheck.ModuleUpdate{
				{Path: "react", Current: "18.0.0", Latest: "18.2.0", Kind: "minor"},
			},
		},
	}

	got := buildFragmentContent(groups, "deps-batch-test", true)

	assert.Contains(t, got, "**Oppdatert react** - Bumpet fra 18.0.0 til 18.2.0. (deps-batch-test)")
}

func TestBuildFragmentContent_MultipleGroups(t *testing.T) {
	groups := []UpdateGroup{
		{
			Name: "react ecosystem",
			Kind: "minor",
			Updates: []depcheck.ModuleUpdate{
				{Path: "react", Current: "18.0.0", Latest: "18.2.0", Kind: "minor"},
			},
		},
		{
			Name: "lodash",
			Kind: "patch",
			Updates: []depcheck.ModuleUpdate{
				{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"},
			},
		},
	}

	got := buildFragmentContent(groups, "deps-batch-test", false)

	assert.Equal(t, 2, strings.Count(got, "\n- "), "expected 2 bullet lines, got: %q", got)
}

func TestBuildFragmentContent_Empty(t *testing.T) {
	got := buildFragmentContent(nil, "deps-batch-test", false)
	assert.Equal(t, "category: Changed\n", got)
}

func TestDetectBilingual_NoDirectory(t *testing.T) {
	dir := t.TempDir()
	assert.False(t, DetectBilingual(dir), "expected false for directory with no changelog.d/")
}

func TestDetectBilingual_MonolingualFragments(t *testing.T) {
	dir := t.TempDir()
	clDir := filepath.Join(dir, "changelog.d")
	require.NoError(t, os.MkdirAll(clDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(clDir, "some-bead.md"), []byte("category: Added\n"), 0o644))

	assert.False(t, DetectBilingual(dir), "expected false for directory with only .md fragments")
}

func TestDetectBilingual_BilingualFragments(t *testing.T) {
	dir := t.TempDir()
	clDir := filepath.Join(dir, "changelog.d")
	require.NoError(t, os.MkdirAll(clDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(clDir, "some-bead.en.md"), []byte("category: Added\n"), 0o644))

	assert.True(t, DetectBilingual(dir), "expected true when .en.md fragment exists")
}

func TestGenerateChangelog_EmptyGroups(t *testing.T) {
	dir := t.TempDir()
	// Empty groups should be a no-op with no error.
	assert.NoError(t, GenerateChangelog(dir, nil, false))
}

// initGitRepo sets up a minimal git repo in dir so git add/commit can run.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
}

func TestGenerateChangelog_Monolingual(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	groups := []UpdateGroup{
		{
			Name: "lodash",
			Kind: "patch",
			Updates: []depcheck.ModuleUpdate{
				{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"},
			},
		},
	}

	require.NoError(t, GenerateChangelog(dir, groups, false))

	// Verify exactly one .md file was created (no .en.md/.nb.md).
	clDir := filepath.Join(dir, "changelog.d")
	entries, err := os.ReadDir(clDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	name := entries[0].Name()
	assert.False(t, strings.HasSuffix(name, ".en.md"), "expected monolingual .md file, got %q", name)
	assert.False(t, strings.HasSuffix(name, ".nb.md"), "expected monolingual .md file, got %q", name)
	assert.True(t, strings.HasSuffix(name, ".md"), "expected .md suffix, got %q", name)

	content, err := os.ReadFile(filepath.Join(clDir, name))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(content), "category: Changed\n"), "missing category header in %q", string(content))
	assert.Contains(t, string(content), "**Updated lodash** - Bumped from 4.17.20 to 4.17.21.")
}

func TestGenerateChangelog_Bilingual(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	groups := []UpdateGroup{
		{
			Name: "react",
			Kind: "minor",
			Updates: []depcheck.ModuleUpdate{
				{Path: "react", Current: "18.0.0", Latest: "18.2.0", Kind: "minor"},
			},
		},
	}

	require.NoError(t, GenerateChangelog(dir, groups, true))

	clDir := filepath.Join(dir, "changelog.d")
	entries, err := os.ReadDir(clDir)
	require.NoError(t, err)
	require.Len(t, entries, 2, "expected 2 files (en+nb)")

	var hasEN, hasNB bool
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".en.md") {
			hasEN = true
			content, _ := os.ReadFile(filepath.Join(clDir, name))
			assert.Contains(t, string(content), "**Updated react**")
		}
		if strings.HasSuffix(name, ".nb.md") {
			hasNB = true
			content, _ := os.ReadFile(filepath.Join(clDir, name))
			assert.Contains(t, string(content), "**Oppdatert react**")
		}
	}
	assert.True(t, hasEN, "expected .en.md file to be created")
	assert.True(t, hasNB, "expected .nb.md file to be created")
}

func TestWriteFragment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.md")
	content := "category: Changed\n- **Updated pkg** - Bumped from 1.0.0 to 2.0.0. (test-tag)\n"

	require.NoError(t, writeFragment(path, content))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, string(got))
}
