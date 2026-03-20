package ledger

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// FilterState holds the state for the search/filter system.
type FilterState struct {
	active bool             // true when the text input is focused
	input  textinput.Model  // bubbles text input component
	text   string           // current filter text (mirrors input.Value for real-time filtering)

	// Structured filters (set via future UI or programmatically).
	anvil    string // filter to a specific anvil name (case-insensitive)
	status   string // filter to a specific status
	priority int    // filter to a specific priority (-1 means no filter)
	label    string // filter to beads containing this label (case-insensitive)
}

// newFilterState creates a FilterState with sensible defaults.
func newFilterState() FilterState {
	ti := textinput.New()
	ti.Placeholder = "type to filter..."
	ti.CharLimit = 128
	ti.Width = 30
	return FilterState{
		input:    ti,
		priority: -1, // no priority filter
	}
}

// Matches reports whether the given bead passes all active filter criteria.
// Text search is case-insensitive against ID, title, description, and labels.
func (f *FilterState) Matches(b Bead) bool {
	// Text filter
	if f.text != "" {
		q := strings.ToLower(f.text)
		match := strings.Contains(strings.ToLower(b.ID), q) ||
			strings.Contains(strings.ToLower(b.Title), q) ||
			strings.Contains(strings.ToLower(b.Description), q)
		if !match {
			for _, l := range b.Labels {
				if strings.Contains(strings.ToLower(l), q) {
					match = true
					break
				}
			}
		}
		if !match {
			return false
		}
	}

	// Anvil filter
	if f.anvil != "" && !strings.EqualFold(b.Anvil, f.anvil) {
		return false
	}

	// Status filter
	if f.status != "" && !strings.EqualFold(b.Status, f.status) {
		return false
	}

	// Priority filter
	if f.priority >= 0 && b.Priority != f.priority {
		return false
	}

	// Label filter
	if f.label != "" {
		found := false
		for _, l := range b.Labels {
			if strings.EqualFold(l, f.label) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

// HasActiveFilter reports whether any filter criteria are set.
func (f *FilterState) HasActiveFilter() bool {
	return f.text != "" || f.anvil != "" || f.status != "" || f.priority >= 0 || f.label != ""
}

// Summary returns a short description of active filters for the header indicator.
func (f *FilterState) Summary() string {
	var parts []string
	if f.text != "" {
		parts = append(parts, f.text)
	}
	if f.anvil != "" {
		parts = append(parts, "Anvil: "+f.anvil)
	}
	if f.status != "" {
		parts = append(parts, "Status: "+f.status)
	}
	if f.priority >= 0 {
		parts = append(parts, fmt.Sprintf("Priority: P%d", f.priority))
	}
	if f.label != "" {
		parts = append(parts, "Label: "+f.label)
	}
	return strings.Join(parts, " | ")
}

// FilterBeads returns beads that match all active filter criteria.
func (f *FilterState) FilterBeads(beads []Bead) []Bead {
	if !f.HasActiveFilter() {
		return beads
	}
	var out []Bead
	for _, b := range beads {
		if f.Matches(b) {
			out = append(out, b)
		}
	}
	return out
}

// activate opens the filter text input for editing.
func (f *FilterState) activate() tea.Cmd {
	f.active = true
	f.input.SetValue(f.text)
	f.input.Focus()
	return textinput.Blink
}

// deactivate closes the filter text input, keeping the current filter text.
func (f *FilterState) deactivate() {
	f.active = false
	f.input.Blur()
}

// clear resets all filter state.
func (f *FilterState) clear() {
	f.active = false
	f.text = ""
	f.input.SetValue("")
	f.input.Blur()
	f.anvil = ""
	f.status = ""
	f.priority = -1
	f.label = ""
}

// update handles a key message when the filter input is active.
// Returns a tea.Cmd and whether the message was consumed.
func (f *FilterState) update(msg tea.KeyMsg) (tea.Cmd, bool) {
	if !f.active {
		return nil, false
	}

	switch msg.Type {
	case tea.KeyEsc:
		if f.input.Value() == "" {
			// Esc on empty input clears the filter entirely.
			f.clear()
		} else {
			// Esc with text keeps the filter but closes input.
			f.text = f.input.Value()
			f.deactivate()
		}
		return nil, true
	case tea.KeyEnter:
		// Enter commits the filter and closes input.
		f.text = f.input.Value()
		f.deactivate()
		return nil, true
	}

	// Forward to textinput.
	var cmd tea.Cmd
	f.input, cmd = f.input.Update(msg)
	// Update text in real-time for live filtering.
	f.text = f.input.Value()
	return cmd, true
}
