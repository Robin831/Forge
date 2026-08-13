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

// sameFileNearAnchorThreshold is the stricter overlap-coefficient bar that
// applies when two findings share a file but have different anchors (or
// different lines). Raised above similarityDedupeThreshold because adjacent
// lines on the same file legitimately host distinct concerns more often than
// the exact-same-anchor case — but only modestly stricter, because the
// dominant noise source observed in shadow mode was the model anchoring at
// slightly different lines for what is, by content, the same paraphrased
// observation (Munin PR #3475 lines :172 vs :204, PR #3514 lines 60/61/62,
// PR #3523 lines 117/120 and 150/160). Calibration: realistic re-run
// paraphrases of the same concern measure ~0.45–0.55 overlap; unrelated
// adjacent-line findings measure well under 0.3.
const sameFileNearAnchorThreshold = 0.45

// nearAnchorMaxLineDistance is the maximum line gap at which two findings on
// the same file are considered "near" each other for the purposes of
// cross-anchor similarity dedup. Set to 15 to cover paraphrases of the same
// observation at adjacent statements within a small method while keeping
// genuinely distinct concerns at the top and bottom of a file apart. A
// finding with no parseable line number is treated as "near" any other on
// the same file (model anchors that name a method rather than a line still
// dedupe sensibly).
const nearAnchorMaxLineDistance = 15

// sameFileFarAnchorThreshold is the strictest overlap-coefficient bar, applied
// only during cross-run suppression when two findings share a file AND a
// category but sit further apart than nearAnchorMaxLineDistance. On a repeat
// review the whole PR often shifts (code inserted above moves every later
// line), so a regenerated paraphrase routinely drifts well past 15 lines and
// escaped the near-anchor regime entirely — each re-run then posted it as a
// fresh comment. Requiring the same category (stable across runs: it defaults
// to the emitting pass name) plus very high body overlap keeps genuinely
// distinct same-file concerns apart while catching the drifted rewording.
// Intra-run dedup deliberately does not use this regime: within one run the
// line numbers cannot have shifted, so a far pair there is two real findings.
const sameFileFarAnchorThreshold = 0.6

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

// ExistingFinding is the narrow shape suppressSimilarToExisting needs from a
// persisted finding: the anchor, the body, and the category (which gates the
// far-anchor regime). Defining it locally keeps the dedup helper decoupled
// from state.Finding (and from sql/db wiring in tests).
type ExistingFinding struct {
	Anchor   string
	Body     string
	Category string
}

// suppressSimilarToExisting drops new findings whose body has high overlap
// with an already-active finding at the same anchor — or at a nearby anchor
// on the same file. The model regenerates the same concern with slightly
// different wording on each re-run; without this pass each re-run would
// insert a fresh row because Finding.Hash includes the canonical body. The
// same-file regime catches the cross-run case where the rewording also
// drifts the line number (e.g. one run anchors at the method header, the
// next at the offending statement two lines below).
//
// Returns the filtered slice; the existing-findings argument is unchanged.
// When either side has fewer than minSimilarityTokens significant tokens the
// pair is skipped (too noisy to compare), matching the intra-run rule.
func suppressSimilarToExisting(newFindings []Finding, existing []ExistingFinding) []Finding {
	if len(newFindings) == 0 || len(existing) == 0 {
		return newFindings
	}
	// Pre-tokenize and pre-parse existing findings once. Same file index lets
	// the near-anchor check run in O(1) per (file, new) pair instead of
	// scanning the whole existing list for every new finding.
	type idxEntry struct {
		anchor   string
		parsed   anchorParts
		tokens   map[string]struct{}
		body     string
		category string
	}
	byFile := make(map[string][]idxEntry)
	for _, e := range existing {
		if e.Anchor == "" {
			continue
		}
		p := parseAnchor(e.Anchor)
		if p.file == "" {
			continue
		}
		byFile[p.file] = append(byFile[p.file], idxEntry{
			anchor:   e.Anchor,
			parsed:   p,
			tokens:   tokenizeForSimilarity(e.Body),
			body:     e.Body,
			category: e.Category,
		})
	}
	out := make([]Finding, 0, len(newFindings))
	for _, f := range newFindings {
		if f.Anchor == "" {
			out = append(out, f)
			continue
		}
		np := parseAnchor(f.Anchor)
		if np.file == "" {
			out = append(out, f)
			continue
		}
		peers := byFile[np.file]
		if len(peers) == 0 {
			out = append(out, f)
			continue
		}
		nt := tokenizeForSimilarity(f.Body)
		if len(nt) < minSimilarityTokens {
			out = append(out, f)
			continue
		}
		suppress := false
		for _, e := range peers {
			if len(e.tokens) < minSimilarityTokens {
				continue
			}
			threshold, eligible := pairThreshold(f.Anchor, e.anchor, np, e.parsed)
			if !eligible {
				// Cross-run only: a same-file, same-category pair beyond the
				// near-anchor distance is still comparable at the strictest
				// threshold — repeat reviews shift line numbers wholesale, so
				// a regenerated paraphrase routinely drifts past the near
				// window (see sameFileFarAnchorThreshold).
				threshold, eligible = farPairThreshold(f.Category, e.category, np, e.parsed)
			}
			if !eligible {
				continue
			}
			if overlapCoefficient(nt, e.tokens) >= threshold {
				suppress = true
				break
			}
		}
		if !suppress {
			out = append(out, f)
		}
	}
	return out
}

