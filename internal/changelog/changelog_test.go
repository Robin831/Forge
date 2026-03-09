package changelog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFragment(t *testing.T) {
	dir := t.TempDir()

	// Valid fragment
	path := filepath.Join(dir, "Forge-abc.md")
	os.WriteFile(path, []byte("category: Added\n\n- **New feature** - Does something cool (Forge-abc)\n"), 0644)

	frag, err := ParseFragment(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if frag.BeadID != "Forge-abc" {
		t.Errorf("BeadID = %q, want %q", frag.BeadID, "Forge-abc")
	}
	if frag.Category != "Added" {
		t.Errorf("Category = %q, want %q", frag.Category, "Added")
	}
	if len(frag.Bullets) != 1 {
		t.Fatalf("Bullets len = %d, want 1", len(frag.Bullets))
	}
}

func TestParseFragmentWithEnSuffix(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "Forge-xyz.en.md")
	os.WriteFile(path, []byte("category: Fixed\n- **Bug fix** - Fixed a thing (Forge-xyz)\n"), 0644)

	frag, err := ParseFragment(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if frag.BeadID != "Forge-xyz" {
		t.Errorf("BeadID = %q, want %q", frag.BeadID, "Forge-xyz")
	}
}

func TestParseFragmentErrors(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name    string
		content string
		errMsg  string
	}{
		{"missing category", "- bullet\n", "missing 'category:'"},
		{"invalid category", "category: Bogus\n- bullet\n", "invalid category"},
		{"no bullets", "category: Added\n\n", "no changelog entries"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name+".md")
			os.WriteFile(path, []byte(tt.content), 0644)
			_, err := ParseFragment(path)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("error %q should contain %q", err.Error(), tt.errMsg)
			}
		})
	}
}

func TestCollectAndAssemble(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "Forge-a.md"), []byte("category: Added\n- **Feature A** - new thing (Forge-a)\n"), 0644)
	os.WriteFile(filepath.Join(dir, "Forge-b.md"), []byte("category: Fixed\n- **Bug B** - fixed thing (Forge-b)\n"), 0644)
	os.WriteFile(filepath.Join(dir, "Forge-c.en.md"), []byte("category: Added\n- **Feature C** - another thing (Forge-c)\n"), 0644)

	fragments, err := CollectFragments(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fragments) != 3 {
		t.Fatalf("got %d fragments, want 3", len(fragments))
	}

	result := Assemble(fragments, "")
	if !strings.Contains(result, "## [Unreleased]") {
		t.Error("missing [Unreleased] header")
	}
	if !strings.Contains(result, "### Added") {
		t.Error("missing Added section")
	}
	if !strings.Contains(result, "### Fixed") {
		t.Error("missing Fixed section")
	}
	if strings.Contains(result, "### Changed") {
		t.Error("unexpected Changed section")
	}
}

func TestUpdateChangelogNewFile(t *testing.T) {
	dir := t.TempDir()
	clPath := filepath.Join(dir, "CHANGELOG.md")

	fragments := []Fragment{
		{BeadID: "Forge-a", Category: "Added", Bullets: []string{"- **Feature** - desc (Forge-a)"}},
	}

	content, err := UpdateChangelog(clPath, fragments, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(content, "# Changelog") {
		t.Error("missing header")
	}
	if !strings.Contains(content, "## [Unreleased]") {
		t.Error("missing [Unreleased]")
	}
	if !strings.Contains(content, "### Added") {
		t.Error("missing Added")
	}
}

func TestUpdateChangelogExisting(t *testing.T) {
	dir := t.TempDir()
	clPath := filepath.Join(dir, "CHANGELOG.md")

	existing := "# Changelog\n\n## [Unreleased]\n\n### Added\n\n- old entry\n\n## [1.0.0]\n\n### Fixed\n\n- old fix\n"
	os.WriteFile(clPath, []byte(existing), 0644)

	fragments := []Fragment{
		{BeadID: "Forge-b", Category: "Fixed", Bullets: []string{"- **New fix** - desc (Forge-b)"}},
	}

	content, err := UpdateChangelog(clPath, fragments, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(content, "## [Unreleased]") {
		t.Error("missing [Unreleased]")
	}
	if !strings.Contains(content, "### Fixed") {
		t.Error("missing Fixed in unreleased")
	}
	if !strings.Contains(content, "## [1.0.0]") {
		t.Error("previous release section should be preserved")
	}
	// Old [Unreleased] content should be replaced
	if strings.Contains(content, "old entry") {
		t.Error("[Unreleased] should be replaced, not appended")
	}
}

func TestValidateFragmentExists(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "Forge-abc.md"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(dir, "Forge-def.en.md"), []byte("x"), 0644)

	if !ValidateFragmentExists(dir, "Forge-abc") {
		t.Error("should find Forge-abc.md")
	}
	if !ValidateFragmentExists(dir, "Forge-def") {
		t.Error("should find Forge-def.en.md")
	}
	if ValidateFragmentExists(dir, "Forge-nope") {
		t.Error("should not find Forge-nope")
	}
}

func TestCollectEmptyDir(t *testing.T) {
	dir := t.TempDir()
	fragments, err := CollectFragments(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fragments) != 0 {
		t.Errorf("expected 0 fragments, got %d", len(fragments))
	}
}

func TestCollectNonExistentDir(t *testing.T) {
	fragments, err := CollectFragments("/nonexistent/path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fragments != nil {
		t.Errorf("expected nil, got %v", fragments)
	}
}
