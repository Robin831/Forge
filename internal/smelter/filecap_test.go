package smelter

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
