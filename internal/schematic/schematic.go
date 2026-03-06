// Package schematic implements the pre-worker that analyses bead scope before
// Smith starts. The schematic is drawn before the smith starts forging.
//
// Schematic can:
//  1. Emit a focused implementation plan appended to Smith's prompt
//  2. Decompose large beads into sub-beads via bd, blocking the parent
//  3. Skip entirely for small/simple beads
package schematic

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/smith"
)

// Action describes what the Schematic decided to do.
type Action string

const (
	// ActionPlan means the bead is implementable as-is; a focused plan was
	// produced to guide Smith.
	ActionPlan Action = "plan"

	// ActionDecompose means the bead was too large or multi-part and has been
	// split into sub-beads. The parent bead should be blocked.
	ActionDecompose Action = "decompose"

	// ActionSkip means the bead was simple enough that no schematic was needed.
	ActionSkip Action = "skip"

	// ActionClarify means the bead requires human clarification and should not
	// be worked on yet.
	ActionClarify Action = "clarify"
)

// Result captures the outcome of a Schematic analysis.
type Result struct {
	// Action is what the Schematic decided.
	Action Action
	// Plan is a focused implementation plan for Smith (only when Action=ActionPlan).
	Plan string
	// SubBeads is the list of sub-bead IDs created (only when Action=ActionDecompose).
	SubBeads []string
	// Reason is a human-readable explanation of the decision.
	Reason string
	// Duration is how long the analysis took.
	Duration time.Duration
	// CostUSD is the estimated cost of the AI session.
	CostUSD float64
	// Error is set if the schematic failed.
	Error error
}

// Config controls Schematic behavior.
type Config struct {
	// Enabled controls whether Schematic runs at all. Defaults to true when
	// the global setting is enabled.
	Enabled bool
	// WordThreshold is the minimum word count in the bead description to
	// trigger automatic schematic analysis. Beads below this are skipped
	// unless they have the decompose tag. Default: 100.
	WordThreshold int
	// MaxTurns limits the AI session length. Default: 10.
	MaxTurns int
}

// DefaultConfig returns sensible defaults for Schematic.
func DefaultConfig() Config {
	return Config{
		Enabled:       false,
		WordThreshold: 100,
		MaxTurns:      10,
	}
}

// ShouldRun determines whether the Schematic should analyse this bead based
// on the configuration and bead metadata.
func ShouldRun(cfg Config, bead poller.Bead) bool {
	if !cfg.Enabled {
		return false
	}

	// Explicit tag always triggers
	for _, tag := range bead.Tags {
		if strings.EqualFold(tag, "decompose") {
			return true
		}
	}

	// Word count heuristic on description
	wordCount := len(strings.Fields(bead.Description))
	return wordCount >= cfg.WordThreshold
}

// schematicVerdict is the JSON structure we ask the AI to produce.
type schematicVerdict struct {
	Action   string   `json:"action"`
	Plan     string   `json:"plan,omitempty"`
	SubTasks []string `json:"sub_tasks,omitempty"`
	Reason   string   `json:"reason"`
}

