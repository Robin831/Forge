package assay

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// stablePrefix renders the leading region of every Assay prompt — the region
// whose bytes depend ONLY on the pull request and the checkout, never on which
// run, push or session is building it.
//
// It exists because a prompt cache matches from the first byte. The
// shared-material-first ordering (see buildPassPrompt) already gives the five
// deep passes of ONE run a common prefix; this is the other axis — a SECOND
// run of the same PR must find those same opening bytes already in the cache
// rather than paying to write them again. That only holds if nothing
// run-varying is allowed above the volatile region, so the boundary is a
// function rather than an implicit substring: everything stablePrefix writes is
// stable, and everything a run varies is written after it by
// writeSharedPromptHead.
//
// The sections, in increasing order of volatility:
//
//   - sharedPromptPreamble — a constant.
//   - Repository review guidance — REVIEW.md from the anvil's checkout, size
//     capped at a rune boundary. Changes only when the file changes.
//   - Change context — the PR title and body. Per PR.
//   - Already-reported findings — per PR, and totally ordered here
//     (sortedPriorFindings) rather than left in whatever order the DB happened
//     to return, since pr_findings is queried without an ORDER BY.
//
// Deliberately NOT here, and written below it instead (by headStablePrefix):
//
//   - The incremental framing and its baseline SHA — per push, and adjacent to
//     the diff it describes, so a new push invalidates the cache at the diff
//     rather than several kilobytes above it.
//   - The elided-file note and the diff — per head.
//
// And below THAT, by writeSharedPromptHead: the triage notes, which are
// model-authored and so differ between two runs of the same head even when
// nothing else does; then the pass instructions and the JSON contract.
//
// TestStablePrefixIsByteStableAcrossRuns is the guard: two runs of the same PR
// differing only in run metadata must produce byte-identical output here.
func stablePrefix(req ReviewRequest) string {
	var b strings.Builder
	b.WriteString(sharedPromptPreamble)
	b.WriteString(repoGuidanceSection(req))
	b.WriteString(contextSection(req))
	b.WriteString(priorFindingsSection(req))
	return b.String()
}

// headStablePrefix is the second stability tier: stablePrefix plus everything
// that depends on the HEAD under review but not on which run is reviewing it —
// the incremental framing and its baseline SHA, the elided-file note, and the
// diff itself.
//
// The two tiers answer two different questions, which is why they are two
// functions rather than one. stablePrefix is what survives a PUSH: a new head
// changes the framing and the diff, and everything above them still matches, so
// the next review of the PR opens on a hit. headStablePrefix is what survives a
// RE-REVIEW of the same head — `forge assay rerun`, a re-dispatch, the five
// deep passes of one run reading what the primer wrote — and it is the tier the
// money is in: the diff is the bulk of every Assay prompt, and it lives here.
//
// It is stable only because the triage notes are written BELOW it. They are
// model-authored, so two runs of one head produce different notes; while they
// sat between the framing and the diff they were the ceiling on this tier, and
// every repeat review paid full write price for a byte-identical diff. Nothing
// model-authored, per-run or per-session may be added above the diff for that
// reason — TestSameHeadRerunReadsTheDiffFromCache is the guard.
//
// The diff it is handed is the caller's: triage gets the FILTERED diff and the
// deep passes get the SCOPED one, so the two tiers coincide for both only while
// triage has not narrowed the file set.
func headStablePrefix(req ReviewRequest, unifiedDiff string) string {
	var b strings.Builder
	b.WriteString(stablePrefix(req))
	b.WriteString(incrementalSection(req))
	b.WriteString(elidedFilesSection(req.elided))
	b.WriteString(diffSection(unifiedDiff))
	return b.String()
}

