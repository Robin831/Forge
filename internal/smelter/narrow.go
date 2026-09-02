package smelter

import (
	"strings"

	"github.com/Robin831/Forge/internal/warden"
	"github.com/bmatcuk/doublestar/v4"
)

// matchEverythingGlobs are the globs that match every path a repository can
// hold. They are the only patterns this package claims to know the coverage of
// beyond equality, which is what keeps globCovers a decision it can actually
// make rather than a glob-subset solver.
var matchEverythingGlobs = map[string]struct{}{
	"**":   {},
	"**/*": {},
}

func matchesEverything(glob string) bool {
	_, ok := matchEverythingGlobs[strings.TrimSpace(glob)]
	return ok
}

// repoWideGlob reports whether a glob names no location: `**/*` and `**`, which
// select nothing at all, and the bare `**/*.ext` form, which selects a language
// and then fires in every directory of the repository.
//
// It is the same claim warden's ranking makes when it discounts a rule by the
// repo-wide globs it carries (globWeight == 0 there for `**/*`), read one step
// wider: for the Warden a `**/*.go` still narrows a mixed diff, while for a
// rewrite of a rule's own Paths it is the shape that says "this rule has not
// been placed anywhere". A glob that names a directory — `internal/**/*.go`,
// `changelog.d/**` — is not repo-wide however many wildcards follow it.
func repoWideGlob(glob string) bool {
	glob = strings.TrimSpace(glob)
	if matchesEverything(glob) {
		return true
	}
	rest, ok := strings.CutPrefix(glob, "**/")
	if !ok {
		return false
	}
	// `**/pkg/*.go` names a directory and only happens to open with `**/`.
	if strings.Contains(rest, "/") {
		return false
	}
	return strings.HasPrefix(rest, "*")
}

func anyRepoWide(globs []string) bool {
	for _, g := range globs {
		if repoWideGlob(g) {
			return true
		}
	}
	return false
}

// globCovers reports whether every path narrow matches is also matched by
// broad. It is deliberately syntactic and conservative — equality, or a broad
// that matches every path there is — rather than a general glob-subset
// decision, because the direction of a wrong answer is not symmetric: a
// coverage claim that is too generous rewrites a rule's Paths to something
// that stops matching files the rule used to be reviewed on, and a rule that
// matches nothing looks exactly like a rule with nothing to say. Anything this
// cannot prove counts as not covered, which leaves the rule as it is.
//
// The pairs it has to decide are narrow by construction: Pass 3 derives
// candidates from warden.DerivePaths (`api/**/*.cs`, the root-level `*.cs`, and
// the bare `**/*.ext` where there was no file to place) and warden's language
// signal table (`**/*.go`, `**/*.ts`, `**/*.tsx`, `changelog.d/**`), so equality
// carries most of them and two relations are left worth naming.
//
// The second is area scoping, and it is a containment this CAN prove rather
// than a guess: every path matching `api/**/*.cs` matches `**/*.cs`, and every
// path matching the root-level `*.cs` does too, because warden.BaseGlob strips
// exactly the part of the glob that restricts WHERE — so a glob and its base
// differ only in a location the base does not constrain. Without it a scoped
// candidate is comparable to nothing on file and Pass 3 declines the whole
// point of the scoping: the rules carrying `**/*.cs` beside `**/*.md` would
// stay exactly as they are.
func globCovers(broad, narrow string) bool {
	b, n := strings.TrimSpace(broad), strings.TrimSpace(narrow)
	if b == n {
		return true
	}
	if matchesEverything(b) {
		return true
	}
	return b == warden.BaseGlob(n)
}

func coveredBy(glob string, set []string) bool {
	for _, g := range set {
		if globCovers(g, glob) {
			return true
		}
	}
	return false
}

// isStrictlyNarrower reports whether candidate selects a strict subset of the
// paths current selects: every glob in candidate is covered by some glob in
// current, and at least one glob in current is covered by nothing in
// candidate. Equal sets are not narrower, which is what makes a repeated Pass 3
// over an already-narrowed rule a no-op.
//
// The repo-wide check between the two halves is not a restatement of
// containment. Containment happens to imply it for globCovers as written
// (a candidate `**/*.go` is only ever covered by a `**/*.go` or a `**/*`, both
// repo-wide), but the property that must hold whatever coverage comes to mean
// is that a rule which named a location never comes back naming the whole
// repository — so it is stated once, here, rather than left as a consequence
// of a helper somebody may later teach a looser relation.
func isStrictlyNarrower(candidate, current []string) bool {
	if len(candidate) == 0 || len(current) == 0 {
		return false
	}
	for _, c := range candidate {
		if !coveredBy(c, current) {
			return false
		}
	}
	if anyRepoWide(candidate) && !anyRepoWide(current) {
		return false
	}
	for _, g := range current {
		if !coveredBy(g, candidate) {
			return true
		}
	}
	return false
}

// matchesAnySource reports whether at least one of sourceFiles matches at least
// one of paths. It is the guard on a narrowing rewrite: the rule has to keep
// matching the pull request it was learned from, since a rule that no longer
// matches its own evidence has been gated on a language or a directory nothing
// it was taught from ever touched — which warden.FilterRules turns into a rule
// that never fires again, silently.
//
// Matching is doublestar with an invalid pattern treated as no match, the same
// as warden.FilterRules does at review time, so a glob that passes here is one
// that can fire there. No source files means no evidence, and no evidence means
// no rewrite.
func matchesAnySource(paths, sourceFiles []string) bool {
	for _, p := range paths {
		for _, f := range sourceFiles {
			if ok, err := doublestar.Match(p, f); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// mayNarrow reports whether a rule's existing Paths could possibly be narrowed,
// deciding it from the globs alone so that a rule that cannot benefit costs no
// PR lookup. Narrowing needs some current glob left uncovered by the candidate,
// and a candidate must be covered by current — so a set of two or more always
// admits a subset, and a single glob admits something narrower exactly when it
// is repo-wide.
//
// The single-glob test is anyRepoWide and not matchesEverything because a bare
// `**/*.cs` now covers a strictly narrower glob: the area-scoped `api/**/*.cs`.
// Held to matchesEverything, every rule gated on one bare language glob — which
// is most of the population this scoping exists for — would be skipped before
// any lookup and never placed. The cost is that such a rule is now re-derived
// every flush, one gh call per distinct source PR, which is the same bargain
// runPathsBackfill already documents.
func mayNarrow(current []string) bool {
	if len(current) >= 2 {
		return true
	}
	return anyRepoWide(current)
}
