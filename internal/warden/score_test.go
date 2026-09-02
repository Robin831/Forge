package warden

import (
	"bytes"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syntheticRules builds a rules file in the shape a real one has: appended in
// age order, most rules scoped repo-wide, a few scoped narrowly.
func syntheticRules(n int) []Rule {
	rules := make([]Rule, 0, n)
	day := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		paths := []string{"**/*.go"}
		if i%7 == 0 {
			paths = []string{"internal/warden/*.go"}
		}
		rules = append(rules, Rule{
			ID:       "rule-" + string(rune('a'+i%26)) + strings.Repeat("x", i%5) + itoa(i),
			Category: "other",
			Pattern:  "worker pipeline dispatch handler for stage number " + itoa(i),
			Check:    "check number " + itoa(i),
			Added:    day.AddDate(0, 0, i).Format(staleAddedLayout),
			Paths:    paths,
		})
	}
	return rules
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestSelectionIsIndependentOfFileOrder is the direct guard on the bug: rules
// are appended as they are learned, so file order is age order, and a cap that
// truncated it could only ever emit the oldest rules. Shuffling the file must
// not move a single entry of the emitted checklist.
// ruleIDs (filter_test.go) returns the emitted set in emission ORDER, which is
// what these compare: an assertion on the set alone would pass while the
// checklist's order still followed the file.
func TestSelectionIsIndependentOfFileOrder(t *testing.T) {
	rules := syntheticRules(200)
	diff := "worker pipeline dispatch handler for stage number 42 and 137 in internal/warden"
	changed := []string{"internal/warden/filter.go", "internal/daemon/daemon.go"}
	cfg := DefaultReviewFilterConfig()

	want := ruleIDs(FilterRules(rules, diff, changed, cfg))
	require.Len(t, want, cfg.MaxRules)

	for seed := int64(1); seed <= 5; seed++ {
		shuffled := append([]Rule(nil), rules...)
		rng := rand.New(rand.NewSource(seed))
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got := ruleIDs(FilterRules(shuffled, diff, changed, cfg))
		assert.Equal(t, want, got, "emitted set and order must not depend on file order (seed %d)", seed)
	}
}

// TestNewRulesReachAReview is the second acceptance property: under truncation
// the tail of the file was unreachable however relevant it was.
func TestNewRulesReachAReview(t *testing.T) {
	var rules []Rule
	for i := 0; i < 60; i++ {
		rules = append(rules, Rule{
			ID:       "old-" + itoa(i),
			Category: "other",
			Pattern:  "generic handler value method returning something",
			Check:    "old check",
			Added:    "2026-03-01",
			Paths:    []string{"**/*"},
		})
	}
	newest := Rule{
		ID:       "learned-in-august",
		Category: "other",
		Pattern:  "preview supervisor restart budget exhausted",
		Check:    "new check",
		Added:    "2026-08-20",
		Paths:    []string{"internal/kiln/restart.go"},
	}
	rules = append(rules, newest)

	got := FilterRules(rules, "preview supervisor restart budget exhausted handler value method", []string{"internal/kiln/restart.go"}, DefaultReviewFilterConfig())
	assert.Contains(t, ruleIDs(got), "learned-in-august")
	// It is not merely present but preferred: it is the only rule naming the
	// file the diff touched.
	assert.Equal(t, "learned-in-august", got[0].ID)
}

func TestGlobNarrownessOrdersByLocation(t *testing.T) {
	assert.Equal(t, 0.0, globWeight("**/*"))
	assert.Less(t, globNarrowness("**/*"), globNarrowness("**/*.go"))
	assert.Less(t, globNarrowness("**/*.go"), globNarrowness("api/**/*.cs"))
	assert.Less(t, globNarrowness("api/**/*.cs"), globNarrowness("internal/warden/filter.go"))
}

func TestRuleSpecificityDiscountsShotgunGlobs(t *testing.T) {
	narrow := Rule{Paths: []string{"internal/warden/*.go"}}
	shotgun := Rule{Paths: []string{"internal/warden/*.go", "**/*", "**/*", "**/*"}}
	changed := []string{"internal/warden/filter.go"}
	assert.Greater(t, ruleSpecificity(narrow, changed), ruleSpecificity(shotgun, changed))
	// A rule with no paths claims no scope, so it scores as the broadest.
	assert.Equal(t, 0.0, ruleSpecificity(Rule{}, changed))
}

func TestPatternRelevanceIsAShareNotACount(t *testing.T) {
	// Three of three beats four of twenty: a terse pattern fully present in
	// the diff is more on-topic than a verbose one that landed a few words.
	assert.Greater(t, patternRelevance(3, 3), patternRelevance(4, 20))
	// No ≥4-char words is no evidence either way.
	assert.Equal(t, 0.5, patternRelevance(0, 0))
}

// TestReviewLogsTheRuleFunnel covers the acceptance criterion that a review
// says how many rules matched and how many were emitted. The funnel is
// invisible in the checklist itself, which reads the same either way.
func TestReviewLogsTheRuleFunnel(t *testing.T) {
	anvil := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(anvil, ".forge"), 0o755))
	require.NoError(t, SaveRules(anvil, &RulesFile{Rules: syntheticRules(120)}))

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	diff := "diff --git a/internal/warden/filter.go b/internal/warden/filter.go\n" +
		"+++ b/internal/warden/filter.go\n" +
		"+worker pipeline dispatch handler for stage number 3\n"
	buildReviewPrompt("Forge-n9aq", "title", "spec", diff, anvil, "")

	line := buf.String()
	assert.Contains(t, line, "rules funnel:")
	assert.Contains(t, line, "120 on file")
	assert.Contains(t, line, "matched")
	assert.Contains(t, line, "emitted")
}

