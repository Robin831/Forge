// Package warden implements the code review agent that reviews Smith's changes.
//
// The Warden spawns a separate Claude session with a review-focused prompt,
// providing the git diff of changes made by the Smith. It returns a structured
// verdict: approve, reject, or request-changes with feedback.
package warden

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/smith"
)

// Verdict represents the Warden's review decision.
type Verdict string

const (
	VerdictApprove        Verdict = "approve"
	VerdictReject         Verdict = "reject"
	VerdictRequestChanges Verdict = "request_changes"
)

// ReviewResult captures the Warden's review outcome.
type ReviewResult struct {
	// Verdict is the review decision.
	Verdict Verdict
	// Summary is a brief summary of the review.
	Summary string
	// Issues is a list of specific issues found.
	Issues []ReviewIssue
	// RawOutput is the full Claude output.
	RawOutput string
	// Duration is how long the review took.
	Duration time.Duration
	// CostUSD is the cost of the review session.
	CostUSD float64
}

// ReviewIssue represents a specific issue found during review.
type ReviewIssue struct {
	// File is the affected file path.
	File string `json:"file"`
	// Line is the approximate line number (0 if unknown).
	Line int `json:"line"`
	// Severity is "error", "warning", or "suggestion".
	Severity string `json:"severity"`
	// Message describes the issue.
	Message string `json:"message"`
}

// Review runs a Warden review of the changes in the given worktree.
// It gets the git diff, spawns a Claude review session, and parses the verdict.
func Review(ctx context.Context, worktreePath, beadID, anvilPath string) (*ReviewResult, error) {
	start := time.Now()

	// Get the diff of changes
	diff, err := getDiff(ctx, worktreePath)
	if err != nil {
		return nil, fmt.Errorf("getting diff: %w", err)
	}

	if strings.TrimSpace(diff) == "" {
		return &ReviewResult{
			Verdict:  VerdictReject,
			Summary:  "No changes detected — Smith produced no diff",
			Duration: time.Since(start),
		}, nil
	}

	// Build the review prompt
	prompt := buildReviewPrompt(beadID, diff, anvilPath)

	// Spawn a Claude review session
	logDir := filepath.Join(anvilPath, ".workers", "logs")
	process, err := smith.Spawn(ctx, worktreePath, prompt, logDir, []string{"--max-turns", "1"})
	if err != nil {
		return nil, fmt.Errorf("spawning warden: %w", err)
	}

	// Wait for the review to complete
	smithResult := process.Wait()

	result := &ReviewResult{
		RawOutput: smithResult.Output,
		Duration:  time.Since(start),
		CostUSD:   smithResult.CostUSD,
	}

	// Parse the verdict from the output
	parseVerdict(smithResult.Output, result)

	return result, nil
}

// getDiff returns the git diff of uncommitted changes in the worktree.
func getDiff(ctx context.Context, worktreePath string) (string, error) {
	// First try staged + unstaged diff against the branch point
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Get diff of all changes (staged and unstaged)
	cmd := exec.CommandContext(cmdCtx, "git", "diff", "HEAD")
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// If HEAD doesn't exist (fresh repo), try just the index
		cmd2 := exec.CommandContext(ctx, "git", "diff")
		cmd2.Dir = worktreePath
		var out2 bytes.Buffer
		cmd2.Stdout = &out2
		if err2 := cmd2.Run(); err2 != nil {
			return "", fmt.Errorf("git diff failed: %v (stderr: %s)", err, stderr.String())
		}
		return out2.String(), nil
	}

	// Also include untracked files as a summary
	diff := stdout.String()

	// Check for any commits on the branch not on the base
	cmd3 := exec.CommandContext(ctx, "git", "log", "--oneline", "origin/main..HEAD")
	cmd3.Dir = worktreePath
	var logOut bytes.Buffer
	cmd3.Stdout = &logOut
	if cmd3.Run() == nil && logOut.Len() > 0 {
		// There are commits — get the full diff against origin/main
		cmd4 := exec.CommandContext(ctx, "git", "diff", "origin/main...HEAD")
		cmd4.Dir = worktreePath
		var diffOut bytes.Buffer
		cmd4.Stdout = &diffOut
		if cmd4.Run() == nil {
			diff = diffOut.String()
		}
	}

	return diff, nil
}

