package warden

import (
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

// Ranking weights for review-time rule selection. They are named constants so
// the balance can be retuned without reading scoreRule's arithmetic.
//
// Specificity leads: a rule whose glob names the directory the diff touched is
// a better use of a checklist slot than one scoped `**/*`. Pattern relevance is
// next — it measures how much of the rule's own vocabulary the diff actually
// contains, which is the signal patternGrep threw away by answering yes/no.
// Recency is weighted last and deliberately low: it must break ties among the
// rules that are equally broad and equally on-topic (which, on a mature rules
// file, is most of them) without letting the newest rule outrank a narrowly
// scoped older one. Age is a proxy for value, not a measure of it — the goal is
// a reachable set spread across the file's whole history, not a truncation from
// the other end.
const (
	specificityWeight = 1.0
	patternWeight     = 0.7
	recencyWeight     = 0.5
)

// ruleScore is one candidate rule with its ranking components. The components
// are kept apart from the total so a selection can be explained (and tested)
// without re-deriving the arithmetic.
type ruleScore struct {
	rule        Rule
	specificity float64
	pattern     float64
	recency     float64
	total       float64
	added       time.Time
}

// isGlobMeta reports whether r is a doublestar metacharacter, i.e. whether its
// presence makes a path segment something other than a literal directory name.
func isGlobMeta(r rune) bool {
	switch r {
	case '*', '?', '[', ']', '{', '}', '!':
		return true
	}
	return false
}

// globWeight scores how narrowly a single glob names a location. Whole literal
// path segments count 1 each; a final segment that is partly literal (`*.go`,
// `*_test.tsx`) counts a half, which is what separates `**/*.go` from a bare
// `**/*` — both name zero directories, but only one names a language.
func globWeight(glob string) float64 {
	glob = strings.TrimSpace(glob)
	if glob == "" {
		return 0
	}
	var weight float64
	for _, seg := range strings.Split(strings.ReplaceAll(glob, "\\", "/"), "/") {
		if seg == "" {
			continue
		}
		if !strings.ContainsFunc(seg, isGlobMeta) {
			weight++
			continue
		}
		// A wildcard segment still carries information when it holds literal
		// characters of its own: `*.cs` selects a language, `**` selects
		// nothing.
		if strings.ContainsFunc(seg, func(r rune) bool { return !isGlobMeta(r) && r != '.' }) {
			weight += 0.5
		}
	}
	return weight
}

// globNarrowness maps a glob weight into [0,1). It saturates rather than grows
// without bound so that a five-segment path cannot outweigh the other two
// ranking components on its own.
func globNarrowness(glob string) float64 {
	w := globWeight(glob)
	return 1 - 1/(1+w)
}

// ruleSpecificity scores how narrowly a rule's paths name the files this diff
// touched. It is the narrowness of the rule's most specific MATCHING glob,
// discounted by how many repo-wide globs the rule carries besides — a rule that
// names `api/**/*.cs` and nothing else is a claim about the backend, while the
// same glob beside three `**/*` entries is a rule that fires everywhere and
// happens to mention the backend.
//
// A rule with no paths at all scores 0: it is in scope everywhere, which is the
// same statement a repo-wide glob makes.
func ruleSpecificity(r Rule, changedFiles []string) float64 {
	if len(r.Paths) == 0 {
		return 0
	}
	var (
		best  float64
		broad int
	)
	for _, p := range r.Paths {
		if globWeight(p) == 0 {
			broad++
		}
		if !globMatchesAny(p, changedFiles) {
			continue
		}
		if n := globNarrowness(p); n > best {
			best = n
		}
	}
	return best / float64(1+broad)
}

func globMatchesAny(pattern string, changedFiles []string) bool {
	for _, f := range changedFiles {
		ok, err := doublestar.Match(pattern, f)
		if err != nil {
			continue
		}
		if ok {
			return true
		}
	}
	return false
}

// patternRelevance is the share of the rule's own ≥4-char pattern vocabulary
// that appears in the diff. A share and not a raw count, so a terse pattern
// whose every word is present is not outranked by a verbose one that happened
// to land three common words.
//
// A pattern with no ≥4-char words carries no evidence either way and scores a
// neutral 0.5 rather than 0, which would bury it beneath every rule the diff
// merely brushes against.
func patternRelevance(hits, words int) float64 {
	if words <= 0 {
		return 0.5
	}
	return float64(hits) / float64(words)
}

// parseRuleAdded parses Rule.Added, reporting whether it was readable. An
// unreadable or absent date is not evidence of age, so callers give it the
// neutral recency rather than the oldest.
func parseRuleAdded(r Rule) (time.Time, bool) {
	if strings.TrimSpace(r.Added) == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(staleAddedLayout, strings.TrimSpace(r.Added))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// recencyScores normalises the candidates' Added dates onto [0,1] with the
// newest at 1. Normalising across the candidate set rather than against wall
// clock time keeps the ranking a comparison between the rules actually in
// contention: on a file whose newest rule is six months old, recency still
// separates March from May instead of collapsing every rule to ~0.
func recencyScores(scores []ruleScore) {
	var (
		oldest, newest time.Time
		seen           bool
	)
	for _, s := range scores {
		if s.added.IsZero() {
			continue
		}
		if !seen {
			oldest, newest, seen = s.added, s.added, true
			continue
		}
		if s.added.Before(oldest) {
			oldest = s.added
		}
		if s.added.After(newest) {
			newest = s.added
		}
	}
	span := newest.Sub(oldest)
	for i := range scores {
		switch {
		case scores[i].added.IsZero():
			scores[i].recency = 0.5
		case span <= 0:
			scores[i].recency = 1
		default:
			scores[i].recency = float64(scores[i].added.Sub(oldest)) / float64(span)
		}
	}
}

// scoreCandidates builds the ranking components for every candidate. patternHits
// carries the per-rule word-hit counts already computed by the pattern filter,
// keyed by candidate index, so the diff is scanned once per rule rather than
// twice.
func scoreCandidates(rules []Rule, changedFiles []string, hits, words []int) []ruleScore {
	scores := make([]ruleScore, len(rules))
	for i, r := range rules {
		added, _ := parseRuleAdded(r)
		scores[i] = ruleScore{
			rule:        r,
			specificity: ruleSpecificity(r, changedFiles),
			pattern:     patternRelevance(hits[i], words[i]),
			added:       added,
		}
	}
	recencyScores(scores)
	for i := range scores {
		scores[i].total = specificityWeight*scores[i].specificity +
			patternWeight*scores[i].pattern +
			recencyWeight*scores[i].recency
	}
	return scores
}

// ruleTieKey is the last resort in the ordering: a total, content-derived key
// so that two rules scoring identically are ordered by what they say and never
// by where they happen to sit in the file. Position is the one input the
// ordering may not have — it is age order, and ordering by it is the truncation
// this whole ranking replaces.
func ruleTieKey(r Rule) string {
	return strings.Join([]string{
		r.ID, r.Category, r.Pattern, r.Check, r.Added,
		strings.Join(r.Paths, ","), strings.Join(r.Source, ","),
	}, "\x00")
}

// selectRules ranks the candidates and returns the best max of them. The order
// is total and deterministic — score, then recency, then a content key — so the
// emitted set does not depend on the order the candidates arrived in, which is
// the order they were learned in.
//
// max <= 0 means no cap; the candidates are still returned in ranked order, so
// the highest-value rules head the checklist either way.
func selectRules(scores []ruleScore, max int) []Rule {
	sort.SliceStable(scores, func(i, j int) bool {
		a, b := scores[i], scores[j]
		if a.total != b.total {
			return a.total > b.total
		}
		if !a.added.Equal(b.added) {
			return a.added.After(b.added)
		}
		return ruleTieKey(a.rule) < ruleTieKey(b.rule)
	})
	if max > 0 && len(scores) > max {
		scores = scores[:max]
	}
	out := make([]Rule, len(scores))
	for i, s := range scores {
		out[i] = s.rule
	}
	return out
}
