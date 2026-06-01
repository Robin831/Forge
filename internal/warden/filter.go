package warden

import (
	"path/filepath"
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
	// UseAllRules bypasses all three filters and applies only the MaxRules cap.
	UseAllRules bool
	// FilterPathGlob enables filtering by Rule.Paths against the changed files.
	FilterPathGlob bool
	// FilterCategory enables filtering by Rule.Category against the in-code
	// extension → category map (see categoriesForFile).
	FilterCategory bool
	// FilterPatternGrep enables filtering by ≥4-char words extracted from
	// Rule.Pattern that must appear as substrings in the diff text.
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
		strings.HasSuffix(lower, ".ts"),
		strings.HasSuffix(lower, ".css"):
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

// patternGrep returns true when at least one ≥4-char word from pattern appears
// as a substring of diffLower. When the pattern carries no ≥4-char words the
// rule is kept (nothing to filter on).
func patternGrep(pattern, diffLower string) bool {
	words := extractDiffWords(pattern)
	if len(words) == 0 {
		return true
	}
	for _, w := range words {
		if strings.Contains(diffLower, w) {
			return true
		}
	}
	return false
}

// FilterRules returns the subset of rules to include in a review-time
// checklist, applying the path-glob, category, and pattern-grep filters (in
// that order, each gated by cfg) and then capping the result to cfg.MaxRules.
// When cfg.UseAllRules is true the three filters are skipped and only the cap
// applies.
func FilterRules(rules []Rule, diff string, changedFiles []string, cfg ReviewFilterConfig) []Rule {
	if cfg.UseAllRules {
		return capRules(rules, cfg.MaxRules)
	}

	diffLower := strings.ToLower(diff)
	categorySet := aggregateCategories(changedFiles)

	out := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if cfg.FilterPathGlob && len(r.Paths) > 0 {
			if len(changedFiles) == 0 || !matchPathGlob(r.Paths, changedFiles) {
				continue
			}
		}
		if cfg.FilterCategory && canonicalCategories[r.Category] {
			if !categorySet[r.Category] {
				continue
			}
		}
		if cfg.FilterPatternGrep && !patternGrep(r.Pattern, diffLower) {
			continue
		}
		out = append(out, r)
	}

	return capRules(out, cfg.MaxRules)
}

func capRules(rules []Rule, max int) []Rule {
	if max <= 0 || len(rules) <= max {
		return rules
	}
	return rules[:max]
}
