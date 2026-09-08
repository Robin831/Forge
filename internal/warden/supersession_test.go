package warden

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func archivedInto(id, target string) ArchivedRule {
	return ArchivedRule{
		Rule:          Rule{ID: id, Added: "2024-01-01"},
		SupersededBy:  target,
		ArchiveReason: ArchiveReasonDuplicate,
	}
}

func TestBuildSupersededByIndex_MapsTargetsToTheirSources(t *testing.T) {
	idx := BuildSupersededByIndex([]ArchivedRule{
		archivedInto("a", "merged"),
		archivedInto("b", "merged"),
		archivedInto("c", "other"),
	})

	assert.Equal(t, []string{"a", "b"}, idx["merged"])
	assert.Equal(t, []string{"c"}, idx["other"])
	assert.Empty(t, idx["never-mentioned"])
}

// TestBuildSupersededByIndex_IgnoresEmptyAndDanglingPointers pins what does
// NOT become an entry. An entry with no superseder was archived for staleness
// or the ceiling and names nothing; an entry naming itself is a degenerate
// record and is evidence of its own retirement, not of anything standing
// behind the active rule that shares its ID.
func TestBuildSupersededByIndex_IgnoresEmptyAndDanglingPointers(t *testing.T) {
	idx := BuildSupersededByIndex([]ArchivedRule{
		{Rule: Rule{ID: "stale-one"}, ArchiveReason: ArchiveReasonStale},
		{Rule: Rule{ID: "over-cap-one"}, ArchiveReason: ArchiveReasonOverCap},
		archivedInto("self", "self"),
	})

	assert.Nil(t, idx, "no entry names a rule other than itself, so the index is empty")

	// A dangling pointer — a target that is not on the active file — is still
	// indexed: the index is a read of the archive, and whether the target
	// exists is the caller's question, asked by looking a rule up.
	idx = BuildSupersededByIndex([]ArchivedRule{archivedInto("a", "gone")})
	assert.Equal(t, []string{"a"}, idx["gone"])
	assert.False(t, IsSupersessionTerminus(Rule{ID: "still-here"}, idx))
}

func TestBuildSupersededByIndex_EmptyArchive(t *testing.T) {
	assert.Nil(t, BuildSupersededByIndex(nil))
	assert.Nil(t, BuildSupersededByIndex([]ArchivedRule{}))
}

func TestIsSupersessionTerminus(t *testing.T) {
	idx := BuildSupersededByIndex([]ArchivedRule{archivedInto("a", "merged")})

	assert.True(t, IsSupersessionTerminus(Rule{ID: "merged"}, idx))
	assert.False(t, IsSupersessionTerminus(Rule{ID: "unrelated"}, idx))
	assert.False(t, IsSupersessionTerminus(Rule{ID: ""}, idx), "a rule with no ID is nobody's successor")
	assert.False(t, IsSupersessionTerminus(Rule{ID: "merged"}, nil), "a nil index protects nothing")
}

// TestIsStale_TerminusRuleIsProtectedWithoutOverride is the guard itself: a
// rule that six archived rules were merged into carries their content, and
// archiving it retires the whole chain with nothing left on the active file to
// say what went.
func TestIsStale_TerminusRuleIsProtectedWithoutOverride(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	terminus := Rule{ID: "merged", Added: now.AddDate(0, 0, -400).Format(staleAddedLayout)}

	var archive []ArchivedRule
	for i := range 6 {
		archive = append(archive, archivedInto(fmt.Sprintf("member-%d", i), "merged"))
	}
	idx := BuildSupersededByIndex(archive)
	require.Len(t, idx["merged"], 6)

	cfg := StaleConfig{ArchiveAfterDays: 180, SupersededBy: idx}
	stale, reason := IsStale(terminus, cfg, now)
	assert.False(t, stale, "an aged, inactive terminus is kept by default")
	assert.Equal(t, ReasonProtectedTerminus, reason)

	// The override takes it, and says so with the ordinary retirement reason:
	// the rule was stale all along, the guard was the only thing holding it.
	cfg.AllowArchiveTerminus = true
	stale, reason = IsStale(terminus, cfg, now)
	assert.True(t, stale, "--force archives a terminus")
	assert.Equal(t, ReasonAgedAndInactive, reason)
}

