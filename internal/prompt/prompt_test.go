package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuild_NotesRenderedWhenPresent(t *testing.T) {
	b := NewBuilder()
	ctx := BeadContext{
		BeadID:       "test-notes",
		Title:        "Test bead with notes",
		Description:  "Do the thing",
		Notes:        "Use the new API endpoint, not the legacy one.",
		IssueType:    "task",
		Priority:     3,
		Branch:       "forge/test-notes",
		AnvilName:    "test-anvil",
		AnvilPath:    t.TempDir(),
		WorktreePath: t.TempDir(),
	}

	result, err := b.Build(ctx)
	require.NoError(t, err)

	assert.Contains(t, result, "### Notes")
	assert.Contains(t, result, "Use the new API endpoint, not the legacy one.")
}

func TestBuild_NotesOmittedWhenEmpty(t *testing.T) {
	b := NewBuilder()
	ctx := BeadContext{
		BeadID:       "test-no-notes",
		Title:        "Test bead without notes",
		Description:  "Do the thing",
		Notes:        "",
		IssueType:    "task",
		Priority:     3,
		Branch:       "forge/test-no-notes",
		AnvilName:    "test-anvil",
		AnvilPath:    t.TempDir(),
		WorktreePath: t.TempDir(),
	}

	result, err := b.Build(ctx)
	require.NoError(t, err)

	assert.NotContains(t, result, "### Notes")
}

func TestBuild_CombinedModeAppendsChecklist(t *testing.T) {
	b := NewBuilder()
	ctx := BeadContext{
		BeadID:              "test-1",
		Title:               "Test bead",
		Description:         "Do the thing",
		IssueType:           "task",
		Priority:            3,
		Branch:              "forge/test-1",
		AnvilName:           "test-anvil",
		AnvilPath:           t.TempDir(),
		WorktreePath:        t.TempDir(),
		CopilotCombinedMode: true,
		WardenRules:         "1. [ ] Check: no hardcoded secrets (pattern: password|secret)\n",
	}

	result, err := b.Build(ctx)
	require.NoError(t, err)

	assert.Contains(t, result, "## Self-Review Checklist")
	assert.Contains(t, result, "self_review")
	assert.Contains(t, result, `"verdict"`)
	assert.Contains(t, result, "no hardcoded secrets")
	assert.Contains(t, result, "### Learned Review Rules")
}

func TestBuild_CombinedModeWithoutRules(t *testing.T) {
	b := NewBuilder()
	ctx := BeadContext{
		BeadID:              "test-2",
		Title:               "Test bead",
		Description:         "Do the thing",
		IssueType:           "task",
		Priority:            3,
		Branch:              "forge/test-2",
		AnvilName:           "test-anvil",
		AnvilPath:           t.TempDir(),
		WorktreePath:        t.TempDir(),
		CopilotCombinedMode: true,
		WardenRules:         "", // no learned rules
	}

	result, err := b.Build(ctx)
	require.NoError(t, err)

	assert.Contains(t, result, "## Self-Review Checklist")
	assert.NotContains(t, result, "### Learned Review Rules")
}

func TestBuild_NoCombinedModeNoChecklist(t *testing.T) {
	b := NewBuilder()
	ctx := BeadContext{
		BeadID:              "test-3",
		Title:               "Test bead",
		Description:         "Do the thing",
		IssueType:           "task",
		Priority:            3,
		Branch:              "forge/test-3",
		AnvilName:           "test-anvil",
		AnvilPath:           t.TempDir(),
		WorktreePath:        t.TempDir(),
		CopilotCombinedMode: false,
	}

	result, err := b.Build(ctx)
	require.NoError(t, err)

	assert.NotContains(t, result, "## Self-Review Checklist")
	assert.NotContains(t, result, "self_review")
}

func TestBuild_CombinedModeChecklistBeforeOverrides(t *testing.T) {
	b := NewBuilder()
	ctx := BeadContext{
		BeadID:              "test-4",
		Title:               "Test bead",
		Description:         "Do the thing",
		IssueType:           "task",
		Priority:            3,
		Branch:              "forge/test-4",
		AnvilName:           "test-anvil",
		AnvilPath:           t.TempDir(),
		WorktreePath:        t.TempDir(),
		CopilotCombinedMode: true,
	}

	result, err := b.Build(ctx)
	require.NoError(t, err)

	checklistIdx := strings.Index(result, "## Self-Review Checklist")
	overridesIdx := strings.Index(result, "## Orchestrator Overrides")
	assert.GreaterOrEqual(t, checklistIdx, 0, "## Self-Review Checklist section must be present in combined mode prompt")
	assert.GreaterOrEqual(t, overridesIdx, 0, "## Orchestrator Overrides section must be present in prompt")
	assert.Greater(t, overridesIdx, checklistIdx,
		"Self-Review Checklist should appear before Orchestrator Overrides")
}

