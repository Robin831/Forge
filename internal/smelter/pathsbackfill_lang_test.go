package smelter

import (
	"context"
	"testing"

	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunPathsBackfill_NarrowsToTheRulesOwnLanguage is the end-to-end shape of
// the bug: a Go rule learned from a PR that also touched the frontend must not
// be backfilled with the frontend's globs.
func TestRunPathsBackfill_NarrowsToTheRulesOwnLanguage(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		return []string{
			"internal/daemon/poll.go",
			"web/src/App.tsx",
			"web/src/api/prs.ts",
			"docs/configuration.md",
		}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{
			ID:      "go-1",
			Pattern: "map mutated from a goroutine",
			Check:   "guard it with sync.Mutex",
			Source:  warden.SourceList{"copilot:PR#682"},
		},
		{
			ID:      "ui-1",
			Pattern: "component state derived during render",
			Check:   "use useMemo instead of a second useState in the React component",
			Source:  warden.SourceList{"copilot:PR#682"},
		},
		{
			ID:      "any-1",
			Pattern: "magic number in a conditional",
			Check:   "name it as a constant",
			Source:  warden.SourceList{"copilot:PR#682"},
		},
	}}

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf).Filled
	require.Equal(t, []string{"go-1", "ui-1", "any-1"}, updated)
	assert.Equal(t, []string{"internal/**/*.go"}, rf.Rules[0].Paths)
	assert.Equal(t, []string{"web/**/*.ts", "web/**/*.tsx"}, rf.Rules[1].Paths)
	assert.Equal(t, []string{"docs/**/*.md", "internal/**/*.go", "web/**/*.ts", "web/**/*.tsx"}, rf.Rules[2].Paths,
		"a rule naming no language keeps the PR-derived set, scoped to its areas")
}
