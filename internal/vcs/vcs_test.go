package vcs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePlatform(t *testing.T) {
	tests := []struct {
		input   string
		want    Platform
		wantErr bool
	}{
		{"", GitHub, false},
		{"github", GitHub, false},
		{"gitlab", GitLab, false},
		{"gitea", Gitea, false},
		{"bitbucket", Bitbucket, false},
		{"azuredevops", AzureDevOps, false},
		{"GitHub", GitHub, false},
		{"GITLAB", GitLab, false},
		{"  github  ", GitHub, false},
		{"svn", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParsePlatform(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPRStatus_IsMerged(t *testing.T) {
	assert.True(t, (&PRStatus{State: "MERGED"}).IsMerged())
	assert.False(t, (&PRStatus{State: "OPEN"}).IsMerged())
	assert.False(t, (&PRStatus{State: "CLOSED"}).IsMerged())
}

func TestPRStatus_IsClosed(t *testing.T) {
	assert.True(t, (&PRStatus{State: "CLOSED"}).IsClosed())
	assert.False(t, (&PRStatus{State: "OPEN"}).IsClosed())
	assert.False(t, (&PRStatus{State: "MERGED"}).IsClosed())
}

func TestPRStatus_CIsPassing(t *testing.T) {
	assert.True(t, (&PRStatus{}).CIsPassing(), "no checks = passing")
	assert.True(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Conclusion: "SUCCESS"},
			{Conclusion: "NEUTRAL"},
			{Conclusion: "SKIPPED"},
		},
	}).CIsPassing())
	assert.False(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Conclusion: "SUCCESS"},
			{Conclusion: "FAILURE"},
		},
	}).CIsPassing())
}

func TestPRStatus_HasApproval(t *testing.T) {
	assert.False(t, (&PRStatus{}).HasApproval())
	assert.True(t, (&PRStatus{
		Reviews: []Review{{State: "APPROVED"}},
	}).HasApproval())
	assert.False(t, (&PRStatus{
		Reviews: []Review{{State: "CHANGES_REQUESTED"}},
	}).HasApproval())
}

func TestPRStatus_NeedsChanges(t *testing.T) {
	assert.False(t, (&PRStatus{}).NeedsChanges())
	assert.True(t, (&PRStatus{
		Reviews: []Review{{State: "CHANGES_REQUESTED"}},
	}).NeedsChanges())
	assert.True(t, (&PRStatus{UnresolvedThreads: 1}).NeedsChanges())
}

func TestPRStatus_HasPendingReviewRequests(t *testing.T) {
	assert.False(t, (&PRStatus{}).HasPendingReviewRequests())
	assert.True(t, (&PRStatus{
		ReviewRequests: []ReviewRequest{{Login: "reviewer"}},
	}).HasPendingReviewRequests())
}

func TestForPlatform(t *testing.T) {
	t.Run("gitlab returns GitLabProvider", func(t *testing.T) {
		p, err := ForPlatform("gitlab")
		require.NoError(t, err)
		assert.Equal(t, GitLab, p.Platform())
	})

	// GitHub happy-path ("" and "github") is tested in forplatform_test.go
	// (package vcs_test) which can import internal/vcs/github without a cycle.

	t.Run("invalid platform returns error", func(t *testing.T) {
		_, err := ForPlatform("svn")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown VCS platform")
	})

	t.Run("case insensitive", func(t *testing.T) {
		p, err := ForPlatform("GitLab")
		require.NoError(t, err)
		assert.Equal(t, GitLab, p.Platform())
	})

	t.Run("gitea returns GiteaProvider", func(t *testing.T) {
		p, err := ForPlatform("gitea")
		require.NoError(t, err)
		assert.Equal(t, Gitea, p.Platform())
	})
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"HTTPS no creds", "https://gitea.example.com/owner/repo", "https://gitea.example.com/owner/repo"},
		{"HTTPS with creds", "https://user:pass@gitea.example.com/owner/repo", "https://gitea.example.com/owner/repo"},
		{"HTTP with creds", "http://token:x@localhost:3000/owner/repo", "http://localhost:3000/owner/repo"},
		{"SSH unchanged", "git@gitea.example.com:owner/repo.git", "git@gitea.example.com:owner/repo.git"},
		{"plain path unchanged", "/some/local/path", "/some/local/path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, redactURL(tt.input))
		})
	}
}

func TestMergeabilityFromStatus(t *testing.T) {
	s := &PRStatus{
		Mergeable:         "CONFLICTING",
		UnresolvedThreads: 2,
		ReviewRequests:    []ReviewRequest{{Login: "bot"}},
	}
	m := MergeabilityFromStatus(s)
	assert.True(t, m.HasConflicts)
	assert.True(t, m.HasUnresolvedThreads)
	assert.True(t, m.HasPendingReviews)

	s2 := &PRStatus{Mergeable: "MERGEABLE"}
	m2 := MergeabilityFromStatus(s2)
	assert.False(t, m2.HasConflicts)
	assert.False(t, m2.HasUnresolvedThreads)
	assert.False(t, m2.HasPendingReviews)
}
