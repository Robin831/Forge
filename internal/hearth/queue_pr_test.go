package hearth

import (
	"strings"
	"testing"
)

func TestPRURLForBead(t *testing.T) {
	urls := map[string]string{
		"forge":     "https://github.com/Robin831/Forge",
		"withslash": "https://github.com/owner/repo/",
	}

	tests := []struct {
		name     string
		anvil    string
		prNumber int
		want     string
	}{
		{"valid", "forge", 42, "https://github.com/Robin831/Forge/pull/42"},
		{"trailing slash trimmed", "withslash", 7, "https://github.com/owner/repo/pull/7"},
		{"no PR number", "forge", 0, ""},
		{"negative PR number", "forge", -1, ""},
		{"unknown anvil", "missing", 5, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := prURLForBead(urls, tt.anvil, tt.prNumber)
			if got != tt.want {
				t.Errorf("prURLForBead(%q, %d) = %q; want %q", tt.anvil, tt.prNumber, got, tt.want)
			}
		})
	}
}

func TestRenderOSC8Link(t *testing.T) {
	out := renderOSC8Link("https://example.com/pull/1", "PR #1")
	if !strings.Contains(out, "\x1b]8;;https://example.com/pull/1\x1b\\") {
		t.Errorf("expected OSC 8 open sequence with URL, got %q", out)
	}
	if !strings.Contains(out, "PR #1") {
		t.Errorf("expected link text in output, got %q", out)
	}
	if !strings.HasSuffix(out, "\x1b]8;;\x1b\\") {
		t.Errorf("expected OSC 8 close sequence, got %q", out)
	}
}

func TestRenderOSC8LinkEmptyURL(t *testing.T) {
	out := renderOSC8Link("", "plain")
	if out != "plain" {
		t.Errorf("expected plain text for empty URL, got %q", out)
	}
}

func TestRenderOSC8LinkRejectsControlChars(t *testing.T) {
	// A URL with an embedded ESC must not produce an OSC 8 sequence.
	out := renderOSC8Link("https://example.com/\x1bevil", "PR #1")
	if strings.Contains(out, "\x1b]8;;") {
		t.Errorf("expected no OSC 8 sequence for unsafe URL, got %q", out)
	}
	if out != "PR #1" {
		t.Errorf("expected fallback to plain text, got %q", out)
	}
}

func TestRenderQueuePRCellWithPR(t *testing.T) {
	cell := renderQueuePRCell(123, "https://github.com/owner/repo/pull/123")
	if !strings.Contains(cell, "PR #123") {
		t.Errorf("expected 'PR #123' in cell, got %q", cell)
	}
	if !strings.Contains(cell, "\x1b]8;;https://github.com/owner/repo/pull/123\x1b\\") {
		t.Errorf("expected OSC 8 hyperlink to the PR URL, got %q", cell)
	}
}

func TestRenderQueuePRCellNoPR(t *testing.T) {
	cell := renderQueuePRCell(0, "")
	if strings.Contains(cell, "PR #") {
		t.Errorf("expected blank cell when no PR, got %q", cell)
	}
	if len(cell) != prColumnWidth {
		t.Errorf("expected blank cell width %d, got %d (%q)", prColumnWidth, len(cell), cell)
	}
}

func TestRenderQueuePRCellNoURLFallsBackToPlainText(t *testing.T) {
	// When the anvil repo URL is unknown the PR number is still shown, but as
	// plain text without an OSC 8 hyperlink.
	cell := renderQueuePRCell(9, "")
	if !strings.Contains(cell, "PR #9") {
		t.Errorf("expected 'PR #9' in cell, got %q", cell)
	}
	if strings.Contains(cell, "\x1b]8;;") {
		t.Errorf("expected no OSC 8 sequence without URL, got %q", cell)
	}
}

func TestRenderQueueShowsPRNumber(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelQueue
	m.width = 80
	m.height = 24
	m.queue = []QueueItem{
		{BeadID: "bd-1", Anvil: "forge", Section: "ready", PRNumber: 55, PRURL: "https://github.com/owner/repo/pull/55"},
	}
	m.queueVP = scrollViewport{cursor: 0}
	m.rebuildQueueNav()

	out := m.renderQueue(80, 24)
	if !strings.Contains(out, "PR #55") {
		t.Errorf("expected rendered queue to contain 'PR #55', got:\n%s", out)
	}
	if !strings.Contains(out, "\x1b]8;;https://github.com/owner/repo/pull/55\x1b\\") {
		t.Errorf("expected rendered queue to contain OSC 8 hyperlink, got:\n%s", out)
	}
}

func TestRenderQueueNoPRNoHyperlink(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelQueue
	m.width = 80
	m.height = 24
	m.queue = []QueueItem{
		{BeadID: "bd-2", Anvil: "forge", Section: "ready"},
	}
	m.queueVP = scrollViewport{cursor: 0}
	m.rebuildQueueNav()

	out := m.renderQueue(80, 24)
	if strings.Contains(out, "\x1b]8;;") {
		t.Errorf("expected no OSC 8 hyperlink for bead without PR, got:\n%s", out)
	}
	if !strings.Contains(out, "bd-2") {
		t.Errorf("expected bead ID in output, got:\n%s", out)
	}
}