func TestFilterStatsLine(t *testing.T) {
	s := FilterStats{Total: 1793, PathMatched: 949, CategoryMatched: 700, Matched: 431, Emitted: 30, Cap: 30}
	assert.Equal(t,
		"rules funnel: 1793 on file, 949 after paths, 700 after category, 431 matched, 30 emitted (cap 30)",
		s.Line())
	assert.Contains(t, FilterStats{Total: 5, Matched: 5, Emitted: 5}.Line(), "(cap none)")
}

func TestEvictOverCapKeepsTheMostValuable(t *testing.T) {
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	rules := []Rule{
		{ID: "old-broad", Added: "2026-03-01", Paths: []string{"**/*"}},
		{ID: "old-narrow", Added: "2026-03-01", Paths: []string{"internal/warden/filter.go"}},
		{ID: "new-broad", Added: "2026-08-01", Paths: []string{"**/*"}},
	}
	active, archived := EvictOverCap(rules, 2, now)
	require.Len(t, active, 2)
	require.Len(t, archived, 1)
	assert.Equal(t, "old-broad", archived[0].ID)
	assert.Equal(t, ArchiveReasonOverCap, archived[0].ArchiveReason)
	// Survivors keep file order, so a cap never reorders the file itself.
	assert.Equal(t, []string{"old-narrow", "new-broad"}, ruleIDs(active))
}

func TestEvictOverCapNoOpUnderCeiling(t *testing.T) {
	rules := syntheticRules(10)
	active, archived := EvictOverCap(rules, 10, time.Now())
	assert.Len(t, active, 10)
	assert.Empty(t, archived)

	// A non-positive ceiling is the disable.
	active, archived = EvictOverCap(rules, 0, time.Now())
	assert.Len(t, active, 10)
	assert.Empty(t, archived)
	active, archived = EvictOverCap(rules, -1, time.Now())
	assert.Len(t, active, 10)
	assert.Empty(t, archived)
}

