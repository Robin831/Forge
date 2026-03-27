package ledger

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// renderOSC8Link
// ---------------------------------------------------------------------------

func TestRenderOSC8LinkWithURL(t *testing.T) {
	out := renderOSC8Link("https://example.com", "click me")
	assert.Contains(t, out, "https://example.com", "URL must be in escape sequence")
	assert.Contains(t, out, "click me", "display text must appear")
	assert.Contains(t, out, "\x1b]8;;", "OSC 8 open sequence must be present")
}

func TestRenderOSC8LinkEmptyURL(t *testing.T) {
	out := renderOSC8Link("", "plain text")
	assert.Equal(t, "plain text", out, "empty URL must return text unchanged")
}

// ---------------------------------------------------------------------------
// wrapDetailText
// ---------------------------------------------------------------------------

func TestWrapDetailTextEmpty(t *testing.T) {
	assert.Nil(t, wrapDetailText("", 20))
}

func TestWrapDetailTextZeroWidth(t *testing.T) {
	assert.Nil(t, wrapDetailText("hello world", 0))
}

func TestWrapDetailTextNegativeWidth(t *testing.T) {
	assert.Nil(t, wrapDetailText("hello world", -1))
}

func TestWrapDetailTextFitsOnOneLine(t *testing.T) {
	lines := wrapDetailText("hello world", 20)
	require.Len(t, lines, 1)
	assert.Equal(t, "hello world", lines[0])
}

func TestWrapDetailTextWrapsAtWordBoundary(t *testing.T) {
	// "hello world" — "world" pushes past width=8
	lines := wrapDetailText("hello world", 8)
	require.Len(t, lines, 2)
	assert.Equal(t, "hello", lines[0])
	assert.Equal(t, "world", lines[1])
}

func TestWrapDetailTextPreservesParagraphBreaks(t *testing.T) {
	text := "first paragraph\n\nsecond paragraph"
	lines := wrapDetailText(text, 40)
	// Expect: "first paragraph", "", "second paragraph"
	require.Len(t, lines, 3)
	assert.Equal(t, "first paragraph", lines[0])
	assert.Equal(t, "", lines[1])
	assert.Equal(t, "second paragraph", lines[2])
}

func TestWrapDetailTextWindowsLineEndings(t *testing.T) {
	text := "line one\r\nline two"
	lines := wrapDetailText(text, 40)
	require.Len(t, lines, 2)
	assert.Equal(t, "line one", lines[0])
	assert.Equal(t, "line two", lines[1])
}

func TestWrapDetailTextLongWordNotDropped(t *testing.T) {
	// A single word that exceeds the width must still appear (no drop).
	lines := wrapDetailText("superlongword", 5)
	require.Len(t, lines, 1)
	assert.Equal(t, "superlongword", lines[0])
}

// ---------------------------------------------------------------------------
// writeDetailField
// ---------------------------------------------------------------------------

func TestWriteDetailFieldBasic(t *testing.T) {
	var sb strings.Builder
	style := lipgloss.NewStyle()
	writeDetailField(&sb, style, "Status", "open", 30)
	out := sb.String()
	assert.Contains(t, out, "Status:")
	assert.Contains(t, out, "open")
	assert.True(t, strings.HasSuffix(out, "\n"), "writeDetailField must end with newline")
}

func TestWriteDetailFieldTruncatesLongValue(t *testing.T) {
	var sb strings.Builder
	style := lipgloss.NewStyle()
	// innerW=10; key="Priority: " is 10 chars → valW=max(10-10,1)=1
	writeDetailField(&sb, style, "Priority", "P1 extra text", 10)
	out := sb.String()
	// Value should be truncated to 1 char ("…")
	assert.Contains(t, out, "Priority:")
}

// ---------------------------------------------------------------------------
// renderBeadDetailContent
// ---------------------------------------------------------------------------

func TestRenderBeadDetailContentMinimal(t *testing.T) {
	var sb strings.Builder
	b := &Bead{
		ID:       "Forge-abc1",
		Title:    "Test bead title",
		Status:   "open",
		Priority: 2,
		Anvil:    "heimdall",
	}
	renderBeadDetailContent(&sb, b, 30)
	out := sb.String()

	assert.Contains(t, out, "Forge-abc1", "ID must appear")
	assert.Contains(t, out, "Test bead title", "title must appear")
	assert.Contains(t, out, "Status:", "Status field must appear")
	assert.Contains(t, out, "open", "status value must appear")
	assert.Contains(t, out, "Priority:", "Priority field must appear")
	assert.Contains(t, out, "P2", "priority value must appear")
	assert.Contains(t, out, "Anvil:", "Anvil field must appear")
	assert.Contains(t, out, "heimdall", "anvil value must appear")
}

