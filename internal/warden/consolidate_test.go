package warden

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMergedRule is what the stub runner returns when invoked.
type fakeMergedRule struct {
	ID      string `json:"id"`
	Pattern string `json:"pattern"`
	Check   string `json:"check"`
}

func stubRunner(t *testing.T, response fakeMergedRule) ConsolidationRunner {
	t.Helper()
	return func(_ context.Context, _, _ string) ([]byte, error) {
		data, err := json.Marshal(response)
		require.NoError(t, err)
		return data, nil
	}
}

func TestDistillMergedRule_ParsesResponse(t *testing.T) {
	cluster := []Rule{
		{ID: "r1", Pattern: "missing error", Check: "check err"},
		{ID: "r2", Pattern: "unchecked error", Check: "verify error handling"},
	}
	runner := stubRunner(t, fakeMergedRule{ID: "err-check", Pattern: "missing error check", Check: "verify errors are handled"})

	p, c, id, err := DistillMergedRule(context.Background(), t.TempDir(), cluster, runner)
	require.NoError(t, err)
	assert.Equal(t, "missing error check", p)
	assert.Equal(t, "verify errors are handled", c)
	assert.Equal(t, "err-check", id)
}

func TestDistillMergedRule_RejectsClusterSmallerThanTwo(t *testing.T) {
	_, _, _, err := DistillMergedRule(context.Background(), "", []Rule{{ID: "r1"}}, stubRunner(t, fakeMergedRule{}))
	require.Error(t, err)
}

func TestDistillMergedRule_RejectsMissingCheckField(t *testing.T) {
	cluster := []Rule{{ID: "r1", Check: "a"}, {ID: "r2", Check: "b"}}
	runner := stubRunner(t, fakeMergedRule{ID: "x", Pattern: "p", Check: ""})
	_, _, _, err := DistillMergedRule(context.Background(), "", cluster, runner)
	require.Error(t, err)
}

func TestMergeMetadata_UnionsSourcesAndPicksOldestAdded(t *testing.T) {
	cluster := []Rule{
		{Source: SourceList{"copilot:PR#1", "copilot:PR#2"}, Added: "2025-01-15"},
		{Source: SourceList{"copilot:PR#2", "copilot:PR#3"}, Added: "2024-12-01"},
		{Source: SourceList{}, Added: ""},
	}
	sources, added := MergeMetadata(cluster)
	assert.Equal(t, SourceList{"copilot:PR#1", "copilot:PR#2", "copilot:PR#3"}, sources)
	assert.Equal(t, "2024-12-01", added)
}

func TestMergeMetadata_EmptyClusterReturnsEmpty(t *testing.T) {
	sources, added := MergeMetadata(nil)
	assert.Empty(t, sources)
	assert.Empty(t, added)
}

func TestMergeRule_SetsAllFields(t *testing.T) {
	cluster := []Rule{
		{ID: "r1", Source: SourceList{"PR-1"}, Added: "2024-01-01", Paths: []string{"**/*.go"}},
		{ID: "r2", Source: SourceList{"PR-2"}, Added: "2023-06-01", Paths: []string{"**/*.go", "**/*.ts"}},
	}
	merged := MergeRule(cluster, "style", "shared pattern", "verify x", "err-check", map[string]struct{}{})
	assert.Equal(t, "err-check", merged.ID)
	assert.Equal(t, "style", merged.Category)
	assert.Equal(t, "shared pattern", merged.Pattern)
	assert.Equal(t, "verify x", merged.Check)
	assert.Equal(t, "2023-06-01", merged.Added)
	assert.Equal(t, SourceList{"PR-1", "PR-2"}, merged.Source)
	assert.Equal(t, []string{"**/*.go", "**/*.ts"}, merged.Paths)
}

func TestMergeRule_GeneratesNonCollidingID(t *testing.T) {
	cluster := []Rule{{ID: "r1"}, {ID: "r2"}}
	existing := map[string]struct{}{"err-check": {}, "err-check-2": {}}
	merged := MergeRule(cluster, "style", "p", "c", "err-check", existing)
	assert.Equal(t, "err-check-3", merged.ID)
}

func TestMergeRule_FallbackIDWhenSuggestionEmpty(t *testing.T) {
	cluster := []Rule{{ID: "r1"}, {ID: "r2"}}
	merged := MergeRule(cluster, "style", "p", "c", "", map[string]struct{}{})
	assert.Equal(t, "merged-r1", merged.ID)
}

