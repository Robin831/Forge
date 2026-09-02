package smelter

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractPRNumber(t *testing.T) {
	cases := []struct {
		name   string
		source string
		wantN  int
		wantOK bool
	}{
		{"plain copilot ref", "copilot:PR#130", 130, true},
		{"larger number", "copilot:PR#1024", 1024, true},
		{"with surrounding text", "from copilot:PR#42 (merged 2026-01-01)", 42, true},
		{"quench source is not copilot", "quench:PR#7", 0, false},
		{"manual entry", "manual", 0, false},
		{"empty string", "", 0, false},
		{"missing number", "copilot:PR#", 0, false},
		{"wrong delimiter", "copilot:PR/130", 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, ok := extractPRNumber(tc.source)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantN, n)
		})
	}
}

func TestExtractPRNumbers(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   []int
	}{
		{"single ref", "copilot:PR#130", []int{130}},
		{"comma-separated two refs", "copilot:PR#1, copilot:PR#2", []int{1, 2}},
		{"space-separated two refs", "copilot:PR#10 copilot:PR#20", []int{10, 20}},
		{"three refs mixed delimiters", "copilot:PR#3, copilot:PR#5 copilot:PR#7", []int{3, 5, 7}},
		{"deduplicated when same token repeated", "copilot:PR#42, copilot:PR#42", []int{42}},
		{"no copilot tokens", "quench:PR#7", nil},
		{"empty string", "", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractPRNumbers(tc.source)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestExtGlobs(t *testing.T) {
	t.Run("derives unique globs sorted", func(t *testing.T) {
		files := []string{
			"cmd/forge/main.go",
			"internal/smelter/smelter.go",
			"web/src/app.ts",
			"web/src/components/Foo.ts",
			"docs/README.md",
		}
		globs := warden.ExtGlobs(files)
		// Deduped and sorted.
		assert.Equal(t, []string{"**/*.go", "**/*.md", "**/*.ts"}, globs)
	})

	t.Run("ignores files without an extension", func(t *testing.T) {
		files := []string{"Makefile", "LICENSE", ".gitignore"}
		// ".gitignore" returns ".gitignore" from filepath.Ext (treated as extension),
		// but we intentionally accept any leading-dot extension. The bare files
		// (Makefile, LICENSE) have no extension and should be skipped. The
		// dotfile contributes a glob.
		globs := warden.ExtGlobs(files)
		assert.Equal(t, []string{"**/*.gitignore"}, globs)
	})

	t.Run("empty input returns nil", func(t *testing.T) {
		assert.Nil(t, warden.ExtGlobs(nil))
		assert.Nil(t, warden.ExtGlobs([]string{}))
	})

	t.Run("all files extensionless returns nil", func(t *testing.T) {
		assert.Nil(t, warden.ExtGlobs([]string{"Makefile", "Dockerfile"}))
	})

	t.Run("dot-only filename is skipped", func(t *testing.T) {
		// filepath.Ext("foo.") returns ".", which is a degenerate extension.
		assert.Nil(t, warden.ExtGlobs([]string{"foo."}))
	})
}

// withStubFetcher temporarily replaces the package fetchChangedFiles hook so
// tests do not invoke the gh CLI. Restored on test cleanup.
func withStubFetcher(t *testing.T, fn func(context.Context, string, int) ([]string, error)) {
	t.Helper()
	prev := fetchChangedFiles
	fetchChangedFiles = fn
	t.Cleanup(func() { fetchChangedFiles = prev })
}

// A rule whose single glob already names a location admits no narrower set — a
// candidate has to be covered by what is on file, and an area-scoped glob covers
// only itself. So the pass declines it from the globs alone, before spending the
// PR lookup that could not have changed anything.
func TestRunPathsBackfill_SkipsRuleWhoseSinglePathCannotNarrow(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	var calls int
	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		calls++
		return []string{"internal/a.go"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#1"}, Paths: []string{"internal/**/*.go"}},
	}}

	result := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Empty(t, result.Filled)
	assert.Empty(t, result.Narrowed)
	assert.Equal(t, 0, calls, "a rule that cannot be narrowed must cost no PR lookup")
	assert.Equal(t, []string{"internal/**/*.go"}, rf.Rules[0].Paths, "Paths must not be mutated")
}

// The mirror image, and the population this scoping exists for: a rule gated on
// one BARE language glob names no location at all, so it does admit a narrower
// set and is worth the lookup. Held to the old "a single glob covers only
// itself" rule it would have been skipped before any fetch and never placed.
func TestRunPathsBackfill_NarrowsARuleGatedOnOneBareLanguageGlob(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		return []string{"api/Controllers/Orders.cs", "api/Services/Pricing.cs"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#1"}, Paths: []string{"**/*.cs"}},
	}}

	result := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Equal(t, []string{"r1"}, result.Narrowed)
	assert.Equal(t, []string{"api/**/*.cs"}, rf.Rules[0].Paths)
}

