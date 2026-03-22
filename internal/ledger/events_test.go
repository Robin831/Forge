package ledger

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEventPanelAlwaysVisible(t *testing.T) {
	m := &Model{}
	// Activity panel is always visible — eventPanelH should always return non-zero.
	assert.Greater(t, m.eventPanelH(), 0, "event panel should always have height")
}

func TestAddEvent(t *testing.T) {
	m := &Model{}

	m.addEvent(EventInfo, "hello world")
	m.addEvent(EventWarn, "something odd")
	m.addEvent(EventError, "something failed")

	assert.Len(t, m.eventLog, 3)
	assert.Equal(t, EventInfo, m.eventLog[0].Level)
	assert.Equal(t, "hello world", m.eventLog[0].Message)
	assert.Equal(t, EventError, m.eventLog[2].Level)
}

func TestAddEventRetentionCap(t *testing.T) {
	m := &Model{}
	for range eventRetentionCap + 10 {
		m.addEvent(EventInfo, "event")
	}
	assert.Len(t, m.eventLog, eventRetentionCap, "log must not exceed retention cap")
}

func TestHasEventErrors(t *testing.T) {
	m := &Model{}
	assert.False(t, m.hasEventErrors(), "no errors initially")

	m.addEvent(EventInfo, "ok")
	assert.False(t, m.hasEventErrors(), "info event should not trigger error flag")

	m.addEvent(EventError, "boom")
	assert.True(t, m.hasEventErrors(), "error event should trigger error flag")
}

func TestEventPanelRender(t *testing.T) {
	m := &Model{width: 80, height: 24, showEventPanel: true}
	m.addEvent(EventInfo, "Created Forge-abc1")
	m.addEvent(EventError, "Refresh error: context deadline exceeded")

	panel := m.renderEventPanel()
	assert.Contains(t, panel, "⚡ Activity", "panel should have title")
	assert.Contains(t, panel, "Created Forge-abc1", "panel should show info event")
	assert.Contains(t, panel, "Refresh error", "panel should show error event")
	// Activity panel is always visible — no hide hint.

	lines := strings.Split(panel, "\n")
	assert.Equal(t, 2+eventPanelContentH, len(lines), "panel height should match eventPanelH")
}

func TestEventPanelH(t *testing.T) {
	m := &Model{}
	// Activity panel is always visible.
	assert.Equal(t, 2+eventPanelContentH, m.eventPanelH(), "panel height should always be 2+content rows")
}

func TestFooterErrorHint(t *testing.T) {
	m := newTestModel()

	// No errors — no hint should appear.
	footer := m.renderFooter()
	if strings.Contains(footer, "⚠ errors") {
		t.Error("footer should not show error hint when no errors are logged")
	}

	// Errors logged — hint should appear since activity panel shows them.
	m.addEvent(EventError, "something failed")
	footer = m.renderFooter()
	if !strings.Contains(footer, "⚠ errors") {
		t.Error("footer should show error hint when errors are logged")
	}
}

func TestPlaceEventPanelOverlay(t *testing.T) {
	// 5-line background, 2-line panel — panel should occupy the bottom 2 lines.
	bg := "line1\nline2\nline3\nline4\nline5"
	panel := "=sep=\n=content="
	result := placeEventPanelOverlay(9, 5, panel, bg)
	lines := strings.Split(result, "\n")
	assert.Equal(t, 5, len(lines))
	assert.Equal(t, "line1", strings.TrimRight(lines[0], " "))
	assert.Equal(t, "line2", strings.TrimRight(lines[1], " "))
	assert.Equal(t, "line3", strings.TrimRight(lines[2], " "))
	assert.Contains(t, lines[3], "=sep=")
	assert.Contains(t, lines[4], "=content=")
}
