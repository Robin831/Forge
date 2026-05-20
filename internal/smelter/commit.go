package smelter

import (
	"fmt"
	"strings"

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

	// Archived holds the stale rules moved to the archive store by Pass 2
	// (ArchiveReason="stale"). Pass 1 duplicates archived with
	// ArchiveReason="duplicate" are *not* included here — they are surfaced
	// through Consolidated.
	Archived []warden.ArchivedRule

	// Backfilled lists the IDs of rules whose Paths field was populated by
	// Pass 3 from the changed files of the rule's source PR(s).
	Backfilled []string
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
// by up to four labeled sections — "Added:", "Consolidated:", "Archived:",
// "Backfilled:" — each listing the affected rule identifiers. Sections with
// no entries are omitted. The subject always ends in "[no-changelog]" so the
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
	if n := len(passes.Archived); n > 0 {
		parts = append(parts, fmt.Sprintf("archive %d stale rule(s)", n))
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

// buildCommitBody renders the four labeled sections. Sections with no
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
		cat := r.Category
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

// displayID renders an empty rule ID as "(no id)" so the bullet list does
// not contain dangling "- " entries when a rule arrives without an ID.
func displayID(id string) string {
	if id == "" {
		return "(no id)"
	}
	return id
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
	if n := len(passes.Archived); n > 0 {
		lines = append(lines, fmt.Sprintf("- %d stale rule(s) archived.", n))
	}
	if n := len(passes.Backfilled); n > 0 {
		lines = append(lines, fmt.Sprintf("- %d rule(s) with paths backfilled from PR changed files.", n))
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
	if n := len(passes.Archived); n > 0 {
		parts = append(parts, fmt.Sprintf("%d archived", n))
	}
	if n := len(passes.Backfilled); n > 0 {
		parts = append(parts, fmt.Sprintf("%d backfilled", n))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, ", ")
}
