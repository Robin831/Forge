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
			name:        "SCP-style path is literal not port",
			url:         "git@gitea.example.com:2222/myorg/myrepo.git",
			wantBaseURL: "https://gitea.example.com",
			wantOwner:   "2222",
			wantRepo:    "myorg",
		},
		{
			name:        "ssh:// scheme without port",
			url:         "ssh://git@gitea.example.com/myorg/myrepo.git",
			wantBaseURL: "https://gitea.example.com",
			wantOwner:   "myorg",
			wantRepo:    "myrepo",
		},
		{
			name:        "ssh:// scheme with port",
			url:         "ssh://git@gitea.example.com:2222/myorg/myrepo.git",
			wantBaseURL: "https://gitea.example.com",
			wantOwner:   "myorg",
			wantRepo:    "myrepo",
		},
		{
			name:    "SSH no repo",
			url:     "git@gitea.example.com:onlyone",
			wantErr: true,
		},
		{
			name:        "HTTPS subpath deployment",
			url:         "https://host.example.com/gitea/owner/repo.git",
			wantBaseURL: "https://host.example.com/gitea",
			wantOwner:   "owner",
			wantRepo:    "repo",
		},
		{
			name:        "ssh:// subpath deployment",
			url:         "ssh://git@host.example.com/gitea/owner/repo.git",
			wantBaseURL: "https://host.example.com/gitea",
			wantOwner:   "owner",
			wantRepo:    "repo",
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
		path        string
		wantSubpath string
		wantOwner   string
		wantRepo    string
		wantErr     bool
	}{
		// Simple owner/repo — no subpath.
		{"owner/repo", "", "owner", "repo", false},
		{"myorg/myproject", "", "myorg", "myproject", false},
		// Three-segment paths: leading segment becomes subpath, last two are owner/repo.
		// This supports Gitea instances hosted at a URL subpath (e.g. /gitea/owner/repo).
		{"gitea/owner/repo", "gitea", "owner", "repo", false},
		{"sub/org/project", "sub", "org", "project", false},
		// Error cases.
		{"onlyone", "", "", "", true},
		{"", "", "", "", true},
		{"/", "", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			subpath, owner, repo, err := splitGiteaPath(tt.path, "test-url")
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantSubpath, subpath)
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

	t.Run("merged PR", func(t *testing.T) {
		raw := `{
			"number": 15,
			"title": "Merged feature",
			"body": "Done",
			"state": "closed",
			"html_url": "https://gitea.example.com/org/repo/pulls/15",
			"mergeable": false,
			"merged": true,
			"head": {"ref": "feature/done", "sha": "fff999"},
			"base": {"ref": "main", "sha": "000aaa"}
		}`

		var pr giteaPullRequest
		require.NoError(t, json.Unmarshal([]byte(raw), &pr))

		assert.True(t, pr.Merged)
		assert.Equal(t, "closed", pr.State)

		// fetchPRView logic: mapGiteaState then override if merged
		state := mapGiteaState(pr.State)
		if pr.Merged {
			state = "MERGED"
		}
		assert.Equal(t, "MERGED", state)
		assert.Equal(t, "UNKNOWN", mapGiteaMergeable(pr.Mergeable, pr.State))
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

// TestGiteaReviewCommentParsing tests JSON deserialization of review comments
// and the unresolved counting logic.
func TestGiteaReviewCommentParsing(t *testing.T) {
	resolved := true
	unresolved := false

	tests := []struct {
		name     string
		comments []giteaReviewComment
		want     int
	}{
		{
			name: "mixed resolved and unresolved",
			comments: []giteaReviewComment{
				{ID: 1, Resolved: &unresolved},
				{ID: 2, Resolved: &resolved},
				{ID: 3, Resolved: &unresolved},
			},
			want: 2,
		},
		{
			name: "all resolved",
			comments: []giteaReviewComment{
				{ID: 1, Resolved: &resolved},
				{ID: 2, Resolved: &resolved},
			},
			want: 0,
		},
		{
			name: "nil resolved means not resolvable",
			comments: []giteaReviewComment{
				{ID: 1, Resolved: nil},
				{ID: 2, Resolved: &unresolved},
			},
			want: 1,
		},
		{
			name:     "empty list",
			comments: nil,
			want:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			count := 0
			for _, c := range tt.comments {
				if c.Resolved != nil && !*c.Resolved {
					count++
				}
			}
			assert.Equal(t, tt.want, count)
		})
	}

	// Also verify JSON deserialization with the resolved field.
	t.Run("JSON deserialization", func(t *testing.T) {
		raw := `[
			{"id": 1, "resolved": false},
			{"id": 2, "resolved": true},
			{"id": 3},
			{"id": 4, "resolved": false}
		]`
		var comments []giteaReviewComment
		require.NoError(t, json.Unmarshal([]byte(raw), &comments))

		assert.Len(t, comments, 4)
		require.NotNil(t, comments[0].Resolved)
		assert.False(t, *comments[0].Resolved)
		require.NotNil(t, comments[1].Resolved)
		assert.True(t, *comments[1].Resolved)
		assert.Nil(t, comments[2].Resolved)
		require.NotNil(t, comments[3].Resolved)
		assert.False(t, *comments[3].Resolved)

		// Count unresolved
		count := 0
		for _, c := range comments {
			if c.Resolved != nil && !*c.Resolved {
				count++
			}
		}
		assert.Equal(t, 2, count)
	})
}

// TestGiteaMergePRRequestJSON verifies the merge request payload uses the correct
// field casing expected by the Gitea API (lowercase "do", not "Do").
func TestGiteaMergePRRequestJSON(t *testing.T) {
	req := giteaMergePRRequest{
		Do:                     "squash",
		DeleteBranchAfterMerge: true,
	}
	data, err := json.Marshal(req)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))

	// "do" must be lowercase — the Gitea API rejects uppercase "Do".
	assert.Contains(t, raw, "do")
	assert.NotContains(t, raw, "Do")
	assert.Equal(t, "squash", raw["do"])
	assert.Equal(t, true, raw["delete_branch_after_merge"])
}

// TestGiteaProviderInterfaceCompliance verifies GiteaProvider satisfies the Provider interface.
func TestGiteaProviderInterfaceCompliance(t *testing.T) {
	var _ Provider = (*GiteaProvider)(nil)
	var _ Provider = NewGiteaProvider()
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

			// Verify the concrete type is GiteaProvider.
			_, ok := p.(*GiteaProvider)
			assert.True(t, ok, "ForPlatform(%q) should return *GiteaProvider", tt.platform)
		})
	}
}