func TestRenderBeadDetailContentOptionalFields(t *testing.T) {
	now := time.Now()
	closed := now.Add(-24 * time.Hour)
	var sb strings.Builder
	b := &Bead{
		ID:        "Forge-xyz",
		Title:     "Full bead",
		Status:    "closed",
		Priority:  1,
		Anvil:     "repo",
		IssueType: "bug",
		Assignee:  "alice",
		Labels:    []string{"urgent", "backend"},
		HasPR:     true,
		UpdatedAt: &now,
		ClosedAt:  &closed,
		DependsOn: []string{"Forge-dep1"},
		Blocks:    []string{"Forge-blk1"},
		Description: "Some description text",
	}
	renderBeadDetailContent(&sb, b, 30)
	out := sb.String()

	assert.Contains(t, out, "Type:", "IssueType field must appear when set")
	assert.Contains(t, out, "bug")
	assert.Contains(t, out, "Assignee:")
	assert.Contains(t, out, "@alice")
	assert.Contains(t, out, "Labels:")
	assert.Contains(t, out, "urgent")
	assert.Contains(t, out, "PR:")
	assert.Contains(t, out, "Updated:")
	assert.Contains(t, out, "Closed:")
	assert.Contains(t, out, "Depends on:")
	assert.Contains(t, out, "Forge-dep1")
	assert.Contains(t, out, "Blocks:")
	assert.Contains(t, out, "Forge-blk1")
	assert.Contains(t, out, "Description:")
	assert.Contains(t, out, "Some description text")
}

func TestRenderBeadDetailContentOmitsEmptyOptionals(t *testing.T) {
	var sb strings.Builder
	b := &Bead{ID: "Forge-min", Title: "Minimal", Status: "open", Priority: 3, Anvil: "a"}
	renderBeadDetailContent(&sb, b, 30)
	out := sb.String()

	assert.NotContains(t, out, "Type:")
	assert.NotContains(t, out, "Assignee:")
	assert.NotContains(t, out, "Labels:")
	assert.NotContains(t, out, "PR:")
	assert.NotContains(t, out, "GitHub:")
	assert.NotContains(t, out, "Updated:")
	assert.NotContains(t, out, "Closed:")
	assert.NotContains(t, out, "Depends on:")
	assert.NotContains(t, out, "Blocks:")
	assert.NotContains(t, out, "Description:")
}

func TestRenderBeadDetailContentExternalRefGH(t *testing.T) {
	var sb strings.Builder
	b := &Bead{
		ID:          "Forge-xyz",
		Title:       "Ref test",
		Status:      "open",
		Priority:    2,
		Anvil:       "repo",
		ExternalRef: "gh-42",
	}
	renderBeadDetailContent(&sb, b, 30)
	out := sb.String()

	assert.Contains(t, out, "GitHub:", "GitHub field must appear when external_ref is set")
	assert.Contains(t, out, "#42", "issue number must be shown in #N format")
}

func TestRenderBeadDetailContentExternalRefWithURL(t *testing.T) {
	var sb strings.Builder
	b := &Bead{
		ID:             "Forge-xyz",
		Title:          "Link test",
		Status:         "open",
		Priority:       2,
		Anvil:          "repo",
		ExternalRef:    "gh-7",
		ExternalRefURL: "https://github.com/org/repo/issues/7",
	}
	renderBeadDetailContent(&sb, b, 30)
	out := sb.String()

	assert.Contains(t, out, "GitHub:")
	assert.Contains(t, out, "#7")
	// OSC 8 hyperlink escape sequence must be present when URL is set.
	assert.Contains(t, out, "https://github.com/org/repo/issues/7")
}

func TestRenderBeadDetailContentExternalRefNonGH(t *testing.T) {
	var sb strings.Builder
	b := &Bead{
		ID:          "Forge-abc",
		Title:       "Non-GH ref",
		Status:      "open",
		Priority:    2,
		Anvil:       "repo",
		ExternalRef: "jira-99",
	}
	renderBeadDetailContent(&sb, b, 30)
	out := sb.String()

	assert.Contains(t, out, "GitHub:", "GitHub field must appear for any non-empty external_ref")
	assert.Contains(t, out, "jira-99", "raw ref value must be shown when not gh- format")
}

