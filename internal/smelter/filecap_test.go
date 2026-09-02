package smelter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/warden"
)

func capRules(n int) []warden.Rule {
	out := make([]warden.Rule, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, warden.Rule{
			ID:       "rule-" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			Category: "other",
			Pattern:  "pattern words for rule",
			Check:    "check text",
			Added:    "2026-03-01",
			Paths:    []string{"**/*"},
		})
	}
	return out
}

// TestApplyFileCapIsTheOneEvictionPass: both entry points (the scheduled
// flush's runFileCap and the CLI's ConsolidateAnvil) go through this one
// function, so the pass cannot come to evict by two sets of rules.
func TestApplyFileCapIsTheOneEvictionPass(t *testing.T) {
	rf := &warden.RulesFile{Rules: capRules(10)}
	var messages []string
	evicted := applyFileCap("anvil", rf, 4, time.Now().UTC(), func(m string) {
		messages = append(messages, m)
	})
	require.Len(t, evicted, 6)
	assert.Len(t, rf.Rules, 4)
	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "Evicted 6 rule(s) over the 4-rule ceiling for anvil")
	for _, e := range evicted {
		assert.Equal(t, warden.ArchiveReasonOverCap, e.ArchiveReason)
	}
}

// A ceiling of <= 0 is the disable, and a file under the ceiling is untouched.
// Neither emits an event: the pass did nothing to report.
func TestApplyFileCapNoOp(t *testing.T) {
	for _, max := range []int{0, -1, 10, 50} {
		rf := &warden.RulesFile{Rules: capRules(10)}
		emitted := false
		evicted := applyFileCap("anvil", rf, max, time.Now().UTC(), func(string) { emitted = true })
		assert.Empty(t, evicted, "max=%d", max)
		assert.Len(t, rf.Rules, 10, "max=%d", max)
		assert.False(t, emitted, "max=%d", max)
	}
	// A nil rules file and a nil sink are both tolerated.
	assert.Empty(t, applyFileCap("anvil", nil, 1, time.Now().UTC(), nil))
	rf := &warden.RulesFile{Rules: capRules(10)}
	assert.Len(t, applyFileCap("anvil", rf, 4, time.Now().UTC(), nil), 6)
}

// TestArchivedByReasonSplitsStaleFromOverCap: PassResults.Archived is one list
// because the archive store takes one write, but the aggregates rendered from
// it must not report an eviction as a rule that aged out.
func TestArchivedByReasonSplitsStaleFromOverCap(t *testing.T) {
	archived := []warden.ArchivedRule{
		{Rule: warden.Rule{ID: "a"}, ArchiveReason: warden.ArchiveReasonStale},
		{Rule: warden.Rule{ID: "b"}, ArchiveReason: warden.ArchiveReasonOverCap},
		{Rule: warden.Rule{ID: "c"}, ArchiveReason: warden.ArchiveReasonOverCap},
		{Rule: warden.Rule{ID: "d"}}, // empty reason renders as stale
		{Rule: warden.Rule{ID: "e"}, ArchiveReason: warden.ArchiveReasonDuplicate},
	}
	stale, overCap := archivedByReason(archived)
	assert.Equal(t, 2, stale)
	assert.Equal(t, 2, overCap)

	passes := PassResults{Archived: archived}
	subject := buildCommitSubject(passes)
	assert.Contains(t, subject, "archive 2 stale rule(s)")
	assert.Contains(t, subject, "evict 2 over-cap rule(s)")

	summary := passResultsSummary(passes)
	assert.Contains(t, summary, "2 archived")
	assert.Contains(t, summary, "2 evicted over cap")
}

// An over-cap-only run must not describe its evictions as stale rules
// anywhere: that is the mislabel folding both into one count produced.
func TestOverCapOnlyRunIsNeverReportedAsStale(t *testing.T) {
	passes := PassResults{Archived: []warden.ArchivedRule{
		{Rule: warden.Rule{ID: "a"}, ArchiveReason: warden.ArchiveReasonOverCap},
	}}
	assert.NotContains(t, buildCommitSubject(passes), "stale")
	assert.NotContains(t, buildPRBody(passes), "stale rule(s) archived")
	assert.Contains(t, buildPRBody(passes), "over its size ceiling")
	assert.NotContains(t, passResultsSummary(passes), "archived")
}

