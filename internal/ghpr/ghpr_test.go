package ghpr

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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

func TestMergeabilityFromStatus(t *testing.T) {
	tests := []struct {
		name   string
		status PRStatus
		want   MergeabilityInputs
	}{
		{
			"clean PR",
			PRStatus{Mergeable: "MERGEABLE"},
			MergeabilityInputs{},
		},
		{
			"conflicting",
			PRStatus{Mergeable: "CONFLICTING"},
			MergeabilityInputs{HasConflicts: true},
		},
		{
			"pending reviews",
			PRStatus{
				Mergeable:      "MERGEABLE",
				ReviewRequests: []ReviewRequest{{Login: "alice"}},
			},
			MergeabilityInputs{HasPendingReviews: true},
		},
		{
			"unresolved threads",
			PRStatus{
				Mergeable:         "MERGEABLE",
				UnresolvedThreads: 3,
			},
			MergeabilityInputs{HasUnresolvedThreads: true},
		},
		{
			"all flags set",
			PRStatus{
				Mergeable:         "CONFLICTING",
				UnresolvedThreads: 1,
				ReviewRequests:    []ReviewRequest{{Slug: "team-a"}},
			},
			MergeabilityInputs{
				HasConflicts:         true,
				HasUnresolvedThreads: true,
				HasPendingReviews:    true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeabilityFromStatus(&tt.status)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildDefaultBody(t *testing.T) {
	tests := []struct {
		name     string
		params   CreateParams
		contains []string
		absent   []string
	}{
		{
			name: "full params with type, description, and change summary",
			params: CreateParams{
				BeadID:          "Forge-abc1",
				Branch:          "forge/Forge-abc1",
				BeadType:        "feature",
				BeadTitle:       "Add widget support",
				BeadDescription: "Users need widget support for the dashboard.",
				ChangeSummary:   "- Added Widget struct\n- Wired up handler",
			},
			contains: []string{
				"## Changes",
				"- Added Widget struct",
				"## Original Issue (feature): Add widget support",
				"Users need widget support for the dashboard.",
				"Bead: Forge-abc1 | Branch: forge/Forge-abc1",
				"Generated by [The Forge]",
			},
		},
		{
			name: "title without type",
			params: CreateParams{
				BeadID:          "Forge-abc2",
				Branch:          "forge/Forge-abc2",
				BeadTitle:       "Fix login bug",
				BeadDescription: "Login is broken.",
			},
			contains: []string{
				"## Original Issue: Fix login bug",
				"Login is broken.",
				"Bead: Forge-abc2 | Branch: forge/Forge-abc2",
			},
			absent: []string{
				"## Changes",
			},
		},
		{
			name: "no title, no description, no change summary",
			params: CreateParams{
				BeadID: "Forge-abc3",
				Branch: "forge/Forge-abc3",
			},
			contains: []string{
				"Bead: Forge-abc3 | Branch: forge/Forge-abc3",
				"Generated by [The Forge]",
			},
			absent: []string{
				"## Changes",
				"## Original Issue",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := buildDefaultBody(tt.params)
			for _, want := range tt.contains {
				assert.Contains(t, body, want, "body should contain %q", want)
			}
			for _, unwanted := range tt.absent {
				assert.NotContains(t, body, unwanted, "body should not contain %q", unwanted)
			}
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

// makeTestRepo creates a temporary git repository with a single commit using
// the given commit message and returns the repo directory.
func makeTestRepo(t *testing.T, commitMsg string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test User",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test User",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", commitMsg)
	return dir
}

func TestCommitSubject(t *testing.T) {
	dir := makeTestRepo(t, "Add initial file")

	t.Run("branch exists locally", func(t *testing.T) {
		subject := commitSubject(context.Background(), dir, "main")
		assert.Equal(t, "Add initial file", subject)
	})

	t.Run("branch does not exist locally or remotely", func(t *testing.T) {
		subject := commitSubject(context.Background(), dir, "nonexistent-branch-xyz")
		assert.Equal(t, "", subject)
	})
}

func TestSelectTitle(t *testing.T) {
	dir := makeTestRepo(t, "English commit subject")

	t.Run("no title and no bead title gets default forge prefix", func(t *testing.T) {
		p := CreateParams{BeadID: "Forge-test", WorktreePath: dir, Branch: "nonexistent-branch-xyz"}
		got := selectTitle(context.Background(), p)
		assert.Equal(t, "forge: Forge-test", got)
	})

	t.Run("bead title takes precedence over commit subject", func(t *testing.T) {
		// The Smith's last commit may describe an incidental fix; the bead title
		// must win so the PR reflects the bead's stated intent.
		p := CreateParams{
			BeadID:       "Forge-test",
			Title:        "feat: fix some incidental bug found during implementation",
			BeadTitle:    "Show PR title in Ready to Merge action menu",
			WorktreePath: dir,
			Branch:       "main",
		}
		got := selectTitle(context.Background(), p)
		assert.Equal(t, "Show PR title in Ready to Merge action menu (Forge-test)", got)
	})

	t.Run("commit subject is used as fallback when no bead title is set", func(t *testing.T) {
		p := CreateParams{
			BeadID:       "Forge-test",
			WorktreePath: dir,
			Branch:       "main",
		}
		got := selectTitle(context.Background(), p)
		assert.Equal(t, "English commit subject (Forge-test)", got)
	})

	t.Run("title with [no-changelog] is preserved unchanged even when bead title is set", func(t *testing.T) {
		originalTitle := "forge: learn rules [no-changelog]"
		p := CreateParams{
			BeadID:       "Forge-test",
			Title:        originalTitle,
			BeadTitle:    "Auto-learn warden rules",
			WorktreePath: dir,
			Branch:       "main",
		}
		got := selectTitle(context.Background(), p)
		assert.Equal(t, originalTitle, got, "title with [no-changelog] must not be overridden")
	})

	t.Run("non-existent branch with no bead title keeps original title", func(t *testing.T) {
		p := CreateParams{
			BeadID:       "Forge-test",
			Title:        "Some provided title",
			WorktreePath: dir,
			Branch:       "nonexistent-branch-xyz",
		}
		got := selectTitle(context.Background(), p)
		assert.Equal(t, "Some provided title", got)
	})
}

