package warden

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
)

// PRComment represents a review comment fetched from GitHub.
type PRComment struct {
	Body     string `json:"body"`
	User     string `json:"user"`
	Path     string `json:"path"`
	PRNumber int    `json:"pr_number"`
}

// ghReviewComment is the JSON shape returned by `gh api`.
type ghReviewComment struct {
	Body string `json:"body"`
	Path string `json:"path"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// parsePaginatedComments decodes the stdout of `gh api --paginate`, which
// concatenates one JSON array per page (e.g. [page1...][page2...]). A plain
// json.Unmarshal fails on this format, and json.Decoder.More() returns false
// at the top level before any array is decoded, so we loop until io.EOF.
func parsePaginatedComments(data []byte) ([]ghReviewComment, error) {
	var all []ghReviewComment
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var page []ghReviewComment
		if err := dec.Decode(&page); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parsing gh response: %w", err)
		}
		all = append(all, page...)
	}
	return all, nil
}

// FetchCopilotComments retrieves review comments on a PR that were authored by
// copilot[bot], github-actions[bot], or copilot via the gh CLI.
func FetchCopilotComments(ctx context.Context, repoDir string, prNumber int) ([]PRComment, error) {
	endpoint := fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/comments", prNumber)
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", "api", endpoint, "--paginate"))
	cmd.Dir = repoDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}

	// gh api --paginate for REST endpoints concatenates multiple JSON arrays
	// (e.g. [page1...][page2...]) which json.Unmarshal cannot parse as one
	// array. Use parsePaginatedComments to handle single and multi-page output.
	raw, err := parsePaginatedComments(stdout.Bytes())
	if err != nil {
		return nil, err
	}

	var comments []PRComment
	for _, c := range raw {
		login := strings.ToLower(c.User.Login)
		if login == "copilot[bot]" || login == "github-actions[bot]" || login == "copilot" {
			comments = append(comments, PRComment{
				Body:     c.Body,
				User:     c.User.Login,
				Path:     c.Path,
				PRNumber: prNumber,
			})
		}
	}
	return comments, nil
}

// FetchRecentPRNumbers returns the most recent merged PR numbers for a repo.
func FetchRecentPRNumbers(ctx context.Context, repoDir string, limit int) ([]int, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "gh", "pr", "list",
		"--state=merged", "--limit", fmt.Sprintf("%d", limit), "--json=number"))
	cmd.Dir = repoDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh pr list: %s (%w)", strings.TrimSpace(stderr.String()), err)
	}

	var prs []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &prs); err != nil {
		return nil, fmt.Errorf("parsing PR list: %w", err)
	}

	nums := make([]int, len(prs))
	for i, pr := range prs {
		nums[i] = pr.Number
	}
	return nums, nil
}

// aiRunner executes an AI session with the given prompt and returns its
// full text response. It uses smith.SpawnWithProvider to benefit from
// stream-json parsing, provider fallback, and cost tracking. It is a
// package-level variable so tests can inject a stub without spawning a real
// process.
var aiRunner = func(ctx context.Context, dir, prompt string) ([]byte, error) {
	logDir := filepath.Join(dir, ".forge-logs")
	providers := provider.Defaults()
	var lastErr error

	for _, pv := range providers {
		// Distillation should be quick, but give it 5 turns just in case AI
		// wants to look at some files to understand the context of the comments.
		extraFlags := []string{"--max-turns", "5"}
		process, err := smith.SpawnWithProvider(ctx, dir, prompt, logDir, pv, extraFlags)
		if err != nil {
			lastErr = err
			continue
		}
		result := process.Wait()
		if result.RateLimited {
			lastErr = fmt.Errorf("provider %s rate limited", pv.Label())
			continue
		}
		if result.IsError {
			lastErr = fmt.Errorf("ai error (%s): %s", pv.Label(), result.ErrorOutput)
			continue
		}
		// Treat non-zero exit codes as errors, even if some output was produced.
		if result.ExitCode != 0 {
			lastErr = fmt.Errorf("ai process error (%s): exit code %d: %s", pv.Label(), result.ExitCode, strings.TrimSpace(result.ErrorOutput))
			continue
		}
		// Treat non-success result subtypes (e.g., error_max_turns) as errors to
		// ensure we don't proceed with partial/invalid output and allow fallback.
		if result.ResultSubtype != "" && result.ResultSubtype != "success" {
			lastErr = fmt.Errorf("ai error subtype (%s): %s: %s", pv.Label(), result.ResultSubtype, strings.TrimSpace(result.ErrorOutput))
			continue
		}

		// Return the full output text from the result event, which excludes
		// the prompt and internal protocol junk.
		output := result.FullOutput
		if output == "" {
			output = result.Output // Fallback for plain-text providers
		}
		return []byte(output), nil
	}
	return nil, fmt.Errorf("ai distillation failed across all providers: %w", lastErr)
}

// DistillRule uses a Claude session to convert a set of similar review
// comments into a single warden rule. Returns the rule or an error.
func DistillRule(ctx context.Context, comments []PRComment, repoDir string) (*Rule, error) {
	if len(comments) == 0 {
		return nil, fmt.Errorf("no comments to distill")
	}

	// Build a prompt for Claude to generate a rule
	var sb strings.Builder
	sb.WriteString("You are helping create a code review checklist rule from Copilot review comments.\n\n")
	sb.WriteString("Given these review comments, create a single reusable review rule.\n\n")
	sb.WriteString("## Comments\n\n")
	for i, c := range comments {
		sb.WriteString(fmt.Sprintf("### Comment %d (PR #%d, file: %s)\n%s\n\n", i+1, c.PRNumber, c.Path, c.Body))
	}
	sb.WriteString(`## Output Format

Respond with ONLY a JSON object (no markdown fences, no explanation) in this exact format:

{"id": "short-kebab-id", "category": "concurrency|ui|error-handling|security|testing|performance|style|other", "pattern": "What code pattern triggers this check", "check": "What the reviewer should verify"}
`)

	prompt := sb.String()

	raw, err := aiRunner(ctx, repoDir, prompt)
	if err != nil {
		return nil, fmt.Errorf("ai distill: %w", err)
	}

	// Extract JSON from AI's output
	output := strings.TrimSpace(string(raw))
	jsonStr := extractJSON(output, "id")
	if jsonStr == "" {
		// Try the raw output directly
		jsonStr = output
	}

	var rule Rule
	if err := json.Unmarshal([]byte(jsonStr), &rule); err != nil {
		return nil, fmt.Errorf("parsing distilled rule: %w (output: %s)", err, output)
	}

	if rule.ID == "" || rule.Check == "" {
		return nil, fmt.Errorf("distilled rule missing required fields (id or check)")
	}

	// Set provenance
	prNums := map[int]bool{}
	for _, c := range comments {
		prNums[c.PRNumber] = true
	}
	sortedNums := make([]int, 0, len(prNums))
	for n := range prNums {
		sortedNums = append(sortedNums, n)
	}
	sort.Ints(sortedNums)
	var sources []string
	for _, n := range sortedNums {
		sources = append(sources, fmt.Sprintf("copilot:PR#%d", n))
	}
	rule.Source = strings.Join(sources, ", ")
	rule.Added = time.Now().Format("2006-01-02")

	return &rule, nil
}

// lintRulePattern matches ESLint-style rule IDs such as:
//   - react-hooks/exhaustive-deps
//   - @typescript-eslint/no-floating-promises
//   - import/order
//
// This pattern intentionally over-accepts and is paired with isLikelyLintRule
// to filter out obvious file paths (e.g. src/index).
var lintRulePattern = regexp.MustCompile(`@?[a-z0-9][a-z0-9-]*/[a-z][a-z0-9-]*`)

// isLikelyLintRule applies a lightweight heuristic to distinguish ESLint rule
// IDs from common file path prefixes in CI logs.
func isLikelyLintRule(id string) bool {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 {
		return false
	}
	ns := parts[0]
	if strings.HasPrefix(ns, "@") {
		ns = strings.TrimPrefix(ns, "@")
	}
	switch ns {
	case "src", "dist", "build", "lib", "app", "apps", "cmd", "internal", "bin", "packages", "node_modules":
		return false
	default:
		return true
	}
}

// extractLintRuleNames scans CI log output for ESLint-style rule IDs and
// returns the unique set, sorted for deterministic ordering.
func extractLintRuleNames(logs map[string]string) []string {
	seen := make(map[string]bool)
	for _, logContent := range logs {
		for _, match := range lintRulePattern.FindAllString(logContent, -1) {
			if !isLikelyLintRule(match) {
				continue
			}
			seen[match] = true
		}
	}
	rules := make([]string, 0, len(seen))
	for r := range seen {
		rules = append(rules, r)
	}
	sort.Strings(rules)
	return rules
}

// LearnFromCIFix extracts lint rule patterns from CI failure logs and the
// subsequent fix diff, then stores them as warden rules so future Smith
// sessions avoid the anti-patterns before they reach CI.
//
// It is intentionally non-fatal: callers should log any returned error but
// not let it block the successful CI fix result from being recorded.
func LearnFromCIFix(ctx context.Context, anvilPath, repoDir string, failingLogs map[string]string, fixDiff string, prNumber int) error {
	if len(failingLogs) == 0 || fixDiff == "" {
		return nil
	}

	ruleNames := extractLintRuleNames(failingLogs)
	if len(ruleNames) == 0 {
		return nil
	}

	// Cap learning to avoid spawning too many sequential Claude calls for
	// CI logs that surface dozens of distinct rule IDs.
	const maxRulesToLearn = 5
	if len(ruleNames) > maxRulesToLearn {
		log.Printf("[warden] LearnFromCIFix: capping rule learning at %d (found %d)", maxRulesToLearn, len(ruleNames))
		ruleNames = ruleNames[:maxRulesToLearn]
	}

	rf, err := LoadRules(anvilPath)
	if err != nil {
		return fmt.Errorf("loading rules: %w", err)
	}

	// Build a set of existing rule IDs to skip duplicates without calling Claude.
	existingIDs := make(map[string]bool)
	for _, r := range rf.Rules {
		existingIDs[r.ID] = true
	}

	source := fmt.Sprintf("cifix:PR#%d", prNumber)
	changed := false

	for _, ruleName := range ruleNames {
		// Derive a candidate rule ID: strip @ and replace / with -.
		ruleID := strings.ReplaceAll(strings.TrimPrefix(ruleName, "@"), "/", "-")
		if existingIDs[ruleID] {
			continue
		}

		rule, err := distillCIFixRule(ctx, ruleName, ruleID, failingLogs, fixDiff, source, repoDir)
		if err != nil {
			log.Printf("[warden] LearnFromCIFix: distill rule %q: %v", ruleName, err)
			continue
		}

		if rf.AddRule(*rule) {
			existingIDs[rule.ID] = true
			changed = true
			log.Printf("[warden] Learned new CI fix rule: %s (source: %s)", rule.ID, rule.Source)
		}
	}

	if changed {
		return SaveRules(anvilPath, rf)
	}
	return nil
}

// distillCIFixRule asks Claude to convert a CI lint failure pattern and its
// fix diff into a structured warden Rule.
func distillCIFixRule(ctx context.Context, ruleName, ruleID string, failingLogs map[string]string, fixDiff, source, repoDir string) (*Rule, error) {
	// Collect log lines that mention this specific rule.
	var ruleLines []string
	for _, logContent := range failingLogs {
		for _, line := range strings.Split(logContent, "\n") {
			if strings.Contains(line, ruleName) {
				trimmed := strings.TrimSpace(line)
				if trimmed != "" {
					ruleLines = append(ruleLines, trimmed)
				}
			}
		}
	}

	var sb strings.Builder
	sb.WriteString("You are creating a code review rule from a CI lint failure that was subsequently fixed.\n\n")
	fmt.Fprintf(&sb, "The failing ESLint rule was: **%s**\n\n", ruleName)

	if len(ruleLines) > 0 {
		limit := 20
		if len(ruleLines) < limit {
			limit = len(ruleLines)
		}
		sb.WriteString("## Failing Log Lines\n\n```\n")
		sb.WriteString(strings.Join(ruleLines[:limit], "\n"))
		sb.WriteString("\n```\n\n")
	}

	truncated := fixDiff
	const maxDiffLen = 3000
	if runes := []rune(truncated); len(runes) > maxDiffLen {
		truncated = "... (truncated)\n" + string(runes[len(runes)-maxDiffLen:])
	}
	fmt.Fprintf(&sb, "## Fix Diff\n\n```diff\n%s\n```\n\n", truncated)

	fmt.Fprintf(&sb, `## Output Format

Respond with ONLY a JSON object (no markdown fences, no explanation) using this exact format:

{"id": %q, "category": "concurrency|ui|error-handling|security|testing|performance|style|other", "pattern": "What code pattern triggers this lint rule", "check": "What the reviewer should verify to avoid this lint violation"}
`, ruleID)

	prompt := sb.String()

	raw, err := aiRunner(ctx, repoDir, prompt)
	if err != nil {
		return nil, fmt.Errorf("ai distill: %w", err)
	}

	output := strings.TrimSpace(string(raw))
	jsonStr := extractJSON(output, "id")
	if jsonStr == "" {
		jsonStr = output
	}

	var rule Rule
	if err := json.Unmarshal([]byte(jsonStr), &rule); err != nil {
		return nil, fmt.Errorf("parsing distilled rule: %w (output: %s)", err, output)
	}

	if rule.ID == "" || rule.Check == "" {
		return nil, fmt.Errorf("distilled rule missing required fields (id or check)")
	}

	// Enforce the expected ID regardless of what Claude returned, so the
	// existingIDs skip logic and deduplication remain consistent.
	rule.ID = ruleID
	rule.Source = source
	rule.Added = time.Now().Format("2006-01-02")
	return &rule, nil
}

