package warden

import (
	"time"
)

// staleAddedLayout is the format used for Rule.Added timestamps.
const staleAddedLayout = "2006-01-02"

// StaleReason names the one thing that decided a rule's fate in the staleness
// sweep. It exists because a boolean cannot: "this rule was kept" reads the
// same for a rule learned yesterday, a rule the reviewer saw last week, and a
// rule that is old, unused and kept only because the archive still points at
// it — and the third is the one an operator has an action for (re-run with
// AllowArchiveTerminus).
//
// Only that third reading is acted on today: ArchiveStale reads
// ReasonProtectedTerminus to fill StaleSweep.Protected, which is what every
// surface reporting a held rule renders. The other reasons are the sweep's
// own reasoning, observable in tests and available to a caller that wants to
// explain one rule's fate; nothing renders them, so this type is deliberately
// a plain named string with no formatting of its own.
type StaleReason string

const (
	// ReasonStalenessDisabled: ArchiveAfterDays <= 0, so the sweep did not run.
	ReasonStalenessDisabled StaleReason = "staleness-disabled"
	// ReasonNoAddedDate: one half of the test has no date to read. It is
	// returned from both halves, because both read the same absent value from
	// opposite ends: either the rule has no readable age anchor at all
	// (neither MergedAt nor Added), or it has an anchor from MergedAt while
	// carrying no readable Added and no usage stamps, so its AGE is
	// establishable and its ACTIVITY is not. One reason for the two because
	// the fate is one — a question the sweep cannot answer is resolved as not
	// stale — and the missing Added date is what produces it either way.
	ReasonNoAddedDate StaleReason = "no-added-date"
	// ReasonTooYoung: the rule has not aged past ArchiveAfterDays.
	ReasonTooYoung StaleReason = "too-young"
	// ReasonRecentActivity: the rule is old enough, but something has used it
	// inside the inactivity window — this is the reading the usage telemetry
	// exists for.
	ReasonRecentActivity StaleReason = "recent-activity"
	// ReasonProtectedTerminus: the rule is aged AND inactive, and was kept
	// anyway because archived rules were merged into it and it has no
	// successor of its own. See IsSupersessionTerminus.
	ReasonProtectedTerminus StaleReason = "protected-terminus"
	// ReasonAgedAndInactive is the only reason that retires a rule.
	ReasonAgedAndInactive StaleReason = "aged-and-inactive"
)

// StaleConfig is everything the staleness sweep decides from. It is a struct
// and not two ints because the two thresholds are only half of it: the
// supersession index and the override are inputs to the same decision, and a
// sweep that took them separately could be called with an index for one rule
// and none for the next.
type StaleConfig struct {
	// ArchiveAfterDays is the AGE threshold: how old a rule's Added date must
	// be before it is eligible at all. Zero or negative disables the sweep
	// entirely (callers may use it to mean "never archive").
	ArchiveAfterDays int

	// InactiveAfterDays is the INACTIVITY threshold: how long a rule must have
	// gone unused (Rule.LastActivityIn) before it is retired. A rule is stale
	// only when BOTH thresholds are crossed.
	//
	// Zero or negative falls back to ArchiveAfterDays rather than switching
	// the inactivity half off, and that choice is deliberate in both
	// directions. Falling back keeps every deployment that configures nothing
	// on exactly the behaviour it already had. Refusing to read it as an off
	// switch is the more important half: age alone is the predicate that
	// retires a rule the selection puts in front of a reviewer every week for
	// the age of its distillation session, so an "off" here would not be a
	// knob, it would be the defect. A deployment that wants a rule retired on
	// age alone can set InactiveAfterDays to 1.
	InactiveAfterDays int

	// SupersededBy is the reverse supersession index: a rule ID mapped to the
	// IDs of the archived rules that were merged INTO it (see
	// BuildSupersededByIndex). Nil means "no archive was read", which is not
	// the same claim as "nothing points at these rules" — but the two are
	// treated alike here on purpose, since the protection is a guard on top of
	// the age test rather than a condition for it, and a sweep that refused to
	// run without an archive would stop working on the first anvil that has
	// never archived anything.
	SupersededBy map[string][]string

	// AllowArchiveTerminus disables the terminus guard: an aged, inactive rule
	// is archived even when archived rules point at it. This is the `--force`
	// an operator reaches for once they have read WHICH rules the guard held
	// and decided the chain can end.
	AllowArchiveTerminus bool
}

