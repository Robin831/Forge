package warden

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TokenSet is the bag-of-words representation used by Jaccard similarity.
// It is a set keyed by lowercase token; values are unused (struct{}).
type TokenSet map[string]struct{}

// minTokenLen is the minimum length for a token to be retained after
// tokenization. Shorter tokens (e.g. "is", "to", "be") add noise without
// improving similarity discrimination.
const minTokenLen = 3

// Tokenize splits s into a set of significant lowercase word tokens:
//   - Splits on any non-alphanumeric rune.
//   - Lowercases every token.
//   - Drops tokens shorter than minTokenLen.
//   - Drops stopwords (defined in learn.go).
//
// Returns an empty (non-nil) set for empty or all-stopword input so callers
// can safely range over the result without nil checks.
func Tokenize(s string) TokenSet {
	out := make(TokenSet)
	if s == "" {
		return out
	}
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, w := range fields {
		if utf8.RuneCountInString(w) < minTokenLen {
			continue
		}
		if stopWords[w] {
			continue
		}
		out[w] = struct{}{}
	}
	return out
}

// Jaccard returns |A ∩ B| / |A ∪ B|. Returns 0 when the union is empty so
// callers can treat "no shared signal" as "not similar" rather than divide
// by zero.
func Jaccard(a, b TokenSet) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	inter := 0
	// Iterate over the smaller set for O(min(|a|,|b|)) intersection.
	small, large := a, b
	if len(small) > len(large) {
		small, large = large, small
	}
	for t := range small {
		if _, ok := large[t]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// RuleWordBag returns the union of tokens from a rule's Pattern and Check
// fields. This is the canonical bag-of-words used to score rule similarity.
func RuleWordBag(r Rule) TokenSet {
	bag := Tokenize(r.Pattern)
	for t := range Tokenize(r.Check) {
		bag[t] = struct{}{}
	}
	return bag
}

// Cluster represents a group of rules that the similarity pass considers
// near-duplicates. MaxSimilarity is the highest Jaccard score observed
// between any pair in the cluster (useful for surfacing diagnostics).
type Cluster struct {
	Rules []Rule
	// Indices are the positions in the slice handed to the clusterer that
	// Rules were taken from, in the same order. They are what identifies a
	// cluster member: a rule's ID is written by whichever distillation
	// session produced it and two rules can carry the same one, so a caller
	// that removes cluster members from a file by ID removes every rule
	// sharing that ID — including one the cluster never contained.
	Indices       []int
	MaxSimilarity float64
}

// ClusterByJaccard groups rules whose pairwise Jaccard similarity on
// RuleWordBag meets or exceeds threshold. Clustering uses union-find:
// every pair scoring >= threshold is unioned, so transitive matches end up
// in the same cluster even when not all pairs cross the threshold directly.
//
// Only clusters containing more than one rule are returned; singletons
// (rules with no similar neighbours) are dropped. The order of returned
// clusters mirrors the order of the first rule in each cluster as it
// appeared in the input — stable for deterministic downstream processing.
//
// A non-positive threshold is a no-op and returns nil: with the criterion
// disabled there is nothing to cluster ON, and unioning every pair (which
// is what a literal reading of ">= 0" would do) is not a plausible reading
// of "the operator turned dedup off".
//
// No production code calls this — every call site uses ClusterNearDuplicates
// so the overlap criterion is explicit — but it is retained as the one-knob
// form for tests and for callers that genuinely want Jaccard alone.
func ClusterByJaccard(rules []Rule, threshold float64) []Cluster {
	return ClusterNearDuplicates(rules, DedupParams{Jaccard: threshold})
}

// GroupRulesByCategory partitions rules by Category, preserving first-seen
// order within each category and across categories. Rules with an empty
// Category are grouped under "".
func GroupRulesByCategory(rules []Rule) ([]string, map[string][]Rule) {
	var order []string
	byCat := make(map[string][]Rule)
	for _, r := range rules {
		if _, ok := byCat[r.Category]; !ok {
			order = append(order, r.Category)
		}
		byCat[r.Category] = append(byCat[r.Category], r)
	}
	return order, byCat
}

// Overlap returns |A ∩ B| / min(|A|, |B|) — the overlap (containment)
// coefficient. It answers a different question from Jaccard: "is the smaller
// rule's vocabulary already contained in the larger one?" rather than "do the
// two rules use the same vocabulary?".
//
// The distinction is load-bearing for learned review rules, which arrive at
// wildly different verbosities from one distillation session to the next. A
// terse restatement of a verbose rule — 12 tokens, every one of them present
// in the other rule's 40 — scores 0.30 on Jaccard (below any usable
// threshold) and 1.00 here. Jaccard structurally cannot see that case: the
// union it divides by is dominated by the longer rule's extra prose, so the
// score is capped near |small|/|large| however complete the containment is.
//
// Returns 0 when either set is empty so an empty bag never reads as a
// perfect containment.
func Overlap(a, b TokenSet) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	small, large := a, b
	if len(small) > len(large) {
		small, large = large, small
	}
	for t := range small {
		if _, ok := large[t]; ok {
			inter++
		}
	}
	return float64(inter) / float64(len(small))
}

// MinOverlapTokens is the smallest word bag the overlap criterion is applied
// to. Overlap is unstable for tiny bags — a three-token rule whose every
// token happens to appear in a forty-token one scores a perfect 1.0 while
// saying something entirely different — so a rule below this size is judged
// on Jaccard alone. The measured corpus (727 learned rules on this repo) has
// a p10 bag size of 24, so the guard costs nothing on real rules and only
// screens out the degenerate ones.
const MinOverlapTokens = 8

