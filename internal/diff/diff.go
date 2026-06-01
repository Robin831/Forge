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