func TestConsolidate_GroupsByCategoryAndMerges(t *testing.T) {
	rf := &RulesFile{Rules: []Rule{
		// Cluster A in "style"
		{ID: "a1", Category: "style", Pattern: "missing comma trailing", Check: "verify trailing comma", Source: SourceList{"PR-1"}, Added: "2024-01-01"},
		{ID: "a2", Category: "style", Pattern: "trailing comma missing", Check: "ensure trailing comma present", Source: SourceList{"PR-2"}, Added: "2024-02-01"},
		// Different category, no cluster
		{ID: "b1", Category: "security", Pattern: "sql injection risk", Check: "use parameterized queries"},
		// Singleton in "style"
		{ID: "a3", Category: "style", Pattern: "unrelated thing", Check: "different concern"},
	}}

	runner := stubRunner(t, fakeMergedRule{ID: "trailing-comma", Pattern: "trailing comma missing", Check: "verify trailing comma is present"})

	replaced, summary, errs := Consolidate(context.Background(), t.TempDir(), rf, 0.3, runner)
	require.Empty(t, errs)
	require.Len(t, summary, 1)
	require.Len(t, replaced, 2)

	// Verify summary metadata.
	assert.Equal(t, "style", summary[0].Category)
	assert.ElementsMatch(t, []string{"a1", "a2"}, summary[0].ReplacedIDs)
	assert.Equal(t, "trailing-comma", summary[0].Merged.ID)
	// Sources should be unioned and sorted.
	assert.Equal(t, SourceList{"PR-1", "PR-2"}, summary[0].Merged.Source)
	// Added should pick the oldest.
	assert.Equal(t, "2024-01-01", summary[0].Merged.Added)

	// Verify the rules file: a1/a2 gone, b1 + a3 + merged remain.
	ids := []string{}
	for _, r := range rf.Rules {
		ids = append(ids, r.ID)
	}
	assert.ElementsMatch(t, []string{"b1", "a3", "trailing-comma"}, ids)
}

func TestConsolidate_BelowThresholdIsNoop(t *testing.T) {
	rf := &RulesFile{Rules: []Rule{
		{ID: "r1", Category: "style", Pattern: "alpha", Check: "beta"},
		{ID: "r2", Category: "style", Pattern: "gamma", Check: "delta"},
	}}
	replaced, summary, errs := Consolidate(context.Background(), t.TempDir(), rf, 0.99, stubRunner(t, fakeMergedRule{ID: "m", Check: "c"}))
	assert.Empty(t, errs)
	assert.Empty(t, summary)
	assert.Empty(t, replaced)
	assert.Len(t, rf.Rules, 2, "no rules should be removed when nothing clusters")
}

func TestConsolidate_ZeroThresholdIsNoop(t *testing.T) {
	rf := &RulesFile{Rules: []Rule{
		{ID: "r1", Category: "style", Pattern: "alpha", Check: "beta"},
		{ID: "r2", Category: "style", Pattern: "alpha", Check: "beta"},
	}}
	// threshold=0 disables the pass entirely
	replaced, summary, errs := Consolidate(context.Background(), t.TempDir(), rf, 0, stubRunner(t, fakeMergedRule{ID: "m", Check: "c"}))
	assert.Empty(t, errs)
	assert.Empty(t, summary)
	assert.Empty(t, replaced)
}

func TestConsolidate_RunnerErrorIsIsolated(t *testing.T) {
	rf := &RulesFile{Rules: []Rule{
		{ID: "r1", Category: "style", Pattern: "shared word here", Check: "verify shared"},
		{ID: "r2", Category: "style", Pattern: "shared word again", Check: "ensure shared"},
	}}
	failing := ConsolidationRunner(func(_ context.Context, _, _ string) ([]byte, error) {
		return nil, assert.AnError
	})
	replaced, summary, errs := Consolidate(context.Background(), t.TempDir(), rf, 0.3, failing)
	assert.Empty(t, summary)
	assert.Empty(t, replaced)
	assert.Len(t, errs, 1)
	assert.Len(t, rf.Rules, 2, "rules unchanged when AI fails")
}

func TestFormatConsolidationSummary_Empty(t *testing.T) {
	assert.Equal(t, "", FormatConsolidationSummary(nil))
}

func TestFormatConsolidationSummary_RendersBullets(t *testing.T) {
	got := FormatConsolidationSummary([]MergeResult{
		{
			Merged:        Rule{ID: "merged-1"},
			ReplacedIDs:   []string{"r1", "r2"},
			Category:      "style",
			MaxSimilarity: 0.72,
		},
	})
	assert.True(t, strings.Contains(got, "merged-1"))
	assert.True(t, strings.Contains(got, "r1, r2"))
	assert.True(t, strings.Contains(got, "[style]"))
	assert.True(t, strings.Contains(got, "0.72"))
}
