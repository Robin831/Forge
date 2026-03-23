package wicket

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
)

// triagePromptTemplate is the AI triage prompt (Appendix A of the Wicket plan).
// It instructs the AI to analyze a GitHub issue and return a structured JSON decision.
const triagePromptTemplate = `You are an AI triage assistant for a software development team. Your task is to analyze a GitHub issue and decide the best course of action.

## Repository Context
{{CONTEXT}}

## Issue to Triage

**Repository**: {{REPO}}
**Issue #{{NUMBER}}**: {{TITLE}}
**Author**: {{AUTHOR}}
**Created**: {{CREATED}}

**Description:**
{{BODY}}

{{COMMENTS_SECTION}}

## Your Task

Analyze this issue and respond with a JSON object (no markdown, no fences, just raw JSON) with one of these decisions:

### Option 1 — Create a Bead (actionable, clear task)
Use when the issue describes a concrete bug or feature that an AI agent can implement autonomously without ambiguity.
` + "```" + `json
{
  "action": "create_bead",
  "title": "Short descriptive title for the bead",
  "description": "Detailed description of what needs to be done, including context from the issue",
  "type": "bug|feature|task",
  "priority": 2,
  "reasoning": "Why this is a clear, actionable task"
}
` + "```" + `
Priority: 1=high, 2=medium (default), 3=low, 4=backlog. Use 1 for production bugs, 2 for most features, 3-4 for nice-to-haves.

### Option 2 — Ask for Clarification
Use when the issue is potentially actionable but lacks critical information (reproduction steps, expected behavior, scope, etc.).
` + "```" + `json
{
  "action": "ask_clarify",
  "question": "The specific question(s) needed to proceed",
  "reasoning": "What information is missing and why it's needed"
}
` + "```" + `

### Option 3 — Flag for Human
Use when the issue requires strategic decisions, architectural input, involves external systems, is a discussion/question (not a bug/feature), is spam or off-topic, or is clearly out of scope.
` + "```" + `json
{
  "action": "flag_human",
  "reasoning": "Why this needs human judgment"
}
` + "```" + `

## Decision Guidelines

**Create a bead** when:
- The issue clearly describes what needs to be built or fixed
- The scope is well-defined and bounded
- An AI agent with access to the codebase could implement it independently

**Ask for clarification** when:
- The expected behavior is unclear
- Reproduction steps are missing for a bug
- The scope could be interpreted multiple ways and the choice matters

**Flag for human** when:
- The issue is a general question or discussion
- It requires product/business decisions
- It involves breaking changes or major architectural decisions
- It references external systems you don't control
- It's clearly out of scope or spam

Respond with ONLY the JSON object. No explanation outside the JSON.`

// buildTriagePrompt constructs the full triage prompt for an issue.
func buildTriagePrompt(repo string, issue *Issue, contextText string) string {
	prompt := triagePromptTemplate
	prompt = strings.ReplaceAll(prompt, "{{REPO}}", repo)
	prompt = strings.ReplaceAll(prompt, "{{NUMBER}}", fmt.Sprintf("%d", issue.Number))
	prompt = strings.ReplaceAll(prompt, "{{TITLE}}", issue.Title)
	prompt = strings.ReplaceAll(prompt, "{{AUTHOR}}", issue.Author.Login)
	prompt = strings.ReplaceAll(prompt, "{{CREATED}}", issue.CreatedAt.Format(time.RFC3339))
	prompt = strings.ReplaceAll(prompt, "{{BODY}}", issue.Body)
	prompt = strings.ReplaceAll(prompt, "{{CONTEXT}}", contextText)

	// Build comments section
	if len(issue.Comments) > 0 {
		var sb strings.Builder
		sb.WriteString("**Comments:**\n")
		for _, c := range issue.Comments {
			fmt.Fprintf(&sb, "\n**%s** (%s):\n%s\n",
				c.Author.Login, c.CreatedAt.Format("2006-01-02"), c.Body)
		}
		prompt = strings.ReplaceAll(prompt, "{{COMMENTS_SECTION}}", sb.String())
	} else {
		prompt = strings.ReplaceAll(prompt, "{{COMMENTS_SECTION}}", "")
	}

	return prompt
}

