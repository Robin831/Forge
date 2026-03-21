package ledger

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildHierarchyTreeEmpty(t *testing.T) {
	roots := buildHierarchyTree(nil)
	assert.Empty(t, roots)
}

func TestBuildHierarchyTreeFlat(t *testing.T) {
	beads := []Bead{
		{ID: "a", Status: "open"},
		{ID: "b", Status: "in_progress"},
		{ID: "c", Status: "closed"},
	}
	roots := buildHierarchyTree(beads)
	// No parent-child relationships: all are roots.
	assert.Len(t, roots, 3)
	for _, r := range roots {
		assert.Empty(t, r.Children)
		assert.Equal(t, 0, r.Depth)
	}
}

func TestBuildHierarchyTreeParentChild(t *testing.T) {
	beads := []Bead{
		{ID: "parent", Status: "open", Blocks: []string{"child1", "child2"}},
		{ID: "child1", Status: "closed"},
		{ID: "child2", Status: "open"},
	}
	roots := buildHierarchyTree(beads)
	require.Len(t, roots, 1)
	parent := roots[0]
	assert.Equal(t, "parent", parent.Bead.ID)
	assert.Len(t, parent.Children, 2)
	assert.Equal(t, 1, parent.Children[0].Depth)

	// Progress: 1 of 2 children closed → "1/2"
	assert.Equal(t, "1/2", parent.Progress)
}

func TestBuildHierarchyTreeOrphanNotChild(t *testing.T) {
	beads := []Bead{
		{ID: "parent", Status: "open", Blocks: []string{"child"}},
		{ID: "child", Status: "open"},
		{ID: "orphan", Status: "open"},
	}
	roots := buildHierarchyTree(beads)
	// parent + orphan are roots; child is not.
	require.Len(t, roots, 2)
	ids := []string{roots[0].Bead.ID, roots[1].Bead.ID}
	assert.Contains(t, ids, "parent")
	assert.Contains(t, ids, "orphan")
	assert.NotContains(t, ids, "child")
}

func TestBuildHierarchyTreeMissingChild(t *testing.T) {
	// Blocks references a bead that doesn't exist in the list.
	beads := []Bead{
		{ID: "parent", Status: "open", Blocks: []string{"missing"}},
	}
	roots := buildHierarchyTree(beads)
	require.Len(t, roots, 1)
	// Missing child is not added to the tree.
	assert.Empty(t, roots[0].Children)
}

func TestFlattenTreeNoExpand(t *testing.T) {
	beads := []Bead{
		{ID: "parent", Status: "open", Blocks: []string{"child"}},
		{ID: "child", Status: "open"},
	}
	roots := buildHierarchyTree(beads)
	byID := map[string]*Bead{"parent": &beads[0], "child": &beads[1]}

	// Not expanded: only the root is visible.
	flat := flattenTree(roots, map[string]bool{}, byID)
	require.Len(t, flat, 1)
	assert.Equal(t, "parent", flat[0].bead.ID)
	assert.True(t, flat[0].hasChildren)
	assert.False(t, flat[0].isExpanded)
}

func TestFlattenTreeExpanded(t *testing.T) {
	beads := []Bead{
		{ID: "parent", Status: "open", Blocks: []string{"child"}},
		{ID: "child", Status: "open"},
	}
	roots := buildHierarchyTree(beads)
	byID := map[string]*Bead{"parent": &beads[0], "child": &beads[1]}

	flat := flattenTree(roots, map[string]bool{"parent": true}, byID)
	require.Len(t, flat, 2)
	assert.Equal(t, "parent", flat[0].bead.ID)
	assert.True(t, flat[0].isExpanded)
	assert.Equal(t, "child", flat[1].bead.ID)
}

func TestFlattenTreeDepItems(t *testing.T) {
	depBead := Bead{ID: "dep", Status: "open"}
	beads := []Bead{
		{ID: "a", Status: "open", DependsOn: []string{"dep"}},
		depBead,
	}
	roots := buildHierarchyTree(beads)
	byID := map[string]*Bead{"a": &beads[0], "dep": &beads[1]}

	// "dep" is a root (not a child via Blocks), "a" is also a root.
	// Expand "a" to see its dep items.
	flat := flattenTree(roots, map[string]bool{"a": true}, byID)

	// Should see "a" then its dep sub-item.
	depItems := 0
	for _, item := range flat {
		if item.isDep {
			depItems++
			assert.Equal(t, "dep", item.depID)
			assert.NotNil(t, item.depBead)
		}
	}
	assert.Equal(t, 1, depItems)
}

func TestHierarchyStatusIcon(t *testing.T) {
	assert.Equal(t, "✓", hierarchyStatusIcon(&Bead{Status: "closed"}))
	assert.Equal(t, "•", hierarchyStatusIcon(&Bead{Status: "in_progress"}))
	assert.Equal(t, "○", hierarchyStatusIcon(&Bead{Status: "open"}))
	// Open bead with DependsOn → blocked icon.
	assert.Equal(t, "✗", hierarchyStatusIcon(&Bead{Status: "open", DependsOn: []string{"dep"}}))
	// Unknown status falls back to open icon.
	assert.Equal(t, "○", hierarchyStatusIcon(&Bead{Status: "unknown"}))
}

