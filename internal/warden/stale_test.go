package warden

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsStale_DisabledWhenThresholdNonPositive(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	r := Rule{ID: "r1", Added: "2020-01-01"}

	assert.False(t, IsStale(r, 0, now))
	assert.False(t, IsStale(r, -1, now))
}

func TestIsStale_NoAddedDate_NotStale(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	r := Rule{ID: "r1"} // Added empty

	assert.False(t, IsStale(r, 30, now))
}

func TestIsStale_MalformedAddedDate_NotStale(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	r := Rule{ID: "r1", Added: "not-a-date"}

	assert.False(t, IsStale(r, 30, now))
}

func TestIsStale_FreshRule_NotStale(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	r := Rule{ID: "r1", Added: "2026-05-01"} // 19 days ago

	assert.False(t, IsStale(r, 30, now), "rule added within threshold should not be stale")
}

func TestIsStale_OldRule_Stale(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// Added 200 days ago, threshold 180 days.
	r := Rule{ID: "r1", Added: now.AddDate(0, 0, -200).Format("2006-01-02")}

	assert.True(t, IsStale(r, 180, now), "rule added past the threshold should be stale")
}

func TestIsStale_AtBoundary_NotStale(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	// Added exactly 30 days ago — boundary is exclusive.
	r := Rule{ID: "r1", Added: now.AddDate(0, 0, -30).Format("2006-01-02")}

	assert.False(t, IsStale(r, 30, now), "boundary should be exclusive — rule equal to threshold is not stale")
}

func TestArchiveStale_PartitionsFreshAndStaleRules(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	fresh := Rule{ID: "fresh", Category: "style", Pattern: "p1", Check: "c1", Added: now.AddDate(0, 0, -10).Format("2006-01-02")}
	stale := Rule{ID: "stale", Category: "style", Pattern: "p2", Check: "c2", Added: now.AddDate(0, 0, -400).Format("2006-01-02")}

	active, archived := ArchiveStale([]Rule{fresh, stale}, 180, now)

	assert.Len(t, active, 1)
	assert.Equal(t, "fresh", active[0].ID)
	assert.Len(t, archived, 1)
	assert.Equal(t, "stale", archived[0].ID)
}

func TestArchiveStale_SetsLastSeenAndReason(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	r := Rule{ID: "old", Category: "style", Pattern: "p", Check: "c", Added: "2020-01-01"}

	_, archived := ArchiveStale([]Rule{r}, 180, now)

	require.Len(t, archived, 1)
	assert.Equal(t, now, archived[0].LastSeen, "LastSeen must be set to now on archived stale rules")
	assert.Equal(t, now, archived[0].ArchivedAt, "ArchivedAt must be set to now on archived stale rules")
	assert.Equal(t, ArchiveReasonStale, archived[0].ArchiveReason)
	assert.Equal(t, "", archived[0].SupersededBy, "stale archive entries have no superseder")
	// The original rule data is carried through unchanged.
	assert.Equal(t, "old", archived[0].ID)
	assert.Equal(t, "style", archived[0].Category)
	assert.Equal(t, "p", archived[0].Pattern)
	assert.Equal(t, "c", archived[0].Check)
	assert.Equal(t, "2020-01-01", archived[0].Added)
}

func TestArchiveStale_PreservesActiveOrder(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	a := Rule{ID: "a", Added: now.AddDate(0, 0, -10).Format("2006-01-02")}
	b := Rule{ID: "b", Added: "2020-01-01"} // stale
	c := Rule{ID: "c", Added: now.AddDate(0, 0, -5).Format("2006-01-02")}
	d := Rule{ID: "d", Added: "2019-01-01"} // stale
	e := Rule{ID: "e", Added: now.AddDate(0, 0, -1).Format("2006-01-02")}

	active, archived := ArchiveStale([]Rule{a, b, c, d, e}, 180, now)

	assert.Equal(t, []string{"a", "c", "e"}, ruleIDs(active))
	assert.Equal(t, []string{"b", "d"}, archivedIDs(archived))
}

func TestArchiveStale_NothingStale_ReturnsOriginalSlice(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	in := []Rule{
		{ID: "a", Added: now.AddDate(0, 0, -10).Format("2006-01-02")},
		{ID: "b", Added: now.AddDate(0, 0, -20).Format("2006-01-02")},
	}

	active, archived := ArchiveStale(in, 180, now)
	assert.Equal(t, in, active)
	assert.Nil(t, archived)
}

func TestArchiveStale_DisabledWhenThresholdNonPositive(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	in := []Rule{{ID: "a", Added: "2020-01-01"}}

	active, archived := ArchiveStale(in, 0, now)
	assert.Equal(t, in, active)
	assert.Nil(t, archived)

	active, archived = ArchiveStale(in, -5, now)
	assert.Equal(t, in, active)
	assert.Nil(t, archived)
}

func TestArchiveStale_EmptyInput(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	active, archived := ArchiveStale(nil, 180, now)
	assert.Nil(t, active)
	assert.Nil(t, archived)
}

func ruleIDs(rules []Rule) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.ID
	}
	return out
}

func archivedIDs(rules []ArchivedRule) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.ID
	}
	return out
}
