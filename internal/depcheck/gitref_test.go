package depcheck

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/executil"
)

// git runs a git command in dir, failing the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// The test process may itself be running inside a worktree, whose GIT_DIR /
	// GIT_WORK_TREE would answer for the wrong repository.
	cmd.Env = append(executil.CleanGitEnv(),
		"GIT_AUTHOR_NAME=depcheck-test", "GIT_AUTHOR_EMAIL=depcheck@example.invalid",
		"GIT_COMMITTER_NAME=depcheck-test", "GIT_COMMITTER_EMAIL=depcheck@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	return string(out)
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// newOriginAndClone builds a bare "origin" holding one commit of files, and a
// clone of it. It returns the clone's path.
func newOriginAndClone(t *testing.T, files map[string]string) string {
	t.Helper()
	requireGit(t)

	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	clone := filepath.Join(root, "clone")

	git(t, root, "init", "--bare", "--initial-branch=main", origin)
	git(t, root, "init", "--initial-branch=main", seed)
	writeFiles(t, seed, files)
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-m", "seed")
	git(t, seed, "remote", "add", "origin", origin)
	git(t, seed, "push", "-u", "origin", "main")

	git(t, root, "clone", origin, clone)
	return clone
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
}

// pushToOrigin commits files on a throwaway clone of the repo's origin so the
// remote advances without touching repo's own working tree.
func pushToOrigin(t *testing.T, repo string, files map[string]string) {
	t.Helper()
	originURL := strings.TrimSpace(git(t, repo, "remote", "get-url", "origin"))
	other := t.TempDir()
	work := filepath.Join(other, "push")
	git(t, other, "clone", originURL, work)
	writeFiles(t, work, files)
	git(t, work, "add", "-A")
	git(t, work, "commit", "-m", "upstream change")
	git(t, work, "push", "origin", "main")
}

func TestResolveUpstream_ReadsConfiguredUpstream(t *testing.T) {
	clone := newOriginAndClone(t, map[string]string{"go.mod": "module x\n"})

	up, err := resolveUpstream(context.Background(), clone)
	require.NoError(t, err)
	assert.Equal(t, upstream{Remote: "origin", Branch: "main", Ref: "origin/main"}, up)
}

func TestResolveUpstream_FallsBackToOriginBranch(t *testing.T) {
	clone := newOriginAndClone(t, map[string]string{"go.mod": "module x\n"})
	// A branch with no upstream at all.
	git(t, clone, "checkout", "-b", "detached-work")

	up, err := resolveUpstream(context.Background(), clone)
	require.NoError(t, err)
	assert.Equal(t, "origin", up.Remote)
	assert.Equal(t, "detached-work", up.Branch)
	assert.Equal(t, "origin/detached-work", up.Ref)
}

func TestResolveUpstream_DetachedHeadHasNoBranchToFallBackOn(t *testing.T) {
	clone := newOriginAndClone(t, map[string]string{"go.mod": "module x\n"})
	head := strings.TrimSpace(git(t, clone, "rev-parse", "HEAD"))
	git(t, clone, "checkout", "--detach", head)

	_, err := resolveUpstream(context.Background(), clone)
	assert.ErrorIs(t, err, ErrNoUpstream)
}

func TestResolveUpstream_NotAGitRepo(t *testing.T) {
	requireGit(t)
	_, err := resolveUpstream(context.Background(), t.TempDir())
	assert.Error(t, err, "a path that is not a checkout must be an error, not a silent empty ref")
}

func TestParseUpstreamRef(t *testing.T) {
	up, ok := parseUpstreamRef("upstream/release/2.x\n")
	require.True(t, ok)
	assert.Equal(t, upstream{Remote: "upstream", Branch: "release/2.x", Ref: "upstream/release/2.x"}, up,
		"a branch name may contain slashes; only the first segment is the remote")

	_, ok = parseUpstreamRef("main")
	assert.False(t, ok)
	_, ok = parseUpstreamRef("")
	assert.False(t, ok)
}

func TestShowBlob_ReadsCommittedContentNotTheWorkingTree(t *testing.T) {
	clone := newOriginAndClone(t, map[string]string{"go.mod": "module committed\n"})

	// Dirty the tracked file the way a permanently-overridden anvil does.
	require.NoError(t, os.WriteFile(filepath.Join(clone, "go.mod"), []byte("module locally-edited\n"), 0o644))

	data, err := showBlob(context.Background(), clone, "origin/main", "go.mod")
	require.NoError(t, err)
	assert.Equal(t, "module committed\n", string(data))
}

func TestShowBlob_NotFound(t *testing.T) {
	clone := newOriginAndClone(t, map[string]string{"go.mod": "module x\n"})

	_, err := showBlob(context.Background(), clone, "origin/main", "package.json")
	assert.ErrorIs(t, err, ErrBlobNotFound)

	// A file that exists on disk but is not tracked is equally "not at the ref".
	require.NoError(t, os.WriteFile(filepath.Join(clone, "untracked.json"), []byte("{}"), 0o644))
	_, err = showBlob(context.Background(), clone, "origin/main", "untracked.json")
	assert.ErrorIs(t, err, ErrBlobNotFound)
}

func TestShowBlob_UnknownRefIsNotABlobNotFound(t *testing.T) {
	clone := newOriginAndClone(t, map[string]string{"go.mod": "module x\n"})

	_, err := showBlob(context.Background(), clone, "origin/no-such-branch", "go.mod")
	require.Error(t, err)
	// A ref that will not resolve is a failed scan, not an anvil that has no Go
	// ecosystem — reading it as the latter would report "nothing to update".
	assert.False(t, errors.Is(err, ErrBlobNotFound), "an unresolvable ref must not read as a missing path")
}

func TestListTreePaths_OnlyTrackedFiles(t *testing.T) {
	clone := newOriginAndClone(t, map[string]string{
		"go.mod":             "module x\n",
		"web/package.json":   "{}",
		"src/App/App.csproj": "<Project/>",
	})
	// Untracked worker/preview checkouts and installed dependencies.
	writeFiles(t, clone, map[string]string{
		".workers/w1/package.json":        "{}",
		"web/node_modules/x/package.json": "{}",
	})

	paths, err := listTreePaths(context.Background(), clone, "origin/main")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"go.mod", "web/package.json", "src/App/App.csproj"}, paths)
}

