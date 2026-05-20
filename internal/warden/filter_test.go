package warden

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractDiffWords(t *testing.T) {
	got := extractDiffWords("async await deadlock; await(again)")
	// distinct ≥4-char tokens, lowercased
	assert.Equal(t, []string{"async", "await", "deadlock", "again"}, got)

	// empty input
	assert.Nil(t, extractDiffWords(""))

	// only short tokens — nothing kept
	assert.Empty(t, extractDiffWords("a b c d ef gh"))
}

func TestPatternGrep_NoWordsAlwaysPasses(t *testing.T) {
	// Pattern with no ≥4-char words — keep the rule.
	assert.True(t, patternGrep("a; b!", "anything"))
	assert.True(t, patternGrep("", "anything"))
}

func TestPatternGrep_MatchAndMiss(t *testing.T) {
	diff := strings.ToLower("public void HandleDeadlock() {}")
	// At least one ≥4-char word matches.
	assert.True(t, patternGrep("async await deadlock", diff))
	// No ≥4-char word matches.
	assert.False(t, patternGrep("xyzzy plover wibble", diff))
}

func TestChangedFilesFromDiff(t *testing.T) {
	diff := "diff --git a/foo.cs b/foo.cs\n@@ -1 +1 @@\n-old\n+new\n" +
		"diff --git a/bar/baz.tsx b/bar/baz.tsx\n@@ -1 +1 @@\n-x\n+y\n"
	got := changedFilesFromDiff(diff)
	assert.Equal(t, []string{"foo.cs", "bar/baz.tsx"}, got)
}

func TestChangedFilesFromDiff_NoHeaders(t *testing.T) {
	assert.Nil(t, changedFilesFromDiff("no headers here"))
	assert.Nil(t, changedFilesFromDiff(""))
}

func TestCategoriesForFile(t *testing.T) {
	cs := categoriesForFile("src/foo.cs")
	assert.True(t, cs["error-handling"])
	assert.True(t, cs["security"])
	assert.False(t, cs["ui"])
	assert.False(t, cs["testing"])

	tsx := categoriesForFile("web/Button.tsx")
	assert.True(t, tsx["ui"])
	assert.True(t, tsx["style"])
	assert.False(t, tsx["error-handling"])

	test := categoriesForFile("src/FooTests.cs")
	assert.True(t, test["testing"])
	assert.False(t, test["security"])

	test2 := categoriesForFile("web/Button_test.tsx")
	assert.True(t, test2["testing"])
	assert.False(t, test2["ui"])

	yaml := categoriesForFile(".github/workflows/ci.yaml")
	assert.True(t, yaml["security"])
	assert.True(t, yaml["other"])

	dockerfile := categoriesForFile("Dockerfile")
	assert.True(t, dockerfile["security"])

	// Fallback for unrecognised extension.
	other := categoriesForFile("README.md")
	assert.True(t, other["ui"])
	assert.True(t, other["security"])
	assert.True(t, other["testing"])
}

func TestFilterRules_PathGlobFilter(t *testing.T) {
	rules := []Rule{
		{ID: "r1", Category: "ui", Pattern: "react component", Paths: []string{"**/*.tsx"}},
		{ID: "r2", Category: "security", Pattern: "csrf", Paths: nil}, // no path constraint
	}
	cfg := DefaultReviewFilterConfig()
	// Only .cs file changed — r1 (tsx-only) must be dropped.
	got := FilterRules(rules, "diff content with react component and csrf words", []string{"foo.cs"}, cfg)
	ids := ruleIDs(got)
	assert.NotContains(t, ids, "r1", "tsx-scoped rule must be filtered out when no .tsx file changed")
	assert.Contains(t, ids, "r2", "rule without Paths must pass the path-glob filter")
}

func TestFilterRules_CategoryFilter_CsOnly(t *testing.T) {
	rules := []Rule{
		{ID: "ui-1", Category: "ui", Pattern: "component", Check: "x"},
		{ID: "test-1", Category: "testing", Pattern: "assertion", Check: "y"},
		{ID: "sec-1", Category: "security", Pattern: "validate", Check: "z"},
		{ID: "doc-1", Category: "documentation", Pattern: "comment", Check: "w"},
	}
	cfg := DefaultReviewFilterConfig()
	// Pass a diff that contains every pattern word so the pattern-grep filter
	// doesn't accidentally drop rules — we're only testing category routing.
	diff := "ui component testing assertion security validate doc comment"
	got := FilterRules(rules, diff, []string{"src/Service.cs"}, cfg)
	ids := ruleIDs(got)
	assert.NotContains(t, ids, "ui-1", ".cs-only diff must drop ui-categorised rules")
	assert.NotContains(t, ids, "test-1", ".cs-only diff must drop testing-categorised rules")
	assert.Contains(t, ids, "sec-1", "security applies to .cs files")
	assert.Contains(t, ids, "doc-1", "non-canonical categories pass the category filter unfiltered")
}

