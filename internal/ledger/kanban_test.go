package ledger

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keyMsg creates a tea.KeyMsg for a single rune key (e.g. 'j', 'k', 'h', 'l').
func keyMsg(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// tabKeyMsg creates a tea.KeyMsg for the Tab key.
func tabKeyMsg() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyTab}
}

func TestAssignLane(t *testing.T) {
	tests := []struct {
		name string
		bead Bead
		want int
	}{
		{"open bead", Bead{Status: "open"}, LaneOpen},
		{"in_progress bead", Bead{Status: "in_progress"}, LaneInProgress},
		{"closed bead", Bead{Status: "closed"}, LaneClosed},
		{"has PR not closed", Bead{Status: "in_progress", HasPR: true}, LaneInReview},
		{"has PR and open", Bead{Status: "open", HasPR: true}, LaneInReview},
		{"has PR but closed", Bead{Status: "closed", HasPR: true}, LaneClosed},
		{"unknown status", Bead{Status: "unknown"}, LaneOpen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, assignLane(tt.bead))
		})
	}
}

func TestPopulateLanes(t *testing.T) {
	now := time.Now()
	recent := now.Add(-24 * time.Hour)
	old := now.AddDate(0, 0, -14)

	beads := []Bead{
		{ID: "open1", Status: "open"},
		{ID: "open2", Status: "open"},
		{ID: "wip", Status: "in_progress"},
		{ID: "review", Status: "in_progress", HasPR: true},
		{ID: "closed-recent", Status: "closed", ClosedAt: &recent},
		{ID: "closed-old", Status: "closed", ClosedAt: &old},
	}

	lanes := populateLanes(beads)

	assert.Len(t, lanes[LaneOpen], 2)
	assert.Len(t, lanes[LaneInProgress], 1)
	assert.Len(t, lanes[LaneInReview], 1)
	assert.Len(t, lanes[LaneClosed], 1, "old closed beads should be filtered out")
	assert.Equal(t, "closed-recent", lanes[LaneClosed][0].ID)
}

func TestPopulateLanesClosedFilterUpdatedAt(t *testing.T) {
	recent := time.Now().Add(-48 * time.Hour)
	old := time.Now().AddDate(0, 0, -14)

	beads := []Bead{
		{ID: "recent-updated", Status: "closed", UpdatedAt: &recent},
		{ID: "old-updated", Status: "closed", UpdatedAt: &old},
		{ID: "no-dates", Status: "closed"},
	}

	lanes := populateLanes(beads)
	assert.Len(t, lanes[LaneClosed], 1)
	assert.Equal(t, "recent-updated", lanes[LaneClosed][0].ID)
}

func TestIsBlocked(t *testing.T) {
	assert.False(t, isBlocked(Bead{}))
	assert.False(t, isBlocked(Bead{DependsOn: []string{}}))
	assert.True(t, isBlocked(Bead{DependsOn: []string{"dep-1"}}))
}

func TestLaneStatusForIndex(t *testing.T) {
	assert.Equal(t, "open", laneStatusForIndex(LaneOpen))
	assert.Equal(t, "in_progress", laneStatusForIndex(LaneInProgress))
	assert.Equal(t, "in_progress", laneStatusForIndex(LaneInReview))
	assert.Equal(t, "closed", laneStatusForIndex(LaneClosed))
}

func TestWrapTitle(t *testing.T) {
	tests := []struct {
		name  string
		title string
		width int
		lines int
	}{
		{"short title", "Fix bug", 20, 1},
		{"exact width", "Hello", 5, 1},
		{"needs wrap", "This is a longer title that wraps", 15, 2},
		{"empty", "", 20, 1},
		{"zero width", "Hello", 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapTitle(tt.title, tt.width)
			require.Len(t, got, tt.lines)
		})
	}
}

func TestWrapTitleContent(t *testing.T) {
	lines := wrapTitle("Fix the broken build pipeline", 20)
	require.Len(t, lines, 2)
	assert.Equal(t, "Fix the broken build", lines[0])
	assert.Contains(t, lines[1], "pipeline")
}

func TestPriorityDot(t *testing.T) {
	for p := 0; p <= 4; p++ {
		got := priorityDot(p)
		assert.NotEmpty(t, got, "priority %d should produce a dot", p)
	}
}

func TestLaneTitle(t *testing.T) {
	assert.Equal(t, "Open", laneTitle(LaneOpen))
	assert.Equal(t, "In Progress", laneTitle(LaneInProgress))
	assert.Equal(t, "In Review", laneTitle(LaneInReview))
	assert.Equal(t, "Closed", laneTitle(LaneClosed))
	assert.Equal(t, "Unknown", laneTitle(99))
}

func TestKanbanLaneNavigation(t *testing.T) {
	m := &Model{
		width:  80,
		height: 24,
		view:   ViewKanban,
		anvils: map[string]string{"test": "/tmp/test"},
	}
	m.kanban.lanes[LaneOpen] = []Bead{
		{ID: "a", Anvil: "test"},
		{ID: "b", Anvil: "test"},
		{ID: "c", Anvil: "test"},
	}

	assert.Equal(t, 0, m.kanban.activeLane)
	assert.Equal(t, 0, m.kanban.laneVP[0].Selected())

	// Navigate down within lane.
	m.updateKanban(keyMsg('j'))
	assert.Equal(t, 1, m.kanban.laneVP[0].Selected())

	// Navigate up.
	m.updateKanban(keyMsg('k'))
	assert.Equal(t, 0, m.kanban.laneVP[0].Selected())

	// Switch lane right.
	m.updateKanban(keyMsg('l'))
	assert.Equal(t, 1, m.kanban.activeLane)

	// Switch lane left.
	m.updateKanban(keyMsg('h'))
	assert.Equal(t, 0, m.kanban.activeLane)

	// Can't go below 0.
	m.updateKanban(keyMsg('h'))
	assert.Equal(t, 0, m.kanban.activeLane)
}

