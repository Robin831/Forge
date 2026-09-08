package smelter

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
