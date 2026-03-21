package ledger

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/provider"
)

// aiOverlayState tracks the current state of the AI improvement overlay.
type aiOverlayState int

const (
	aiOverlayNone     aiOverlayState = iota
	aiOverlaySpinner                 // running the AI provider, showing spinner
	aiOverlayApproval                // showing before/after comparison for user approval
)

// aiImprovementResult holds the parsed output from the AI improvement run.
type aiImprovementResult struct {
	Title       string
	Description string
	Complexity  string
	AIEffort    string
}

// aiImprovementDoneMsg is delivered when the AI improvement completes.
type aiImprovementDoneMsg struct {
	result aiImprovementResult
	err    error
}

// aiSpinnerTickMsg drives the spinner animation frame updates.
type aiSpinnerTickMsg struct{}

// spinnerFrames are the Unicode braille spinner characters for the loading animation.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func aiSpinnerTickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg {
		return aiSpinnerTickMsg{}
	})
}

// getAIProvider reads the forge config and returns the first provider from
// SmithProviders (if set) or Providers, defaulting to claude.
func getAIProvider() provider.Provider {
	cfg, err := config.Load("")
	if err != nil || cfg == nil {
		return provider.Provider{Kind: provider.Claude}
	}
	specs := cfg.Settings.SmithProviders
	if len(specs) == 0 {
		specs = cfg.Settings.Providers
	}
	if len(specs) == 0 {
		return provider.Provider{Kind: provider.Claude}
	}
	providers := provider.FromConfig(specs)
	if len(providers) == 0 {
		return provider.Provider{Kind: provider.Claude}
	}
	return providers[0]
}

// runAIImprovementCmd spawns the AI provider to analyze the bead and codebase,
// then returns an improved title, description, complexity, and AI effort estimate.
func runAIImprovementCmd(bead Bead, anvilPath string) tea.Cmd {
	return func() tea.Msg {
		prov := getAIProvider()
		prompt := buildAIImprovementPrompt(bead, anvilPath)

		// Build command args. Use --output-format text for simple section parsing.
		var args []string
		switch prov.Kind {
		case provider.Gemini:
			args = []string{"--yolo", "-o", "text"}
			if prov.Model != "" {
				args = append(args, "--model", prov.Model)
			}
		case provider.Copilot:
			args = []string{"-p", "-", "--yolo", "--output-format", "text", "--no-auto-update"}
			if prov.Model != "" {
				args = append(args, "--model", prov.Model)
			}
		default: // claude and others
			args = []string{"--dangerously-skip-permissions", "-p", "-", "--output-format", "text"}
			if prov.Model != "" {
				args = append(args, "--model", prov.Model)
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		cmd := executil.HideWindow(exec.CommandContext(ctx, prov.Cmd(), args...))
		cmd.Dir = anvilPath
		cmd.Stdin = strings.NewReader(prompt)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			return aiImprovementDoneMsg{err: fmt.Errorf("AI improvement failed: %w\n%s", err, stderr.String())}
		}

		result := parseAIResponse(stdout.String())
		return aiImprovementDoneMsg{result: result}
	}
}

// buildAIImprovementPrompt constructs the prompt sent to the AI provider.
func buildAIImprovementPrompt(bead Bead, anvilPath string) string {
	return fmt.Sprintf(`You are helping improve a software issue (bead) for the Forge AI orchestrator system.

Investigate the codebase at: %s

Current bead details:
- ID: %s
- Title: %s
- Description:
%s

Please analyze the bead and the codebase, then provide an improved version with:
1. A clearer, more specific title
2. A more detailed description that helps an AI agent understand exactly what needs to be done
3. A complexity estimate (low/medium/high)
4. An AI effort estimate — how long an AI coding agent (not a human) would take to complete this task (e.g. "~5 minutes with smith", "~20 minutes with smith")

Format your response EXACTLY as follows (keep the section headers exactly as shown):

### Title
<improved title here>

### Description
<improved description here>

### Complexity
<low|medium|high>

### AI Effort
<time estimate for an AI agent, e.g. "~10 minutes with smith">`, anvilPath, bead.ID, bead.Title, bead.Description)
}

// parseAIResponse extracts the four sections from the AI response.
func parseAIResponse(output string) aiImprovementResult {
	result := aiImprovementResult{}

	type section struct {
		header string
		dest   *string
	}
	sections := []section{
		{"### Title", &result.Title},
		{"### Description", &result.Description},
		{"### Complexity", &result.Complexity},
		{"### AI Effort", &result.AIEffort},
	}

	lines := strings.Split(output, "\n")
	var current *string
	var buf strings.Builder

	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t\r")

		matched := false
		for _, s := range sections {
			if trimmed == s.header {
				if current != nil {
					*current = strings.TrimSpace(buf.String())
				}
				current = s.dest
				buf.Reset()
				matched = true
				break
			}
		}
		if !matched && current != nil {
			buf.WriteString(line)
			buf.WriteString("\n")
		}
	}
	if current != nil {
		*current = strings.TrimSpace(buf.String())
	}

	return result
}