func TestBuild_CombinedModeWardenRulesTemplateInjection(t *testing.T) {
	// Verify that WardenRules containing Go template directives are rendered
	// literally and do NOT trigger template execution (defense-in-depth).
	b := NewBuilder()
	maliciousRules := "1. [ ] Check: {{.AgentsMD}} injection attempt (pattern: {{template}})\n"
	ctx := BeadContext{
		BeadID:              "test-inject",
		Title:               "Injection test",
		Description:         "Verify template injection is safe",
		IssueType:           "task",
		Priority:            3,
		Branch:              "forge/test-inject",
		AnvilName:           "test-anvil",
		AnvilPath:           t.TempDir(),
		WorktreePath:        t.TempDir(),
		CopilotCombinedMode: true,
		WardenRules:         maliciousRules,
	}

	result, err := b.Build(ctx)
	require.NoError(t, err)

	// The malicious template directives must appear literally in the output,
	// not be interpreted by the template engine.
	assert.Contains(t, result, "{{.AgentsMD}}")
	assert.Contains(t, result, "{{template}}")
	assert.NotContains(t, result, wardenRulesPlaceholder,
		"placeholder must be replaced with actual rules content")
}

func TestBuild_CombinedModeWithLoadedWardenRules(t *testing.T) {
	// Create a temporary anvil directory with a .forge/warden-rules.yaml file
	// to verify the full LoadRules → FormatChecklist → prompt injection path.
	anvilDir := t.TempDir()
	forgeDir := filepath.Join(anvilDir, ".forge")
	require.NoError(t, os.MkdirAll(forgeDir, 0o755))

	rulesContent := `rules:
  - id: rule-001
    category: security
    pattern: "password|secret"
    check: "No hardcoded secrets in source code"
    source: "copilot:PR#42"
    added: "2025-01-01"
  - id: rule-002
    category: style
    pattern: "TODO"
    check: "No unresolved TODOs in production code"
    source: "copilot:PR#55"
    added: "2025-01-15"
`
	require.NoError(t, os.WriteFile(filepath.Join(forgeDir, "warden-rules.yaml"), []byte(rulesContent), 0o644))

	// Load rules using warden.LoadRules (the same function the pipeline uses).
	rf, err := warden.LoadRules(anvilDir)
	require.NoError(t, err)
	require.Len(t, rf.Rules, 2)

	// Format as checklist (the same function the pipeline uses).
	checklist := rf.FormatChecklist()
	assert.Contains(t, checklist, "No hardcoded secrets")
	assert.Contains(t, checklist, "No unresolved TODOs")
	assert.Contains(t, checklist, "pattern: password|secret")

	// Build the prompt with the formatted checklist injected.
	b := NewBuilder()
	ctx := BeadContext{
		BeadID:              "test-rules",
		Title:               "Test with real rules",
		Description:         "Verify LoadRules+FormatChecklist integration",
		IssueType:           "task",
		Priority:            3,
		Branch:              "forge/test-rules",
		AnvilName:           "test-anvil",
		AnvilPath:           anvilDir,
		WorktreePath:        t.TempDir(),
		CopilotCombinedMode: true,
		WardenRules:         checklist,
	}

	result, err := b.Build(ctx)
	require.NoError(t, err)

	// Verify the full chain: rules file → LoadRules → FormatChecklist → prompt.
	assert.Contains(t, result, "## Self-Review Checklist")
	assert.Contains(t, result, "### Learned Review Rules")
	assert.Contains(t, result, "No hardcoded secrets in source code")
	assert.Contains(t, result, "No unresolved TODOs in production code")
	assert.Contains(t, result, "pattern: password|secret")
}

func TestBuild_ExternalRefRenderedWhenSet(t *testing.T) {
	b := NewBuilder()
	ctx := BeadContext{
		BeadID:       "test-extref",
		Title:        "Test bead with external ref",
		Description:  "Do the thing",
		IssueType:    "task",
		Priority:     3,
		Branch:       "forge/test-extref",
		AnvilName:    "test-anvil",
		AnvilPath:    t.TempDir(),
		WorktreePath: t.TempDir(),
		ExternalRef:  "https://github.com/org/repo/issues/42",
	}

	result, err := b.Build(ctx)
	require.NoError(t, err)

	assert.Contains(t, result, "**External Reference**")
	assert.Contains(t, result, "https://github.com/org/repo/issues/42")
}

func TestBuild_ExternalRefGhShorthandRendered(t *testing.T) {
	b := NewBuilder()
	ctx := BeadContext{
		BeadID:       "test-extref-gh",
		Title:        "Test bead with gh shorthand",
		Description:  "Do the thing",
		IssueType:    "task",
		Priority:     3,
		Branch:       "forge/test-extref-gh",
		AnvilName:    "test-anvil",
		AnvilPath:    t.TempDir(),
		WorktreePath: t.TempDir(),
		ExternalRef:  "gh-42",
	}

	result, err := b.Build(ctx)
	require.NoError(t, err)

	assert.Contains(t, result, "**External Reference**")
	assert.Contains(t, result, "gh-42")
}

