package assay

import (
	"crypto/sha256"
	"encoding/hex"
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
// Deliberately NOT here, and written below it instead:
//
//   - The incremental framing and its baseline SHA — per push, and adjacent to
//     the diff it describes, so a new push invalidates the cache at the diff
//     rather than several kilobytes above it.
//   - The triage notes — model-authored, so they differ between two runs of the
//     same head even when nothing else does.
//   - The diff, the pass instructions and the JSON contract.
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
// than merely mostly-defined. Comparing the digest instead of continuing to
// compare raw text also keeps the cost of the last tiebreaker independent of
// how long a title is.
//
// The input is never sorted in place: one ReviewRequest is shared by the five
// deep passes building their prompts concurrently, and its PriorFindings slice
// with them.
func sortedPriorFindings(list []PriorFinding) []PriorFinding {
	out := slices.Clone(list)
	slices.SortFunc(out, func(a, b PriorFinding) int {
		pa, pb := parseAnchor(a.Anchor), parseAnchor(b.Anchor)
		if c := strings.Compare(pa.file, pb.file); c != 0 {
			return c
		}
		if pa.line != pb.line {
			return pa.line - pb.line
		}
		if c := strings.Compare(a.Severity, b.Severity); c != 0 {
			return c
		}
		if c := strings.Compare(a.Title, b.Title); c != 0 {
			return c
		}
		return strings.Compare(priorFindingDigest(a), priorFindingDigest(b))
	})
	return out
}

// priorFindingDigest hashes a prior finding's whole record. It is the last
// tiebreaker in sortedPriorFindings, and the fields are joined with a separator
// that cannot occur in any of them so two different findings cannot collide by
// shifting text across a field boundary.
func priorFindingDigest(p PriorFinding) string {
	var b strings.Builder
	b.WriteString(p.Anchor)
	b.WriteByte(0)
	b.WriteString(p.Severity)
	b.WriteByte(0)
	b.WriteString(p.Title)
	b.WriteByte(0)
	if p.Resolved {
		b.WriteByte('1')
	} else {
		b.WriteByte('0')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
