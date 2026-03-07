// Package changelog handles changelog fragment parsing and assembly.
//
// Fragments live in changelog.d/<bead-id>.md (or <bead-id>.en.md for
// backwards compatibility). Each fragment has a "category: <Category>"
// header line followed by markdown bullet points describing changes.
package changelog

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Valid changelog categories, in the order they appear in the assembled output.
var Categories = []string{
	"Added",
	"Changed",
	"Deprecated",
	"Removed",
	"Fixed",
	"Security",
}

// Fragment represents a single changelog entry file.
type Fragment struct {
	BeadID   string
	Category string
	Bullets  []string
}

// ParseFragment reads a changelog fragment file and returns a Fragment.
func ParseFragment(path string) (Fragment, error) {
	f, err := os.Open(path)
	if err != nil {
		return Fragment{}, fmt.Errorf("opening fragment: %w", err)
	}
	defer f.Close()

	base := filepath.Base(path)
	beadID := strings.TrimSuffix(base, ".md")
	beadID = strings.TrimSuffix(beadID, ".en") // strip optional .en suffix

	var frag Fragment
	frag.BeadID = beadID

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()

		// Parse category header
		if strings.HasPrefix(line, "category:") {
			frag.Category = strings.TrimSpace(strings.TrimPrefix(line, "category:"))
			continue
		}

		// Collect bullet lines (non-empty, non-category lines)
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		frag.Bullets = append(frag.Bullets, line)
	}

	if err := scanner.Err(); err != nil {
		return Fragment{}, fmt.Errorf("reading fragment: %w", err)
	}

	if frag.Category == "" {
		return Fragment{}, fmt.Errorf("fragment %s: missing 'category:' header", path)
	}

	if !isValidCategory(frag.Category) {
		return Fragment{}, fmt.Errorf("fragment %s: invalid category %q (valid: %s)",
			path, frag.Category, strings.Join(Categories, ", "))
	}

	if len(frag.Bullets) == 0 {
		return Fragment{}, fmt.Errorf("fragment %s: no changelog entries", path)
	}

	return frag, nil
}

// CollectFragments reads all fragment files from a changelog.d directory.
func CollectFragments(dir string) ([]Fragment, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading changelog.d: %w", err)
	}

	var fragments []Fragment
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		frag, err := ParseFragment(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		fragments = append(fragments, frag)
	}

	return fragments, nil
}

// Assemble builds an [Unreleased] section from fragments, grouped by category.
func Assemble(fragments []Fragment) string {
	if len(fragments) == 0 {
		return ""
	}

	// Group by category
	grouped := make(map[string][]string)
	for _, f := range fragments {
		grouped[f.Category] = append(grouped[f.Category], f.Bullets...)
	}

	var sb strings.Builder
	sb.WriteString("## [Unreleased]\n")

	for _, cat := range Categories {
		bullets, ok := grouped[cat]
		if !ok || len(bullets) == 0 {
			continue
		}
		sort.Strings(bullets)
		sb.WriteString("\n### " + cat + "\n\n")
		for _, b := range bullets {
			sb.WriteString(b + "\n")
		}
	}

	return sb.String()
}

// UpdateChangelog reads an existing CHANGELOG.md, replaces the [Unreleased]
// section with assembled fragments, and returns the new content.
// If the file doesn't exist, a new one is created.
func UpdateChangelog(changelogPath string, fragments []Fragment) (string, error) {
	unreleased := Assemble(fragments)
	if unreleased == "" {
		return "", fmt.Errorf("no fragments to assemble")
	}

	existing, err := os.ReadFile(changelogPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Create fresh changelog
			var sb strings.Builder
			sb.WriteString("# Changelog\n\n")
			sb.WriteString("All notable changes to The Forge will be documented in this file.\n\n")
			sb.WriteString("The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).\n\n")
			sb.WriteString(unreleased)
			return sb.String(), nil
		}
		return "", fmt.Errorf("reading changelog: %w", err)
	}

	content := string(existing)

	// Find and replace the [Unreleased] section
	unreleasedStart := strings.Index(content, "## [Unreleased]")
	if unreleasedStart == -1 {
		// No [Unreleased] section — insert after the header
		headerEnd := strings.Index(content, "\n\n")
		if headerEnd == -1 {
			return content + "\n\n" + unreleased, nil
		}
		// Find end of preamble (after blank line following first heading)
		rest := content[headerEnd:]
		return content[:headerEnd] + "\n\n" + unreleased + rest, nil
	}

	// Find the end of the [Unreleased] section (next ## heading or EOF)
	afterStart := unreleasedStart + len("## [Unreleased]")
	nextSection := strings.Index(content[afterStart:], "\n## ")
	if nextSection == -1 {
		// [Unreleased] is the last section
		return content[:unreleasedStart] + unreleased, nil
	}

	// Replace [Unreleased] through to the next section
	sectionEnd := afterStart + nextSection + 1 // +1 to include the newline
	return content[:unreleasedStart] + unreleased + "\n" + content[sectionEnd:], nil
}

func isValidCategory(cat string) bool {
	for _, c := range Categories {
		if strings.EqualFold(c, cat) {
			return true
		}
	}
	return false
}

// ValidateFragmentExists checks if a changelog fragment exists for the given bead ID.
func ValidateFragmentExists(dir, beadID string) bool {
	patterns := []string{
		filepath.Join(dir, beadID+".md"),
		filepath.Join(dir, beadID+".en.md"),
	}
	for _, p := range patterns {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// ListBeadIDs returns the bead IDs from all fragments in the directory.
func ListBeadIDs(dir string) ([]string, error) {
	fragments, err := CollectFragments(dir)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(fragments))
	for i, f := range fragments {
		ids[i] = f.BeadID
	}
	return ids, nil
}
