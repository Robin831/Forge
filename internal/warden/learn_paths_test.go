package warden

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubDistiller replaces the AI session with a fixed answer for the duration of
// one test. The model's answer is deliberately silent about paths, which is
// what the output contract asks for.
func stubDistiller(t *testing.T, answer string) {
	t.Helper()
	prev := aiRunner
	aiRunner = func(context.Context, string, string) ([]byte, error) { return []byte(answer), nil }
	t.Cleanup(func() { aiRunner = prev })
}

// The bead's acceptance criterion, at the learner: a rule distilled from
// comments on a code-only change must come out gated on the area those files
// live in — never on the bare `**/*.cs` that fires on every C# diff in the
// repository, and never on documentation the change never touched.
func TestDistillRuleScopesPathsToTheCommentedArea(t *testing.T) {
	stubDistiller(t, `{"id":"resource-authz","category":"security","pattern":"endpoint reading an id from the route","check":"apply the per-resource permission filter its siblings apply"}`)

	rule, err := DistillRule(context.Background(), []PRComment{
		{PRNumber: 41, Path: "api/Controllers/OrdersController.cs", Body: "missing the permission filter"},
		{PRNumber: 41, Path: "api/Controllers/InvoicesController.cs", Body: "same here"},
	}, t.TempDir())
	require.NoError(t, err)

	assert.Equal(t, []string{"api/**/*.cs"}, rule.Paths)
	assert.NotContains(t, rule.Paths, "**/*.md",
		"a code-only change is no evidence about documentation")
	assert.NotContains(t, rule.Paths, "**/*.cs",
		"a bare language glob names no location, which is the shape this replaces")
}

// The multi-area case: one glob per area, and still nothing repo-wide.
func TestDistillRuleScopesEachAreaSeparately(t *testing.T) {
	stubDistiller(t, `{"id":"dispose-scope","category":"other","pattern":"service resolved outside a scope","check":"resolve it inside the request scope"}`)

	rule, err := DistillRule(context.Background(), []PRComment{
		{PRNumber: 7, Path: "api/Startup.cs", Body: "resolve inside the scope"},
		{PRNumber: 7, Path: "worker/Jobs/Nightly.cs", Body: "and here"},
		{PRNumber: 7, Path: "docs/architecture.md", Body: "document it"},
	}, t.TempDir())
	require.NoError(t, err)

	assert.Equal(t, []string{"api/**/*.cs", "docs/**/*.md", "worker/**/*.cs"}, rule.Paths)
}

// A rule is never gated on a path the model invented: the output contract asks
// for no paths, so anything in that field is a guess, while the files the
// comments sit on are observed. A model answering with the very glob this
// change exists to remove must not be able to put it back.
func TestDistillRuleIgnoresModelSuppliedPaths(t *testing.T) {
	stubDistiller(t, `{"id":"x","category":"style","pattern":"p","check":"c","paths":["**/*.cs","**/*.md"]}`)

	rule, err := DistillRule(context.Background(), []PRComment{
		{PRNumber: 1, Path: "api/Orders.cs", Body: "b"},
	}, t.TempDir())
	require.NoError(t, err)

	assert.Equal(t, []string{"api/**/*.cs"}, rule.Paths)
}

// A pull-request-level comment carries no path, so there is nothing to derive
// from. The rule is written with no Paths rather than a guessed one, and the
// smelter's Pass 3 places it later from the PR itself — which is exactly what
// happens for every rule learned before this derivation existed.
func TestDistillRuleLeavesPathsEmptyWithoutFileEvidence(t *testing.T) {
	stubDistiller(t, `{"id":"x","category":"style","pattern":"p","check":"c"}`)

	rule, err := DistillRule(context.Background(), []PRComment{
		{PRNumber: 1, Body: "a general remark about the pull request"},
	}, t.TempDir())
	require.NoError(t, err)

	assert.Empty(t, rule.Paths)
}

// The quench learner reads its evidence out of the fix diff rather than off
// comment anchors, and is held to the same derivation.
func TestLearnFromCIFixScopesPathsToTheFixedArea(t *testing.T) {
	anvilPath := t.TempDir()
	stubDistiller(t, `{"id":"react-hooks-exhaustive-deps","category":"ui","pattern":"missing dep in useEffect","check":"list every used value in the deps array"}`)

	fixDiff := "diff --git a/web/src/Foo.tsx b/web/src/Foo.tsx\n" +
		"--- a/web/src/Foo.tsx\n+++ b/web/src/Foo.tsx\n@@ -1 +1 @@\n-bad\n+good\n"
	logs := map[string]string{"eslint": "  2:5  error  React Hook issue  react-hooks/exhaustive-deps"}

	require.NoError(t, LearnFromCIFix(context.Background(), anvilPath, anvilPath, logs, fixDiff, 99))

	loaded, err := LoadRules(anvilPath)
	require.NoError(t, err)
	require.Len(t, loaded.Rules, 1)
	assert.Equal(t, []string{"web/**/*.tsx"}, loaded.Rules[0].Paths)
}
