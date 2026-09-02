package warden

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/bmatcuk/doublestar/v4"
)

// ReviewFilterConfig controls how Rule lists are filtered for the review-time
// prompt. The Forge daemon reads these from settings.warden in forge.yaml and
// assigns ActiveFilterConfig at startup.
type ReviewFilterConfig struct {
	// MaxRules caps the number of rules emitted in the checklist. When zero or
	// negative, no cap is applied.
	MaxRules int
	// UseAllRules bypasses all three filters, leaving every rule on file a
	// candidate for the MaxRules cap.
	//
	// It does not bypass the RANKING. A cap has to choose which rules reach
	// the checklist, and the only orders available are a ranked one and file
	// order — which is age order, and taking the head of it is the truncation
	// this package exists to have stopped doing. So the candidates are still
	// scored against the diff here, pattern relevance included: a filter says
	// which rules MAY appear and a score says which of them appear FIRST, and
	// turning the first off is not a statement about the second. FilterStats
	// reports the bypass so the funnel line does not read as if three filters
	// had run and kept everything.
	UseAllRules bool
	// FilterPathGlob enables filtering by Rule.Paths against the changed files.
	FilterPathGlob bool
	// FilterCategory enables filtering by Rule.Category against the in-code
	// extension → category map (see categoriesForFile).
	FilterCategory bool
	// FilterPatternGrep enables filtering by ≥4-char words extracted from
	// Rule.Pattern that must appear as substrings in the diff text (see
	// minPatternWordHits).
	//
	// As with UseAllRules, this gates the exclusion and not the ranking: the
	// word hits are counted for every candidate either way and feed
	// patternRelevance, because they are the strongest topical signal a cap
	// has to choose on and they are cheapest to compute exactly once.
	FilterPatternGrep bool
}

// DefaultReviewFilterConfig returns the default review-time filter
// configuration: all three filters enabled, MaxRules=30, UseAllRules=false.
func DefaultReviewFilterConfig() ReviewFilterConfig {
	return ReviewFilterConfig{
		MaxRules:          30,
		UseAllRules:       false,
		FilterPathGlob:    true,
		FilterCategory:    true,
		FilterPatternGrep: true,
	}
}

// activeFilterConfig holds the active review-time configuration behind an
// atomic.Value so concurrent reads (review-time) and writes (hot-reload
// callback) are race-free without a mutex.
var activeFilterConfig atomic.Value

func init() {
	activeFilterConfig.Store(DefaultReviewFilterConfig())
}

// GetActiveFilterConfig returns the current active review filter configuration.
// Safe to call from any goroutine.
func GetActiveFilterConfig() ReviewFilterConfig {
	return activeFilterConfig.Load().(ReviewFilterConfig)
}

// SetActiveFilterConfig replaces the active review filter configuration.
// Safe to call from any goroutine, including the hot-reload callback.
func SetActiveFilterConfig(cfg ReviewFilterConfig) {
	activeFilterConfig.Store(cfg)
}

// canonicalCategories is the set of categories the category filter recognises.
// Rules with a category outside this set always pass the filter (treated as
// "always relevant") so domain-specific learned categories like
// "documentation" or "backward-compatibility" never silently disappear.
var canonicalCategories = map[string]bool{
	"ui":             true,
	"style":          true,
	"testing":        true,
	"error-handling": true,
	"security":       true,
	"concurrency":    true,
	"performance":    true,
	"other":          true,
}

