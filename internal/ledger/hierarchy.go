package ledger

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// maxTreeDepth caps recursion depth to guard against cycles in bead data.
const maxTreeDepth = 20

// hierarchyTitleOffset is the total fixed-width chrome consumed per hierarchy row:
// expArrow(1) + space(1) + icon(1) + space(1) + id(colID=14) + two-spaces(2) + style-padding(4) + progress-margin(10).
const hierarchyTitleOffset = 34

// TreeNode represents a node in the bead hierarchy tree.
type TreeNode struct {
	Bead     *Bead
	Depth    int
	Children []*TreeNode
	Progress string // e.g. "3/5" (closed/total children); empty for leaf nodes
}

// flatItem is a single displayable row in the flattened hierarchy list.
type flatItem struct {
	bead        *Bead  // nil for dep-arrow rows
	depth       int
	isDep       bool   // true if this is a depends_on sub-item
	depID       string // dependency bead ID (when isDep=true)
	depBead     *Bead  // resolved dep bead, may be nil (when isDep=true)
	hasChildren bool   // true if this bead has any Blocks entries
	isExpanded  bool   // true if currently expanded
	progress    string // pre-computed "3/5" string for parent nodes
}

// hierarchyState holds all state for the hierarchy tree view.
type hierarchyState struct {
	vp       scrollViewport
	expanded map[string]bool
	nodes    []*TreeNode // root nodes
	flat     []flatItem  // flattened visible item list
}

// buildHierarchyTree constructs root TreeNodes from a flat bead slice.
// Root nodes = beads with Blocks (parents) + orphan beads (not a child of anyone).
func buildHierarchyTree(beads []Bead) []*TreeNode {
	byID := make(map[string]*Bead, len(beads))
	for i := range beads {
		byID[beads[i].ID] = &beads[i]
	}

	// Track which IDs appear as children in someone else's Blocks list.
	isChild := make(map[string]bool)
	for i := range beads {
		for _, childID := range beads[i].Blocks {
			isChild[childID] = true
		}
	}

	// Build parent → children index.
	childrenOf := make(map[string][]*Bead)
	for i := range beads {
		for _, childID := range beads[i].Blocks {
			if child, ok := byID[childID]; ok {
				childrenOf[beads[i].ID] = append(childrenOf[beads[i].ID], child)
			}
		}
	}

	var roots []*TreeNode
	for i := range beads {
		b := &beads[i]
		if isChild[b.ID] {
			continue // rendered under its parent
		}
		node := buildNode(b, childrenOf, 0)
		roots = append(roots, node)
	}

	return roots
}

// buildNode recursively constructs a TreeNode, capping depth at maxTreeDepth.
func buildNode(b *Bead, childrenOf map[string][]*Bead, depth int) *TreeNode {
	node := &TreeNode{
		Bead:  b,
		Depth: depth,
	}

	if depth >= maxTreeDepth {
		return node
	}

	children := childrenOf[b.ID]
	if len(children) > 0 {
		node.Children = make([]*TreeNode, 0, len(children))
		closed := 0
		for _, child := range children {
			childNode := buildNode(child, childrenOf, depth+1)
			node.Children = append(node.Children, childNode)
			if child.Status == "closed" {
				closed++
			}
		}
		node.Progress = fmt.Sprintf("%d/%d", closed, len(children))
	}

	return node
}

// flattenTree converts the tree into a flat slice of visible items, respecting
// the expanded set. Dependency sub-items are only shown when the parent is expanded.
func flattenTree(nodes []*TreeNode, expanded map[string]bool, byID map[string]*Bead) []flatItem {
	var items []flatItem
	for _, n := range nodes {
		flattenNode(n, expanded, byID, &items)
	}
	return items
}

func flattenNode(n *TreeNode, expanded map[string]bool, byID map[string]*Bead, items *[]flatItem) {
	isExp := expanded[n.Bead.ID]
	hasKids := len(n.Children) > 0
	hasDeps := len(n.Bead.DependsOn) > 0
	expandable := hasKids || hasDeps

	*items = append(*items, flatItem{
		bead:        n.Bead,
		depth:       n.Depth,
		hasChildren: hasKids,
		isExpanded:  isExp && expandable,
		progress:    n.Progress,
	})

	if !isExp {
		return
	}

	// Render child nodes.
	for _, child := range n.Children {
		flattenNode(child, expanded, byID, items)
	}

	// Render DependsOn entries as indented arrow sub-items.
	if hasDeps {
		for _, depID := range n.Bead.DependsOn {
			dep := byID[depID]
			*items = append(*items, flatItem{
				depth:   n.Depth + 1,
				isDep:   true,
				depID:   depID,
				depBead: dep,
			})
		}
	}
}

// refreshHierarchy rebuilds nodes and flat item list from the current filtered
// bead list and expanded state. Call after beads change or after toggling a node.
func (m *Model) refreshHierarchy() {
	if m.hierarchy.expanded == nil {
		m.hierarchy.expanded = make(map[string]bool)
	}
	filtered := m.filteredBeads()
	byID := make(map[string]*Bead, len(filtered))
	for i := range filtered {
		byID[filtered[i].ID] = &filtered[i]
	}
	m.hierarchy.nodes = buildHierarchyTree(filtered)
	m.hierarchy.flat = flattenTree(m.hierarchy.nodes, m.hierarchy.expanded, byID)
	m.hierarchy.vp.ClampToTotal(len(m.hierarchy.flat))
}

