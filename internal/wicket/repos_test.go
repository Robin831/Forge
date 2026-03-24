package wicket

import (
	"context"
	"errors"
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubGitRemote returns a gitRemoteFunc that always returns the given URL.
func stubGitRemote(url string) func(ctx context.Context, dir string) (string, error) {
	return func(_ context.Context, _ string) (string, error) {
		return url, nil
	}
}

// errGitRemote returns a gitRemoteFunc that always returns the given error.
func errGitRemote(err error) func(ctx context.Context, dir string) (string, error) {
	return func(_ context.Context, _ string) (string, error) {
		return "", err
	}
}

// TestResolveRepos_ExplicitList verifies that when WicketRepos is set, the git
// remote is never consulted and the configured list is returned verbatim.
func TestResolveRepos_ExplicitList(t *testing.T) {
	anvilCfg := config.AnvilConfig{
		Path:        "/some/path",
		WicketRepos: []string{"owner/repo-a", "owner/repo-b"},
	}
	gitCalled := false
	r := &RepoResolver{
		gitRemoteFunc: func(_ context.Context, _ string) (string, error) {
			gitCalled = true
			return "", errors.New("should not be called")
		},
	}

	repos, err := r.ResolveRepos(context.Background(), anvilCfg)
	require.NoError(t, err)
	assert.False(t, gitCalled, "git remote should not be called when WicketRepos is set")
	assert.Equal(t, []string{"owner/repo-a", "owner/repo-b"}, repos)
}

// TestResolveRepos_MultipleExplicit verifies that multiple explicit repos are
// all returned without modification.
func TestResolveRepos_MultipleExplicit(t *testing.T) {
	anvilCfg := config.AnvilConfig{
		WicketRepos: []string{"org/frontend", "org/backend", "org/shared"},
	}
	r := &RepoResolver{
		gitRemoteFunc: errGitRemote(errors.New("should not be called")),
	}

	repos, err := r.ResolveRepos(context.Background(), anvilCfg)
	require.NoError(t, err)
	assert.Equal(t, []string{"org/frontend", "org/backend", "org/shared"}, repos)
}

// TestResolveRepos_DeriveFromGitRemoteSSH verifies that an SSH remote URL is
// correctly parsed into an "owner/repo" slug when WicketRepos is empty.
func TestResolveRepos_DeriveFromGitRemoteSSH(t *testing.T) {
	anvilCfg := config.AnvilConfig{
		Path: "/srv/repos/myrepo",
	}
	r := &RepoResolver{
		gitRemoteFunc: stubGitRemote("git@github.com:acme/widgets.git"),
	}

	repos, err := r.ResolveRepos(context.Background(), anvilCfg)
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/widgets"}, repos)
}

// TestResolveRepos_DeriveFromGitRemoteHTTPS verifies that an HTTPS remote URL
// is correctly parsed when WicketRepos is empty.
func TestResolveRepos_DeriveFromGitRemoteHTTPS(t *testing.T) {
	anvilCfg := config.AnvilConfig{
		Path: "/srv/repos/myrepo",
	}
	r := &RepoResolver{
		gitRemoteFunc: stubGitRemote("https://github.com/acme/widgets"),
	}

	repos, err := r.ResolveRepos(context.Background(), anvilCfg)
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/widgets"}, repos)
}

// TestResolveRepos_DeriveFromGitRemoteHTTPSWithDotGit verifies the .git suffix
// is stripped from HTTPS URLs.
func TestResolveRepos_DeriveFromGitRemoteHTTPSWithDotGit(t *testing.T) {
	anvilCfg := config.AnvilConfig{
		Path: "/srv/repos/myrepo",
	}
	r := &RepoResolver{
		gitRemoteFunc: stubGitRemote("https://github.com/acme/widgets.git"),
	}

	repos, err := r.ResolveRepos(context.Background(), anvilCfg)
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/widgets"}, repos)
}

// TestResolveRepos_GitRemoteError verifies that an error from the git command
// is propagated as an error from ResolveRepos.
func TestResolveRepos_GitRemoteError(t *testing.T) {
	anvilCfg := config.AnvilConfig{
		Path: "/srv/repos/not-a-repo",
	}
	r := &RepoResolver{
		gitRemoteFunc: errGitRemote(errors.New("not a git repo")),
	}

	repos, err := r.ResolveRepos(context.Background(), anvilCfg)
	require.Error(t, err)
	assert.Nil(t, repos)
}

// TestResolveRepos_UnparsableURL verifies that a remote URL that cannot be
// recognised as a GitHub URL returns an appropriate error.
func TestResolveRepos_UnparsableURL(t *testing.T) {
	anvilCfg := config.AnvilConfig{
		Path: "/srv/repos/non-github",
	}
	r := &RepoResolver{
		gitRemoteFunc: stubGitRemote("https://gitlab.com/acme/widgets"),
	}

	repos, err := r.ResolveRepos(context.Background(), anvilCfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot parse GitHub owner/repo")
	assert.Nil(t, repos)
}

// TestParseGitHubRepo covers the URL parsing helper directly.
func TestParseGitHubRepo(t *testing.T) {
	tests := []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{"git@github.com:owner/myrepo.git", "owner/myrepo", false},
		{"git@github.com:owner/myrepo", "owner/myrepo", false},
		{"https://github.com/owner/myrepo.git", "owner/myrepo", false},
		{"https://github.com/owner/myrepo", "owner/myrepo", false},
		{"http://github.com/owner/myrepo", "owner/myrepo", false},
		{"https://gitlab.com/owner/myrepo", "", true},
		{"not-a-url-at-all", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseGitHubRepo(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