// Triager runs AI triage on GitHub issues.
type Triager struct {
	provider provider.Provider
}

// newTriager creates a Triager using the given provider.
func newTriager(pv provider.Provider) *Triager {
	return &Triager{provider: pv}
}

// Triage runs AI triage on a GitHub issue.
// It retries once on JSON parse failure; defaults to ActionFlagHuman on all failures.
func (t *Triager) Triage(ctx context.Context, repo string, issue *Issue, contextText string) (*TriageDecision, error) {
	prompt := buildTriagePrompt(repo, issue, contextText)

	decision, err := t.runTriage(ctx, prompt)
	if err != nil {
		// Retry once with a stricter prompt on parse failure.
		log.Printf("[wicket] triage parse failure for %s#%d, retrying: %v", repo, issue.Number, err)
		strictPrompt := prompt + "\n\nIMPORTANT: Your previous response could not be parsed as JSON. Respond with ONLY a valid JSON object, nothing else. No markdown, no backticks, no explanation."
		decision, err = t.runTriage(ctx, strictPrompt)
		if err != nil {
			log.Printf("[wicket] triage retry also failed for %s#%d, defaulting to flag_human: %v", repo, issue.Number, err)
			return &TriageDecision{
				Action:    ActionFlagHuman,
				Reasoning: fmt.Sprintf("AI triage failed after retry: %v", err),
			}, nil
		}
	}
	return decision, nil
}

// runTriage spawns an AI session with the triage prompt and parses the JSON response.
func (t *Triager) runTriage(ctx context.Context, prompt string) (*TriageDecision, error) {
	workDir, err := os.MkdirTemp("", "forge-wicket-triage-*")
	if err != nil {
		return nil, fmt.Errorf("creating triage workdir: %w", err)
	}
	defer os.RemoveAll(workDir)

	logDir := workDir // logs go in the temp dir
	extraFlags := []string{"--max-turns", "3"}

	process, err := smith.SpawnWithProvider(ctx, workDir, prompt, logDir, t.provider, extraFlags)
	if err != nil {
		return nil, fmt.Errorf("spawning triage AI session: %w", err)
	}

	result := process.Wait()
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("triage AI session exited with code %d", result.ExitCode)
	}

	output := result.FullOutput
	if output == "" {
		output = result.Output
	}

	return parseTriageDecision(output)
}

// parseTriageDecision extracts a TriageDecision from the AI output.
// It scans for a JSON object in the output, tolerating surrounding text.
func parseTriageDecision(output string) (*TriageDecision, error) {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil, fmt.Errorf("empty AI output")
	}

	// Try direct parse first.
	var decision TriageDecision
	if err := json.Unmarshal([]byte(output), &decision); err == nil {
		return validateTriageDecision(&decision)
	}

	// Scan for JSON object within the output (handles markdown fences, prose, etc.)
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start >= 0 && end > start {
		candidate := output[start : end+1]
		if err := json.Unmarshal([]byte(candidate), &decision); err == nil {
			return validateTriageDecision(&decision)
		}
	}

	return nil, fmt.Errorf("could not extract JSON from AI output (len=%d)", len(output))
}

// validateTriageDecision ensures the decision has required fields.
func validateTriageDecision(d *TriageDecision) (*TriageDecision, error) {
	switch d.Action {
	case ActionCreateBead, ActionAskClarify, ActionFlagHuman:
		// valid
	case "":
		return nil, fmt.Errorf("missing action field in triage decision")
	default:
		return nil, fmt.Errorf("unknown triage action %q", d.Action)
	}

	if d.Action == ActionCreateBead && d.Title == "" {
		return nil, fmt.Errorf("create_bead decision missing title")
	}

	// Apply priority bounds.
	if d.Priority < 0 || d.Priority > 4 {
		d.Priority = 2 // default to medium
	}

	return d, nil
}
