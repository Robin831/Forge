package hearth

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/Robin831/Forge/internal/depupdate"
)

// updateFilterChoice controls which update groups are included when applying.
type updateFilterChoice int

const (
	updateFilterAll        updateFilterChoice = iota // apply all updates
	updateFilterPatchMinor                           // skip major version bumps
	updateFilterCancel                               // close the overlay without applying
)

// updateScanDoneMsg is delivered when the background depupdate.Scan call completes.
type updateScanDoneMsg struct {
	reports []depupdate.AnvilReport
	err     error
}

// updateApplyDoneMsg is delivered when dependency updates have been applied across all anvils.
type updateApplyDoneMsg struct {
	applied int // groups successfully installed, verified, and committed
	failed  int // groups that failed or were rolled back
	anvils  int // distinct anvils with at least one applied group
}

// openUpdateOverlay opens the update overlay and kicks off a background scan.
// Returns a Cmd that runs depupdate.Scan or emits an error toast when no anvils are configured.
func (m *Model) openUpdateOverlay() tea.Cmd {
	if len(m.UpdateAnvils) == 0 {
		return m.addToast("No anvils configured for dependency updates", true)
	}
	m.showUpdateOverlay = true
	m.updateScanning = true
	m.updateReports = nil
	m.updateForm = nil
	m.updateFilterKind = updateFilterAll
	m.updateRunning = false
	return m.runUpdateScan()
}

// closeUpdateOverlay resets all update overlay state.
func (m *Model) closeUpdateOverlay() {
	m.showUpdateOverlay = false
	m.updateScanning = false
	m.updateReports = nil
	m.updateForm = nil
	m.updateRunning = false
}

// runUpdateScan runs depupdate.Scan across m.UpdateAnvils in a background goroutine
// and delivers the results as an updateScanDoneMsg.
func (m *Model) runUpdateScan() tea.Cmd {
	anvils := m.UpdateAnvils
	return func() tea.Msg {
		ctx := context.Background()
		reports, err := depupdate.Scan(ctx, anvils, depupdate.Options{})
		return updateScanDoneMsg{reports: reports, err: err}
	}
}

// runUpdateApply applies the scanned reports with the chosen filter in a background goroutine
// and delivers the aggregate result as an updateApplyDoneMsg.
func runUpdateApply(reports []depupdate.AnvilReport, filter updateFilterChoice) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		opts := depupdate.Options{}
		if filter == updateFilterPatchMinor {
			opts.NoMajor = true
		}

		applied, failed, anvilsUpdated := 0, 0, 0
		for _, report := range reports {
			groups := depupdate.FilterGroups(report.Groups, opts)
			if len(groups) == 0 {
				continue
			}
			results, _ := depupdate.Apply(ctx, report.Anvil.Path, report.Anvil.Config, groups)
			anvilApplied := false
			for _, r := range results {
				if r.Applied {
					applied++
					anvilApplied = true
				} else {
					failed++
				}
			}
			if anvilApplied {
				anvilsUpdated++
			}
		}
		return updateApplyDoneMsg{applied: applied, failed: failed, anvils: anvilsUpdated}
	}
}

// countUpdateGroups returns the total number of update groups across all reports.
func countUpdateGroups(reports []depupdate.AnvilReport) int {
	total := 0
	for _, r := range reports {
		total += len(r.Groups)
	}
	return total
}

// countUpdateAnvils returns the number of anvils that have at least one update group.
func countUpdateAnvils(reports []depupdate.AnvilReport) int {
	count := 0
	for _, r := range reports {
		if len(r.Groups) > 0 {
			count++
		}
	}
	return count
}

// buildUpdateFilterForm builds the huh selection form for choosing the update filter.
func buildUpdateFilterForm(choice *updateFilterChoice, totalGroups, totalAnvils int) *huh.Form {
	return huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[updateFilterChoice]().
				Title(fmt.Sprintf("Apply dependency updates — %d groups across %d anvil(s)", totalGroups, totalAnvils)).
				Options(
					huh.NewOption("All updates (patch, minor, major)", updateFilterAll),
					huh.NewOption("Patch + minor only (skip major)", updateFilterPatchMinor),
					huh.NewOption("Cancel", updateFilterCancel),
				).
				Value(choice),
		),
	).WithTheme(huh.ThemeCharm()).WithWidth(60)
}

// renderUpdateOverlay renders the dependency update overlay, showing either a scanning
// message, a running message, or the grouped scan results with the selection form.
func (m *Model) renderUpdateOverlay() string {
	viewerWidth := m.width - 8
	if viewerWidth < 50 {
		viewerWidth = 50
	}
	viewerHeight := m.height - 6
	if viewerHeight < 10 {
		viewerHeight = 10
	}

	var lines []string
	lines = append(lines, actionMenuTitleStyle.Render("Dependency Updates"))
	lines = append(lines, "")

	switch {
	case m.updateScanning:
		lines = append(lines, dimStyle.Render("  Scanning dependencies across anvils..."))
		lines = append(lines, "")
		lines = append(lines, dimStyle.Render("  Press Esc to cancel"))

	case m.updateRunning:
		lines = append(lines, dimStyle.Render("  Applying updates... (working in background)"))
		lines = append(lines, "")
		lines = append(lines, dimStyle.Render("  Results will appear as a notification when complete"))

	case m.updateForm != nil:
		// Grouped scan results: one section per anvil, then the selection form.
		for _, report := range m.updateReports {
			if len(report.Groups) == 0 {
				lines = append(lines, dimStyle.Render(fmt.Sprintf("  %s: no updates found", report.Anvil.Name)))
				continue
			}
			lines = append(lines, lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("  %s:", report.Anvil.Name)))
			for _, g := range report.Groups {
				kindStyle := lipgloss.NewStyle().Foreground(colorSuccess)
				switch g.Kind {
				case "major":
					kindStyle = lipgloss.NewStyle().Foreground(colorDanger)
				case "minor":
					kindStyle = lipgloss.NewStyle().Foreground(colorWarning)
				}
				pkgWord := "pkg"
				if len(g.Updates) != 1 {
					pkgWord = "pkgs"
				}
				kindTag := kindStyle.Render(g.Kind)
				lines = append(lines, fmt.Sprintf("    • %s  [%s, %d %s]", g.Name, kindTag, len(g.Updates), pkgWord))
			}
		}
		lines = append(lines, "")
		lines = append(lines, m.updateForm.View())
	}

	content := strings.Join(lines, "\n")
	return logViewerStyle.Width(viewerWidth).Height(viewerHeight).Render(content)
}
