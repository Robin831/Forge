package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/smelter"
	"github.com/Robin831/Forge/internal/warden"
)

// overCapArchive builds n archive entries carrying the eviction's own reason,
// which is what keeps them out of the summary's stale count.
func overCapArchive(n int) []warden.ArchivedRule {
	out := make([]warden.ArchivedRule, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, warden.ArchivedRule{ArchiveReason: warden.ArchiveReasonOverCap})
	}
	return out
}

// The active count alone cannot say whether a file is anywhere near the
// ceiling that bounds it, and the eviction line is printed only when something
// was evicted — so a run one rule under the ceiling and a run at half of it
// render identically without this.
func TestActiveAfterLine_NamesTheCeilingInEffect(t *testing.T) {
	line := activeAfterLine(smelter.ConsolidateResult{
		FinalActive: 299,
		Passes:      smelter.PassResults{ActiveRules: 299, RuleCap: 300},
	})
	assert.Equal(t, "299 (ceiling 300)", line)
}

// With eviction disabled the ceiling is omitted rather than printed as zero:
// RuleCap <= 0 means no ceiling ran, and "ceiling 0" reads as a ceiling that
// allows no rules at all.
func TestActiveAfterLine_OmitsADisabledCeiling(t *testing.T) {
	for _, cap := range []int{0, -1} {
		line := activeAfterLine(smelter.ConsolidateResult{
			FinalActive: 42,
			Passes:      smelter.PassResults{ActiveRules: 42, RuleCap: cap},
		})
		assert.Equal(t, "42", line, "cap=%d", cap)
	}
}

// The summary an operator reads after `forge warden consolidate` carries the
// occupancy whether or not the ceiling evicted anything this run.
func TestRenderConsolidateSummary_ReportsOccupancyWithNoEvictions(t *testing.T) {
	var out, errOut bytes.Buffer
	err := renderConsolidateSummary(&out, &errOut, "anvil-a", t.TempDir(), smelter.ConsolidateResult{
		InitialCount: 288,
		FinalActive:  288,
		Passes:       smelter.PassResults{ActiveRules: 288, RuleCap: 300},
	})
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Active after:    288 (ceiling 300)")
	assert.NotContains(t, out.String(), "Evicted:",
		"nothing was evicted, so nothing may claim it was")
}

// A run that DID evict keeps reporting the eviction apart from the staleness
// sweep, and the occupancy is the post-eviction count.
func TestRenderConsolidateSummary_EvictionAndOccupancyAreBothReported(t *testing.T) {
	var out, errOut bytes.Buffer
	err := renderConsolidateSummary(&out, &errOut, "anvil-a", t.TempDir(), smelter.ConsolidateResult{
		InitialCount: 305,
		FinalActive:  300,
		Passes: smelter.PassResults{
			ActiveRules: 300,
			RuleCap:     300,
			Archived:    overCapArchive(5),
		},
	})
	require.NoError(t, err)
	text := out.String()
	assert.Contains(t, text, "Active after:    300 (ceiling 300)")
	assert.Contains(t, text, "Evicted:         5 rule(s) over the file ceiling")
	assert.False(t, strings.Contains(text, "Archived stale:"),
		"an over-cap eviction is not a rule that aged out")
}
