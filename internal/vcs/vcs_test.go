package vcs

import (
	"errors"
	"fmt"
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
	// Case-insensitive conclusion
	assert.True(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Conclusion: "success"},
		},
	}).CIsPassing(), "lowercase conclusion should be handled")
	// StatusContext items
	assert.True(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{State: "SUCCESS", Context: "ci/build"},
		},
	}).CIsPassing(), "StatusContext SUCCESS = passing")
	assert.False(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{State: "FAILURE", Context: "ci/build"},
		},
	}).CIsPassing(), "StatusContext FAILURE = not passing")
	assert.False(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{State: "PENDING", Context: "ci/build"},
		},
	}).CIsPassing(), "StatusContext PENDING = not passing")
	assert.False(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{State: "ERROR", Context: "ci/build"},
		},
	}).CIsPassing(), "StatusContext ERROR = not passing")
	// Mixed CheckRun and StatusContext
	assert.True(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{State: "SUCCESS", Context: "ci/deploy"},
		},
	}).CIsPassing(), "mixed CheckRun+StatusContext all passing")
	assert.False(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{State: "FAILURE", Context: "ci/deploy"},
		},
	}).CIsPassing(), "mixed CheckRun passing + StatusContext failing = not passing")
}

func TestPRStatus_CIsInProgress(t *testing.T) {
	assert.False(t, (&PRStatus{}).CIsInProgress(), "no checks = not in progress")
	assert.False(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Status: "COMPLETED", Conclusion: "FAILURE"},
		},
	}).CIsInProgress(), "all completed = not in progress")
	assert.True(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Status: "IN_PROGRESS", Conclusion: ""},
		},
	}).CIsInProgress(), "one in_progress = in progress")
	assert.True(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Status: "QUEUED", Conclusion: ""},
		},
	}).CIsInProgress(), "queued = in progress")
	assert.True(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Status: "PENDING", Conclusion: ""},
		},
	}).CIsInProgress(), "pending = in progress")
	assert.True(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Status: "WAITING", Conclusion: ""},
		},
	}).CIsInProgress(), "waiting = in progress")
	assert.True(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Status: "REQUESTED", Conclusion: ""},
		},
	}).CIsInProgress(), "requested = in progress")
	assert.True(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Status: "", Conclusion: ""},
		},
	}).CIsInProgress(), "empty status with empty conclusion = in progress")
	assert.True(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Status: "COMPLETED", Conclusion: ""},
		},
	}).CIsInProgress(), "completed with empty conclusion = transient, treat as in progress")
	assert.False(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Status: "COMPLETED", Conclusion: "NEUTRAL"},
		},
	}).CIsInProgress(), "all completed success/neutral = not in progress")
	// StatusContext items
	assert.True(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{State: "PENDING", Context: "ci/build"},
		},
	}).CIsInProgress(), "StatusContext PENDING = in progress")
	assert.True(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{State: "EXPECTED", Context: "ci/build"},
		},
	}).CIsInProgress(), "StatusContext EXPECTED = in progress")
	assert.False(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{State: "SUCCESS", Context: "ci/build"},
		},
	}).CIsInProgress(), "StatusContext SUCCESS = not in progress")
	assert.False(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{State: "FAILURE", Context: "ci/build"},
		},
	}).CIsInProgress(), "StatusContext FAILURE = not in progress (completed)")
	assert.False(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{State: "ERROR", Context: "ci/build"},
		},
	}).CIsInProgress(), "StatusContext ERROR = not in progress (completed)")
	// Mixed: one CheckRun completed, one StatusContext pending
	assert.True(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "FAILURE"},
			{State: "PENDING", Context: "ci/deploy"},
		},
	}).CIsInProgress(), "CheckRun completed+failure with StatusContext pending = in progress")
	// Mixed: all completed
	assert.False(t, (&PRStatus{
		StatusCheckRollup: []CheckRun{
			{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
			{State: "SUCCESS", Context: "ci/deploy"},
		},
	}).CIsInProgress(), "all completed across CheckRun and StatusContext = not in progress")
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

func TestErrPRAlreadyExists(t *testing.T) {
	t.Run("sentinel is detectable with errors.Is", func(t *testing.T) {
		wrapped := fmt.Errorf("gh pr create: %w: A pull request already exists for branch", ErrPRAlreadyExists)
		assert.True(t, errors.Is(wrapped, ErrPRAlreadyExists))
	})

	t.Run("double-wrapped sentinel is detectable", func(t *testing.T) {
		inner := fmt.Errorf("gitea create PR: %w: 409 conflict", ErrPRAlreadyExists)
		outer := fmt.Errorf("provider error: %w", inner)
		assert.True(t, errors.Is(outer, ErrPRAlreadyExists))
	})

	t.Run("unrelated error is not detected", func(t *testing.T) {
		other := fmt.Errorf("gh pr create failed: permission denied")
		assert.False(t, errors.Is(other, ErrPRAlreadyExists))
	})
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
