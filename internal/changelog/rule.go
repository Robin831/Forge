package changelog

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// DefaultFragmentDir is where a repository keeps its changelog fragments
// unless it says otherwise. Named once because three readers need it — the
// daemon's `git ls-tree` pathspec, `forge changelog validate`, and this
// package's own rule resolution — and a directory spelled out at each of them
// is the same drift, one axis over, that FragmentRule exists to close.
const DefaultFragmentDir = "changelog.d"

// BeadPlaceholder is what a configured fragment glob writes where the bead id
// belongs. A glob without it is refused at config load: `*.md` would match
// every fragment in the directory, so the FIRST one on a stranded branch —
// typically a sibling bead's, merged weeks earlier and still sitting in
// changelog.d/ — would read as this bead's completion signal.
const BeadPlaceholder = "{bead}"

// FragmentRule decides which file names in a repository's changelog directory
// are fragments for a given bead. It exists because the convention is the
// ANVIL's and not Forge's: the repository states it a second time in its own CI
// gate (Munin: "Check changelog updates"), and two independent encodings of one
// convention drift — Forge-fj09 is the drift already observed, where Munin
// added a `-technical` fragment kind, its own gate passed a `-technical`-only
// pair, and Forge's matcher did not, so completed and pushed work
// (Fhi.Metadata-hwbwz, 2026-09-07) was escalated to needs_human and its PR
// opened by hand.
//
// The zero value is the built-in convention (FragmentMatchesBead under
// changelog.d/), which every anvil gets without configuring anything. Globs are
// the escape hatch for a repository whose gate accepts a shape that grammar
// does not — an underscore-delimited kind, a different directory, a towncrier
// layout — and they REPLACE the grammar rather than extend it, because a rule
// stated in one place and half-overridden in another is the arrangement being
// removed.
//
// What the escape hatch does not do is remove the possibility of drift, since
// the anvil could always add a shape neither its config nor the default names.
// That is why NearMisses exists beside Matches: a fragment the rule rejects but
// that plainly names the bead is reported rather than silently counted as
// absent, which turns the false negative — visible until now only as an
// escalation an operator has to read and disbelieve — into a signal that says
// what it saw.
type FragmentRule struct {
	// Dir is the repository-relative directory fragments live in. Empty means
	// DefaultFragmentDir.
	Dir string

	// Globs are the accepted file-NAME patterns (path.Match syntax) with
	// BeadPlaceholder standing in for the bead id. Empty means the built-in
	// grammar, FragmentMatchesBead.
	Globs []string
}

// ResolvedDir is the rule's directory with the default filled in, without a
// trailing separator.
func (r FragmentRule) ResolvedDir() string {
	dir := strings.Trim(strings.TrimSpace(r.Dir), "/")
	if dir == "" {
		return DefaultFragmentDir
	}
	return dir
}

