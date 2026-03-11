package hearth

import "github.com/charmbracelet/lipgloss"

// Adaptive color palette — each pair auto-switches between dark and light
// terminal backgrounds via lipgloss.AdaptiveColor (dark value / light value).
//
// Dark values are the original terminal-256 codes tuned for dark backgrounds.
// Light values use darker equivalents that maintain contrast on light backgrounds.
var (
	// colorAccent is the primary accent color (orange) used for headers and focus.
	colorAccent = lipgloss.AdaptiveColor{Dark: "208", Light: "166"}

	// colorSuccess is used for passing/done/ready states (green).
	colorSuccess = lipgloss.AdaptiveColor{Dark: "82", Light: "28"}

	// colorWarning is used for in-progress/unlabeled/reviewing states (yellow).
	colorWarning = lipgloss.AdaptiveColor{Dark: "226", Light: "136"}

	// colorDanger is used for failures, errors, and attention items (red).
	colorDanger = lipgloss.AdaptiveColor{Dark: "196", Light: "160"}

	// colorInfo is used for informational states (blue).
	colorInfo = lipgloss.AdaptiveColor{Dark: "75", Light: "26"}

	// colorMuted is used for borders, dim text, and secondary elements.
	colorMuted = lipgloss.AdaptiveColor{Dark: "240", Light: "243"}

	// colorFg is used for panel titles (near-white on dark, near-black on light).
	colorFg = lipgloss.AdaptiveColor{Dark: "255", Light: "16"}

	// colorSubtle is used for activity panel titles and group headers.
	colorSubtle = lipgloss.AdaptiveColor{Dark: "245", Light: "240"}

	// colorPink is used for review-related items (pink/magenta).
	colorPink = lipgloss.AdaptiveColor{Dark: "213", Light: "127"}

	// colorBlue is used for dispatching/bellows states (blue).
	colorBlue = lipgloss.AdaptiveColor{Dark: "33", Light: "20"}

	// colorCyan is used for temper (bright cyan).
	colorCyan = lipgloss.AdaptiveColor{Dark: "51", Light: "30"}

	// colorMagenta is used for warden (magenta).
	colorMagenta = lipgloss.AdaptiveColor{Dark: "201", Light: "90"}

	// colorSkyBlue is used for schematic (light blue).
	colorSkyBlue = lipgloss.AdaptiveColor{Dark: "117", Light: "25"}

	// colorOrangeAlt is used for crucible (orange variant).
	colorOrangeAlt = lipgloss.AdaptiveColor{Dark: "214", Light: "166"}

	// colorBlueCyan is used for final PR phase in crucible.
	colorBlueCyan = lipgloss.AdaptiveColor{Dark: "39", Light: "27"}

	// colorGreen is used for complete/success state (darker green).
	colorGreen = lipgloss.AdaptiveColor{Dark: "42", Light: "22"}
)