func TestRunPathsBackfill_PopulatesPathsFromPR(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		assert.Equal(t, 42, prNum)
		return []string{
			"cmd/forge/main.go",
			"internal/smelter/paths.go",
			"web/src/app.ts",
		}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#42"}},
	}}

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf).Filled
	assert.Equal(t, []string{"r1"}, updated)
	assert.Equal(t, []string{"cmd/**/*.go", "internal/**/*.go", "web/**/*.ts"}, rf.Rules[0].Paths,
		"one glob per (area, extension) pair the PR actually touched")
}

func TestRunPathsBackfill_UnionsAcrossMultiplePRs(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		switch prNum {
		case 1:
			return []string{"internal/a.go", "internal/b.go"}, nil
		case 2:
			return []string{"web/c.ts", "web/d.ts"}, nil
		}
		t.Fatalf("unexpected PR #%d", prNum)
		return nil, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#1", "copilot:PR#2"}},
	}}

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf).Filled
	assert.Equal(t, []string{"r1"}, updated)
	assert.Equal(t, []string{"internal/**/*.go", "web/**/*.ts"}, rf.Rules[0].Paths)
}

func TestRunPathsBackfill_SkipsNonCopilotSources(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	var calls int
	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		calls++
		return []string{"a.go"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"quench:PR#7"}},
		{ID: "r2", Source: warden.SourceList{"manual"}},
	}}

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf).Filled
	assert.Empty(t, updated)
	assert.Equal(t, 0, calls, "rules without copilot sources must not trigger fetcher")
	assert.Empty(t, rf.Rules[0].Paths)
	assert.Empty(t, rf.Rules[1].Paths)
}

func TestRunPathsBackfill_FetchErrorLeavesRuleUnchanged(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		return nil, errors.New("gh exploded")
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#42"}},
	}}

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf).Filled
	assert.Empty(t, updated, "fetch errors must not count as successful backfills")
	assert.Empty(t, rf.Rules[0].Paths, "Paths must stay empty on error so a future flush can retry")
}

func TestRunPathsBackfill_PartialFetchSucceedsWhenOnePRWorks(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		if prNum == 1 {
			return nil, errors.New("gh exploded")
		}
		return []string{"internal/foo.go"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#1", "copilot:PR#2"}},
	}}

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf).Filled
	assert.Equal(t, []string{"r1"}, updated)
	assert.Equal(t, []string{"internal/**/*.go"}, rf.Rules[0].Paths)
}

func TestRunPathsBackfill_NoExtensionsLeavesRuleUnchanged(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		return []string{"Makefile", "LICENSE"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#42"}},
	}}

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf).Filled
	assert.Empty(t, updated)
	assert.Empty(t, rf.Rules[0].Paths)
}

func TestRunPathsBackfill_Idempotency_RepeatedRunIsNoop(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	var calls int
	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		calls++
		return []string{"internal/foo.go"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#1"}},
	}}

	// First run: populates Paths.
	first := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf).Filled
	require.Equal(t, []string{"r1"}, first)
	require.Equal(t, []string{"internal/**/*.go"}, rf.Rules[0].Paths)

	// Second run: the rule now carries a single glob that nothing narrower can
	// be covered by, so the pass declines it before the lookup and the stub
	// fetcher is not called again.
	prevCalls := calls
	second := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Empty(t, second.Filled, "second run must report no fills")
	assert.Empty(t, second.Narrowed, "second run must report no narrowings")
	assert.Equal(t, prevCalls, calls, "second run must not invoke the fetcher")
}

func TestRunPathsBackfill_HonorsCanceledContext(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	var calls int
	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		calls++
		return []string{"a.go"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#1"}},
		{ID: "r2", Source: warden.SourceList{"copilot:PR#2"}},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	updated := s.runPathsBackfill(ctx, t.TempDir(), "anvil-a", rf).Filled
	assert.Empty(t, updated)
	assert.Equal(t, 0, calls, "canceled context must short-circuit before any fetch")
}

func TestRunPathsBackfill_MultiTokenSourceString(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		switch prNum {
		case 10:
			return []string{"cmd/main.go"}, nil
		case 20:
			return []string{"web/app.ts"}, nil
		}
		t.Fatalf("unexpected PR #%d", prNum)
		return nil, nil
	})

	// Single source string containing two copilot:PR#N tokens separated by a comma.
	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#10, copilot:PR#20"}},
	}}

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf).Filled
	assert.Equal(t, []string{"r1"}, updated)
	assert.Equal(t, []string{"cmd/**/*.go", "web/**/*.ts"}, rf.Rules[0].Paths)
}