// categoriesForFile returns the canonical categories considered relevant for
// the given file path. The mapping is intentionally coarse; expanding it
// requires editing this function.
func categoriesForFile(path string) map[string]bool {
	lower := strings.ToLower(path)
	base := strings.ToLower(filepath.Base(path))

	if strings.HasSuffix(lower, "_test.tsx") || strings.HasSuffix(lower, "tests.cs") {
		return map[string]bool{"testing": true, "other": true}
	}
	switch {
	case strings.HasSuffix(lower, ".cs"):
		return map[string]bool{
			"error-handling": true, "security": true, "concurrency": true,
			"performance": true, "style": true, "other": true,
		}
	case strings.HasSuffix(lower, ".tsx"),
		strings.HasSuffix(lower, ".ts"):
		// TypeScript is a full application language, not just a styling
		// surface: a frontend-scoped rule categorised security, performance,
		// error-handling, concurrency or testing must reach a frontend-only
		// diff, so every canonical category is relevant here.
		return allCanonicalCategories()
	case strings.HasSuffix(lower, ".css"):
		return map[string]bool{"ui": true, "style": true, "other": true}
	case strings.HasSuffix(lower, ".yaml"),
		strings.HasSuffix(lower, ".yml"),
		strings.HasSuffix(lower, ".ps1"),
		base == "dockerfile",
		strings.HasPrefix(base, "dockerfile."),
		strings.HasSuffix(base, ".dockerfile"):
		return map[string]bool{"other": true, "security": true}
	}
	return allCanonicalCategories()
}

func allCanonicalCategories() map[string]bool {
	out := make(map[string]bool, len(canonicalCategories))
	for k := range canonicalCategories {
		out[k] = true
	}
	return out
}

// aggregateCategories computes the union of category sets across all changed
// files. When changedFiles is empty the fallback (all canonical categories)
// applies so an unknown diff doesn't accidentally filter every rule.
func aggregateCategories(changedFiles []string) map[string]bool {
	if len(changedFiles) == 0 {
		return allCanonicalCategories()
	}
	out := make(map[string]bool)
	for _, f := range changedFiles {
		for c := range categoriesForFile(f) {
			out[c] = true
		}
	}
	return out
}

// matchPathGlob returns true when any changed file matches any of the given
// doublestar patterns. Invalid patterns are skipped silently (never match).
func matchPathGlob(patterns, changedFiles []string) bool {
	for _, p := range patterns {
		for _, f := range changedFiles {
			ok, err := doublestar.Match(p, f)
			if err != nil {
				continue
			}
			if ok {
				return true
			}
		}
	}
	return false
}

