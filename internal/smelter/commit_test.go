package smelter

import (
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
)

func TestPassResults_HasChanges(t *testing.T) {
	t.Run("empty is false", func(t *testing.T) {
		assert.False(t, PassResults{}.HasChanges())
	})
	t.Run("added alone is true", func(t *testing.T) {
		assert.True(t, PassResults{Added: []string{"r1"}}.HasChanges())
	})
	t.Run("consolidated alone is true", func(t *testing.T) {
		assert.True(t, PassResults{Consolidated: []warden.MergeResult{{Merged: warden.Rule{ID: "m"}}}}.HasChanges())
	})
	t.Run("archived alone is true", func(t *testing.T) {
		assert.True(t, PassResults{Archived: []warden.ArchivedRule{{Rule: warden.Rule{ID: "r1"}}}}.HasChanges())
	})
	t.Run("backfilled alone is true", func(t *testing.T) {
		assert.True(t, PassResults{Backfilled: []string{"r1"}}.HasChanges())
	})
}

func TestBuildCommitMessage_AllSectionsPopulated(t *testing.T) {
	passes := PassResults{
		Added: []string{"new-rule-1", "new-rule-2"},
		Consolidated: []warden.MergeResult{
			{
				Merged:        warden.Rule{ID: "merged-style"},
				ReplacedIDs:   []string{"old-1", "old-2"},
				Category:      "style",
				MaxSimilarity: 0.85,
			},
		},
		Archived: []warden.ArchivedRule{
			{
				Rule:          warden.Rule{ID: "ancient-rule"},
				ArchiveReason: warden.ArchiveReasonStale,
				LastSeen:      time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		Backfilled: []string{"path-rule-1"},
	}

	msg := buildCommitMessage(passes)

	// Subject: single line ending with [no-changelog], lists all four actions.
	lines := strings.SplitN(msg, "\n\n", 2)
	require := assert.New(t)
	require.Len(lines, 2, "expected a body separated by a blank line")
	subject := lines[0]
	require.True(strings.HasPrefix(subject, "forge: "), "subject should start with 'forge: '")
	require.True(strings.HasSuffix(subject, "[no-changelog]"), "subject must end in [no-changelog]")
	require.Contains(subject, "learn 2 warden rule(s)")
	require.Contains(subject, "consolidate 1 cluster(s)")
	require.Contains(subject, "archive 1 stale rule(s)")
	require.Contains(subject, "backfill paths on 1 rule(s)")

	body := lines[1]
	require.Contains(body, "Added: 2 rule(s)")
	require.Contains(body, "- new-rule-1")
	require.Contains(body, "- new-rule-2")
	require.Contains(body, "Consolidated: 1 cluster(s)")
	require.Contains(body, "[style] merged-style ← old-1, old-2 (sim=0.85)")
	require.Contains(body, "Archived: 1 rule(s)")
	require.Contains(body, "- ancient-rule (stale)")
	require.Contains(body, "Backfilled: 1 rule(s)")
	require.Contains(body, "- path-rule-1")
}

func TestBuildCommitMessage_OmitsEmptySections(t *testing.T) {
	passes := PassResults{Added: []string{"r1"}}
	msg := buildCommitMessage(passes)

	assert.Contains(t, msg, "Added: 1 rule(s)")
	assert.Contains(t, msg, "- r1")
	assert.NotContains(t, msg, "Consolidated:")
	assert.NotContains(t, msg, "Archived:")
	assert.NotContains(t, msg, "Backfilled:")
}

func TestBuildCommitMessage_AddedOnly_SubjectMatchesLegacy(t *testing.T) {
	passes := PassResults{Added: []string{"r1", "r2", "r3"}}
	subject := strings.SplitN(buildCommitMessage(passes), "\n", 2)[0]
	assert.Equal(t, "forge: learn 3 warden rule(s) [no-changelog]", subject)
}

func TestBuildCommitMessage_ConsolidatedOnly(t *testing.T) {
	passes := PassResults{
		Consolidated: []warden.MergeResult{
			{Merged: warden.Rule{ID: "m"}, ReplacedIDs: []string{"a", "b"}, Category: "style", MaxSimilarity: 0.5},
		},
	}
	msg := buildCommitMessage(passes)
	assert.Contains(t, msg, "consolidate 1 cluster(s)")
	assert.Contains(t, msg, "Consolidated: 1 cluster(s)")
	assert.NotContains(t, msg, "Added:")
	assert.NotContains(t, msg, "Archived:")
	assert.NotContains(t, msg, "Backfilled:")
}

func TestBuildCommitMessage_ArchivedOnly_PicksUpReason(t *testing.T) {
	passes := PassResults{
		Archived: []warden.ArchivedRule{
			{Rule: warden.Rule{ID: "r1"}, ArchiveReason: warden.ArchiveReasonStale},
		},
	}
	msg := buildCommitMessage(passes)
	assert.Contains(t, msg, "Archived: 1 rule(s)")
	assert.Contains(t, msg, "- r1 (stale)")
}

func TestBuildCommitMessage_BackfilledOnly(t *testing.T) {
	passes := PassResults{Backfilled: []string{"r1", "r2"}}
	msg := buildCommitMessage(passes)
	assert.Contains(t, msg, "backfill paths on 2 rule(s)")
	assert.Contains(t, msg, "Backfilled: 2 rule(s)")
	assert.Contains(t, msg, "- r1")
	assert.Contains(t, msg, "- r2")
}

func TestBuildCommitMessage_NoPasses_FallbackSubject(t *testing.T) {
	// Defensive path — callers should not invoke buildCommitMessage with no
	// changes, but the function must still produce a non-empty subject.
	msg := buildCommitMessage(PassResults{})
	assert.True(t, strings.HasPrefix(msg, "forge: "))
	assert.True(t, strings.HasSuffix(msg, "[no-changelog]"))
	// No body when nothing happened.
	assert.NotContains(t, msg, "\n\n")
}

func TestBuildCommitMessage_MissingCategoryRendersPlaceholder(t *testing.T) {
	passes := PassResults{
		Consolidated: []warden.MergeResult{
			{Merged: warden.Rule{ID: "m"}, ReplacedIDs: []string{"a", "b"}, Category: "", MaxSimilarity: 0.4},
		},
	}
	msg := buildCommitMessage(passes)
	assert.Contains(t, msg, "[(no category)] m ← a, b (sim=0.40)")
}

func TestBuildCommitMessage_MissingIDsRenderPlaceholder(t *testing.T) {
	passes := PassResults{
		Added:      []string{""},
		Backfilled: []string{""},
		Archived:   []warden.ArchivedRule{{Rule: warden.Rule{ID: ""}, ArchiveReason: ""}},
	}
	msg := buildCommitMessage(passes)
	assert.Contains(t, msg, "- (no id)")
	assert.Contains(t, msg, "- (no id) (stale)", "archive reason falls back to 'stale' when empty")
}