func TestEvictOverCapIsIndependentOfFileOrder(t *testing.T) {
	rules := syntheticRules(120)
	want, _ := EvictOverCap(rules, 40, time.Now())
	wantSet := map[string]bool{}
	for _, r := range want {
		wantSet[r.ID] = true
	}
	shuffled := append([]Rule(nil), rules...)
	rng := rand.New(rand.NewSource(7))
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	got, _ := EvictOverCap(shuffled, 40, time.Now())
	require.Len(t, got, 40)
	for _, r := range got {
		assert.True(t, wantSet[r.ID], "%s kept only under one ordering", r.ID)
	}
}

// TestEvictOverCapPassesThroughUnchangedUnderTheCeiling pins the nil-vs-empty
// contract: nothing evicted returns the caller's own slice (a nil stays nil),
// and an eviction returns a non-nil slice of exactly max rules. Callers assign
// the result straight onto RulesFile.Rules, so the two cases have to be
// distinguishable.
func TestEvictOverCapPassesThroughUnchangedUnderTheCeiling(t *testing.T) {
	active, archived := EvictOverCap(nil, 10, time.Now())
	assert.Nil(t, active)
	assert.Nil(t, archived)

	active, _ = EvictOverCap([]Rule{}, 10, time.Now())
	assert.NotNil(t, active)
	assert.Empty(t, active)

	active, archived = EvictOverCap(syntheticRules(5), 3, time.Now())
	require.NotNil(t, active)
	assert.Len(t, active, 3)
	assert.Len(t, archived, 2)
}

// TestFilterStatsLineReportsTheBypass: with UseAllRules the three survivor
// counts are just the file size, so the ordinary funnel wording would claim
// three filters ran and kept everything.
func TestFilterStatsLineReportsTheBypass(t *testing.T) {
	s := FilterStats{Total: 1793, PathMatched: 1793, CategoryMatched: 1793, Matched: 1793, Emitted: 30, Cap: 30, Bypassed: true}
	line := s.Line()
	assert.Contains(t, line, "1793 on file")
	assert.Contains(t, line, "filters bypassed (use_all_rules)")
	assert.Contains(t, line, "1793 ranked")
	assert.Contains(t, line, "30 emitted (cap 30)")
	assert.NotContains(t, line, "after paths")
}

// TestUseAllRulesStillRanks: bypassing the filters must not restore the file
// order truncation. Every rule is a candidate, and the cap still takes the
// best of them by rank — including the pattern-relevance component, which is a
// ranking input and not the filter that was switched off.
func TestUseAllRulesStillRanks(t *testing.T) {
	diff := "+++ b/internal/warden/filter.go\n+ selection ranking candidates emitted\n"
	changed := []string{"internal/warden/filter.go"}
	rules := []Rule{
		// Oldest and first in file order, but repo-wide and off-topic: file
		// order would emit it, rank must not.
		{ID: "old-broad", Category: "other", Added: "2026-01-01",
			Pattern: "unrelated vocabulary about dockerfiles", Paths: []string{"**/*"}},
		{ID: "narrow-relevant", Category: "other", Added: "2026-08-01",
			Pattern: "selection ranking candidates", Paths: []string{"internal/warden/filter.go"}},
	}
	cfg := ReviewFilterConfig{MaxRules: 1, UseAllRules: true}

	got, stats := FilterRulesWithStats(rules, diff, changed, cfg)
	require.Len(t, got, 1)
	assert.Equal(t, "narrow-relevant", got[0].ID)
	assert.True(t, stats.Bypassed)
	assert.Equal(t, len(rules), stats.Matched, "every rule on file is a candidate under UseAllRules")

	// And the answer does not depend on which order the file held them in.
	reversed := []Rule{rules[1], rules[0]}
	got2, _ := FilterRulesWithStats(reversed, diff, changed, cfg)
	require.Len(t, got2, 1)
	assert.Equal(t, "narrow-relevant", got2[0].ID)
}