func TestBuild_ConventionsRenderedWhenFilePresent(t *testing.T) {
	anvilDir := t.TempDir()
	forgeDir := filepath.Join(anvilDir, ".forge")
	require.NoError(t, os.MkdirAll(forgeDir, 0o755))

	conventions := "- Bilingual changelog fragments required (en + nb).\n- All stories must have a Storybook file.\n"
	require.NoError(t, os.WriteFile(filepath.Join(forgeDir, "conventions.md"), []byte(conventions), 0o644))

	b := NewBuilder()
	ctx := BeadContext{
		BeadID:       "test-conv-present",
		Title:        "Test with conventions",
		Description:  "Do the thing",
		IssueType:    "task",
		Priority:     3,
		Branch:       "forge/test-conv-present",
		AnvilName:    "test-anvil",
		AnvilPath:    anvilDir,
		WorktreePath: t.TempDir(),
	}

	result, err := b.Build(ctx)
	require.NoError(t, err)

	assert.Contains(t, result, "## Project Rules (Non-Negotiable)")
	assert.Contains(t, result, "Bilingual changelog fragments required")
	assert.Contains(t, result, "All stories must have a Storybook file.")
}

func TestBuild_ConventionsPositionedBetweenInstructionsAndEscalation(t *testing.T) {
	anvilDir := t.TempDir()
	forgeDir := filepath.Join(anvilDir, ".forge")
	require.NoError(t, os.MkdirAll(forgeDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(forgeDir, "conventions.md"), []byte("- rule one\n"), 0o644))

	b := NewBuilder()
	ctx := BeadContext{
		BeadID:       "test-conv-pos",
		Title:        "Test conventions position",
		Description:  "Do the thing",
		IssueType:    "task",
		Priority:     3,
		Branch:       "forge/test-conv-pos",
		AnvilName:    "test-anvil",
		AnvilPath:    anvilDir,
		WorktreePath: t.TempDir(),
	}

	result, err := b.Build(ctx)
	require.NoError(t, err)

	instructionsIdx := strings.Index(result, "## Instructions")
	conventionsIdx := strings.Index(result, "## Project Rules (Non-Negotiable)")
	escalationIdx := strings.Index(result, "## Escalation")

	require.GreaterOrEqual(t, instructionsIdx, 0, "## Instructions section must be present")
	require.GreaterOrEqual(t, conventionsIdx, 0, "## Project Rules section must be present")
	require.GreaterOrEqual(t, escalationIdx, 0, "## Escalation section must be present")

	assert.Greater(t, conventionsIdx, instructionsIdx,
		"Project Rules should appear after Instructions")
	assert.Less(t, conventionsIdx, escalationIdx,
		"Project Rules should appear before Escalation")
}

func TestBuild_ConventionsOmittedWhenFileAbsent(t *testing.T) {
	// Anvil directory exists but has no .forge/conventions.md file.
	anvilDir := t.TempDir()

	b := NewBuilder()
	ctx := BeadContext{
		BeadID:       "test-conv-absent",
		Title:        "Test without conventions",
		Description:  "Do the thing",
		IssueType:    "task",
		Priority:     3,
		Branch:       "forge/test-conv-absent",
		AnvilName:    "test-anvil",
		AnvilPath:    anvilDir,
		WorktreePath: t.TempDir(),
	}

	result, err := b.Build(ctx)
	require.NoError(t, err)

	assert.NotContains(t, result, "## Project Rules (Non-Negotiable)")
}

func TestBuild_ConventionsContentNotTemplateExpanded(t *testing.T) {
	// Conventions content containing Go template directives must appear
	// literally in the rendered prompt — Go's text/template does not
	// re-process substituted values, but we assert the behavior explicitly
	// so future refactors cannot silently break this invariant.
	anvilDir := t.TempDir()
	forgeDir := filepath.Join(anvilDir, ".forge")
	require.NoError(t, os.MkdirAll(forgeDir, 0o755))

	conventions := "Use the placeholder {{.SomeField}} and the directive {{if foo}}bar{{end}} verbatim.\n"
	require.NoError(t, os.WriteFile(filepath.Join(forgeDir, "conventions.md"), []byte(conventions), 0o644))

	b := NewBuilder()
	ctx := BeadContext{
		BeadID:       "test-conv-no-expand",
		Title:        "Test conventions template injection",
		Description:  "Verify template directives are not expanded",
		IssueType:    "task",
		Priority:     3,
		Branch:       "forge/test-conv-no-expand",
		AnvilName:    "test-anvil",
		AnvilPath:    anvilDir,
		WorktreePath: t.TempDir(),
	}

	result, err := b.Build(ctx)
	require.NoError(t, err)

	assert.Contains(t, result, "{{.SomeField}}")
	assert.Contains(t, result, "{{if foo}}bar{{end}}")
}

func TestBuild_ExternalRefOmittedWhenEmpty(t *testing.T) {
	b := NewBuilder()
	ctx := BeadContext{
		BeadID:       "test-no-extref",
		Title:        "Test bead without external ref",
		Description:  "Do the thing",
		IssueType:    "task",
		Priority:     3,
		Branch:       "forge/test-no-extref",
		AnvilName:    "test-anvil",
		AnvilPath:    t.TempDir(),
		WorktreePath: t.TempDir(),
		ExternalRef:  "",
	}

	result, err := b.Build(ctx)
	require.NoError(t, err)

	assert.NotContains(t, result, "**External Reference**")
}