// dedupeBySimilarity collapses multiple findings whose bodies are highly
// similar. Two regimes apply:
//
//   - Same exact anchor: collapse when body overlap >= similarityDedupeThreshold.
//     This catches the common case where two passes (e.g. tests-missing and
//     logic) flag the same gap with different category labels and reworded
//     reasoning. Finding.Hash includes the category so the earlier hash dedup
//     misses these.
//   - Same file but different anchors (or different lines within
//     nearAnchorMaxLineDistance): collapse when body overlap >=
//     sameFileNearAnchorThreshold. Catches the pattern where the model emits
//     three paraphrases of one observation at adjacent statement boundaries
//     (e.g. Munin PR #3514 lines 60/61/62, PR #3523 lines 117/120 and 150/160).
//
// Findings on different files are never collapsed — the same advice landed
// in two files is genuinely two pieces of work. When two findings collapse,
// the higher-severity one is kept; severity ties keep the earlier finding so
// output ordering is deterministic.
func dedupeBySimilarity(findings []Finding) []Finding {
	if len(findings) <= 1 {
		return findings
	}
	keep := make([]bool, len(findings))
	for i := range keep {
		keep[i] = true
	}
	tokens := make([]map[string]struct{}, len(findings))
	parsed := make([]anchorParts, len(findings))
	for i, f := range findings {
		tokens[i] = tokenizeForSimilarity(f.Body)
		parsed[i] = parseAnchor(f.Anchor)
	}
	for i := 0; i < len(findings); i++ {
		if !keep[i] {
			continue
		}
		for j := i + 1; j < len(findings); j++ {
			if !keep[j] {
				continue
			}
			if len(tokens[i]) < minSimilarityTokens || len(tokens[j]) < minSimilarityTokens {
				continue
			}
			threshold, eligible := pairThreshold(findings[i].Anchor, findings[j].Anchor, parsed[i], parsed[j])
			if !eligible {
				continue
			}
			if overlapCoefficient(tokens[i], tokens[j]) < threshold {
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

// anchorParts is the parsed shape of a finding anchor: the file path and an
// optional line number. Anchors that don't include a parseable line have
// line=-1, which pairThreshold treats as "near any other line on the same file".
type anchorParts struct {
	file string
	line int // -1 when no parseable line number is present
}

// parseAnchor splits an anchor like "src/foo.go:42" or "src/foo.go:42-58" into
// its file and line components. The line is the first integer following the
// rightmost colon; ranges use the start line. When no integer is found, line is
// -1. The whole anchor falls through as the file when no colon is present so
// pairThreshold's file equality still works on raw symbol-style anchors.
func parseAnchor(anchor string) anchorParts {
	anchor = strings.TrimSpace(anchor)
	if anchor == "" {
		return anchorParts{file: "", line: -1}
	}
	idx := strings.LastIndex(anchor, ":")
	if idx < 0 {
		return anchorParts{file: anchor, line: -1}
	}
	file := anchor[:idx]
	tail := anchor[idx+1:]
	// Strip a "-end" suffix for ranges so we anchor on the start line.
	if dash := strings.IndexByte(tail, '-'); dash > 0 {
		tail = tail[:dash]
	}
	n := 0
	parsedAny := false
	for _, r := range tail {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
		parsedAny = true
	}
	if !parsedAny {
		// Anchor like "src/foo.go:MethodName" — keep the file as everything
		// before the colon and treat the line as unknown.
		return anchorParts{file: file, line: -1}
	}
	return anchorParts{file: file, line: n}
}

// pairThreshold returns the overlap threshold to require for collapse and
// whether the pair is eligible at all. Eligibility:
//   - Both anchors non-empty.
//   - Either anchors are exactly equal, OR files match (same file regime).
//   - When files match but lines differ, the lines must be within
//     nearAnchorMaxLineDistance. An unknown line (parseAnchor returned -1)
//     counts as near any other line on the same file.
//
// Same-anchor pairs use similarityDedupeThreshold; near-anchor pairs use the
// stricter sameFileNearAnchorThreshold.
func pairThreshold(anchorA, anchorB string, a, b anchorParts) (float64, bool) {
	if anchorA == "" || anchorB == "" {
		return 0, false
	}
	if anchorA == anchorB {
		return similarityDedupeThreshold, true
	}
	if a.file == "" || a.file != b.file {
		return 0, false
	}
	if a.line >= 0 && b.line >= 0 {
		diff := a.line - b.line
		if diff < 0 {
			diff = -diff
		}
		if diff > nearAnchorMaxLineDistance {
			return 0, false
		}
	}
	return sameFileNearAnchorThreshold, true
}

// farPairThreshold is the cross-run-only fallback regime for a pair that
// pairThreshold rejected: same non-empty file, same non-empty category, at any
// line distance, compared at sameFileFarAnchorThreshold. The category gate is
// what keeps two genuinely distinct concerns in one large file apart — they
// would have to come from the same pass AND share >60% of their meaningful
// tokens to collapse.
func farPairThreshold(catA, catB string, a, b anchorParts) (float64, bool) {
	if catA == "" || catA != catB {
		return 0, false
	}
	if a.file == "" || a.file != b.file {
		return 0, false
	}
	return sameFileFarAnchorThreshold, true
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
	return capNitsBudget(findings, limit)
}

// capNitsBudget is capNits with an explicit remaining budget: a budget < 0
// means "no cap", while a budget of 0 drops every Nit. The distinction matters
// for the cumulative per-PR Nit budget — a PR that already carries nit_cap
// posted Nits has a budget of exactly 0 for this run, which capNits' "<= 0 is
// unlimited" convention would invert into no cap at all (that inversion is why
// each re-run used to add nit_cap fresh Nits).
func capNitsBudget(findings []Finding, budget int) ([]Finding, int) {
	if budget < 0 {
		return findings, 0
	}
	out := make([]Finding, 0, len(findings))
	kept := 0
	dropped := 0
	for _, f := range findings {
		if f.Severity == SeverityNit {
			if kept >= budget {
				dropped++
				continue
			}
			kept++
		}
		out = append(out, f)
	}
	return out, dropped
}

// capTotalFindings bounds the total number of findings this run may emit,
// severity included: unlike the Nit cap, this is the hard brake on overall
// per-PR comment volume, so Important findings are subject to it too. A
// budget < 0 means "no cap". When over budget, the lowest-severity findings
// are dropped first (Nit, then PreExisting, then Important), later findings
// before earlier ones within a severity, so what survives is the highest-value
// prefix of the aggregated set. Returns the capped slice (order preserved) and
// the number of findings dropped.
func capTotalFindings(findings []Finding, budget int) ([]Finding, int) {
	if budget < 0 || len(findings) <= budget {
		return findings, 0
	}
	drop := len(findings) - budget
	dropIdx := make(map[int]bool, drop)
	for _, sev := range []Severity{SeverityNit, SeverityPreExisting, SeverityImportant} {
		for i := len(findings) - 1; i >= 0 && drop > 0; i-- {
			if findings[i].Severity == sev && !dropIdx[i] {
				dropIdx[i] = true
				drop--
			}
		}
		if drop == 0 {
			break
		}
	}
	out := make([]Finding, 0, budget)
	for i, f := range findings {
		if !dropIdx[i] {
			out = append(out, f)
		}
	}
	return out, len(findings) - len(out)
}