// sortedPriorFindings returns list in a total order derived entirely from the
// findings' own content, so the already-reported block reads identically no
// matter what order the rows arrived in.
//
// The order matters twice over. It is the block's byte order, so a differently
// ordered query result would rewrite the prefix for no semantic reason; and it
// decides which entries survive the maxPriorFindingsListed cap, which is
// applied to this order (by the caller) rather than to the DB's — truncating an
// arbitrary order means two runs over the same set can list two different
// hundreds.
//
// The key is (file, line, severity, title, digest): file and line put the list
// in the order a reader walks the diff, severity and title separate findings
// sharing an anchor, and the digest — over the whole record, so no two distinct
// entries can tie — is the final tiebreaker that makes the order total rather
// than merely mostly-defined.
//
// Both derived halves of that key are computed once per finding rather than
// inside the comparator, which is called O(n log n) times: parseAnchor rescans
// the anchor and priorFindingDigest hashes the whole record, and neither result
// depends on what it is being compared against.
//
// The line comparison goes through cmp.Compare rather than a subtraction.
// Anchors are model-authored text read back out of pr_findings and parseAnchor
// bounds neither the digit count nor the value, so a 19-digit anchor tail can
// overflow to an arbitrary int — and a subtraction over two such values wraps
// to the wrong sign, which is not a strict weak ordering, which lets
// slices.SortFunc return a different permutation per input order: precisely the
// cross-run instability this file exists to remove.
//
// The input is never sorted in place: one ReviewRequest is shared by the five
// deep passes building their prompts concurrently, and its PriorFindings slice
// with them.
func sortedPriorFindings(list []PriorFinding) []PriorFinding {
	// keyedPriorFinding is a prior finding decorated with the derived halves of
	// its sort key, so neither is re-derived per comparison. The anchor is kept
	// in the package's own parsed shape rather than flattened into fields of
	// this type: a shape parseAnchor grows a field for (it already discards a
	// range's end line) is then carried here too, instead of this decoration
	// silently keeping the older one.
	type keyedPriorFinding struct {
		finding PriorFinding
		parsed  anchorParts
		digest  string
	}
	keyed := make([]keyedPriorFinding, len(list))
	for i, p := range list {
		keyed[i] = keyedPriorFinding{
			finding: p,
			parsed:  parseAnchor(p.Anchor),
			digest:  priorFindingDigest(p),
		}
	}
	slices.SortFunc(keyed, func(a, b keyedPriorFinding) int {
		if c := strings.Compare(a.parsed.file, b.parsed.file); c != 0 {
			return c
		}
		if c := cmp.Compare(a.parsed.line, b.parsed.line); c != 0 {
			return c
		}
		if c := strings.Compare(a.finding.Severity, b.finding.Severity); c != 0 {
			return c
		}
		if c := strings.Compare(a.finding.Title, b.finding.Title); c != 0 {
			return c
		}
		return strings.Compare(a.digest, b.digest)
	})
	out := make([]PriorFinding, len(keyed))
	for i, k := range keyed {
		out[i] = k.finding
	}
	return out
}

// priorFindingDigest hashes a prior finding's whole record. It is the last
// tiebreaker in sortedPriorFindings, so it must cover every field that can
// distinguish two PriorFindings — a field left out of it (or added to
// PriorFinding and not added here) makes two distinct entries tie, and a tie
// under an unstable sort is resolved by the DB's arbitrary row order.
//
// Each field is written length-prefixed rather than separated by a delimiter.
// These are model-authored strings round-tripped through JSON and SQLite, so
// there is no byte they are guaranteed not to contain — including NUL — and a
// delimiter that a field CAN contain lets two different findings collide by
// shifting text across a field boundary. A length prefix needs no such
// assumption.
func priorFindingDigest(p PriorFinding) string {
	var b strings.Builder
	for _, field := range []string{p.Anchor, p.Severity, p.Title} {
		fmt.Fprintf(&b, "%d:%s", len(field), field)
	}
	if p.Resolved {
		b.WriteByte('1')
	} else {
		b.WriteByte('0')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
