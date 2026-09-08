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

func TestDuplicateArchiveEntriesCarryTheirMergeTarget(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	entries := duplicateArchiveEntries(
		[]warden.Rule{{ID: "dup-a"}, {ID: "dup-b"}, {ID: "unmentioned"}},
		[]warden.MergeResult{{Merged: warden.Rule{ID: "merged"}, ReplacedIDs: []string{"dup-a", "dup-b"}}},
		now,
	)

	require.Len(t, entries, 3)
	for _, e := range entries {
		assert.Equal(t, warden.ArchiveReasonDuplicate, e.ArchiveReason)
		assert.Equal(t, now, e.ArchivedAt)
		assert.Equal(t, now, e.LastSeen)
	}
	assert.Equal(t, "merged", entries[0].SupersededBy)
	assert.Equal(t, "merged", entries[1].SupersededBy)
	// A rule no merge result names has no successor, which is what
	// BuildSupersededByIndex reads as "merged into nothing" — never a guess.
	assert.Empty(t, entries[2].SupersededBy)
}

// The archive summary and the archive WRITE must agree about which rule each
// duplicate was folded into: derived separately, a fold persisted with a
// successor the summary never saw is reported as a class that left the file.
func TestDuplicateArchiveEntriesAreWhatTheSummaryReads(t *testing.T) {
	replaced := []warden.Rule{{ID: "dup"}}
	summary := []warden.MergeResult{{Merged: warden.Rule{ID: "merged"}, ReplacedIDs: []string{"dup"}}}
	entries := duplicateArchiveEntries(replaced, summary, time.Now().UTC())

	// "merged" is on the file the run wrote, so the class moved forward and
	// nothing is unrepresented.
	got := warden.SummarizeArchiveRun(
		[]warden.Rule{{ID: "dup"}, {ID: "merged"}},
		[]warden.Rule{{ID: "merged"}},
		entries,
	)
	assert.Equal(t, 1, got.Duplicate)
	assert.Empty(t, got.UnrepresentedClasses)
}

func TestReportArchiveSummaryStaysSilentOnAnEmptyRun(t *testing.T) {
	emitted := 0
	got := reportArchiveSummary("anvil-a", nil, nil, nil, nil, func(string) { emitted++ })

	assert.False(t, got.HasSubstance())
	assert.Zero(t, emitted, "a run that archived nothing has nothing to put in the feed")
}

func TestReportArchiveSummaryEmitsOnlyForAnUnrepresentedClass(t *testing.T) {
	var messages []string
	emit := func(m string) { messages = append(messages, m) }

	// An ordinary retirement: the counts already reach the feed from the pass
	// that produced them, so a second row here would read as a second run.
	reportArchiveSummary("anvil-a",
		[]warden.Rule{{ID: "lonely"}}, nil,
		[]warden.ArchivedRule{{Rule: warden.Rule{ID: "lonely"}, ArchiveReason: warden.ArchiveReasonStale}},
		nil, emit)
	assert.Empty(t, messages)

	// A chain ending: the one thing nothing else says.
	archive := []warden.ArchivedRule{{Rule: warden.Rule{ID: "old"}, SupersededBy: "terminus"}}
	entry := warden.ArchivedRule{Rule: warden.Rule{ID: "terminus"}, ArchiveReason: warden.ArchiveReasonStale}
	reportArchiveSummary("anvil-a",
		[]warden.Rule{{ID: "terminus"}}, nil,
		[]warden.ArchivedRule{entry},
		warden.BuildSupersededByIndex(append(archive, entry)), emit)

	require.Len(t, messages, 1)
	assert.Contains(t, messages[0], "classes now unrepresented: terminus")
	assert.Contains(t, messages[0], "anvil-a")
}

// The entries the summary counted must be the entries that land in the
// archive store — same superseded_by, same stamps. Derived a second time at
// the write, one fold is recorded under two clock readings and its mapping is
// only accidentally the one the class analysis read.
func TestTheArchiveWritePersistsTheEntriesTheSummaryCounted(t *testing.T) {
	dir := t.TempDir()
	stamped := time.Date(2026, 9, 8, 9, 30, 0, 0, time.UTC)
	entries := duplicateArchiveEntries(
		[]warden.Rule{{ID: "dup", Category: "style", Pattern: "p", Check: "c"}},
		[]warden.MergeResult{{Merged: warden.Rule{ID: "merged"}, ReplacedIDs: []string{"dup"}}},
		stamped,
	)

	summary := warden.SummarizeArchiveRun(
		[]warden.Rule{{ID: "dup"}, {ID: "merged"}}, []warden.Rule{{ID: "merged"}}, entries)
	require.Equal(t, 1, summary.Duplicate)

	require.NoError(t, archiveRules(dir, entries, nil))

	a, err := warden.LoadArchive(warden.ArchivePath(dir))
	require.NoError(t, err)
	require.Len(t, a.Rules, 1)
	assert.Equal(t, "merged", a.Rules[0].SupersededBy)
	assert.True(t, a.Rules[0].ArchivedAt.Equal(stamped),
		"the write must not re-stamp an entry the summary already counted")
}

