package warden

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func summaryRules(ids ...string) []Rule {
	rules := make([]Rule, 0, len(ids))
	for _, id := range ids {
		rules = append(rules, Rule{ID: id, Added: "2024-01-01"})
	}
	return rules
}

func staleEntry(id string) ArchivedRule {
	return ArchivedRule{Rule: Rule{ID: id, Added: "2024-01-01"}, ArchiveReason: ArchiveReasonStale}
}

func TestSummarizeArchiveRun_TerminusArchivedNamesClass(t *testing.T) {
	// A -> B -> C: A and B were merged away in earlier runs and C is the
	// terminus carrying their content. This run archives C, so the whole
	// chain is now in the archive and nothing on the file reviews the class.
	archive := []ArchivedRule{
		archivedInto("a", "b"),
		archivedInto("b", "c"),
	}
	archived := []ArchivedRule{staleEntry("c")}

	got := SummarizeArchiveRunWithIndex(
		summaryRules("c", "unrelated"),
		summaryRules("unrelated"),
		archived,
		BuildSupersededByIndex(append(archive, archived...)),
	)

	assert.Equal(t, 1, got.Archived)
	assert.Equal(t, 1, got.Stale)
	assert.Zero(t, got.Duplicate)
	assert.Equal(t, []string{"c"}, got.UnrepresentedClasses)
}

func TestSummarizeArchiveRun_DuplicatesOnlyNamesNothing(t *testing.T) {
	// A run that only folds redundant duplicates into a survivor still on the
	// file has removed no coverage: the merged rule carries it. Naming the
	// members here would report every consolidation as a loss.
	archived := []ArchivedRule{archivedInto("dup-a", "merged"), archivedInto("dup-b", "merged")}

	got := SummarizeArchiveRun(
		summaryRules("dup-a", "dup-b", "merged"),
		summaryRules("merged"),
		archived,
	)

	assert.Equal(t, 2, got.Archived)
	assert.Equal(t, 2, got.Duplicate)
	assert.Zero(t, got.Stale)
	assert.Empty(t, got.UnrepresentedClasses)
}

func TestSummarizeArchiveRun_ChainWithSurvivingMemberNamesNothing(t *testing.T) {
	// The terminus is archived, but a member of its chain is still on the
	// active file — a naive "was this rule a terminus" test would name the
	// class anyway, which is what the chain walk exists to prevent.
	archive := []ArchivedRule{archivedInto("still-active", "terminus")}
	archived := []ArchivedRule{staleEntry("terminus")}

	got := SummarizeArchiveRunWithIndex(
		summaryRules("terminus", "still-active"),
		summaryRules("still-active"),
		archived,
		BuildSupersededByIndex(append(archive, archived...)),
	)

	assert.Equal(t, 1, got.Archived)
	assert.Empty(t, got.UnrepresentedClasses)
}

func TestSummarizeArchiveRun_TerminusMergedForwardNamesNothing(t *testing.T) {
	// The terminus left the file by being merged into a rule that is still on
	// it, so its chain's content moved forward rather than out. The entry
	// carries superseded_by, which is the whole difference between this and
	// the aged-out case.
	archive := []ArchivedRule{archivedInto("old", "terminus")}
	archived := []ArchivedRule{archivedInto("terminus", "successor")}

	got := SummarizeArchiveRunWithIndex(
		summaryRules("terminus", "successor"),
		summaryRules("successor"),
		archived,
		BuildSupersededByIndex(append(archive, archived...)),
	)

	assert.Empty(t, got.UnrepresentedClasses)
}

func TestSummarizeArchiveRun_LoneRuleIsNotAClass(t *testing.T) {
	// Nothing was ever merged into this rule, so archiving it retires one
	// check rather than a class of them. Counting archived rules is exactly
	// what this summary is not.
	got := SummarizeArchiveRun(summaryRules("lonely"), nil, []ArchivedRule{staleEntry("lonely")})

	assert.Equal(t, 1, got.Archived)
	assert.Empty(t, got.UnrepresentedClasses)
}

