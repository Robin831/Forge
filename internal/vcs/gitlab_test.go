package vcs

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitLabProvider_Platform(t *testing.T) {
	p := NewGitLabProvider()
	assert.Equal(t, GitLab, p.Platform())
}

func TestParseGitLabRepoURL(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{
			name:      "HTTPS simple",
			url:       "https://gitlab.com/mygroup/myproject.git",
			wantOwner: "mygroup",
			wantRepo:  "myproject",
		},
		{
			name:      "HTTPS without .git",
			url:       "https://gitlab.com/mygroup/myproject",
			wantOwner: "mygroup",
			wantRepo:  "myproject",
		},
		{
			name:      "HTTPS nested groups",
			url:       "https://gitlab.com/org/team/subteam/myproject.git",
			wantOwner: "org/team/subteam",
			wantRepo:  "myproject",
		},
		{
			name:      "SSH simple",
			url:       "git@gitlab.com:mygroup/myproject.git",
			wantOwner: "mygroup",
			wantRepo:  "myproject",
		},
		{
			name:      "SSH nested groups",
			url:       "git@gitlab.com:org/team/myproject.git",
			wantOwner: "org/team",
			wantRepo:  "myproject",
		},
		{
			name:      "self-hosted HTTPS",
			url:       "https://gitlab.example.com/department/myproject.git",
			wantOwner: "department",
			wantRepo:  "myproject",
		},
		{
			name:      "self-hosted SSH",
			url:       "git@gitlab.internal:infra/deploy-tools.git",
			wantOwner: "infra",
			wantRepo:  "deploy-tools",
		},
		{
			name:    "invalid URL",
			url:     "not-a-url",
			wantErr: true,
		},
		{
			name:    "SSH no namespace",
			url:     "git@gitlab.com:project",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, repo, err := ParseGitLabRepoURL(tt.url)
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

func TestMapGitLabState(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"opened", "OPEN"},
		{"merged", "MERGED"},
		{"closed", "CLOSED"},
		{"Opened", "OPEN"},
		{"MERGED", "MERGED"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, mapGitLabState(tt.input))
		})
	}
}

func TestMapGitLabMergeable(t *testing.T) {
	tests := []struct {
		name         string
		mergeStatus  string
		hasConflicts bool
		want         string
	}{
		{"conflicts override", "can_be_merged", true, "CONFLICTING"},
		{"can be merged", "can_be_merged", false, "MERGEABLE"},
		{"ci must pass", "ci_must_pass", false, "MERGEABLE"},
		{"ci running", "ci_still_running", false, "MERGEABLE"},
		{"cannot be merged", "cannot_be_merged", false, "CONFLICTING"},
		{"recheck", "cannot_be_merged_recheck", false, "CONFLICTING"},
		{"unknown status", "checking", false, "UNKNOWN"},
		{"unchecked", "unchecked", false, "UNKNOWN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mapGitLabMergeable(tt.mergeStatus, tt.hasConflicts))
		})
	}
}

func TestMapGitLabJobConclusion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"success", "SUCCESS"},
		{"failed", "FAILURE"},
		{"canceled", "CANCELLED"},
		{"skipped", "SKIPPED"},
		{"manual", "NEUTRAL"},
		{"running", ""},
		{"pending", ""},
		{"created", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, mapGitLabJobConclusion(tt.input))
		})
	}
}

func TestExtractMRNumber(t *testing.T) {
	tests := []struct {
		url  string
		want int
	}{
		{"https://gitlab.com/group/project/-/merge_requests/42", 42},
		{"https://gitlab.example.com/org/team/project/-/merge_requests/7", 7},
		{"", 0},
		{"https://gitlab.com/group/project/-/merge_requests/abc", 0},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			assert.Equal(t, tt.want, extractMRNumber(tt.url))
		})
	}
}

func TestExtractGlabURL(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "URL on its own line",
			output: "Creating merge request...\nhttps://gitlab.com/group/project/-/merge_requests/42\n",
			want:   "https://gitlab.com/group/project/-/merge_requests/42",
		},
		{
			name:   "URL with prefix text",
			output: "MR created: https://gitlab.com/group/project/-/merge_requests/7\n",
			want:   "https://gitlab.com/group/project/-/merge_requests/7",
		},
		{
			name:   "fallback to last line URL",
			output: "some output\nhttps://example.com/result\n",
			want:   "https://example.com/result",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractGlabURL(tt.output))
		})
	}
}

