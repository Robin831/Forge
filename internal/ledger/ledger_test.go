package ledger

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
)

func TestRenderAnvilListPathTruncation(t *testing.T) {
	const panelW = 60
	longPath := "/very/long/path/that/exceeds/the/panel/width/significantly/more/than/once"

	m := &Model{
		width:  panelW,
		height: 20,
		view:   ViewAnvils,
		anvils: map[string]string{
			"myrepo": longPath,
		},
	}

	out := m.renderAnvilList()

	// Every non-empty content line must fit within the panel width.
	for _, line := range strings.Split(out, "\n") {
		w := lipgloss.Width(line)
		assert.LessOrEqualf(t, w, panelW, "line exceeds panel width %d: %q (width=%d)", panelW, line, w)
	}

	// The long path must not appear verbatim — it should be truncated.
	assert.NotContains(t, out, longPath, "untruncated path must not appear in output")

	// Exactly one line should contain the anvil name (no wrapping produces extra lines).
	var anvilLines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "myrepo") {
			anvilLines = append(anvilLines, line)
		}
	}
	if assert.Len(t, anvilLines, 1, "expected exactly one line for the anvil item (no wrapping)") {
		assert.Contains(t, anvilLines[0], "…", "truncated path should include the ellipsis marker")
	}
}

func TestRenderAnvilListSelectedPathTruncation(t *testing.T) {
	const panelW = 60
	longPath := "/another/very/long/path/that/would/wrap/to/next/line/if/not/truncated"

	m := &Model{
		width:  panelW,
		height: 20,
		view:   ViewAnvils,
		anvils: map[string]string{
			"anvil1": longPath,
			"anvil2": "/short/path",
		},
	}
	// Select the first anvil (index 0 after sort).
	m.anvilSt.vp.cursor = 0

	out := m.renderAnvilList()

	for _, line := range strings.Split(out, "\n") {
		w := lipgloss.Width(line)
		assert.LessOrEqualf(t, w, panelW, "line exceeds panel width %d (width=%d): %q", panelW, w, line)
	}

	// The full untruncated path must not appear verbatim in the output.
	assert.NotContains(t, out, longPath, "untruncated path must not appear in output")

	// Extract just the lines corresponding to the anvil entries.
	var anvilLines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "anvil1") || strings.Contains(line, "anvil2") {
			anvilLines = append(anvilLines, line)
		}
	}

	// With two anvils, the body should render exactly two item lines (no wrapping).
	if assert.Len(t, anvilLines, 2, "expected exactly two anvil lines in rendered list") {
		// The long path (anvil1) should be truncated and show an ellipsis.
		assert.Contains(t, anvilLines[0], "…", "selected long path should be truncated with an ellipsis")
	}
}

func TestRenderTooSmallContainsSize(t *testing.T) {
	m := &Model{width: 70, height: 20}
	out := m.renderTooSmall()
	assert.Contains(t, out, "70x20", "should include current dimensions")
	assert.Contains(t, out, "80x24", "should include minimum required dimensions")
}

func TestViewEnforceMinimumSize(t *testing.T) {
	// Too narrow.
	m := &Model{width: minTermWidth - 1, height: minTermHeight}
	out := m.View()
	assert.Contains(t, out, "Terminal too small", "view should delegate to renderTooSmall when too narrow")

	// Too short.
	m = &Model{width: minTermWidth, height: minTermHeight - 1}
	out = m.View()
	assert.Contains(t, out, "Terminal too small", "view should delegate to renderTooSmall when too short")

	// Exactly at minimum — should not show too-small message.
	m = &Model{width: minTermWidth, height: minTermHeight, loading: true}
	out = m.View()
	assert.NotContains(t, out, "Terminal too small", "view must not block at minimum dimensions")

	// Zero dimensions (initial state before WindowSizeMsg) — must not block.
	m = &Model{width: 0, height: 0, loading: true}
	out = m.View()
	assert.NotContains(t, out, "Terminal too small", "zero dimensions should not trigger too-small guard")
}