func TestSummarizeArchiveRun_IgnoresRulesTheRunNeverHeld(t *testing.T) {
	// An entry for a rule that was not on the active set this run started
	// from is not this run's to report as having left it.
	archive := []ArchivedRule{archivedInto("old", "terminus")}
	archived := []ArchivedRule{staleEntry("terminus")}

	got := SummarizeArchiveRunWithIndex(
		summaryRules("something-else"),
		summaryRules("something-else"),
		archived,
		BuildSupersededByIndex(append(archive, archived...)),
	)

	assert.Empty(t, got.UnrepresentedClasses)
}

func TestSummarizeArchiveRun_ChainCycleTerminates(t *testing.T) {
	// Two archive entries naming each other are a cycle in the recorded
	// pointers. The visited set has to end the walk rather than let the shape
	// of the archive decide the answer.
	archive := []ArchivedRule{archivedInto("x", "y"), archivedInto("y", "x")}
	archived := []ArchivedRule{staleEntry("x")}

	done := make(chan ArchiveSummary, 1)
	go func() {
		done <- SummarizeArchiveRunWithIndex(
			summaryRules("x"), nil, archived,
			BuildSupersededByIndex(append(archive, archived...)),
		)
	}()
	select {
	case got := <-done:
		assert.Equal(t, []string{"x"}, got.UnrepresentedClasses)
	case <-time.After(5 * time.Second):
		t.Fatal("chain walk did not terminate on a cycle")
	}
}

func TestSummarizeArchiveRun_OverCapEvictionIsItsOwnReason(t *testing.T) {
	// An eviction is a lost slot, not a retirement, so it is never folded
	// into the stale count — the reading that had a commit subject reporting
	// rules evicted the day they were learned as having aged out.
	got := SummarizeArchiveRun(summaryRules("a", "b"), summaryRules("b"), []ArchivedRule{
		{Rule: Rule{ID: "a"}, ArchiveReason: ArchiveReasonOverCap},
	})

	assert.Equal(t, 1, got.Archived)
	assert.Equal(t, 1, got.OverCap)
	assert.Zero(t, got.Stale)
	assert.Contains(t, got.String(), "1 over-cap")
}

func TestSummarizeArchiveRun_UnrecognisedReasonIsCountedInNoBucket(t *testing.T) {
	got := SummarizeArchiveRun(summaryRules("a"), nil, []ArchivedRule{
		{Rule: Rule{ID: "a"}, ArchiveReason: "some-future-reason"},
	})

	assert.Equal(t, 1, got.Archived)
	assert.Zero(t, got.Stale)
	assert.Zero(t, got.Duplicate)
	assert.Zero(t, got.OverCap)
}

func TestSummarizeArchiveRun_ClassesAreSortedAndDeduplicated(t *testing.T) {
	// The archive entries arrive in whatever order the passes produced them,
	// and the rendered line has to be the same either way.
	archive := []ArchivedRule{archivedInto("p", "zebra"), archivedInto("q", "alpha")}
	archived := []ArchivedRule{staleEntry("zebra"), staleEntry("alpha"), staleEntry("zebra")}

	got := SummarizeArchiveRunWithIndex(
		summaryRules("zebra", "alpha"), nil, archived,
		BuildSupersededByIndex(append(archive, archived...)),
	)

	assert.Equal(t, []string{"alpha", "zebra"}, got.UnrepresentedClasses)
}

func TestArchiveSummaryString(t *testing.T) {
	assert.Equal(t,
		"archived 7 rule(s) (5 stale, 2 duplicate), classes now unrepresented: none",
		ArchiveSummary{Archived: 7, Stale: 5, Duplicate: 2}.String(),
		"a run that checked and found nothing must not read like one written before the check existed")

	assert.Equal(t,
		"archived 3 rule(s) (3 stale, 0 duplicate), classes now unrepresented: alpha, beta",
		ArchiveSummary{Archived: 3, Stale: 3, UnrepresentedClasses: []string{"alpha", "beta"}}.String())

	assert.Equal(t,
		"archived 4 rule(s) (1 stale, 0 duplicate, 3 over-cap), classes now unrepresented: none",
		ArchiveSummary{Archived: 4, Stale: 1, OverCap: 3}.String())
}