func TestURLEncode(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"nested groups", "group/subgroup/project", "group%2Fsubgroup%2Fproject"},
		{"no slashes", "simple", "simple"},
		{"single group", "group/project", "group%2Fproject"},
		{"space in name", "my group/my project", "my%20group%2Fmy%20project"},
		{"dots in name", "my.group/my.project", "my.group%2Fmy.project"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, urlEncode(tt.input))
		})
	}
}

func TestBuildGitLabBody(t *testing.T) {
	body := buildGitLabBody(CreateParams{
		BeadID:          "Forge-42",
		Branch:          "forge/Forge-42",
		BeadTitle:       "Fix the thing",
		BeadDescription: "It's broken",
		BeadType:        "bug",
		ChangeSummary:   "Fixed the broken thing",
	})
	assert.Contains(t, body, "## Changes")
	assert.Contains(t, body, "Fixed the broken thing")
	assert.Contains(t, body, "## Original Issue (bug): Fix the thing")
	assert.Contains(t, body, "It's broken")
	assert.Contains(t, body, "Bead: Forge-42")
}

// TestGlabMRStatusParsing tests JSON deserialization of glab mr view output,
// covering the CLI-dependent parsing path without requiring glab to be installed.
func TestGlabMRStatusParsing(t *testing.T) {
	t.Run("full MR with pipeline and jobs", func(t *testing.T) {
		raw := `{
			"iid": 42,
			"state": "opened",
			"merge_status": "can_be_merged",
			"has_conflicts": false,
			"web_url": "https://gitlab.com/group/project/-/merge_requests/42",
			"source_branch": "feature/test",
			"draft": false,
			"head_pipeline": {
				"status": "success",
				"jobs": [
					{"name": "build", "status": "success"},
					{"name": "test", "status": "failed"},
					{"name": "lint", "status": "skipped"}
				]
			}
		}`

		var mr glabMRStatus
		require.NoError(t, json.Unmarshal([]byte(raw), &mr))

		assert.Equal(t, 42, mr.IID)
		assert.Equal(t, "opened", mr.State)
		assert.Equal(t, "can_be_merged", mr.MergeStatus)
		assert.False(t, mr.HasConflicts)
		assert.Equal(t, "feature/test", mr.SourceBranch)
		assert.False(t, mr.Draft)
		require.NotNil(t, mr.HeadPipeline)
		assert.Len(t, mr.HeadPipeline.Jobs, 3)

		// Simulate what fetchMRView does with the parsed data
		status := &PRStatus{
			State:       mapGitLabState(mr.State),
			Mergeable:   mapGitLabMergeable(mr.MergeStatus, mr.HasConflicts),
			HeadRefName: mr.SourceBranch,
			URL:         mr.WebURL,
		}
		for _, job := range mr.HeadPipeline.Jobs {
			status.StatusCheckRollup = append(status.StatusCheckRollup, CheckRun{
				Name:       job.Name,
				Status:     mapGitLabJobStatus(job.Status),
				Conclusion: mapGitLabJobConclusion(job.Status),
			})
		}

		assert.Equal(t, "OPEN", status.State)
		assert.Equal(t, "MERGEABLE", status.Mergeable)
		assert.Equal(t, "feature/test", status.HeadRefName)
		assert.Len(t, status.StatusCheckRollup, 3)
		assert.Equal(t, "SUCCESS", status.StatusCheckRollup[0].Conclusion)
		assert.Equal(t, "FAILURE", status.StatusCheckRollup[1].Conclusion)
		assert.Equal(t, "SKIPPED", status.StatusCheckRollup[2].Conclusion)
		assert.False(t, status.CIsPassing(), "should fail due to FAILURE conclusion")
	})

	t.Run("merged MR with conflicts", func(t *testing.T) {
		raw := `{
			"iid": 7,
			"state": "merged",
			"merge_status": "cannot_be_merged",
			"has_conflicts": true,
			"web_url": "https://gitlab.example.com/org/project/-/merge_requests/7",
			"source_branch": "fix/bug",
			"draft": false,
			"head_pipeline": null
		}`

		var mr glabMRStatus
		require.NoError(t, json.Unmarshal([]byte(raw), &mr))

		status := &PRStatus{
			State:     mapGitLabState(mr.State),
			Mergeable: mapGitLabMergeable(mr.MergeStatus, mr.HasConflicts),
		}

		assert.True(t, status.IsMerged())
		assert.Equal(t, "CONFLICTING", status.Mergeable)
		assert.Empty(t, status.StatusCheckRollup)
	})

	t.Run("pipeline without individual jobs", func(t *testing.T) {
		raw := `{
			"iid": 99,
			"state": "opened",
			"merge_status": "ci_still_running",
			"has_conflicts": false,
			"web_url": "https://gitlab.com/g/p/-/merge_requests/99",
			"source_branch": "wip",
			"draft": true,
			"head_pipeline": {
				"status": "running",
				"jobs": []
			}
		}`

		var mr glabMRStatus
		require.NoError(t, json.Unmarshal([]byte(raw), &mr))
		assert.True(t, mr.Draft)
		assert.NotNil(t, mr.HeadPipeline)
		assert.Empty(t, mr.HeadPipeline.Jobs)

		// When no individual jobs, fetchMRView uses pipeline-level status
		status := &PRStatus{
			State:     mapGitLabState(mr.State),
			Mergeable: mapGitLabMergeable(mr.MergeStatus, mr.HasConflicts),
		}
		if mr.HeadPipeline != nil && len(mr.HeadPipeline.Jobs) == 0 {
			status.StatusCheckRollup = []CheckRun{
				{
					Name:       "pipeline",
					Status:     mapGitLabJobStatus(mr.HeadPipeline.Status),
					Conclusion: mapGitLabJobConclusion(mr.HeadPipeline.Status),
				},
			}
		}

		assert.Equal(t, "MERGEABLE", status.Mergeable)
		assert.Len(t, status.StatusCheckRollup, 1)
		assert.Equal(t, "pipeline", status.StatusCheckRollup[0].Name)
		assert.Equal(t, "IN_PROGRESS", status.StatusCheckRollup[0].Status)
		assert.Equal(t, "", status.StatusCheckRollup[0].Conclusion)
	})
}

