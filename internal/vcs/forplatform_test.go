package vcs_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/vcs"
	// Register the GitHub VCS provider factory so ForPlatform("github") works.
	_ "github.com/Robin831/Forge/internal/vcs/github"
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
}
