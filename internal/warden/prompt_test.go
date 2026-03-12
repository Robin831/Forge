package warden

import (
	"strings"
	"testing"
)

func TestBuildReviewPrompt_ContainsBeadMetadata(t *testing.T) {
	prompt := buildReviewPrompt("Forge-123", "Fix the login bug", "Users cannot log in after password reset.", "diff content", t.TempDir())

	if !strings.Contains(prompt, "Forge-123") {
		t.Error("prompt should contain bead ID")
	}
	if !strings.Contains(prompt, "Fix the login bug") {
		t.Error("prompt should contain bead title")
	}
	if !strings.Contains(prompt, "Users cannot log in after password reset.") {
		t.Error("prompt should contain bead description when non-empty")
	}
}

func TestBuildReviewPrompt_OmitsDescriptionWhenEmpty(t *testing.T) {
	prompt := buildReviewPrompt("Forge-456", "Refactor config loader", "", "diff content", t.TempDir())

	if !strings.Contains(prompt, "Forge-456") {
		t.Error("prompt should contain bead ID")
	}
	if !strings.Contains(prompt, "Refactor config loader") {
		t.Error("prompt should contain bead title")
	}
	// The description label should not appear when description is empty.
	if strings.Contains(prompt, "**Description**") {
		t.Error("prompt should not include Description section when description is empty")
	}
}

func TestBuildReviewPrompt_OmitsDescriptionWhenWhitespaceOnly(t *testing.T) {
	prompt := buildReviewPrompt("Forge-789", "Update deps", "   \n\t  ", "diff content", t.TempDir())

	if strings.Contains(prompt, "**Description**") {
		t.Error("prompt should not include Description section for whitespace-only description")
	}
}

func TestBuildReviewPrompt_ContainsScopeAlignmentCheck(t *testing.T) {
	prompt := buildReviewPrompt("Forge-001", "Some task", "Do something useful.", "diff content", t.TempDir())

	// Check that the 6th review criterion for scope alignment is present.
	if !strings.Contains(prompt, "scope alignment") && !strings.Contains(prompt, "scope drift") {
		t.Error("prompt should include a scope alignment / scope drift check")
	}
}

func TestBuildReviewPrompt_ContainsDiff(t *testing.T) {
	diff := "--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old\n+new"
	prompt := buildReviewPrompt("Forge-002", "Title", "Desc", diff, t.TempDir())

	if !strings.Contains(prompt, diff) {
		t.Error("prompt should contain the full diff")
	}
}