// DefaultOverlapThreshold is the shipped overlap-coefficient threshold.
//
// It is calibrated rather than guessed. Measured over the 727 rules in this
// repository's own .forge/warden-rules.yaml:
//
//	overlap >= 0.50 → 27 clusters / 63 rules (starts admitting marginals)
//	overlap >= 0.55 → 17 clusters / 35 rules, largest cluster 3
//	overlap >= 0.60 →  9 clusters / 18 rules (misses known duplicates)
//
// At 0.55 every cluster is a genuine near-duplicate (three separate rules all
// saying "a doc comment must match the implementation", two saying "the PR
// description must match the diff", and so on), which is the precision the
// pass needs: a merge is irreversible from the reviewer's point of view.
const DefaultOverlapThreshold = 0.55

// DedupParams are the two independent criteria under which two rules are
// considered near-duplicates. They are two criteria rather than one tuned
// number because the failure they were introduced for is not a threshold
// that was set slightly too high — it is a metric that cannot see the case
// at any threshold.
//
// Measured on this repository's own 727-rule file, the shipped Jaccard
// threshold of 0.6 clusters NOTHING: not one pair out of 263,901 reaches it,
// while the file demonstrably holds three copies of "a doc comment must match
// the implementation". Real near-duplicates score 0.28–0.50 on Jaccard, so
// the only Jaccard threshold that fires is one low enough to also merge rules
// that merely share a topic. Overlap separates the two populations cleanly
// (see DefaultOverlapThreshold), which is why it is the criterion that does
// the work and Jaccard is kept as the stricter, order-independent backstop.
type DedupParams struct {
	// Jaccard is the |A ∩ B| / |A ∪ B| threshold. Values <= 0 disable the
	// criterion.
	Jaccard float64
	// Overlap is the |A ∩ B| / min(|A|, |B|) threshold, applied only to
	// pairs where both bags hold at least MinOverlapTokens tokens. Values
	// <= 0 disable the criterion.
	Overlap float64
}

// IsZero reports whether neither criterion is active, i.e. no pair of rules
// can ever be judged a near-duplicate. Callers use it to skip the pass
// outright rather than walk an O(n²) comparison that cannot return anything.
func (p DedupParams) IsZero() bool {
	return p.Jaccard <= 0 && p.Overlap <= 0
}

// NearDuplicate reports whether two word bags meet either criterion, and the
// score that decided it. When both fire, the larger score is returned so the
// reported MaxSimilarity is never an understatement of how alike the pair is.
func NearDuplicate(a, b TokenSet, p DedupParams) (bool, float64) {
	var best float64
	var hit bool
	if p.Jaccard > 0 {
		if s := Jaccard(a, b); s >= p.Jaccard {
			hit, best = true, s
		}
	}
	if p.Overlap > 0 && len(a) >= MinOverlapTokens && len(b) >= MinOverlapTokens {
		if s := Overlap(a, b); s >= p.Overlap && s > best {
			hit, best = true, s
		}
	}
	return hit, best
}

// ClusterNearDuplicates groups rules that NearDuplicate joins under p, using
// the same union-find, first-occurrence ordering and singleton-dropping
// contract as ClusterByJaccard. It makes no claim about the rules' categories
// — the caller decides whether to partition by category first.
//
// That choice is the second half of the PR #682 failure: a learned rule's
// category is the model's to pick, and one check arrives labelled `style` in
// one session, `documentation` in the next and `other` in the third, so a
// pass that clusters strictly within a category cannot see a cluster that a
// human reads as obviously one rule. The whole-file pass keeps grouping by
// category (merging across categories in a 700-rule file would change what
// the category filter selects for every review); the batch pass deliberately
// does not.
func ClusterNearDuplicates(rules []Rule, p DedupParams) []Cluster {
	n := len(rules)
	if n < 2 || p.IsZero() {
		return nil
	}

	bags := make([]TokenSet, n)
	for i, r := range rules {
		bags[i] = RuleWordBag(r)
	}

	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}

	maxSim := make(map[int]float64, n)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			ok, sim := NearDuplicate(bags[i], bags[j], p)
			if !ok {
				continue
			}
			ri, rj := find(i), find(j)
			if ri != rj {
				// Carry the losing root's accumulated maxSim to the winner
				// before re-parenting so it isn't orphaned.
				if maxSim[ri] > maxSim[rj] {
					maxSim[rj] = maxSim[ri]
				}
				parent[ri] = rj
			}
			root := find(i)
			if sim > maxSim[root] {
				maxSim[root] = sim
			}
		}
	}

	type bucket struct {
		members []int
		firstIx int
	}
	buckets := make(map[int]*bucket)
	for i := 0; i < n; i++ {
		root := find(i)
		b, ok := buckets[root]
		if !ok {
			b = &bucket{firstIx: i}
			buckets[root] = b
		}
		b.members = append(b.members, i)
	}

	roots := make([]int, 0, len(buckets))
	for r := range buckets {
		roots = append(roots, r)
	}
	sort.Slice(roots, func(i, j int) bool {
		return buckets[roots[i]].firstIx < buckets[roots[j]].firstIx
	})

	var out []Cluster
	for _, root := range roots {
		b := buckets[root]
		if len(b.members) < 2 {
			continue
		}
		c := Cluster{
			Rules:         make([]Rule, 0, len(b.members)),
			Indices:       make([]int, 0, len(b.members)),
			MaxSimilarity: maxSim[root],
		}
		for _, ix := range b.members {
			c.Rules = append(c.Rules, rules[ix])
			c.Indices = append(c.Indices, ix)
		}
		out = append(out, c)
	}
	return out
}