func TestFetchUpstream_LeavesLocalModificationsIntact(t *testing.T) {
	clone := newOriginAndClone(t, map[string]string{
		"go.mod":             "module x\n\nrequire github.com/foo/bar v1.0.0\n",
		".beads/config.yaml": "sync.remote: \"http://laptop:50051/beads\"\n",
	})

	// The anvil carries a permanent pod-local override of a tracked file...
	override := "sync.remote: \"http://dolt-beads.svc.cluster.local:50051/beads\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(clone, ".beads", "config.yaml"), []byte(override), 0o644))
	statusBefore := git(t, clone, "status", "--porcelain")
	require.NotEmpty(t, strings.TrimSpace(statusBefore))

	// ...and upstream touches that very file, which is what `git pull --ff-only`
	// refused on, skipping the anvil forever.
	pushToOrigin(t, clone, map[string]string{
		".beads/config.yaml": "# the pod and the laptop need different values here\nsync.remote: \"http://laptop:50051/beads\"\n",
		"go.mod":             "module x\n\nrequire github.com/foo/bar v1.5.0\n",
	})

	up, err := resolveUpstream(context.Background(), clone)
	require.NoError(t, err)
	ref, err := fetchUpstream(context.Background(), clone, up)
	require.NoError(t, err, "a fetch is network-only and cannot be refused by local modifications")
	assert.Equal(t, "origin/main", ref)

	assert.Equal(t, statusBefore, git(t, clone, "status", "--porcelain"),
		"the working tree must be byte-identical after the scan's git work")
	kept, err := os.ReadFile(filepath.Join(clone, ".beads", "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, override, string(kept), "the local override survives")

	// And the manifests now read from upstream rather than from the stale checkout.
	mod, err := showBlob(context.Background(), clone, ref, "go.mod")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"github.com/foo/bar": "v1.5.0"}, parseGoModRequires(mod))
}

func TestRefSourceFor_FetchesAndReadsFromTheTrackingRef(t *testing.T) {
	clone := newOriginAndClone(t, map[string]string{"web/package.json": `{"dependencies":{"lodash":"^4.17.20"}}`})
	require.NoError(t, os.WriteFile(filepath.Join(clone, "web", "package.json"),
		[]byte(`{"dependencies":{"lodash":"^0.0.1"}}`), 0o644))
	pushToOrigin(t, clone, map[string]string{"web/package.json": `{"dependencies":{"lodash":"^4.17.21"}}`})

	statusBefore := git(t, clone, "status", "--porcelain")

	s := &Scanner{timeout: 30 * time.Second}
	src, err := s.refSourceFor(context.Background(), "anvil", clone)
	require.NoError(t, err)

	paths, err := src.Paths(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"web/package.json"}, npmPackageFiles(paths))

	data, err := src.Read(context.Background(), "web/package.json")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"lodash": "^4.17.21"}, parsePackageJSONDeps(data),
		"the committed upstream range, not the local edit and not the stale checkout")

	assert.Equal(t, statusBefore, git(t, clone, "status", "--porcelain"))
}

