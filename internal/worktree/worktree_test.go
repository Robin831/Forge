package worktree

import (
	"testing"
)

func TestCreateOptions_DefaultBranch(t *testing.T) {
	opts := CreateOptions{}
	if opts.Branch != "" {
		t.Errorf("expected empty default Branch, got %q", opts.Branch)
	}
	if opts.BaseBranch != "" {
		t.Errorf("expected empty default BaseBranch, got %q", opts.BaseBranch)
	}
}

func TestWorktree_BaseBranch(t *testing.T) {
	wt := &Worktree{
		BeadID:     "test-1",
		AnvilPath:  "/tmp/repo",
		Path:       "/tmp/repo/.workers/test-1",
		Branch:     "forge/test-1",
		BaseBranch: "feature/epic-1",
	}
	if wt.BaseBranch != "feature/epic-1" {
		t.Errorf("expected BaseBranch %q, got %q", "feature/epic-1", wt.BaseBranch)
	}
}

func TestSanitizePath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Forge-n1g.4.1", "Forge-n1g.4.1"},
		{"feat/fix-bug", "feat-fix-bug"},
		{"fix:typo", "fix-typo"},
		{"my work", "my-work"},
		{"a\\b", "a-b"},
	}

	for _, tt := range tests {
		got := sanitizePath(tt.input)
		if got != tt.expected {
			t.Errorf("sanitizePath(%q) = %q; want %q", tt.input, got, tt.expected)
		}
	}
}
