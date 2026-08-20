package temper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeFile writes content at rel under dir, creating parents.
func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
}

// TestGoEmbedPaths_CoversEmbeddedAssets is the gap the static extension list
// cannot close: `go build` compiles what a package embeds, so a diff touching
// only an embedded asset has to reach the build step. Forge is itself the
// case — internal/assay embeds prompts/*.md — and a prompt-only PR used to
// produce an all-skipped PASS.
func TestGoEmbedPaths_CoversEmbeddedAssets(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module test\n")
	writeFile(t, dir, "internal/assay/passes.go", "package assay\n\nimport _ \"embed\"\n\n//go:embed prompts/*.md\nvar prompts embed.FS\n")
	writeFile(t, dir, "internal/assay/prompts/logic.md", "logic\n")
	writeFile(t, dir, "internal/web/embed.go", "package web\n\n//go:embed dist\nvar assets embed.FS\n")

	globs, err := goEmbedPaths(dir)
	require.NoError(t, err)
	assert.Contains(t, globs, "internal/assay/prompts/*.md")
	assert.Contains(t, globs, "internal/web/dist")
	assert.Contains(t, globs, "internal/web/dist/**")

	paths := goStepPaths(dir)
	assert.True(t, matchesChangedFiles(paths, []string{"internal/assay/prompts/logic.md"}),
		"an embedded prompt is a build input and must match the Go step globs")
	assert.True(t, matchesChangedFiles(paths, []string{"internal/web/dist/assets/index.js"}),
		"a file inside an embedded directory must match the Go step globs")
	assert.False(t, matchesChangedFiles(paths, []string{"docs/architecture.md"}),
		"a doc no package embeds must still be gated out")
}

// TestGoEmbedPaths_DirectiveForms covers the syntax the compiler accepts:
// several patterns on one directive, quoted patterns (which may contain
// spaces), and the `all:` prefix, which changes what is included rather than
// where it is looked for.
func TestGoEmbedPaths_DirectiveForms(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module test\n")
	writeFile(t, dir, "pkg/a.go", "package a\n\n//go:embed templates/*.tmpl schema.sql\nvar fs embed.FS\n")
	writeFile(t, dir, "pkg/b.go", "package a\n\n//go:embed \"data files\"\nvar spaced embed.FS\n")
	writeFile(t, dir, "pkg/c.go", "package a\n\n//go:embed all:static\nvar static embed.FS\n")
	writeFile(t, dir, "pkg/d.go", "package a\n\n// go:embed notadirective\n//go:embedded nope\nvar x int\n")

	globs, err := goEmbedPaths(dir)
	require.NoError(t, err)
	assert.Contains(t, globs, "pkg/templates/*.tmpl")
	assert.Contains(t, globs, "pkg/schema.sql")
	assert.Contains(t, globs, "pkg/data files")
	assert.Contains(t, globs, "pkg/static")
	assert.NotContains(t, globs, "pkg/notadirective")
	assert.NotContains(t, globs, "pkg/nope")
}

// TestGoEmbedPaths_RootPackage keeps a main package at the repository root
// from producing globs with a leading "./".
func TestGoEmbedPaths_RootPackage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", "package main\n\n//go:embed version.txt\nvar version string\n")

	globs, err := goEmbedPaths(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"version.txt", "version.txt/**"}, globs)
}

// TestGoEmbedPaths_SkipsVendorAndNodeModules keeps the scan proportional to
// first-party source: a vendored tree is already gated on wholesale by
// `**/*.go`, and node_modules holds no Go at all.
func TestGoEmbedPaths_SkipsVendorAndNodeModules(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "vendor/example.com/x/x.go", "package x\n\n//go:embed vendored.txt\nvar v string\n")
	writeFile(t, dir, "web/node_modules/pkg/gen.go", "package pkg\n\n//go:embed dep.txt\nvar d string\n")
	writeFile(t, dir, "app/app.go", "package app\n\n//go:embed banner.txt\nvar b string\n")

	globs, err := goEmbedPaths(dir)
	require.NoError(t, err)
	assert.Contains(t, globs, "app/banner.txt")
	for _, g := range globs {
		assert.NotContains(t, g, "vendor/")
		assert.NotContains(t, g, "node_modules/")
	}
}

// TestGoStepPaths_NoEmbedsIsTheStaticSet documents the ordinary case: a repo
// embedding nothing is gated exactly on defaultGoPaths.
func TestGoStepPaths_NoEmbedsIsTheStaticSet(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module test\n")
	writeFile(t, dir, "main.go", "package main\n\nfunc main() {}\n")

	assert.Equal(t, defaultGoPaths, goStepPaths(dir))
}

// TestGoStepPaths_MissingWorktreeFailsOpen pins the fail-open rule: a scan
// that cannot run returns nil, and a nil `paths` runs the step
// unconditionally. Gating on a set we could not finish computing is the miss
// the whole scheme exists to avoid.
func TestGoStepPaths_MissingWorktreeFailsOpen(t *testing.T) {
	assert.Nil(t, goStepPaths(filepath.Join(t.TempDir(), "does-not-exist")))
}

