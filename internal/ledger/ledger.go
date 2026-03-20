// Package ledger provides an interactive TUI for browsing and managing beads
// across all registered Forge anvils.
package ledger

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Robin831/Forge/internal/state"
)

// refreshInterval controls how often the ledger auto-refreshes bead data.
const refreshInterval = 30 * time.Second

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

	anvils map[string]string // name → path
	db     *state.DB
}

// NewModel creates a new Ledger model.
func NewModel(anvils map[string]string, db *state.DB) Model {
	return Model{
		anvils:  anvils,
		db:      db,
		loading: true,
	}
}

// Init schedules the initial data fetch and periodic refresh tick.
func (m Model) Init() tea.Cmd {
	return tea.Batch(tickCmd(), FetchAllBeads(m.anvils, m.db))
}

// Update handles incoming messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case UpdateBeadsMsg:
		m.loading = false
		m.fetching = false
		m.beads = msg.Beads
		m.err = msg.Err

	case tickMsg:
		// Only launch a new fetch if no fetch is already in-flight.
		if m.fetching {
			return m, tickCmd()
		}
		m.fetching = true
		return m, tea.Batch(tickCmd(), FetchAllBeads(m.anvils, m.db))
	}
	return m, nil
}

// View renders the current state.
func (m Model) View() string {
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(colorAccent).
		Padding(1, 2)

	if m.loading {
		return titleStyle.Render("⚒ Forge Ledger — Loading beads...")
	}

	var statusCounts [4]int // open, in_progress, closed, total
	for _, b := range m.beads {
		statusCounts[3]++
		switch b.Status {
		case "open":
			statusCounts[0]++
		case "in_progress":
			statusCounts[1]++
		case "closed":
			statusCounts[2]++
		}
	}

	header := titleStyle.Render("⚒ Forge Ledger")

	countStyle := lipgloss.NewStyle().Padding(0, 2)
	counts := countStyle.Render(fmt.Sprintf(
		"%d beads total  |  %s open  |  %s in progress  |  %s recently closed",
		statusCounts[3],
		lipgloss.NewStyle().Foreground(colorInfo).Render(fmt.Sprintf("%d", statusCounts[0])),
		lipgloss.NewStyle().Foreground(colorWarning).Render(fmt.Sprintf("%d", statusCounts[1])),
		lipgloss.NewStyle().Foreground(colorMuted).Render(fmt.Sprintf("%d", statusCounts[2])),
	))

	var errLine string
	if m.err != nil {
		errLine = lipgloss.NewStyle().
			Foreground(colorDanger).
			Padding(0, 2).
			Render(fmt.Sprintf("Warning: %v", m.err))
	}

	help := lipgloss.NewStyle().
		Foreground(colorMuted).
		Padding(1, 2).
		Render("Press q to quit")

	if errLine != "" {
		return header + "\n" + counts + "\n" + errLine + "\n" + help
	}
	return header + "\n" + counts + "\n" + help
}
