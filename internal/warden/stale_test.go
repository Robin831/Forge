package warden

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ageOnly is the config a caller that configures nothing gets: one threshold,
// which InactiveAfterDays falls back to. It is the shape every pre-existing
// case below was written against, so the cases keep asserting the behaviour
// this deployment had before the split.
func ageOnly(days int) StaleConfig {
	return StaleConfig{ArchiveAfterDays: days}
}

// staleAt is IsStale's boolean half, for a case whose subject is the verdict
// rather than the reason.
func staleAt(r Rule, cfg StaleConfig, now time.Time) bool {
	stale, _ := IsStale(r, cfg, now)
	return stale
}

func TestIsStale_DisabledWhenThresholdNonPositive(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	r := Rule{ID: "r1", Added: "2020-01-01"}

	stale, reason := IsStale(r, ageOnly(0), now)
	assert.False(t, stale)
	assert.Equal(t, ReasonStalenessDisabled, reason)

	stale, reason = IsStale(r, ageOnly(-1), now)
	assert.False(t, stale)
	assert.Equal(t, ReasonStalenessDisabled, reason)
}

func TestIsStale_NoAddedDate_NotStale(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	r := Rule{ID: "r1"} // Added empty

	stale, reason := IsStale(r, ageOnly(30), now)
	assert.False(t, stale)
	assert.Equal(t, ReasonNoAddedDate, reason)
}

func TestIsStale_MalformedAddedDate_NotStale(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	r := Rule{ID: "r1", Added: "not-a-date"}

	stale, reason := IsStale(r, ageOnly(30), now)
	assert.False(t, stale)
	assert.Equal(t, ReasonNoAddedDate, reason)
}

// TestIsStale_UnreadableAddedIsNotRescuedByAStamp pins the direction the age
// half fails in. A rule with an unparseable Added but a readable LastEmitted
// has an activity date and no age, so the first half of the test cannot be
// answered at all — and an unanswerable question retires nothing.
func TestIsStale_UnreadableAddedIsNotRescuedByAStamp(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	r := Rule{ID: "r1", Added: "", LastEmitted: now.AddDate(0, 0, -400).Format(staleAddedLayout)}

	stale, reason := IsStale(r, ageOnly(180), now)
	assert.False(t, stale, "an ancient emission does not establish an age the rule never recorded")
	assert.Equal(t, ReasonNoAddedDate, reason)
}

func TestIsStale_FreshRule_NotStale(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	r := Rule{ID: "r1", Added: "2026-05-01"} // 19 days ago

	stale, reason := IsStale(r, ageOnly(30), now)
	assert.False(t, stale, "rule added within threshold should not be stale")
	assert.Equal(t, ReasonTooYoung, reason)
}

func TestIsStale_OldRule_Stale(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// Added 200 days ago, threshold 180 days.
	r := Rule{ID: "r1", Added: now.AddDate(0, 0, -200).Format("2006-01-02")}

	stale, reason := IsStale(r, ageOnly(180), now)
	assert.True(t, stale, "rule added past the threshold should be stale")
	assert.Equal(t, ReasonAgedAndInactive, reason)
}

func TestIsStale_AtBoundary_NotStale(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// Added exactly 30 days ago — boundary is exclusive.
	r := Rule{ID: "r1", Added: now.AddDate(0, 0, -30).Format("2006-01-02")}

	assert.False(t, staleAt(r, ageOnly(30), now),
		"boundary should be exclusive — rule equal to threshold is not stale")
}

// TestIsStale_RequiresBothAgeAndInactivity is the two-signal predicate itself:
// neither half alone retires a rule, and the two thresholds are read
// independently.
func TestIsStale_RequiresBothAgeAndInactivity(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	cfg := StaleConfig{ArchiveAfterDays: 90, InactiveAfterDays: 30}

	// Old, and silent for longer than the inactivity window: both halves hold.
	old := Rule{
		ID:          "old-and-silent",
		Added:       now.AddDate(0, 0, -365).Format(staleAddedLayout),
		LastEmitted: now.AddDate(0, 0, -60).Format(staleAddedLayout),
	}
	stale, reason := IsStale(old, cfg, now)
	assert.True(t, stale)
	assert.Equal(t, ReasonAgedAndInactive, reason)

	// Same rule, emitted inside the inactivity window: the age half still
	// holds and the rule is kept anyway.
	used := old
	used.LastEmitted = now.AddDate(0, 0, -10).Format(staleAddedLayout)
	stale, reason = IsStale(used, cfg, now)
	assert.False(t, stale)
	assert.Equal(t, ReasonRecentActivity, reason)

	// A rule younger than the age threshold but silent for longer than the
	// inactivity one: inactivity alone retires nothing. Learned 45 days ago
	// and never emitted, so its only activity is its own Added date.
	young := Rule{ID: "young-and-silent", Added: now.AddDate(0, 0, -45).Format(staleAddedLayout)}
	stale, reason = IsStale(young, cfg, now)
	assert.False(t, stale, "a rule under the age threshold is never stale, however quiet")
	assert.Equal(t, ReasonTooYoung, reason)
}

