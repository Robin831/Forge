package ledger

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"empty", "", 5, ""},
		{"fits exactly", "hello", 5, "hello"},
		{"shorter", "hi", 5, "hi"},
		{"truncated", "hello world", 5, "hell…"},
		{"zero max", "hello", 0, ""},
		{"max one", "hello", 1, "…"},
		{"negative max", "hello", -1, ""},
		{"unicode fits", "héllo", 5, "héllo"},
		{"unicode truncated", "héllo world", 5, "héll…"},
		{"multibyte runes", "日本語テスト", 4, "日…"},
		{"emoji truncated", "🎉🎊🎈🎁", 3, "🎉…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.input, tt.maxLen)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPadRight(t *testing.T) {
	tests := []struct {
		name  string
		input string
		width int
		want  string
	}{
		{"pad needed", "hi", 5, "hi   "},
		{"exact width", "hello", 5, "hello"},
		{"longer", "hello!", 5, "hello!"},
		{"empty input", "", 3, "   "},
		{"unicode input", "日本", 5, "日本 "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := padRight(tt.input, tt.width)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestStatusOrder(t *testing.T) {
	assert.Equal(t, 0, statusOrder("in_progress"))
	assert.Equal(t, 1, statusOrder("open"))
	assert.Equal(t, 2, statusOrder("closed"))
	assert.Equal(t, 3, statusOrder("unknown"))
}

func TestStatusColor(t *testing.T) {
	assert.Equal(t, colorInfo, statusColor("open"))
	assert.Equal(t, colorWarning, statusColor("in_progress"))
	assert.Equal(t, colorMuted, statusColor("closed"))
	assert.Equal(t, colorDanger, statusColor("blocked"))
	assert.Equal(t, colorDanger, statusColor("unknown"))
}

func TestSortBeadsPriority(t *testing.T) {
	beads := []Bead{
		{ID: "b", Priority: 2},
		{ID: "a", Priority: 0},
		{ID: "c", Priority: 1},
	}
	sorted := sortBeads(beads, SortPriority)
	require.Len(t, sorted, 3)
	assert.Equal(t, "a", sorted[0].ID)
	assert.Equal(t, "c", sorted[1].ID)
	assert.Equal(t, "b", sorted[2].ID)

	// Original slice must not be mutated.
	assert.Equal(t, "b", beads[0].ID)
}

func TestSortBeadsStatus(t *testing.T) {
	beads := []Bead{
		{ID: "closed", Status: "closed"},
		{ID: "open", Status: "open"},
		{ID: "inprog", Status: "in_progress"},
	}
	sorted := sortBeads(beads, SortStatus)
	require.Len(t, sorted, 3)
	assert.Equal(t, "inprog", sorted[0].ID)
	assert.Equal(t, "open", sorted[1].ID)
	assert.Equal(t, "closed", sorted[2].ID)
}

func TestSortBeadsUpdatedAt(t *testing.T) {
	now := time.Now()
	earlier := now.Add(-1 * time.Hour)
	var nilTime *time.Time

	beads := []Bead{
		{ID: "nil", UpdatedAt: nilTime},
		{ID: "earlier", UpdatedAt: &earlier},
		{ID: "now", UpdatedAt: &now},
	}
	sorted := sortBeads(beads, SortUpdatedAt)
	require.Len(t, sorted, 3)
	// Most recent first.
	assert.Equal(t, "now", sorted[0].ID)
	assert.Equal(t, "earlier", sorted[1].ID)
	assert.Equal(t, "nil", sorted[2].ID)
}

func TestSortFieldString(t *testing.T) {
	assert.Equal(t, "Priority", SortPriority.String())
	assert.Equal(t, "Status", SortStatus.String())
	assert.Equal(t, "Updated", SortUpdatedAt.String())
	assert.Equal(t, "Priority", SortField(99).String())
}

func TestScrollViewport(t *testing.T) {
	t.Run("ScrollDown and ScrollUp", func(t *testing.T) {
		var vp scrollViewport
		assert.Equal(t, 0, vp.Selected())

		vp.ScrollDown(5)
		assert.Equal(t, 1, vp.Selected())

		vp.ScrollDown(5)
		vp.ScrollDown(5)
		vp.ScrollDown(5)
		assert.Equal(t, 4, vp.Selected())

		// Can't go past total-1.
		vp.ScrollDown(5)
		assert.Equal(t, 4, vp.Selected())

		vp.ScrollUp()
		assert.Equal(t, 3, vp.Selected())

		// Scroll all the way up.
		for i := 0; i < 10; i++ {
			vp.ScrollUp()
		}
		assert.Equal(t, 0, vp.Selected())
	})

	t.Run("ClampToTotal", func(t *testing.T) {
		vp := scrollViewport{cursor: 10, viewStart: 8}
		vp.ClampToTotal(5)
		assert.Equal(t, 4, vp.Selected())
		assert.Equal(t, 4, vp.viewStart)

		vp.ClampToTotal(0)
		assert.Equal(t, 0, vp.Selected())
		assert.Equal(t, 0, vp.viewStart)
	})

	t.Run("VisibleRange", func(t *testing.T) {
		vp := scrollViewport{cursor: 0, viewStart: 0}
		start, end := vp.VisibleRange(3, 10)
		assert.Equal(t, 0, start)
		assert.Equal(t, 3, end)

		vp.viewStart = 8
		start, end = vp.VisibleRange(3, 10)
		assert.Equal(t, 8, start)
		assert.Equal(t, 10, end)
	})

	t.Run("AdjustViewport keeps cursor visible", func(t *testing.T) {
		vp := scrollViewport{cursor: 5, viewStart: 0}
		vp.AdjustViewport(3, 10)
		// cursor=5 should be visible, so viewStart should be at least 3
		start, end := vp.VisibleRange(3, 10)
		assert.True(t, vp.Selected() >= start && vp.Selected() < end,
			"cursor %d should be in range [%d, %d)", vp.Selected(), start, end)
	})
}

func TestProcessSortResult(t *testing.T) {
	m := &Model{}

	// nil sortChoice is a no-op.
	m.processSortResult()
	assert.Equal(t, SortPriority, m.list.sortBy)

	// Set to status.
	s := "status"
	m.sortChoice = &s
	m.processSortResult()
	assert.Equal(t, SortStatus, m.list.sortBy)
	assert.Nil(t, m.sortChoice)

	// Set to updated.
	u := "updated"
	m.sortChoice = &u
	m.processSortResult()
	assert.Equal(t, SortUpdatedAt, m.list.sortBy)

	// Set to priority.
	p := "priority"
	m.sortChoice = &p
	m.processSortResult()
	assert.Equal(t, SortPriority, m.list.sortBy)
}

func TestRenderBeadRowNoBulk(t *testing.T) {
	m := &Model{width: 120}
	b := Bead{ID: "Forge-r1", Title: "Normal row", Priority: 2, Status: "open", Anvil: "heimdall"}
	row := m.renderBeadRow(b, 30, false)
	assert.Contains(t, row, "Forge-r1")
	assert.NotContains(t, row, "[ ]", "no checkbox when bulk selection is inactive")
	assert.NotContains(t, row, "[✓]", "no checkbox when bulk selection is inactive")
}

func TestRenderBeadRowBulkUnchecked(t *testing.T) {
	m := &Model{width: 120}
	// Select a *different* bead so bulk mode is active but this bead is not checked.
	m.bulk.Toggle("other-bead")
	b := Bead{ID: "Forge-r2", Title: "Unchecked row", Priority: 2, Status: "open"}
	row := m.renderBeadRow(b, 30, false)
	assert.Contains(t, row, "[ ]", "unchecked checkbox shown when bulk mode active and bead not selected")
	assert.Contains(t, row, "Forge-r2")
}

func TestRenderBeadRowBulkChecked(t *testing.T) {
	m := &Model{width: 120}
	m.bulk.Toggle("Forge-r3")
	b := Bead{ID: "Forge-r3", Title: "Checked row", Priority: 1, Status: "in_progress"}
	row := m.renderBeadRow(b, 30, false)
	assert.Contains(t, row, "[✓]", "checked checkbox shown when bead is selected in bulk mode")
	assert.Contains(t, row, "Forge-r3")
}
