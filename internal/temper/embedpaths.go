package temper

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// embedDirective is the comment that binds a file tree into a Go binary.
const embedDirective = "//go:embed"

// maxEmbedScanFileSize bounds how much of a .go file is read while scanning
// for embed directives. A source file larger than this is generated code (a
// vendored blob, a huge table) that in practice does not carry an embed, and
// reading it whole would make the scan proportional to the repo's biggest
// artifacts rather than to its source.
//
// Exceeding it ABORTS the scan rather than skipping the file, because the cap
// is about the scan's cost and says nothing about the file's contents: a
// skipped .go file may carry the one embed that had to reach the build step,
// and a list missing it is indistinguishable from a complete one. Aborting
// leaves the Go steps ungated, which only costs a full run.
const maxEmbedScanFileSize = 4 << 20 // 4 MiB

// errEmbedScanFileTooLarge is returned for a .go file past maxEmbedScanFileSize.
var errEmbedScanFileTooLarge = errors.New("file exceeds the embed scan size cap")

// embedScanSkipDirs are directories never descended into while scanning for
// embed directives: they hold no first-party Go source whose embeds matter
// (vendor's do not — `**/*.go` already gates on the vendored tree wholesale),
// and walking them is where the scan's cost would otherwise go.
var embedScanSkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".previews":    true,
}

// goStepPaths returns the `paths` globs every auto-detected Go step is gated
// on for this worktree: the static defaultGoPaths plus the embed targets
// discovered by scanning the tree.
//
// Fails OPEN, like everything else on this path: a scan that errors returns
// nil, and a nil `paths` on a step means it runs unconditionally. Gating on a
// set we could not finish computing would be exactly the miss the whole
// scheme exists to avoid — `go build` compiles what a package embeds, so a
// diff touching only an embedded asset must still reach the build step.
func goStepPaths(worktreePath string) []string {
	embeds, err := goEmbedPaths(worktreePath)
	if err != nil {
		log.Printf("[temper] WARN could not scan %s for //go:embed directives (%v) — leaving Go steps ungated", worktreePath, err)
		return nil
	}
	paths := make([]string, 0, len(defaultGoPaths)+len(embeds))
	paths = append(paths, defaultGoPaths...)
	return append(paths, embeds...)
}