// Run executes the Schematic analysis for a bead. It spawns a lightweight AI
// session to determine whether the bead should be decomposed, planned, or
// skipped.
func Run(ctx context.Context, cfg Config, bead poller.Bead, anvilPath string, pv provider.Provider) *Result {
	start := time.Now()

	if !ShouldRun(cfg, bead) {
		return &Result{
			Action:   ActionSkip,
			Reason:   "Below complexity threshold",
			Duration: time.Since(start),
		}
	}

	promptText := buildPrompt(bead)

	log.Printf("[schematic:%s] Analysing bead scope (provider: %s)", bead.ID, pv.Label())

	// Use the same smith.SpawnWithProvider to run the AI session in a temp dir
	// so the schematic session cannot modify the main repo.
	workDir, err := os.MkdirTemp("", "forge-schematic-*")
	if err != nil {
		return &Result{
			Action:   ActionSkip,
			Reason:   fmt.Sprintf("Failed to create temp dir: %v", err),
			Duration: time.Since(start),
			Error:    fmt.Errorf("creating schematic workdir: %w", err),
		}
	}
	defer os.RemoveAll(workDir)

	logDir := filepath.Join(workDir, "logs")
	extraFlags := []string{"--max-turns", fmt.Sprintf("%d", cfg.MaxTurns)}
	process, err := smith.SpawnWithProvider(ctx, workDir, promptText, logDir, pv, extraFlags)
	if err != nil {
		return &Result{
			Action:   ActionSkip,
			Reason:   fmt.Sprintf("Failed to spawn schematic session: %v", err),
			Duration: time.Since(start),
			Error:    fmt.Errorf("spawning schematic: %w", err),
		}
	}

	smithResult := process.Wait()

	result := &Result{
		Duration: time.Since(start),
		CostUSD:  smithResult.CostUSD,
	}

	if smithResult.RateLimited {
		result.Action = ActionSkip
		result.Reason = "Rate limited — skipping schematic"
		result.Error = fmt.Errorf("schematic rate limited")
		return result
	}

	if smithResult.ExitCode != 0 {
		result.Action = ActionSkip
		result.Reason = fmt.Sprintf("Schematic session failed (exit %d) — skipping", smithResult.ExitCode)
		result.Error = fmt.Errorf("schematic exit code %d", smithResult.ExitCode)
		return result
	}

	// Parse structured verdict from output
	verdict, err := parseVerdict(smithResult.Output)
	if err != nil {
		// On parse failure, skip rather than block the pipeline
		result.Action = ActionSkip
		result.Reason = fmt.Sprintf("Could not parse schematic output — skipping: %v", err)
		result.Error = err
		return result
	}

	switch verdict.Action {
	case "plan":
		result.Action = ActionPlan
		result.Plan = verdict.Plan
		result.Reason = verdict.Reason

	case "decompose":
		result.Action = ActionDecompose
		result.Reason = verdict.Reason
		// Create sub-beads via bd
		subIDs, err := createSubBeads(ctx, bead, verdict.SubTasks, anvilPath)
		if err != nil {
			// Failed to create sub-beads — fall back to plan
			log.Printf("[schematic:%s] Failed to create sub-beads: %v — treating as plan", bead.ID, err)
			result.Action = ActionPlan
			result.Plan = strings.Join(verdict.SubTasks, "\n")
			result.Reason = fmt.Sprintf("Decomposition failed (%v), using tasks as plan", err)
		} else {
			result.SubBeads = subIDs
		}

	case "clarify":
		result.Action = ActionClarify
		result.Reason = verdict.Reason

	default:
		result.Action = ActionSkip
		result.Reason = fmt.Sprintf("Unknown action %q — skipping", verdict.Action)
	}

	return result
}

// parseVerdict extracts the structured JSON verdict from the AI output.
func parseVerdict(output string) (*schematicVerdict, error) {
	// Try to find JSON block in ```json ... ``` fences
	if idx := strings.Index(output, "```json"); idx >= 0 {
		start := idx + len("```json")
		if end := strings.Index(output[start:], "```"); end >= 0 {
			var v schematicVerdict
			if err := json.Unmarshal([]byte(strings.TrimSpace(output[start:start+end])), &v); err == nil {
				return &v, nil
			}
		}
	}

	// Try plain ``` fences containing "action"
	if idx := strings.Index(output, "```"); idx >= 0 {
		start := idx + 3
		// Skip optional language tag on same line
		if nl := strings.Index(output[start:], "\n"); nl >= 0 {
			start += nl + 1
		}
		if end := strings.Index(output[start:], "```"); end >= 0 {
			block := strings.TrimSpace(output[start : start+end])
			if strings.Contains(block, "action") {
				var v schematicVerdict
				if err := json.Unmarshal([]byte(block), &v); err == nil {
					return &v, nil
				}
			}
		}
	}

	// Scan for raw JSON objects with "action"
	for i := 0; i < len(output); i++ {
		if output[i] != '{' {
			continue
		}
		// Find matching closing brace
		depth := 0
		for j := i; j < len(output); j++ {
			switch output[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					block := output[i : j+1]
					if strings.Contains(block, `"action"`) {
						var v schematicVerdict
						if err := json.Unmarshal([]byte(block), &v); err == nil {
							return &v, nil
						}
					}
					i = j // skip past this block
					break
				}
			}
			if depth == 0 {
				break
			}
		}
	}

	return nil, fmt.Errorf("no valid schematic verdict JSON found in output")
}