// startAIImprovement initiates the AI improvement workflow for the selected bead.
func (m *Model) startAIImprovement() tea.Cmd {
	b := m.selectedBead()
	if b == nil {
		return nil
	}
	anvilPath, ok := m.anvils[b.Anvil]
	if !ok {
		return func() tea.Msg {
			return ActionErrorMsg{Err: fmt.Errorf("unknown anvil for bead %s: %s", b.ID, b.Anvil)}
		}
	}

	m.aiTarget = b
	m.aiOverlay = aiOverlaySpinner
	m.aiSpinFrame = 0

	return tea.Batch(aiSpinnerTickCmd(), runAIImprovementCmd(*b, anvilPath))
}

// acceptAIImprovement applies the AI's proposed title and description via bd update.
func (m *Model) acceptAIImprovement() tea.Cmd {
	bead := m.aiTarget
	if bead == nil {
		m.aiOverlay = aiOverlayNone
		return nil
	}
	anvilPath, ok := m.anvils[bead.Anvil]
	if !ok {
		m.aiOverlay = aiOverlayNone
		m.aiTarget = nil
		return func() tea.Msg {
			return ActionErrorMsg{Err: fmt.Errorf("unknown anvil for bead %s: %s", bead.ID, bead.Anvil)}
		}
	}

	m.aiOverlay = aiOverlayNone
	m.aiTarget = nil

	return EditBeadCmd(anvilPath, bead.ID, m.aiResult.Title, m.aiResult.Description)
}

// updateAIOverlay handles keyboard input when the AI improvement overlay is active.
func (m *Model) updateAIOverlay(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.aiOverlay = aiOverlayNone
		m.aiTarget = nil
		return m, nil
	}

	if m.aiOverlay == aiOverlayApproval {
		switch msg.String() {
		case "tab", "shift+tab", "left", "right":
			m.aiApprovalFocus = 1 - m.aiApprovalFocus
		case "y", "a":
			return m, m.acceptAIImprovement()
		case "n", "r":
			m.aiOverlay = aiOverlayNone
			m.aiTarget = nil
		case "enter":
			if m.aiApprovalFocus == 0 {
				return m, m.acceptAIImprovement()
			}
			m.aiOverlay = aiOverlayNone
			m.aiTarget = nil
		}
	}
	return m, nil
}

// renderAISpinnerOverlay renders the spinner overlay shown while Claude is running.
func (m *Model) renderAISpinnerOverlay() string {
	frame := spinnerFrames[m.aiSpinFrame%len(spinnerFrames)]

	beadID := ""
	if m.aiTarget != nil {
		beadID = m.aiTarget.ID
	}

	spinStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorAccent).
		Padding(1, 4).
		Width(44)

	content := fmt.Sprintf(
		"%s  Analyzing %s...\n\nInvestigating codebase and\ngenerating improvements",
		frame, beadID,
	)
	overlay := spinStyle.Render(content)

	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, overlay,
		lipgloss.WithWhitespaceBackground(lipgloss.AdaptiveColor{Dark: "0", Light: "15"}))
}

