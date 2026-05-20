package smelter

import (
	"context"
	"errors"
	"testing"

	"github.com/Robin831/Forge/internal/warden"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractPRNumber(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		wantN   int
		wantOK  bool
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

func TestGlobsFromExtensions(t *testing.T) {
	t.Run("derives unique globs sorted", func(t *testing.T) {
		files := []string{
			"cmd/forge/main.go",
			"internal/smelter/smelter.go",
			"web/src/app.ts",
			"web/src/components/Foo.ts",
			"docs/README.md",
		}
		globs := globsFromExtensions(files)
		// Deduped and sorted.
		assert.Equal(t, []string{"**/*.go", "**/*.md", "**/*.ts"}, globs)
	})

	t.Run("ignores files without an extension", func(t *testing.T) {
		files := []string{"Makefile", "LICENSE", ".gitignore"}
		// ".gitignore" returns ".gitignore" from filepath.Ext (treated as extension),
		// but we intentionally accept any leading-dot extension. The bare files
		// (Makefile, LICENSE) have no extension and should be skipped. The
		// dotfile contributes a glob.
		globs := globsFromExtensions(files)
		assert.Equal(t, []string{"**/*.gitignore"}, globs)
	})

	t.Run("empty input returns nil", func(t *testing.T) {
		assert.Nil(t, globsFromExtensions(nil))
		assert.Nil(t, globsFromExtensions([]string{}))
	})

	t.Run("all files extensionless returns nil", func(t *testing.T) {
		assert.Nil(t, globsFromExtensions([]string{"Makefile", "Dockerfile"}))
	})

	t.Run("dot-only filename is skipped", func(t *testing.T) {
		// filepath.Ext("foo.") returns ".", which is a degenerate extension.
		assert.Nil(t, globsFromExtensions([]string{"foo."}))
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

func TestRunPathsBackfill_SkipsRulesWithExistingPaths(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	var calls int
	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		calls++
		return []string{"a.go"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#1"}, Paths: []string{"**/*.go"}},
	}}

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Empty(t, updated, "rule with existing Paths must be skipped")
	assert.Equal(t, 0, calls, "stub fetcher must not be invoked for rules with existing Paths")
	assert.Equal(t, []string{"**/*.go"}, rf.Rules[0].Paths, "Paths must not be mutated")
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

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Equal(t, []string{"r1"}, updated)
	assert.Equal(t, []string{"**/*.go", "**/*.ts"}, rf.Rules[0].Paths)
}

func TestRunPathsBackfill_UnionsAcrossMultiplePRs(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		switch prNum {
		case 1:
			return []string{"a.go", "b.go"}, nil
		case 2:
			return []string{"c.ts", "d.ts"}, nil
		}
		t.Fatalf("unexpected PR #%d", prNum)
		return nil, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#1", "copilot:PR#2"}},
	}}

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Equal(t, []string{"r1"}, updated)
	assert.Equal(t, []string{"**/*.go", "**/*.ts"}, rf.Rules[0].Paths)
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

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
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

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
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
		return []string{"foo.go"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#1", "copilot:PR#2"}},
	}}

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Equal(t, []string{"r1"}, updated)
	assert.Equal(t, []string{"**/*.go"}, rf.Rules[0].Paths)
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

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Empty(t, updated)
	assert.Empty(t, rf.Rules[0].Paths)
}

func TestRunPathsBackfill_Idempotency_RepeatedRunIsNoop(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	var calls int
	withStubFetcher(t, func(_ context.Context, _ string, _ int) ([]string, error) {
		calls++
		return []string{"foo.go"}, nil
	})

	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#1"}},
	}}

	// First run: populates Paths.
	first := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	require.Equal(t, []string{"r1"}, first)
	require.Equal(t, []string{"**/*.go"}, rf.Rules[0].Paths)

	// Second run: rule has Paths, so the pass must skip it entirely — the
	// stub fetcher must not be called a second time.
	prevCalls := calls
	second := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Empty(t, second, "second run must report no updates")
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

	updated := s.runPathsBackfill(ctx, t.TempDir(), "anvil-a", rf)
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

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Equal(t, []string{"r1"}, updated)
	assert.Equal(t, []string{"**/*.go", "**/*.ts"}, rf.Rules[0].Paths)
}

func TestRunPathsBackfill_CachesResultsAcrossRules(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	var calls int
	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		calls++
		assert.Equal(t, 99, prNum)
		return []string{"a.go"}, nil
	})

	// Two rules both reference the same PR — the fetcher must only be called once.
	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#99"}},
		{ID: "r2", Source: warden.SourceList{"copilot:PR#99"}},
	}}

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Equal(t, []string{"r1", "r2"}, updated)
	assert.Equal(t, 1, calls, "fetch result must be cached and reused across rules")
	assert.Equal(t, []string{"**/*.go"}, rf.Rules[0].Paths)
	assert.Equal(t, []string{"**/*.go"}, rf.Rules[1].Paths)
}

func TestRunPathsBackfill_DedupesSameSourceRepeated(t *testing.T) {
	db := openTestDB(t)
	s := New(db, 0, map[string]string{})

	var calls int
	withStubFetcher(t, func(_ context.Context, _ string, prNum int) ([]string, error) {
		calls++
		assert.Equal(t, 7, prNum)
		return []string{"x.go"}, nil
	})

	// Two entries for the same PR should only trigger one fetch.
	rf := &warden.RulesFile{Rules: []warden.Rule{
		{ID: "r1", Source: warden.SourceList{"copilot:PR#7", "copilot:PR#7"}},
	}}

	updated := s.runPathsBackfill(context.Background(), t.TempDir(), "anvil-a", rf)
	assert.Equal(t, []string{"r1"}, updated)
	assert.Equal(t, 1, calls, "duplicate PR references must be deduplicated")
	assert.Equal(t, []string{"**/*.go"}, rf.Rules[0].Paths)
}