// updateHierarchy handles key messages when the hierarchy view is active.
func (m *Model) updateHierarchy(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "j", "down":
		m.hierarchy.vp.ScrollDown(len(m.hierarchy.flat))
	case "k", "up":
		m.hierarchy.vp.ScrollUp()
	case "enter":
		// Toggle expand/collapse for the focused node.
		idx := m.hierarchy.vp.Selected()
		if idx >= 0 && idx < len(m.hierarchy.flat) {
			item := m.hierarchy.flat[idx]
			if !item.isDep && item.bead != nil {
				expandable := item.hasChildren || len(item.bead.DependsOn) > 0
				if expandable {
					if m.hierarchy.expanded[item.bead.ID] {
						delete(m.hierarchy.expanded, item.bead.ID)
					} else {
						m.hierarchy.expanded[item.bead.ID] = true
					}
					m.refreshHierarchy()
				}
			}
		}
	case "q":
		return tea.Quit
	case "tab":
		m.view = ViewList
	}
	return nil
}

// hierarchyStatusIcon returns the status icon for a bead.
//
//	✓  closed
//	•  in_progress
//	✗  open and has any DependsOn entries (same check as isBlocked)
//	○  open
func hierarchyStatusIcon(b *Bead) string {
	switch b.Status {
	case "closed":
		return "✓"
	case "in_progress":
		return "•"
	case "open":
		if len(b.DependsOn) > 0 {
			return "✗"
		}
		return "○"
	default:
		return "○"
	}
}

// renderHierarchy renders the full hierarchy view.
func (m *Model) renderHierarchy() string {
	total := len(m.hierarchy.flat)

	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Padding(0, 2)
	selNote := ""
	if m.bulk.Count() > 0 {
		selNote = fmt.Sprintf("  [%d selected]", m.bulk.Count())
	}
	header := headerStyle.Render(
		fmt.Sprintf("⚒ Forge Ledger — Hierarchy  %d beads  (Enter: expand/collapse)%s%s", len(m.filteredBeads()), selNote, m.filterHint()),
	)

	// Reserve one line each for header and footer.
	rowsHeight := max(m.height-2, 1)

	m.hierarchy.vp.ClampToTotal(total)
	m.hierarchy.vp.AdjustViewport(rowsHeight, total)
	start, end := m.hierarchy.vp.VisibleRange(rowsHeight, total)

	var rows strings.Builder
	for i := start; i < end; i++ {
		row := m.renderHierarchyRow(m.hierarchy.flat[i], i == m.hierarchy.vp.Selected())
		rows.WriteString(row)
		if i < end-1 {
			rows.WriteByte('\n')
		}
	}

	footerStyle := lipgloss.NewStyle().Foreground(colorMuted).Padding(0, 2)
	var hierarchyFooter string
	if m.bulk.Count() > 0 {
		hierarchyFooter = fmt.Sprintf("%d selected: Ctrl+X close  Ctrl+L label  Ctrl+P priority  Esc: clear  |  j/k: navigate  Space: toggle  Enter: expand/collapse  q: quit", m.bulk.Count())
	} else {
		hierarchyFooter = "j/k: navigate  Space: select  Enter: expand/collapse  f: filter anvil  s: show/hide closed  d: add dep  b: view deps  Tab: list view  q: quit"
	}
	footer := footerStyle.Render(hierarchyFooter)

	return header + "\n" + rows.String() + "\n" + footer
}

// renderHierarchyRow renders a single flat item as a display line.
func (m *Model) renderHierarchyRow(item flatItem, selected bool) string {
	indent := strings.Repeat("  ", item.depth)

	if item.isDep {
		// Dependency sub-item: "  → dep-id  Title"
		label := item.depID
		if item.depBead != nil {
			icon := hierarchyStatusIcon(item.depBead)
			// Fixed chrome before the title: indent(2*depth) + "  → "(4) + icon(1) + " "(1) + padRight(14) + "  "(2) + style padding(4) = 26 + 2*depth
			depTitleWidth := max(m.width-26-2*item.depth, 10)
			label = fmt.Sprintf("%s %s  %s",
				icon,
				padRight(item.depID, 14),
				truncate(item.depBead.Title, depTitleWidth),
			)
		}
		line := indent + "  → " + label
		style := lipgloss.NewStyle().Foreground(colorMuted).Padding(0, 2)
		if selected {
			style = style.Background(lipgloss.AdaptiveColor{Dark: "237", Light: "254"})
		}
		return style.Render(line)
	}

	b := item.bead
	icon := hierarchyStatusIcon(b)

	// Expand/collapse arrow for nodes with children or dependencies.
	expArrow := " "
	if item.hasChildren || len(b.DependsOn) > 0 {
		if item.isExpanded {
			expArrow = "▾"
		} else {
			expArrow = "▸"
		}
	}

	// Progress badge for parent nodes: " [3/5]"
	progress := ""
	if item.progress != "" {
		progress = "  [" + item.progress + "]"
	}

	// Selection checkbox prefix — shown when any bead is selected.
	checkPrefix := ""
	if m.bulk.Count() > 0 {
		if m.bulk.IsSelected(b.ID) {
			checkPrefix = "[✓] "
		} else {
			checkPrefix = "[ ] "
		}
	}

	titleWidth := max(m.width-lipgloss.Width(indent)-hierarchyTitleOffset-lipgloss.Width(checkPrefix), 10)
	line := fmt.Sprintf("%s%s%s %s %s  %s%s",
		checkPrefix,
		indent,
		expArrow,
		icon,
		padRight(b.ID, colID),
		truncate(b.Title, titleWidth),
		progress,
	)

	sc := statusColor(b.Status)
	style := lipgloss.NewStyle().Foreground(sc).Padding(0, 2)
	if selected {
		style = style.
			Bold(true).
			Background(lipgloss.AdaptiveColor{Dark: "237", Light: "254"})
	}
	return style.Render(line)
}
