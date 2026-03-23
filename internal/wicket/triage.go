package wicket

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
)

// TriageConfig holds configuration for a RunTriage call.
type TriageConfig struct {
	// Providers is the ordered list of AI providers to try.
	// Defaults to provider.Defaults() when empty.
	Providers []provider.Provider
	// ExtraPrompt is appended to the default triage prompt to provide
	// project-specific context or constraints.
	ExtraPrompt string
	// runner is the AI call function. Nil means use the default smith-based
	// runner. Tests replace this to avoid spawning real subprocesses.
	runner func(ctx context.Context, prompt string) (string, error)
}

// buildTriagePrompt formats an issue into a prompt string for the triage AI.
func buildTriagePrompt(issue Issue, extraPrompt string) string {
	var b strings.Builder

	b.WriteString("You are a triage agent for an automated software development system.\n")
	b.WriteString("Analyze the following GitHub issue and decide what action to take.\n\n")
	b.WriteString("Available actions:\n")
	b.WriteString(`- "create_bead": The issue is clear, actionable, and suitable for automated implementation.` + "\n")
	b.WriteString(`- "ask_clarify": The issue needs more information before it can be worked on.` + "\n")
	b.WriteString(`- "flag_human": The issue is too complex, requires human judgment, or is not suitable for automation.` + "\n\n")

	b.WriteString("<issue>\n")
	fmt.Fprintf(&b, "<repository>%s</repository>\n", issue.Repo)
	fmt.Fprintf(&b, "<number>%d</number>\n", issue.Number)
	fmt.Fprintf(&b, "<title>%s</title>\n", issue.Title)
	fmt.Fprintf(&b, "<author>%s</author>\n", issue.Author)
	if len(issue.Labels) > 0 {
		fmt.Fprintf(&b, "<labels>%s</labels>\n", strings.Join(issue.Labels, ", "))
	}
	b.WriteString("<description>\n")
	if issue.Body != "" {
		b.WriteString(issue.Body)
	} else {
		b.WriteString("(no description provided)")
	}
	b.WriteString("\n</description>\n")
	b.WriteString("</issue>\n")

	if extraPrompt != "" {
		b.WriteString("\nAdditional context:\n")
		b.WriteString(extraPrompt)
		b.WriteString("\n")
	}

	b.WriteString(`
Respond with ONLY a JSON object using this exact format:
{
  "action": "create_bead",
  "reason": "brief explanation of your decision",
  "bead_title": "short title for the work item (only when action is create_bead)",
  "bead_description": "clear description of what needs to be done (only when action is create_bead)"
}

Choose "create_bead" only when the issue describes a clear, specific, implementable task.
Omit "bead_title" and "bead_description" for "ask_clarify" and "flag_human" actions.
`)
	return b.String()
}

// rawTriageDecision is the JSON shape expected from the AI.
type rawTriageDecision struct {
	Action          string `json:"action"`
	Reason          string `json:"reason"`
	BeadTitle       string `json:"bead_title"`
	BeadDescription string `json:"bead_description"`
}

// parseTriageDecision extracts a TriageDecision from raw AI output.
// Returns (decision, true) on success, (zero, false) if the output cannot be
// parsed or contains an unrecognised action.
func parseTriageDecision(output string) (TriageDecision, bool) {
	jsonStr := extractTriageJSON(output)
	if jsonStr == "" {
		return TriageDecision{}, false
	}

	var raw rawTriageDecision
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return TriageDecision{}, false
	}

	action := TriageAction(raw.Action)
	switch action {
	case ActionCreateBead, ActionAskClarify, ActionFlagHuman:
		// valid
	default:
		return TriageDecision{}, false
	}

	// create_bead requires non-empty title and description so downstream bead
	// creation doesn't produce an invalid/empty work item.
	if action == ActionCreateBead && (raw.BeadTitle == "" || raw.BeadDescription == "") {
		return TriageDecision{}, false
	}

	return TriageDecision{
		Action:          action,
		Reason:          raw.Reason,
		BeadTitle:       raw.BeadTitle,
		BeadDescription: raw.BeadDescription,
	}, true
}