// renderAIApprovalOverlay renders the before/after comparison overlay.
func (m *Model) renderAIApprovalOverlay() string {
	bead := m.aiTarget
	if bead == nil {
		return ""
	}

	result := m.aiResult
	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	mutedStyle := lipgloss.NewStyle().Foreground(colorMuted)

	maxWidth := min(m.width-12, 72)

	var sb strings.Builder
	sb.WriteString(labelStyle.Render(fmt.Sprintf("AI Improvement — %s", bead.ID)))
	sb.WriteString("\n\n")

	// Title comparison
	sb.WriteString(mutedStyle.Render("Before title:"))
	sb.WriteString("\n")
	sb.WriteString("  " + truncate(bead.Title, maxWidth))
	sb.WriteString("\n")
	sb.WriteString(labelStyle.Render("After title:"))
	sb.WriteString("\n")
	sb.WriteString("  " + truncate(result.Title, maxWidth))
	sb.WriteString("\n\n")

	// Description comparison
	sb.WriteString(mutedStyle.Render("Before description:"))
	sb.WriteString("\n")
	sb.WriteString(indentLines(truncateLines(bead.Description, maxWidth, 4), "  "))
	sb.WriteString("\n")
	sb.WriteString(labelStyle.Render("After description:"))
	sb.WriteString("\n")
	sb.WriteString(indentLines(truncateLines(result.Description, maxWidth, 4), "  "))
	sb.WriteString("\n\n")

	// Estimates
	if result.Complexity != "" || result.AIEffort != "" {
		sb.WriteString(mutedStyle.Render("Complexity: "))
		sb.WriteString(result.Complexity)
		if result.AIEffort != "" {
			sb.WriteString("   ")
			sb.WriteString(mutedStyle.Render("AI Effort: "))
			sb.WriteString(result.AIEffort)
		}
		sb.WriteString("\n\n")
	}

	// Accept / Reject buttons
	activeAccept := lipgloss.NewStyle().
		Padding(0, 2).
		Background(lipgloss.Color("2")).
		Foreground(lipgloss.Color("15"))
	activeReject := lipgloss.NewStyle().
		Padding(0, 2).
		Background(lipgloss.Color("1")).
		Foreground(lipgloss.Color("15"))
	inactiveStyle := lipgloss.NewStyle().
		Padding(0, 2).
		Border(lipgloss.NormalBorder()).
		BorderForeground(colorMuted)

	var acceptBtn, rejectBtn string
	if m.aiApprovalFocus == 0 {
		acceptBtn = activeAccept.Render("[ Accept (y) ]")
		rejectBtn = inactiveStyle.Render("  Reject (n)  ")
	} else {
		acceptBtn = inactiveStyle.Render("  Accept (y)  ")
		rejectBtn = activeReject.Render("[ Reject (n) ]")
	}

	sb.WriteString(lipgloss.JoinHorizontal(lipgloss.Center, acceptBtn, "  ", rejectBtn))
	sb.WriteString("\n")
	sb.WriteString(mutedStyle.Render("  tab: switch   enter: confirm   esc: cancel"))

	overlayStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorAccent).
		Padding(1, 2).
		Width(min(maxWidth+8, m.width-4))

	overlay := overlayStyle.Render(sb.String())
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, overlay,
		lipgloss.WithWhitespaceBackground(lipgloss.AdaptiveColor{Dark: "0", Light: "15"}))
}

// truncateLines truncates a multi-line string to at most maxLines lines,
// each line truncated to maxWidth visual columns.
func truncateLines(s string, maxWidth, maxLines int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines = append(lines, "…")
	}
	for i, l := range lines {
		lines[i] = truncate(l, maxWidth)
	}
	return strings.Join(lines, "\n")
}

// indentLines prepends each line of s with prefix.
func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