// GroupComments groups semantically similar comments using keyword overlap.
// First, comments with identical normalized text are merged. Then, groups
// whose keyword sets exceed a Jaccard similarity threshold are merged
// together so that comments about the same pattern (e.g. "missing error
// check on Open()" vs "error from ReadFile not handled") land in one group.
func GroupComments(comments []PRComment) [][]PRComment {
	if len(comments) == 0 {
		return nil
	}

	// Phase 1: exact-match grouping (preserves old behaviour as a fast path).
	seen := map[string]int{} // normalized body -> group index
	var groups [][]PRComment

	for _, c := range comments {
		key := normalizeBody(c.Body)
		if idx, ok := seen[key]; ok {
			groups[idx] = append(groups[idx], c)
		} else {
			seen[key] = len(groups)
			groups = append(groups, []PRComment{c})
		}
	}

	if len(groups) <= 1 {
		return groups
	}

	// Phase 2: semantic merge via keyword-overlap scoring.
	// Build keyword sets for each group (union of all comments in the group).
	kwSets := make([]map[string]bool, len(groups))
	for i, g := range groups {
		kw := map[string]bool{}
		for _, c := range g {
			for _, w := range extractKeywords(c.Body) {
				kw[w] = true
			}
		}
		kwSets[i] = kw
	}

	// Union-Find to merge similar groups.
	parent := make([]int, len(groups))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
			// Merge keyword sets so transitive matches work.
			for w := range kwSets[ra] {
				kwSets[rb][w] = true
			}
		}
	}

	const similarityThreshold = 0.30

	for i := 0; i < len(groups); i++ {
		for j := i + 1; j < len(groups); j++ {
			if find(i) == find(j) {
				continue
			}
			if jaccardSimilarity(kwSets[find(i)], kwSets[find(j)]) >= similarityThreshold {
				union(i, j)
			}
		}
	}

	// Collect merged groups.
	merged := map[int][]PRComment{}
	var order []int
	for i, g := range groups {
		root := find(i)
		if _, exists := merged[root]; !exists {
			order = append(order, root)
		}
		merged[root] = append(merged[root], g...)
	}

	result := make([][]PRComment, 0, len(merged))
	for _, root := range order {
		result = append(result, merged[root])
	}
	return result
}