// TestGlabApprovalParsing tests JSON deserialization of the approvals API response.
func TestGlabApprovalParsing(t *testing.T) {
	t.Run("with approvals", func(t *testing.T) {
		raw := `{
			"approved": true,
			"approved_by": [
				{"user": {"username": "alice"}},
				{"user": {"username": "bob"}}
			],
			"approval_rules_left": []
		}`

		var approval glabApproval
		require.NoError(t, json.Unmarshal([]byte(raw), &approval))

		assert.True(t, approval.Approved)
		assert.Len(t, approval.ApprovedBy, 2)

		// Simulate what fetchApprovals does
		var reviews []Review
		for _, a := range approval.ApprovedBy {
			reviews = append(reviews, Review{
				Author: ReviewAuthor{Login: a.User.Username},
				State:  "APPROVED",
			})
		}

		assert.Len(t, reviews, 2)
		assert.Equal(t, "alice", reviews[0].Author.Login)
		assert.Equal(t, "bob", reviews[1].Author.Login)
	})

	t.Run("no approvals", func(t *testing.T) {
		raw := `{
			"approved": false,
			"approved_by": [],
			"approval_rules_left": [{"approved": false}]
		}`

		var approval glabApproval
		require.NoError(t, json.Unmarshal([]byte(raw), &approval))

		assert.False(t, approval.Approved)
		assert.Empty(t, approval.ApprovedBy)
	})
}

// TestGlabDiscussionParsing tests JSON deserialization of discussion/thread API responses.
func TestGlabDiscussionParsing(t *testing.T) {
	raw := `[
		{
			"notes": [
				{"id": 1, "body": "Fix this", "resolvable": true, "resolved": false},
				{"id": 2, "body": "Reply", "resolvable": true, "resolved": false}
			]
		},
		{
			"notes": [
				{"id": 3, "body": "LGTM", "resolvable": true, "resolved": true}
			]
		},
		{
			"notes": [
				{"id": 4, "body": "System note", "resolvable": false, "resolved": false}
			]
		},
		{
			"notes": [
				{"id": 5, "body": "Another issue", "resolvable": true, "resolved": false}
			]
		}
	]`

	var discussions []struct {
		Notes []glabNote `json:"notes"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &discussions))

	// Simulate the unresolved thread counting logic from FetchUnresolvedThreadCount
	count := 0
	for _, d := range discussions {
		for _, note := range d.Notes {
			if note.Resolvable && !note.Resolved {
				count++
				break // count each discussion once
			}
		}
	}

	assert.Equal(t, 2, count, "should count 2 unresolved discussions (first and fourth)")
}

// TestGlabApprovalStateParsing tests JSON deserialization of the approval_state API response.
func TestGlabApprovalStateParsing(t *testing.T) {
	raw := `{
		"rules": [
			{
				"name": "Code Review",
				"approved": false,
				"eligible_approvers": [
					{"username": "alice", "name": "Alice A"},
					{"username": "bob", "name": "Bob B"}
				]
			},
			{
				"name": "Security Review",
				"approved": true,
				"eligible_approvers": [
					{"username": "charlie", "name": "Charlie C"}
				]
			}
		]
	}`

	var approvalState struct {
		Rules []struct {
			Name              string `json:"name"`
			Approved          bool   `json:"approved"`
			EligibleApprovers []struct {
				Username string `json:"username"`
				Name     string `json:"name"`
			} `json:"eligible_approvers"`
		} `json:"rules"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &approvalState))

	// Simulate FetchPendingReviewRequests logic
	var requests []ReviewRequest
	for _, rule := range approvalState.Rules {
		if rule.Approved {
			continue
		}
		for _, approver := range rule.EligibleApprovers {
			requests = append(requests, ReviewRequest{
				Login: approver.Username,
				Name:  approver.Name,
			})
		}
	}

	assert.Len(t, requests, 2, "only unapproved rule should produce requests")
	assert.Equal(t, "alice", requests[0].Login)
	assert.Equal(t, "bob", requests[1].Login)
}

