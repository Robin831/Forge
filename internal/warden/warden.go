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

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
)

// Verdict represents the Warden's review decision.
type Verdict string

const (
	VerdictApprove        Verdict = "approve"
	VerdictReject         Verdict = "reject"
	VerdictRequestChanges Verdict = "request_changes"
)

// wardenMaxTurns is the maximum number of turns the Warden review session may
// use. Enough to output the verdict JSON first and then do file analysis.
// Higher than 3 (the previous value) because tool reads can consume turns
// before the model emits the verdict block.
const wardenMaxTurns = 5

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
	// NoDiff is true when the rejection was because Smith produced no diff.
	NoDiff bool
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
//
// db and anvilName are used to log lifecycle events; db may be nil to skip logging.
// providers is the ordered list of AI providers to try. When empty,
// provider.Defaults() is used. Provider fallback applies on rate limit.
func Review(ctx context.Context, worktreePath, beadID, anvilPath string, db *state.DB, anvilName string, providers ...provider.Provider) (*ReviewResult, error) {
	start := time.Now()

	if db != nil {
		_ = db.LogEvent(state.EventWardenStarted, fmt.Sprintf("Starting review for %s", beadID), beadID, anvilName)
	}

	pvList := providers
	if len(pvList) == 0 {
		pvList = provider.Defaults()
	}

	// Get the diff of changes
	diff, err := getDiff(ctx, worktreePath)
	if err != nil {
		return nil, fmt.Errorf("getting diff: %w", err)
	}

	if strings.TrimSpace(diff) == "" {
		result := &ReviewResult{
			Verdict:  VerdictReject,
			Summary:  "No changes detected — Smith produced no diff",
			Duration: time.Since(start),
			NoDiff:   true,
		}
		if db != nil {
			_ = db.LogEvent(state.EventWardenReject,
				fmt.Sprintf("Verdict: %s — %s", result.Verdict, result.Summary),
				beadID, anvilName)
		}
		return result, nil
	}

	// Build the review prompt
	prompt := buildReviewPrompt(beadID, diff, anvilPath)

	// Spawn a Claude review session. The diff is embedded in the prompt so
	// Claude doesn't need to read files. Previously --tools "" was passed to
	// try to disable tool use, but it was unreliable across providers and caused
	// error_max_turns before the verdict was emitted. Instead the prompt now
	// instructs Claude to output the verdict JSON FIRST so partial runs are
	// still parseable. max-turns is set to 5 to give Claude enough room to
	// output the verdict and then do analysis (even if it reads a few files).
	logDir := filepath.Join(worktreePath, ".forge-logs")
	wardenFlags := []string{"--max-turns", fmt.Sprintf("%d", wardenMaxTurns)}

	var smithResult *smith.Result
	// For non-Claude providers --max-turns is translated/dropped;
	// pass flags as-is (provider.BuildArgs handles translation).
	for pi, pv := range pvList {
		process, err := smith.SpawnWithProvider(ctx, worktreePath, prompt, logDir, pv, wardenFlags)
		if err != nil {
			return nil, fmt.Errorf("spawning warden (%s): %w", pv.Label(), err)
		}
		smithResult = process.Wait()
		if !smithResult.RateLimited {
			break
		}
		if pi+1 < len(pvList) {
			// try next provider
			continue
		}
		// All providers exhausted
		return nil, fmt.Errorf("all warden providers rate limited")
	}

	result := &ReviewResult{
		RawOutput: smithResult.Output,
		Duration:  time.Since(start),
		CostUSD:   smithResult.CostUSD,
	}

	// Parse the verdict from the full text output (stream-json result field)
	outputText := smithResult.FullOutput
	if outputText == "" {
		outputText = smithResult.Output
	}
	parseVerdict(outputText, result)

	if db != nil {
		evtType := state.EventWardenPass
		if result.Verdict == VerdictReject || result.Verdict == VerdictRequestChanges {
			evtType = state.EventWardenReject
		}
		_ = db.LogEvent(evtType, fmt.Sprintf("Verdict: %s — %s", result.Verdict, result.Summary), beadID, anvilName)
	}

	return result, nil
}

