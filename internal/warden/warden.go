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
	// UsedProvider records which provider actually completed the review.
	UsedProvider *provider.Provider
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
	var usedProvider provider.Provider
	for pi, pv := range pvList {
		process, err := smith.SpawnWithProvider(ctx, worktreePath, prompt, logDir, pv, wardenFlags)
		if err != nil {
			return nil, fmt.Errorf("spawning warden (%s): %w", pv.Label(), err)
		}
		smithResult = process.Wait()
		if !smithResult.RateLimited {
			usedProvider = pv
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
		RawOutput:    smithResult.Output,
		Duration:     time.Since(start),
		CostUSD:      smithResult.CostUSD,
		UsedProvider: &usedProvider,
	}

	// Parse the verdict from the full text output (stream-json result field)
	outputText := smithResult.FullOutput
	if outputText == "" {
		outputText = smithResult.Output
	}
	parseVerdict(outputText, usedProvider.Kind, result)

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

	// Load learned review rules for this anvil
	rulesSection := ""
	if rf, err := LoadRules(anvilPath); err == nil {
		if checklist := rf.FormatChecklist(); checklist != "" {
			rulesSection = "\n## Learned Review Rules\n\nThese are domain-specific patterns learned from past reviews. Check each one against the diff:\n\n" + checklist
		}
	} else {
		fmt.Fprintf(os.Stderr, "warden: failed to load learned review rules for %s: %v\n", anvilPath, err)
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
%s
## Git Diff

%s

%s
%s`,
		"Use the following JSON format, replacing each field with your actual verdict, summary, and issues:\n\n```json\n{\"verdict\": \"approve\", \"summary\": \"\", \"issues\": []}\n```\n\nSet `verdict` to one of: `approve`, `reject`, `request_changes`.",
		beadID,
		rulesSection,
		"```diff\n"+truncateDiff(diff, 50000)+"\n```",
		conditionalSection("## Repository Guidelines (AGENTS.md)", agentsMD),
		"Be thorough but practical. Focus on issues that would cause bugs or maintenance problems.",
	)
}

// parseVerdict extracts the structured verdict from the review output.
// providerKind enables provider-specific fallback heuristics when the primary
// JSON extraction fails. Each provider has different tendencies:
//   - Claude: reliably emits fenced ```json blocks
//   - Gemini: sometimes uses plain ``` blocks or embeds JSON in prose
//   - Copilot (Haiku): often outputs natural language verdicts without JSON
func parseVerdict(output string, providerKind provider.Kind, result *ReviewResult) {
	// Phase 1: Try structured JSON extraction — works across all providers.
	jsonStr := extractJSON(output, "verdict")
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
				result.Verdict = VerdictApprove
				parsed.Summary = "Unknown verdict value in parsed JSON; defaulting to approve for human review"
			}
			result.Summary = parsed.Summary
			result.Issues = parsed.Issues
			return
		}
	}

	// Phase 2: Provider-specific fallback heuristics.
	switch providerKind {
	case provider.Copilot:
		parseCopilotVerdict(output, result)
	case provider.Gemini:
		parseGeminiVerdict(output, result)
	default:
		parseClaudeVerdict(output, result)
	}
}

// parseClaudeVerdict handles fallback parsing for Claude output.
// Claude almost always emits valid JSON; this is a rare edge case.
func parseClaudeVerdict(output string, result *ReviewResult) {
	norm := strings.ToLower(strings.ReplaceAll(output, " ", ""))
	switch {
	case strings.Contains(norm, `"verdict":"approve"`) ||
		strings.Contains(strings.ToLower(output), "lgtm") ||
		strings.Contains(strings.ToLower(output), "looks good to merge"):
		result.Verdict = VerdictApprove
		result.Summary = "Inferred approval from output (claude fallback)"
	case strings.Contains(norm, `"verdict":"reject"`):
		result.Verdict = VerdictReject
		result.Summary = "Inferred rejection from output (claude fallback)"
	case strings.Contains(norm, `"verdict":"request_changes"`):
		result.Verdict = VerdictRequestChanges
		result.Summary = "Inferred request_changes from output (claude fallback)"
	default:
		result.Verdict = VerdictApprove
		result.Summary = "Could not parse structured verdict; defaulting to approve for human review"
	}
}

// parseGeminiVerdict handles fallback parsing for Gemini output.
// Gemini may wrap verdicts in markdown bold, headers, or key-value lines.
func parseGeminiVerdict(output string, result *ReviewResult) {
	lower := strings.ToLower(output)
	norm := strings.ToLower(strings.ReplaceAll(output, " ", ""))

	// First try the same JSON-fragment checks.
	switch {
	case strings.Contains(norm, `"verdict":"approve"`):
		result.Verdict = VerdictApprove
		result.Summary = "Inferred approval from output (gemini fallback)"
		return
	case strings.Contains(norm, `"verdict":"request_changes"`):
		result.Verdict = VerdictRequestChanges
		result.Summary = "Inferred request_changes from output (gemini fallback)"
		return
	case strings.Contains(norm, `"verdict":"reject"`):
		result.Verdict = VerdictReject
		result.Summary = "Inferred rejection from output (gemini fallback)"
		return
	}

	// Gemini sometimes uses "Verdict: approve" or "**Verdict:** approve" in prose.
	if v, ok := extractKeyValueVerdict(lower); ok {
		result.Verdict = v
		result.Summary = "Inferred verdict from key-value line (gemini fallback)"
		return
	}

	// Natural language signals.
	if containsAny(lower, "lgtm", "looks good to merge", "approve this", "code is correct and ready") {
		result.Verdict = VerdictApprove
		result.Summary = "Inferred approval from prose (gemini fallback)"
		return
	}
	if containsAny(lower, "changes need", "request changes", "needs to be fixed", "issues that should be addressed") {
		result.Verdict = VerdictRequestChanges
		result.Summary = "Inferred request_changes from prose (gemini fallback)"
		return
	}

	result.Verdict = VerdictApprove
	result.Summary = "Could not parse structured verdict; defaulting to approve for human review"
}

