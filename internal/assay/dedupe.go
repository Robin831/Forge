package assay

import "strings"

// similarityDedupeThreshold is the overlap-coefficient threshold above which
// two findings on the same anchor are treated as semantically identical.
// Overlap coefficient (|A ∩ B| / min(|A|, |B|)) is the right metric here
// because paraphrases from different passes have very unequal verbosity —
// Jaccard penalises the longer body too harshly. Tuned against real Munin
// output where tests-missing and logic each flagged the same untested code
// path: overlap ~0.45–0.55, while unrelated findings on the same line measure
// well under 0.2.
const similarityDedupeThreshold = 0.4

// minSimilarityTokens is the minimum number of meaningful tokens both findings
// must contain before similarity-dedup considers them. Tiny bodies are too
// noisy to compare reliably with Jaccard — a one-line nit and a one-line
// Important happening to share the same anchor should both survive.
const minSimilarityTokens = 5

// dedupeByHash returns findings with duplicate hashes removed, preserving the
// first occurrence and overall order. Findings with an empty hash are dropped
// (they were never finalized and cannot be persisted or deduped reliably).
func dedupeByHash(findings []Finding) []Finding {
	seen := make(map[string]bool, len(findings))
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if f.Hash == "" || seen[f.Hash] {
			continue
		}
		seen[f.Hash] = true
		out = append(out, f)
	}
	return out
}

// dedupeBySimilarity collapses multiple findings on the same anchor whose
// bodies are highly similar. This catches the common case where two passes
// (e.g. tests-missing and logic) flag the same gap with different category
// labels and reworded reasoning — Finding.Hash includes the category so the
// earlier hash dedup misses them.
//
// When two findings on the same anchor exceed similarityDedupeThreshold, the
// higher-severity one is kept. Severity ties go to the earlier finding so
// output ordering is deterministic. Findings on different anchors are never
// merged, even if their bodies are similar — same advice on different lines
// is genuinely distinct work to do.
func dedupeBySimilarity(findings []Finding) []Finding {
	if len(findings) <= 1 {
		return findings
	}
	keep := make([]bool, len(findings))
	for i := range keep {
		keep[i] = true
	}
	tokens := make([]map[string]struct{}, len(findings))
	for i, f := range findings {
		tokens[i] = tokenizeForSimilarity(f.Body)
	}
	for i := 0; i < len(findings); i++ {
		if !keep[i] {
			continue
		}
		for j := i + 1; j < len(findings); j++ {
			if !keep[j] {
				continue
			}
			if findings[i].Anchor == "" || findings[i].Anchor != findings[j].Anchor {
				continue
			}
			if len(tokens[i]) < minSimilarityTokens || len(tokens[j]) < minSimilarityTokens {
				continue
			}
			if overlapCoefficient(tokens[i], tokens[j]) < similarityDedupeThreshold {
				continue
			}
			// Collapse to the higher-severity finding. Ties keep i (earlier).
			if severityRank(findings[j].Severity) > severityRank(findings[i].Severity) {
				keep[i] = false
				break // i is dead; nothing else can collapse into it
			}
			keep[j] = false
		}
	}
	out := make([]Finding, 0, len(findings))
	for i, f := range findings {
		if keep[i] {
			out = append(out, f)
		}
	}
	return out
}

// tokenizeForSimilarity lowercases the input, splits on non-alphanumeric runs,
// and keeps tokens of length >= 3 that are not in a small English stopword set.
// Identifiers (camelCase, snake_case) are preserved because they are the
// highest-signal tokens for code-review bodies.
func tokenizeForSimilarity(s string) map[string]struct{} {
	set := make(map[string]struct{})
	if s == "" {
		return set
	}
	s = strings.ToLower(s)
	var b strings.Builder
	flush := func() {
		if b.Len() >= 3 {
			t := b.String()
			if !similarityStopword(t) {
				set[t] = struct{}{}
			}
		}
		b.Reset()
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return set
}

// similarityStopword returns true for common English filler words that add
// noise to Jaccard scores without changing meaning. Deliberately small —
// over-aggressive stopword lists hurt precision more than they help.
func similarityStopword(t string) bool {
	switch t {
	case "the", "and", "for", "that", "this", "with", "from", "into", "are",
		"not", "but", "any", "all", "its", "has", "have", "was", "were",
		"will", "would", "could", "should", "via", "per", "out":
		return true
	}
	return false
}

// overlapCoefficient returns |A ∩ B| / min(|A|, |B|). Preferred over Jaccard
// for paraphrase detection because two findings on the same concern often
// differ wildly in length — the verbose one would dominate a Jaccard union
// and depress the score even when every meaningful token of the shorter one
// is present.
func overlapCoefficient(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	for t := range small {
		if _, ok := large[t]; ok {
			inter++
		}
	}
	denom := len(small)
	if denom == 0 {
		return 0
	}
	return float64(inter) / float64(denom)
}

// severityRank orders severities for the similarity-collapse tiebreaker:
// Important outranks PreExisting outranks Nit. Unknown severities sort lowest.
func severityRank(s Severity) int {
	switch s {
	case SeverityImportant:
		return 3
	case SeverityPreExisting:
		return 2
	case SeverityNit:
		return 1
	}
	return 0
}

// suppressPostedNits drops Nit findings whose hash was already posted on a prior
// review of the same PR. Important and PreExisting findings are never
// suppressed. Returns the filtered slice and the number of Nits dropped.
func suppressPostedNits(findings []Finding, posted map[string]bool) ([]Finding, int) {
	if len(posted) == 0 {
		return findings, 0
	}
	out := make([]Finding, 0, len(findings))
	dropped := 0
	for _, f := range findings {
		if f.Severity == SeverityNit && posted[f.Hash] {
			dropped++
			continue
		}
		out = append(out, f)
	}
	return out, dropped
}

// capNits limits the number of Nit findings to limit, preserving order and
// keeping the first limit Nits encountered. Important and PreExisting findings
// are always kept. A limit <= 0 means "no cap". Returns the capped slice and
// the number of Nits dropped.
func capNits(findings []Finding, limit int) ([]Finding, int) {
	if limit <= 0 {
		return findings, 0
	}
	out := make([]Finding, 0, len(findings))
	kept := 0
	dropped := 0
	for _, f := range findings {
		if f.Severity == SeverityNit {
			if kept >= limit {
				dropped++
				continue
			}
			kept++
		}
		out = append(out, f)
	}
	return out, dropped
}
