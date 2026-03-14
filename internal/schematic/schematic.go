// Package schematic implements the pre-worker that analyses bead scope before
// Smith starts. The schematic is drawn before the smith starts forging.
//
// Schematic can:
//  1. Emit a focused implementation plan appended to Smith's prompt
//  2. Decompose large beads into sub-beads via bd, blocking the parent
//  3. Skip entirely for small/simple beads
//  4. Request human clarification for ambiguous beads and block work until clarified
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

	// ActionCrucible means the bead has children that need to be orchestrated
	// together on a feature branch (crucible mode).
	ActionCrucible Action = "crucible"
)

// SubBead holds the ID and title of a created sub-bead.
type SubBead struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Result captures the outcome of a Schematic analysis.
type Result struct {
	// Action is what the Schematic decided.
	Action Action
	// Plan is a focused implementation plan for Smith (only when Action=ActionPlan).
	Plan string
	// SubBeads is the list of sub-beads created (only when Action=ActionDecompose).
	SubBeads []SubBead
	// Reason is a human-readable explanation of the decision.
	Reason string
	// Duration is how long the analysis took.
	Duration time.Duration
	// CostUSD is the estimated cost of the AI session.
	CostUSD float64
	// Quota holds rate-limit quota data from the AI session, if available.
	Quota *provider.Quota
	// Error is set if the schematic failed.
	Error error
}

