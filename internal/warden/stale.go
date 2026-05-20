package warden

import (
	"time"
)

// staleAddedLayout is the format used for Rule.Added timestamps.
const staleAddedLayout = "2006-01-02"

// IsStale reports whether a rule should be archived due to inactivity.
//
// A rule is considered stale when BOTH of the following hold:
//  1. rule.Added is older than archiveAfterDays.
//  2. No source entry has been recorded in the last archiveAfterDays/2 days.
//
// Rule source entries currently carry no per-entry timestamp, so the rule's
// Added date is the only timestamp tracking when this rule was last touched.
// Until per-source timestamps are introduced, Added is treated as the most
// recent source activity for purposes of the half-window check — the second
// condition therefore reduces to "Added is older than archiveAfterDays/2".
//
// Rules with no parseable Added date are conservatively treated as not
// stale: we cannot prove inactivity without a timestamp. archiveAfterDays
// <= 0 also disables staleness (callers may use it to mean "never archive").
func IsStale(rule Rule, archiveAfterDays int, now time.Time) bool {
	if archiveAfterDays <= 0 {
		return false
	}
	if rule.Added == "" {
		return false
	}
	added, err := time.Parse(staleAddedLayout, rule.Added)
	if err != nil {
		return false
	}
	// Compare in whole days: Added is a date-only value parsed at midnight,
	// so truncate now to midnight as well to keep the boundary at exact day
	// counts (e.g. threshold=30 and added 30 days ago is not stale).
	nowDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	elapsed := nowDay.Sub(added)
	fullWindow := time.Duration(archiveAfterDays) * 24 * time.Hour
	if elapsed <= fullWindow {
		return false
	}
	halfWindow := fullWindow / 2
	return elapsed > halfWindow
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
