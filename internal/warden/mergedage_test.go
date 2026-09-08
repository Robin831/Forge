package warden

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file covers the one invariant that ties consolidation to the staleness
// sweep: a merge must not produce a rule the very next sweep archives.
//
// It could, and did. MergeRule dated the survivor from the OLDEST member it
// folded, and consolidation exists to collapse old near-duplicates — so a
// cluster whose members were all past archive_after_days produced a rule born
// past the threshold, carrying no usage stamps because its members had none.
// The next sweep archived it, and every member it stood in for was already in
// the archive, so the file lost the coverage with nothing left on it to say
// what went. avoid-unbounded-in-clause and sql-in-clause-parameter-limit-2 are
// the two rules that were lost that way.

// oldDate renders a date days before now in the layout the rule files use.
func oldDate(now time.Time, days int) string {
	return formatUsageDate(now.AddDate(0, 0, -days))
}

// TestMergedRuleSurvivesTheVeryNextSweep is the acceptance case, run end to
// end: a real cluster of rules that are ALL older than archive_after_days,
// through the real consolidation pass, into the real sweep with no clock
// advanced between them.
//
// It asserts on the survivor rather than on MergedAt, because what the bead
// is about is the rule still being on the file — a later change that stamps
// the field and then anchors the age somewhere else would keep a field
// assertion green while deleting the rule again.
func TestMergedRuleSurvivesTheVeryNextSweep(t *testing.T) {
	now := time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC)
	// Both members were learned well over a year ago and neither has ever been
	// emitted: the exact population Pass 1 exists to fold.
	rf := &RulesFile{Rules: []Rule{
		{ID: "avoid-unbounded-in-clause", Category: "style",
			Pattern: "unbounded IN clause parameter list built from a slice",
			Check:   "chunk the IN clause so the parameter count stays bounded",
			Added:   oldDate(now, 420)},
		{ID: "sql-in-clause-parameter-limit-2", Category: "style",
			Pattern: "IN clause parameter list built from an unbounded slice",
			Check:   "bound the IN clause parameter count by chunking the slice",
			Added:   oldDate(now, 400)},
	}}

	replaced, summary, errs := ConsolidateWithParams(context.Background(), t.TempDir(), rf,
		shippedParams(),
		stubRunner(t, fakeMergedRule{ID: "sql-in-clause-parameter-limit",
			Pattern: "unbounded IN clause parameter list",
			Check:   "chunk the IN clause so the parameter count stays bounded"}))
	require.Empty(t, errs)
	require.Len(t, summary, 1, "the two restatements are a near-duplicate cluster")
	require.Len(t, replaced, 2)
	require.Len(t, rf.Rules, 1)

	merged := rf.Rules[0]
	require.Equal(t, "sql-in-clause-parameter-limit", merged.ID)

	// The sweep runs at the same instant, with thresholds every member is
	// already past.
	cfg := StaleConfig{ArchiveAfterDays: 90, InactiveAfterDays: 90}
	stale, reason := IsStale(merged, cfg, now)
	assert.False(t, stale,
		"a rule created by this run must not be archived by the sweep that follows it")
	assert.Equal(t, ReasonTooYoung, reason)

	sweep := ArchiveStale(rf.Rules, cfg, now)
	assert.Empty(t, sweep.Archived)
	require.Len(t, sweep.Active, 1)
	assert.Equal(t, "sql-in-clause-parameter-limit", sweep.Active[0].ID)
}

// The merge re-anchors the age, it does not exempt the rule forever: once the
// merged rule has itself aged past the threshold with nothing having used it,
// it is stale like any other.
func TestMergedRuleIsStaleOnceTheMergeItselfHasAged(t *testing.T) {
	now := time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC)
	r := Rule{ID: "merged", Added: oldDate(now, 400), MergedAt: oldDate(now, 200)}

	stale, reason := IsStale(r, StaleConfig{ArchiveAfterDays: 90, InactiveAfterDays: 90}, now)
	assert.True(t, stale)
	assert.Equal(t, ReasonAgedAndInactive, reason)
}

