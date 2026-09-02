package smelter

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/warden"
)

func TestRepoWideGlob(t *testing.T) {
	for _, tc := range []struct {
		glob string
		want bool
	}{
		{"**/*", true},
		{"**", true},
		{"**/*.go", true},
		{"**/*.tsx", true},
		{"changelog.d/**", false},
		{"internal/**/*.go", false},
		// Opens with `**/` but still names a directory below it.
		{"**/testdata/*.json", false},
		{"docs/configuration.md", false},
	} {
		assert.Equal(t, tc.want, repoWideGlob(tc.glob), tc.glob)
	}
}

func TestIsStrictlyNarrower(t *testing.T) {
	t.Run("a proper subset is narrower", func(t *testing.T) {
		assert.True(t, isStrictlyNarrower(
			[]string{"**/*.go"},
			[]string{"**/*.go", "**/*.md", "**/*.tsx"}))
	})

	t.Run("an equal set is not narrower", func(t *testing.T) {
		// The property that makes a repeated Pass 3 a no-op.
		assert.False(t, isStrictlyNarrower(
			[]string{"**/*.go", "**/*.md"},
			[]string{"**/*.md", "**/*.go"}))
	})

	t.Run("a wider set is not narrower", func(t *testing.T) {
		assert.False(t, isStrictlyNarrower(
			[]string{"**/*.go", "**/*.md"},
			[]string{"**/*.go"}))
	})

	t.Run("non-comparable sets are not narrower", func(t *testing.T) {
		// Fewer globs, but it names files the rule was never gated on: a
		// smaller list is not a smaller scope.
		assert.False(t, isStrictlyNarrower(
			[]string{"**/*.ts"},
			[]string{"**/*.go", "**/*.md"}))
	})

	t.Run("anything narrows a match-everything glob", func(t *testing.T) {
		assert.True(t, isStrictlyNarrower(
			[]string{"**/*.go"},
			[]string{"**/*"}))
		assert.True(t, isStrictlyNarrower(
			[]string{"changelog.d/**"},
			[]string{"**"}))
	})

	t.Run("a repo-wide candidate never replaces located paths", func(t *testing.T) {
		// `**/*.md` matches every markdown file in the repository, while the
		// rule is currently gated on one directory. Fewer globs, wider scope.
		assert.False(t, isStrictlyNarrower(
			[]string{"**/*.md"},
			[]string{"changelog.d/**", "docs/**"}))
	})

	t.Run("an empty side is never narrower", func(t *testing.T) {
		assert.False(t, isStrictlyNarrower(nil, []string{"**/*.go"}))
		assert.False(t, isStrictlyNarrower([]string{"**/*.go"}, nil))
	})
}

func TestMatchesAnySource(t *testing.T) {
	files := []string{"internal/daemon/poll.go", "docs/configuration.md"}

	assert.True(t, matchesAnySource([]string{"**/*.go"}, files))
	assert.True(t, matchesAnySource([]string{"**/*.ts", "**/*.md"}, files),
		"one matching glob is enough")
	assert.False(t, matchesAnySource([]string{"**/*.ts"}, files))
	assert.False(t, matchesAnySource([]string{"**/*.go"}, nil),
		"no source files is no evidence, so no match")
	assert.False(t, matchesAnySource(nil, files))
	assert.False(t, matchesAnySource([]string{"[", "**/*.ts"}, files),
		"an invalid pattern must not match, exactly as at review time")
}

func TestMayNarrow(t *testing.T) {
	assert.True(t, mayNarrow([]string{"**/*.go", "**/*.md"}))
	assert.True(t, mayNarrow([]string{"**/*"}), "everything is narrower than **/*")
	assert.False(t, mayNarrow([]string{"**/*.go"}))
	assert.False(t, mayNarrow(nil))
}

// The case the bead is about: a Go rule learned from a PR that also touched
// documentation carries `**/*.md` beside `**/*.go`, so it joins the Warden
// checklist on every documentation-only diff. Pass 3 used to skip it outright
// because its Paths were not empty.
func TestRunPathsBackfill_NarrowsAnOverBroadRule(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		return []string{"internal/daemon/poll.go", "docs/configuration.md"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{{
		ID:      "go-1",
		Pattern: "map mutated from a goroutine",
		Check:   "guard it with sync.Mutex",
		Source:  warden.SourceList{"copilot:PR#682"},
		Paths:   []string{"**/*.go", "**/*.md"},
	}}}

	result := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Empty(t, result.Filled, "the rule had paths, so nothing was filled")
	assert.Equal(t, []string{"go-1"}, result.Narrowed)
	assert.Equal(t, []string{"**/*.go"}, rf.Rules[0].Paths)
}

