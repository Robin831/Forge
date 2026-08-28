package smelter

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Robin831/Forge/internal/warden"
	"github.com/bmatcuk/doublestar/v4"
)

// languageSignal maps one language to the globs a rule about it should be
// gated on, plus the patterns in a rule's own text that identify it.
//
// The patterns are matched against the rule's pattern + check text, never
// against the source PR's files: the whole point is to tell what a rule is
// ABOUT apart from what the PR that taught it happened to touch.
//
// Every pattern is a token that only appears when the rule really is about
// that language — an import path, a stdlib identifier, a file extension, a
// framework name. Bare English words that a language also happens to use are
// deliberately absent: `nil`, `defer`, `chan` and `props` each fire on
// ordinary review prose ("do not defer validation to the client", "avoid
// rendering a nil value"), which attributes a language-agnostic or frontend
// rule to Go. globsForRule no longer turns such a misfire into a permanent
// gate — an inference the PR's own files contradict is discarded in favour of
// them — but a misfire still costs the rule the narrowing this table exists to
// provide, so the bar for adding a pattern here is that it cannot be written
// by accident in a sentence about something else.
type languageSignal struct {
	name    string
	globs   []string
	signals []*regexp.Regexp
}

// languageSignals is walked in declaration order, which fixes the order
// inferRuleGlobs returns a multi-language rule's globs in. It is not what makes
// the encoded warden-rules.yaml deterministic: every path out of globsForRule
// sorts, so declaration order never reaches the caller.
var languageSignals = []languageSignal{
	{
		name:  "go",
		globs: []string{"**/*.go"},
		signals: compileSignals(
			`(?i)\bgolang\b`,
			`(?i)\bgoroutines?\b`,
			`(?i)\bgo\.mod\b`,
			`(?i)\bgo\.sum\b`,
			`\.go\b`,
			`\bgo func\b`,
			`\bsync\.(Map|Mutex|RWMutex|WaitGroup|Once|Pool)\b`,
			`\bcontext\.Context\b`,
			`\bfilepath\.`,
			`\berrors\.(Is|As|New)\b`,
			`\bfmt\.Errorf\b`,
			`err != nil`,
			// defer f.Close(), defer mu.Unlock(), defer resp.Body.Close().
			// The selector is what distinguishes the Go statement from the
			// English verb: `defer` followed directly by a call on a value is
			// not a sentence anybody writes about something else, while a bare
			// `defer word(` is one keystroke from ordinary review prose. The
			// chain is repeated rather than fixed at one selector because
			// `defer resp.Body.Close()` and `defer rows.Next()` are the same
			// idiom, and anchoring at a single one matched only the second.
			`(?i)\bdefer\s+\w+(?:\.\w+)+\(`,
		),
	},
	{
		name:  "frontend",
		globs: []string{"**/*.ts", "**/*.tsx"},
		signals: compileSignals(
			`(?i)\breact\b`,
			`(?i)\bjsx\b`,
			`(?i)\btypescript\b`,
			`\.tsx?\b`,
			// React hooks, built-in and custom: useState, useEffect, useFoo.
			`\buse[A-Z][A-Za-z]*\b`,
		),
	},
	{
		name:  "changelog",
		globs: []string{"changelog.d/**"},
		signals: compileSignals(
			`(?i)changelog\.d`,
			`(?i)\bchangelog fragments?\b`,
			`(?i)\bnews fragments?\b`,
		),
	},
}

// compileSignals compiles the patterns of one language entry at package init.
// A pattern that does not compile is a programming error in the table above,
// so MustCompile is the right strictness here.
func compileSignals(patterns ...string) []*regexp.Regexp {
	res := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		res = append(res, regexp.MustCompile(p))
	}
	return res
}

// ruleText is the text a rule's own language is inferred from: what the rule
// says to look for and what it says to check. Everything else on a Rule —
// source, id, added date — describes where the rule came from, not what it is
// about, which is precisely the confusion this package had.
func ruleText(r warden.Rule) string {
	return r.Pattern + "\n" + r.Check
}

// matchedLanguages returns the languageSignals entries whose patterns the text
// matches, in declaration order.
func matchedLanguages(text string) []languageSignal {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []languageSignal
	for _, lang := range languageSignals {
		if matchesAny(lang.signals, text) {
			out = append(out, lang)
		}
	}
	return out
}

// languageOutcomes names the languages a rule's text was read as naming AND
// what globsForRule then did with each: `go=kept`, `changelog=discarded`,
// `frontend=partial(1/2)`. It exists so the backfill log can say WHY a rule
// got the globs it got.
//
// The outcome is the half that cannot be left out. The inference alone answers
// a question nobody is asking: a Go-signalled rule learned from a PR of .ts
// files is backfilled with **/*.ts and would have logged `languages: go` —
// indistinguishable from a rule the narrowing genuinely applied to, and read
// by whoever is debugging that rule as confirmation it did. So the outcome is
// read back off the globs the rule actually carries rather than re-derived
// from the text, which is exactly the information the text does not hold.
func languageOutcomes(text string, globs []string) []string {
	langs := matchedLanguages(text)
	if len(langs) == 0 {
		return nil
	}
	final := make(map[string]struct{}, len(globs))
	for _, g := range globs {
		final[g] = struct{}{}
	}
	out := make([]string, 0, len(langs))
	for _, lang := range langs {
		kept := 0
		for _, g := range lang.globs {
			if _, ok := final[g]; ok {
				kept++
			}
		}
		switch {
		case kept == 0:
			out = append(out, lang.name+"=discarded")
		case kept == len(lang.globs):
			out = append(out, lang.name+"=kept")
		default:
			// A language whose globs came through in part — the frontend rule
			// from a PR that touched .ts but no .tsx. Reported as neither, so
			// that a gate narrower than the inference does not read as the
			// whole of it.
			out = append(out, fmt.Sprintf("%s=partial(%d/%d)", lang.name, kept, len(lang.globs)))
		}
	}
	return out
}

