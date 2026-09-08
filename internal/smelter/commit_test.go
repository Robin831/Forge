package smelter

import (
	"fmt"
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

// A narrowed rule is not a backfilled one: its Paths field was never empty, so
// a subject reporting it as a backfill describes work that did not happen.
func TestBuildCommitMessage_NarrowedIsReportedApartFromBackfilled(t *testing.T) {
	passes := PassResults{Backfilled: []string{"r1"}, Narrowed: []string{"r2", "r3"}}
	msg := buildCommitMessage(passes)
	assert.Contains(t, msg, "backfill paths on 1 rule(s)")
	assert.Contains(t, msg, "narrow paths on 2 rule(s)")
	assert.Contains(t, msg, "Backfilled: 1 rule(s)")
	assert.Contains(t, msg, "Narrowed: 2 rule(s)")
	assert.Contains(t, msg, "- r2")
	assert.Contains(t, msg, "- r3")
}

func TestBuildCommitMessage_NarrowedOnlyStillCommits(t *testing.T) {
	passes := PassResults{Narrowed: []string{"r1"}}
	assert.True(t, passes.HasChanges(),
		"a run whose only outcome is a narrowing has a changed rules file")
	msg := buildCommitMessage(passes)
	assert.Contains(t, msg, "narrow paths on 1 rule(s)")
	assert.NotContains(t, msg, "Backfilled:")
}

func TestBuildCommitMessage_ConsolidatedEmptyReplacedIDs(t *testing.T) {
	passes := PassResults{
		Consolidated: []warden.MergeResult{
			{Merged: warden.Rule{ID: "m"}, ReplacedIDs: []string{"", "valid-id"}, Category: "style", MaxSimilarity: 0.5},
		},
	}
	msg := buildCommitMessage(passes)
	assert.Contains(t, msg, "[style] m ← (no id), valid-id (sim=0.50)",
		"empty replaced IDs should render as (no id), not as blank")
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

// The guard's whole value is that the rules it holds are NAMED: "0 archived"
// reads identically for a file with nothing stale in it and one whose every
// stale rule is holding a supersession chain. Each of the surfaces that names
// them is asserted here, because dropping one is otherwise a silent regression
// back to a sweep that quietly keeps rules and says nothing.
func TestBuildCommitMessage_ProtectedTerminiSection(t *testing.T) {
	msg := buildCommitMessage(PassResults{ProtectedTermini: []string{"rule-a", "rule-b"}})

	assert.Contains(t, msg, "Protected (aged and inactive, kept as supersession termini): 2 rule(s)")
	assert.Contains(t, msg, "- rule-a")
	assert.Contains(t, msg, "- rule-b")
	assert.Contains(t, msg, "warden.allow_archive_terminus")
	assert.Contains(t, msg, "forge warden consolidate --force")
}

// The header and bullets come from formatIDSection, the one renderer of a
// labelled rule-ID list — so a change to how an ID is rendered reaches this
// section too, rather than every section but this one.
func TestFormatProtectedTerminiSection_UsesTheSharedIDSection(t *testing.T) {
	ids := []string{"rule-a", "rule-b"}
	shared := formatIDSection("Protected (aged and inactive, kept as supersession termini)", ids)

	assert.True(t, strings.HasPrefix(formatProtectedTerminiSection(ids), shared),
		"the section must open with the shared renderer's output verbatim")
}

// A rule ID is whatever the distillation JSON returned and nothing validates a
// character of it, while this body is published under Forge's own GitHub
// identity — so the section is only safe if displayID actually reaches it.
func TestBuildCommitMessage_ProtectedTerminiSanitizesRuleIDs(t *testing.T) {
	msg := buildCommitMessage(PassResults{
		ProtectedTermini: []string{"bad`rule\n@org/team"},
	})

	assert.Contains(t, msg, "- bad?rule?org/team")
	for _, line := range strings.Split(msg, "\n") {
		assert.NotContains(t, line, "@org/team", "an unsanitized mention must not survive")
	}
}

// The PR body is the surface a reviewer reads before deciding whether the
// chain can end, so it carries the same list plus the remedy.
func TestBuildPRBody_NamesProtectedTerminiAndSanitizesThem(t *testing.T) {
	body := buildPRBody(PassResults{
		Added:            []string{"new-rule"},
		ProtectedTermini: []string{"terminus-1", "bad`rule\n@org/team"},
	})

	assert.Contains(t, body, "**2 rule(s) were aged and inactive but not archived.**")
	assert.Contains(t, body, "`terminus-1`")
	assert.Contains(t, body, "`bad?rule?org/team`")
	assert.Contains(t, body, "`warden.allow_archive_terminus`")
}

// The flush's own one-line summary: without it a run that held a chain reads
// exactly like a run with nothing stale on the file.
func TestPassResultsSummary_ReportsProtectedTermini(t *testing.T) {
	assert.Contains(t,
		passResultsSummary(PassResults{ProtectedTermini: []string{"a", "b"}}),
		"2 kept as supersession termini")
}

// The one-line form counts the whole set the sweep held and names only the
// rules this announcement is about, since the two are different quantities and
// the noun phrase is a total. Rendered from the fresh subset it would state a
// smaller sweep than the commit body of the same run.
func TestProtectedTerminiLine_CountsTheSetAndNamesTheNewlyHeld(t *testing.T) {
	rules := func(ids ...string) []warden.Rule {
		out := make([]warden.Rule, 0, len(ids))
		for _, id := range ids {
			out = append(out, warden.Rule{ID: id})
		}
		return out
	}

	all := rules("a", "b", "c")

	line := protectedTerminiLine("munin", all, rules("c"))
	assert.Contains(t, line, "Kept 3 supersession terminus rules for munin (1 newly held: c):")
	assert.Contains(t, line, "warden.allow_archive_terminus")

	assert.NotContains(t, protectedTerminiLine("munin", all, all), "newly held",
		"a first announcement holds nothing back, so the total already says it")

	assert.Contains(t, protectedTerminiLine("munin", rules("a"), rules("a")),
		"Kept 1 supersession terminus rule for munin:")
}

// The named IDs are the model's text and the set is bounded only by how many
// termini a file holds, while the line is one log record and one feed row.
func TestProtectedTerminiLine_SanitizesAndCapsTheNamedIDs(t *testing.T) {
	all := make([]warden.Rule, 9)
	fresh := make([]warden.Rule, 0, 8)
	for i := range all {
		all[i] = warden.Rule{ID: fmt.Sprintf("rule-%d", i)}
		if i > 0 {
			fresh = append(fresh, all[i])
		}
	}
	fresh[0].ID = "bad`rule\n@org/team"

	line := protectedTerminiLine("munin", all, fresh)

	assert.Contains(t, line, "Kept 9 supersession terminus rules for munin (8 newly held: ")
	assert.Contains(t, line, "bad?rule?org/team", "a rule ID is model text, sanitized like every other rendering")
	assert.Contains(t, line, "and 3 more", "the cap says what it left out rather than trailing off")
	assert.NotContains(t, line, "rule-8")
	assert.Equal(t, 1, len(strings.Split(line, "\n")), "the log and the feed row read this as one line")
}

// A protected terminus left the file exactly as the sweep found it, so it must
// not make the flush commit and push an unchanged rules file. Pinned here
// because the disjunction is one edit away from including it.
func TestPassResults_ProtectedTerminiAloneAreNotAChange(t *testing.T) {
	assert.False(t, PassResults{ProtectedTermini: []string{"terminus"}}.HasChanges())
}