// CrucibleCheckResult captures whether a bead with children needs crucible
// orchestration or can be dispatched as a standalone bead.
type CrucibleCheckResult struct {
	// NeedsCrucible is true when the children should be orchestrated on a
	// feature branch rather than dispatched individually.
	NeedsCrucible bool
	// Reason explains the decision.
	Reason string
	// Duration is how long the check took.
	Duration time.Duration
	// CostUSD is the estimated cost of the AI session.
	CostUSD float64
	// Quota holds rate-limit quota data from the AI session, if available.
	Quota *provider.Quota
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
	// ExtraFlags are additional CLI flags forwarded to the Claude session
	// (e.g. model selection, auth tokens). Mirrors pipeline.Params.ExtraFlags.
	ExtraFlags []string
	// OnSpawn is an optional callback invoked immediately after the AI
	// subprocess is started, before waiting for it to finish. It receives the
	// process PID and the path to the session log file. Use this to update
	// monitoring records (e.g. the worker DB row) so that live-tail and
	// progress tracking work during the schematic phase.
	OnSpawn func(pid int, logPath string)
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
	for _, tag := range bead.Labels {
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
	extraFlags := append([]string{"--max-turns", fmt.Sprintf("%d", cfg.MaxTurns)}, cfg.ExtraFlags...)
	process, err := smith.SpawnWithProvider(ctx, workDir, promptText, logDir, pv, extraFlags)
	if err != nil {
		return &Result{
			Action:   ActionSkip,
			Reason:   fmt.Sprintf("Failed to spawn schematic session: %v", err),
			Duration: time.Since(start),
			Error:    fmt.Errorf("spawning schematic: %w", err),
		}
	}
	if cfg.OnSpawn != nil {
		cfg.OnSpawn(process.PID, process.LogPath)
	}

	smithResult := process.Wait()

	result := &Result{
		Duration: time.Since(start),
		CostUSD:  smithResult.CostUSD,
		Quota:    smithResult.Quota,
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

	// Parse structured verdict from output — prefer FullOutput (natural-language
	// response) over Output (raw stream-JSON protocol lines).
	output := smithResult.FullOutput
	if output == "" {
		output = smithResult.Output
	}
	verdict, err := parseVerdict(output)
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
		subs, err := createSubBeads(ctx, bead, verdict.SubTasks, anvilPath, defaultRunCmd)
		if err != nil {
			// Failed to create sub-beads — escalate to ActionClarify (not ActionSkip) so the
			// pipeline releases the bead for human attention rather than silently continuing.
			// Partial sub-beads are preserved so operators can identify and clean up orphans.
			log.Printf("[schematic:%s] Failed to create sub-beads: %v (partial: %v)", bead.ID, err, subs)
			result.Action = ActionClarify
			result.SubBeads = subs // preserve partial sub-beads for caller visibility
			result.Reason = fmt.Sprintf("Automatic decomposition failed, bead requires manual review: %v", err)
			result.Error = err
		} else {
			result.SubBeads = subs
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

// bdRunner executes a bd command with a timeout and returns combined output.
// It is a function type so tests can inject a fake without spawning real processes.
type bdRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// defaultRunCmd is the production bdRunner that delegates to the real bd CLI.
func defaultRunCmd(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, "bd", args...))
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// createSubBeads creates sub-beads via bd CLI with blocking dependency links.
// Each sub-bead blocks the parent so that bd ready excludes the parent until
// all sub-beads are closed. Children are chained sequentially (child N+1
// depends on child N) so the poller dispatches them in the order the AI
// specified. If adding a sequential dependency fails the function returns an
// error immediately (with the partial list of already-created sub-beads) so
// the caller can escalate to ActionClarify and prevent out-of-order dispatch.
func createSubBeads(ctx context.Context, parent poller.Bead, tasks []string, anvilPath string, run bdRunner) ([]SubBead, error) {
	if len(tasks) == 0 {
		return nil, fmt.Errorf("no sub-tasks to create")
	}

	resetParent := func() {
		rCtx, rCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer rCancel()
		if out, err := run(rCtx, anvilPath, "update", parent.ID, "--status=open", "--json"); err != nil {
			log.Printf("[schematic:%s] Warning: failed to reset parent to open: %v: %s", parent.ID, err, out)
		}
	}

	var subBeads []SubBead
	for _, task := range tasks {
		// Create sub-bead with blocks dependency so the parent is blocked
		// until all sub-beads are closed.
		createCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		out, err := run(createCtx, anvilPath,
			"create",
			"--title="+task,
			"--description=Sub-task decomposed from "+parent.ID+": "+parent.Title,
			"--type=task",
			fmt.Sprintf("--priority=%d", parent.Priority),
			"--deps", "blocks:"+parent.ID,
			"--json",
		)
		cancel()
		if err != nil {
			log.Printf("[schematic:%s] Partial decomposition failure after creating %d sub-beads: %v", parent.ID, len(subBeads), err)
			resetParent()
			return subBeads, fmt.Errorf("creating sub-bead %q: %w: %s", task, err, out)
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
				log.Printf("[schematic:%s] Partial decomposition failure after creating %d sub-beads: could not parse ID", parent.ID, len(subBeads))
				resetParent()
				return subBeads, fmt.Errorf("parsing sub-bead ID from output: %w: %s", err, out)
			}
		}

		subBeads = append(subBeads, SubBead{ID: created.ID, Title: task})

		// Chain sequential dependency: child N+1 depends on child N.
		// The schematic prompt asks the AI to order sub-tasks logically,
		// so we enforce that ordering via bd dep add.
		// A failure here is fatal: without the sequencing link a later child
		// could be dispatched before an earlier one completes, reintroducing
		// the original ordering problem. Return the partial list so the caller
		// can surface the issue to operators via ActionClarify.
		if len(subBeads) >= 2 {
			prev := subBeads[len(subBeads)-2]
			depCtx, depCancel := context.WithTimeout(ctx, 15*time.Second)
			depOut, depErr := run(depCtx, anvilPath, "dep", "add", created.ID, prev.ID)
			depCancel()
			if depErr != nil {
				log.Printf("[schematic:%s] Failed to add sequential dep %s -> %s: %v: %s",
					parent.ID, created.ID, prev.ID, depErr, depOut)
				resetParent()
				return subBeads, fmt.Errorf("adding sequential dependency %s -> %s: %w: %s",
					created.ID, prev.ID, depErr, depOut)
			}
		}
	}

	// Keep the parent open-but-blocked: its work is represented by its sub-beads.
	// Downstream beads can depend on blocks:<parent>, and will only be ready once
	// the children complete and unblock the parent.
	resetParent()

	return subBeads, nil
}

// buildPrompt creates the analysis prompt for the Schematic AI session.
// It uses strings.Builder instead of fmt.Sprintf to avoid issues with
// user-controlled bead fields containing '%' characters.
func buildPrompt(bead poller.Bead) string {
	prompt := `You are a software architect analysing a work item (bead) to determine the best approach.

## Bead to Analyse

`

	var b strings.Builder
	b.WriteString(prompt)

	// Append bead metadata literally so that any '%' characters in the bead fields
	// are treated as plain text and cannot affect formatting.
	b.WriteString("**ID**: ")
	b.WriteString(bead.ID)
	b.WriteString("\n**Title**: ")
	b.WriteString(bead.Title)
	b.WriteString("\n**Type**: ")
	b.WriteString(bead.IssueType)
	b.WriteString("\n**Priority**: ")
	b.WriteString(fmt.Sprintf("%d", bead.Priority))
	b.WriteString("\n\n### Description\n\n")
	b.WriteString(bead.Description)
	b.WriteString("\n")

	b.WriteString(`
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

` + "```json" + `
{
  "action": "plan|decompose|clarify",
  "plan": "Step-by-step implementation plan (only for action=plan)",
  "sub_tasks": ["Task 1 title", "Task 2 title"],
  "reason": "Brief explanation of your decision"
}
` + "```" + `

Keep sub_tasks to 2-5 items. Each should be a clear, self-contained task title.
For "plan", provide concrete steps: which files to modify, what to add, what to test.
`)

	return b.String()
}

// ChildBead is a lightweight summary of a child bead for the crucible check prompt.
type ChildBead struct {
	ID          string
	Title       string
	Description string
}

// RunCrucibleCheck determines whether a bead with children needs crucible
// orchestration (sequential work on a feature branch) or can be dispatched
// as a standalone bead with children handled individually.
//
// This is a lightweight schematic call that only inspects the relationship
// between parent and children — it does not produce implementation plans.
func RunCrucibleCheck(ctx context.Context, cfg Config, parent poller.Bead, children []ChildBead, anvilPath string, pv provider.Provider) *CrucibleCheckResult {
	start := time.Now()

	if !cfg.Enabled {
		return &CrucibleCheckResult{
			NeedsCrucible: false,
			Reason:        "Schematic disabled — defaulting to standalone dispatch",
			Duration:      time.Since(start),
		}
	}

	promptText := buildCruciblePrompt(parent, children)

	log.Printf("[schematic:%s] Running crucible check (provider: %s)", parent.ID, pv.Label())

	workDir, err := os.MkdirTemp("", "forge-crucible-check-*")
	if err != nil {
		return &CrucibleCheckResult{
			NeedsCrucible: false,
			Reason:        fmt.Sprintf("Failed to create temp dir: %v — defaulting to standalone", err),
			Duration:      time.Since(start),
		}
	}
	defer os.RemoveAll(workDir)

	logDir := filepath.Join(workDir, "logs")
	extraFlags := append([]string{"--max-turns", "5"}, cfg.ExtraFlags...)
	process, err := smith.SpawnWithProvider(ctx, workDir, promptText, logDir, pv, extraFlags)
	if err != nil {
		return &CrucibleCheckResult{
			NeedsCrucible: false,
			Reason:        fmt.Sprintf("Failed to spawn session: %v — defaulting to standalone", err),
			Duration:      time.Since(start),
		}
	}
	if cfg.OnSpawn != nil {
		cfg.OnSpawn(process.PID, process.LogPath)
	}

	smithResult := process.Wait()

	result := &CrucibleCheckResult{
		Duration: time.Since(start),
		CostUSD:  smithResult.CostUSD,
		Quota:    smithResult.Quota,
	}

	if smithResult.ExitCode != 0 || smithResult.RateLimited {
		result.Reason = "Schematic session failed — defaulting to standalone"
		return result
	}

	output := smithResult.FullOutput
	if output == "" {
		output = smithResult.Output
	}

	verdict, err := parseCrucibleVerdict(output)
	if err != nil {
		result.Reason = fmt.Sprintf("Could not parse crucible verdict — defaulting to standalone: %v", err)
		return result
	}

	result.NeedsCrucible = verdict.NeedsCrucible
	result.Reason = verdict.Reason
	return result
}

// crucibleVerdict is the JSON structure returned by the crucible check prompt.
type crucibleVerdict struct {
	NeedsCrucible bool   `json:"needs_crucible"`
	Reason        string `json:"reason"`
}

// parseCrucibleVerdict extracts the crucible decision from AI output.
func parseCrucibleVerdict(output string) (*crucibleVerdict, error) {
	// Try JSON in ```json fences
	if idx := strings.Index(output, "```json"); idx >= 0 {
		start := idx + len("```json")
		if end := strings.Index(output[start:], "```"); end >= 0 {
			var v crucibleVerdict
			if err := json.Unmarshal([]byte(strings.TrimSpace(output[start:start+end])), &v); err == nil {
				return &v, nil
			}
		}
	}

	// Try plain ``` fences
	if idx := strings.Index(output, "```"); idx >= 0 {
		start := idx + 3
		if nl := strings.Index(output[start:], "\n"); nl >= 0 {
			start += nl + 1
		}
		if end := strings.Index(output[start:], "```"); end >= 0 {
			block := strings.TrimSpace(output[start : start+end])
			if strings.Contains(block, "needs_crucible") {
				var v crucibleVerdict
				if err := json.Unmarshal([]byte(block), &v); err == nil {
					return &v, nil
				}
			}
		}
	}

	// Scan for raw JSON objects
	for i := 0; i < len(output); i++ {
		if output[i] != '{' {
			continue
		}
		depth := 0
		for j := i; j < len(output); j++ {
			switch output[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					block := output[i : j+1]
					if strings.Contains(block, `"needs_crucible"`) {
						var v crucibleVerdict
						if err := json.Unmarshal([]byte(block), &v); err == nil {
							return &v, nil
						}
					}
					i = j
					break
				}
			}
			if depth == 0 {
				break
			}
		}
	}

	return nil, fmt.Errorf("no valid crucible verdict JSON found in output")
}

// buildCruciblePrompt creates the prompt for the crucible check.
func buildCruciblePrompt(parent poller.Bead, children []ChildBead) string {
	var b strings.Builder

	b.WriteString(`You are a software architect determining how related work items should be orchestrated.

## Parent Bead

`)
	b.WriteString("**ID**: ")
	b.WriteString(parent.ID)
	b.WriteString("\n**Title**: ")
	b.WriteString(parent.Title)
	b.WriteString("\n**Type**: ")
	b.WriteString(parent.IssueType)
	b.WriteString("\n\n### Description\n\n")
	b.WriteString(parent.Description)
	b.WriteString("\n\n## Children (beads that depend on the parent)\n\n")

	for i, child := range children {
		b.WriteString(fmt.Sprintf("### Child %d: %s\n", i+1, child.ID))
		b.WriteString("**Title**: ")
		b.WriteString(child.Title)
		b.WriteString("\n**Description**: ")
		b.WriteString(child.Description)
		b.WriteString("\n\n")
	}

	b.WriteString(`## Your Task

Determine whether these beads need **crucible orchestration** (sequential work on a shared feature branch) or can be dispatched **individually** as standalone work items.

**Crucible orchestration** means: create a feature branch, complete the parent first, then each child in order — each building on the previous one's merged code. Use this when:
- The children depend on the parent's code changes being present
- The children modify the same files or build on each other's work
- The work forms a logical sequence where order matters for correctness

**Standalone dispatch** means: each bead is dispatched independently on its own branch. Use this when:
- The beads are independent pieces of work that happen to relate to the same goal
- Each bead can be implemented and tested without the others
- The dependency is just about priority/ordering, not code dependencies

## Output Format

Output your verdict as a JSON block:

` + "```json" + `
{
  "needs_crucible": true|false,
  "reason": "Brief explanation"
}
` + "```" + `
`)

	return b.String()
}
