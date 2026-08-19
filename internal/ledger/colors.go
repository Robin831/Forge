package ledger

import "github.com/charmbracelet/lipgloss"

// Adaptive color palette — each pair auto-switches between dark and light
// terminal backgrounds via lipgloss.AdaptiveColor (dark value / light value).
var (
	colorAccent  = lipgloss.AdaptiveColor{Dark: "208", Light: "166"}
	colorWarning = lipgloss.AdaptiveColor{Dark: "226", Light: "136"}
	colorDanger  = lipgloss.AdaptiveColor{Dark: "196", Light: "160"}
	colorInfo    = lipgloss.AdaptiveColor{Dark: "75", Light: "26"}
	colorMuted   = lipgloss.AdaptiveColor{Dark: "240", Light: "243"}
)