// TestGlabMRListParsing tests JSON deserialization of glab mr list output.
func TestGlabMRListParsing(t *testing.T) {
	raw := `[
		{"iid": 1, "title": "First MR", "source_branch": "feature/one", "description": "First desc"},
		{"iid": 2, "title": "Second MR", "source_branch": "fix/two", "description": "Second desc"},
		{"iid": 3, "title": "Third MR", "source_branch": "chore/three", "description": ""}
	]`

	var mrs []struct {
		IID          int    `json:"iid"`
		Title        string `json:"title"`
		SourceBranch string `json:"source_branch"`
		Description  string `json:"description"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &mrs))

	out := make([]OpenPR, len(mrs))
	for i, r := range mrs {
		out[i] = OpenPR{
			Number: r.IID,
			Title:  r.Title,
			Branch: r.SourceBranch,
			Body:   r.Description,
		}
	}

	assert.Len(t, out, 3)
	assert.Equal(t, 1, out[0].Number)
	assert.Equal(t, "First MR", out[0].Title)
	assert.Equal(t, "feature/one", out[0].Branch)
	assert.Equal(t, "", out[2].Body)
}

func TestMapGitLabJobStatus(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"success", "COMPLETED"},
		{"failed", "COMPLETED"},
		{"canceled", "COMPLETED"},
		{"skipped", "COMPLETED"},
		{"running", "IN_PROGRESS"},
		{"pending", "IN_PROGRESS"},
		{"created", "IN_PROGRESS"},
		{"waiting_for_resource", "IN_PROGRESS"},
		{"preparing", "IN_PROGRESS"},
		{"manual", "QUEUED"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, mapGitLabJobStatus(tt.input))
		})
	}
}

func TestBuildGitLabBody_Variants(t *testing.T) {
	t.Run("no change summary", func(t *testing.T) {
		body := buildGitLabBody(CreateParams{
			BeadID:          "Forge-1",
			Branch:          "forge/Forge-1",
			BeadDescription: "Some issue",
		})
		assert.NotContains(t, body, "## Changes")
		assert.Contains(t, body, "## Original Issue\n")
		assert.Contains(t, body, "Some issue")
	})

	t.Run("no description", func(t *testing.T) {
		body := buildGitLabBody(CreateParams{
			BeadID:        "Forge-2",
			Branch:        "forge/Forge-2",
			ChangeSummary: "Made improvements",
		})
		assert.Contains(t, body, "## Changes")
		assert.NotContains(t, body, "## Original Issue")
	})

	t.Run("minimal params", func(t *testing.T) {
		body := buildGitLabBody(CreateParams{
			BeadID: "Forge-3",
			Branch: "forge/Forge-3",
		})
		assert.Contains(t, body, "Bead: Forge-3")
		assert.Contains(t, body, "Branch: forge/Forge-3")
		assert.Contains(t, body, "Generated by")
	})
}

func TestSplitNamespacePath(t *testing.T) {
	tests := []struct {
		path      string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"group/project", "group", "project", false},
		{"org/team/project", "org/team", "project", false},
		{"a/b/c/d", "a/b/c", "d", false},
		{"project", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			owner, repo, err := splitNamespacePath(tt.path, "test-url")
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