// TestIsStale_ReportedBug_YearOldRuleEmittedYesterday is the case this was
// filed for: a rule whose Added date is a year old and whose last emission was
// yesterday is in active use, and no age threshold may retire it.
func TestIsStale_ReportedBug_YearOldRuleEmittedYesterday(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	r := Rule{
		ID:          "old-but-emitted-yesterday",
		Added:       now.AddDate(-1, 0, 0).Format(staleAddedLayout),
		LastEmitted: now.AddDate(0, 0, -1).Format(staleAddedLayout),
		EmitCount:   42,
	}

	for _, cfg := range []StaleConfig{
		ageOnly(90),
		{ArchiveAfterDays: 90, InactiveAfterDays: 30},
		{ArchiveAfterDays: 1, InactiveAfterDays: 1},
	} {
		stale, reason := IsStale(r, cfg, now)
		assert.False(t, stale, "a rule emitted yesterday is not stale (cfg=%+v)", cfg)
		assert.Equal(t, ReasonRecentActivity, reason)
	}
}

// TestIsStale_InactiveAfterDaysFallsBackToTheAgeThreshold pins the resolution
// of the unset value. It is not an off switch — reading it as one would put
// the predicate back on age alone, which is the defect — so both zero and a
// negative resolve to ArchiveAfterDays.
func TestIsStale_InactiveAfterDaysFallsBackToTheAgeThreshold(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	r := Rule{
		ID:          "old-but-used",
		Added:       now.AddDate(0, 0, -400).Format(staleAddedLayout),
		LastEmitted: now.AddDate(0, 0, -3).Format(staleAddedLayout),
	}

	for _, inactive := range []int{0, -1, -365} {
		cfg := StaleConfig{ArchiveAfterDays: 180, InactiveAfterDays: inactive}
		assert.Equal(t, 180, cfg.inactiveDays())
		stale, reason := IsStale(r, cfg, now)
		assert.False(t, stale, "inactive_after_days=%d must not disable the inactivity half", inactive)
		assert.Equal(t, ReasonRecentActivity, reason)
	}
}

func TestArchiveStale_PartitionsFreshAndStaleRules(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	fresh := Rule{ID: "fresh", Category: "style", Pattern: "p1", Check: "c1", Added: now.AddDate(0, 0, -10).Format("2006-01-02")}
	stale := Rule{ID: "stale", Category: "style", Pattern: "p2", Check: "c2", Added: now.AddDate(0, 0, -400).Format("2006-01-02")}

	sweep := ArchiveStale([]Rule{fresh, stale}, ageOnly(180), now)

	assert.Len(t, sweep.Active, 1)
	assert.Equal(t, "fresh", sweep.Active[0].ID)
	assert.Len(t, sweep.Archived, 1)
	assert.Equal(t, "stale", sweep.Archived[0].ID)
	assert.Empty(t, sweep.Protected)
}

func TestArchiveStale_SetsLastSeenAndReason(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	r := Rule{ID: "old", Category: "style", Pattern: "p", Check: "c", Added: "2020-01-01"}

	sweep := ArchiveStale([]Rule{r}, ageOnly(180), now)

	require.Len(t, sweep.Archived, 1)
	archived := sweep.Archived[0]
	assert.Equal(t, now, archived.LastSeen, "LastSeen must be set to now on archived stale rules")
	assert.Equal(t, now, archived.ArchivedAt, "ArchivedAt must be set to now on archived stale rules")
	assert.Equal(t, ArchiveReasonStale, archived.ArchiveReason)
	assert.Equal(t, "", archived.SupersededBy, "stale archive entries have no superseder")
	// The original rule data is carried through unchanged.
	assert.Equal(t, "old", archived.ID)
	assert.Equal(t, "style", archived.Category)
	assert.Equal(t, "p", archived.Pattern)
	assert.Equal(t, "c", archived.Check)
	assert.Equal(t, "2020-01-01", archived.Added)
}

