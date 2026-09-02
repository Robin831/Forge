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
// evicted when max <= 0 (the disable) or the file already fits, and in that
// case rules is returned unchanged — a nil slice stays nil rather than becoming
// an empty one, so a caller can still tell "not run" from "ran and kept none".
// When it does evict, active holds exactly max rules and is never nil.
//
// Evicted rules are returned as ArchivedRule entries, not dropped: the archive
// file is the record of what the cap took, and the same rules can be restored
// by raising it.
func EvictOverCap(rules []Rule, max int, now time.Time) (active []Rule, archived []ArchivedRule) {
	if max <= 0 || len(rules) <= max {
		return rules, nil
	}

	// The ranking reuses ruleScore, staticSpecificity (the review-time
	// specificity under a nil scope predicate) and higherRanked (the ordering
	// selectRules uses) rather than arithmetic of its own, so the two
	// components eviction reads are the same fields, computed the same way and
	// ordered by the same comparator, as the ones the review-time selection
	// reads. Positions are carried
	// alongside because the kept rules are returned in FILE order: the file is
	// a record, and rewriting its order on every flush would churn the diff of
	// a file nothing reads sequentially.
	scored := make([]ruleScore, len(rules))
	for i, r := range rules {
		added, _ := parseRuleAdded(r)
		scored[i] = ruleScore{rule: r, added: added, specificity: staticSpecificity(r)}
	}
	recencyScores(scored)

	order := make([]int, len(rules))
	for i := range rules {
		order[i] = i
		scored[i].total = specificityWeight*scored[i].specificity + recencyWeight*scored[i].recency
	}
	sort.SliceStable(order, func(i, j int) bool {
		return higherRanked(scored[order[i]], scored[order[j]])
	})

	keep := make(map[int]bool, max)
	for _, idx := range order[:max] {
		keep[idx] = true
	}
	active = make([]Rule, 0, max)
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

// staticSpecificity is the review-time specificity with no diff to match
// against — the same function under a nil scope predicate, so every glob is
// eligible. Eviction has no diff, so it reads what the rule claims about its
// own scope rather than what it matched; where every glob does match, the two
// agree by construction, which is what
// TestStaticSpecificityMatchesRuleSpecificity pins.
func staticSpecificity(r Rule) float64 {
	return specificity(r, nil)
}
