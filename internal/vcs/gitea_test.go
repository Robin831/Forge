package vcs

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGiteaProvider_Platform(t *testing.T) {
	p := NewGiteaProvider()
	assert.Equal(t, Gitea, p.Platform())
}

func TestParseGiteaRepoURL(t *testing.T) {
	tests := []struct {
		name        string
		url         string
		wantBaseURL string
		wantOwner   string
		wantRepo    string
		wantErr     bool
	}{
		{
			name:        "HTTPS simple",
			url:         "https://gitea.example.com/myorg/myrepo.git",
			wantBaseURL: "https://gitea.example.com",
			wantOwner:   "myorg",
			wantRepo:    "myrepo",
		},
		{
			name:        "HTTPS without .git",
			url:         "https://gitea.example.com/myorg/myrepo",
			wantBaseURL: "https://gitea.example.com",
			wantOwner:   "myorg",
			wantRepo:    "myrepo",
		},
		{
			name:        "SSH simple",
			url:         "git@gitea.example.com:myorg/myrepo.git",
			wantBaseURL: "https://gitea.example.com",
			wantOwner:   "myorg",
			wantRepo:    "myrepo",
		},
		{
			name:        "SSH without .git",
			url:         "git@gitea.example.com:myorg/myrepo",
			wantBaseURL: "https://gitea.example.com",
			wantOwner:   "myorg",
			wantRepo:    "myrepo",
		},
		{
			name:        "Forgejo HTTPS",
			url:         "https://codeberg.org/user/project.git",
			wantBaseURL: "https://codeberg.org",
			wantOwner:   "user",
			wantRepo:    "project",
		},
		{
			name:        "Forgejo SSH",
			url:         "git@codeberg.org:user/project.git",
			wantBaseURL: "https://codeberg.org",
			wantOwner:   "user",
			wantRepo:    "project",
		},
		{
			name:        "HTTP (non-TLS)",
			url:         "http://localhost:3000/admin/testrepo.git",
			wantBaseURL: "http://localhost:3000",
			wantOwner:   "admin",
			wantRepo:    "testrepo",
		},
		{
			name:        "HTTPS with port",
			url:         "https://gitea.internal:8443/team/project.git",
			wantBaseURL: "https://gitea.internal:8443",
			wantOwner:   "team",
			wantRepo:    "project",
		},
		{
			name:    "invalid URL",
			url:     "not-a-url",
			wantErr: true,
		},
		{
			name:        "SSH with port prefix",
			url:         "git@gitea.example.com:2222/myorg/myrepo.git",
			wantBaseURL: "https://gitea.example.com",
			wantOwner:   "myorg",
			wantRepo:    "myrepo",
		},
		{
			name:    "SSH no repo",
			url:     "git@gitea.example.com:onlyone",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseURL, owner, repo, err := ParseGiteaRepoURL(tt.url)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantBaseURL, baseURL)
			assert.Equal(t, tt.wantOwner, owner)
			assert.Equal(t, tt.wantRepo, repo)
		})
	}
}

func TestMapGiteaState(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"open", "OPEN"},
		{"closed", "CLOSED"},
		{"Open", "OPEN"},
		{"CLOSED", "CLOSED"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, mapGiteaState(tt.input))
		})
	}
}

func TestMapGiteaMergeable(t *testing.T) {
	tests := []struct {
		name      string
		mergeable bool
		state     string
		want      string
	}{
		{"open and mergeable", true, "open", "MERGEABLE"},
		{"open and conflicting", false, "open", "CONFLICTING"},
		{"closed", true, "closed", "UNKNOWN"},
		{"closed not mergeable", false, "closed", "UNKNOWN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mapGiteaMergeable(tt.mergeable, tt.state))
		})
	}
}

func TestMapGiteaReviewState(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"APPROVED", "APPROVED"},
		{"REQUEST_CHANGES", "CHANGES_REQUESTED"},
		{"REJECTED", "CHANGES_REQUESTED"},
		{"COMMENT", ""},
		{"unknown", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, mapGiteaReviewState(tt.input))
		})
	}
}

func TestMapGiteaStatusState(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"success", "COMPLETED"},
		{"failure", "COMPLETED"},
		{"error", "COMPLETED"},
		{"pending", "IN_PROGRESS"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, mapGiteaStatusState(tt.input))
		})
	}
}

func TestMapGiteaStatusConclusion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"success", "SUCCESS"},
		{"failure", "FAILURE"},
		{"error", "FAILURE"},
		{"pending", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, mapGiteaStatusConclusion(tt.input))
		})
	}
}

