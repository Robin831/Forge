package assay

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Robin831/Forge/internal/diff"
)

// triageResult is what the Triage pass produces: the subset of changed files
// that warrant deeper review, plus free-form notes passed to the deep passes.
type triageResult struct {
	// ReviewFiles is the scoped file list. Empty means "review everything".
	ReviewFiles []string `json:"review_files"`
	// Notes is short guidance forwarded to the deep passes.
	Notes string `json:"notes"`
}

// runTriage runs the scoping pass. Like the deep passes it parses strict JSON
// with a single retry; a second failure is surfaced as a run error.
func runTriage(ctx context.Context, runner PassRunner, cfg Config, req ReviewRequest, filteredDiff string) (triageResult, float64, error) {
	prompt, err := buildTriagePrompt(req, filteredDiff)
	if err != nil {
		return triageResult{}, 0, err
	}

	out, err := runner(ctx, passTriage.Name, passTriage.Tier, prompt)
	if err != nil {
		return triageResult{}, 0, err
	}
	cost := out.CostUSD

	res, perr := parseTriage(out.Text)
	if perr != nil {
		out2, err2 := runner(ctx, passTriage.Name, passTriage.Tier, prompt+"\n\n"+strictJSONReminder)
		if err2 != nil {
			return triageResult{}, cost, err2
		}
		cost += out2.CostUSD
		res, perr = parseTriage(out2.Text)
		if perr != nil {
			return triageResult{}, cost, fmt.Errorf("assay pass %s: invalid JSON output after retry: %w", passTriage.Name, perr)
		}
	}
	return res, cost, nil
}

// buildTriagePrompt assembles the Triage prompt from its embedded instructions,
// its JSON contract, the change context, and the (already filtered) diff.
func buildTriagePrompt(req ReviewRequest, filteredDiff string) (string, error) {
	const contract = "## Required Output\n\n" +
		"Respond with a single JSON object and nothing else:\n\n" +
		"```json\n" +
		`{"review_files": ["path/one", "path/two"], "notes": "short guidance for the deep review"}` +
		"\n```\n\n" +
		"- `review_files` lists the changed files that deserve close review; omit purely " +
		"mechanical or trivial files. Use an empty array to mean \"review everything\".\n" +
		"- `notes` is optional one-paragraph guidance (risk areas, intent). Keep it brief."

	instructions, err := loadPrompt(passTriage.promptFile)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(instructions)
	b.WriteString("\n\n")
	b.WriteString(contract)
	b.WriteString("\n\n")
	b.WriteString(repoGuidanceSection(req))
	b.WriteString(contextSection(req))
	b.WriteString("\n## Diff\n\n```diff\n")
	b.WriteString(filteredDiff)
	b.WriteString("\n```\n")
	return b.String(), nil
}

// parseTriage extracts and decodes the triage JSON object. A missing or
// malformed object is an error so the caller can retry.
func parseTriage(text string) (triageResult, error) {
	js := extractJSONObject(text, "review_files")
	if js == "" {
		return triageResult{}, fmt.Errorf("no JSON object with a \"review_files\" key found")
	}
	var res triageResult
	if err := json.Unmarshal([]byte(js), &res); err != nil {
		return triageResult{}, fmt.Errorf("decoding triage JSON: %w", err)
	}
	return res, nil
}

// scopeDiffToFiles keeps only the diff blocks whose b-side path is in files.
// When files is empty the diff is returned unchanged. If scoping would drop
// every block (e.g. the model named files not present in the diff), the full
// diff is returned so the deep passes still have content to review.
func scopeDiffToFiles(unifiedDiff string, files []string) string {
	if len(files) == 0 || strings.TrimSpace(unifiedDiff) == "" {
		return unifiedDiff
	}
	keep := make(map[string]bool, len(files))
	for _, f := range files {
		keep[strings.TrimSpace(f)] = true
	}

	const marker = "diff --git "
	const sep = "\ndiff --git "

	remaining := unifiedDiff
	var preamble string
	if !strings.HasPrefix(remaining, marker) {
		idx := strings.Index(remaining, sep)
		if idx == -1 {
			return unifiedDiff // no block headers
		}
		preamble = remaining[:idx+1]
		remaining = remaining[idx+1:]
	}

	var kept strings.Builder
	for len(remaining) > 0 {
		nextIdx := strings.Index(remaining[len(marker):], sep)
		var block string
		if nextIdx == -1 {
			block = remaining
			remaining = ""
		} else {
			end := len(marker) + nextIdx + 1
			block = remaining[:end]
			remaining = remaining[end:]
		}
		headerLine := block
		if nl := strings.IndexByte(block, '\n'); nl != -1 {
			headerLine = block[:nl]
		}
		if path := diff.ParseGitPath(headerLine); path != "" && keep[path] {
			kept.WriteString(block)
		}
	}

	if strings.TrimSpace(kept.String()) == "" {
		return unifiedDiff
	}
	return preamble + kept.String()
}
