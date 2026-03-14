package vcs_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/vcs"
	// Register the GitHub VCS provider factory so ForPlatform("github") works.
	githubvcs "github.com/Robin831/Forge/internal/vcs/github"
)

func TestForPlatform(t *testing.T) {
	t.Run("gitlab returns GitLabProvider", func(t *testing.T) {
		p, err := vcs.ForPlatform("gitlab")
		require.NoError(t, err)
		assert.Equal(t, vcs.GitLab, p.Platform())
	})

	t.Run("empty string defaults to github", func(t *testing.T) {
		p, err := vcs.ForPlatform("")
		require.NoError(t, err)
		assert.Equal(t, vcs.GitHub, p.Platform())
	})

	t.Run("explicit github", func(t *testing.T) {
		p, err := vcs.ForPlatform("github")
		require.NoError(t, err)
		assert.Equal(t, vcs.GitHub, p.Platform())
	})

	t.Run("invalid platform returns error", func(t *testing.T) {
		_, err := vcs.ForPlatform("svn")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown VCS platform")
	})

	t.Run("case insensitive", func(t *testing.T) {
		p, err := vcs.ForPlatform("GitLab")
		require.NoError(t, err)
		assert.Equal(t, vcs.GitLab, p.Platform())
	})

	t.Run("github factory not registered returns error", func(t *testing.T) {
		// Temporarily unregister the GitHub factory, then restore it.
		vcs.RegisterGitHubProvider(nil)
		defer vcs.RegisterGitHubProvider(func() vcs.Provider { return githubvcs.New(nil) })

		_, err := vcs.ForPlatform("github")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "GitHub VCS provider not available")
	})

	// These platforms are recognised but not yet implemented; the default
	// branch in ForPlatform should return a stable "not yet implemented" error.
	for _, platform := range []string{"gitea", "bitbucket", "azuredevops"} {
		t.Run("not yet implemented: "+platform, func(t *testing.T) {
			_, err := vcs.ForPlatform(platform)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not yet implemented")
		})
	}
}