func TestSplitGiteaPath(t *testing.T) {
	tests := []struct {
		path      string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"owner/repo", "owner", "repo", false},
		{"myorg/myproject", "myorg", "myproject", false},
		{"2222/owner/repo", "owner", "repo", false},
		{"22/org/project", "org", "project", false},
		{"2222/onlyone", "", "", true},
		{"onlyone", "", "", true},
		{"", "", "", true},
		{"/", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			owner, repo, err := splitGiteaPath(tt.path, "test-url")
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOwner, owner)
			assert.Equal(t, tt.wantRepo, repo)
		})
	}
}

// TestGiteaPRParsing tests JSON deserialization of Gitea PR API responses.
func TestGiteaPRParsing(t *testing.T) {
	t.Run("open PR with head ref", func(t *testing.T) {
		raw := `{
			"number": 42,
			"title": "Add feature X",
			"body": "Description of changes",
			"state": "open",
			"html_url": "https://gitea.example.com/org/repo/pulls/42",
			"mergeable": true,
			"head": {"ref": "feature/test", "sha": "abc123"},
			"base": {"ref": "main", "sha": "def456"}
		}`

		var pr giteaPullRequest
		require.NoError(t, json.Unmarshal([]byte(raw), &pr))

		assert.Equal(t, 42, pr.Number)
		assert.Equal(t, "Add feature X", pr.Title)
		assert.Equal(t, "open", pr.State)
		assert.True(t, pr.Mergeable)
		assert.Equal(t, "feature/test", pr.Head.Ref)
		assert.Equal(t, "abc123", pr.Head.SHA)
		assert.Equal(t, "main", pr.Base.Ref)

		status := &PRStatus{
			State:       mapGiteaState(pr.State),
			Mergeable:   mapGiteaMergeable(pr.Mergeable, pr.State),
			HeadRefName: pr.Head.Ref,
			URL:         pr.HTMLURL,
		}

		assert.Equal(t, "OPEN", status.State)
		assert.Equal(t, "MERGEABLE", status.Mergeable)
		assert.Equal(t, "feature/test", status.HeadRefName)
	})

	t.Run("closed PR not mergeable", func(t *testing.T) {
		raw := `{
			"number": 7,
			"title": "Fix bug",
			"body": "",
			"state": "closed",
			"html_url": "https://gitea.example.com/org/repo/pulls/7",
			"mergeable": false,
			"head": {"ref": "fix/bug", "sha": ""},
			"base": {"ref": "main", "sha": ""}
		}`

		var pr giteaPullRequest
		require.NoError(t, json.Unmarshal([]byte(raw), &pr))

		status := &PRStatus{
			State:     mapGiteaState(pr.State),
			Mergeable: mapGiteaMergeable(pr.Mergeable, pr.State),
		}

		assert.Equal(t, "CLOSED", status.State)
		assert.Equal(t, "UNKNOWN", status.Mergeable)
	})
}

// TestGiteaReviewParsing tests JSON deserialization of Gitea review API responses.
func TestGiteaReviewParsing(t *testing.T) {
	raw := `[
		{"id": 1, "body": "LGTM", "state": "APPROVED", "user": {"login": "alice"}},
		{"id": 2, "body": "Fix this", "state": "REQUEST_CHANGES", "user": {"login": "bob"}},
		{"id": 3, "body": "Nice work", "state": "COMMENT", "user": {"login": "charlie"}}
	]`

	var giteaReviews []giteaReview
	require.NoError(t, json.Unmarshal([]byte(raw), &giteaReviews))

	var reviews []Review
	for _, r := range giteaReviews {
		state := mapGiteaReviewState(r.State)
		if state == "" {
			continue
		}
		reviews = append(reviews, Review{
			Author: ReviewAuthor{Login: r.User.Login},
			State:  state,
			Body:   r.Body,
		})
	}

	assert.Len(t, reviews, 2)
	assert.Equal(t, "alice", reviews[0].Author.Login)
	assert.Equal(t, "APPROVED", reviews[0].State)
	assert.Equal(t, "bob", reviews[1].Author.Login)
	assert.Equal(t, "CHANGES_REQUESTED", reviews[1].State)
}

