package vcs

import (
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
	assert.Equal(t, "group%2Fsubgroup%2Fproject", urlEncode("group/subgroup/project"))
	assert.Equal(t, "simple", urlEncode("simple"))
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