// MatchesName reports whether a bare file name in the rule's directory is a
// fragment for beadID.
func (r FragmentRule) MatchesName(name, beadID string) bool {
	name = strings.TrimSpace(name)
	if beadID == "" || name == "" || strings.ContainsAny(name, `/\`) {
		return false
	}
	if len(r.Globs) == 0 {
		return FragmentMatchesBead(name, beadID)
	}
	for _, g := range r.Globs {
		pattern := strings.TrimSpace(g)
		if pattern == "" {
			continue
		}
		// The bead id is escaped before it is substituted: it is a value Forge
		// did not write, and an id carrying `*`, `?` or `[` would otherwise
		// widen the pattern it was supposed to anchor.
		expanded := strings.ReplaceAll(pattern, BeadPlaceholder, escapeGlobMeta(beadID))
		if ok, err := path.Match(expanded, name); err == nil && ok {
			return true
		}
	}
	return false
}

// MatchesPath is MatchesName for a repository-relative path — the shape `git
// ls-tree` reports. A path outside the rule's directory, or nested below it, is
// not a fragment: the directory is flat by construction and a nested file
// belongs to whatever tooling put it there.
func (r FragmentRule) MatchesPath(p, beadID string) bool {
	name, ok := r.fragmentName(p)
	if !ok {
		return false
	}
	return r.MatchesName(name, beadID)
}

// fragmentName strips the rule's directory prefix off a repository-relative
// path and reports whether what is left is a bare file name inside it.
func (r FragmentRule) fragmentName(p string) (string, bool) {
	p = strings.TrimSpace(p)
	prefix := r.ResolvedDir() + "/"
	name, ok := strings.CutPrefix(p, prefix)
	if !ok || name == "" || strings.ContainsAny(name, `/\`) {
		return "", false
	}
	return name, true
}

// NearMisses returns the paths that plainly name beadID as a fragment but that
// this rule rejects — the drift signal. It is what stands between a repository
// adding a fragment shape nobody told Forge about and that work being reported
// as unfinished: the caller logs it and puts it in front of the operator beside
// the escalation, so "no completion signal" is never claimed over a directory
// that visibly holds one.
//
// The result is deduplicated and sorted so two probes over one tree produce the
// same sentence.
func (r FragmentRule) NearMisses(paths []string, beadID string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, p := range paths {
		name, ok := r.fragmentName(p)
		if !ok {
			continue
		}
		if r.MatchesName(name, beadID) || !PlausibleFragment(name, beadID) {
			continue
		}
		full := r.ResolvedDir() + "/" + name
		if seen[full] {
			continue
		}
		seen[full] = true
		out = append(out, full)
	}
	sort.Strings(out)
	return out
}

// Describe renders the shapes this rule accepts for beadID, for an operator
// reading a refusal. It is derived from the rule rather than written out beside
// it, so a message can never describe a convention the matcher stopped using.
func (r FragmentRule) Describe(beadID string) string {
	dir := r.ResolvedDir()
	if len(r.Globs) == 0 {
		return fmt.Sprintf(
			"%s/%s.md, or that id followed by a \".\" or \"-\" and an alphabetic kind or language — e.g. %s.en.md, %s-technical.nb.md",
			dir, beadID, beadID, beadID)
	}
	shapes := make([]string, 0, len(r.Globs))
	for _, g := range r.Globs {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		shapes = append(shapes, dir+"/"+strings.ReplaceAll(g, BeadPlaceholder, beadID))
	}
	return strings.Join(shapes, ", ") + " (this anvil's configured changelog.fragment_globs)"
}

// PlausibleFragment reports whether a file name reads as a changelog fragment
// for beadID to a human, independent of any rule. It is deliberately WIDER than
// every rule: a markdown file whose name opens with the bead id and decorates
// it with anything at all.
//
// Its one exclusion is the shape that plausibly names a DIFFERENT bead, and it
// is the same one FragmentMatchesBead draws: bd's hierarchical ids are
// literally <parent>.<n>, so a child's <parent>.7.md is the parent id followed
// by a delimiter and would report as drift on every parent escalation forever —
// a permanent false alarm about a rejection that is correct. A decoration whose
// first segment is all digits is therefore a child's ordinal and not a fragment
// kind. Everything else — an underscore, a `+`, a `2fa` that is neither a
// language nor a plain word — is a shape somebody chose on purpose and Forge
// has no opinion about, which is precisely what a drift report is for.
func PlausibleFragment(name, beadID string) bool {
	name = strings.TrimSpace(name)
	if beadID == "" || strings.ContainsAny(name, `/\`) {
		return false
	}
	stem, ok := strings.CutSuffix(name, ".md")
	if !ok {
		return false
	}
	if stem == beadID {
		return true
	}
	rest, ok := strings.CutPrefix(stem, beadID)
	if !ok || rest == "" {
		return false
	}
	if isAlphanumeric(rest[0]) {
		// Not a decoration at all — a longer bead id that happens to open with
		// this one (Fhi.Metadata-15ed91 against Fhi.Metadata-15ed9).
		return false
	}
	return !isAllDigits(leadingAlphanumericRun(rest[1:]))
}

// ValidateFragmentGlobs reports what is wrong with a configured glob list, one
// message per offending entry. A pattern is refused for exactly two reasons:
// path.Match cannot parse it (it would match nothing, so the anvil's every
// stranded branch would read as incomplete), or it omits BeadPlaceholder (it
// would match a sibling bead's fragment, so every stranded branch would read as
// complete). Both failures are silent at runtime, which is why they are decided
// at config load.
func ValidateFragmentGlobs(globs []string) []error {
	var errs []error
	for i, g := range globs {
		pattern := strings.TrimSpace(g)
		if pattern == "" {
			errs = append(errs, fmt.Errorf("fragment_globs[%d] is empty", i))
			continue
		}
		if strings.ContainsAny(pattern, `/\`) {
			errs = append(errs, fmt.Errorf("fragment_globs[%d] %q must be a file name pattern, not a path (set changelog.dir for the directory)", i, pattern))
			continue
		}
		if !strings.Contains(pattern, BeadPlaceholder) {
			errs = append(errs, fmt.Errorf("fragment_globs[%d] %q must contain %s, otherwise it matches another bead's fragment", i, pattern, BeadPlaceholder))
			continue
		}
		probe := strings.ReplaceAll(pattern, BeadPlaceholder, "bead")
		if _, err := path.Match(probe, "bead.md"); err != nil {
			errs = append(errs, fmt.Errorf("fragment_globs[%d] %q is not a valid pattern: %w", i, pattern, err))
		}
	}
	return errs
}

// escapeGlobMeta backslash-escapes the characters path.Match reads as syntax,
// so a substituted bead id is matched as the literal it is.
func escapeGlobMeta(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '*', '?', '[', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// leadingAlphanumericRun returns the leading run of ASCII letters and digits,
// i.e. everything up to the next delimiter of any kind.
func leadingAlphanumericRun(s string) string {
	for i := 0; i < len(s); i++ {
		if !isAlphanumeric(s[i]) {
			return s[:i]
		}
	}
	return s
}

func isAlphanumeric(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// isAllDigits reports whether s is a non-empty run of ASCII digits — a bd child
// bead's ordinal, and the one decoration that names a different bead.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
