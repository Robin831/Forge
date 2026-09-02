package warden

import (
	"path/filepath"
	"sort"
	"strings"
)

// ExtGlobPrefix is the leading text of a bare, repo-wide extension glob:
// `**/*.go` names a language and no location at all, so a rule carrying one
// joins the review checklist on every diff that happens to touch that
// language. It is a constant rather than a literal at each site because three
// separate things test for it — the derivation below, the smelter's
// language-inference intersection, and BaseGlob — and written out three times
// they would drift apart by a character with nothing failing to compile.
const ExtGlobPrefix = "**/*."

// scopedGlobInfix is what sits between an area and its extension in the scoped
// form `api/**/*.cs`. `**` matches zero or more path segments in doublestar,
// so the one glob covers both `api/Foo.cs` and `api/Controllers/Foo.cs`.
const scopedGlobInfix = "/**/*."

// rootGlobPrefix is the scoped form for a file that sits in the repository
// root and so has no area to name: `*.cs` matches `Foo.cs` and nothing under a
// directory, which is exactly the scope the evidence supports.
const rootGlobPrefix = "*."

// DerivePaths returns the path globs a rule learned from changedFiles should be
// gated on: one glob per (top-level area, extension) pair the files actually
// contain.
//
// It is the one derivation both ends of a rule's life share — the learner,
// which has the files that taught the rule in hand when it writes the rule, and
// the smelter's Pass 3 backfill, which re-derives them from the rule's source
// PRs later. Two copies would be two answers to one question, and the whole
// point of the backfill being idempotent is that it re-derives what learning
// already decided.
//
// What it will never emit is a bare `**/*.ext`. That shape is the bug it exists
// to remove: measured on one anvil, 484 rules carried the path set
// `['**/*.cs', '**/*.md']` — a claim about two languages and about no part of
// the repository, so the rule was selected for review on every C# diff in the
// codebase and on every documentation-only one besides. `api/**/*.cs` is a
// claim a reviewer can act on; `**/*.cs` is the absence of one.
//
// The area is the TOP-LEVEL directory and not the file's own, because a rule is
// about a part of a system rather than about one folder: a comment left on
// `api/Controllers/Foo.cs` is evidence about the backend, and gating the rule on
// `api/Controllers/**` would stop it firing on the sibling directory the same
// convention governs. It is also the level at which two derivations can agree —
// the learner sees the files a review comment was anchored to, Pass 3 sees the
// whole PR, and only a coarse enough area makes those the same answer.
//
// Documentation globs need no special case: an extension only appears in the
// output when a file carrying it appears in the evidence, so a code-only diff
// yields no `.md` glob at all. TestDerivePaths pins that rather than a branch.
func DerivePaths(changedFiles []string) []string {
	return ScopeExtGlobs(ExtGlobs(changedFiles), changedFiles)
}

