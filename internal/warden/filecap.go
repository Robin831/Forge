package warden

import (
	"sort"
	"time"
)

// EvictOverCap trims the active rules file to at most max rules, returning the
// rules to keep (in their original file order) and archive entries for the ones
// evicted.
//
// A rules file grows without bound while a review reads a few dozen rules from
// it, so past some size every rule added is a rule competing for a slot it will
// almost never win. The ceiling is a hard count rather than a shorter staleness
// window because it is deterministic: an age threshold evicts nothing on a
// dormant anvil and everything on a busy one, while a count means what it says
// whatever the learning rate has been.
//
// What is evicted is the lowest-value rules, valued by the same two properties
// the review-time ranking reads that do not depend on a diff: how narrowly the
// rule's paths name a location, and how recently it was learned. Age alone
// would evict a precise old rule in favour of a vague new one. Nothing is
// evicted when max <= 0 (the disable) or the file already fits.
//
// Evicted rules are returned as ArchivedRule entries, not dropped: the archive
// file is the record of what the cap took, and the same rules can be restored
// by raising it.
func EvictOverCap(rules []Rule, max int, now time.Time) (active []Rule, archived []ArchivedRule) {
	if max <= 0 || len(rules) <= max {
		return rules, nil
	}

	type ranked struct {
		index int
		rule  Rule
		value float64
		added time.Time
	}
	scored := make([]ruleScore, len(rules))
	for i, r := range rules {
		added, _ := parseRuleAdded(r)
		scored[i] = ruleScore{rule: r, added: added, specificity: staticSpecificity(r)}
	}
	recencyScores(scored)

	order := make([]ranked, len(rules))
	for i := range rules {
		order[i] = ranked{
			index: i,
			rule:  rules[i],
			value: specificityWeight*scored[i].specificity + recencyWeight*scored[i].recency,
			added: scored[i].added,
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.value != b.value {
			return a.value > b.value
		}
		if !a.added.Equal(b.added) {
			return a.added.After(b.added)
		}
		return ruleTieKey(a.rule) < ruleTieKey(b.rule)
	})

	keep := make(map[int]bool, max)
	for _, r := range order[:max] {
		keep[r.index] = true
	}
	for i, r := range rules {
		if keep[i] {
			active = append(active, r)
			continue
		}
		archived = append(archived, ArchivedRule{
			Rule:          r,
			LastSeen:      now,
			ArchivedAt:    now,
			ArchiveReason: ArchiveReasonOverCap,
		})
	}
	return active, archived
}

// staticSpecificity is ruleSpecificity with no diff to match against: the
// narrowness of the rule's most specific glob, discounted by how many repo-wide
// globs it carries beside it. Eviction has no diff, so it reads what the rule
// claims about its own scope rather than what it matched.
func staticSpecificity(r Rule) float64 {
	if len(r.Paths) == 0 {
		return 0
	}
	var (
		best  float64
		broad int
	)
	for _, p := range r.Paths {
		if globWeight(p) == 0 {
			broad++
		}
		if n := globNarrowness(p); n > best {
			best = n
		}
	}
	return best / float64(1+broad)
}
