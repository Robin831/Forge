package prompt

import (
	"strings"
	"testing"

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
	assert.Greater(t, overridesIdx, checklistIdx,
		"Self-Review Checklist should appear before Orchestrator Overrides")
}