// TestGoEmbedPaths_SkipsSymlinkedGoFiles pins the trust boundary: the scan
// runs over trees Forge did not author (quench/burnish verify contributor
// branches behind ext-* PRs), and a committed `x.go -> /dev/zero` would read
// without bound while `-> /dev/stdin` or a FIFO would block the verification
// goroutine forever. WalkDir reports entries by Lstat, so the size guard sees
// only the link's own length — the entry type is what has to refuse it.
//
// The link here points at an ordinary in-tree file so the test asserts the
// skip itself rather than depending on a device node; the real link never
// gets far enough to be read.
func TestGoEmbedPaths_SkipsSymlinkedGoFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module test\n")
	writeFile(t, dir, "outside.txt", "package p\n\n//go:embed secret/*\nvar fs embed.FS\n")
	if err := os.Symlink(filepath.Join(dir, "outside.txt"), filepath.Join(dir, "link.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	globs, err := goEmbedPaths(dir)
	require.NoError(t, err, "a symlink is skipped, not an abort")
	assert.NotContains(t, globs, "secret/*",
		"a symlinked .go file is never opened, so its directives are not read")
}

// TestGoEmbedPaths_OversizedFileAbortsScan is the completeness rule the walk
// callback documents: a .go file the scan cannot read whole may carry the one
// embed that had to reach the build step, and a list missing it reads exactly
// like a complete one. So the cap ends the scan — goStepPaths then fails open
// and leaves the Go steps ungated, which only costs a full run.
func TestGoEmbedPaths_OversizedFileAbortsScan(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module test\n")
	writeFile(t, dir, "pkg/small.go", "package pkg\n\n//go:embed assets/*\nvar fs embed.FS\n")
	writeFile(t, dir, "pkg/huge.go", "package pkg\n"+strings.Repeat("// filler\n", (maxEmbedScanFileSize/10)+1))

	_, err := goEmbedPaths(dir)
	require.Error(t, err)
	assert.ErrorIs(t, err, errEmbedScanFileTooLarge)

	assert.Nil(t, goStepPaths(dir),
		"an aborted scan leaves the Go steps ungated rather than gated on a partial list")
}

// TestGoEmbedPaths_UnreadableFileAbortsScan is the same rule for a .go file
// that exists but cannot be opened — a permission bit, or a file that vanished
// mid-walk. Silently dropping it produced the partial list.
func TestGoEmbedPaths_UnreadableFileAbortsScan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny reads")
	}
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module test\n")
	writeFile(t, dir, "pkg/locked.go", "package pkg\n\n//go:embed assets/*\nvar fs embed.FS\n")
	locked := filepath.Join(dir, "pkg", "locked.go")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skipf("chmod unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	if f, err := os.Open(locked); err == nil {
		f.Close()
		t.Skip("filesystem does not enforce mode bits")
	}

	_, err := goEmbedPaths(dir)
	assert.Error(t, err)
	assert.Nil(t, goStepPaths(dir))
}

// TestReadForEmbedScan_RefusesIrregularAndOversized covers the descriptor-side
// checks directly: the walk's entry type describes the path as it was a moment
// ago, so the open file is re-checked before anything is read.
func TestReadForEmbedScan_RefusesIrregularAndOversized(t *testing.T) {
	dir := t.TempDir()

	small := filepath.Join(dir, "ok.go")
	require.NoError(t, os.WriteFile(small, []byte("package p\n"), 0o644))
	data, err := readForEmbedScan(small)
	require.NoError(t, err)
	assert.Equal(t, "package p\n", string(data))

	big := filepath.Join(dir, "big.go")
	require.NoError(t, os.WriteFile(big, make([]byte, maxEmbedScanFileSize+1), 0o644))
	_, err = readForEmbedScan(big)
	assert.ErrorIs(t, err, errEmbedScanFileTooLarge)

	_, err = readForEmbedScan(dir)
	assert.Error(t, err, "a directory is not a regular file")
}

func TestParseEmbedPatterns(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"none", "package p\n\nfunc f() {}\n", nil},
		{"single", "//go:embed a.txt\n", []string{"a.txt"}},
		{"multiple", "//go:embed a.txt b/*.json\n", []string{"a.txt", "b/*.json"}},
		{"tabbed", "//go:embed\ta.txt\n", []string{"a.txt"}},
		{"raw quoted", "//go:embed `raw dir`\n", []string{"raw dir"}},
		{"absolute rejected", "//go:embed /etc/passwd\n", nil},
		{"dot rejected", "//go:embed .\n", nil},
		{"indented", "\t//go:embed a.txt\n", []string{"a.txt"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseEmbedPatterns(tc.src))
		})
	}
}
