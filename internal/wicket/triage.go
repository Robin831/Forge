package wicket

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
)

// BeadSummary is a compact representation of a bead used to provide context
// to the triage AI for duplicate and already-fixed detection.
type BeadSummary struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

// bdListRunner is the function used to execute `bd list`. Tests replace this
// to avoid spawning a real subprocess. The anvilPath parameter sets cmd.Dir
// so the correct .beads/ config is used for the target repository.
var bdListRunner func(ctx context.Context, args []string, anvilPath string) (string, error) = defaultBDListRunner

func defaultBDListRunner(ctx context.Context, args []string, anvilPath string) (string, error) {
	cmdArgs := append([]string{"list"}, args...)
	cmd := executil.HideWindow(exec.CommandContext(ctx, "bd", cmdArgs...))
	if anvilPath != "" {
		cmd.Dir = anvilPath
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		se := strings.TrimSpace(stderr.String())
		if se != "" {
			return "", fmt.Errorf("bd list: %v: %s", err, se)
		}
		return "", fmt.Errorf("bd list: %w", err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// fetchBeadSummaries calls bd list with the given status filter and returns
// parsed bead summaries. A limit of 0 means no limit. anvilPath sets the
// working directory for the bd command so the correct .beads/ config is used.
// A 30-second timeout is applied to prevent a hung beads DB from stalling the
// scan loop indefinitely.
func fetchBeadSummaries(ctx context.Context, status string, limit int, anvilPath string) []BeadSummary {
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	args := []string{"--status", status, "--json"}
	if limit > 0 {
		args = append(args, "--limit", strconv.Itoa(limit))
	} else {
		args = append(args, "--limit", "0")
	}
	output, err := bdListRunner(tctx, args, anvilPath)
	if err != nil {
		log.Printf("[wicket:triage] bd list --status=%s: %v", status, err)
		return nil
	}
	if output == "" {
		return nil
	}
	var summaries []BeadSummary
	if err := json.Unmarshal([]byte(output), &summaries); err != nil {
		log.Printf("[wicket:triage] parse bd list output: %v", err)
		return nil
	}
	return summaries
}

// sanitizeBeadText strips characters that could break the prompt structure when
// bead content is interpolated into XML-tagged sections. Newlines are collapsed
// to spaces (each bead occupies a single line) and angle brackets are removed
// to prevent XML tag injection.
func sanitizeBeadText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "<", "")
	s = strings.ReplaceAll(s, ">", "")
	return s
}

// formatBeadSummaries formats a slice of BeadSummary into a compact multi-line
// string suitable for injection into a prompt.
func formatBeadSummaries(beads []BeadSummary) string {
	if len(beads) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, bead := range beads {
		title := sanitizeBeadText(bead.Title)
		desc := sanitizeBeadText(bead.Description)
		// Truncate by rune count to avoid splitting multibyte UTF-8 sequences.
		descRunes := []rune(desc)
		if len(descRunes) > 120 {
			desc = string(descRunes[:120]) + "…"
		}
		fmt.Fprintf(&b, "- %s: %s", bead.ID, title)
		if desc != "" {
			fmt.Fprintf(&b, " — %s", desc)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// TriageConfig holds configuration for a RunTriage call.
type TriageConfig struct {
	// Providers is the ordered list of AI providers to try.
	// Defaults to provider.Defaults() when empty.
	Providers []provider.Provider
	// ExtraPrompt is appended to the default triage prompt to provide
	// project-specific context or constraints.
	ExtraPrompt string
	// AnvilPath is the local filesystem path of the anvil repository. It is
	// passed as cmd.Dir to `bd list` so the correct .beads/ config is used.
	// When empty, the daemon's working directory is used (may query the wrong DB).
	AnvilPath string
	// AllAnvilPaths is the list of ALL configured anvil paths (including the
	// current anvil). When non-empty, RunTriage performs a cross-anvil Source
	// URL lookup before calling the AI so that issues already tracked in a
	// different anvil are detected as duplicates immediately.
	AllAnvilPaths []string
	// MonitoredAnvilPaths is the list of local filesystem paths for all anvils
	// that correspond to repos monitored by the current anvil (via the 5a
	// wicket_repos mapping). When non-empty, open and closed beads are
	// aggregated from all these paths and included in the triage prompt,
	// giving the AI context across all related repositories. When empty, only
	// AnvilPath is used for bead context.
	MonitoredAnvilPaths []string
	// runner is the AI call function. Nil means use the default smith-based
	// runner. Tests replace this to avoid spawning real subprocesses.
	runner func(ctx context.Context, prompt string) (string, error)
	// beadLister overrides bead fetching for tests. When non-nil it is called
	// instead of fetchBeadSummaries to retrieve open and closed beads.
	// The first call receives status "open,in_progress" and limit 50;
	// the second receives status "closed" and limit 20.
	beadLister func(ctx context.Context, status string, limit int) []BeadSummary
	// crossAnvilLister overrides cross-anvil bead fetching for tests. When
	// non-nil it is called instead of fetchBeadSummaries for each anvil path
	// in AllAnvilPaths. Each call receives the anvil path and returns open and
	// in-progress beads (status "open,in_progress").
	crossAnvilLister func(ctx context.Context, anvilPath string) []BeadSummary
	// monitoredAnvilLister overrides multi-path bead fetching for tests when
	// MonitoredAnvilPaths is non-empty. Called once per path in
	// MonitoredAnvilPaths; status is "open,in_progress" or "closed". If nil,
	// fetchBeadSummaries is used.
	monitoredAnvilLister func(ctx context.Context, anvilPath string, status string) []BeadSummary
}

// buildTriagePrompt formats an issue into a prompt string for the triage AI.
func buildTriagePrompt(issue Issue, extraPrompt string) string {
	return buildTriagePromptWithBeads(issue, nil, extraPrompt, nil, nil)
}

// buildTriagePromptWithBeads builds a triage prompt including optional comment
// history and bead context for duplicate/already-fixed detection.
func buildTriagePromptWithBeads(issue Issue, comments []Comment, extraPrompt string, openBeads, closedBeads []BeadSummary) string {
	var b strings.Builder

	b.WriteString("You are a triage agent for an automated software development system.\n")
	if len(comments) > 0 {
		b.WriteString("Analyze the following GitHub issue and its conversation history, then decide what action to take.\n")
		b.WriteString("The issue author has provided additional information — re-evaluate the original request in light of the new context.\n\n")
	} else {
		b.WriteString("Analyze the following GitHub issue and decide what action to take.\n\n")
	}
	b.WriteString("Available actions:\n")
	b.WriteString(`- "create_bead": The issue is clear, actionable, and suitable for automated implementation.` + "\n")
	b.WriteString(`- "ask_clarify": The issue needs more information before it can be worked on.` + "\n")
	b.WriteString(`- "flag_human": The issue is too complex, requires human judgment, or is not suitable for automation.` + "\n")
	b.WriteString(`- "duplicate": The issue is already tracked by an existing open bead. Include "duplicate_id" with the matching bead ID.` + "\n")
	b.WriteString(`- "already_fixed": The issue describes a problem that was already resolved. Include "reference_pr" with the relevant PR URL or bead ID.` + "\n")
	b.WriteString(`- "out_of_scope": The issue is outside the scope of this project. Include your reasoning in "reason".` + "\n\n")

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

	if len(comments) > 0 {
		b.WriteString("<conversation>\n")
		for _, c := range comments {
			fmt.Fprintf(&b, "<comment author=%q>\n%s\n</comment>\n", c.Author, c.Body)
		}
		b.WriteString("</conversation>\n")
	}

	b.WriteString("</issue>\n")

	// Inject existing bead context to help the AI detect duplicates and
	// already-resolved issues.
	b.WriteString("\n<existing_work>\n")
	b.WriteString("Existing open issues (check for duplicates):\n")
	b.WriteString(formatBeadSummaries(openBeads))
	b.WriteString("\n\nRecently closed issues (check for already-fixed):\n")
	b.WriteString(formatBeadSummaries(closedBeads))
	b.WriteString("\n</existing_work>\n")

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
  "bead_description": "clear description of what needs to be done (only when action is create_bead)",
  "duplicate_id": "Forge-abc1 (only when action is duplicate)",
  "reference_pr": "PR URL or bead ID (only when action is already_fixed)"
}

Choose "create_bead" only when the issue describes a clear, specific, implementable task.
Omit "bead_title" and "bead_description" for non-create_bead actions.
Omit "duplicate_id" unless action is "duplicate".
Omit "reference_pr" unless action is "already_fixed".
`)
	return b.String()
}

// rawTriageDecision is the JSON shape expected from the AI.
type rawTriageDecision struct {
	Action          string `json:"action"`
	Reason          string `json:"reason"`
	BeadTitle       string `json:"bead_title"`
	BeadDescription string `json:"bead_description"`
	DuplicateID     string `json:"duplicate_id"`
	ReferencePR     string `json:"reference_pr"`
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
	case ActionCreateBead, ActionAskClarify, ActionFlagHuman,
		ActionDuplicate, ActionAlreadyFixed, ActionOutOfScope:
		// valid
	default:
		return TriageDecision{}, false
	}

	// create_bead requires non-empty title and description so downstream bead
	// creation doesn't produce an invalid/empty work item.
	if action == ActionCreateBead && (raw.BeadTitle == "" || raw.BeadDescription == "") {
		return TriageDecision{}, false
	}
	// duplicate requires a bead ID to reference; without it the comment would
	// be meaningless and the outcome ambiguous.
	if action == ActionDuplicate && raw.DuplicateID == "" {
		return TriageDecision{}, false
	}
	// already_fixed requires a PR/bead reference; without it the comment
	// cannot point the user to the fix.
	if action == ActionAlreadyFixed && raw.ReferencePR == "" {
		return TriageDecision{}, false
	}
	// out_of_scope requires a non-empty reason so the posted comment explains
	// why the issue was declined.
	if action == ActionOutOfScope && raw.Reason == "" {
		return TriageDecision{}, false
	}

	return TriageDecision{
		Action:          action,
		Reason:          raw.Reason,
		BeadTitle:       raw.BeadTitle,
		BeadDescription: raw.BeadDescription,
		DuplicateID:     raw.DuplicateID,
		ReferencePR:     raw.ReferencePR,
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

// extractSourceURL returns the GitHub issue URL embedded in a bead description
// by buildBDArgs (format: "Source: <url>"). Returns "" if not present.
func extractSourceURL(desc string) string {
	for _, line := range strings.Split(desc, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Source: ") {
			return strings.TrimPrefix(line, "Source: ")
		}
	}
	return ""
}

// findDuplicateBySourceURL searches all configured anvil paths for a bead
// whose description contains a Source URL matching the incoming GitHub issue.
// Returns (beadID, true) on the first match, ("", false) if none found.
// The lister callback is called once per anvil path and should return all
// open/in-progress beads for that path.
func findDuplicateBySourceURL(ctx context.Context, issue Issue, allAnvilPaths []string, lister func(ctx context.Context, anvilPath string) []BeadSummary) (string, bool) {
	target := issueURL(issue.Repo, issue.Number)
	for _, path := range allAnvilPaths {
		beads := lister(ctx, path)
		for _, b := range beads {
			if extractSourceURL(b.Description) == target {
				return b.ID, true
			}
		}
	}
	return "", false
}

// RunTriage calls the AI provider with a triage prompt for the given issue and
// returns a TriageDecision. It retries once if the first response cannot be
// parsed (parse failure only), and defaults to ActionFlagHuman on persistent
// failure. Runner errors (provider failures) cause an immediate fallback
// without retrying to avoid doubling cost on provider outages.
func RunTriage(ctx context.Context, issue Issue, cfg TriageConfig) TriageDecision {
	return RunTriageWithComments(ctx, issue, nil, cfg)
}

// RunTriageWithComments is like RunTriage but includes the issue's comment
// history in the prompt. Used for clarification re-triage when the author
// has replied with additional details.
func RunTriageWithComments(ctx context.Context, issue Issue, comments []Comment, cfg TriageConfig) TriageDecision {
	run := cfg.runner
	if run == nil {
		run = buildDefaultTriageRunner(cfg.Providers)
	}

	// Cross-anvil duplicate check: before invoking the AI, search all
	// configured anvil bead databases for a bead whose description contains
	// a Source URL matching this issue. A match means the issue was already
	// triaged, so we return duplicate immediately.
	if len(cfg.AllAnvilPaths) > 0 {
		xLister := cfg.crossAnvilLister
		if xLister == nil {
			// Cap the number of open/in_progress beads scanned per anvil to avoid
			// unbounded bd list calls as databases grow.
			const crossAnvilMaxOpenBeads = 200
			xLister = func(ctx context.Context, anvilPath string) []BeadSummary {
				return fetchBeadSummaries(ctx, "open,in_progress", crossAnvilMaxOpenBeads, anvilPath)
			}
		}
		if beadID, ok := findDuplicateBySourceURL(ctx, issue, cfg.AllAnvilPaths, xLister); ok {
			log.Printf("[wicket:triage] %s#%d cross-anvil duplicate found: %s", issue.Repo, issue.Number, beadID)
			return TriageDecision{
				Action:      ActionDuplicate,
				Reason:      fmt.Sprintf("issue already tracked as existing bead %s", beadID),
				DuplicateID: beadID,
			}
		}
	}

	// Fetch existing bead context for duplicate/already-fixed detection.
	// Open beads are capped at 50 to keep prompt size manageable; closed
	// beads are limited to the 20 most recent for already-fixed detection.
	const maxOpenBeads = 50
	var openBeads, closedBeads []BeadSummary

	if len(cfg.MonitoredAnvilPaths) > 0 {
		// Multi-repo: aggregate beads from all monitored anvil paths and
		// deduplicate by ID so the AI sees the full cross-repo work context.
		mlister := cfg.monitoredAnvilLister
		if mlister == nil {
			mlister = func(ctx context.Context, anvilPath string, status string) []BeadSummary {
				limit := maxOpenBeads
				if status == "closed" {
					limit = 20
				}
				return fetchBeadSummaries(ctx, status, limit, anvilPath)
			}
		}
		seenOpen := make(map[string]bool)
		seenClosed := make(map[string]bool)
		for _, path := range cfg.MonitoredAnvilPaths {
			for _, b := range mlister(ctx, path, "open,in_progress") {
				if !seenOpen[b.ID] {
					seenOpen[b.ID] = true
					openBeads = append(openBeads, b)
				}
			}
			for _, b := range mlister(ctx, path, "closed") {
				if !seenClosed[b.ID] {
					seenClosed[b.ID] = true
					closedBeads = append(closedBeads, b)
				}
			}
		}
		// Cap to keep prompt size manageable.
		if len(openBeads) > maxOpenBeads {
			openBeads = openBeads[:maxOpenBeads]
		}
		if len(closedBeads) > 20 {
			closedBeads = closedBeads[:20]
		}
	} else {
		lister := cfg.beadLister
		if lister == nil {
			anvilPath := cfg.AnvilPath
			lister = func(ctx context.Context, status string, limit int) []BeadSummary {
				return fetchBeadSummaries(ctx, status, limit, anvilPath)
			}
		}
		openBeads = lister(ctx, "open,in_progress", maxOpenBeads)
		closedBeads = lister(ctx, "closed", 20)
	}

	prompt := buildTriagePromptWithBeads(issue, comments, cfg.ExtraPrompt, openBeads, closedBeads)

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
