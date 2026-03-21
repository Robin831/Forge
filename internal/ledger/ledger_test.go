package ledger

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
