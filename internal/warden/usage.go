package warden

import (
	"strings"
	"time"
)

// This file is the rule-usage telemetry: when a rule was last put in front of
// a reviewer, how often it has been, and when it last contributed to a
// finding. It is the evidence a rule is alive, and it exists because nothing
// recorded any: Rule.Added is the only timestamp a rule has ever carried, so
// "learned in March" and "learned in March and emitted yesterday" are one
// value to every consumer — which is why IsStale reduces to an age test on
// Added and says so in its own doc comment.
//
// Every field is optional and every reader is zero-tolerant, in one direction
// on purpose: an absent timestamp means the rule has never been OBSERVED, not
// that it was observed a long time ago. A consumer that treats the two alike
// retires the rules learned before the telemetry existed on the strength of a
// measurement that was never taken.

// usageDateLayout is the layout the usage timestamps are written in. It is
// staleAddedLayout — the same one Rule.Added uses — so the three dates on a
// rule read and parse alike, and so LastActivity can compare them without
// converting between two representations.
const usageDateLayout = staleAddedLayout

// parseUsageDate parses one of the rule's date fields, reporting whether it
// was readable. An empty or malformed value yields the zero time, which every
// caller here treats as "no evidence" rather than as the oldest possible date.
func parseUsageDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(usageDateLayout, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// formatUsageDate renders a timestamp for storage in a usage field. It is UTC
// so that two workers on hosts in different zones cannot record the same
// review as two different days.
func formatUsageDate(t time.Time) string {
	return t.UTC().Format(usageDateLayout)
}

// LastActivity reports the most recent evidence that the rule is alive: the
// last time it was emitted into a review, the last time it contributed to a
// finding, or — failing both — when it was added.
//
// The zero time is returned when none of the three is readable, and it means
// "unknown", never "ancient": a rule whose Added date does not parse has no
// timestamp at all, and a consumer that reads the zero time as a date in 1970
// would retire it for having no record rather than for having no use.
func (r *Rule) LastActivity() time.Time {
	if r == nil {
		return time.Time{}
	}
	var best time.Time
	for _, field := range []string{r.LastEmitted, r.LastFinding, r.Added} {
		if t, ok := parseUsageDate(field); ok && t.After(best) {
			best = t
		}
	}
	return best
}

// MarkEmitted records that the rule was rendered into a review at now.
//
// now is a parameter rather than a time.Now() call inside so that a caller
// stamping a whole checklist gives every rule in it the same timestamp, and so
// that a test can assert on the value rather than on the clock.
func (r *Rule) MarkEmitted(now time.Time) {
	if r == nil {
		return
	}
	r.LastEmitted = formatUsageDate(now)
	r.EmitCount++
}

// MarkFinding records that the rule contributed to an accepted finding at now.
//
// Nothing calls this yet, and that is a property of the review result rather
// than an omission here: warden.Issue carries a file, a line, a severity and a
// message, so a review that flags something a rule warned about produces no
// record of WHICH rule it was. The field is carried, persisted and read by
// LastActivity so that the attribution can be added without a second migration
// of every anvil's rules file.
func (r *Rule) MarkFinding(now time.Time) {
	if r == nil {
		return
	}
	r.LastFinding = formatUsageDate(now)
}

// MarkEmittedAt stamps the rules at the given positions in the file as having
// been emitted at now, and reports how many it stamped.
//
// It takes POSITIONS and not IDs, because a rule's ID is written by whichever
// distillation session produced it and names one rule only by luck — the file
// routinely holds two rules under one ID (see AddRuleDistinct), and stamping
// by ID would credit an emission to a rule that was never selected. That is
// the same mistake applyClusters made when it removed by ID, and here it would
// feed the staleness sweep a rule that looks alive and never fires.
//
// Out-of-range positions are ignored rather than panicking: the caller derives
// them from a filter pass over this same slice, so one that does not fit is a
// bug in that derivation and not grounds for failing a review.
func (rf *RulesFile) MarkEmittedAt(positions []int, now time.Time) int {
	stamped := 0
	for _, i := range positions {
		if i < 0 || i >= len(rf.Rules) {
			continue
		}
		rf.Rules[i].MarkEmitted(now)
		stamped++
	}
	return stamped
}
