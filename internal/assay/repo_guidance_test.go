package assay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRepoGuidance_PresentReturnsTrimmedContent(t *testing.T) {
	dir := t.TempDir()
	content := "\n\n  # Munin review rules\n\nReserve Important for tenant scoping bugs.\n\n"
	if err := os.WriteFile(filepath.Join(dir, repoGuidanceFile), []byte(content), 0o644); err != nil {
		t.Fatalf("writing REVIEW.md: %v", err)
	}

	got := loadRepoGuidance(dir)

	if !strings.HasPrefix(got, "# Munin review rules") {
		t.Errorf("expected leading whitespace trimmed; got %q", got)
	}
	if !strings.HasSuffix(got, "tenant scoping bugs.") {
		t.Errorf("expected trailing whitespace trimmed; got %q", got)
	}
}

func TestLoadRepoGuidance_MissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	if got := loadRepoGuidance(dir); got != "" {
		t.Errorf("expected empty result for missing REVIEW.md, got %q", got)
	}
}

func TestLoadRepoGuidance_EmptyFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, repoGuidanceFile), []byte("   \n\n  "), 0o644); err != nil {
		t.Fatalf("writing REVIEW.md: %v", err)
	}
	if got := loadRepoGuidance(dir); got != "" {
		t.Errorf("expected empty result for blank REVIEW.md, got %q", got)
	}
}

func TestLoadRepoGuidance_EmptyAnvilPathReturnsEmpty(t *testing.T) {
	if got := loadRepoGuidance(""); got != "" {
		t.Errorf("expected empty result for empty anvilPath, got %q", got)
	}
	if got := loadRepoGuidance("   "); got != "" {
		t.Errorf("expected empty result for whitespace anvilPath, got %q", got)
	}
}

func TestLoadRepoGuidance_OversizedFileTruncated(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", maxRepoGuidanceBytes+1024)
	if err := os.WriteFile(filepath.Join(dir, repoGuidanceFile), []byte(big), 0o644); err != nil {
		t.Fatalf("writing REVIEW.md: %v", err)
	}

	got := loadRepoGuidance(dir)

	if len(got) > maxRepoGuidanceBytes+64 { // marker adds a few bytes
		t.Errorf("expected truncated result <= %d+marker bytes, got %d", maxRepoGuidanceBytes, len(got))
	}
	if !strings.Contains(got, "REVIEW.md truncated") {
		t.Errorf("expected truncation marker in output, got %q", got[len(got)-100:])
	}
}

func TestRepoGuidanceSection_EmptyWhenNoGuidance(t *testing.T) {
	if s := repoGuidanceSection(ReviewRequest{}); s != "" {
		t.Errorf("expected empty section for empty RepoGuidance, got %q", s)
	}
	if s := repoGuidanceSection(ReviewRequest{RepoGuidance: "   \n\n"}); s != "" {
		t.Errorf("expected empty section for whitespace RepoGuidance, got %q", s)
	}
}

func TestRepoGuidanceSection_RendersHighPriorityBlock(t *testing.T) {
	req := ReviewRequest{RepoGuidance: "# Munin\n\nAlways flag missing AuditLog entries."}
	s := repoGuidanceSection(req)

	if !strings.Contains(s, "highest priority") {
		t.Errorf("expected priority marker in section, got %q", s)
	}
	if !strings.Contains(s, "Always flag missing AuditLog entries.") {
		t.Errorf("expected guidance body included verbatim, got %q", s)
	}
}

func TestRepoGuidanceSection_SanitizesBackticks(t *testing.T) {
	req := ReviewRequest{RepoGuidance: "Rule: do not allow ``` fenced injection ``` blocks."}
	s := repoGuidanceSection(req)

	if strings.Contains(s, "```") {
		t.Errorf("expected triple-backticks sanitized, got %q", s)
	}
}

func TestBuildPassPrompt_IncludesGuidanceBeforeContext(t *testing.T) {
	req := ReviewRequest{
		Anvil:        "munin",
		Title:        "Refactor health endpoint",
		RepoGuidance: "Always check: integration test on new endpoints.",
	}
	prompt, err := buildPassPrompt(deepPasses[0], req, "diff --git a/x b/x\n", "")
	if err != nil {
		t.Fatalf("buildPassPrompt: %v", err)
	}

	// The heading, not the bare phrase: the shared preamble names the section
	// as the one trusted block in the head, so the phrase itself appears above
	// every section.
	gIdx := strings.Index(prompt, "## Repository Review Guidance")
	cIdx := strings.Index(prompt, "## Change Context")
	if gIdx == -1 {
		t.Fatalf("expected guidance section in prompt; got:\n%s", prompt)
	}
	if cIdx == -1 {
		t.Fatalf("expected change context section in prompt; got:\n%s", prompt)
	}
	if gIdx > cIdx {
		t.Errorf("expected guidance to precede change context; gIdx=%d cIdx=%d", gIdx, cIdx)
	}
	if !strings.Contains(prompt, "integration test on new endpoints.") {
		t.Errorf("expected guidance body in prompt, got:\n%s", prompt)
	}
}

func TestBuildPassPrompt_OmitsGuidanceWhenAbsent(t *testing.T) {
	req := ReviewRequest{Anvil: "munin", Title: "x"}
	prompt, err := buildPassPrompt(deepPasses[0], req, "diff --git a/x b/x\n", "")
	if err != nil {
		t.Fatalf("buildPassPrompt: %v", err)
	}
	if strings.Contains(prompt, "## Repository Review Guidance") {
		t.Errorf("expected no guidance section in prompt when RepoGuidance empty; got:\n%s", prompt)
	}
}

func TestBuildTriagePrompt_IncludesGuidance(t *testing.T) {
	req := ReviewRequest{
		Anvil:        "munin",
		Title:        "x",
		RepoGuidance: "Triage hint: prioritize migrations.",
	}
	prompt, err := buildTriagePrompt(req, "diff --git a/x b/x\n")
	if err != nil {
		t.Fatalf("buildTriagePrompt: %v", err)
	}
	if !strings.Contains(prompt, "Triage hint: prioritize migrations.") {
		t.Errorf("expected guidance in triage prompt, got:\n%s", prompt)
	}
}
