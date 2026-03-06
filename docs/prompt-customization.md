# Per-Anvil Prompt Customization

Each anvil can override the default Smith prompt by placing a Go template file in the repository.

## Template Location

Forge looks for custom templates in this order (first found wins):

1. `<anvil-path>/.forge/prompt.tmpl`
2. `<anvil-path>/.forge/smith-prompt.tmpl`

If neither exists, the built-in default prompt is used.

## Template Variables

The template receives a `templateData` struct with these fields:

| Variable | Type | Description |
|----------|------|-------------|
| `{{.Bead.BeadID}}` | string | Unique bead identifier (e.g., `BD-42`) |
| `{{.Bead.Title}}` | string | Bead title |
| `{{.Bead.Description}}` | string | Full bead description |
| `{{.Bead.IssueType}}` | string | Type: `bug`, `feature`, `task`, `epic`, `chore` |
| `{{.Bead.Priority}}` | int | Priority level (0=critical, 4=backlog) |
| `{{.Bead.Parent}}` | string | Parent bead ID (if hierarchical) |
| `{{.Bead.Branch}}` | string | Git branch name for this bead |
| `{{.Bead.AnvilName}}` | string | Name of the anvil |
| `{{.Bead.AnvilPath}}` | string | Filesystem path to the anvil root |
| `{{.Bead.WorktreePath}}` | string | Filesystem path to the worktree |
| `{{.Bead.SchematicPlan}}` | string | Pre-analysis plan from Schematic (if enabled) |
| `{{.AgentsMD}}` | string | Contents of `AGENTS.md` from the anvil root |
| `{{.ClaudeMD}}` | string | Contents of `CLAUDE.md` from the anvil root |
| `{{.ReadmeMD}}` | string | Contents of `README.md` from the anvil root |

## Template Functions

Two built-in functions are available:

| Function | Description | Example |
|----------|-------------|---------|
| `readFile` | Read a file safely (returns empty string on error) | `{{readFile ".forge/extra-context.md"}}` |
| `upper` | Convert string to uppercase | `{{upper .Bead.IssueType}}` |

## Example Template

```
You are an autonomous AI developer working on the {{.Bead.AnvilName}} repository.

## Task

**Bead**: {{.Bead.BeadID}}
**Title**: {{.Bead.Title}}
**Type**: {{.Bead.IssueType}} | **Priority**: {{.Bead.Priority}}

### Description

{{.Bead.Description}}

## Working Directory

You are working in a git worktree at: {{.Bead.WorktreePath}}
Branch: {{.Bead.Branch}}
Main repository: {{.Bead.AnvilPath}}

## Instructions

1. Implement the task fully
2. Follow the repository's coding standards
3. Run tests — do not break existing tests
4. Commit with message: "feat: <description> ({{.Bead.BeadID}})"
5. Push your branch
6. Do NOT create a PR

{{if .Bead.SchematicPlan}}
## Implementation Plan

{{.Bead.SchematicPlan}}
{{end}}

{{if .AgentsMD}}
## Repository Guidelines (AGENTS.md)

{{.AgentsMD}}
{{end}}

{{if .ClaudeMD}}
## AI Instructions (CLAUDE.md)

{{.ClaudeMD}}
{{end}}
```

## Default Prompt Structure

When no custom template is provided, the built-in prompt includes:

1. **Task section** — bead ID, title, type, priority, parent reference
2. **Working directory** — worktree path, branch, repository root
3. **Instructions** — 6-point directive (implement, follow standards, test, commit, push, no PR)
4. **Constraints** — stay focused on bead, handle blockers, use best judgment
5. **Schematic plan** (conditional) — included if the pre-worker produced one
6. **Repository context** (conditional) — AGENTS.md, CLAUDE.md, README.md if present

## File Reading

The `readFile` function calls `os.ReadFile` on the path exactly as given — relative paths are resolved relative to the **Forge process's working directory**, not the worktree. To reliably read files from the worktree or anvil, construct absolute paths using `.Bead.WorktreePath` or `.Bead.AnvilPath`:

```
{{readFile (printf "%s/.forge/style-guide.md" .Bead.WorktreePath)}}
```

The function returns an empty string on any error (missing file, permission denied), so templates can safely reference files that may or may not exist.
