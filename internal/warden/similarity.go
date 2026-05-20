package warden

import (
	"strings"
	"unicode"
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
		if len(w) < minTokenLen {
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
	Rules         []Rule
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
func ClusterByJaccard(rules []Rule, threshold float64) []Cluster {
	n := len(rules)
	if n < 2 {
		return nil
	}

	// Pre-compute word bags once.
	bags := make([]TokenSet, n)
	for i, r := range rules {
		bags[i] = RuleWordBag(r)
	}

	// Union-find over rule indices.
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

	// Track the max similarity per cluster root so we can report it later.
	maxSim := make(map[int]float64, n)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			sim := Jaccard(bags[i], bags[j])
			if sim < threshold {
				continue
			}
			ri, rj := find(i), find(j)
			if ri != rj {
				parent[ri] = rj
			}
			// Record max similarity against the (new) root.
			root := find(i)
			if sim > maxSim[root] {
				maxSim[root] = sim
			}
		}
	}

	// Collect clusters in first-occurrence order.
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

	// Sort by firstIx to keep output deterministic.
	roots := make([]int, 0, len(buckets))
	for r := range buckets {
		roots = append(roots, r)
	}
	// Simple insertion sort — small N (clusters per category).
	for i := 1; i < len(roots); i++ {
		for j := i; j > 0 && buckets[roots[j-1]].firstIx > buckets[roots[j]].firstIx; j-- {
			roots[j-1], roots[j] = roots[j], roots[j-1]
		}
	}

	var out []Cluster
	for _, root := range roots {
		b := buckets[root]
		if len(b.members) < 2 {
			continue
		}
		c := Cluster{
			Rules:         make([]Rule, 0, len(b.members)),
			MaxSimilarity: maxSim[root],
		}
		for _, ix := range b.members {
			c.Rules = append(c.Rules, rules[ix])
		}
		out = append(out, c)
	}
	return out
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
