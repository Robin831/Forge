// Package epic defines the opt-in contract for epic orchestration (the
// Crucible) and the single source of truth for an epic's branch name.
//
// Parent/child relations in bd are cheap and common: an operator files a
// parent bead and hangs work off it purely for grouping. Routing every such
// child through a shared feature branch — and every such parent through the
// Crucible — was the old default, and it was almost never what was wanted.
// Orchestration is therefore *opt-in*: a parent must carry the "crucible"
// label (or name a branch explicitly with "epic-branch:<name>") for any epic
// routing to happen. The bead's issue_type is deliberately NOT consulted:
// `issue_type: epic` alone no longer opts in.
//
// Children of a parent that has not opted in dispatch as ordinary standalone
// beads — worktree from main, PR to main — with their bd relations untouched.
package epic

import "strings"

const (
	// CrucibleLabel is the opt-in label a parent bead must carry for its
	// children to be routed through a shared branch and for the Crucible to
	// orchestrate it.
	CrucibleLabel = "crucible"

	// BranchLabelPrefix names an epic's shared branch explicitly. A label
	// "epic-branch:feature/depcheck" means the epic uses "feature/depcheck".
	// Carrying it is itself an opt-in: it names a branch on purpose.
	BranchLabelPrefix = "epic-branch:"

	// DefaultBranchPrefix is the prefix of the derived branch name used when a
	// parent opts in without naming a branch. The derived name is
	// "feature/<parent-id>" — the same name the Crucible creates, which is what
	// keeps an independently dispatched child from basing on a branch that
	// never exists.
	DefaultBranchPrefix = "feature/"
)

// IsOrchestrated reports whether a bead's labels opt it into epic
// orchestration. Only the "crucible" label or an "epic-branch:<name>" label
// qualify; issue type is not consulted.
func IsOrchestrated(labels []string) bool {
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), CrucibleLabel) {
			return true
		}
		if hasBranchLabel(label) {
			return true
		}
	}
	return false
}

// ExplicitBranch returns the branch named by an "epic-branch:<name>" label, or
// "" when no such label carries a non-empty name.
func ExplicitBranch(labels []string) string {
	for _, label := range labels {
		if !hasBranchLabel(label) {
			continue
		}
		if branch := strings.TrimSpace(label[len(BranchLabelPrefix):]); branch != "" {
			return branch
		}
	}
	return ""
}

// BranchName returns the shared branch name for a parent bead: the branch named
// by an "epic-branch:<name>" label when present, otherwise the derived
// "feature/<id>". It is the single source of truth for that name — the poller
// (which stamps children with it) and the Crucible (which creates it) must
// agree, or independently dispatched children fail with "base branch not found
// on origin".
//
// BranchName derives a name for any bead; callers gate on IsOrchestrated first.
func BranchName(id string, labels []string) string {
	if branch := ExplicitBranch(labels); branch != "" {
		return branch
	}
	return DefaultBranchPrefix + SanitizeID(id)
}

// hasBranchLabel reports whether a label carries the epic-branch prefix
// (case-insensitively).
func hasBranchLabel(label string) bool {
	return len(label) >= len(BranchLabelPrefix) &&
		strings.EqualFold(label[:len(BranchLabelPrefix)], BranchLabelPrefix)
}

// SanitizeID converts a bead ID to a safe branch name component. Slashes are
// replaced so the result does not create unexpected path segments when used as
// "feature/<id>" (matching worktree.sanitizePath behaviour).
func SanitizeID(id string) string {
	r := strings.NewReplacer(
		" ", "-",
		":", "-",
		"\\", "-",
		"/", "-",
	)
	return r.Replace(id)
}