func TestRefSourceFor_ReportsAnUnusableCheckout(t *testing.T) {
	requireGit(t)
	s := &Scanner{timeout: 30 * time.Second}
	_, err := s.refSourceFor(context.Background(), "anvil", t.TempDir())
	assert.Error(t, err)
}

// TestScanFromRefLeavesAPermanentlyModifiedAnvilAlone is the acceptance case:
// an anvil that can never have a clean working tree is scanned normally, its
// local modifications are intact afterwards, and the answer comes from what
// upstream committed rather than from the stale checkout.
func TestScanFromRefLeavesAPermanentlyModifiedAnvilAlone(t *testing.T) {
	clone := newOriginAndClone(t, map[string]string{
		"web/package.json":   `{"dependencies":{"lodash":"^4.17.20","react":"^18.0.0"}}`,
		".beads/config.yaml": "sync.remote: \"http://laptop:50051/beads\"\n",
	})

	// The override this anvil must carry forever.
	override := "sync.remote: \"http://dolt-beads.svc.cluster.local:50051/beads\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(clone, ".beads", "config.yaml"), []byte(override), 0o644))
	statusBefore := git(t, clone, "status", "--porcelain")

	// Upstream both bumps a dependency and touches the overridden file, which is
	// exactly the pair `git pull --ff-only` refused on.
	pushToOrigin(t, clone, map[string]string{
		"web/package.json":   `{"dependencies":{"lodash":"^4.17.21","react":"^18.0.0"}}`,
		".beads/config.yaml": "# pod and laptop need different values here\nsync.remote: \"http://laptop:50051/beads\"\n",
	})

	// The checkout's node_modules still hold the pre-bump versions, so npm
	// reports both as outdated.
	orig := runNpmOutdatedFn
	t.Cleanup(func() { runNpmOutdatedFn = orig })
	runNpmOutdatedFn = func(_ context.Context, _ time.Duration, _ string) ([]ModuleUpdate, error) {
		return []ModuleUpdate{
			{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"},
			{Path: "react", Current: "18.0.0", Latest: "18.2.0", Kind: "minor"},
		}, nil
	}

	s := &Scanner{timeout: 30 * time.Second}
	src, err := s.refSourceFor(context.Background(), "heimdall", clone)
	require.NoError(t, err, "the scan must not be blocked by local modifications")

	result := s.scanNpm(context.Background(), "heimdall", clone, src)
	require.NotNil(t, result)
	require.NoError(t, result.Error)

	assert.Empty(t, result.Patch, "lodash is already at latest upstream — filing a bead for it duplicates merged work")
	require.Len(t, result.Minor, 1)
	assert.Equal(t, "react", result.Minor[0].Path)

	assert.Equal(t, statusBefore, git(t, clone, "status", "--porcelain"),
		"the working tree must be untouched by the scan")
	kept, err := os.ReadFile(filepath.Join(clone, ".beads", "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, override, string(kept))
}

// TestScanGoFromRefSkipsANonGoAnvil covers the presence check moving off the
// filesystem: a repository with no committed go.mod reports no Go ecosystem.
func TestScanGoFromRefSkipsANonGoAnvil(t *testing.T) {
	clone := newOriginAndClone(t, map[string]string{"web/package.json": "{}"})
	// An untracked go.mod in the checkout is not the anvil's committed state.
	require.NoError(t, os.WriteFile(filepath.Join(clone, "go.mod"), []byte("module stray\n"), 0o644))

	s := &Scanner{timeout: 30 * time.Second}
	src, err := s.refSourceFor(context.Background(), "anvil", clone)
	require.NoError(t, err)

	assert.Nil(t, s.scanGo(context.Background(), "anvil", clone, src))
}

// TestScanDotnetDiscoversProjectsFromTheRef covers project discovery moving off
// the filesystem walk: a .csproj that only exists in an untracked worker
// checkout is not a project of this anvil.
func TestScanDotnetDiscoversProjectsFromTheRef(t *testing.T) {
	clone := newOriginAndClone(t, map[string]string{
		"src/App/App.csproj": `<Project><ItemGroup><PackageReference Include="Serilog" Version="3.1.1" /></ItemGroup></Project>`,
	})
	writeFiles(t, clone, map[string]string{".workers/w1/Other/Other.csproj": "<Project/>"})

	s := &Scanner{timeout: 30 * time.Second}
	src, err := s.refSourceFor(context.Background(), "anvil", clone)
	require.NoError(t, err)

	paths, err := src.Paths(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"src/App/App.csproj"}, dotnetProjectPaths(paths))
	assert.Equal(t, map[string]string{"Serilog": "3.1.1"},
		committedPackageRefs(context.Background(), src, paths))
}