// Narrowing is a fixed point: the globs are a function of the rule's text and
// its PRs' files, so the flush after the one that narrowed a rule derives the
// set already on file and changes nothing.
func TestRunPathsBackfill_NarrowingIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		return []string{"internal/a.go", "internal/b.ts", "README.md"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{{
		ID:      "go-1",
		Pattern: "err != nil ignored",
		Check:   "wrap it with fmt.Errorf",
		Source:  warden.SourceList{"copilot:PR#1"},
		Paths:   []string{"**/*.go", "**/*.md", "**/*.ts"},
	}}}

	first := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	require.Equal(t, []string{"go-1"}, first.Narrowed)
	require.Equal(t, []string{"**/*.go"}, rf.Rules[0].Paths)

	second := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Empty(t, second.Narrowed, "the second run derives the set already on file")
	assert.Equal(t, []string{"**/*.go"}, rf.Rules[0].Paths)
}

// A candidate that names files the rule was never gated on is not a narrowing,
// however much shorter the list is. Here the rule is placed in one directory
// and the derivation would replace that with a repo-wide language gate.
func TestRunPathsBackfill_LeavesNonComparablePathsAlone(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		return []string{"internal/daemon/poll.go", "web/src/App.tsx"}, nil
	})

	current := []string{"internal/daemon/**", "web/src/**"}
	rf := &warden.RulesFile{Rules: []warden.Rule{{
		ID:      "placed-1",
		Pattern: "magic number in a conditional",
		Check:   "name it as a constant",
		Source:  warden.SourceList{"copilot:PR#682"},
		Paths:   append([]string(nil), current...),
	}}}

	result := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Empty(t, result.Narrowed)
	assert.Equal(t, current, rf.Rules[0].Paths)
}

// The source-PR guard. The rule's own text names Go, its Paths carry Go beside
// the frontend, and the PR it was learned from reports no file with an
// extension at all — so globsForRule falls back to the inferred `**/*.go`,
// which is strictly narrower and matches nothing the rule was taught from.
// Written in, the rule would be gated on a language its own evidence never
// contained, which warden.FilterRules turns into a rule that never fires again.
func TestRunPathsBackfill_SourceGuardRejectsAnOverNarrowRewrite(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		return []string{"Makefile", "Dockerfile", "LICENSE"}, nil
	})

	current := []string{"**/*.go", "**/*.tsx"}
	rf := &warden.RulesFile{Rules: []warden.Rule{{
		ID:      "go-2",
		Pattern: "goroutine leak",
		Check:   "pass a context.Context and honour cancellation",
		Source:  warden.SourceList{"copilot:PR#7"},
		Paths:   append([]string(nil), current...),
	}}}

	// The candidate really is strictly narrower — the guard, not the
	// narrowness test, is what has to decline it.
	require.True(t, isStrictlyNarrower([]string{"**/*.go"}, current))

	result := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Empty(t, result.Narrowed)
	assert.Equal(t, current, rf.Rules[0].Paths,
		"a rewrite matching none of the rule's own source files must be declined")
}

// The two outcomes are reported apart from each other, because "this rule had
// no gate and now has one" and "this rule stopped being gated on **/*.md" are
// different claims about a rule.
func TestRunPathsBackfill_ReportsFillsAndNarrowingsSeparately(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		return []string{"internal/a.go", "docs/x.md"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{
			ID:      "empty-1",
			Pattern: "err != nil ignored",
			Check:   "wrap it with fmt.Errorf",
			Source:  warden.SourceList{"copilot:PR#1"},
		},
		{
			ID:      "broad-1",
			Pattern: "map mutated from a goroutine",
			Check:   "guard it with sync.Mutex",
			Source:  warden.SourceList{"copilot:PR#1"},
			Paths:   []string{"**/*.go", "**/*.md"},
		},
	}}

	result := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Equal(t, []string{"empty-1"}, result.Filled)
	assert.Equal(t, []string{"broad-1"}, result.Narrowed)
	assert.Equal(t, "backfilled paths on 1 rule(s), narrowed paths on 1 rule(s) for anvil-a",
		result.summary("anvil-a"))
	assert.Empty(t, backfillResult{}.summary("anvil-a"),
		"a run that changed nothing must say nothing")
}
