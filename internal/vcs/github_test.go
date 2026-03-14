package vcs

import (
	"testing"

	"github.com/Robin831/Forge/internal/ghpr"
	"github.com/stretchr/testify/assert"
)

func TestGitHubProvider_Platform(t *testing.T) {
	p := NewGitHubProvider()
	assert.Equal(t, GitHub, p.Platform())
}

func TestConvertGHPRStatus(t *testing.T) {
	t.Run("full status conversion", func(t *testing.T) {
		ghStatus := &ghpr.PRStatus{
			State:             "OPEN",
			Mergeable:         "MERGEABLE",
			UnresolvedThreads: 3,
			HeadRefName:       "feature/test",
			URL:               "https://github.com/org/repo/pull/42",
			StatusCheckRollup: []ghpr.CheckRun{
				{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Name: "lint", Status: "IN_PROGRESS", Conclusion: ""},
			},
			Reviews: []ghpr.Review{
				{Author: ghpr.ReviewAuthor{Login: "alice"}, State: "APPROVED", Body: "LGTM"},
				{Author: ghpr.ReviewAuthor{Login: "bob"}, State: "CHANGES_REQUESTED", Body: "Fix typo"},
			},
			ReviewRequests: []ghpr.ReviewRequest{
				{Login: "charlie", Slug: "", Name: ""},
				{Login: "", Slug: "security-team", Name: "Security Team"},
			},
		}

		result := convertGHPRStatus(ghStatus)

		assert.Equal(t, "OPEN", result.State)
		assert.Equal(t, "MERGEABLE", result.Mergeable)
		assert.Equal(t, 3, result.UnresolvedThreads)
		assert.Equal(t, "feature/test", result.HeadRefName)
		assert.Equal(t, "https://github.com/org/repo/pull/42", result.URL)

		// Check runs
		assert.Len(t, result.StatusCheckRollup, 2)
		assert.Equal(t, "build", result.StatusCheckRollup[0].Name)
		assert.Equal(t, "SUCCESS", result.StatusCheckRollup[0].Conclusion)
		assert.Equal(t, "lint", result.StatusCheckRollup[1].Name)

		// Reviews
		assert.Len(t, result.Reviews, 2)
		assert.Equal(t, "alice", result.Reviews[0].Author.Login)
		assert.Equal(t, "APPROVED", result.Reviews[0].State)
		assert.Equal(t, "LGTM", result.Reviews[0].Body)
		assert.Equal(t, "bob", result.Reviews[1].Author.Login)
		assert.Equal(t, "CHANGES_REQUESTED", result.Reviews[1].State)

		// Review requests
		assert.Len(t, result.ReviewRequests, 2)
		assert.Equal(t, "charlie", result.ReviewRequests[0].Login)
		assert.Equal(t, "security-team", result.ReviewRequests[1].Slug)
		assert.Equal(t, "Security Team", result.ReviewRequests[1].Name)
	})

	t.Run("empty status conversion", func(t *testing.T) {
		ghStatus := &ghpr.PRStatus{
			State:     "MERGED",
			Mergeable: "UNKNOWN",
		}

		result := convertGHPRStatus(ghStatus)

		assert.Equal(t, "MERGED", result.State)
		assert.Equal(t, "UNKNOWN", result.Mergeable)
		assert.Empty(t, result.StatusCheckRollup)
		assert.Empty(t, result.Reviews)
		assert.Empty(t, result.ReviewRequests)
		assert.Equal(t, 0, result.UnresolvedThreads)
	})

	t.Run("converted status methods work correctly", func(t *testing.T) {
		ghStatus := &ghpr.PRStatus{
			State:     "OPEN",
			Mergeable: "CONFLICTING",
			StatusCheckRollup: []ghpr.CheckRun{
				{Conclusion: "SUCCESS"},
				{Conclusion: "FAILURE"},
			},
			Reviews: []ghpr.Review{
				{State: "CHANGES_REQUESTED"},
			},
			ReviewRequests: []ghpr.ReviewRequest{
				{Login: "reviewer"},
			},
			UnresolvedThreads: 1,
		}

		result := convertGHPRStatus(ghStatus)

		assert.False(t, result.IsMerged())
		assert.False(t, result.IsClosed())
		assert.False(t, result.CIsPassing())
		assert.False(t, result.HasApproval())
		assert.True(t, result.NeedsChanges())
		assert.True(t, result.HasPendingReviewRequests())

		m := MergeabilityFromStatus(result)
		assert.True(t, m.HasConflicts)
		assert.True(t, m.HasUnresolvedThreads)
		assert.True(t, m.HasPendingReviews)
	})
}

func TestForPlatform_ProviderInterface(t *testing.T) {
	// Verify that both providers satisfy the Provider interface at compile time
	// and return the correct platform.
	tests := []struct {
		platform string
		want     Platform
	}{
		{"", GitHub},
		{"github", GitHub},
		{"GitHub", GitHub},
		{"  GITHUB  ", GitHub},
		{"gitlab", GitLab},
		{"GitLab", GitLab},
		{"  GITLAB  ", GitLab},
	}

	for _, tt := range tests {
		t.Run(tt.platform, func(t *testing.T) {
			p, err := ForPlatform(tt.platform)
			assert.NoError(t, err)
			assert.Equal(t, tt.want, p.Platform())

			// Verify the provider implements the full interface
			var _ Provider = p
		})
	}
}

func TestForPlatform_Unimplemented(t *testing.T) {
	unimplemented := []string{"gitea", "bitbucket", "azuredevops"}
	for _, platform := range unimplemented {
		t.Run(platform, func(t *testing.T) {
			_, err := ForPlatform(platform)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "not yet implemented")
		})
	}
}