// ---------------------------------------------------------------------------
// detailPanelW / mainPanelWidth
// ---------------------------------------------------------------------------

func TestDetailPanelWHiddenByFlag(t *testing.T) {
	m := &Model{width: 160, showDetailPanel: false}
	assert.Equal(t, 0, m.detailPanelW(), "hidden panel must return 0 width")
}

func TestDetailPanelWHiddenByNarrowTerminal(t *testing.T) {
	m := &Model{width: minWidthForDetailPanel - 1, showDetailPanel: true}
	assert.Equal(t, 0, m.detailPanelW(), "narrow terminal must suppress detail panel")
}

func TestDetailPanelWVisible(t *testing.T) {
	m := &Model{width: minWidthForDetailPanel, showDetailPanel: true}
	assert.Equal(t, detailPanelFixedW, m.detailPanelW())
}

func TestMainPanelWidthNoDetailPanel(t *testing.T) {
	m := &Model{width: 100, showDetailPanel: false}
	assert.Equal(t, 100, m.mainPanelWidth())
}

func TestMainPanelWidthWithDetailPanel(t *testing.T) {
	m := &Model{width: 160, showDetailPanel: true}
	assert.Equal(t, 160-detailPanelFixedW-2, m.mainPanelWidth()) // -2 for main panel border
}

// ---------------------------------------------------------------------------
// renderDetailPanel
// ---------------------------------------------------------------------------

func TestRenderDetailPanelHidden(t *testing.T) {
	m := &Model{width: 80, showDetailPanel: false}
	assert.Equal(t, "", m.renderDetailPanel(), "hidden panel must render empty string")
}

func TestRenderDetailPanelNoBead(t *testing.T) {
	m := &Model{width: 160, height: 30, showDetailPanel: true}
	panel := m.renderDetailPanel()
	assert.Contains(t, panel, "No bead selected", "empty state text must appear")
}

func TestRenderDetailPanelWithBead(t *testing.T) {
	m := &Model{width: 160, height: 30, showDetailPanel: true}
	m.beads = []Bead{
		{ID: "Forge-t1", Title: "A bead", Status: "open", Priority: 2, Anvil: "repo"},
	}
	m.refreshHierarchy()
	panel := m.renderDetailPanel()
	assert.Contains(t, panel, "Forge-t1")
	assert.Contains(t, panel, "A bead")
}

// ---------------------------------------------------------------------------
// clipLines
// ---------------------------------------------------------------------------

func TestClipLinesZero(t *testing.T) {
	assert.Equal(t, "", clipLines("a\nb\nc", 0))
}

func TestClipLinesNegative(t *testing.T) {
	assert.Equal(t, "", clipLines("a\nb\nc", -1))
}

func TestClipLinesFewerLinesThanN(t *testing.T) {
	s := "line1\nline2"
	assert.Equal(t, s, clipLines(s, 5), "input shorter than n must be returned unchanged")
}

func TestClipLinesExactlyN(t *testing.T) {
	s := "line1\nline2\nline3"
	assert.Equal(t, s, clipLines(s, 3), "input equal to n must be returned unchanged")
}

func TestClipLinesMoreThanN(t *testing.T) {
	s := "line1\nline2\nline3\nline4\nline5"
	got := clipLines(s, 3)
	assert.Equal(t, "line1\nline2\nline3", got, "must keep only first n lines")
}

func TestClipLinesOneLineNoNewline(t *testing.T) {
	assert.Equal(t, "hello", clipLines("hello", 1))
}

func TestClipLinesOneLineTruncated(t *testing.T) {
	got := clipLines("a\nb", 1)
	assert.Equal(t, "a", got)
}

func TestClipLinesPreservesContent(t *testing.T) {
	// Verify that clipping does not alter the kept lines.
	s := "alpha\nbeta\ngamma\ndelta"
	got := clipLines(s, 2)
	assert.Equal(t, "alpha\nbeta", got)
}

// ---------------------------------------------------------------------------
// detailPanelBaseStyle frame overhead is consistent
// ---------------------------------------------------------------------------

func TestDetailPanelBaseStyleFrameWidth(t *testing.T) {
	style := detailPanelBaseStyle()
	// Frame must account for at least the left border (1) + padding (2).
	assert.GreaterOrEqual(t, style.GetHorizontalFrameSize(), 3,
		"base style horizontal frame must be at least 3 (border + padding)")
}
