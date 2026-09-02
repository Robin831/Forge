package smelter

import (
	"fmt"
	"strings"

	"github.com/Robin831/Forge/internal/textfmt"
	"github.com/Robin831/Forge/internal/warden"
)

// PassResults aggregates the structured outputs of the smelter's three flush
// passes for a single anvil. Each field is independent: a pass that did not
// run, or that produced no changes, contributes an empty slice. The result is
// fed to buildCommitMessage so a single commit message can summarize all
// passes in one PR per anvil per run.
type PassResults struct {
	// Added lists the IDs of rules newly inserted from the pending queue
	// (Pass 1, before consolidation). IDs are taken from the rule YAML.
	Added []string

	// Consolidated holds the per-cluster merge outcomes from Pass 1.
	Consolidated []warden.MergeResult

	// Archived holds the rules moved to the archive store by the two passes
	// that remove rules from the active file: the staleness sweep
	// (ArchiveReason="stale") and the file-ceiling eviction
	// (ArchiveReason="over-cap"). One list because the archive store takes one
	// write — but the two are different claims about a rule, so every
	// aggregate rendered from it splits them again through archivedByReason
	// (ArchivedByReason for callers outside this package). Reading
	// len(Archived) as a stale count is the bug that helper exists to prevent.
	//
	// Pass 1 duplicates archived with ArchiveReason="duplicate" are *not*
	// included here — they are surfaced through Consolidated.
	Archived []warden.ArchivedRule

	// Backfilled lists the IDs of rules whose Paths field was populated by
	// Pass 3 from the changed files of the rule's source PR(s).
	Backfilled []string

	// Contradictions holds pairs of rules from one source PR that prescribe
	// opposite orderings. They are reported, never resolved — see
	// warden.Contradiction — so they are deliberately NOT part of
	// HasChanges: a batch whose only finding is a contradiction has an
	// unchanged rules file and nothing to commit.
	Contradictions []warden.Contradiction
}

// HasChanges reports whether at least one pass produced an outcome. When
// false, callers should skip committing — the rules file is byte-identical
// to main and a commit would be empty.
func (p PassResults) HasChanges() bool {
	return len(p.Added) > 0 || len(p.Consolidated) > 0 || len(p.Archived) > 0 || len(p.Backfilled) > 0
}

// buildCommitMessage renders the single commit message used for the
// forge/warden-learn-batch/<anvil> PR. The message has a one-line subject
// summarizing what happened (kept short for git log readability) followed
// by up to five labeled sections — "Added:", "Consolidated:", "Archived:",
// "Backfilled:" and "Contradictions:" — each listing the affected rule
// identifiers. Sections with no entries are omitted. The subject always ends in "[no-changelog]" so the
// changelog validator skips this PR.
func buildCommitMessage(passes PassResults) string {
	subject := buildCommitSubject(passes)
	body := buildCommitBody(passes)
	if body == "" {
		return subject
	}
	return subject + "\n\n" + body
}

// buildCommitSubject produces the one-line subject. Mirrors the legacy
// "forge: ... [no-changelog]" form so that downstream consumers (changelog
// validator, PR title heuristics) keep working.
func buildCommitSubject(passes PassResults) string {
	var parts []string
	if n := len(passes.Added); n > 0 {
		parts = append(parts, fmt.Sprintf("learn %d warden rule(s)", n))
	}
	if n := len(passes.Consolidated); n > 0 {
		parts = append(parts, fmt.Sprintf("consolidate %d cluster(s)", n))
	}
	if stale, overCap := archivedByReason(passes.Archived); stale > 0 || overCap > 0 {
		if stale > 0 {
			parts = append(parts, fmt.Sprintf("archive %d stale rule(s)", stale))
		}
		if overCap > 0 {
			parts = append(parts, fmt.Sprintf("evict %d over-cap rule(s)", overCap))
		}
	}
	if n := len(passes.Backfilled); n > 0 {
		parts = append(parts, fmt.Sprintf("backfill paths on %d rule(s)", n))
	}
	if len(parts) == 0 {
		// Defensive fallback: callers must not invoke buildCommitMessage when
		// nothing happened, but keep the legacy form to avoid producing an
		// empty subject if invariants ever shift.
		return "forge: batch warden rule update [no-changelog]"
	}
	return "forge: " + strings.Join(parts, ", ") + " [no-changelog]"
}

// buildCommitBody renders the five labeled sections. Sections with no
// entries are omitted entirely so the body stays compact when only one
// pass produced changes.
func buildCommitBody(passes PassResults) string {
	var sections []string
	if s := formatAddedSection(passes.Added); s != "" {
		sections = append(sections, s)
	}
	if s := formatConsolidatedSection(passes.Consolidated); s != "" {
		sections = append(sections, s)
	}
	if s := formatArchivedSection(passes.Archived); s != "" {
		sections = append(sections, s)
	}
	if s := formatBackfilledSection(passes.Backfilled); s != "" {
		sections = append(sections, s)
	}
	// Last, and phrased as unfinished work: a contradiction is the one thing
	// in this message the smelter did not act on.
	if s := formatContradictionsSection(passes.Contradictions); s != "" {
		sections = append(sections, s)
	}
	return strings.Join(sections, "\n\n")
}

