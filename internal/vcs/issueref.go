package vcs

import (
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
)

// IssueRef identifies a single GitHub issue referenced by a bead's
// external_ref (or by a "Source:" line in a bead description). Number is
// always set for a valid ref; Owner/Repo are set only when the ref carried
// them (a full issue URL), and are what makes the cross-repo qualified form
// possible.
type IssueRef struct {
	Owner  string
	Repo   string
	Number string
}

// IsZero reports whether the ref identifies no issue at all.
func (r IssueRef) IsZero() bool { return r.Number == "" }

// Qualified reports whether the ref knows which repository its issue lives in.
func (r IssueRef) Qualified() bool { return r.Owner != "" && r.Repo != "" }

// String renders the reference in the form GitHub's closing-keyword parser
// accepts from any repository: "owner/repo#N" when the repository is known,
// else "#N". The qualified form is emitted unconditionally — it is valid in
// the issue's own repository too, and it is the only form that works from a
// PR in a different repository (a bare "#N" there silently resolves against
// the wrong repo and closes nothing).
func (r IssueRef) String() string {
	if r.IsZero() {
		return ""
	}
	if r.Qualified() {
		return r.Owner + "/" + r.Repo + "#" + r.Number
	}
	return "#" + r.Number
}

// SameIssue reports whether two refs name the same issue. A ref that does not
// know its repository matches on number only when the other side doesn't know
// its repository either — a bare "#N" seen in a PR body cannot be proven to be
// the qualified issue "owner/repo#N", because the PR may live in a different
// repository where "#N" is someone else's issue.
func (r IssueRef) SameIssue(o IssueRef) bool {
	if r.IsZero() || o.IsZero() || r.Number != o.Number {
		return false
	}
	if r.Qualified() != o.Qualified() {
		return false
	}
	if !r.Qualified() {
		return true
	}
	return strings.EqualFold(r.Owner, o.Owner) && strings.EqualFold(r.Repo, o.Repo)
}

// ghIssueURLPathRe matches the path portion of a GitHub issue URL like
// /org/repo/issues/42, capturing owner, repo and number.
var ghIssueURLPathRe = regexp.MustCompile(`^/([^/]+)/([^/]+)/issues/(\d+)$`)

// ParseIssueRef extracts a GitHub issue reference from an external_ref value.
// Recognised formats:
//   - "gh-42"                                  → #42 (repository unknown)
//   - "https://github.com/org/repo/issues/42"  → org/repo#42
//
// Returns the zero IssueRef for non-GitHub references (e.g. "jira-123",
// GitLab URLs), empty strings, or malformed values.
func ParseIssueRef(externalRef string) IssueRef {
	if externalRef == "" {
		return IssueRef{}
	}
	// Shorthand: gh-<number>
	if num, ok := strings.CutPrefix(externalRef, "gh-"); ok && num != "" {
		for _, c := range num {
			if c < '0' || c > '9' {
				return IssueRef{}
			}
		}
		return IssueRef{Number: num}
	}
	// Full URL: must be a github.com URL with path /org/repo/issues/<number>.
	u, err := url.Parse(externalRef)
	if err != nil || u.Hostname() != "github.com" {
		return IssueRef{}
	}
	if m := ghIssueURLPathRe.FindStringSubmatch(u.Path); len(m) == 4 {
		return IssueRef{Owner: m[1], Repo: m[2], Number: m[3]}
	}
	return IssueRef{}
}

// closingRefRe matches a GitHub closing-keyword issue reference in any of the
// target forms the closing-keyword parser accepts: "#N", "owner/repo#N", or a
// full issue URL. Group 1 is the keyword, group 2 the target.
var closingRefRe = regexp.MustCompile(
	`(?i)\b(close[sd]?|fix(?:e[sd])?|resolve[sd]?)(?::\s*|\s+)` +
		`((?:[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?#\d+|https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/issues/\d+)`)

// refTargetToIssueRef parses the target half of a closingRefRe match.
func refTargetToIssueRef(target string) IssueRef {
	if strings.HasPrefix(target, "http") {
		return ParseIssueRef(target)
	}
	slug, num, ok := strings.Cut(target, "#")
	if !ok {
		return IssueRef{}
	}
	if slug == "" {
		return IssueRef{Number: num}
	}
	owner, repo, ok := strings.Cut(slug, "/")
	if !ok {
		return IssueRef{}
	}
	return IssueRef{Owner: owner, Repo: repo, Number: num}
}

