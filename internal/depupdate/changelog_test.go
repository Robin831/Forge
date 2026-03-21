package depupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/depcheck"
)

func TestBuildFragmentContent_Monolingual(t *testing.T) {
	groups := []UpdateGroup{
		{
			Name: "vite ecosystem",
			Kind: "minor",
			Updates: []depcheck.ModuleUpdate{
				{Path: "vite", Current: "4.0.0", Latest: "5.0.0", Kind: "major"},
				{Path: "vite-plugin-foo", Current: "1.2.0", Latest: "1.3.0", Kind: "minor"},
			},
		},
	}

	got := buildFragmentContent(groups)

	if !strings.HasPrefix(got, "category: Changed\n") {
		t.Errorf("expected 'category: Changed' header, got: %q", got)
	}
	if !strings.Contains(got, "`vite`: 4.0.0 → 5.0.0") {
		t.Errorf("expected vite entry, got: %q", got)
	}
	if !strings.Contains(got, "`vite-plugin-foo`: 1.2.0 → 1.3.0") {
		t.Errorf("expected vite-plugin-foo entry, got: %q", got)
	}
}

func TestBuildFragmentContent_MultipleGroups(t *testing.T) {
	groups := []UpdateGroup{
		{
			Name: "react ecosystem",
			Kind: "minor",
			Updates: []depcheck.ModuleUpdate{
				{Path: "react", Current: "18.0.0", Latest: "18.2.0", Kind: "minor"},
			},
		},
		{
			Name: "lodash",
			Kind: "patch",
			Updates: []depcheck.ModuleUpdate{
				{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"},
			},
		},
	}

	got := buildFragmentContent(groups)

	if strings.Count(got, "\n- ") != 2 {
		t.Errorf("expected 2 bullet lines, got: %q", got)
	}
}

func TestBuildFragmentContent_Empty(t *testing.T) {
	got := buildFragmentContent(nil)
	if got != "category: Changed\n" {
		t.Errorf("expected only header for empty groups, got: %q", got)
	}
}

func TestDetectBilingual_NoDirectory(t *testing.T) {
	dir := t.TempDir()
	if DetectBilingual(dir) {
		t.Error("expected false for directory with no changelog.d/")
	}
}

func TestDetectBilingual_MonolingualFragments(t *testing.T) {
	dir := t.TempDir()
	clDir := filepath.Join(dir, "changelog.d")
	if err := os.MkdirAll(clDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clDir, "some-bead.md"), []byte("category: Added\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if DetectBilingual(dir) {
		t.Error("expected false for directory with only .md fragments")
	}
}

func TestDetectBilingual_BilingualFragments(t *testing.T) {
	dir := t.TempDir()
	clDir := filepath.Join(dir, "changelog.d")
	if err := os.MkdirAll(clDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clDir, "some-bead.en.md"), []byte("category: Added\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !DetectBilingual(dir) {
		t.Error("expected true when .en.md fragment exists")
	}
}

func TestGenerateChangelog_EmptyGroups(t *testing.T) {
	dir := t.TempDir()
	// Empty groups should be a no-op with no error.
	if err := GenerateChangelog(dir, nil, false); err != nil {
		t.Errorf("expected nil error for empty groups, got: %v", err)
	}
}

// initGitRepo sets up a minimal git repo in dir so git add/commit can run.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
}

func TestGenerateChangelog_Monolingual(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	groups := []UpdateGroup{
		{
			Name: "lodash",
			Kind: "patch",
			Updates: []depcheck.ModuleUpdate{
				{Path: "lodash", Current: "4.17.20", Latest: "4.17.21", Kind: "patch"},
			},
		},
	}

	if err := GenerateChangelog(dir, groups, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify exactly one .md file was created (no .en.md/.nb.md).
	clDir := filepath.Join(dir, "changelog.d")
	entries, err := os.ReadDir(clDir)
	if err != nil {
		t.Fatalf("reading changelog.d: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	name := entries[0].Name()
	if strings.HasSuffix(name, ".en.md") || strings.HasSuffix(name, ".nb.md") {
		t.Errorf("expected monolingual .md file, got %q", name)
	}
	if !strings.HasSuffix(name, ".md") {
		t.Errorf("expected .md suffix, got %q", name)
	}

	content, err := os.ReadFile(filepath.Join(clDir, name))
	if err != nil {
		t.Fatalf("reading fragment: %v", err)
	}
	if !strings.HasPrefix(string(content), "category: Changed\n") {
		t.Errorf("missing category header in %q", string(content))
	}
	if !strings.Contains(string(content), "`lodash`: 4.17.20 → 4.17.21") {
		t.Errorf("expected lodash entry in %q", string(content))
	}
}

func TestGenerateChangelog_Bilingual(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)

	groups := []UpdateGroup{
		{
			Name: "react",
			Kind: "minor",
			Updates: []depcheck.ModuleUpdate{
				{Path: "react", Current: "18.0.0", Latest: "18.2.0", Kind: "minor"},
			},
		},
	}

	if err := GenerateChangelog(dir, groups, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	clDir := filepath.Join(dir, "changelog.d")
	entries, err := os.ReadDir(clDir)
	if err != nil {
		t.Fatalf("reading changelog.d: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 files (en+nb), got %d", len(entries))
	}

	var hasEN, hasNB bool
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".en.md") {
			hasEN = true
		}
		if strings.HasSuffix(e.Name(), ".nb.md") {
			hasNB = true
		}
	}
	if !hasEN {
		t.Error("expected .en.md file to be created")
	}
	if !hasNB {
		t.Error("expected .nb.md file to be created")
	}
}

func TestWriteFragment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.md")
	content := "category: Changed\n- `pkg`: 1.0.0 → 2.0.0\n"

	if err := writeFragment(path, content); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}
	if string(got) != content {
		t.Errorf("file content mismatch: got %q, want %q", string(got), content)
	}
}
