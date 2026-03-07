// Package prompt builds prompts for Smith (Claude Code) workers from bead metadata.
//
// Prompts combine repo context (AGENTS.md), bead description, coding standards,
// and per-anvil overrides into a single instruction string for Claude.
package prompt

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// BeadContext holds the information needed to build a Smith prompt.
type BeadContext struct {
	// BeadID is the unique bead identifier.
	BeadID string
	// Title is the bead title.
	Title string
	// Description is the full bead description.
	Description string
	// IssueType is the bead type (bug, feature, task, etc.).
	IssueType string
	// Priority is the bead priority (0-4).
	Priority int
	// Parent is the parent bead ID (if any).
	Parent string
	// Branch is the git branch created for this work.
	Branch string
	// AnvilName is the name of the anvil (repo label).
	AnvilName string
	// AnvilPath is the path to the main repo.
	AnvilPath string
	// WorktreePath is the path to the worker's worktree.
	WorktreePath string
	// SchematicPlan is an optional implementation plan produced by the
	// Schematic pre-worker. When non-empty it is included in the prompt.
	SchematicPlan string
	// Iteration is the current Smith-Warden cycle (1 = first attempt).
	// On iteration 2+ the prompt includes prior feedback.
	Iteration int
	// PriorFeedback is pre-formatted feedback from the previous iteration
	// (Warden review or Temper build/test failure). Set on iteration 2+.
	PriorFeedback string
	// PriorFeedbackSource describes where the feedback came from
	// (e.g. "Warden review" or "build/test verification").
	PriorFeedbackSource string
}

// Builder constructs prompts from templates and context.
type Builder struct {
	// CustomTemplate is an optional per-anvil template override.
	// If empty, the default template is used.
	CustomTemplate string
}

// NewBuilder creates a Builder with default settings.
func NewBuilder() *Builder {
	return &Builder{}
}

// Build constructs a complete prompt for a Smith worker.
func (b *Builder) Build(ctx BeadContext) (string, error) {
	tmplText := defaultTemplate
	if b.CustomTemplate != "" {
		tmplText = b.CustomTemplate
	}

	tmpl, err := template.New("prompt").Funcs(template.FuncMap{
		"readFile": readFileSafe,
		"upper":    strings.ToUpper,
	}).Parse(tmplText)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}

	// Gather repo context files
	data := templateData{
		Bead:      ctx,
		AgentsMD:  readFileSafe(filepath.Join(ctx.AnvilPath, "AGENTS.md")),
		ClaudeMD:  readFileSafe(filepath.Join(ctx.AnvilPath, "CLAUDE.md")),
		ReadmeMD:  readFileSafe(filepath.Join(ctx.AnvilPath, "README.md")),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}

	return buf.String(), nil
}

// LoadCustomTemplate reads a custom template file for an anvil.
// Returns empty string if the file doesn't exist.
func LoadCustomTemplate(anvilPath string) string {
	paths := []string{
		filepath.Join(anvilPath, ".forge", "prompt.tmpl"),
		filepath.Join(anvilPath, ".forge", "smith-prompt.tmpl"),
	}

	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err == nil {
			return string(data)
		}
	}

	return ""
}

// templateData is the full set of data available to the prompt template.
type templateData struct {
	Bead     BeadContext
	AgentsMD string
	ClaudeMD string
	ReadmeMD string
}

