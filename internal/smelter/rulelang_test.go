package smelter

import (
	"context"
	"testing"

	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInferRuleGlobs(t *testing.T) {
	cases := []struct {
		name string
		rule warden.Rule
		want []string
	}{
		{
			name: "go concurrency rule",
			rule: warden.Rule{
				Pattern: "map written from more than one goroutine",
				Check:   "guard shared state with sync.Map or a mutex",
			},
			want: []string{"**/*.go"},
		},
		{
			name: "go error handling rule",
			rule: warden.Rule{
				Pattern: "error returned without context",
				Check:   "wrap with fmt.Errorf and check err != nil at the call site",
			},
			want: []string{"**/*.go"},
		},
		{
			name: "react rule",
			rule: warden.Rule{
				Pattern: "state derived during render",
				Check:   "compute it with useMemo instead of a second useState",
			},
			want: []string{"**/*.ts", "**/*.tsx"},
		},
		{
			name: "typescript rule naming the extension",
			rule: warden.Rule{
				Pattern: "component file without an explicit props type",
				Check:   "declare the props interface in the .tsx file",
			},
			want: []string{"**/*.ts", "**/*.tsx"},
		},
		{
			name: "changelog rule",
			rule: warden.Rule{
				Pattern: "PR without a changelog fragment",
				Check:   "add changelog.d/<bead-id>.md",
			},
			want: []string{"changelog.d/**"},
		},
		{
			name: "rule naming both go and the changelog",
			rule: warden.Rule{
				Pattern: "exported function added with no changelog fragment",
				Check:   "every .go change that is user visible needs one",
			},
			want: []string{"**/*.go", "changelog.d/**"},
		},
		{
			name: "generic rule names no language",
			rule: warden.Rule{
				Pattern: "magic number in a conditional",
				Check:   "name it as a constant",
			},
			want: nil,
		},
		{
			name: "empty rule text",
			rule: warden.Rule{},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, inferRuleGlobs(ruleText(tc.rule)))
		})
	}
}

func TestGlobsForRule(t *testing.T) {
	goRule := warden.Rule{
		Pattern: "shared map mutated from a goroutine",
		Check:   "use sync.Map or hold a mutex across the write",
	}
	frontendRule := warden.Rule{
		Pattern: "effect without a dependency array",
		Check:   "React re-runs useEffect on every render otherwise",
	}
	changelogRule := warden.Rule{
		Pattern: "PR without a changelog fragment",
		Check:   "add a file under changelog.d",
	}
	genericRule := warden.Rule{
		Pattern: "magic number in a conditional",
		Check:   "name it as a constant",
	}

	cases := []struct {
		name  string
		rule  warden.Rule
		files []string
		want  []string
	}{
		{
			name:  "go rule from a mixed go and tsx PR keeps only go",
			rule:  goRule,
			files: []string{"internal/daemon/poll.go", "web/src/App.tsx", "changelog.d/Forge-abc1.md"},
			want:  []string{"**/*.go"},
		},
		{
			name:  "frontend rule from a mixed PR keeps only the frontend globs",
			rule:  frontendRule,
			files: []string{"internal/daemon/poll.go", "web/src/api/prs.ts", "web/src/App.tsx"},
			want:  []string{"**/*.ts", "**/*.tsx"},
		},
		{
			name:  "frontend rule keeps only the frontend extension the PR touched",
			rule:  frontendRule,
			files: []string{"internal/daemon/poll.go", "web/src/api/prs.ts"},
			want:  []string{"**/*.ts"},
		},
		{
			name:  "changelog rule from a mixed PR keeps the directory glob",
			rule:  changelogRule,
			files: []string{"internal/daemon/poll.go", "web/src/App.tsx", "changelog.d/Forge-abc1.md"},
			want:  []string{"changelog.d/**"},
		},
		{
			name:  "no signal falls back to the PR-derived set",
			rule:  genericRule,
			files: []string{"internal/daemon/poll.go", "docs/configuration.md"},
			want:  []string{"**/*.go", "**/*.md"},
		},
		{
			name:  "inferred language the PR never touched is kept rather than emptied",
			rule:  goRule,
			files: []string{"web/src/api/prs.ts"},
			want:  []string{"**/*.go"},
		},
		{
			name:  "no files and no signal yields nothing",
			rule:  genericRule,
			files: nil,
			want:  nil,
		},
		{
			name:  "no files still yields the inferred set",
			rule:  goRule,
			files: nil,
			want:  []string{"**/*.go"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, globsForRule(tc.rule, tc.files))
		})
	}
}

// TestGlobsForRuleIsDeterministic pins the ordering guarantee the encoded
// warden-rules.yaml depends on: the same inputs must produce the same slice,
// whatever order the PR reported its files in.
func TestGlobsForRuleIsDeterministic(t *testing.T) {
	rule := warden.Rule{
		Pattern: "exported function added with no changelog fragment",
		Check:   "every .go change that is user visible needs a changelog.d entry",
	}
	first := globsForRule(rule, []string{"a.go", "b.ts", "changelog.d/x.md"})
	second := globsForRule(rule, []string{"changelog.d/x.md", "b.ts", "a.go"})
	assert.Equal(t, first, second)
	assert.Equal(t, []string{"**/*.go", "changelog.d/**"}, first)
}

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

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	require.Equal(t, []string{"go-1", "ui-1", "any-1"}, updated)
	assert.Equal(t, []string{"**/*.go"}, rf.Rules[0].Paths)
	assert.Equal(t, []string{"**/*.ts", "**/*.tsx"}, rf.Rules[1].Paths)
	assert.Equal(t, []string{"**/*.go", "**/*.md", "**/*.ts", "**/*.tsx"}, rf.Rules[2].Paths,
		"a rule naming no language keeps the PR-derived set")
}
