package smelter

import (
	"regexp"
	"sort"
	"strings"

	"github.com/Robin831/Forge/internal/warden"
)

// extGlobPrefix is the shape globsFromExtensions emits: one doublestar glob
// per file extension. A glob carrying it can be compared against the
// PR-derived set; anything else (a directory glob like changelog.d/**) cannot,
// since no extension implies it.
const extGlobPrefix = "**/*."

// languageSignal maps one language to the globs a rule about it should be
// gated on, plus the patterns in a rule's own text that identify it.
//
// The patterns are matched against the rule's pattern + check text, never
// against the source PR's files: the whole point is to tell what a rule is
// ABOUT apart from what the PR that taught it happened to touch. They are
// deliberately narrow — a signal that fires on generic review prose ("hook",
// "component") re-widens exactly the rules this narrowing exists to fix.
type languageSignal struct {
	name    string
	globs   []string
	signals []*regexp.Regexp
}

// languageSignals is walked in declaration order, so the globs of a rule that
// matches more than one language come out in a stable order.
var languageSignals = []languageSignal{
	{
		name:  "go",
		globs: []string{"**/*.go"},
		signals: compileSignals(
			`(?i)\bgolang\b`,
			`(?i)\bgoroutines?\b`,
			`(?i)\bdefer\b`,
			`(?i)\bgo\.mod\b`,
			`(?i)\bgo\.sum\b`,
			`\.go\b`,
			`\bgo func\b`,
			`\bsync\.(Map|Mutex|RWMutex|WaitGroup|Once|Pool)\b`,
			`\bcontext\.Context\b`,
			`\bfilepath\.`,
			`\berrors\.(Is|As|New)\b`,
			`\bfmt\.Errorf\b`,
			`\bchan\b`,
			`err != nil`,
			`(?i)\bnil\b`,
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
			`(?i)\bprops\b`,
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

// inferRuleGlobs returns the globs implied by the languages a rule's own text
// names, in languageSignals declaration order and deduplicated. It returns nil
// when the text names none — the caller then has nothing to narrow with.
func inferRuleGlobs(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var globs []string
	seen := make(map[string]struct{})
	for _, lang := range languageSignals {
		if !matchesAny(lang.signals, text) {
			continue
		}
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
// The two are combined as follows:
//
//   - No language inferred → the PR-derived set, unchanged. This is the old
//     behaviour and the right fallback: without a signal there is nothing to
//     narrow with, and a guess would be worse than the status quo.
//   - An inferred glob that names an extension is kept only when the PR
//     actually touched that extension, since the two sets are then comparable.
//   - An inferred glob that names no extension (changelog.d/**) is kept as-is:
//     a PR-derived set says the fragment is a .md file and can neither confirm
//     nor refute a directory glob.
//   - If that leaves nothing at all, the inferred set is used rather than an
//     empty one — warden.FilterRules treats empty Paths as "no path
//     constraint", so returning nothing would silently disable the filter for
//     exactly the rule whose language the PR did not touch.
func globsForRule(rule warden.Rule, files []string) []string {
	prGlobs := globsFromExtensions(files)
	inferred := inferRuleGlobs(ruleText(rule))
	if len(inferred) == 0 {
		return prGlobs
	}

	var extGlobs, dirGlobs []string
	for _, g := range inferred {
		if strings.HasPrefix(g, extGlobPrefix) {
			extGlobs = append(extGlobs, g)
			continue
		}
		dirGlobs = append(dirGlobs, g)
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
	out = append(out, dirGlobs...)

	if len(out) == 0 {
		// The rule names a language the PR's files do not corroborate. Keep the
		// inferred set: too narrow costs a rule some diffs it could have applied
		// to, empty costs the filter entirely.
		out = append(out, inferred...)
	}
	sort.Strings(out)
	return out
}