// ExtGlobs returns the unique BARE extension globs implied by files, sorted.
// Files with no extension are skipped.
//
// It is the repo-wide shape DerivePaths exists to avoid, kept as its own step
// because the smelter's language inference is written in it: a rule's own text
// is read as naming `**/*.go`, and only the intersection of that with the
// extensions the source PR touched says which language the rule should be gated
// on. That comparison is between bare globs on both sides, so the scoping is
// applied after it, never before.
func ExtGlobs(files []string) []string {
	seen := make(map[string]struct{})
	for _, f := range files {
		ext, ok := fileExt(f)
		if !ok {
			continue
		}
		seen[ExtGlobPrefix+ext] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	return sortedKeys(seen)
}

// ScopeExtGlobs rewrites each bare `**/*.ext` glob in globs into one glob per
// top-level area of files carrying that extension, and passes every other glob
// through untouched — a directory glob like `changelog.d/**` already names a
// location, so there is nothing to scope.
//
// A bare glob whose extension appears in no file is passed through as it is
// rather than dropped. That is the case where there is no evidence to scope
// with (the smelter reaches it for a rule whose own text names a language its
// source PR's files never carried an extension for), and a rule gated on
// nothing at all is worse than one gated on a language: dropping the glob is
// what turns a broad rule into a rule that fires everywhere.
//
// The result is deduplicated and sorted, so the Paths field encoded into
// warden-rules.yaml is a function of its inputs and not of the order a PR
// happened to report its files in.
func ScopeExtGlobs(globs, files []string) []string {
	areas := areasByExt(files)
	out := make(map[string]struct{}, len(globs))
	for _, g := range globs {
		ext, ok := bareExt(g)
		if !ok {
			out[strings.TrimSpace(g)] = struct{}{}
			continue
		}
		scoped := areas[ext]
		if len(scoped) == 0 {
			out[strings.TrimSpace(g)] = struct{}{}
			continue
		}
		for _, area := range scoped {
			out[scopedGlob(area, ext)] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return sortedKeys(out)
}

// BaseGlob returns the unscoped form of a glob: the extension glob an
// area-scoped one is a subset of (`api/**/*.cs` and the root-level `*.cs` both
// yield `**/*.cs`), and the glob itself for anything else.
//
// Two callers need exactly this and for opposite reasons. The smelter's
// coverage test uses it to decide that `**/*.cs` covers `api/**/*.cs`, which is
// what lets Pass 3 recognise a scoping as a narrowing rather than as an
// unrelated set. Its backfill log uses it to report which language a rule ended
// up gated on, which the scoped glob alone can no longer say.
func BaseGlob(glob string) string {
	if ext, ok := scopedExt(glob); ok {
		return ExtGlobPrefix + ext
	}
	return strings.TrimSpace(glob)
}

// scopedGlob assembles one area's glob for one extension.
func scopedGlob(area, ext string) string {
	if area == "" {
		return rootGlobPrefix + ext
	}
	return area + scopedGlobInfix + ext
}

// areasByExt maps each extension present in files to the sorted, deduplicated
// set of top-level directories the files carrying it live in. A file in the
// repository root contributes the empty area, which scopedGlob renders as the
// root-only `*.ext`.
func areasByExt(files []string) map[string][]string {
	seen := make(map[string]map[string]struct{})
	for _, f := range files {
		ext, ok := fileExt(f)
		if !ok {
			continue
		}
		if seen[ext] == nil {
			seen[ext] = make(map[string]struct{})
		}
		seen[ext][topLevelArea(f)] = struct{}{}
	}
	out := make(map[string][]string, len(seen))
	for ext, areas := range seen {
		out[ext] = sortedKeys(areas)
	}
	return out
}

// topLevelArea returns the first path segment of a normalized path, or "" for a
// file that sits in the repository root.
//
// The number of areas is deliberately not capped. A cap that drops areas
// removes coverage from a rule with nothing to say so, and a cap that falls
// back to the bare `**/*.ext` restores the exact shape this file exists to
// remove — so a diff that genuinely spans twenty top-level directories is
// described as spanning twenty, which is what its own evidence says. The number
// of top-level directories a repository has is the only bound that means
// anything here, and it is already the bound.
func topLevelArea(path string) string {
	p := normalizePath(path)
	if i := strings.Index(p, "/"); i > 0 {
		return p[:i]
	}
	return ""
}

// fileExt returns a file's extension without its leading dot, reporting false
// for a name that carries none (`Makefile`) or a degenerate one (`foo.`).
func fileExt(path string) (string, bool) {
	ext := filepath.Ext(normalizePath(path))
	if ext == "" || ext == "." {
		return "", false
	}
	return strings.TrimPrefix(ext, "."), true
}

// normalizePath puts a changed-file path into the one shape the globs are built
// against: forward slashes, no `./` lead, no leading separator. gh reports
// repo-relative forward-slash paths already, but a diff read off a Windows
// checkout does not, and a leading `./` silently makes the first segment empty
// — which reads as a repository-root file and so drops the area.
func normalizePath(path string) string {
	p := strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	for strings.HasPrefix(p, "./") {
		p = strings.TrimPrefix(p, "./")
	}
	return strings.TrimPrefix(p, "/")
}

// bareExt reports the extension of a bare `**/*.ext` glob, and false for
// anything else — including an area-scoped glob, which is already placed.
func bareExt(glob string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(glob), ExtGlobPrefix)
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// scopedExt reports the extension of a glob that names one extension, in either
// the bare, the area-scoped or the repository-root form.
func scopedExt(glob string) (string, bool) {
	g := strings.TrimSpace(glob)
	if ext, ok := bareExt(g); ok {
		return ext, true
	}
	if rest, ok := strings.CutPrefix(g, rootGlobPrefix); ok {
		if rest == "" || strings.Contains(rest, "/") || strings.Contains(rest, "*") {
			return "", false
		}
		return rest, true
	}
	i := strings.Index(g, scopedGlobInfix)
	if i <= 0 {
		return "", false
	}
	ext := g[i+len(scopedGlobInfix):]
	if ext == "" || strings.Contains(ext, "/") || strings.Contains(ext, "*") {
		return "", false
	}
	return ext, true
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
