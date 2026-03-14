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

func TestSaveAndLoadRulesWithColonSpace(t *testing.T) {
	dir := t.TempDir()
	rf := &RulesFile{
		Rules: []Rule{
			{
				ID:       "colon-rule",
				Category: "convention: naming",
				Pattern:  "pattern: either use X or Y",
				Check:    "convention: either use camelCase or snake_case",
				Source:   "copilot",
				Added:    "2026-03-14",
			},
			{
				ID:       "hash-rule",
				Category: "comments",
				Pattern:  "inline comment",
				Check:    "avoid # style comments in YAML values",
				Source:   "manual",
				Added:    "2026-03-14",
			},
		},
	}

	err := SaveRules(dir, rf)
	require.NoError(t, err)

	// Round-trip: reload should succeed without parse errors
	loaded, err := LoadRules(dir)
	require.NoError(t, err)
	require.Len(t, loaded.Rules, 2)

	assert.Equal(t, "convention: either use camelCase or snake_case", loaded.Rules[0].Check)
	assert.Equal(t, "convention: naming", loaded.Rules[0].Category)
	assert.Equal(t, "pattern: either use X or Y", loaded.Rules[0].Pattern)
	assert.Equal(t, "avoid # style comments in YAML values", loaded.Rules[1].Check)
}

func TestSaveRulesQuotesSpecialValues(t *testing.T) {
	dir := t.TempDir()
	rf := &RulesFile{
		Rules: []Rule{
			{
				ID:    "q1",
				Check: "convention: use consistent naming",
			},
		},
	}

	err := SaveRules(dir, rf)
	require.NoError(t, err)

	// Read raw YAML and verify the check value is quoted
	data, err := os.ReadFile(filepath.Join(dir, RulesFileName))
	require.NoError(t, err)

	raw := string(data)
	assert.Contains(t, raw, `"convention: use consistent naming"`,
		"check value containing ': ' should be double-quoted in YAML output")
}

func TestParsePaginatedComments_SinglePage(t *testing.T) {
	input := `[{"body":"comment one","path":"foo.go","user":{"login":"copilot[bot]"}},{"body":"comment two","path":"bar.go","user":{"login":"copilot[bot]"}}]`
	got, err := parsePaginatedComments([]byte(input))
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "comment one", got[0].Body)
	assert.Equal(t, "comment two", got[1].Body)
}

func TestParsePaginatedComments_MultiPage(t *testing.T) {
	// gh api --paginate concatenates arrays without a separator
	page1 := `[{"body":"page1 comment","path":"a.go","user":{"login":"copilot[bot]"}}]`
	page2 := `[{"body":"page2 comment","path":"b.go","user":{"login":"copilot[bot]"}}]`
	input := page1 + page2
	got, err := parsePaginatedComments([]byte(input))
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "page1 comment", got[0].Body)
	assert.Equal(t, "page2 comment", got[1].Body)
}

func TestGroupComments_ExactMatch(t *testing.T) {
	comments := []PRComment{
		{Body: "Check for data races", PRNumber: 1},
		{Body: "Missing error check", PRNumber: 1},
		{Body: "Check for data races", PRNumber: 2},    // duplicate body
		{Body: "  missing error check  ", PRNumber: 3}, // whitespace variant
	}

	groups := GroupComments(comments)
	assert.Len(t, groups, 2) // "check for data races" and "missing error check"

	// First group should have 2 comments (PR#1 and PR#2 with same body)
	assert.Len(t, groups[0], 2)
	// Second group should have 2 comments (body normalized to same)
	assert.Len(t, groups[1], 2)
}

func TestGroupComments_SemanticMerge(t *testing.T) {
	// Comments about the same pattern but with different wording should be
	// merged into the same group via keyword overlap.
	comments := []PRComment{
		{Body: "missing error check on Open()", PRNumber: 1},
		{Body: "error from ReadFile not handled — missing check", PRNumber: 2},
		{Body: "possible data race on shared counter", PRNumber: 3},
		{Body: "data race: concurrent access to shared map", PRNumber: 4},
	}

	groups := GroupComments(comments)
	// Should produce 2 groups: error-handling and data-race
	assert.Len(t, groups, 2)

	// Each semantic cluster should contain 2 comments
	assert.Len(t, groups[0], 2)
	assert.Len(t, groups[1], 2)
}

func TestGroupComments_NoMergeDissimilar(t *testing.T) {
	// Comments about completely different topics must stay separate.
	comments := []PRComment{
		{Body: "SQL injection vulnerability in user query builder", PRNumber: 1},
		{Body: "unused import should be removed from module", PRNumber: 2},
		{Body: "missing unit test coverage for edge case", PRNumber: 3},
	}

	groups := GroupComments(comments)
	assert.Len(t, groups, 3)
}

func TestExtractKeywords(t *testing.T) {
	kw := extractKeywords("The missing error check should be handled")
	assert.Contains(t, kw, "missing")
	assert.Contains(t, kw, "error")
	assert.Contains(t, kw, "check")
	assert.Contains(t, kw, "handled")
	// Stop words removed
	assert.NotContains(t, kw, "the")
	assert.NotContains(t, kw, "should")
}

func TestJaccardSimilarity(t *testing.T) {
	a := map[string]bool{"error": true, "check": true, "missing": true}
	b := map[string]bool{"error": true, "handled": true, "missing": true, "check": true}

	sim := jaccardSimilarity(a, b)
	// intersection=3, union=4, sim=0.75
	assert.InDelta(t, 0.75, sim, 0.01)

	// Completely disjoint sets
	c := map[string]bool{"data": true, "race": true}
	sim2 := jaccardSimilarity(a, c)
	assert.InDelta(t, 0.0, sim2, 0.01)

	// Both empty — should return 0.0, not 1.0, to avoid merging zero-keyword comments
	empty1 := map[string]bool{}
	empty2 := map[string]bool{}
	sim3 := jaccardSimilarity(empty1, empty2)
	assert.InDelta(t, 0.0, sim3, 0.01)
}
