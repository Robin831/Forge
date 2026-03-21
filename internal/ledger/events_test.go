package ledger

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEventPanelToggle(t *testing.T) {
	m := newTestModel()

	if m.showEventPanel {
		t.Fatal("event panel should be hidden initially")
	}

	// Press E to show the panel.
	m = sendKey(m, "E")
	if !m.showEventPanel {
		t.Error("event panel should be visible after pressing E")
	}

	// Press E again to hide it.
	m = sendKey(m, "E")
	if m.showEventPanel {
		t.Error("event panel should be hidden after pressing E a second time")
	}
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
	assert.Contains(t, panel, "E: hide", "panel should hint how to hide")

	lines := strings.Split(panel, "\n")
	assert.Equal(t, 2+eventPanelContentH, len(lines), "panel height should match eventPanelH")
}

func TestEventPanelH(t *testing.T) {
	m := &Model{}
	assert.Equal(t, 0, m.eventPanelH(), "hidden panel has zero height")

	m.showEventPanel = true
	assert.Equal(t, 2+eventPanelContentH, m.eventPanelH(), "visible panel height should be 2+content rows")
}

func TestFooterErrorHint(t *testing.T) {
	m := newTestModel()

	// No errors and panel hidden — no hint should appear.
	footer := m.renderFooter()
	if strings.Contains(footer, "E: show") {
		t.Error("footer should not show error hint when no errors are logged")
	}

	// Errors logged but panel hidden — hint must appear.
	m.addEvent(EventError, "something failed")
	footer = m.renderFooter()
	if !strings.Contains(footer, "E: show") {
		t.Error("footer should show error hint when errors are logged and panel is hidden")
	}

	// Errors logged but panel is open — hint must NOT appear (panel is already visible).
	m.showEventPanel = true
	footer = m.renderFooter()
	if strings.Contains(footer, "E: show") {
		t.Error("footer should not show error hint when event panel is already open")
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
