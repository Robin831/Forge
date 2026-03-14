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