// TestIsStale_TerminusGuardIsCheckedLast keeps the reason informative. A
// terminus rule that is young, or recently used, is kept for THAT reason —
// reported as "protected" it would hide the fact that the sweep never wanted
// it, and would make every young merged rule look like it needed --force.
func TestIsStale_TerminusGuardIsCheckedLast(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	idx := BuildSupersededByIndex([]ArchivedRule{archivedInto("a", "merged")})
	cfg := StaleConfig{ArchiveAfterDays: 180, InactiveAfterDays: 30, SupersededBy: idx}

	young := Rule{ID: "merged", Added: now.AddDate(0, 0, -5).Format(staleAddedLayout)}
	_, reason := IsStale(young, cfg, now)
	assert.Equal(t, ReasonTooYoung, reason)

	used := Rule{
		ID:          "merged",
		Added:       now.AddDate(0, 0, -400).Format(staleAddedLayout),
		LastEmitted: now.AddDate(0, 0, -2).Format(staleAddedLayout),
	}
	_, reason = IsStale(used, cfg, now)
	assert.Equal(t, ReasonRecentActivity, reason)
}

// TestArchiveStale_ProtectedTerminusStaysOnTheFileAndIsNamed is the sweep's
// half. The rule is kept in Active — the guard names a rule, it does not
// remove one — and reported in Protected, because "0 archived" cannot say
// whether a file had nothing stale in it or nothing it was allowed to take.
func TestArchiveStale_ProtectedTerminusStaysOnTheFileAndIsNamed(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	old := func(id string) Rule {
		return Rule{ID: id, Added: now.AddDate(0, 0, -400).Format(staleAddedLayout)}
	}
	fresh := Rule{ID: "fresh", Added: now.AddDate(0, 0, -1).Format(staleAddedLayout)}

	cfg := StaleConfig{
		ArchiveAfterDays: 180,
		SupersededBy:     BuildSupersededByIndex([]ArchivedRule{archivedInto("a", "terminus")}),
	}
	sweep := ArchiveStale([]Rule{fresh, old("terminus"), old("plain")}, cfg, now)

	assert.Equal(t, []string{"fresh", "terminus"}, ruleIDs(sweep.Active))
	assert.Equal(t, []string{"plain"}, archivedIDs(sweep.Archived))
	assert.Equal(t, []string{"terminus"}, ruleIDs(sweep.Protected))

	// With the override, the same input archives both and protects nothing.
	cfg.AllowArchiveTerminus = true
	sweep = ArchiveStale([]Rule{fresh, old("terminus"), old("plain")}, cfg, now)
	assert.Equal(t, []string{"fresh"}, ruleIDs(sweep.Active))
	assert.Equal(t, []string{"terminus", "plain"}, archivedIDs(sweep.Archived))
	assert.Empty(t, sweep.Protected)
}

// TestArchiveStale_NoArchiveBehavesAsBefore is the compatibility floor for the
// guard: an anvil that has never archived anything has a nil index, and the
// sweep must behave exactly as it did before the guard existed rather than
// refusing to run without evidence.
func TestArchiveStale_NoArchiveBehavesAsBefore(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	in := []Rule{{ID: "old", Added: now.AddDate(0, 0, -400).Format(staleAddedLayout)}}

	sweep := ArchiveStale(in, StaleConfig{ArchiveAfterDays: 180}, now)
	assert.Equal(t, []string{"old"}, archivedIDs(sweep.Archived))
	assert.Empty(t, sweep.Protected)
}
