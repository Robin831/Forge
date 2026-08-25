// Package diff provides shared primitives for parsing and shaping unified git
// diffs before they are embedded in an AI review prompt. The helpers here are
// consumed by both the Warden (code review) and Assay stages so the diff size
// cap, auto-generated file filtering, and changed-file extraction behave
// identically across stages.
package diff

import (
	"bufio"
	"fmt"
	"strings"
)

// MaxBytes limits the diff size passed to a review prompt. Sized to cover real
// EF Core migration diffs (~138KB observed) with headroom for larger schemas
// while still keeping the review prompt comfortably within a single-turn
// budget. Auto-generated files are filtered out before this cap is applied, so
// most diffs stay well under the limit.
const MaxBytes = 250000

// Truncate limits the diff size to avoid token overflow.
func Truncate(diff string, maxLen int) string {
	if len(diff) <= maxLen {
		return diff
	}
	return diff[:maxLen] + "\n\n... (diff truncated, " + fmt.Sprintf("%d", len(diff)-maxLen) + " bytes omitted)"
}

// KeepFiles returns only the blocks of unifiedDiff whose b-side path is in
// files. Unlike Assay's triage scoping (which falls back to the full diff when
// scoping would drop everything), an empty result here IS the answer: it is
// how an incremental review discovers that nothing in the delta survives into
// the net PR diff (upstream merge churn, reverted changes) and that there is
// nothing to review. A diff with no parseable block headers comes back empty
// for the same reason.
func KeepFiles(unifiedDiff string, files []string) string {
	if strings.TrimSpace(unifiedDiff) == "" || len(files) == 0 {
		return ""
	}
	keep := make(map[string]bool, len(files))
	for _, f := range files {
		keep[strings.TrimSpace(f)] = true
	}

	const marker = "diff --git "
	const sep = "\ndiff --git "

	remaining := unifiedDiff
	if !strings.HasPrefix(remaining, marker) {
		idx := strings.Index(remaining, sep)
		if idx == -1 {
			return ""
		}
		remaining = remaining[idx+1:]
	}

	var kept strings.Builder
	for len(remaining) > 0 {
		nextIdx := strings.Index(remaining[len(marker):], sep)
		var block string
		if nextIdx == -1 {
			block = remaining
			remaining = ""
		} else {
			end := len(marker) + nextIdx + 1
			block = remaining[:end]
			remaining = remaining[end:]
		}
		headerLine := block
		if nl := strings.IndexByte(block, '\n'); nl != -1 {
			headerLine = block[:nl]
		}
		if path := ParseGitPath(headerLine); path != "" && keep[path] {
			kept.WriteString(block)
		}
	}
	return kept.String()
}

// ParseGitPath extracts the b-side file path from a git diff header line of the
// form "diff --git a/<path> b/<path>". Returns "" if the header cannot be
// parsed. The b-side path is preferred because it reflects the file's
// post-change name (renames show the new path).
func ParseGitPath(header string) string {
	const prefix = "diff --git "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	rest := header[len(prefix):]
	idx := strings.LastIndex(rest, " b/")
	if idx == -1 {
		return ""
	}
	return rest[idx+len(" b/"):]
}

// maxPromptPathLen bounds one rendered path. The elided-file note is a
// sentence beside a diff, not a manifest: a path longer than this teaches a
// reader nothing further and is exactly the shape a payload takes.
const maxPromptPathLen = 120

// SafePath renders a path taken off a diff header as an inert label for a
// prompt or a PR comment.
//
// ParseGitPath returns everything after the last " b/" in a header, unvalidated
// — so every path this package hands back is a string the author of the pull
// request under review chose, byte for byte. Both consumers of the elided list
// (Assay's prompt head, the Warden's diff note) then name those paths inside a
// sentence Forge wrote itself, which is the one place in either prompt where an
// attacker's bytes can be read as Forge's own words rather than as data under
// review. Neutralising the fence is not enough there: the injection does not
// need to break out of anything, only to be read as prose.
//
// So the alphabet is closed rather than the dangerous characters blocked:
// letters, digits, '.', '_', '-' and '/' survive, and every run of anything
// else — spaces, punctuation, backticks, newlines, control bytes, the whole of
// non-ASCII — collapses to a single "?". A sentence needs spaces; what comes
// out of this cannot form one. The "?" is deliberate rather than a silent drop:
// a name that was scrubbed should read as scrubbed, not as a real filename.
func SafePath(path string) string {
	var b strings.Builder
	dropped := false
	for _, r := range path {
		if !safePathRune(r) {
			dropped = true
			continue
		}
		if dropped {
			b.WriteByte('?')
			dropped = false
		}
		b.WriteRune(r)
	}
	if dropped {
		b.WriteByte('?')
	}
	// Every rune kept above is one ASCII byte, so this cut cannot split one.
	out := b.String()
	if len(out) > maxPromptPathLen {
		out = out[:maxPromptPathLen] + "..."
	}
	if out == "" {
		return "?"
	}
	return out
}

func safePathRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '.', r == '_', r == '-', r == '/':
		return true
	}
	return false
}

// ChangedFiles extracts the b-side file paths from a unified diff. Returns nil
// when the diff has no "diff --git" headers. ParseGitPath is reused so renames
// and a/ b/ paths with spaces behave the same here as in the diff-filter
// pre-truncation pass.
//
// Uses bufio.Scanner to avoid allocating a full slice of every line in a
// potentially large diff.
func ChangedFiles(diff string) []string {
	if diff == "" {
		return nil
	}
	const marker = "diff --git "
	var out []string
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(strings.NewReader(diff))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, marker) {
			continue
		}
		p := ParseGitPath(line)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	if err := scanner.Err(); err != nil {
		return out
	}
	return out
}
