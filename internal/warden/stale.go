package warden

import (
	"time"
)

// staleAddedLayout is the format used for Rule.Added timestamps.
const staleAddedLayout = "2006-01-02"

// IsStale reports whether a rule should be archived due to inactivity.
//
// A rule is stale when its last ACTIVITY — the most recent of the last review
// it was emitted into, the last finding it contributed to, and the date it was
// added (Rule.LastActivityIn) — is older than archiveAfterDays. Reading the
// activity rather than Added alone is the whole point of the usage telemetry:
// before it, "learned in March" and "learned in March and emitted yesterday"
// were one value, so a rule the selection puts in front of a reviewer every
// week was retired for the age of its distillation session.
//
// The reading is zero-tolerant in one direction on purpose. A rule with no
// usage stamps — every rule on every file written before the telemetry existed
// — reads its Added date exactly as it always did, and a rule with no readable
// date at all is conservatively NOT stale: inactivity cannot be proven from a
// measurement nobody took. archiveAfterDays <= 0 also disables staleness
// (callers may use it to mean "never archive").
//
// The file ceiling (EvictOverCap) deliberately does not read this. Its recency
// component stays the learn date, because a ceiling that ranked on emissions
// would be self-reinforcing: a rule keeps its slot because it was emitted, and
// it was emitted because it had a slot.
func IsStale(rule Rule, archiveAfterDays int, now time.Time) bool {
	if archiveAfterDays <= 0 {
		return false
	}
	// Parsed in now's location so both sides of the subtraction share the same
	// timezone; mismatched locations (e.g. UTC vs local) would inject a fixed
	// offset and corrupt the "whole days" boundary.
	last := rule.LastActivityIn(now.Location())
	if last.IsZero() {
		return false
	}
	// Compare in whole days: the dates are date-only values parsed at
	// midnight, so truncate now to midnight as well to keep the boundary at
	// exact day counts (e.g. threshold=30 and last active 30 days ago is not
	// stale).
	nowDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return nowDay.Sub(last) > time.Duration(archiveAfterDays)*24*time.Hour
}

// ArchiveStale partitions rules into the active set (rules to keep) and a
// slice of ArchivedRule entries describing the stale rules that should be
// moved to the archive store with reason="stale". The ArchivedRule entries
// embed the original Rule unchanged and carry LastSeen and ArchivedAt set
// to now.
//
// Active rules are returned in their original order. ArchiveStale does not
// touch storage; the caller is responsible for persisting the archived
// entries to the archive store and replacing the active rules slice.
//
// When archiveAfterDays <= 0 (or no rules are stale), the input slice is
// returned as the active set and the archived slice is nil.
func ArchiveStale(rules []Rule, archiveAfterDays int, now time.Time) (active []Rule, archived []ArchivedRule) {
	if archiveAfterDays <= 0 || len(rules) == 0 {
		return rules, nil
	}
	for _, r := range rules {
		if IsStale(r, archiveAfterDays, now) {
			archived = append(archived, ArchivedRule{
				Rule:          r,
				LastSeen:      now,
				ArchivedAt:    now,
				ArchiveReason: ArchiveReasonStale,
			})
			continue
		}
		active = append(active, r)
	}
	if archived == nil {
		return rules, nil
	}
	return active, archived
}