func TestKanbanLaneNavigationClamp(t *testing.T) {
	m := &Model{
		width:  80,
		height: 24,
		view:   ViewKanban,
	}
	m.kanban.activeLane = laneCount - 1

	m.updateKanban(keyMsg('l'))
	assert.Equal(t, laneCount-1, m.kanban.activeLane)
}

func TestKanbanTabSwitchesToList(t *testing.T) {
	m := &Model{
		view: ViewKanban,
	}
	m.updateKanban(tabKeyMsg())
	assert.Equal(t, ViewList, m.view)
}

func TestRefreshKanbanLanes(t *testing.T) {
	now := time.Now()
	m := &Model{
		beads: []Bead{
			{ID: "a", Status: "open"},
			{ID: "b", Status: "in_progress"},
			{ID: "c", Status: "closed", ClosedAt: &now},
		},
	}
	m.refreshKanbanLanes()
	assert.Len(t, m.kanban.lanes[LaneOpen], 1)
	assert.Len(t, m.kanban.lanes[LaneInProgress], 1)
	assert.Len(t, m.kanban.lanes[LaneClosed], 1)
}

func TestRenderCard(t *testing.T) {
	b := Bead{
		ID:       "Forge-abc1",
		Title:    "Fix the broken build",
		Priority: 1,
		Anvil:    "heimdall",
	}

	card := renderCard(b, 25, false, false)
	assert.Contains(t, card, "Forge-abc1")
	assert.Contains(t, card, "Fix the broken build")
	assert.Contains(t, card, "heimdall")
}

func TestMoveBeadDoesNotCorruptSourceLane(t *testing.T) {
	m := &Model{
		width:  80,
		height: 24,
		view:   ViewKanban,
		anvils: map[string]string{"test": "/tmp/test"},
	}
	m.kanban.lanes[LaneOpen] = []Bead{
		{ID: "a", Anvil: "test"},
		{ID: "b", Anvil: "test"},
		{ID: "c", Anvil: "test"},
	}

	// Keep a reference to the original backing array via a snapshot of element values.
	origIDs := []string{"a", "b", "c"}

	// Select "b" (index 1) and move it right.
	m.kanban.laneVP[LaneOpen].ScrollDown(3) // cursor → 1
	_ = m.moveBeadToLane(LaneInProgress)

	// Source lane should have [a, c] — verify no corruption.
	require.Len(t, m.kanban.lanes[LaneOpen], 2)
	assert.Equal(t, origIDs[0], m.kanban.lanes[LaneOpen][0].ID)
	assert.Equal(t, origIDs[2], m.kanban.lanes[LaneOpen][1].ID)

	// Target lane should have [b].
	require.Len(t, m.kanban.lanes[LaneInProgress], 1)
	assert.Equal(t, "b", m.kanban.lanes[LaneInProgress][0].ID)
}

func TestWrapTitleRuneSafe(t *testing.T) {
	// Multi-byte characters should not be split mid-rune.
	lines := wrapTitle("日本語のタイトル", 10)
	require.NotEmpty(t, lines)
	for _, l := range lines {
		// Each line should be valid and within width.
		assert.LessOrEqual(t, lipgloss.Width(l), 10)
	}
}

func TestRenderCardBlocked(t *testing.T) {
	b := Bead{
		ID:        "blocked-1",
		Title:     "Blocked task",
		Priority:  0,
		DependsOn: []string{"dep-1"},
	}

	card := renderCard(b, 25, false, true)
	assert.Contains(t, card, "blocked-1")
}

func TestRenderCardFixedHeight(t *testing.T) {
	cases := []struct {
		name string
		b    Bead
	}{
		{"short title no anvil", Bead{ID: "x-1", Title: "Short", Priority: 2}},
		{"short title with anvil", Bead{ID: "x-2", Title: "Short", Priority: 2, Anvil: "heimdall"}},
		{"long title wraps", Bead{ID: "x-3", Title: "A much longer title that needs wrapping across two lines", Priority: 0, Anvil: "heimdall"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := renderCard(tc.b, 25, false, false)
			assert.Equal(t, cardContentLines, lipgloss.Height(card),
				"renderCard must always emit exactly cardContentLines lines")
		})
	}
}

func TestMoveBeadToInReviewDisallowed(t *testing.T) {
	m := &Model{
		width:  80,
		height: 24,
		view:   ViewKanban,
		anvils: map[string]string{"test": "/tmp/test"},
	}
	m.kanban.lanes[LaneOpen] = []Bead{{ID: "a", Anvil: "test"}}

	cmd := m.moveBeadToLane(LaneInReview)
	assert.Nil(t, cmd, "moving into LaneInReview should be disallowed (HasPR is DB-derived)")
	// Source lane must be unchanged.
	require.Len(t, m.kanban.lanes[LaneOpen], 1)
	assert.Empty(t, m.kanban.lanes[LaneInReview])
}