// The summary has to reach a surface a caller can read: logging it leaves the
// class list nowhere the commit body, the PR body or the CLI can find it.
func TestUnrepresentedClassesReachTheCommitAndPRBodies(t *testing.T) {
	passes := PassResults{
		Archived: []warden.ArchivedRule{
			{Rule: warden.Rule{ID: "terminus"}, ArchiveReason: warden.ArchiveReasonStale},
		},
		ArchiveSummary: warden.ArchiveSummary{
			Archived:             1,
			Stale:                1,
			UnrepresentedClasses: []string{"terminus"},
		},
	}

	assert.Contains(t, buildCommitBody(passes), "terminus")
	assert.Contains(t, buildCommitBody(passes), "Classes now unrepresented")
	assert.Contains(t, buildPRBody(passes), "terminus")

	// And a run that left every class represented says nothing about them:
	// the section exists to name a loss, not to report its absence in every
	// commit message the smelter writes.
	quiet := passes
	quiet.ArchiveSummary.UnrepresentedClasses = nil
	assert.NotContains(t, buildCommitBody(quiet), "Classes now unrepresented")
	assert.NotContains(t, buildPRBody(quiet), "supersession class")
}

// Everything above tests the summary from its own inputs. These two pin the
// ASSEMBLY at the entry points the daemon and the operator actually run, which
// is where every failure mode is silent: a beforePasses snapshot taken after
// the passes have mutated rf.Rules in place makes the startedActive filter drop
// every archived rule, a missed entry list or supersession index makes the walk
// find no chain, and a summary that never reaches PassResults leaves the commit
// body, the PR body and the CLI with nothing to render. Each one reports no
// classes and keeps every other assertion in this package passing.

// The scheduled flush's half. The archive holds member-a merged into terminus,
// and the file ceiling — which deliberately does not honour the terminus guard
// — evicts terminus, so the chain ends the run with nothing on the active file.
func TestBuildFlushRules_NamesTheClassTheCeilingLeftUnrepresented(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) { return nil, nil })

	older := time.Now().UTC().AddDate(0, 0, -20).Format("2006-01-02")
	newer := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	require.NoError(t, warden.SaveRules(dir, &warden.RulesFile{Rules: []warden.Rule{
		{ID: "terminus", Category: "other", Pattern: "terminus pattern", Check: "terminus check", Added: older},
		{ID: "keeper", Category: "other", Pattern: "keeper pattern", Check: "keeper check", Added: newer},
	}}))
	archive := &warden.Archive{Rules: []warden.ArchivedRule{
		{Rule: warden.Rule{ID: "member-a"}, SupersededBy: "terminus", ArchiveReason: warden.ArchiveReasonDuplicate},
	}}
	require.NoError(t, archive.Save(warden.ArchivePath(dir)))

	s := New(db, time.Hour, map[string]string{},
		WithArchiveAfterDays(func() int { return 180 }),
		WithMaxRulesInFile(func() int { return 1 }),
		WithDedupThreshold(func() float64 { return -1 }),
	)

	built, err := s.buildFlushRules(context.Background(), dir, "anvil-a", nil)
	require.NoError(t, err)

	require.Len(t, built.passes.Archived, 1)
	require.Equal(t, "terminus", built.passes.Archived[0].ID, "the ceiling evicts the older rule")
	assert.Equal(t, []string{"terminus"}, built.passes.ArchiveSummary.UnrepresentedClasses,
		"the flush must carry the assembled summary onto PassResults, where every renderer reads it")
	assert.Equal(t, 1, built.passes.ArchiveSummary.OverCap)

	// And it reaches the feed, which is the only surface that says a class went
	// unrepresented — the counts already get there from the passes themselves.
	events, err := db.RecentEvents(20)
	require.NoError(t, err)
	var found bool
	for _, e := range events {
		if e.Type == state.EventSmelterFlushed && strings.Contains(e.Message, "classes now unrepresented: terminus") {
			found = true
		}
	}
	assert.True(t, found, "the class list must reach the activity feed: %+v", events)
}

// The operator's half: `forge warden consolidate --force` is how a terminus the
// guard held gets archived anyway, so it is exactly the run that has to name
// the class it took.
func TestConsolidateAnvil_NamesTheClassAForcedSweepTook(t *testing.T) {
	setup := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		writeRulesFile(t, dir, &warden.RulesFile{Rules: []warden.Rule{
			{ID: "terminus", Category: "style", Pattern: "p", Check: "c", Source: warden.SourceList{"manual"}, Added: "2020-01-01"},
		}})
		archive := &warden.Archive{Rules: []warden.ArchivedRule{
			{Rule: warden.Rule{ID: "member-a"}, SupersededBy: "terminus", ArchiveReason: warden.ArchiveReasonDuplicate},
		}}
		require.NoError(t, archive.Save(warden.ArchivePath(dir)))
		return dir
	}
	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)

	forced, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath:            setup(t),
		AnvilName:            "test",
		ArchiveAfterDays:     30,
		AllowArchiveTerminus: true,
		Now:                  now,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"terminus"}, forced.Passes.ArchiveSummary.UnrepresentedClasses,
		"an operator who has just retired a whole class must be told its ID while the archive entry is findable by name")

	// Without --force the guard holds the rule, so nothing left the file and
	// there is no class to name: the two lines are counterparts, never both.
	held, err := ConsolidateAnvil(context.Background(), ConsolidateOptions{
		AnvilPath:        setup(t),
		AnvilName:        "test",
		ArchiveAfterDays: 30,
		Now:              now,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"terminus"}, held.Passes.ProtectedTermini)
	assert.Empty(t, held.Passes.ArchiveSummary.UnrepresentedClasses)
}
