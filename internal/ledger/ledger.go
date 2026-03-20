// Package ledger provides an interactive TUI for browsing and managing beads
// across all registered Forge anvils.
package ledger

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Robin831/Forge/internal/state"
)

// refreshInterval controls how often the ledger auto-refreshes bead data.
const refreshInterval = 30 * time.Second

// ViewMode determines which screen the Ledger is showing.
type ViewMode int

const (
	ViewList ViewMode = iota
)

// tickMsg triggers a periodic data refresh.
type tickMsg struct{}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return tickMsg{}
	})
}

// Model is the top-level Bubbletea model for the Ledger TUI.
type Model struct {
	beads    []Bead
	loading  bool
	fetching bool // true while a FetchAllBeads call is in-flight
	err      error

	width  int
	height int

	view   ViewMode
	list   listState
	// sortChoice holds the pointer used by the huh sort selector form.
	sortChoice *string

	anvils map[string]string // name → path
	db     *state.DB
}

// NewModel creates a new Ledger model.
func NewModel(anvils map[string]string, db *state.DB) *Model {
	return &Model{
		anvils:  anvils,
		db:      db,
		loading: true,
		view:    ViewList,
	}
}

// Init schedules the initial data fetch and periodic refresh tick.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(tickCmd(), FetchAllBeads(m.anvils, m.db))
}

// Update handles incoming messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Global quit keys (unless sort form is open).
		if m.list.sortForm == nil {
			switch msg.String() {
			case "q", "ctrl+c":
				return m, tea.Quit
			}
		}

		// Delegate to view-specific handler.
		switch m.view {
		case ViewList:
			cmd := m.updateList(msg)
			return m, cmd
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case UpdateBeadsMsg:
		m.loading = false
		m.fetching = false
		m.beads = msg.Beads
		m.err = msg.Err
		m.list.vp.ClampToTotal(len(m.beads))

	case tickMsg:
		if m.fetching {
			return m, tickCmd()
		}
		m.fetching = true
		return m, tea.Batch(tickCmd(), FetchAllBeads(m.anvils, m.db))
	}
	return m, nil
}

// View renders the current state.
func (m *Model) View() string {
	if m.loading {
		titleStyle := lipgloss.NewStyle().
			Bold(true).
			Foreground(colorAccent).
			Padding(1, 2)
		return titleStyle.Render("⚒ Forge Ledger — Loading beads...")
	}

	switch m.view {
	case ViewList:
		return m.renderList()
	default:
		return m.renderList()
	}
}
