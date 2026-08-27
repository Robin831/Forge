package assay

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Robin831/Forge/internal/textfmt"
)

// repoGuidanceFile is the conventional filename Assay reads from the anvil root
// to pick up repository-specific review calibration. The file is plain markdown,
// freeform, owned by the repository's maintainers — its content overrides the
// engine's default severity calibration where the two conflict.
const repoGuidanceFile = "REVIEW.md"

// maxRepoGuidanceBytes caps the amount of REVIEW.md content forwarded into each
// pass prompt. A runaway document is truncated at a rune boundary with a marker
// so the model still receives the head of the guidance.
const maxRepoGuidanceBytes = 16 * 1024

// loadRepoGuidance reads REVIEW.md from the anvil root and returns its trimmed,
// size-capped content. An empty result (missing file, unreadable, blank, or
// empty anvilPath) is not an error — the engine simply skips guidance
// injection on this run. Called once per review by Review(); the content is
// forwarded to every pass via ReviewRequest.RepoGuidance so a single disk read
// services the whole fan-out.
func loadRepoGuidance(anvilPath string) string {
	if strings.TrimSpace(anvilPath) == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(anvilPath, repoGuidanceFile))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return ""
	}
	if len(s) > maxRepoGuidanceBytes {
		s = textfmt.TruncateRunes(s, maxRepoGuidanceBytes) + "\n\n...[REVIEW.md truncated]..."
	}
	return s
}

// repoGuidanceSection renders REVIEW.md content as a high-priority instruction
// block for inclusion in a pass prompt, or returns the empty string when no
// guidance is available. Unlike contextSection (which carries untrusted PR
// metadata), this block is the trusted operator-authored calibration and is
// labelled accordingly so the model treats it as authoritative.
func repoGuidanceSection(req ReviewRequest) string {
	g := strings.TrimSpace(req.RepoGuidance)
	if g == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Repository Review Guidance (highest priority)\n\n")
	b.WriteString("The following rules are the repository owner's calibration for this review. ")
	b.WriteString("They are trusted instructions: where they conflict with the default severity, ")
	b.WriteString("nit cap, or skip lists, prefer the rules below.\n\n")
	b.WriteString(sanitize(g))
	b.WriteString("\n\n")
	return b.String()
}