// TestGiteaRequestedReviewersParsing tests JSON deserialization of the requested reviewers response.
func TestGiteaRequestedReviewersParsing(t *testing.T) {
	raw := `{
		"users": [
			{"login": "alice", "full_name": "Alice A"},
			{"login": "bob", "full_name": "Bob B"}
		],
		"teams": [
			{"id": 5, "name": "security-team"}
		]
	}`

	var result giteaRequestedReviewers
	require.NoError(t, json.Unmarshal([]byte(raw), &result))

	var requests []ReviewRequest
	for _, u := range result.Users {
		requests = append(requests, ReviewRequest{
			Login: u.Login,
			Name:  u.FullName,
		})
	}
	for _, team := range result.Teams {
		requests = append(requests, ReviewRequest{
			Slug: "5",
			Name: team.Name,
		})
	}

	assert.Len(t, requests, 3)
	assert.Equal(t, "alice", requests[0].Login)
	assert.Equal(t, "Alice A", requests[0].Name)
	assert.Equal(t, "bob", requests[1].Login)
	assert.Equal(t, "5", requests[2].Slug)
	assert.Equal(t, "security-team", requests[2].Name)
}

// TestGiteaCombinedStatusParsing tests JSON deserialization of combined commit status.
func TestGiteaCombinedStatusParsing(t *testing.T) {
	raw := `{
		"state": "success",
		"statuses": [
			{"context": "ci/build", "status": "success"},
			{"context": "ci/test", "status": "failure"},
			{"context": "ci/lint", "status": "pending"}
		]
	}`

	var combined giteaCombinedStatus
	require.NoError(t, json.Unmarshal([]byte(raw), &combined))

	assert.Equal(t, "success", combined.State)
	assert.Len(t, combined.Statuses, 3)

	var checks []CheckRun
	for _, s := range combined.Statuses {
		checks = append(checks, CheckRun{
			Name:       s.Context,
			Status:     mapGiteaStatusState(s.Status),
			Conclusion: mapGiteaStatusConclusion(s.Status),
		})
	}

	assert.Len(t, checks, 3)
	assert.Equal(t, "ci/build", checks[0].Name)
	assert.Equal(t, "COMPLETED", checks[0].Status)
	assert.Equal(t, "SUCCESS", checks[0].Conclusion)
	assert.Equal(t, "ci/test", checks[1].Name)
	assert.Equal(t, "FAILURE", checks[1].Conclusion)
	assert.Equal(t, "ci/lint", checks[2].Name)
	assert.Equal(t, "IN_PROGRESS", checks[2].Status)
	assert.Equal(t, "", checks[2].Conclusion)
}

// TestGiteaPRListParsing tests JSON deserialization of Gitea PR list response.
func TestGiteaPRListParsing(t *testing.T) {
	raw := `[
		{"number": 1, "title": "First PR", "body": "First desc", "state": "open", "html_url": "https://gitea.example.com/o/r/pulls/1", "mergeable": true, "head": {"ref": "feature/one", "sha": "aaa"}, "base": {"ref": "main", "sha": "bbb"}},
		{"number": 2, "title": "Second PR", "body": "", "state": "open", "html_url": "https://gitea.example.com/o/r/pulls/2", "mergeable": false, "head": {"ref": "fix/two", "sha": "ccc"}, "base": {"ref": "main", "sha": "ddd"}}
	]`

	var prs []giteaPullRequest
	require.NoError(t, json.Unmarshal([]byte(raw), &prs))

	out := make([]OpenPR, len(prs))
	for i, pr := range prs {
		out[i] = OpenPR{
			Number: pr.Number,
			Title:  pr.Title,
			Branch: pr.Head.Ref,
			Body:   pr.Body,
		}
	}

	assert.Len(t, out, 2)
	assert.Equal(t, 1, out[0].Number)
	assert.Equal(t, "First PR", out[0].Title)
	assert.Equal(t, "feature/one", out[0].Branch)
	assert.Equal(t, "First desc", out[0].Body)
	assert.Equal(t, 2, out[1].Number)
	assert.Equal(t, "", out[1].Body)
}

func TestForPlatform_Gitea(t *testing.T) {
	tests := []struct {
		platform string
		want     Platform
	}{
		{"gitea", Gitea},
		{"Gitea", Gitea},
		{"  GITEA  ", Gitea},
	}
	for _, tt := range tests {
		t.Run(tt.platform, func(t *testing.T) {
			p, err := ForPlatform(tt.platform)
			assert.NoError(t, err)
			assert.Equal(t, tt.want, p.Platform())
			var _ Provider = p
		})
	}
}
