package ghpr

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractPRNumber(t *testing.T) {
	tests := []struct {
		url      string
		expected int
	}{
		{"https://github.com/Robin831/Forge/pull/123", 123},
		{"https://github.com/owner/repo/pull/1", 1},
		{"invalid", 0},
		{"", 0},
	}

	for _, tt := range tests {
		got := extractPRNumber(tt.url)
		if got != tt.expected {
			t.Errorf("extractPRNumber(%q) = %d; want %d", tt.url, got, tt.expected)
		}
	}
}

func TestParseRepoURL(t *testing.T) {
	tests := []struct {
		url         string
		wantOwner   string
		wantRepo    string
		wantErr     bool
	}{
		{"https://github.com/Robin831/Forge", "Robin831", "Forge", false},
		{"https://github.com/Robin831/Forge.git", "Robin831", "Forge", false},
		{"git@github.com:Robin831/Forge.git", "Robin831", "Forge", false},
		{"git@github.com:Robin831/Forge", "Robin831", "Forge", false},
		{"https://github.com/owner/repo/extra", "owner", "repo", false},
		{"invalid", "", "", true},
		{"", "", "", true},
	}

	for _, tt := range tests {
		owner, repo, err := ParseRepoURL(tt.url)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseRepoURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			continue
		}
		if owner != tt.wantOwner || repo != tt.wantRepo {
			t.Errorf("ParseRepoURL(%q) = (%q, %q); want (%q, %q)", tt.url, owner, repo, tt.wantOwner, tt.wantRepo)
		}
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
	tests := []struct {
		name   string
		status PRStatus
		want   bool
	}{
		{"no checks → passing", PRStatus{}, true},
		{"all success", PRStatus{StatusCheckRollup: []CheckRun{{Conclusion: "SUCCESS"}, {Conclusion: "SUCCESS"}}}, true},
		{"neutral is ok", PRStatus{StatusCheckRollup: []CheckRun{{Conclusion: "NEUTRAL"}}}, true},
		{"skipped is ok", PRStatus{StatusCheckRollup: []CheckRun{{Conclusion: "SKIPPED"}}}, true},
		{"one failure", PRStatus{StatusCheckRollup: []CheckRun{{Conclusion: "SUCCESS"}, {Conclusion: "FAILURE"}}}, false},
		{"pending", PRStatus{StatusCheckRollup: []CheckRun{{Conclusion: ""}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.status.CIsPassing())
		})
	}
}

func TestPRStatus_HasApproval(t *testing.T) {
	tests := []struct {
		name   string
		status PRStatus
		want   bool
	}{
		{"no reviews", PRStatus{}, false},
		{"approved", PRStatus{Reviews: []Review{{State: "APPROVED"}}}, true},
		{"changes requested only", PRStatus{Reviews: []Review{{State: "CHANGES_REQUESTED"}}}, false},
		{"mixed with approval", PRStatus{Reviews: []Review{{State: "CHANGES_REQUESTED"}, {State: "APPROVED"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.status.HasApproval())
		})
	}
}

func TestPRStatus_NeedsChanges(t *testing.T) {
	tests := []struct {
		name   string
		status PRStatus
		want   bool
	}{
		{"no reviews, no threads", PRStatus{}, false},
		{"changes requested", PRStatus{Reviews: []Review{{State: "CHANGES_REQUESTED"}}}, true},
		{"approved only", PRStatus{Reviews: []Review{{State: "APPROVED"}}}, false},
		{"unresolved threads", PRStatus{UnresolvedThreads: 2}, true},
		{"zero unresolved threads", PRStatus{UnresolvedThreads: 0}, false},
		{"both changes and threads", PRStatus{Reviews: []Review{{State: "CHANGES_REQUESTED"}}, UnresolvedThreads: 1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.status.NeedsChanges())
		})
	}
}

func TestPRStatus_HasPendingReviewRequests(t *testing.T) {
	tests := []struct {
		name   string
		status PRStatus
		want   bool
	}{
		{"no review requests", PRStatus{}, false},
		{"one pending user review", PRStatus{ReviewRequests: []ReviewRequest{{Login: "copilot"}}}, true},
		{"one pending team review", PRStatus{ReviewRequests: []ReviewRequest{{Slug: "my-team"}}}, true},
		{"multiple pending reviews", PRStatus{ReviewRequests: []ReviewRequest{{Login: "alice"}, {Login: "bob"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.status.HasPendingReviewRequests())
		})
	}
}

// TestReviewAuthorUnmarshal is a regression test for the bug where Review.Author
// was typed as string and failed JSON unmarshaling when GitHub returned a nested
// {"login":"..."} object, silently producing empty reviews and suppressing all
// bellows review-change events.
func TestReviewAuthorUnmarshal(t *testing.T) {
	payload := `{"reviews":[{"author":{"login":"octocat"},"state":"CHANGES_REQUESTED","body":"please fix"},{"author":{"login":"alice"},"state":"APPROVED","body":""}]}`
	var status PRStatus
	if err := json.Unmarshal([]byte(payload), &status); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if len(status.Reviews) != 2 {
		t.Fatalf("expected 2 reviews, got %d", len(status.Reviews))
	}
	assert.Equal(t, "octocat", status.Reviews[0].Author.Login)
	assert.Equal(t, "CHANGES_REQUESTED", status.Reviews[0].State)
	assert.Equal(t, "alice", status.Reviews[1].Author.Login)
	assert.Equal(t, "APPROVED", status.Reviews[1].State)
	assert.True(t, status.NeedsChanges(), "CHANGES_REQUESTED review should cause NeedsChanges")
	assert.True(t, status.HasApproval(), "APPROVED review should be detected")
}