func TestUpdateHierarchyQuit(t *testing.T) {
	m := &Model{view: ViewHierarchy}
	m.hierarchy.expanded = make(map[string]bool)
	cmd := m.updateHierarchy(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	require.NotNil(t, cmd, "q should return a tea.Quit command")
	msg := cmd()
	assert.Equal(t, tea.Quit(), msg)
}

func TestUpdateHierarchyTabSwitchesToList(t *testing.T) {
	m := &Model{view: ViewHierarchy}
	m.hierarchy.expanded = make(map[string]bool)
	m.updateHierarchy(tabKeyMsg())
	assert.Equal(t, ViewList, m.view)
}

func TestUpdateHierarchyNavigation(t *testing.T) {
	m := &Model{
		view:   ViewHierarchy,
		width:  80,
		height: 24,
		beads: []Bead{
			{ID: "a", Status: "open"},
			{ID: "b", Status: "open"},
			{ID: "c", Status: "open"},
		},
	}
	m.hierarchy.expanded = make(map[string]bool)
	m.refreshHierarchy()
	require.Len(t, m.hierarchy.flat, 3)

	assert.Equal(t, 0, m.hierarchy.vp.Selected())

	m.updateHierarchy(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	assert.Equal(t, 1, m.hierarchy.vp.Selected())

	m.updateHierarchy(tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, 2, m.hierarchy.vp.Selected())

	m.updateHierarchy(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	assert.Equal(t, 1, m.hierarchy.vp.Selected())

	m.updateHierarchy(tea.KeyMsg{Type: tea.KeyUp})
	assert.Equal(t, 0, m.hierarchy.vp.Selected())
}

func TestUpdateHierarchyExpandCollapse(t *testing.T) {
	m := &Model{
		view:   ViewHierarchy,
		width:  80,
		height: 24,
		beads: []Bead{
			{ID: "parent", Status: "open", Blocks: []string{"child"}},
			{ID: "child", Status: "open"},
		},
	}
	m.hierarchy.expanded = make(map[string]bool)
	m.refreshHierarchy()
	require.Len(t, m.hierarchy.flat, 1, "only root visible before expand")

	// Press Enter to expand.
	m.updateHierarchy(tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, m.hierarchy.expanded["parent"])
	assert.Len(t, m.hierarchy.flat, 2, "root + child visible after expand")

	// Press Enter again to collapse.
	m.updateHierarchy(tea.KeyMsg{Type: tea.KeyEnter})
	assert.False(t, m.hierarchy.expanded["parent"])
	assert.Len(t, m.hierarchy.flat, 1, "only root visible after collapse")
}

func TestRenderHierarchyRowLeaf(t *testing.T) {
	m := &Model{width: 80, height: 24}
	item := flatItem{
		bead:        &Bead{ID: "Forge-abc1", Title: "Fix the bug", Status: "open"},
		depth:       0,
		hasChildren: false,
		isExpanded:  false,
	}
	row := m.renderHierarchyRow(item, false)
	assert.Contains(t, row, "Forge-abc1")
	assert.Contains(t, row, "Fix the bug")
}

func TestRenderHierarchyRowSelected(t *testing.T) {
	m := &Model{width: 80, height: 24}
	item := flatItem{
		bead:        &Bead{ID: "Forge-abc1", Title: "Fix the bug", Status: "in_progress"},
		depth:       0,
		hasChildren: true,
		isExpanded:  true,
		progress:    "2/3",
	}
	row := m.renderHierarchyRow(item, true)
	assert.Contains(t, row, "Forge-abc1")
	assert.Contains(t, row, "2/3")
}

func TestRenderHierarchyRowDepItem(t *testing.T) {
	depBead := &Bead{ID: "dep-1", Title: "Dependency", Status: "closed"}
	m := &Model{width: 80, height: 24}
	item := flatItem{
		depth:   1,
		isDep:   true,
		depID:   "dep-1",
		depBead: depBead,
	}
	row := m.renderHierarchyRow(item, false)
	assert.Contains(t, row, "dep-1")
	assert.Contains(t, row, "→")
}

func TestRenderHierarchyRowDepItemNilBead(t *testing.T) {
	m := &Model{width: 80, height: 24}
	item := flatItem{
		depth: 1,
		isDep: true,
		depID: "dep-missing",
	}
	row := m.renderHierarchyRow(item, false)
	assert.Contains(t, row, "dep-missing")
}

func TestRefreshHierarchy(t *testing.T) {
	m := &Model{
		beads: []Bead{
			{ID: "parent", Status: "open", Blocks: []string{"child"}},
			{ID: "child", Status: "closed"},
		},
	}
	m.refreshHierarchy()
	assert.Len(t, m.hierarchy.nodes, 1)
	assert.Len(t, m.hierarchy.flat, 1, "child not visible until expanded")
	assert.NotNil(t, m.hierarchy.expanded)
}