// getDiff returns the git diff of uncommitted changes in the worktree.
func getDiff(ctx context.Context, worktreePath string) (string, error) {
	// First try staged + unstaged diff against the branch point
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Get diff of all changes (staged and unstaged)
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, "git", "diff", "HEAD"))
	cmd.Dir = worktreePath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// If HEAD doesn't exist (fresh repo), try just the index
		cmd2 := executil.HideWindow(exec.CommandContext(ctx, "git", "diff"))
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
	cmd3 := executil.HideWindow(exec.CommandContext(ctx, "git", "log", "--oneline", "origin/main..HEAD"))
	cmd3.Dir = worktreePath
	var logOut bytes.Buffer
	cmd3.Stdout = &logOut
	if cmd3.Run() == nil && logOut.Len() > 0 {
		// There are commits — get the full diff against origin/main
		cmd4 := executil.HideWindow(exec.CommandContext(ctx, "git", "diff", "origin/main...HEAD"))
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

## REQUIRED: Output Your Verdict JSON Block First

Before writing anything else, output a JSON block in this format as the VERY FIRST content
in your response — even before any analysis or comments. Replace each field with your actual
verdict, summary, and list of issues:

%s

Fields:
- verdict: one of "approve", "reject", or "request_changes" (required — do not copy the example value)
- summary: a one-line summary of your overall review finding
- issues: array of specific problems found; use [] when approving

## Verdict Meanings

- "approve" — the code is correct and ready to merge as-is
- "request_changes" — minor fixable issues found; the Smith can address them
- "reject" — fundamental problems that require a complete rethink

## Task: Review Bead %s

After outputting the JSON verdict above, review the following git diff:

1. Check for correctness — does the code work as intended?
2. Check for coding standards — does it follow the repository's conventions?
3. Check for completeness — does it fully implement what was requested?
4. Check for safety — any security issues, resource leaks, error handling gaps?
5. Check for tests — are there adequate tests for the changes?

## Git Diff

%s

%s
%s`,
		"Use the following JSON format, replacing each field with your actual verdict, summary, and issues:\n\n```json\n{\"verdict\": \"request_changes\", \"summary\": \"\", \"issues\": []}\n```\n\nSet `verdict` to one of: `approve`, `reject`, `request_changes`.",
		beadID,
		"```diff\n"+truncateDiff(diff, 50000)+"\n```",
		conditionalSection("## Repository Guidelines (AGENTS.md)", agentsMD),
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

	// Fallback: scan for verdict value with several formatting variants
	// (handles missing spaces, Gemini markdown quirks, etc.)
	norm := strings.ToLower(strings.ReplaceAll(output, " ", ""))
	switch {
	case strings.Contains(norm, `"verdict":"approve"`) ||
		strings.Contains(strings.ToLower(output), "lgtm") ||
		strings.Contains(strings.ToLower(output), "looks good to merge"):
		result.Verdict = VerdictApprove
		result.Summary = "Inferred approval from output"
	case strings.Contains(norm, `"verdict":"reject"`):
		result.Verdict = VerdictReject
		result.Summary = "Inferred rejection from output"
	case strings.Contains(norm, `"verdict":"request_changes"`) ||
		strings.Contains(norm, `"verdict":"requestchanges"`):
		result.Verdict = VerdictRequestChanges
		result.Summary = "Inferred request_changes from output"
	default:
		result.Verdict = VerdictRequestChanges
		result.Summary = "Could not parse structured verdict; defaulting to request_changes"
	}
}

// extractJSON finds the first JSON object in the text that looks like a verdict.
func extractJSON(text string) string {
	// 1. Look for ```json ... ``` blocks (Claude style)
	if s := extractFencedBlock(text, "```json"); s != "" {
		return s
	}

	// 2. Look for plain ``` ... ``` blocks that contain "verdict" (Gemini style)
	if s := extractFencedBlock(text, "```"); s != "" && strings.Contains(s, "verdict") {
		return s
	}

	// 3. Look for raw JSON objects containing "verdict"
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

// extractFencedBlock returns the content between the first occurrence of
// fence and the next closing ```.  Returns "" if not found.
func extractFencedBlock(text, fence string) string {
	start := strings.Index(text, fence)
	if start == -1 {
		return ""
	}
	start += len(fence)
	// Skip optional space/newline immediately after the fence marker
	for start < len(text) && (text[start] == '\n' || text[start] == '\r' || text[start] == ' ') {
		start++
	}
	end := strings.Index(text[start:], "```")
	if end == -1 {
		return ""
	}
	return strings.TrimSpace(text[start : start+end])
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