func TestIsStale_AgeAnchorPrefersMergedAt(t *testing.T) {
	now := time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC)
	cfg := StaleConfig{ArchiveAfterDays: 90, InactiveAfterDays: 90}

	t.Run("no MergedAt falls back to Added", func(t *testing.T) {
		stale, reason := IsStale(Rule{ID: "r", Added: oldDate(now, 400)}, cfg, now)
		assert.True(t, stale)
		assert.Equal(t, ReasonAgedAndInactive, reason)
	})

	t.Run("recent MergedAt over ancient Added is too young", func(t *testing.T) {
		r := Rule{ID: "r", Added: oldDate(now, 400), MergedAt: oldDate(now, 3)}
		stale, reason := IsStale(r, cfg, now)
		assert.False(t, stale)
		assert.Equal(t, ReasonTooYoung, reason)
	})

	t.Run("both ancient is stale", func(t *testing.T) {
		r := Rule{ID: "r", Added: oldDate(now, 400), MergedAt: oldDate(now, 380)}
		stale, reason := IsStale(r, cfg, now)
		assert.True(t, stale)
		assert.Equal(t, ReasonAgedAndInactive, reason)
	})

	// A merge does not make a rule younger than the file says it is: an
	// unreadable MergedAt is no measurement at all, so the anchor falls back
	// to Added rather than the rule reading as ageless.
	t.Run("unreadable MergedAt falls back to Added", func(t *testing.T) {
		r := Rule{ID: "r", Added: oldDate(now, 400), MergedAt: "not-a-date"}
		stale, reason := IsStale(r, cfg, now)
		assert.True(t, stale)
		assert.Equal(t, ReasonAgedAndInactive, reason)
	})

	// The anchor answers the AGE half only. A rule whose merge has aged but
	// which a reviewer saw last week is kept by the inactivity half, exactly
	// as an unmerged one would be.
	t.Run("MergedAt does not override recent activity", func(t *testing.T) {
		r := Rule{ID: "r", Added: oldDate(now, 400), MergedAt: oldDate(now, 380),
			LastEmitted: oldDate(now, 7)}
		stale, reason := IsStale(r, cfg, now)
		assert.False(t, stale)
		assert.Equal(t, ReasonRecentActivity, reason)
	})

	// An anchor with no activity record at all is the sweep's unanswerable
	// question: MergedAt gives it an age, but Added is unreadable and nothing
	// has ever been stamped, so there is no inactivity to measure. It resolves
	// the way every unanswerable question here does, under the reason that
	// names the half which went unanswered — the age half was answered, so
	// this is not the ageless rule's ReasonNoAgeAnchor.
	t.Run("anchor without any activity record is not stale", func(t *testing.T) {
		r := Rule{ID: "r", Added: "not-a-date", MergedAt: oldDate(now, 380)}
		stale, reason := IsStale(r, cfg, now)
		assert.False(t, stale)
		assert.Equal(t, ReasonNoActivitySignal, reason)
	})
}

func TestMergeRule_StampsTheMergeInstant(t *testing.T) {
	now := time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC)
	cluster := []Rule{
		{ID: "a", Added: "2024-01-01", LastEmitted: "2025-03-01", EmitCount: 4},
		{ID: "b", Added: "2024-06-01", LastEmitted: "2025-05-01", EmitCount: 3},
	}

	merged := MergeRule(cluster, "style", "p", "c", "merged", map[string]struct{}{}, now)
	assert.Equal(t, "2026-09-08", merged.MergedAt)
	assert.Equal(t, "2024-06-01", merged.Added, "Added is the newest member's")
	assert.Equal(t, "2025-05-01", merged.LastEmitted, "usage carries the newest emission")
	assert.Equal(t, 7, merged.EmitCount, "usage carries the summed count")
}

// A single-member cluster is a rewrite and not a fold, so it earns no fresh
// age. Stamping it would let any rule that passed through consolidation reset
// its own staleness clock.
func TestMergeRule_DoesNotStampASingleMemberCluster(t *testing.T) {
	now := time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC)
	merged := MergeRule([]Rule{{ID: "a", Added: "2024-01-01"}},
		"style", "p", "c", "merged", map[string]struct{}{}, now)
	assert.Empty(t, merged.MergedAt)
	assert.Equal(t, "2024-01-01", merged.Added)
}

// MergedAt is a fact about the file, not evidence a reviewer wanted the rule.
// Counting it as activity would let a consolidation reset the inactivity clock
// of every rule it touched.
func TestMergedAtIsNotActivity(t *testing.T) {
	now := time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC)
	r := Rule{ID: "r", Added: oldDate(now, 400), MergedAt: oldDate(now, 1)}
	assert.Equal(t, formatUsageDate(now.AddDate(0, 0, -400)),
		formatUsageDate(r.LastActivityIn(time.UTC)))
}