// sourceLineRe matches the "Source: <github issue url>" line that wicket
// appends to the description of every bead it creates from a triaged GitHub
// issue. That issue is a user report (e.g. labelled `innmeldt`): it must be
// referenced by the PR but never auto-closed — it stays open until the
// reporter verifies the fix.
var sourceLineRe = regexp.MustCompile(`(?im)^source:\s*(https://github\.com/\S+/issues/\d+)\s*$`)

// ParseSourceRefs extracts the issue refs named by "Source:" lines in a bead
// description. Duplicates are folded.
func ParseSourceRefs(description string) []IssueRef {
	var refs []IssueRef
	for _, m := range sourceLineRe.FindAllStringSubmatch(description, -1) {
		r := ParseIssueRef(m[1])
		if r.IsZero() {
			continue
		}
		dup := false
		for _, seen := range refs {
			if seen.SameIssue(r) {
				dup = true
				break
			}
		}
		if !dup {
			refs = append(refs, r)
		}
	}
	return refs
}

// noCloseLabels holds the issue labels that mark an issue as
// must-not-auto-close (a user report that stays open until the reporter
// verifies). Configurable via settings.github_no_close_labels; the default
// honours the `innmeldt` convention.
var noCloseLabels atomic.Value

// DefaultNoCloseLabels is the built-in no-close label set.
var DefaultNoCloseLabels = []string{"innmeldt"}

// SetNoCloseLabels overrides the no-close label set. The daemon calls this at
// startup (and on config hot-reload) from settings.github_no_close_labels.
// Passing nil or an empty slice restores the default.
func SetNoCloseLabels(labels []string) {
	cp := make([]string, len(labels))
	copy(cp, labels)
	noCloseLabels.Store(cp)
}

// NoCloseLabels returns the active no-close label set.
func NoCloseLabels() []string {
	if v := noCloseLabels.Load(); v != nil {
		if s, ok := v.([]string); ok && len(s) > 0 {
			return s
		}
	}
	return DefaultNoCloseLabels
}

// EnsureIssueReferences rewrites body so that its issue references are exactly
// the ones derived from the worked bead:
//
//  1. Every existing closing-keyword reference (Closes/Fixes/Resolves + issue)
//     that does not provably name closeRef's issue is demoted to a plain
//     "Refs" reference. Model- or human-authored text must never decide which
//     issue a Forge PR closes — a wrong "Closes #N" silently closes a
//     sibling's issue, and a stale one suppressed the correct injection.
//  2. A "Closes <closeRef>" line is appended (or "Refs <closeRef>" when
//     refsOnly is true — the external_ref issue carries a no-close label such
//     as `innmeldt`, so it must stay open for reporter verification).
//  3. A "Refs <r>" line is appended for every sourceRef not already
//     referenced — the user reports a wicket bead was triaged from.
//
// The appended block ends without a trailing newline; callers add their own
// separators. A zero closeRef appends no Closes line but demotion and source
// refs still apply.
func EnsureIssueReferences(body string, closeRef IssueRef, refsOnly bool, sourceRefs []IssueRef) string {
	// Demote closing references to other issues. When refsOnly is set, the
	// external_ref issue itself must not be closed either, so every closing
	// reference is demoted.
	body = closingRefRe.ReplaceAllStringFunc(body, func(match string) string {
		sub := closingRefRe.FindStringSubmatch(match)
		target := refTargetToIssueRef(sub[2])
		if !refsOnly && target.SameIssue(closeRef) {
			return match
		}
		return "Refs " + sub[2]
	})

	var lines []string

	if !closeRef.IsZero() {
		keyword := "Closes"
		if refsOnly {
			keyword = "Refs"
		}
		want := keyword + " " + closeRef.String()
		if !strings.Contains(body, want) {
			lines = append(lines, want)
		}
	}

	for _, r := range sourceRefs {
		if r.SameIssue(closeRef) {
			continue
		}
		want := "Refs " + r.String()
		if !strings.Contains(body, want) && !strings.Contains(strings.Join(lines, "\n"), want) {
			lines = append(lines, want)
		}
	}

	if len(lines) == 0 {
		return body
	}
	block := strings.Join(lines, "\n")
	if body == "" {
		return block
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + "\n" + block
}

// AppendIssueReferences is EnsureIssueReferences plus a trailing blank line,
// for body builders that write a footer after the reference block.
func AppendIssueReferences(body string, closeRef IssueRef, refsOnly bool, sourceRefs []IssueRef) string {
	out := EnsureIssueReferences(body, closeRef, refsOnly, sourceRefs)
	if out != "" && !strings.HasSuffix(out, "\n\n") {
		if strings.HasSuffix(out, "\n") {
			out += "\n"
		} else {
			out += "\n\n"
		}
	}
	return out
}