// readFileSafe reads a file and returns its contents, or empty string on error.
func readFileSafe(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// defaultTemplate is the standard Smith prompt template.
// It provides repo context, task description, and clear instructions.
var defaultTemplate = `You are an autonomous AI developer working on the {{.Bead.AnvilName}} repository.

## Task

**Bead**: {{.Bead.BeadID}}
**Title**: {{.Bead.Title}}
**Type**: {{.Bead.IssueType}} | **Priority**: {{.Bead.Priority}}
{{- if .Bead.Parent}}
**Parent**: {{.Bead.Parent}}
{{- end}}

### Description

{{.Bead.Description}}

## Working Directory

You are working in a git worktree at: {{.Bead.WorktreePath}}
Branch: {{.Bead.Branch}}
Main repository: {{.Bead.AnvilPath}}

## Instructions

1. **Implement the task** described above fully and completely
2. **Follow the repository's coding standards** (see context below)
3. **Do NOT run builds, tests, or linters** — the Temper verification step runs build, lint, and test checks automatically after you finish. Focus your time on implementation.
4. **Commit your changes** with a clear commit message referencing the bead:
   - Format: "feat: <description>" or "fix: <description>"
   - Include "Bead: {{.Bead.BeadID}}" in the commit body
5. **Push your branch** to the remote: git push -u origin {{.Bead.Branch}}
6. **Do not create a PR** — that will be handled by the orchestrator

## Escalation — When to Ask for Human Help

If you encounter a situation where you cannot reasonably complete the task, signal this by
including the exact marker **NEEDS_HUMAN:** followed by a short reason on its own line in
your final output. Do **not** commit or push when escalating — just output the marker and stop.

Escalate when you encounter:
- **Ambiguous requirements** you cannot resolve with a reasonable interpretation
- **Missing configuration or secrets** you cannot access (API keys, tokens, environment variables)
- **External dependencies or URLs** that are required but not provided or reachable
- **Architectural decisions** that require human judgment (e.g. choosing between incompatible approaches)
- **Permission or access issues** that prevent you from completing the work
- **Scope far beyond the bead** — the task requires changes across many unrelated systems

Example escalation output:
` + "`" + `` + "`" + `` + "`" + `
NEEDS_HUMAN: The bead requires an API key for the payment service which is not available in the environment or config files.
` + "`" + `` + "`" + `` + "`" + `

Do NOT escalate for:
- Build or test failures (Temper handles verification)
- Minor ambiguity you can resolve with a reasonable default
- Missing documentation you can infer from code

## Changelog Fragment

If your changes are user-visible (new features, bug fixes, behavior changes, config changes),
you **MUST** create a changelog fragment file at:

` + "`" + `changelog.d/{{.Bead.BeadID}}.en.md` + "`" + `

Use this exact format:

` + "`" + `` + "`" + `` + "`" + `markdown
category: <Added|Changed|Fixed|Removed>
- **Short bold summary of the change** - Additional detail explaining what changed and why. ({{.Bead.BeadID}})
` + "`" + `` + "`" + `` + "`" + `

**Category guide:**
- ` + "`" + `Added` + "`" + ` — new features or capabilities
- ` + "`" + `Changed` + "`" + ` — modifications to existing behavior
- ` + "`" + `Fixed` + "`" + ` — bug fixes
- ` + "`" + `Removed` + "`" + ` — removed features or deprecated items

**Skip the fragment** only for purely internal changes (refactoring, test-only, CI config,
documentation) that have zero user-visible effect.

## Constraints

- Stay focused on this bead only — do not fix unrelated issues
- If you discover blocking issues, note them in your commit message
- If the task is unclear, implement the most reasonable interpretation
- Do not run tests, builds, or linters — Temper handles verification
{{- if .Bead.PriorFeedback}}

## Previous Iteration Feedback (from {{.Bead.PriorFeedbackSource}})

This is iteration {{.Bead.Iteration}} — your previous attempt had issues. Fix ALL of the
following problems while preserving the parts that were correct:

{{.Bead.PriorFeedback}}
{{- end}}
{{- if .Bead.SchematicPlan}}

## Implementation Plan (from Schematic analysis)

The following plan was produced by an architectural pre-analysis of this bead.
Follow it as a guide, but use your judgement if you discover the plan needs adjustment.

{{.Bead.SchematicPlan}}
{{- end}}

{{- if .AgentsMD}}

## Repository Guidelines (AGENTS.md)

{{.AgentsMD}}
{{- end}}

{{- if .ClaudeMD}}

## AI Instructions (CLAUDE.md)

{{.ClaudeMD}}
{{- end}}

{{- if .ReadmeMD}}

## Project Overview (README.md)

{{.ReadmeMD}}
{{- end}}
`