func TestArchiveStale_PreservesActiveOrder(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	a := Rule{ID: "a", Added: now.AddDate(0, 0, -10).Format("2006-01-02")}
	b := Rule{ID: "b", Added: "2020-01-01"} // stale
	c := Rule{ID: "c", Added: now.AddDate(0, 0, -5).Format("2006-01-02")}
	d := Rule{ID: "d", Added: "2019-01-01"} // stale
	e := Rule{ID: "e", Added: now.AddDate(0, 0, -1).Format("2006-01-02")}

	sweep := ArchiveStale([]Rule{a, b, c, d, e}, ageOnly(180), now)

	assert.Equal(t, []string{"a", "c", "e"}, ruleIDs(sweep.Active))
	assert.Equal(t, []string{"b", "d"}, archivedIDs(sweep.Archived))
}

func TestArchiveStale_NothingStale_ReturnsOriginalSlice(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	in := []Rule{
		{ID: "a", Added: now.AddDate(0, 0, -10).Format("2006-01-02")},
		{ID: "b", Added: now.AddDate(0, 0, -20).Format("2006-01-02")},
	}

	sweep := ArchiveStale(in, ageOnly(180), now)
	assert.Equal(t, in, sweep.Active)
	assert.Nil(t, sweep.Archived)
	assert.Nil(t, sweep.Protected)
}

func TestArchiveStale_DisabledWhenThresholdNonPositive(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	in := []Rule{{ID: "a", Added: "2020-01-01"}}

	sweep := ArchiveStale(in, ageOnly(0), now)
	assert.Equal(t, in, sweep.Active)
	assert.Nil(t, sweep.Archived)

	sweep = ArchiveStale(in, ageOnly(-5), now)
	assert.Equal(t, in, sweep.Active)
	assert.Nil(t, sweep.Archived)
}

func TestArchiveStale_EmptyInput(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	sweep := ArchiveStale(nil, ageOnly(180), now)
	assert.Nil(t, sweep.Active)
	assert.Nil(t, sweep.Archived)
}

func archivedIDs(rules []ArchivedRule) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.ID
	}
	return out
}

// TestIsStale_RecentEmissionKeepsAnOldRule is the point of the usage
// telemetry: a rule learned long ago but put in front of a reviewer last week
// is in use, and retiring it for the age of its distillation session is what
// the sweep did while Added was a rule's only timestamp.
func TestIsStale_RecentEmissionKeepsAnOldRule(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	r := Rule{
		ID:          "old-but-used",
		Added:       now.AddDate(0, 0, -400).Format(staleAddedLayout),
		LastEmitted: now.AddDate(0, 0, -3).Format(staleAddedLayout),
	}

	assert.False(t, staleAt(r, ageOnly(180), now), "a rule emitted three days ago is not inactive")

	// A finding counts the same way, and a rule whose only activity is old is
	// still stale — the telemetry keeps rules alive, it does not exempt them.
	r.LastEmitted = ""
	r.LastFinding = now.AddDate(0, 0, -3).Format(staleAddedLayout)
	assert.False(t, staleAt(r, ageOnly(180), now), "a rule that produced a finding three days ago is not inactive")

	r.LastFinding = now.AddDate(0, 0, -300).Format(staleAddedLayout)
	assert.True(t, staleAt(r, ageOnly(180), now), "a rule with no activity inside the window is stale")
}

// TestIsStale_UnstampedRuleReadsItsAddedDate is the backward-compatibility
// floor: every rules file written before the telemetry existed carries no
// stamps, and those rules must age exactly as they always did.
func TestIsStale_UnstampedRuleReadsItsAddedDate(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	assert.True(t, staleAt(Rule{Added: now.AddDate(0, 0, -200).Format(staleAddedLayout)}, ageOnly(180), now))
	assert.False(t, staleAt(Rule{Added: now.AddDate(0, 0, -10).Format(staleAddedLayout)}, ageOnly(180), now))
}