// extractDiffWords returns the distinct ≥4-char alphanumeric words from text,
// lowercased and in first-seen order.
func extractDiffWords(text string) []string {
	if text == "" {
		return nil
	}
	lower := strings.ToLower(text)
	var (
		seen = make(map[string]bool)
		out  []string
		cur  strings.Builder
	)
	flush := func() {
		if cur.Len() >= 4 {
			s := cur.String()
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
		cur.Reset()
	}
	for _, r := range lower {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// minPatternWordHits is how many distinct ≥4-char pattern words must appear in
// the diff before the pattern filter keeps a rule. One was a no-op: a pattern
// is ordinary English, so a single common word ("value", "method", "return")
// matches nearly every diff and the filter passed nearly every rule, leaving
// the cap to perform the whole of the selection.
//
// A pattern carrying fewer than this many ≥4-char words is held to what it
// has (see patternGrepPasses) rather than excluded forever.
const minPatternWordHits = 2

// patternWordHits returns how many of the distinct ≥4-char words in pattern
// appear as substrings of diffLower, alongside how many such words the pattern
// carries at all. The two together are what both the filter (a threshold) and
// the ranking (a share) need, computed once per rule.
func patternWordHits(pattern, diffLower string) (hits, words int) {
	ws := extractDiffWords(pattern)
	for _, w := range ws {
		if strings.Contains(diffLower, w) {
			hits++
		}
	}
	return hits, len(ws)
}

// patternGrepPasses reports whether a rule's pattern is close enough to the
// diff to keep. It requires minPatternWordHits distinct words, except for a
// pattern that does not have that many, which is held to all of the words it
// does carry — a rule whose pattern is one word is not one that may never fire
// again. A pattern with no ≥4-char words is kept: there is nothing to filter
// on.
func patternGrepPasses(hits, words int) bool {
	if words == 0 {
		return true
	}
	need := minPatternWordHits
	if words < need {
		need = words
	}
	return hits >= need
}

// FilterStats reports how a review's rule set narrowed at each stage. It exists
// because the narrowing was invisible: a checklist of 30 rules reads the same
// whether 30 candidates survived the filters or 949 did and 919 were discarded
// by a cap that truncated in file order.
type FilterStats struct {
	// Total is the number of rules on file.
	Total int
	// PathMatched, CategoryMatched and Matched are the survivor counts after
	// the path-glob, category and pattern filters respectively; Matched is
	// therefore the candidate set the ranking chose from.
	PathMatched     int
	CategoryMatched int
	Matched         int
	// Emitted is the number of rules that reached the checklist, and Cap the
	// limit that decided it (0 when uncapped).
	Emitted int
	Cap     int
	// Bypassed records that UseAllRules was set, so the three survivor counts
	// above are all just the file size rather than three filters that kept
	// everything. Without it the funnel line reads identically in the two
	// cases, which is the kind of silence this whole struct exists to end.
	Bypassed bool
}

// Line renders the funnel as one log sentence.
func (s FilterStats) Line() string {
	capText := "none"
	if s.Cap > 0 {
		capText = strconv.Itoa(s.Cap)
	}
	if s.Bypassed {
		return fmt.Sprintf("rules funnel: %d on file, filters bypassed (use_all_rules), %d ranked, %d emitted (cap %s)",
			s.Total, s.Matched, s.Emitted, capText)
	}
	return fmt.Sprintf("rules funnel: %d on file, %d after paths, %d after category, %d matched, %d emitted (cap %s)",
		s.Total, s.PathMatched, s.CategoryMatched, s.Matched, s.Emitted, capText)
}

// FilterRules returns the rules to include in a review-time checklist: the
// subset matching the diff (path-glob, category and pattern filters, in that
// order, each gated by cfg), ranked, and cut to cfg.MaxRules. When
// cfg.UseAllRules is true the three filters are skipped and every rule on file
// is ranked against the diff instead — the cap still has to choose, and it
// chooses by rank there exactly as it does here.
func FilterRules(rules []Rule, diff string, changedFiles []string, cfg ReviewFilterConfig) []Rule {
	out, _ := FilterRulesWithStats(rules, diff, changedFiles, cfg)
	return out
}

// FilterRulesWithStats is FilterRules plus the per-stage funnel counts.
//
// The cut to cfg.MaxRules is a SELECTION and not a truncation, which is the
// whole point of this function: rules are appended to the file as they are
// learned, so file order is age order, and taking the first N of it returned
// the oldest surviving rules and only ever those. Measured on one anvil's
// 1793-rule file against its own PR history, 61 rules were reachable across
// every PR ever opened, and nothing learned in the preceding four months could
// reach a review at all.
func FilterRulesWithStats(rules []Rule, diff string, changedFiles []string, cfg ReviewFilterConfig) ([]Rule, FilterStats) {
	stats := FilterStats{Total: len(rules), Cap: cfg.MaxRules, Bypassed: cfg.UseAllRules}
	diffLower := strings.ToLower(diff)

	var (
		candidates []Rule
		hits       []int
		words      []int
	)
	keep := func(r Rule, h, w int) {
		candidates = append(candidates, r)
		hits = append(hits, h)
		words = append(words, w)
	}

	if cfg.UseAllRules {
		stats.PathMatched = len(rules)
		stats.CategoryMatched = len(rules)
		for _, r := range rules {
			h, w := patternWordHits(r.Pattern, diffLower)
			keep(r, h, w)
		}
	} else {
		categorySet := aggregateCategories(changedFiles)
		for _, r := range rules {
			if cfg.FilterPathGlob && len(r.Paths) > 0 {
				if len(changedFiles) == 0 || !matchPathGlob(r.Paths, changedFiles) {
					continue
				}
			}
			stats.PathMatched++
			if cfg.FilterCategory && canonicalCategories[r.Category] {
				if !categorySet[r.Category] {
					continue
				}
			}
			stats.CategoryMatched++
			h, w := patternWordHits(r.Pattern, diffLower)
			if cfg.FilterPatternGrep && !patternGrepPasses(h, w) {
				continue
			}
			keep(r, h, w)
		}
	}
	stats.Matched = len(candidates)

	selected := selectRules(scoreCandidates(candidates, changedFiles, hits, words), cfg.MaxRules)
	stats.Emitted = len(selected)
	return selected, stats
}
