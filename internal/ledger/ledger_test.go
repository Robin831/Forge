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
	assert.NotContains(t, out, longPath, "untruncated path must not appear in output")
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