// The scheduled flush's half of the pass has the same three branches the
// staleness sweep does — unwired, disabled, and evicting — and it is the one
// the daemon runs on a schedule against every anvil's live rules file.

// TestRunFileCap_DisabledByDefault: without WithMaxRulesInFile the ceiling is
// unwired and the pass is a no-op, whatever the file holds.
func TestRunFileCap_DisabledByDefault(t *testing.T) {
	db := openTestDB(t)
	s := New(db, time.Hour, map[string]string{})

	rf := &warden.RulesFile{Rules: capRules(10)}

	archived := s.runFileCap("anvil-a", rf)
	assert.Empty(t, archived)
	assert.Len(t, rf.Rules, 10, "rf.Rules must be untouched when the ceiling is unwired")
}

// TestRunFileCap_ZeroOrNegativeCeilingSkipsPass: a ceiling of <= 0 is the
// operator's off switch and reaches the pass through the configured function,
// so it has to be honoured there and not only in warden.EvictOverCap.
func TestRunFileCap_ZeroOrNegativeCeilingSkipsPass(t *testing.T) {
	for _, max := range []int{0, -1} {
		db := openTestDB(t)
		s := New(db, time.Hour, map[string]string{},
			WithMaxRulesInFile(func() int { return max }),
		)

		rf := &warden.RulesFile{Rules: capRules(10)}

		archived := s.runFileCap("anvil-a", rf)
		assert.Empty(t, archived, "max=%d", max)
		assert.Len(t, rf.Rules, 10, "max=%d", max)
	}
}

// TestRunFileCap_EvictsOverCeilingAndLogsIt: over the ceiling the rules leave
// rf in place, come back as archive entries under the eviction's OWN reason
// (not stale — they did not age out), and the flush says so in the event feed.
func TestRunFileCap_EvictsOverCeilingAndLogsIt(t *testing.T) {
	db := openTestDB(t)
	s := New(db, time.Hour, map[string]string{},
		WithMaxRulesInFile(func() int { return 4 }),
	)

	rf := &warden.RulesFile{Rules: capRules(10)}

	archived := s.runFileCap("anvil-a", rf)
	require.Len(t, archived, 6)
	assert.Len(t, rf.Rules, 4, "the pass mutates rf in place so later passes see only survivors")
	for _, a := range archived {
		assert.Equal(t, warden.ArchiveReasonOverCap, a.ArchiveReason)
		assert.False(t, a.ArchivedAt.IsZero())
	}

	events, err := db.RecentEvents(20)
	require.NoError(t, err)
	var found bool
	for _, e := range events {
		if e.Type == state.EventSmelterFlushed && strings.Contains(e.Message, "Evicted 6 rule(s) over the 4-rule ceiling for anvil-a") {
			found = true
		}
	}
	assert.True(t, found, "an eviction must reach the activity feed: %+v", events)
}