// A rules file written before MergedAt existed round-trips byte-identically:
// the field is omitempty, so nothing appears for a rule that is not a merge
// product.
func TestMergedAtIsOmittedForUnmergedRules(t *testing.T) {
	dir := t.TempDir()
	rf := &RulesFile{Rules: []Rule{
		{ID: "plain", Category: "style", Pattern: "p", Check: "c", Added: "2026-01-01"},
		{ID: "merged", Category: "style", Pattern: "p", Check: "c", Added: "2026-01-01",
			MergedAt: "2026-09-08"},
	}}
	path := RulesPath(dir)
	require.NoError(t, SaveRules(path, rf))

	loaded, err := LoadRules(path)
	require.NoError(t, err)
	require.Len(t, loaded.Rules, 2)
	assert.Empty(t, loaded.Rules[0].MergedAt)
	assert.Equal(t, "2026-09-08", loaded.Rules[1].MergedAt)
}

// A learner never produces a merge product, whatever the model's JSON claimed.
// Both learners unmarshal the model's raw answer into a Rule and extractJSON
// does not restrict it to the keys the output contract asked for, so a
// merged_at in that answer would become the rule's age anchor — and since
// IsStale prefers MergedAt over Added unconditionally, a recent or future one
// makes the rule report too-young forever. Nothing else retires it: the file
// ceiling deliberately does not read MergedAt, and the terminus guard is only
// reached after both thresholds are crossed.
func TestDistillRuleIgnoresModelSuppliedMergeAndUsageStamps(t *testing.T) {
	stubDistiller(t, `{"id":"x","category":"style","pattern":"p","check":"c",`+
		`"merged_at":"2099-01-01","last_emitted":"2099-01-01","emit_count":99,"last_finding":"2099-01-02"}`)

	rule, err := DistillRule(context.Background(), []PRComment{
		{PRNumber: 1, Path: "api/Foo.cs", Body: "check this"},
	}, t.TempDir())
	require.NoError(t, err)

	assert.Empty(t, rule.MergedAt, "a freshly learned rule is not a merge product")
	assert.Empty(t, rule.LastEmitted, "no review has selected this rule yet")
	assert.Zero(t, rule.EmitCount)
	assert.Empty(t, rule.LastFinding)
}

// The CI-fix learner is the same hole by the same route.
func TestDistillCIFixRuleIgnoresModelSuppliedMergeAndUsageStamps(t *testing.T) {
	stubDistiller(t, `{"id":"lint-x","category":"style","pattern":"p","check":"c",`+
		`"merged_at":"2099-01-01","last_emitted":"2099-01-01","emit_count":99,"last_finding":"2099-01-02"}`)

	rule, err := distillCIFixRule(context.Background(), "SA1000", "lint-sa1000",
		map[string]string{"build": "SA1000: bad spacing"},
		"diff --git a/api/Foo.cs b/api/Foo.cs\n+fixed\n", "ci:PR#1", t.TempDir())
	require.NoError(t, err)

	assert.Empty(t, rule.MergedAt)
	assert.Empty(t, rule.LastEmitted)
	assert.Zero(t, rule.EmitCount)
	assert.Empty(t, rule.LastFinding)
}

// The end the clearing exists for: a rule the model stamped with a future
// merge date and a full emission history must still be an ordinary, sweepable
// rule once it has aged and nothing has used it.
func TestLearnedRuleWithModelSuppliedStampsStaysSweepable(t *testing.T) {
	stubDistiller(t, `{"id":"x","category":"style","pattern":"p","check":"c",`+
		`"merged_at":"2099-01-01","last_emitted":"2099-01-01","emit_count":99}`)

	rule, err := DistillRule(context.Background(), []PRComment{
		{PRNumber: 1, Path: "api/Foo.cs", Body: "check this"},
	}, t.TempDir())
	require.NoError(t, err)

	now := time.Now().UTC()
	rule.Added = oldDate(now, 400)
	stale, reason := IsStale(*rule, StaleConfig{ArchiveAfterDays: 90}, now)
	assert.True(t, stale)
	assert.Equal(t, ReasonAgedAndInactive, reason)
}
