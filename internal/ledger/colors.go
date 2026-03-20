package ledger

import "github.com/charmbracelet/lipgloss"

// Adaptive color palette — each pair auto-switches between dark and light
// terminal backgrounds via lipgloss.AdaptiveColor (dark value / light value).
var (
	colorAccent    = lipgloss.AdaptiveColor{Dark: "208", Light: "166"}
	colorSuccess   = lipgloss.AdaptiveColor{Dark: "82", Light: "28"}
	colorWarning   = lipgloss.AdaptiveColor{Dark: "226", Light: "136"}
	colorDanger    = lipgloss.AdaptiveColor{Dark: "196", Light: "160"}
	colorInfo      = lipgloss.AdaptiveColor{Dark: "75", Light: "26"}
	colorMuted     = lipgloss.AdaptiveColor{Dark: "240", Light: "243"}
	colorFg        = lipgloss.AdaptiveColor{Dark: "255", Light: "16"}
	colorSubtle    = lipgloss.AdaptiveColor{Dark: "245", Light: "240"}
	colorPink      = lipgloss.AdaptiveColor{Dark: "213", Light: "127"}
	colorBlue      = lipgloss.AdaptiveColor{Dark: "33", Light: "20"}
	colorCyan      = lipgloss.AdaptiveColor{Dark: "51", Light: "30"}
	colorMagenta   = lipgloss.AdaptiveColor{Dark: "201", Light: "90"}
	colorSkyBlue   = lipgloss.AdaptiveColor{Dark: "117", Light: "25"}
	colorOrangeAlt = lipgloss.AdaptiveColor{Dark: "214", Light: "166"}
	colorBlueCyan  = lipgloss.AdaptiveColor{Dark: "39", Light: "27"}
	colorGreen     = lipgloss.AdaptiveColor{Dark: "42", Light: "22"}
)
