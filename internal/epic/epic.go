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

	// maxBranchNameLen bounds a label-supplied branch name. The branch becomes a
	// directory component of a worktree path, so an unbounded name is a path
	// length failure on some filesystems rather than a git one.
	maxBranchNameLen = 200
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
// "" when no such label carries a *usable* name.
//
// The value is used verbatim as a git branch: it is stamped onto children as
// their PR base, passed to `git worktree add`, and folded into worktree paths.
// A name git would reject — or would read as a flag — therefore never leaves
// this function: ValidBranchName screens it, and a label naming an unusable
// branch falls back to the derived "feature/<id>" (the label still counts as an
// opt-in; it named a branch on purpose, it just did not name a legal one).
func ExplicitBranch(labels []string) string {
	for _, label := range labels {
		branch, ok := branchLabelValue(label)
		if !ok || branch == "" {
			continue
		}
		if !ValidBranchName(branch) {
			continue
		}
		return branch
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

// branchLabelValue reports whether a label carries the epic-branch prefix
// (case-insensitively, ignoring surrounding whitespace) and returns the name it
// carries. Both opt-in forms are normalised the same way — the "crucible" check
// trims too — so " epic-branch:foo" is not silently ignored while " crucible"
// opts in.
func branchLabelValue(label string) (string, bool) {
	label = strings.TrimSpace(label)
	if len(label) < len(BranchLabelPrefix) ||
		!strings.EqualFold(label[:len(BranchLabelPrefix)], BranchLabelPrefix) {
		return "", false
	}
	return strings.TrimSpace(label[len(BranchLabelPrefix):]), true
}

// hasBranchLabel reports whether a label carries the epic-branch prefix. It
// says nothing about the name being usable — carrying the prefix is the opt-in.
func hasBranchLabel(label string) bool {
	_, ok := branchLabelValue(label)
	return ok
}

// ValidBranchName reports whether name is safe to hand to git as a branch name.
// It is a conservative subset of `git check-ref-format --branch`: the rules that
// matter here are the ones that turn a semi-trusted label value into something
// other than a branch — a leading "-" that a git invocation would read as a
// flag, and ".." or path traversal that escapes the worktree directory the name
// is folded into.
func ValidBranchName(name string) bool {
	if name == "" || len(name) > maxBranchNameLen {
		return false
	}
	// A leading "-" is read as a flag by any git call that passes the branch
	// as a positional argument without a "--" separator.
	if strings.HasPrefix(name, "-") {
		return false
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return false
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock") {
		return false
	}
	for _, bad := range []string{"..", "@{", "\\", "~", "^", ":", "?", "*", "[", " "} {
		if strings.Contains(name, bad) {
			return false
		}
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	// No path component may start with "." or end in ".lock" ("refs/heads/" +
	// name is split on "/" by git).
	for _, part := range strings.Split(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
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
