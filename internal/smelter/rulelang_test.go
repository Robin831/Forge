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

// TestInferRuleGlobsIgnoresGenericProse is the false-positive boundary of
// languageSignals, and the assertion that fails first if a signal is ever
// broadened back to a bare English word. Each of these rules is
// language-agnostic or about the frontend, and each was attributed to Go (or
// to the frontend) by the `nil`, `defer`, `chan` and `props` signals this
// table used to carry — a misfire with no symptom, since a rule gated on a
// language its diffs never contain simply stops appearing in reviews.
func TestInferRuleGlobsIgnoresGenericProse(t *testing.T) {
	cases := []struct {
		name string
		rule warden.Rule
	}{
		{
			name: "defer in ordinary review prose",
			rule: warden.Rule{
				Pattern: "do not defer validation to the client",
				Check:   "validate in the form component before submit",
			},
		},
		{
			name: "nil in ordinary review prose",
			rule: warden.Rule{
				Pattern: "avoid rendering a nil value",
				Check:   "guard the optional field",
			},
		},
		{
			name: "chan as an abbreviation, not the keyword",
			rule: warden.Rule{
				Pattern: "channel names in the UI must be escaped",
				Check:   "escape the chan name",
			},
		},
		{
			name: "props without any other frontend token",
			rule: warden.Rule{
				Pattern: "props drilled through three levels",
				Check:   "pass them explicitly instead",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Nil(t, inferRuleGlobs(ruleText(tc.rule)),
				"generic review prose must not be read as naming a language")
		})
	}
}

// TestGlobsForRuleNeverGatesOnAnUncorroboratedLanguage pins the consequence of
// a misfire: even if a signal is broadened and fires on a rule the source PR's
// files contradict, the rule must come out gated on what the PR actually
// touched — never on the language it never contained, which warden.FilterRules
// turns into a rule that silently never fires again.
func TestGlobsForRuleNeverGatesOnAnUncorroboratedLanguage(t *testing.T) {
	tsFiles := []string{"web/src/App.tsx", "web/src/api/prs.ts"}
	goRule := warden.Rule{
		Pattern: "map mutated from a goroutine",
		Check:   "hold a sync.Mutex across the write",
	}

	got := globsForRule(goRule, tsFiles)
	assert.Equal(t, []string{"**/*.ts", "**/*.tsx"}, got)
	assert.NotContains(t, got, "**/*.go",
		"a language the PR's own files contradict must not become the rule's gate")
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
	goChangelogRule := warden.Rule{
		Pattern: "exported function added with no changelog fragment",
		Check:   "every .go change that is user visible needs one",
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
			name:  "changelog rule adds the directory glob to the PR-derived set",
			rule:  changelogRule,
			files: []string{"internal/daemon/poll.go", "web/src/App.tsx", "changelog.d/Forge-abc1.md"},
			want:  []string{"**/*.go", "**/*.md", "**/*.tsx", "changelog.d/**"},
		},
		{
			name:  "changelog glob is dropped when the source PR touched no fragment",
			rule:  changelogRule,
			files: []string{"internal/daemon/poll.go"},
			want:  []string{"**/*.go"},
		},
		{
			name:  "a directory glob is additive to a corroborated language glob",
			rule:  goChangelogRule,
			files: []string{"internal/daemon/poll.go", "web/src/App.tsx", "changelog.d/Forge-abc1.md"},
			want:  []string{"**/*.go", "changelog.d/**"},
		},
		{
			name:  "no signal falls back to the PR-derived set",
			rule:  genericRule,
			files: []string{"internal/daemon/poll.go", "docs/configuration.md"},
			want:  []string{"**/*.go", "**/*.md"},
		},
		{
			name:  "inferred language the PR never touched falls back to the PR's own set",
			rule:  goRule,
			files: []string{"web/src/api/prs.ts"},
			want:  []string{"**/*.ts"},
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