// inferRuleGlobs returns the globs implied by the languages a rule's own text
// names, in languageSignals declaration order and deduplicated. It returns nil
// when the text names none — the caller then has nothing to narrow with.
func inferRuleGlobs(text string) []string {
	var globs []string
	seen := make(map[string]struct{})
	for _, lang := range matchedLanguages(text) {
		for _, g := range lang.globs {
			if _, dup := seen[g]; dup {
				continue
			}
			seen[g] = struct{}{}
			globs = append(globs, g)
		}
	}
	return globs
}

func matchesAny(signals []*regexp.Regexp, text string) bool {
	for _, re := range signals {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// globsForRule derives the Paths a backfilled rule should carry from two
// independent pieces of evidence: the extensions of the files the source PR
// touched (what was changed) and the languages the rule's own text names (what
// the rule is about). Taking the PR's set alone is what made a Go concurrency
// rule learned from a PR that also touched the frontend carry **/*.ts,
// **/*.tsx and **/*.md — globs that match unrelated diffs, so the rule joined
// the Warden checklist on every one of them.
//
// The two are combined as follows. The ladder is decided on the EXTENSION
// evidence alone, and directory globs are folded in afterwards, because the
// two kinds of glob are not comparable and letting a directory glob decide the
// ladder is how a rule that merely mentions "changelog fragments" in passing
// ended up gated on changelog.d/** and nothing else:
//
//   - No language inferred → the PR-derived set, unchanged. This is the old
//     behaviour and the right fallback: without a signal there is nothing to
//     narrow with, and a guess would be worse than the status quo.
//   - No PR-derived set (the PR reported no file carrying an extension) → the
//     inferred EXTENSION globs, whole. There is no observation to weigh them
//     against. The inferred directory globs are not exempted by that: they are
//     corroborated here exactly as they are below, since a PR of extensionless
//     files (Makefile, Dockerfile, LICENSE) is silent about a rule's language
//     but says as much about changelog.d/** as any other PR does.
//   - An inferred extension glob is kept when the PR actually touched that
//     extension, since the two sets are then comparable.
//   - If the rule named extensions and the PR corroborates none of them, the
//     inference is discarded in favour of the PR-derived set. Keeping it would
//     gate the rule on a language its own diffs never contain, which
//     warden.FilterRules turns into a rule that never fires again — silently,
//     since a rule that matches nothing looks exactly like a rule with nothing
//     to say.
//   - A directory glob (changelog.d/**) names no extension, so it is ADDITIVE:
//     it is unioned onto whatever the ladder produced, never a replacement for
//     it, and only when the source PR actually touched a file it matches. That
//     corroboration is also what keeps the glob out of an anvil that uses a
//     different fragment directory, where changelog.d/** would match nothing.
func globsForRule(rule warden.Rule, files []string) []string {
	prGlobs := globsFromExtensions(files)
	inferred := inferRuleGlobs(ruleText(rule))
	if len(inferred) == 0 {
		return prGlobs
	}

	// The split happens before any of the ladder's exits, so that a directory
	// glob is corroborated on every one of them. Split after the empty-prGlobs
	// exit, it was not: a rule merely mentioning changelog fragments, learned
	// from a PR of extensionless files, came out gated on changelog.d/** and
	// nothing else — the uncorroborated sole gate this whole arrangement
	// exists to prevent, reached by the one path that skipped the check.
	var extGlobs, dirGlobs []string
	for _, g := range inferred {
		if strings.HasPrefix(g, extGlobPrefix) {
			extGlobs = append(extGlobs, g)
			continue
		}
		dirGlobs = append(dirGlobs, g)
	}
	dirGlobs = corroboratedGlobs(dirGlobs, files)

	if len(prGlobs) == 0 {
		return sortedCopy(append(extGlobs, dirGlobs...))
	}

	inPR := make(map[string]struct{}, len(prGlobs))
	for _, g := range prGlobs {
		inPR[g] = struct{}{}
	}
	var out []string
	for _, g := range extGlobs {
		if _, ok := inPR[g]; ok {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		// The rule named no extension at all, or named ones the PR's files
		// contradict. Either way the observed evidence is the only evidence.
		out = append(out, prGlobs...)
	}
	out = append(out, dirGlobs...)

	sort.Strings(out)
	return out
}

// corroboratedGlobs returns the globs that at least one of the PR's own files
// matches. It is the directory-glob counterpart of the extension
// intersection — the same question ("did the PR that taught this rule touch
// what the glob names?") asked of a glob no extension implies. Matching is
// doublestar, the same as warden.FilterRules applies at review time, so a glob
// kept here is one that could fire there; an invalid pattern never matches,
// again as at review time.
func corroboratedGlobs(globs, files []string) []string {
	var out []string
	for _, g := range globs {
		for _, f := range files {
			if ok, err := doublestar.Match(g, f); err == nil && ok {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

func sortedCopy(globs []string) []string {
	out := append([]string(nil), globs...)
	sort.Strings(out)
	return out
}