// inactiveDays resolves the inactivity threshold, applying the documented
// fallback. It is one function so the predicate and any caller reporting the
// effective threshold cannot resolve it two ways.
func (c StaleConfig) inactiveDays() int {
	if c.InactiveAfterDays <= 0 {
		return c.ArchiveAfterDays
	}
	return c.InactiveAfterDays
}

// IsStale reports whether a rule should be archived, and the one signal that
// decided it.
//
// A rule is stale only when BOTH halves hold:
//
//	(a) it has AGED — now - Rule.ageAnchorIn > ArchiveAfterDays, where the
//	    anchor is Rule.MergedAt when consolidation stamped one and Rule.Added
//	    otherwise; and
//	(b) it is INACTIVE — now - Rule.LastActivityIn > InactiveAfterDays,
//	    where the activity is the most recent of the last review the rule was
//	    emitted into, the last finding it contributed to, and its Added date.
//
// The anchor is two dates and not one because a merged rule's Added belongs
// to the rules it folded rather than to itself. Consolidation exists to
// collapse old near-duplicates, so a survivor's members are routinely all
// past ArchiveAfterDays already — read on Added alone, the merge produces a
// rule that is stale the moment it is written, the next sweep archives it,
// and every member it stands in for is archived too, so the file loses the
// coverage with nothing left on it to say what went. MergedAt dates the rule
// itself; see Rule.ageAnchorIn.
//
// Two signals and not one because they answer different questions. Age says
// how long the rule has existed, which is a fact about the distillation
// session that produced it and about nothing else; inactivity says whether
// anything has used it since. Read on age alone — which is what this was — a
// rule the selection puts in front of a reviewer every week is retired for
// being old, which is the reported defect. Read on activity alone, a rule
// learned this morning and never yet selected reads as inactive on the day it
// was written. Requiring both means a rule leaves the file only when it is
// old AND nothing has wanted it, and the two windows can be set apart: a
// deployment can demand a rule be a quarter old and silent for a month.
//
// Every unanswerable question is resolved as NOT stale, in one direction on
// purpose. A rule with no readable anchor date has no age to test, and one
// with an anchor but no readable Added and no usage stamps has no activity to
// test (both ReasonNoAddedDate). A rule with no usage stamps — every rule on
// every file written before the telemetry existed — reads its Added date for
// both halves exactly as it always did. Retiring rules on a measurement
// nobody took is the one failure this sweep must not produce; keeping one
// costs a line in a file the ceiling bounds anyway.
//
// The last guard is supersession. A rule that other, archived rules were
// merged INTO is the terminus of a chain: archiving it retires the merged
// content of every rule behind it, and those rules are already archived, so
// nothing on the active file says what was lost. Such a rule is kept and
// reported (ReasonProtectedTerminus) unless AllowArchiveTerminus says the
// operator has looked. The guard is checked LAST, after both thresholds, so
// its reason names a rule the sweep would otherwise have taken — a young
// terminus rule reports ReasonTooYoung, which is why it was kept.
//
// The file ceiling (EvictOverCap) reads none of this, and deliberately: its
// recency component stays the learn date, because a ceiling ranking on
// emissions is self-reinforcing (a rule keeps its slot because it was
// emitted, and it was emitted because it had a slot). Nor does the ceiling
// honour the terminus guard — an eviction is a lost slot rather than a
// retirement, and raising the ceiling is enough to want the rule back.
func IsStale(rule Rule, cfg StaleConfig, now time.Time) (bool, StaleReason) {
	if cfg.ArchiveAfterDays <= 0 {
		return false, ReasonStalenessDisabled
	}

	// Parsed in now's location so both sides of every subtraction share the
	// same timezone; mismatched locations (e.g. UTC vs local) would inject a
	// fixed offset and corrupt the "whole days" boundary.
	loc := now.Location()
	// The dates are date-only values parsed at midnight, so truncate now to
	// midnight as well to keep every boundary at exact day counts (threshold
	// 30 and a date 30 days ago is not past the threshold).
	nowDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	anchor, ok := rule.ageAnchorIn(loc)
	if !ok {
		return false, ReasonNoAddedDate
	}
	if !olderThanDays(nowDay, anchor, cfg.ArchiveAfterDays) {
		return false, ReasonTooYoung
	}

	// LastActivityIn is the max of the three usage dates, of which Added is
	// one, so it is non-zero for every rule whose Added parses. It CAN be zero
	// here, because the anchor above may have come from MergedAt instead — a
	// merged rule whose Added is unreadable and which has never been emitted
	// has an age and no usage record at all. That is the sweep's unanswerable
	// question, and it resolves the way every other one does: not stale.
	last := rule.LastActivityIn(loc)
	if last.IsZero() {
		return false, ReasonNoAddedDate
	}
	if !olderThanDays(nowDay, last, cfg.inactiveDays()) {
		return false, ReasonRecentActivity
	}

	if !cfg.AllowArchiveTerminus && IsSupersessionTerminus(rule, cfg.SupersededBy) {
		return false, ReasonProtectedTerminus
	}

	return true, ReasonAgedAndInactive
}

