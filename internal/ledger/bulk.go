package ledger

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// BulkState tracks which beads are currently selected for bulk operations.
type BulkState struct {
	selected map[string]bool
}

// Toggle flips the selection state for the given bead ID.
func (b *BulkState) Toggle(id string) {
	if b.selected == nil {
		b.selected = make(map[string]bool)
	}
	if b.selected[id] {
		delete(b.selected, id)
	} else {
		b.selected[id] = true
	}
}

// SelectAll marks all given beads as selected.
func (b *BulkState) SelectAll(beads []Bead) {
	if b.selected == nil {
		b.selected = make(map[string]bool)
	}
	for _, bead := range beads {
		b.selected[bead.ID] = true
	}
}

// Clear removes all selections.
func (b *BulkState) Clear() {
	b.selected = nil
}

// Count returns the number of selected beads.
func (b *BulkState) Count() int {
	return len(b.selected)
}

// IsSelected reports whether the given bead ID is selected.
func (b *BulkState) IsSelected(id string) bool {
	return b.selected != nil && b.selected[id]
}

// copySelected returns a shallow copy of the selected map, safe to use in a
// command goroutine without racing against the main model.
func (b *BulkState) copySelected() map[string]bool {
	if len(b.selected) == 0 {
		return nil
	}
	cp := make(map[string]bool, len(b.selected))
	for k, v := range b.selected {
		cp[k] = v
	}
	return cp
}

// BulkCloseResultMsg is returned after a bulk close operation completes.
type BulkCloseResultMsg struct {
	Closed int
	Failed int
}

// BulkUpdatedMsg is returned after a bulk update operation (label/priority) completes.
type BulkUpdatedMsg struct {
	Updated int
	Failed  int
}

// BulkCloseCmd closes all selected beads sequentially and returns a summary message.
func BulkCloseCmd(anvils map[string]string, beads []Bead, selectedIDs map[string]bool) tea.Cmd {
	return func() tea.Msg {
		var closed, failed int
		for _, bead := range beads {
			if !selectedIDs[bead.ID] {
				continue
			}
			if bead.Status == "closed" {
				// Already closed — count as success without calling bd.
				closed++
				continue
			}
			anvilPath, ok := anvils[bead.Anvil]
			if !ok {
				failed++
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := bdCloseExec(ctx, anvilPath, "close", bead.ID, "--json")
			cancel()
			if err != nil {
				failed++
			} else {
				closed++
			}
		}
		return BulkCloseResultMsg{Closed: closed, Failed: failed}
	}
}

// BulkLabelCmd adds or removes a label from all selected beads sequentially.
func BulkLabelCmd(anvils map[string]string, beads []Bead, selectedIDs map[string]bool, label string, remove bool) tea.Cmd {
	return func() tea.Msg {
		flag := "--add-label"
		if remove {
			flag = "--remove-label"
		}
		var updated, failed int
		for _, bead := range beads {
			if !selectedIDs[bead.ID] {
				continue
			}
			anvilPath, ok := anvils[bead.Anvil]
			if !ok {
				failed++
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := bdExec(ctx, anvilPath, "update", bead.ID, flag, label)
			cancel()
			if err != nil {
				failed++
			} else {
				updated++
			}
		}
		return BulkUpdatedMsg{Updated: updated, Failed: failed}
	}
}

// BulkPriorityCmd sets the priority of all selected beads sequentially.
func BulkPriorityCmd(anvils map[string]string, beads []Bead, selectedIDs map[string]bool, priority int) tea.Cmd {
	return func() tea.Msg {
		var updated, failed int
		for _, bead := range beads {
			if !selectedIDs[bead.ID] {
				continue
			}
			anvilPath, ok := anvils[bead.Anvil]
			if !ok {
				failed++
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := bdExec(ctx, anvilPath, "update", bead.ID, "--priority", fmt.Sprintf("%d", priority))
			cancel()
			if err != nil {
				failed++
			} else {
				updated++
			}
		}
		return BulkUpdatedMsg{Updated: updated, Failed: failed}
	}
}
