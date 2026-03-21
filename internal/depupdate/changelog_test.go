package depupdate

import (
	"os"
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