func TestFilterRules_PatternGrepFilter(t *testing.T) {
	rules := []Rule{
		{ID: "keep", Category: "concurrency", Pattern: "async await deadlock", Check: "x"},
		{ID: "drop", Category: "concurrency", Pattern: "xyzzy plover wibble", Check: "y"},
	}
	cfg := DefaultReviewFilterConfig()
	diff := "diff --git a/foo.cs b/foo.cs\n+await Task.Delay(); // potential deadlock"
	got := FilterRules(rules, diff, []string{"foo.cs"}, cfg)
	ids := ruleIDs(got)
	assert.Contains(t, ids, "keep")
	assert.NotContains(t, ids, "drop")
}

func TestFilterRules_MaxRulesCap(t *testing.T) {
	var rules []Rule
	for i := 0; i < 10; i++ {
		rules = append(rules, Rule{
			ID:       string(rune('a'+i)) + "-rule",
			Category: "other",
			Pattern:  "always", // 6 chars → matches "always" in the diff, kept by pattern-grep
			Check:    "x",
		})
	}
	cfg := DefaultReviewFilterConfig()
	cfg.MaxRules = 3
	// Diff contains the word "always" so pattern grep keeps every rule.
	got := FilterRules(rules, "the diff always mentions always", []string{"foo.cs"}, cfg)
	assert.Len(t, got, 3)
}

func TestFilterRules_UseAllRulesBypassesFilters(t *testing.T) {
	rules := []Rule{
		{ID: "ui-1", Category: "ui", Pattern: "component", Paths: []string{"**/*.tsx"}},
		{ID: "sec-1", Category: "security", Pattern: "csrf", Paths: nil},
	}
	cfg := DefaultReviewFilterConfig()
	cfg.UseAllRules = true
	// .cs-only diff that doesn't mention any pattern word; normal filtering
	// would drop both rules but UseAllRules must keep them.
	got := FilterRules(rules, "diff with nothing relevant", []string{"foo.cs"}, cfg)
	assert.Len(t, got, 2)
}

func TestFormatChecklistForDiff_FilteredCount(t *testing.T) {
	rf := &RulesFile{Rules: []Rule{
		{ID: "ui-1", Category: "ui", Pattern: "react component", Check: "ui-check"},
		{ID: "sec-1", Category: "security", Pattern: "csrf token", Check: "sec-check"},
	}}
	cfg := DefaultReviewFilterConfig()
	// .cs diff, pattern words "csrf" and "token" both ≥4 chars present.
	diff := "diff --git a/Service.cs b/Service.cs\n+if (!ValidateCsrfToken(req)) return;"
	out := rf.FormatChecklistForDiff(diff, []string{"Service.cs"}, cfg)
	assert.Contains(t, out, "sec-check")
	assert.NotContains(t, out, "ui-check")
}

func TestFormatChecklistForDiff_EmptyRulesReturnsEmpty(t *testing.T) {
	rf := &RulesFile{}
	assert.Equal(t, "", rf.FormatChecklistForDiff("diff", []string{"foo.cs"}, DefaultReviewFilterConfig()))
}

// TestFormatChecklistForDiff_AchievesTokenReduction is the acceptance assertion
// from the bead: ≥80% reduction in checklist size on a typical Munin-style
// .cs-only diff. The fixture mirrors the rule distribution seen in
// .forge/warden-rules.yaml at the time of writing — mostly other/ui/testing
// with a handful of security/error-handling/concurrency entries.
func TestFormatChecklistForDiff_AchievesTokenReduction(t *testing.T) {
	// Build 75 rules across the categories that show up in a real warden-rules
	// file. Only the 5 cs-relevant rules should pass — everything else is
	// dropped by either the category, path-glob, or pattern-grep filter.
	var rules []Rule
	for i := 0; i < 40; i++ {
		rules = append(rules, Rule{ID: "ui", Category: "ui", Pattern: "react component layout", Check: "ui rule"})
	}
	for i := 0; i < 25; i++ {
		rules = append(rules, Rule{ID: "tcase", Category: "testing", Pattern: "expect assertion", Check: "test rule"})
	}
	for i := 0; i < 5; i++ {
		rules = append(rules, Rule{ID: "style", Category: "style", Pattern: "indent spacing", Paths: []string{"**/*.tsx"}, Check: "style rule"})
	}
	for i := 0; i < 5; i++ {
		rules = append(rules, Rule{ID: "csrelevant", Category: "security", Pattern: "validate input authorisation", Check: "cs rule"})
	}
	rf := &RulesFile{Rules: rules}

	// .cs-only diff that mentions the security pattern words.
	diff := "diff --git a/Service.cs b/Service.cs\n+if (!Validate(input)) Authorisation.Reject();\n"
	changed := []string{"Service.cs"}

	all := rf.FormatChecklist()
	cfg := DefaultReviewFilterConfig()
	cfg.MaxRules = 100 // disable the cap so we measure the filter alone
	filtered := rf.FormatChecklistForDiff(diff, changed, cfg)

	if len(all) == 0 {
		t.Fatal("baseline checklist must not be empty")
	}
	reduction := 1.0 - float64(len(filtered))/float64(len(all))
	if reduction < 0.80 {
		t.Fatalf("filter must shrink the checklist by ≥80%%, got %.2f%% (all=%d, filtered=%d)",
			reduction*100, len(all), len(filtered))
	}
}

func ruleIDs(rs []Rule) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}
