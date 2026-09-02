package smelter

import (
	"context"
	"testing"

	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// learnedRule builds a rule exactly as the learner writes one: the model's
// answer plus warden.DeriveRulePaths over the files the review comments were
// anchored to. It is the real derivation and not a fixture, because the whole
// property under test is that this call and the one Pass 3 makes are the same
// function — a hand-written Paths list would pass these tests whatever the
// learner actually does.
func learnedRule(id, pattern, check string, commentAnchors []string, source string) warden.Rule {
	r := warden.Rule{ID: id, Pattern: pattern, Check: check, Source: warden.SourceList{source}}
	r.Paths = warden.DeriveRulePaths(r, commentAnchors)
	return r
}

// The property the shared derivation exists for: Pass 3 must not rewrite a rule
// the learner has just placed. The two see different evidence — the learner the
// files a comment was anchored to, Pass 3 the whole of the source PR — so the
// only thing that can make them agree is running the same function over it, and
// then the areas Pass 3 adds are areas the learner's set does not cover, which
// isStrictlyNarrower declines.
//
// The Go rule is the case that used to fail. The learner scoped its evidence
// and stopped, so it wrote `internal/**/*.go` beside the `web/**/*.ts` its
// comments also touched; Pass 3 additionally intersected the extensions with
// the rule's own language and rewrote it to the Go glob alone — a narrowing of
// a rule learned minutes earlier, reported as a backfill finding. Both ends now
// run the ladder, so the learner writes the narrow set itself and the pass has
// nothing to say about it.
func TestPass3IsANoopOnAFreshlyLearnedRule(t *testing.T) {
	prFiles := []string{
		"internal/daemon/poll.go",
		"internal/warden/rules.go",
		"web/src/App.tsx",
		"web/src/api/prs.ts",
		"docs/configuration.md",
	}

	cases := []struct {
		name      string
		rule      warden.Rule
		wantPaths []string
	}{
		{
			name: "language-signalled rule, evidence spanning two languages",
			rule: learnedRule("go-1",
				"map mutated from a goroutine",
				"guard it with sync.Mutex",
				[]string{"internal/daemon/poll.go", "web/src/api/prs.ts"},
				"copilot:PR#682"),
			wantPaths: []string{"internal/**/*.go"},
		},
		{
			name: "rule naming no language keeps its own evidence's areas",
			rule: learnedRule("any-1",
				"magic number in a conditional",
				"name it as a constant",
				[]string{"internal/daemon/poll.go", "docs/configuration.md"},
				"copilot:PR#682"),
			wantPaths: []string{"docs/**/*.md", "internal/**/*.go"},
		},
		{
			name: "comment anchored on a single file of a much larger PR",
			rule: learnedRule("ui-1",
				"component state derived during render",
				"use useMemo instead of a second useState in the React component",
				[]string{"web/src/App.tsx"},
				"copilot:PR#682"),
			wantPaths: []string{"web/**/*.tsx"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withStubFetcher(t, func(context.Context, string, int) ([]string, error) {
				return prFiles, nil
			})

			require.Equal(t, tc.wantPaths, tc.rule.Paths, "the learner's own placement")

			rf := &warden.RulesFile{Rules: []warden.Rule{tc.rule}}
			res := pathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)

			assert.Empty(t, res.Filled, "the learner left no empty Paths to fill")
			assert.Empty(t, res.Narrowed, "the backfill must not re-place a rule the learner placed")
			assert.Equal(t, tc.wantPaths, rf.Rules[0].Paths)
		})
	}
}

// The same agreement stated as an equality rather than as a declined rewrite:
// handed the SAME evidence, the learner's call and Pass 3's return the same
// bytes. This is the half that a difference in the two file sets can never
// explain away — if it fails, the two ends are running different code.
func TestLearnerAndBackfillDeriveTheSameGlobs(t *testing.T) {
	files := []string{
		"internal/daemon/poll.go",
		"web/src/App.tsx",
		"changelog.d/Forge-abc1.md",
	}
	cases := []warden.Rule{
		{Pattern: "map mutated from a goroutine", Check: "guard it with sync.Mutex"},
		{Pattern: "component state derived during render", Check: "use useMemo in the React component"},
		{Pattern: "exported function added", Check: "every user-visible .go change needs a changelog.d entry"},
		{Pattern: "magic number in a conditional", Check: "name it as a constant"},
	}

	for _, rule := range cases {
		t.Run(rule.Pattern, func(t *testing.T) {
			learned := warden.DeriveRulePaths(rule, files)

			withStubFetcher(t, func(context.Context, string, int) ([]string, error) {
				return files, nil
			})
			r := rule
			r.ID = "r-1"
			r.Source = warden.SourceList{"copilot:PR#1"}
			rf := &warden.RulesFile{Rules: []warden.Rule{r}}
			pathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)

			assert.Equal(t, learned, rf.Rules[0].Paths)
		})
	}
}