// normalizeBody returns a lowered, trimmed version for dedup comparison.
func normalizeBody(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	// Collapse whitespace
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

// extractKeywords returns the significant lowercase tokens from a comment
// body after removing common stop words and short tokens.
func extractKeywords(body string) []string {
	words := strings.Fields(strings.ToLower(body))
	var out []string
	for _, w := range words {
		w = strings.Trim(w, ".,;:!?\"'`()[]{}#*-_/\\<>@=+~|")
		if len(w) < 3 {
			continue
		}
		if stopWords[w] {
			continue
		}
		out = append(out, w)
	}
	return out
}

// jaccardSimilarity returns |A ∩ B| / |A ∪ B|.
func jaccardSimilarity(a, b map[string]bool) float64 {
	// Treat two empty keyword sets as providing no evidence of similarity
	// to avoid over-merging comments that extract to zero keywords.
	if len(a) == 0 && len(b) == 0 {
		return 0.0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	unionSize := len(a) + len(b) - inter
	if unionSize == 0 {
		return 0
	}
	return float64(inter) / float64(unionSize)
}

// stopWords are common English words filtered out during keyword extraction.
var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "but": true,
	"not": true, "you": true, "all": true, "can": true, "had": true,
	"her": true, "was": true, "one": true, "our": true, "out": true,
	"has": true, "have": true, "been": true, "will": true, "more": true,
	"when": true, "who": true, "way": true, "about": true, "many": true,
	"then": true, "them": true, "would": true, "like": true, "some": true,
	"into": true, "its": true, "only": true, "also": true, "after": true,
	"that": true, "this": true, "with": true, "from": true, "they": true,
	"which": true, "could": true, "other": true, "than": true, "what": true,
	"their": true, "there": true, "these": true, "does": true, "should": true,
	"here": true, "each": true, "where": true, "those": true, "being": true,
}