func TestArchiveSummaryStringSanitizesRuleIDs(t *testing.T) {
	// A rule ID is whatever the distillation JSON returned and this line
	// reaches daemon.log and a feed row Hearth wraps without stripping.
	line := ArchiveSummary{Archived: 1, Stale: 1,
		UnrepresentedClasses: []string{"bad\x1b[31mid"}}.String()

	assert.NotContains(t, line, "\x1b")
	assert.Contains(t, line, "bad")
}

func TestArchiveSummaryHasSubstance(t *testing.T) {
	assert.False(t, ArchiveSummary{}.HasSubstance(), "a run with nothing to say must say nothing")
	assert.True(t, ArchiveSummary{Archived: 1}.HasSubstance())
	assert.True(t, ArchiveSummary{UnrepresentedClasses: []string{"a"}}.HasSubstance())
}

func TestSummarizeArchiveRun_EmptyRunIsAllZeroes(t *testing.T) {
	got := SummarizeArchiveRun(nil, nil, nil)
	require.Zero(t, got.Archived)
	assert.Empty(t, got.UnrepresentedClasses)
	assert.False(t, got.HasSubstance())
}

// A chain broken in ONE run answers the terminus test at every intermediate
// member, and reported as they stand the class list is a count of chain links.
// The archive holds a -> b; this run folds b into m (Pass 1) and then evicts m
// (the file ceiling, which deliberately does not honour the terminus guard), so
// both b and m end the run archived with nothing live behind them. One class
// went unrepresented, not two, and m is the member to name: it already carries
// b's merged content, so recovering m recovers the class.
func TestSummarizeArchiveRun_ChainBrokenInOneRunIsOneClass(t *testing.T) {
	archive := []ArchivedRule{archivedInto("a", "b")}
	archived := []ArchivedRule{
		archivedInto("b", "m"),
		{Rule: Rule{ID: "m", Added: "2024-01-01"}, ArchiveReason: ArchiveReasonOverCap},
	}

	got := SummarizeArchiveRunWithIndex(
		summaryRules("b", "m"),
		nil,
		archived,
		BuildSupersededByIndex(append(archive, archived...)),
	)

	assert.Equal(t, 2, got.Archived)
	assert.Equal(t, []string{"m"}, got.UnrepresentedClasses,
		"one chain must read as one class, named by its maximal member")
}

// The suppression is against the classes being REPORTED and not against the
// run's archived entries: b's successor m is archived here too, but m's own
// chain moved forward into a live rule, so m is represented and b is not.
// Suppressed on the strength of m being archived, the one class that did go
// unrepresented would go unnamed.
func TestSummarizeArchiveRun_SuccessorArchivedButRepresentedStillNamesTheClass(t *testing.T) {
	archive := []ArchivedRule{archivedInto("a", "b")}
	archived := []ArchivedRule{
		archivedInto("b", "m"),
		archivedInto("m", "live"),
	}

	got := SummarizeArchiveRunWithIndex(
		summaryRules("b", "m", "live"),
		summaryRules("live"),
		archived,
		BuildSupersededByIndex(append(archive, archived...)),
	)

	assert.Equal(t, []string{"b"}, got.UnrepresentedClasses,
		"m moved forward into a live rule, so b is the maximal member with nothing representing it")
}

// The line is a daemon.log record and a one-line activity-feed row, and a
// single file-ceiling run can archive hundreds of rules. The whole set stays on
// UnrepresentedClasses for the surfaces that render it in full.
func TestArchiveSummaryStringCapsTheClassList(t *testing.T) {
	ids := []string{"c1", "c2", "c3", "c4", "c5", "c6", "c7"}
	line := ArchiveSummary{Archived: 7, Stale: 7, UnrepresentedClasses: ids}.String()

	assert.Equal(t,
		"archived 7 rule(s) (7 stale, 0 duplicate), classes now unrepresented: c1, c2, c3, c4, c5 and 2 more",
		line)
	assert.NotContains(t, line, "c6", "past the cap the count stands in for the names")
}
