package smelter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/warden"
)

// The occupancy is a report, not a change: a flush whose only "outcome" is that
// the file has a size must not commit, push and open a PR every cycle.
func TestOccupancyIsNotAChange(t *testing.T) {
	passes := PassResults{ActiveRules: 300, RuleCap: 300}
	assert.False(t, passes.HasChanges(),
		"a run that only measured the file has nothing to commit")
	assert.Equal(t, "no changes", passResultsSummary(passes),
		"and it must still say so — the occupancy is appended to a summary of changes, never used as one")
}

// A run that changed something reports the occupancy beside what it changed,
// whether or not the ceiling evicted anything: at "0 evicted" a file at half
// the ceiling and a file one rule under it read identically.
func TestSummaryCarriesOccupancyBesideTheChanges(t *testing.T) {
	summary := passResultsSummary(PassResults{
		Added:       []string{"r1", "r2"},
		ActiveRules: 288,
		RuleCap:     300,
	})
	assert.Equal(t, "2 added, 288/300 on file", summary)
}

// With no ceiling in effect the occupancy is omitted rather than rendered
// against zero. RuleCap is an int, so a deployment that disabled eviction and a
// caller that never measured a ceiling are the same value here, and "288/0"
// would be a claim about a ceiling nobody set.
func TestSummaryOmitsOccupancyWithNoCeiling(t *testing.T) {
	for _, ceiling := range []int{0, -1} {
		summary := passResultsSummary(PassResults{
			Added:       []string{"r1"},
			ActiveRules: 288,
			RuleCap:     ceiling,
		})
		assert.Equal(t, "1 added", summary, "ceiling=%d", ceiling)
	}
}

// The PR a reviewer reads carries the same figure, from the same renderer.
func TestPRBodyStatesTheOccupancy(t *testing.T) {
	body := buildPRBody(PassResults{
		Added:       []string{"r1"},
		ActiveRules: 300,
		RuleCap:     300,
	})
	assert.Contains(t, body, "- 300/300 on file once every pass had run (the `warden.max_rules_in_file` ceiling).")

	uncapped := buildPRBody(PassResults{Added: []string{"r1"}, ActiveRules: 300})
	assert.NotContains(t, uncapped, "on file",
		"no ceiling in effect, nothing to report it against")
}

// The occupancy the flush reports is measured after EVERY pass and against the
// ceiling the eviction pass was actually handed — not against a second read of
// a hot-reloadable setting, and not against the file as it stood before the
// staleness sweep.
func TestBuildFlushRulesReportsPostPassOccupancy(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()

	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
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
		WithMaxRulesInFile(func() int { return 4 }),
		WithDedupThreshold(func() float64 { return -1 }),
	)

	built, err := s.buildFlushRules(context.Background(), dir, "anvil-a", nil)
	require.NoError(t, err)

	assert.Equal(t, 2, built.passes.ActiveRules,
		"three rules in, one archived as stale: the occupancy is what the file holds when the flush ends")
	assert.Equal(t, 4, built.passes.RuleCap)
	assert.Contains(t, passResultsSummary(built.passes), "2/4 on file")
}

// The ceiling is a BACKSTOP behind the passes above it, not a substitute for
// them: with consolidation and staleness having brought the file under the
// ceiling, it evicts nothing — and the occupancy still says how close the file
// came, which is the whole reason it is reported on a run with no evictions.
func TestCeilingIsABackstopBehindTheEarlierPasses(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()

	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		return []string{"internal/smelter/smelter.go"}, nil
	})

	fresh := time.Now().UTC().AddDate(0, 0, -5).Format("2006-01-02")
	rules := []warden.Rule{
		{ID: "aged-one", Category: "other", Pattern: "aged pattern one", Check: "aged check one",
			Added: "2020-01-01", Source: warden.SourceList{"copilot:PR#1"}},
		{ID: "aged-two", Category: "other", Pattern: "aged pattern two", Check: "aged check two",
			Added: "2020-01-02", Source: warden.SourceList{"copilot:PR#2"}},
		{ID: "keeper", Category: "other", Pattern: "keeper pattern", Check: "keeper check",
			Added: fresh, Source: warden.SourceList{"copilot:PR#3"}},
	}
	require.NoError(t, warden.SaveRules(dir, &warden.RulesFile{Rules: rules}))

	s := New(db, time.Hour, map[string]string{},
		WithArchiveAfterDays(func() int { return 90 }),
		WithMaxRulesInFile(func() int { return 1 }),
		WithDedupThreshold(func() float64 { return -1 }),
	)

	built, err := s.buildFlushRules(context.Background(), dir, "anvil-a", nil)
	require.NoError(t, err)

	stale, overCap := built.passes.ArchivedByReason()
	assert.Equal(t, 2, stale, "both aged rules are past the 90-day threshold")
	assert.Equal(t, 0, overCap,
		"the earlier passes brought the file under the ceiling, so the backstop has nothing to evict")
	assert.Equal(t, 1, built.passes.ActiveRules)
	assert.Equal(t, 1, built.passes.RuleCap)

	summary := passResultsSummary(built.passes)
	assert.Contains(t, summary, "1/1 on file")
	assert.False(t, strings.Contains(summary, "evicted over cap"),
		"nothing was evicted, so nothing may report an eviction")
}