// parseCopilotVerdict handles fallback parsing for Copilot (Haiku) output.
// Haiku frequently outputs natural language reviews without any JSON, so
// this parser is the most aggressive at extracting verdicts from prose.
func parseCopilotVerdict(output string, result *ReviewResult) {
	lower := strings.ToLower(output)
	norm := strings.ToLower(strings.ReplaceAll(output, " ", ""))

	// Try JSON fragments first (sometimes Copilot does emit partial JSON).
	switch {
	case strings.Contains(norm, `"verdict":"approve"`):
		result.Verdict = VerdictApprove
		result.Summary = "Inferred approval from output (copilot fallback)"
		return
	case strings.Contains(norm, `"verdict":"request_changes"`):
		result.Verdict = VerdictRequestChanges
		result.Summary = "Inferred request_changes from output (copilot fallback)"
		return
	case strings.Contains(norm, `"verdict":"reject"`):
		result.Verdict = VerdictReject
		result.Summary = "Inferred rejection from output (copilot fallback)"
		return
	}

	// Key-value style: "Verdict: approve", "**Verdict**: request_changes", etc.
	if v, ok := extractKeyValueVerdict(lower); ok {
		result.Verdict = v
		result.Summary = "Inferred verdict from key-value line (copilot fallback)"
		return
	}

	// Copilot/Haiku approval signals — broader set than other providers.
	if containsAny(lower,
		"lgtm", "looks good to merge", "looks good to me",
		"i approve", "approve this", "approved",
		"code is correct and ready", "ready to merge",
		"no issues found", "no significant issues",
	) {
		result.Verdict = VerdictApprove
		result.Summary = "Inferred approval from prose (copilot fallback)"
		return
	}

	// Rejection signals.
	if containsAny(lower,
		"i reject", "rejecting this", "fundamental problem",
		"requires a complete rethink", "cannot approve",
	) {
		result.Verdict = VerdictReject
		result.Summary = "Inferred rejection from prose (copilot fallback)"
		return
	}

	// Request-changes signals — check AFTER rejection to avoid false positives.
	if containsAny(lower,
		"request changes", "requesting changes", "changes requested",
		"changes need", "needs to be fixed", "should be addressed",
		"issues that need", "please fix", "must be fixed",
		"several issues", "some issues",
	) {
		result.Verdict = VerdictRequestChanges
		result.Summary = "Inferred request_changes from prose (copilot fallback)"
		return
	}

	result.Verdict = VerdictApprove
	result.Summary = "Could not parse structured verdict; defaulting to approve for human review"
}

// extractKeyValueVerdict looks for "verdict: <value>" or "**verdict**: <value>"
// patterns in lowercased text. Returns the verdict and true if found.
func extractKeyValueVerdict(lower string) (Verdict, bool) {
	// Match patterns like "verdict: approve", "verdict : request_changes",
	// "**verdict:** approve", "**verdict**: reject"
	patterns := []string{"verdict:", "**verdict**:", "**verdict:**"}
	for _, pat := range patterns {
		idx := strings.Index(lower, pat)
		if idx == -1 {
			continue
		}
		// Extract the word(s) after the colon.
		after := strings.TrimSpace(lower[idx+len(pat):])
		// Take only the first line / first few words.
		if nl := strings.IndexAny(after, "\n\r"); nl != -1 {
			after = after[:nl]
		}
		after = strings.TrimSpace(after)
		// Strip surrounding quotes, backticks, bold markers.
		after = strings.Trim(after, "\"'`*")
		after = strings.TrimSpace(after)

		switch {
		case strings.HasPrefix(after, "approve"):
			return VerdictApprove, true
		case strings.HasPrefix(after, "request_changes") || strings.HasPrefix(after, "request changes"):
			return VerdictRequestChanges, true
		case strings.HasPrefix(after, "reject"):
			return VerdictReject, true
		}
	}
	return "", false
}

// containsAny returns true if s contains any of the given substrings.
func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}


// extractJSON finds the first JSON object in the text that contains the given
// requiredKey. When requiredKey is empty or omitted, any JSON object is returned.
// The requiredKey is matched as a quoted JSON key ("key") to avoid false
// positives from occurrences inside string values.
func extractJSON(text string, requiredKey ...string) string {
	key := ""
	if len(requiredKey) > 0 {
		key = requiredKey[0]
	}
	quotedKey := ""
	if key != "" {
		quotedKey = `"` + key + `"`
	}

	containsKey := func(s string) bool {
		return quotedKey == "" || strings.Contains(s, quotedKey)
	}

	// 1. Look for ```json ... ``` blocks (Claude style)
	if s := extractFencedBlock(text, "```json"); s != "" {
		if containsKey(s) {
			return s
		}
	}

	// 2. Look for plain ``` ... ``` blocks that contain the required key
	if s := extractFencedBlock(text, "```"); s != "" {
		if containsKey(s) {
			return s
		}
	}

	// 3. Look for raw JSON objects containing the required key
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
						if containsKey(candidate) {
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