// createSubBeads creates sub-beads via bd CLI with discovered-from dependency links.
func createSubBeads(ctx context.Context, parent poller.Bead, tasks []string, anvilPath string) ([]string, error) {
	if len(tasks) == 0 {
		return nil, fmt.Errorf("no sub-tasks to create")
	}

	resetParent := func() {
		rCtx, rCancel := context.WithTimeout(ctx, 15*time.Second)
		defer rCancel()
		rCmd := executil.HideWindow(exec.CommandContext(rCtx,
			"bd", "update", parent.ID, "--status=open", "--json",
		))
		rCmd.Dir = anvilPath
		if out, err := rCmd.CombinedOutput(); err != nil {
			log.Printf("[schematic:%s] Warning: failed to reset parent to open: %v: %s", parent.ID, err, out)
		}
	}

	var subIDs []string
	for _, task := range tasks {
		// Create sub-bead with discovered-from dependency in a single call
		createCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		cmd := executil.HideWindow(exec.CommandContext(createCtx,
			"bd", "create",
			"--title="+task,
			"--description=Sub-task decomposed from "+parent.ID+": "+parent.Title,
			"--type=task",
			fmt.Sprintf("--priority=%d", parent.Priority),
			"--deps", "discovered-from:"+parent.ID,
			"--json",
		))
		cmd.Dir = anvilPath
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			log.Printf("[schematic:%s] Partial decomposition failure after creating %d sub-beads: %v", parent.ID, len(subIDs), err)
			resetParent()
			return subIDs, fmt.Errorf("creating sub-bead %q: %w: %s", task, err, out)
		}

		// Extract ID from JSON output
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(out, &created); err != nil {
			// Try to find the ID in the output
			lines := strings.Split(string(out), "\n")
			for _, line := range lines {
				if err2 := json.Unmarshal([]byte(line), &created); err2 == nil && created.ID != "" {
					break
				}
			}
			if created.ID == "" {
				log.Printf("[schematic:%s] Partial decomposition failure after creating %d sub-beads: could not parse ID", parent.ID, len(subIDs))
				resetParent()
				return subIDs, fmt.Errorf("parsing sub-bead ID from output: %w: %s", err, out)
			}
		}

		subIDs = append(subIDs, created.ID)
	}

	// Block the parent bead (set status back to open — it will be blocked by dependencies)
	resetParent()

	return subIDs, nil
}

// buildPrompt creates the analysis prompt for the Schematic AI session.
func buildPrompt(bead poller.Bead) string {
	return fmt.Sprintf(`You are a software architect analysing a work item (bead) to determine the best approach.

## Bead to Analyse

**ID**: %s
**Title**: %s
**Type**: %s
**Priority**: %d

### Description

%s

## Your Task

Analyse this bead and decide ONE of the following actions:

1. **plan** — The bead is implementable as a single unit of work. Produce a focused, step-by-step implementation plan that a coding agent can follow.
2. **decompose** — The bead is too large, has multiple independent parts, or would benefit from being split. List the sub-tasks.
3. **clarify** — The bead is ambiguous or missing critical information and cannot be worked on yet.

## Decision Criteria

- If the description has multiple independent features/changes → decompose
- If the scope is large (>500 lines of change expected) → decompose
- If the bead title and description are clear and focused → plan
- If the bead has contradictory or missing requirements → clarify
- Prefer "plan" over "decompose" when in doubt — avoid unnecessary splits

## Output Format

Output your verdict as a JSON block FIRST, before any explanation:

`+"```json"+`
{
  "action": "plan|decompose|clarify",
  "plan": "Step-by-step implementation plan (only for action=plan)",
  "sub_tasks": ["Task 1 title", "Task 2 title"],
  "reason": "Brief explanation of your decision"
}
`+"```"+`

Keep sub_tasks to 2-5 items. Each should be a clear, self-contained task title.
For "plan", provide concrete steps: which files to modify, what to add, what to test.
`, bead.ID, bead.Title, bead.IssueType, bead.Priority, bead.Description)
}
