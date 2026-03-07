package warden

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadRules_NonExistent(t *testing.T) {
	rf, err := LoadRules(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, rf.Rules)
}

func TestSaveAndLoadRules(t *testing.T) {
	dir := t.TempDir()
	rf := &RulesFile{
		Rules: []Rule{
			{ID: "test-rule", Category: "testing", Pattern: "test pattern", Check: "verify test", Source: "manual", Added: "2026-03-07"},
		},
	}

	err := SaveRules(dir, rf)
	require.NoError(t, err)

	// Verify file exists
	_, err = os.Stat(filepath.Join(dir, RulesFileName))
	require.NoError(t, err)

	// Reload
	loaded, err := LoadRules(dir)
	require.NoError(t, err)
	require.Len(t, loaded.Rules, 1)
	assert.Equal(t, "test-rule", loaded.Rules[0].ID)
	assert.Equal(t, "testing", loaded.Rules[0].Category)
}

func TestAddRule_Dedup(t *testing.T) {
	rf := &RulesFile{}

	added := rf.AddRule(Rule{ID: "r1", Check: "check 1"})
	assert.True(t, added)
	assert.Len(t, rf.Rules, 1)

	added = rf.AddRule(Rule{ID: "r1", Check: "different check"})
	assert.False(t, added)
	assert.Len(t, rf.Rules, 1)
	assert.Equal(t, "check 1", rf.Rules[0].Check) // original unchanged

	added = rf.AddRule(Rule{ID: "r2", Check: "check 2"})
	assert.True(t, added)
	assert.Len(t, rf.Rules, 2)
}

func TestRemoveRule(t *testing.T) {
	rf := &RulesFile{
		Rules: []Rule{
			{ID: "a"}, {ID: "b"}, {ID: "c"},
		},
	}

	assert.True(t, rf.RemoveRule("b"))
	assert.Len(t, rf.Rules, 2)
	assert.Equal(t, "a", rf.Rules[0].ID)
	assert.Equal(t, "c", rf.Rules[1].ID)

	assert.False(t, rf.RemoveRule("nonexistent"))
	assert.Len(t, rf.Rules, 2)
}

func TestFormatChecklist_Empty(t *testing.T) {
	rf := &RulesFile{}
	assert.Equal(t, "", rf.FormatChecklist())
}

func TestFormatChecklist(t *testing.T) {
	rf := &RulesFile{
		Rules: []Rule{
			{ID: "r1", Pattern: "data race", Check: "verify atomic access"},
			{ID: "r2", Pattern: "width calc", Check: "ensure consistent formula"},
		},
	}

	checklist := rf.FormatChecklist()
	assert.Contains(t, checklist, "1. [ ] Check: verify atomic access (pattern: data race)")
	assert.Contains(t, checklist, "2. [ ] Check: ensure consistent formula (pattern: width calc)")
}

func TestGroupComments(t *testing.T) {
	comments := []PRComment{
		{Body: "Check for data races", PRNumber: 1},
		{Body: "Missing error check", PRNumber: 1},
		{Body: "Check for data races", PRNumber: 2}, // duplicate body
		{Body: "  missing error check  ", PRNumber: 3}, // whitespace variant
	}

	groups := GroupComments(comments)
	assert.Len(t, groups, 2) // "check for data races" and "missing error check"

	// First group should have 2 comments (PR#1 and PR#2 with same body)
	assert.Len(t, groups[0], 2)
	// Second group should have 2 comments (body normalized to same)
	assert.Len(t, groups[1], 2)
}
