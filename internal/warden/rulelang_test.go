package warden

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInferRuleGlobs(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
		want []string
	}{
		{
			name: "go concurrency rule",
			rule: Rule{
				Pattern: "map written from more than one goroutine",
				Check:   "guard shared state with sync.Map or a mutex",
			},
			want: []string{"**/*.go"},
		},
		{
			name: "go error handling rule",
			rule: Rule{
				Pattern: "error returned without context",
				Check:   "wrap with fmt.Errorf and check err != nil at the call site",
			},
			want: []string{"**/*.go"},
		},
		{
			name: "go defer rule, single selector",
			rule: Rule{
				Pattern: "file opened without a matching close",
				Check:   "defer f.Close() on the line after the open",
			},
			want: []string{"**/*.go"},
		},
		{
			name: "go defer rule, chained selector",
			rule: Rule{
				Pattern: "response body left open",
				Check:   "defer resp.Body.Close() once the error is checked",
			},
			want: []string{"**/*.go"},
		},
		{
			name: "go defer rule, unlock",
			rule: Rule{
				Pattern: "mutex held across a return",
				Check:   "take the lock and defer mu.Unlock()",
			},
			want: []string{"**/*.go"},
		},
		{
			name: "react rule",
			rule: Rule{
				Pattern: "state derived during render",
				Check:   "compute it with useMemo instead of a second useState",
			},
			want: []string{"**/*.ts", "**/*.tsx"},
		},
		{
			name: "typescript rule naming the extension",
			rule: Rule{
				Pattern: "component file without an explicit props type",
				Check:   "declare the props interface in the .tsx file",
			},
			want: []string{"**/*.ts", "**/*.tsx"},
		},
		{
			name: "changelog rule",
			rule: Rule{
				Pattern: "PR without a changelog fragment",
				Check:   "add changelog.d/<bead-id>.md",
			},
			want: []string{"changelog.d/**"},
		},
		{
			name: "rule naming both go and the changelog",
			rule: Rule{
				Pattern: "exported function added with no changelog fragment",
				Check:   "every .go change that is user visible needs one",
			},
			want: []string{"**/*.go", "changelog.d/**"},
		},
		{
			name: "generic rule names no language",
			rule: Rule{
				Pattern: "magic number in a conditional",
				Check:   "name it as a constant",
			},
			want: nil,
		},
		{
			name: "empty rule text",
			rule: Rule{},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, InferRuleGlobs(RuleText(tc.rule)))
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
		rule Rule
	}{
		{
			name: "defer in ordinary review prose",
			rule: Rule{
				Pattern: "do not defer validation to the client",
				Check:   "validate in the form component before submit",
			},
		},
		{
			// The boundary of the narrowed defer signal, stated rather than
			// left to be inferred from the pattern: a selector is required,
			// so the bare-call form is read as naming no language. It is the
			// deliberate side of the trade — `defer word(` is one keystroke
			// from prose about deferring something ("defer validation(s) to
			// the client"), and a missed narrowing costs a rule the gate it
			// could have had, while a misfire costs it every gate it has.
			name: "defer of a bare call, no selector",
			rule: Rule{
				Pattern: "context created without a cancel",
				Check:   "defer cancel() immediately after",
			},
		},
		{
			name: "nil in ordinary review prose",
			rule: Rule{
				Pattern: "avoid rendering a nil value",
				Check:   "guard the optional field",
			},
		},
		{
			name: "chan as an abbreviation, not the keyword",
			rule: Rule{
				Pattern: "channel names in the UI must be escaped",
				Check:   "escape the chan name",
			},
		},
		{
			name: "props without any other frontend token",
			rule: Rule{
				Pattern: "props drilled through three levels",
				Check:   "pass them explicitly instead",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Nil(t, InferRuleGlobs(RuleText(tc.rule)),
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
	goRule := Rule{
		Pattern: "map mutated from a goroutine",
		Check:   "hold a sync.Mutex across the write",
	}

	got := DeriveRulePaths(goRule, tsFiles)
	assert.Equal(t, []string{"web/**/*.ts", "web/**/*.tsx"}, got)
	assert.NotContains(t, got, "**/*.go",
		"a language the PR's own files contradict must not become the rule's gate")
}

func TestGlobsForRule(t *testing.T) {
	goRule := Rule{
		Pattern: "shared map mutated from a goroutine",
		Check:   "use sync.Map or hold a mutex across the write",
	}
	frontendRule := Rule{
		Pattern: "effect without a dependency array",
		Check:   "React re-runs useEffect on every render otherwise",
	}
	changelogRule := Rule{
		Pattern: "PR without a changelog fragment",
		Check:   "add a file under changelog.d",
	}
	genericRule := Rule{
		Pattern: "magic number in a conditional",
		Check:   "name it as a constant",
	}
	goChangelogRule := Rule{
		Pattern: "exported function added with no changelog fragment",
		Check:   "every .go change that is user visible needs one",
	}

	cases := []struct {
		name  string
		rule  Rule
		files []string
		want  []string
	}{
		{
			name:  "go rule from a mixed go and tsx PR keeps only go",
			rule:  goRule,
			files: []string{"internal/daemon/poll.go", "web/src/App.tsx", "changelog.d/Forge-abc1.md"},
			want:  []string{"internal/**/*.go"},
		},
		{
			name:  "frontend rule from a mixed PR keeps only the frontend globs",
			rule:  frontendRule,
			files: []string{"internal/daemon/poll.go", "web/src/api/prs.ts", "web/src/App.tsx"},
			want:  []string{"web/**/*.ts", "web/**/*.tsx"},
		},
		{
			name:  "frontend rule keeps only the frontend extension the PR touched",
			rule:  frontendRule,
			files: []string{"internal/daemon/poll.go", "web/src/api/prs.ts"},
			want:  []string{"web/**/*.ts"},
		},
		{
			name:  "changelog rule adds the directory glob to the PR-derived set",
			rule:  changelogRule,
			files: []string{"internal/daemon/poll.go", "web/src/App.tsx", "changelog.d/Forge-abc1.md"},
			want:  []string{"changelog.d/**", "changelog.d/**/*.md", "internal/**/*.go", "web/**/*.tsx"},
		},
		{
			name:  "changelog glob is dropped when the source PR touched no fragment",
			rule:  changelogRule,
			files: []string{"internal/daemon/poll.go"},
			want:  []string{"internal/**/*.go"},
		},
		{
			name:  "a directory glob is additive to a corroborated language glob",
			rule:  goChangelogRule,
			files: []string{"internal/daemon/poll.go", "web/src/App.tsx", "changelog.d/Forge-abc1.md"},
			want:  []string{"changelog.d/**", "internal/**/*.go"},
		},
		{
			name:  "no signal falls back to the PR-derived set",
			rule:  genericRule,
			files: []string{"internal/daemon/poll.go", "docs/configuration.md"},
			want:  []string{"docs/**/*.md", "internal/**/*.go"},
		},
		{
			name:  "inferred language the PR never touched falls back to the PR's own set",
			rule:  goRule,
			files: []string{"web/src/api/prs.ts"},
			want:  []string{"web/**/*.ts"},
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
		{
			// The empty-prGlobs exit corroborates its directory globs like
			// every other exit does. Uncorroborated, changelog.d/** would be
			// this rule's SOLE gate — and on an anvil whose fragments live
			// somewhere else it matches nothing, so the rule silently never
			// fires again.
			name:  "no files drops an uncorroborated directory glob",
			rule:  changelogRule,
			files: nil,
			want:  nil,
		},
		{
			name:  "extensionless files drop an uncorroborated directory glob",
			rule:  changelogRule,
			files: []string{"Makefile", "Dockerfile", "LICENSE"},
			want:  nil,
		},
		{
			name:  "extensionless files keep a corroborated directory glob",
			rule:  changelogRule,
			files: []string{"Makefile", "changelog.d/Forge-abc1"},
			want:  []string{"changelog.d/**"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, DeriveRulePaths(tc.rule, tc.files))
		})
	}
}

// TestGlobsForRuleIsDeterministic pins the ordering guarantee the encoded
// warden-rules.yaml depends on: the same inputs must produce the same slice,
// whatever order the PR reported its files in.
func TestGlobsForRuleIsDeterministic(t *testing.T) {
	rule := Rule{
		Pattern: "exported function added with no changelog fragment",
		Check:   "every .go change that is user visible needs a changelog.d entry",
	}
	first := DeriveRulePaths(rule, []string{"a.go", "b.ts", "changelog.d/x.md"})
	second := DeriveRulePaths(rule, []string{"changelog.d/x.md", "b.ts", "a.go"})
	assert.Equal(t, first, second)
	// `a.go` sits in the repository root, so its area is the root: `*.go`
	// matches it and nothing under a directory, which is the whole of what the
	// evidence supports.
	assert.Equal(t, []string{"*.go", "changelog.d/**"}, first)
}

// TestLanguageOutcomesReportsWhatSurvived pins the half of the backfill log
// that the inference alone cannot supply: whether the language a rule's text
// named actually became its gate. A Go rule learned from a PR of .ts files is
// backfilled with **/*.ts, and logging a bare `go` there reads to whoever is
// debugging it as confirmation the Go narrowing applied.
func TestLanguageOutcomesReportsWhatSurvived(t *testing.T) {
	goRule := Rule{
		Pattern: "map mutated from a goroutine",
		Check:   "hold a sync.Mutex across the write",
	}
	frontendRule := Rule{
		Pattern: "effect without a dependency array",
		Check:   "React re-runs useEffect on every render otherwise",
	}
	changelogRule := Rule{
		Pattern: "PR without a changelog fragment",
		Check:   "add a file under changelog.d",
	}

	cases := []struct {
		name  string
		rule  Rule
		files []string
		want  []string
	}{
		{
			name:  "inference that became the gate",
			rule:  goRule,
			files: []string{"internal/daemon/poll.go", "web/src/App.tsx"},
			want:  []string{"go=kept"},
		},
		{
			name:  "inference the PR contradicted",
			rule:  goRule,
			files: []string{"web/src/api/prs.ts"},
			want:  []string{"go=discarded"},
		},
		{
			name:  "directory glob the PR did not corroborate",
			rule:  changelogRule,
			files: []string{"internal/daemon/poll.go"},
			want:  []string{"changelog=discarded"},
		},
		{
			name:  "only one of a language's globs corroborated",
			rule:  frontendRule,
			files: []string{"web/src/api/prs.ts"},
			want:  []string{"frontend=partial(1/2)"},
		},
		{
			name:  "no language named",
			rule:  Rule{Pattern: "magic number", Check: "name it as a constant"},
			files: []string{"internal/daemon/poll.go"},
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			globs := DeriveRulePaths(tc.rule, tc.files)
			assert.Equal(t, tc.want, LanguageOutcomes(RuleText(tc.rule), globs))
		})
	}
}
