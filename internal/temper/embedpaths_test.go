package temper

import (
	"os"
	"path/filepath"
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