// TestBuildFlushRules_FileCapRunsAfterStalenessAndBeforeBackfill pins the
// ordering the comment at the call site calls load-bearing, from the entry
// point the daemon actually runs.
//
// After staleness: three rules against a ceiling of two, one of them stale. The
// staleness sweep takes that one and the file then FITS, so nothing is evicted.
// Run the other way round, the cap would spend its eviction on a rule staleness
// was about to remove anyway and the file would come out one rule short.
//
// Before the backfill: the rules carry no Paths and a source PR each, so the
// backfill would fetch every one of them. A rule the cap removed must never be
// looked up — the point of the ordering is that no PR round trip is spent on a
// rule that is leaving.
func TestBuildFlushRules_FileCapRunsAfterStalenessAndBeforeBackfill(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()

	var fetched []int
	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		fetched = append(fetched, prNum)
		return []string{"internal/smelter/smelter.go"}, nil
	})

	fresh := time.Now().UTC().AddDate(0, 0, -5).Format("2006-01-02")
	rules := []warden.Rule{
		{ID: "aged", Category: "other", Pattern: "aged pattern", Check: "aged check",
			Added: "2020-01-01", Source: warden.SourceList{"copilot:PR#1"}},
		{ID: "keeper", Category: "other", Pattern: "keeper pattern", Check: "keeper check",
			Added: fresh, Source: warden.SourceList{"copilot:PR#2"}},
		{ID: "runner-up", Category: "other", Pattern: "runner pattern", Check: "runner check",
			Added: fresh, Source: warden.SourceList{"copilot:PR#3"}},
	}
	require.NoError(t, warden.SaveRules(dir, &warden.RulesFile{Rules: rules}))

	s := New(db, time.Hour, map[string]string{},
		WithArchiveAfterDays(func() int { return 180 }),
		WithMaxRulesInFile(func() int { return 2 }),
		// Consolidation is a different pass with its own tests, and its AI
		// call is not wired here.
		WithDedupThreshold(func() float64 { return -1 }),
	)

	built, err := s.buildFlushRules(context.Background(), dir, "anvil-a", nil)
	require.NoError(t, err)

	stale, overCap := built.passes.ArchivedByReason()
	assert.Equal(t, 1, stale, "the staleness sweep takes the aged rule")
	assert.Equal(t, 0, overCap,
		"the file fits once staleness has run, so the ceiling must evict nothing — an eviction here means the cap ran first and spent a slot on a rule staleness was about to remove")
	require.Len(t, built.rules.Rules, 2)

	// Both survivors were backfilled; the archived rule was never looked up.
	assert.ElementsMatch(t, []string{"keeper", "runner-up"}, built.passes.Backfilled)
	assert.ElementsMatch(t, []int{2, 3}, fetched,
		"the backfill must run after the removals, so no PR lookup is spent on a rule that is leaving the file")
}

// The mirror case: with nothing stale, the ceiling does evict, and what it
// evicts never reaches the backfill either.
func TestBuildFlushRules_FileCapEvictsBeforeTheBackfill(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()

	var fetched []int
	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		fetched = append(fetched, prNum)
		return []string{"internal/smelter/smelter.go"}, nil
	})

	newest := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	middle := time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02")
	oldest := time.Now().UTC().AddDate(0, 0, -20).Format("2006-01-02")
	rules := []warden.Rule{
		{ID: "oldest", Category: "other", Pattern: "oldest pattern", Check: "oldest check",
			Added: oldest, Source: warden.SourceList{"copilot:PR#1"}},
		{ID: "middle", Category: "other", Pattern: "middle pattern", Check: "middle check",
			Added: middle, Source: warden.SourceList{"copilot:PR#2"}},
		{ID: "newest", Category: "other", Pattern: "newest pattern", Check: "newest check",
			Added: newest, Source: warden.SourceList{"copilot:PR#3"}},
	}
	require.NoError(t, warden.SaveRules(dir, &warden.RulesFile{Rules: rules}))

	s := New(db, time.Hour, map[string]string{},
		WithArchiveAfterDays(func() int { return 180 }),
		WithMaxRulesInFile(func() int { return 2 }),
		WithDedupThreshold(func() float64 { return -1 }),
	)

	built, err := s.buildFlushRules(context.Background(), dir, "anvil-a", nil)
	require.NoError(t, err)

	stale, overCap := built.passes.ArchivedByReason()
	assert.Equal(t, 0, stale)
	require.Equal(t, 1, overCap, "one rule over a ceiling of two")
	require.Len(t, built.rules.Rules, 2)

	// With no paths to separate them the ranking falls back to recency, so the
	// oldest loses the slot — and is never fetched for.
	require.Len(t, built.passes.Archived, 1)
	assert.Equal(t, "oldest", built.passes.Archived[0].ID)
	assert.NotContains(t, fetched, 1,
		"the evicted rule's source PR must not be looked up")
	assert.ElementsMatch(t, []int{2, 3}, fetched)
}
