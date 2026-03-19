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