// olderThanDays reports whether t is more than days whole days before nowDay.
// The boundary is exclusive: exactly days old is not past the threshold.
func olderThanDays(nowDay, t time.Time, days int) bool {
	return nowDay.Sub(t) > time.Duration(days)*24*time.Hour
}

// StaleSweep is what one pass of ArchiveStale produced.
//
// Protected is its own field rather than an absence, because a rule the
// terminus guard held is the one outcome a count of archived rules cannot
// state: "0 archived" reads identically for a file with nothing stale in it
// and a file whose every stale rule is holding a supersession chain, and only
// the second has something for an operator to decide. A caller that renders
// nothing for it turns the guard into a sweep that quietly stops working.
type StaleSweep struct {
	// Active is the rules to keep, in their original order.
	Active []Rule
	// Archived is the entries to persist to the archive store, reason "stale".
	Archived []ArchivedRule
	// Protected is the rules that were aged AND inactive but kept by the
	// terminus guard. They are in Active too — this names them, it does not
	// remove them.
	Protected []Rule
}

// ArchiveStale partitions rules into the set to keep and the ArchivedRule
// entries describing the stale ones, and names the rules the terminus guard
// held back. The ArchivedRule entries embed the original Rule unchanged and
// carry LastSeen and ArchivedAt set to now.
//
// ArchiveStale does not touch storage: the caller persists the archived
// entries and replaces the active rules slice. When nothing is stale the
// input slice is returned as the active set unchanged, so a caller can
// compare identity to know the pass was a no-op.
//
// The supersession index in cfg is built once by the caller (see
// BuildSupersededByIndex) rather than per rule here, because it is a read of
// the whole archive file and a sweep is O(rules).
func ArchiveStale(rules []Rule, cfg StaleConfig, now time.Time) StaleSweep {
	if cfg.ArchiveAfterDays <= 0 || len(rules) == 0 {
		return StaleSweep{Active: rules}
	}
	var sweep StaleSweep
	for _, r := range rules {
		stale, reason := IsStale(r, cfg, now)
		if stale {
			sweep.Archived = append(sweep.Archived, ArchivedRule{
				Rule:          r,
				LastSeen:      now,
				ArchivedAt:    now,
				ArchiveReason: ArchiveReasonStale,
			})
			continue
		}
		if reason == ReasonProtectedTerminus {
			sweep.Protected = append(sweep.Protected, r)
		}
		sweep.Active = append(sweep.Active, r)
	}
	if sweep.Archived == nil {
		sweep.Active = rules
	}
	return sweep
}