// goEmbedPaths scans worktreePath for `//go:embed` directives and returns the
// glob patterns covering what they name, relative to the worktree root.
//
// Embed targets are the one build input no extension list can predict: they
// are ordinary data files (prompts, templates, a built web bundle) that the
// compiler reads as surely as it reads a .go file, so a diff touching only
// them has to run the build. Rather than guess at extensions, this reads the
// directives themselves — the same source of truth the compiler uses.
//
// Each pattern yields two globs: the pattern itself, and the pattern plus
// `/**` because `//go:embed dist` embeds the whole subtree beneath dist. Both
// only ever widen the gate.
func goEmbedPaths(worktreePath string) ([]string, error) {
	if worktreePath == "" {
		return nil, nil
	}
	seen := make(map[string]bool)
	var out []string
	add := func(glob string) {
		if glob == "" || seen[glob] {
			return
		}
		seen[glob] = true
		out = append(out, glob)
	}

	err := filepath.WalkDir(worktreePath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Any unreadable path aborts the scan, which makes goStepPaths
			// fail open and leave the Go steps ungated. A PARTIAL embed list
			// would be the dangerous answer: it reads as a complete one, and
			// the embeds it missed are the ones that would skip the build.
			// The same rule covers every later `return rerr` below — an
			// oversized or unreadable .go file ends the scan rather than
			// being quietly dropped from the list. Its one deliberate
			// exception is non-regular files, justified where they are
			// skipped.
			return err
		}
		if d.IsDir() {
			if p != worktreePath && embedScanSkipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		if !d.Type().IsRegular() {
			// Symlinks, FIFOs and device nodes are never opened. WalkDir
			// reports entries by Lstat, so for a committed `x.go -> /dev/zero`
			// the size guard sees the link's own few bytes and a following
			// read grows without bound until the daemon is OOM-killed; a link
			// to /dev/stdin or a FIFO blocks this goroutine forever (nothing
			// inside the walk consults a context); and any link out of the
			// worktree reads a host file the scan has no business opening.
			// The trees reaching here are not all Forge's own — quench and
			// burnish verify contributor branches behind ext-* PRs.
			//
			// This is the one accepted hole in the completeness rule above: a
			// symlinked .go file's directives go unread. `//go:embed` itself
			// refuses irregular files, and a link whose target is inside the
			// worktree is walked at that target's own path anyway.
			return nil
		}
		data, rerr := readForEmbedScan(p)
		if rerr != nil {
			return rerr
		}
		if !bytes.Contains(data, []byte(embedDirective)) {
			return nil
		}
		rel, rerr := filepath.Rel(worktreePath, filepath.Dir(p))
		if rerr != nil {
			return nil
		}
		pkgDir := filepath.ToSlash(rel)
		if pkgDir == "." {
			pkgDir = ""
		}
		for _, pattern := range parseEmbedPatterns(string(data)) {
			glob := pattern
			if pkgDir != "" {
				glob = path.Join(pkgDir, pattern)
			}
			add(glob)
			add(glob + "/**")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// readForEmbedScan reads p for directive scanning, refusing anything that is
// not a regular file and bounding the read at maxEmbedScanFileSize.
//
// Both checks are made against the open descriptor rather than the walk's
// Lstat entry: that entry describes the path as it was a moment ago, and a
// path replaced in between (or one that lies about its length, as every /dev
// character device does) must not be able to make this read without limit.
// The LimitReader is what enforces the cap even when the reported size does
// not — the Stat check only lets an oversized ordinary file fail by name.
func readForEmbedScan(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file (%s)", p, info.Mode().Type())
	}
	if info.Size() > maxEmbedScanFileSize {
		return nil, fmt.Errorf("%s (%d bytes): %w", p, info.Size(), errEmbedScanFileTooLarge)
	}

	data, err := io.ReadAll(io.LimitReader(f, maxEmbedScanFileSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxEmbedScanFileSize {
		return nil, fmt.Errorf("%s: %w", p, errEmbedScanFileTooLarge)
	}
	return data, nil
}

// parseEmbedPatterns extracts the patterns named by every `//go:embed`
// directive in a Go source file. Patterns are space-separated and may be
// quoted (Go allows both interpreted and raw string literals), and may carry
// the `all:` prefix that includes files a plain pattern would skip — the
// prefix is stripped, since it changes what the compiler includes, not where
// it looks.
func parseEmbedPatterns(src string) []string {
	var patterns []string
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if !strings.HasPrefix(line, embedDirective) {
			continue
		}
		rest := strings.TrimPrefix(line, embedDirective)
		// `//go:embedded` and friends are not the directive.
		if rest != "" && !isEmbedSeparator(rest[0]) {
			continue
		}
		for _, field := range splitEmbedFields(rest) {
			field = strings.TrimPrefix(field, "all:")
			if field == "" || strings.HasPrefix(field, "/") || field == "." {
				continue
			}
			patterns = append(patterns, path.Clean(field))
		}
	}
	return patterns
}

func isEmbedSeparator(c byte) bool { return c == ' ' || c == '\t' }

// splitEmbedFields splits a directive's argument list on whitespace, keeping
// quoted patterns (which may contain spaces) whole and unquoting them.
func splitEmbedFields(s string) []string {
	var fields []string
	for i := 0; i < len(s); {
		c := s[i]
		if isEmbedSeparator(c) {
			i++
			continue
		}
		if c == '"' || c == '`' {
			closing := byte('"')
			if c == '`' {
				closing = '`'
			}
			end := strings.IndexByte(s[i+1:], closing)
			if end < 0 {
				// Unterminated literal — the file will not compile; take the
				// rest verbatim rather than dropping the pattern.
				fields = append(fields, s[i+1:])
				break
			}
			raw := s[i : i+1+end+1]
			if unquoted, err := strconv.Unquote(raw); err == nil {
				fields = append(fields, unquoted)
			} else {
				fields = append(fields, s[i+1:i+1+end])
			}
			i += end + 2
			continue
		}
		end := i
		for end < len(s) && !isEmbedSeparator(s[end]) {
			end++
		}
		fields = append(fields, s[i:end])
		i = end
	}
	return fields
}