func formatAddedSection(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Added: %d rule(s)\n", len(ids))
	for _, id := range ids {
		fmt.Fprintf(&sb, "- %s\n", displayID(id))
	}
	return strings.TrimRight(sb.String(), "\n")
}

func formatConsolidatedSection(summary []warden.MergeResult) string {
	if len(summary) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Consolidated: %d cluster(s)\n", len(summary))
	for _, r := range summary {
		// A category is model-authored too: distillRule keeps whatever the
		// provider's JSON returned and nothing downstream validates a
		// character of it, so it gets the same closed alphabet as an ID.
		cat := safeRuleID(r.Category)
		if cat == "" {
			cat = "(no category)"
		}
		replacedDisplay := make([]string, len(r.ReplacedIDs))
		for i, id := range r.ReplacedIDs {
			replacedDisplay[i] = displayID(id)
		}
		fmt.Fprintf(&sb, "- [%s] %s ← %s (sim=%.2f)\n",
			cat, displayID(r.Merged.ID), strings.Join(replacedDisplay, ", "), r.MaxSimilarity)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// formatContradictionsSection renders the pairs the smelter found and did
// not act on, as the last section of the commit body.
//
// It builds the bullets here rather than calling warden.FormatContradictions
// for the same reason buildPRBody does: that renderer prints
// Contradiction.Detail, which contradictionDetail interpolates the two rule
// IDs and the source reference into with %s. All three are model output —
// distillRule keeps whatever id the provider's JSON returned and
// AddRuleDistinct validates no character of it — and this message reaches
// `git commit -m` on a branch that is opened as a PR against main, where an
// ID carrying "\n\nCloses #123" or a "Co-authored-by:" line would forge
// structure in a commit Forge authors, and any extra section forges content
// in the message a reviewer reads to decide whether to merge the batch.
//
// Kind is one of warden's own constants, so it is printed as it stands.
func formatContradictionsSection(cs []warden.Contradiction) string {
	if len(cs) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Contradictions: %d pair(s) — NOT resolved, a human must pick the convention\n", len(cs))
	for _, c := range cs {
		fmt.Fprintf(&sb, "- [%s] %s and %s (both from %s) %s\n",
			c.Kind, displayID(c.A.ID), displayID(c.B.ID), safeRuleID(c.Source), c.Kind.Disagreement())
	}
	return strings.TrimRight(sb.String(), "\n")
}

func formatArchivedSection(archived []warden.ArchivedRule) string {
	if len(archived) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Archived: %d rule(s)\n", len(archived))
	for _, r := range archived {
		reason := r.ArchiveReason
		if reason == "" {
			reason = "stale"
		}
		fmt.Fprintf(&sb, "- %s (%s)\n", displayID(r.ID), reason)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// archivedByReason splits an archive list into the two reasons a pass in this
// package can produce. PassResults.Archived is one list because the archive
// store takes one write, but "aged out with no recent source activity" and
// "lost a slot to the file ceiling" are different claims about a rule, and
// every aggregate rendered from that list used to call all of them stale — a
// commit subject reading "archive 12 stale rule(s)" for twelve rules that were
// evicted the day they were learned. A reason this function does not recognise
// (a duplicate folded in by some future caller) counts as neither, so a new
// reason cannot silently be reported as an old one.
// ArchivedByReason is archivedByReason over this result's own archive list,
// exported for the surfaces outside this package that render it — `forge warden
// consolidate`, which otherwise prints every eviction as a rule that "aged out
// with no recent source activity".
func (p PassResults) ArchivedByReason() (stale, overCap int) {
	return archivedByReason(p.Archived)
}

func archivedByReason(archived []warden.ArchivedRule) (stale, overCap int) {
	for _, r := range archived {
		switch r.ArchiveReason {
		case warden.ArchiveReasonOverCap:
			overCap++
		case warden.ArchiveReasonStale, "":
			// An empty reason is rendered as "stale" by
			// formatArchivedSection, so it is counted as one here too.
			stale++
		}
	}
	return stale, overCap
}

func formatBackfilledSection(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Backfilled: %d rule(s)\n", len(ids))
	for _, id := range ids {
		fmt.Fprintf(&sb, "- %s\n", displayID(id))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// displayID renders a rule ID for a commit message or a PR body: sanitized,
// with an empty one rendered as "(no id)" so the bullet list does not
// contain dangling "- " entries when a rule arrives without an ID.
//
// Sanitized because a rule ID is not Forge's text. distillRule unmarshals a
// learned rule from the model's JSON and keeps whatever `id` it returned,
// the distillation prompt is built from Copilot's comments on a
// contributor's PR, and LoadRules/AddRule/AddRuleDistinct validate no
// character of it. These bullets are published under Forge's own GitHub
// identity — `gh pr create --body` — so an ID carrying a backtick, a
// newline, an `@org/team` or an HTML comment would break out of its code
// span, forge further body sections or mass-notify a team.
//
// The alphabet is closed rather than escaped, the same treatment
// diff.SafePath gives an elided filename and for the same reason: the
// injection never needs to break the fence, only to be read as an
// instruction. A rule ID is kebab-case by construction, so nothing
// legitimate is lost.
func displayID(id string) string {
	safe := safeRuleID(id)
	if safe == "" {
		return "(no id)"
	}
	return safe
}

// maxRuleIDBytes bounds a rendered ID. A learned ID is a short kebab-case
// slug; anything longer is a model that ignored the contract, and an
// unbounded one is a single bullet that pushes the rest of the body out of
// view. Bytes and runes are the same count here — the alphabet safeRuleID
// leaves behind is ASCII by construction.
const maxRuleIDBytes = 120

// safeRuleID reduces s to the closed alphabet [A-Za-z0-9._/:#-], collapsing
// every other run to a single "?", and truncates it at a rune boundary.
// Returns "" for an empty or all-whitespace input so displayID can render
// the "(no id)" placeholder rather than a lone "?".
//
// It is the renderer for a rule's SOURCE too, which is why ":" and "#" are
// in the alphabet: a source reference is `copilot:PR#708`, and reducing it
// to `copilot?PR?708` would mangle every legitimate one to guard against two
// characters that cannot do anything here — neither closes a code span, and
// "#" only opens a heading at the start of a line, which is a position no
// input can reach once every line break has been collapsed. The space is
// deliberately NOT in the alphabet: it is what a forged bullet or a fake
// parenthetical needs to read as structure.
func safeRuleID(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	var sb strings.Builder
	lastQ := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '/', r == '-', r == ':', r == '#':
			sb.WriteRune(r)
			lastQ = false
		default:
			if !lastQ {
				sb.WriteByte('?')
				lastQ = true
			}
		}
	}
	return textfmt.TruncateRunes(sb.String(), maxRuleIDBytes)
}

// buildPRBody renders the PR description for the smelter batch PR, describing
// all pass outcomes so the body stays accurate when only consolidation,
// archival, or backfill changes are present (added count may be zero).
func buildPRBody(passes PassResults) string {
	var lines []string
	lines = append(lines, "Automated batch update of warden rules.", "")
	if n := len(passes.Added); n > 0 {
		lines = append(lines, fmt.Sprintf("- %d new rule(s) learned from Copilot review comments.", n))
	}
	if n := len(passes.Consolidated); n > 0 {
		lines = append(lines, fmt.Sprintf("- %d cluster(s) of near-duplicate rules consolidated.", n))
	}
	if stale, overCap := archivedByReason(passes.Archived); stale > 0 || overCap > 0 {
		if stale > 0 {
			lines = append(lines, fmt.Sprintf("- %d stale rule(s) archived.", stale))
		}
		if overCap > 0 {
			lines = append(lines, fmt.Sprintf("- %d rule(s) evicted into the archive because the active file was over its size ceiling.", overCap))
		}
	}
	if n := len(passes.Backfilled); n > 0 {
		lines = append(lines, fmt.Sprintf("- %d rule(s) with paths backfilled from PR changed files.", n))
	}
	if n := len(passes.Contradictions); n > 0 {
		lines = append(lines, "", fmt.Sprintf("**%d contradictory rule pair(s) need a human decision.** Each pair was learned from one source PR and prescribes opposite orderings, so the Warden flags an implementation whichever convention it follows. Nothing was merged or dropped for them:", n))
		for _, c := range passes.Contradictions {
			// Every field but Kind is model-authored: the two IDs come out
			// of the distillation JSON and Source out of the rule's own
			// source list. Kind is one of this package's own constants.
			lines = append(lines, fmt.Sprintf("- `%s` vs `%s` (%s, %s)",
				displayID(c.A.ID), displayID(c.B.ID), safeRuleID(c.Source), c.Kind))
		}
	}
	lines = append(lines, "", "Generated by the Forge Smelter.")
	return strings.Join(lines, "\n")
}

// passResultsSummary returns a compact human-readable string listing each
// non-zero pass outcome. Used in log messages and events so operators can
// see the full picture even when the added count is zero.
func passResultsSummary(passes PassResults) string {
	var parts []string
	if n := len(passes.Added); n > 0 {
		parts = append(parts, fmt.Sprintf("%d added", n))
	}
	if n := len(passes.Consolidated); n > 0 {
		parts = append(parts, fmt.Sprintf("%d consolidated", n))
	}
	if stale, overCap := archivedByReason(passes.Archived); stale > 0 || overCap > 0 {
		if stale > 0 {
			parts = append(parts, fmt.Sprintf("%d archived", stale))
		}
		if overCap > 0 {
			parts = append(parts, fmt.Sprintf("%d evicted over cap", overCap))
		}
	}
	if n := len(passes.Backfilled); n > 0 {
		parts = append(parts, fmt.Sprintf("%d backfilled", n))
	}
	if n := len(passes.Contradictions); n > 0 {
		parts = append(parts, fmt.Sprintf("%d contradiction(s) flagged", n))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, ", ")
}