// extractTriageJSON returns the first JSON object in text that contains the
// key "action". It checks fenced code blocks first, then scans for raw JSON.
func extractTriageJSON(text string) string {
	const requiredKey = `"action"`

	// 1. Look for ```json ... ``` blocks.
	if s := extractFencedTriageBlock(text, "```json"); s != "" && strings.Contains(s, requiredKey) {
		return s
	}
	// 2. Look for plain ``` ... ``` blocks.
	if s := extractFencedTriageBlock(text, "```"); s != "" && strings.Contains(s, requiredKey) {
		return s
	}
	// 3. Scan for raw JSON objects containing the required key.
Outer:
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		depth := 0
		inString := false
		escaped := false
		for j := i; j < len(text); j++ {
			char := text[j]
			if inString {
				if escaped {
					escaped = false
				} else if char == '\\' {
					escaped = true
				} else if char == '"' {
					inString = false
				}
				continue
			}
			switch char {
			case '"':
				inString = true
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					candidate := text[i : j+1]
					if strings.Contains(candidate, requiredKey) {
						return candidate
					}
					continue Outer
				}
			}
		}
	}
	return ""
}

// extractFencedTriageBlock returns the content between the first occurrence of
// fence and the next closing ```. Returns "" if not found.
func extractFencedTriageBlock(text, fence string) string {
	start := strings.Index(text, fence)
	if start == -1 {
		return ""
	}
	content := text[start+len(fence):]
	if len(content) > 0 && content[0] == '\n' {
		content = content[1:]
	}
	end := strings.Index(content, "```")
	if end == -1 {
		return ""
	}
	return strings.TrimSpace(content[:end])
}

// RunTriage calls the AI provider with a triage prompt for the given issue and
// returns a TriageDecision. It retries once if the first response cannot be
// parsed (parse failure only), and defaults to ActionFlagHuman on persistent
// failure. Runner errors (provider failures) cause an immediate fallback
// without retrying to avoid doubling cost on provider outages.
func RunTriage(ctx context.Context, issue Issue, cfg TriageConfig) TriageDecision {
	run := cfg.runner
	if run == nil {
		run = buildDefaultTriageRunner(cfg.Providers)
	}

	prompt := buildTriagePrompt(issue, cfg.ExtraPrompt)

	for attempt := 0; attempt < 2; attempt++ {
		output, err := run(ctx, prompt)
		if err != nil {
			log.Printf("[wicket:triage] %s#%d runner error: %v", issue.Repo, issue.Number, err)
			// Runner errors are not retried — fall through to flag_human.
			break
		}
		dec, ok := parseTriageDecision(output)
		if ok {
			return dec
		}
		log.Printf("[wicket:triage] %s#%d attempt %d parse failed (output: %.200s)", issue.Repo, issue.Number, attempt+1, output)
	}

	log.Printf("[wicket:triage] %s#%d defaulting to flag_human", issue.Repo, issue.Number)
	return TriageDecision{
		Action: ActionFlagHuman,
		Reason: "triage AI returned unparseable response",
	}
}

// buildDefaultTriageRunner returns a runner backed by smith.SpawnWithProvider
// that tries each provider in order, falling back on rate limits.
func buildDefaultTriageRunner(providers []provider.Provider) func(ctx context.Context, prompt string) (string, error) {
	pvs := providers
	if len(pvs) == 0 {
		pvs = provider.Defaults()
	}
	return func(ctx context.Context, prompt string) (string, error) {
		tempDir, err := os.MkdirTemp("", "forge-triage-")
		if err != nil {
			return "", fmt.Errorf("create triage temp dir: %w", err)
		}
		defer os.RemoveAll(tempDir)

		var lastErr error
		for _, pv := range pvs {
			proc, err := smith.SpawnWithProvider(ctx, tempDir, prompt, tempDir, pv, []string{"--max-turns", "1"})
			if err != nil {
				lastErr = fmt.Errorf("spawn %s: %w", pv.Label(), err)
				continue
			}
			result := proc.Wait()
			if result.RateLimited {
				lastErr = fmt.Errorf("provider %s rate limited", pv.Label())
				continue
			}
			if result.IsError || result.ExitCode != 0 {
				lastErr = fmt.Errorf("provider %s failed (exit %d, subtype %q): %s", pv.Label(), result.ExitCode, result.ResultSubtype, result.ErrorOutput)
				continue
			}
			if result.FullOutput != "" {
				return result.FullOutput, nil
			}
			return result.Output, nil
		}
		if lastErr != nil {
			return "", lastErr
		}
		return "", fmt.Errorf("all providers failed")
	}
}
