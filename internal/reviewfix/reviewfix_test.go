package reviewfix

import (
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/vcs"
)

func TestFilterActionableComments(t *testing.T) {
	tests := []struct {
		name    string
		input   []vcs.ReviewComment
		wantLen int
	}{
		{
			name:    "empty input",
			input:   nil,
			wantLen: 0,
		},
		{
			name: "skip approved",
			input: []vcs.ReviewComment{
				{Author: "alice", Body: "looks good", State: "APPROVED"},
			},
			wantLen: 0,
		},
		{
			name: "skip dismissed",
			input: []vcs.ReviewComment{
				{Author: "alice", Body: "fix this", State: "DISMISSED"},
			},
			wantLen: 0,
		},
		{
			name: "skip empty body",
			input: []vcs.ReviewComment{
				{Author: "alice", Body: "", State: "CHANGES_REQUESTED"},
			},
			wantLen: 0,
		},
		{
			name: "keep changes requested",
			input: []vcs.ReviewComment{
				{Author: "copilot", Body: "please fix the typo", State: "CHANGES_REQUESTED"},
			},
			wantLen: 1,
		},
		{
			name: "keep thread comment with no state",
			input: []vcs.ReviewComment{
				{Author: "copilot", Body: "this method is too long", ThreadID: "T_kwDO123"},
			},
			wantLen: 1,
		},
		{
			name: "mixed comments",
			input: []vcs.ReviewComment{
				{Author: "alice", Body: "LGTM", State: "APPROVED"},
				{Author: "copilot", Body: "fix the null check", State: "CHANGES_REQUESTED"},
				{Author: "bob", Body: "", State: "CHANGES_REQUESTED"},
				{Author: "copilot", Body: "this is unused", ThreadID: "T_kwDO456"},
			},
			wantLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterActionableComments(tt.input)
			if len(got) != tt.wantLen {
				t.Errorf("filterActionableComments() returned %d comments, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestBuildReviewFixPrompt(t *testing.T) {
	p := FixParams{
		PRNumber:     42,
		Branch:       "forge/Forge-xyz",
		BeadID:       "Forge-xyz",
		WorktreePath: "/tmp/worktree",
	}
	comments := []vcs.ReviewComment{
		{Author: "copilot", Body: "Fix the nil pointer", Path: "main.go", Line: 10, State: "CHANGES_REQUESTED"},
		{Author: "alice", Body: "Rename this variable", Path: "util.go", Line: 25},
	}

	prompt := buildReviewFixPrompt(p, comments)

	if !strings.Contains(prompt, "PR #42") {
		t.Error("prompt should contain PR number")
	}
	if !strings.Contains(prompt, "forge/Forge-xyz") {
		t.Error("prompt should contain branch name")
	}
	if !strings.Contains(prompt, "Forge-xyz") {
		t.Error("prompt should contain bead ID")
	}
	if !strings.Contains(prompt, "Fix the nil pointer") {
		t.Error("prompt should contain first comment body")
	}
	if !strings.Contains(prompt, "Rename this variable") {
		t.Error("prompt should contain second comment body")
	}
	if !strings.Contains(prompt, "main.go") {
		t.Error("prompt should contain file path from first comment")
	}
	if !strings.Contains(prompt, "@copilot") {
		t.Error("prompt should contain comment author")
	}
}

func TestBuildReviewFixPrompt_NoAuthorOrPath(t *testing.T) {
	p := FixParams{PRNumber: 1, Branch: "main", BeadID: "Forge-abc"}
	comments := []vcs.ReviewComment{
		{Body: "fix something"},
	}
	prompt := buildReviewFixPrompt(p, comments)
	if !strings.Contains(prompt, "fix something") {
		t.Error("prompt should include body even when author and path are empty")
	}
}