func TestRunPathsBackfill_CachesResultsAcrossRules(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	var calls int
	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		calls++
		assert.Equal(t, 99, prNum)
		return []string{"internal/a.go"}, nil
	})

	// Two rules both reference the same PR — the fetcher must only be called once.
	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#99"}},
		{ID: "r2", Source: warden.SourceList{"copilot:PR#99"}},
	}}

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf).Filled
	assert.Equal(t, []string{"r1", "r2"}, updated)
	assert.Equal(t, 1, calls, "fetch result must be cached and reused across rules")
	assert.Equal(t, []string{"internal/**/*.go"}, rf.Rules[0].Paths)
	assert.Equal(t, []string{"internal/**/*.go"}, rf.Rules[1].Paths)
}

func TestRunPathsBackfill_DedupesSameSourceRepeated(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	var calls int
	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		calls++
		assert.Equal(t, 7, prNum)
		return []string{"internal/x.go"}, nil
	})

	// Two entries for the same PR should only trigger one fetch.
	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#7", "copilot:PR#7"}},
	}}

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf).Filled
	assert.Equal(t, []string{"r1"}, updated)
	assert.Equal(t, 1, calls, "duplicate PR references must be deduplicated")
	assert.Equal(t, []string{"internal/**/*.go"}, rf.Rules[0].Paths)
}

// TestSafeGlobListNeutralizesPRDerivedBytes pins the log-site treatment of a
// glob whose extension came out of a filename an external contributor chose.
// filepath.Ext returns everything after the LAST dot, so a file named
// "a/b.go\n[smelter] forged line" reaches the backfill log as a glob carrying
// a newline (there is no later dot in the payload to cut it short) — which, printed raw, is a line of the operator's daemon.log that
// Forge did not write.
func TestSafeGlobListNeutralizesPRDerivedBytes(t *testing.T) {
	forged := warden.ExtGlobs([]string{"a/b.go\n[smelter] paths backfill: rule x on y kept everything"})
	require.Len(t, forged, 1)
	require.Contains(t, forged[0], "\n", "the raw glob carries the injected newline")

	out := safeGlobList(forged)
	assert.NotContains(t, out, "\n", "a newline must never reach a log line")
	assert.NotContains(t, out, " ", "nor a space, which is what a forged sentence needs")

	// The alphabet is closed, so the ESC and the '[' are gone and what is left
	// of the sequence is inert literal text — the "?" marking where bytes were
	// removed, as diff.SafePath does.
	esc := safeGlobList([]string{"**/*.go\x1b[31m", "**/*.ts"})
	assert.Equal(t, "**/*.go?31m, **/*.ts", esc, "escape bytes collapse to the scrub marker")
}

// TestSafeGlobListKeepsGlobsReadable is the other half: the sanitizer exists
// to be used on every backfill log line, so it must leave an ordinary glob
// alone. diff.SafePath cannot be used here for exactly this reason — '*' is
// not in its alphabet, so it renders every glob this package emits as "?/?.go".
func TestSafeGlobListKeepsGlobsReadable(t *testing.T) {
	assert.Equal(t, "**/*.go, **/*.tsx, changelog.d/**",
		safeGlobList([]string{"**/*.go", "**/*.tsx", "changelog.d/**"}))
}

// TestSafeGlobListCapsTheList bounds one rendered list, on the argument
// diff.MaxElidedFilesListed is bounded on: a PR touching hundreds of distinct
// extensions would otherwise put the whole set — the attacker-controlled part
// of it included — into a single log line.
func TestSafeGlobListCapsTheList(t *testing.T) {
	globs := make([]string, 0, 13)
	for i := 0; i < 13; i++ {
		globs = append(globs, fmt.Sprintf("**/*.e%d", i))
	}
	out := safeGlobList(globs)
	assert.Contains(t, out, "**/*.e0")
	assert.NotContains(t, out, "**/*.e10")
	assert.Contains(t, out, "and 3 more")
}