// buildReviewPrompt creates the Warden's review prompt.
func buildReviewPrompt(beadID, diff, anvilPath string) string {
	// Read AGENTS.md for context on coding standards
	agentsMD := ""
	if data, err := os.ReadFile(filepath.Join(anvilPath, "AGENTS.md")); err == nil {
		agentsMD = string(data)
	}

	return fmt.Sprintf(`You are a code reviewer (the "Warden") for an AI-generated pull request.

## Task

Review the following git diff for bead %s. Your job is to:

1. Check for correctness — does the code work as intended?
2. Check for coding standards — does it follow the repository's conventions?
3. Check for completeness — does it fully implement what was requested?
4. Check for safety — any security issues, resource leaks, error handling gaps?
5. Check for tests — are there adequate tests for the changes?

## Your Verdict

You MUST output a JSON block at the end of your response in exactly this format:

%s

Where:
- verdict is one of: "approve", "reject", "request_changes"
- summary is a one-line summary of your review
- issues is an array of specific issues (empty if approving)

## Review Guidelines

- "approve" means the code is good to merge as-is
- "request_changes" means minor fixes are needed (the Smith can fix them)
- "reject" means fundamental issues that need a complete rethink

## Git Diff

%s

%s
%s
%s`,
		beadID,
		"```json\n{\"verdict\": \"approve|reject|request_changes\", \"summary\": \"...\", \"issues\": [{\"file\": \"path\", \"line\": 0, \"severity\": \"error|warning|suggestion\", \"message\": \"...\"}]}\n```",
		"```diff\n"+truncateDiff(diff, 50000)+"\n```",
		conditionalSection("## Repository Guidelines (AGENTS.md)", agentsMD),
		"",
		"Be thorough but practical. Focus on issues that would cause bugs or maintenance problems.",
	)
}

// parseVerdict extracts the structured verdict from Claude's output.
func parseVerdict(output string, result *ReviewResult) {
	// Try to find a JSON block in the output
	jsonStr := extractJSON(output)
	if jsonStr != "" {
		var parsed struct {
			Verdict string        `json:"verdict"`
			Summary string        `json:"summary"`
			Issues  []ReviewIssue `json:"issues"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &parsed); err == nil {
			switch Verdict(parsed.Verdict) {
			case VerdictApprove, VerdictReject, VerdictRequestChanges:
				result.Verdict = Verdict(parsed.Verdict)
			default:
				result.Verdict = VerdictRequestChanges
			}
			result.Summary = parsed.Summary
			result.Issues = parsed.Issues
			return
		}
	}

	// Fallback: try to infer from keywords
	lower := strings.ToLower(output)
	switch {
	case strings.Contains(lower, "\"verdict\": \"approve\"") || strings.Contains(lower, "lgtm") || strings.Contains(lower, "looks good"):
		result.Verdict = VerdictApprove
		result.Summary = "Inferred approval from output"
	case strings.Contains(lower, "\"verdict\": \"reject\""):
		result.Verdict = VerdictReject
		result.Summary = "Inferred rejection from output"
	default:
		result.Verdict = VerdictRequestChanges
		result.Summary = "Could not parse structured verdict; defaulting to request_changes"
	}
}

// extractJSON finds the first JSON object in the text that looks like a verdict.
func extractJSON(text string) string {
	// Look for JSON blocks in code fences
	start := strings.Index(text, "```json")
	if start != -1 {
		start += len("```json")
		end := strings.Index(text[start:], "```")
		if end != -1 {
			return strings.TrimSpace(text[start : start+end])
		}
	}

	// Look for raw JSON objects containing "verdict"
	for i := 0; i < len(text); i++ {
		if text[i] == '{' {
			// Find matching closing brace
			depth := 0
			for j := i; j < len(text); j++ {
				switch text[j] {
				case '{':
					depth++
				case '}':
					depth--
					if depth == 0 {
						candidate := text[i : j+1]
						if strings.Contains(candidate, "verdict") {
							return candidate
						}
					}
				}
			}
		}
	}

	return ""
}

// truncateDiff limits the diff size to avoid token overflow.
func truncateDiff(diff string, maxLen int) string {
	if len(diff) <= maxLen {
		return diff
	}
	return diff[:maxLen] + "\n\n... (diff truncated, " + fmt.Sprintf("%d", len(diff)-maxLen) + " bytes omitted)"
}

// conditionalSection returns a formatted section if content is non-empty.
func conditionalSection(header, content string) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}
	return fmt.Sprintf("\n%s\n\n%s\n", header, content)
}
