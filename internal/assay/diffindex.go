package assay

import (
	"strconv"
	"strings"
)

// diffIndex maps a new-file path to the set of new-file line numbers that appear
// in the PR's unified diff hunks (added or context lines on the RIGHT side).
// GitHub only accepts an inline review comment whose (path, line) is one of
// these positions; anchoring anywhere else 422s with
// "pull_request_review_thread.path/line could not be resolved".
type diffIndex map[string]map[int]bool

// buildDiffIndex parses a unified diff (as produced by `git diff`) into a
// diffIndex. It walks each file's hunks, tracking the new-file line counter from
// the "@@ -a,b +c,d @@" headers, and records every context (' ') and added ('+')
// line — the positions GitHub will accept for a RIGHT-side inline comment.
// Deleted ('-') lines do not advance the new-file counter. A "+++ /dev/null"
// target (file deleted) contributes nothing. An empty or unparseable diff yields
// an empty index, which callers treat as "diff unavailable" (skip filtering).
func buildDiffIndex(diff string) diffIndex {
	idx := diffIndex{}
	var path string
	var newLine int
	inHunk := false

	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			path = ""
			inHunk = false
		case strings.HasPrefix(line, "+++ "):
			path = parseDiffTargetPath(strings.TrimPrefix(line, "+++ "))
			inHunk = false
		case strings.HasPrefix(line, "--- "):
			// old-file header; ignored (we anchor on the new side).
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			if n, ok := parseHunkNewStart(line); ok {
				newLine = n
				inHunk = path != ""
			} else {
				inHunk = false
			}
		case inHunk:
			if line == "" {
				// A blank line inside a hunk is a zero-length context line.
				if idx[path] == nil {
					idx[path] = map[int]bool{}
				}
				idx[path][newLine] = true
				newLine++
				continue
			}
			switch line[0] {
			case '+':
				if idx[path] == nil {
					idx[path] = map[int]bool{}
				}
				idx[path][newLine] = true
				newLine++
			case ' ':
				if idx[path] == nil {
					idx[path] = map[int]bool{}
				}
				idx[path][newLine] = true
				newLine++
			case '-':
				// deletion: left side only, does not advance the new-file line.
			case '\\':
				// "\ No newline at end of file": not a real line.
			default:
				// Anything else ends the hunk body.
				inHunk = false
			}
		}
	}
	return idx
}

// parseDiffTargetPath extracts the file path from a "+++ " target spec, stripping
// the conventional "b/" prefix. Returns "" for /dev/null (deleted file).
func parseDiffTargetPath(spec string) string {
	spec = strings.TrimSpace(spec)
	// Strip a trailing tab-delimited timestamp if present.
	if i := strings.IndexByte(spec, '\t'); i >= 0 {
		spec = spec[:i]
	}
	if spec == "/dev/null" {
		return ""
	}
	spec = strings.TrimPrefix(spec, "b/")
	return spec
}

// parseHunkNewStart extracts the new-file starting line from a hunk header of the
// form "@@ -a,b +c,d @@" (or "@@ -a +c @@"). Returns (c, true) on success.
func parseHunkNewStart(header string) (int, bool) {
	plus := strings.IndexByte(header, '+')
	if plus < 0 {
		return 0, false
	}
	rest := header[plus+1:]
	// rest looks like "c,d @@ ..." or "c @@ ...".
	end := strings.IndexAny(rest, ", ")
	if end >= 0 {
		rest = rest[:end]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

// anchorableInDiff reports whether finding f can be posted as an inline comment:
// its file is in the diff and its line (and start line, for a range) is a RIGHT-
// side position present in a hunk.
func anchorableInDiff(idx diffIndex, f Finding) bool {
	lines, ok := idx[f.File]
	if !ok {
		return false
	}
	start, end, ok := parseLineSpec(f.Anchor)
	if !ok {
		return false
	}
	if start > 0 && start < end {
		return lines[start] && lines[end]
	}
	return lines[end]
}