// TestRunPathsBackfill_NarrowingRequiresEverySourcePR is the completeness rule
// on the rewrite branch. Narrowing replaces the gate a rule already carries, so
// the globs doing the replacing have to cover ALL of the rule's evidence: a
// transient gh failure on one of two source PRs leaves the derived set holding
// only the surviving PR's extensions, which is strictly narrower and matches
// its own source — both guards pass — and re-gating on it drops the failed PR's
// paths for good, since the next run derives the full, WIDER set and
// isStrictlyNarrower declines that.
func TestRunPathsBackfill_NarrowingRequiresEverySourcePR(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		if prNum == 1 {
			return nil, errors.New("gh exploded")
		}
		return []string{"internal/a.go"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{{
		ID:     "r1",
		Source: warden.SourceList{"copilot:PR#1", "copilot:PR#2"},
		Paths:  []string{"**/*.go", "**/*.md"},
	}}}

	result := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Empty(t, result.Narrowed, "a rule whose evidence is incomplete must not be narrowed")
	assert.Empty(t, result.Filled)
	assert.Equal(t, []string{"**/*.go", "**/*.md"}, rf.Rules[0].Paths,
		"the paths the failed PR justifies must survive the failure")
}

// The counterpart: the identical rule and the identical derived set, with both
// fetches succeeding, IS narrowed — so the case above is refused for the
// missing evidence and not because the narrowing itself was unavailable.
func TestRunPathsBackfill_NarrowsWhenEverySourcePRIsRead(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		if prNum == 1 {
			return []string{"internal/b.go"}, nil
		}
		return []string{"internal/a.go"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{{
		ID:     "r1",
		Source: warden.SourceList{"copilot:PR#1", "copilot:PR#2"},
		Paths:  []string{"**/*.go", "**/*.md"},
	}}}

	result := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Equal(t, []string{"r1"}, result.Narrowed)
	assert.Equal(t, []string{"internal/**/*.go"}, rf.Rules[0].Paths)
}

// Filling an empty Paths field keeps the best-effort behaviour it has always
// had: partial evidence there replaces a rule gated on nothing at all, and
// nothing is lost by acting on it — a later run that reads every source PR can
// still narrow the result further, which is the branch the completeness rule
// guards.
func TestRunPathsBackfill_PartialFetchStillFillsEmptyPaths(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		if prNum == 1 {
			return nil, errors.New("gh exploded")
		}
		return []string{"internal/a.go"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{{
		ID:     "r1",
		Source: warden.SourceList{"copilot:PR#1", "copilot:PR#2"},
	}}}

	result := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Equal(t, []string{"r1"}, result.Filled)
	assert.Empty(t, result.Narrowed)
	assert.Equal(t, []string{"internal/**/*.go"}, rf.Rules[0].Paths)
}

// A fetch failure is cached with the PR, so a second rule citing the same
// failed PR must reach the same refusal without a second gh call — and must
// reach it as a refusal, not as an unnoticed partial narrowing.
func TestRunPathsBackfill_CachedFetchFailureStillBlocksNarrowing(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	var calls int
	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		calls++
		if prNum == 1 {
			return nil, errors.New("gh exploded")
		}
		return []string{"internal/a.go"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#1", "copilot:PR#2"}, Paths: []string{"**/*.go", "**/*.md"}},
		{ID: "r2", Source: warden.SourceList{"copilot:PR#1", "copilot:PR#2"}, Paths: []string{"**/*.go", "**/*.md"}},
	}}

	result := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Empty(t, result.Narrowed)
	assert.Equal(t, 2, calls, "both outcomes, the failure included, are cached per PR")
	assert.Equal(t, []string{"**/*.go", "**/*.md"}, rf.Rules[1].Paths)
}

// sourceEvidence is what the two branches read the fetch outcome off, so the
// counts it carries have to survive a mix of outcomes rather than collapsing
// to "something worked".
func TestSourcePRFilesReportsIncompleteEvidence(t *testing.T) {
	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		if prNum == 1 {
			return nil, errors.New("gh exploded")
		}
		return []string{"internal/a.go"}, nil
	})

	rule := &warden.Rule{ID: "r1", Source: warden.SourceList{"copilot:PR#1", "copilot:PR#2"}}
	ev, ok := sourcePRFiles(context.Background(), t.TempDir(), "anvil-a", rule, map[int]prFetchResult{})
	require.True(t, ok, "one successful fetch is still usable evidence")
	assert.False(t, ev.complete())
	assert.Equal(t, 1, ev.fetched)
	assert.Equal(t, 1, ev.failed)
	assert.Equal(t, []string{"internal/a.go"}, ev.files)

	rule2 := &warden.Rule{ID: "r2", Source: warden.SourceList{"copilot:PR#2"}}
	ev2, ok := sourcePRFiles(context.Background(), t.TempDir(), "anvil-a", rule2, map[int]prFetchResult{})
	require.True(t, ok)
	assert.True(t, ev2.complete())
	assert.Equal(t, 0, ev2.failed)
}
