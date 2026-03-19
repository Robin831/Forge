package cifix

import (
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/vcs"
)

func TestBuildBatchCIPrompt(t *testing.T) {
	p := BatchFixParams{
		PRNumber:     42,
		Branch:       "forge/Forge-xyz",
		BeadID:       "Forge-xyz",
		WorktreePath: "/tmp/worktree",
		FailingChecks: []vcs.CICheck{
			{Name: "build", Status: "fail"},
			{Name: "lint", Status: "fail"},
			{Name: "test", Status: "fail"},
		},
		CILogs: map[string]string{
			"build": "error: missing import\n  main.go:10",
			"lint":  "golangci-lint: unused variable x",
		},
	}

	prompt := buildBatchCIPrompt(p)

	// Verify PR metadata is included.
	if !strings.Contains(prompt, "PR #42") {
		t.Error("prompt should contain PR number")
	}
	if !strings.Contains(prompt, "forge/Forge-xyz") {
		t.Error("prompt should contain branch name")
	}
	if !strings.Contains(prompt, "Forge-xyz") {
		t.Error("prompt should contain bead ID")
	}

	// Verify all check names are present.
	for _, name := range []string{"build", "lint", "test"} {
		if !strings.Contains(prompt, name) {
			t.Errorf("prompt should contain check name %q", name)
		}
	}

	// Verify CI logs are included for checks that have them.
	if !strings.Contains(prompt, "missing import") {
		t.Error("prompt should contain build log content")
	}
	if !strings.Contains(prompt, "unused variable") {
		t.Error("prompt should contain lint log content")
	}

	// Verify numbered list format.
	if !strings.Contains(prompt, "1. build") {
		t.Error("prompt should number checks starting at 1")
	}
	if !strings.Contains(prompt, "2. lint") {
		t.Error("prompt should number checks sequentially")
	}

	// Verify instructions mention all failures.
	if !strings.Contains(prompt, "3 failing checks") {
		t.Error("prompt should mention total number of failing checks in instructions")
	}
}

func TestBuildBatchCIPrompt_NoLogs(t *testing.T) {
	p := BatchFixParams{
		PRNumber:     7,
		Branch:       "fix/ci",
		BeadID:       "test-1",
		WorktreePath: "/tmp/wt",
		FailingChecks: []vcs.CICheck{
			{Name: "changelog-check", Status: "fail"},
		},
		CILogs: nil,
	}

	prompt := buildBatchCIPrompt(p)

	if !strings.Contains(prompt, "changelog-check") {
		t.Error("prompt should contain check name even without logs")
	}
	// Should not contain log block markers when no logs available.
	if strings.Contains(prompt, "**CI Log:**") {
		t.Error("prompt should not contain CI Log header when no log for the check exists")
	}
}

func TestTruncateOutput(t *testing.T) {
	short := "hello"
	if got := truncateOutput(short, 10); got != short {
		t.Errorf("truncateOutput(%q, 10) = %q, want %q", short, got, short)
	}

	long := strings.Repeat("x", 100)
	got := truncateOutput(long, 50)
	if !strings.HasPrefix(got, "... (truncated)") {
		t.Error("truncated output should start with truncation marker")
	}
	if len(got) > 50+len("... (truncated)\n") {
		t.Errorf("truncated output too long: %d chars", len(got))
	}
}
