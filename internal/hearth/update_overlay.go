package hearth

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/Robin831/Forge/internal/depupdate"
	"github.com/Robin831/Forge/internal/state"
)

// updateFilterChoice controls which update groups are included when applying.
type updateFilterChoice int

const (
	updateFilterAll          updateFilterChoice = iota // apply all updates
	updateFilterPatchMinor                             // skip major version bumps
	updateFilterSelectGroups                           // let user pick groups individually
	updateFilterCancel                                 // close the overlay without applying
)

// updateScanDoneMsg is delivered when the background depupdate.Scan call completes.
// generation matches the Model.updateScanGeneration value at the time the scan was started;
// the handler discards the message if the generation no longer matches (e.g. overlay was closed).
type updateScanDoneMsg struct {
	reports    []depupdate.AnvilReport
	err        error
	generation int
}

// updateApplyDoneMsg is delivered when dependency updates have been applied across all anvils.
type updateApplyDoneMsg struct {
	applied   int      // groups successfully installed, verified, and committed
	failed    int      // groups that failed or were rolled back
	skipped   int      // groups excluded by the filter (e.g. major groups when patch+minor only)
	anvils    int      // distinct anvils with at least one applied group
	prErrors  []string // per-anvil PR creation errors (non-fatal; updates were applied but PR failed)
}

// openUpdateOverlay opens the update overlay and kicks off a background scan.
// Returns a Cmd that runs depupdate.Scan or emits an error toast when no anvils are configured.
func (m *Model) openUpdateOverlay() tea.Cmd {
	if len(m.UpdateAnvils) == 0 {
		return m.addToast("No anvils configured for dependency updates", true)
	}
	m.updateScanGeneration++
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
	m.updateGroupSelectForm = nil
	m.updateSelectedKeys = nil
	m.updateRunning = false
}

// runUpdateScan runs depupdate.Scan across m.UpdateAnvils in a background goroutine
// and delivers the results as an updateScanDoneMsg.
func (m *Model) runUpdateScan() tea.Cmd {
	anvils := m.UpdateAnvils
	gen := m.updateScanGeneration
	return func() tea.Msg {
		ctx := context.Background()
		reports, err := depupdate.Scan(ctx, anvils, depupdate.Options{})
		return updateScanDoneMsg{reports: reports, err: err, generation: gen}
	}
}

// groupKey returns a stable string key for an update group scoped to an anvil.
func groupKey(anvilName, groupName string) string {
	return anvilName + "/" + groupName
}

// runUpdateApply applies the scanned reports in a background goroutine.
// filter controls which groups to include (all or patch+minor only); it is
// ignored when selectedKeys is non-nil, in which case only groups whose key
// (groupKey(anvilName, groupName)) is present in the set are applied.
// db is optional; when non-nil a synthetic worker record is inserted into the
// state DB so the Hearth Workers panel and Live Activity panel show progress.
func runUpdateApply(reports []depupdate.AnvilReport, filter updateFilterChoice, selectedKeys map[string]bool, db *state.DB) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		opts := depupdate.Options{}
		if filter == updateFilterPatchMinor {
			opts.NoMajor = true
		}

		// Determine a display anvil name for the worker record.
		anvilName := ""
		if len(reports) == 1 {
			anvilName = reports[0].Anvil.Name
		} else if len(reports) > 1 {
			names := make([]string, 0, len(reports))
			for _, r := range reports {
				names = append(names, r.Anvil.Name)
			}
			anvilName = strings.Join(names, ", ")
		}

		// Create a temp log file so the Hearth Live Activity panel can tail it.
		workerID := fmt.Sprintf("depupdate-%d", time.Now().UnixNano())
		var logFile *os.File
		if db != nil {
			var err error
			logFile, err = os.CreateTemp("", "forge-depupdate-*.log")
			if err != nil {
				logFile = nil // proceed without log capture
			}
		}
		logPath := ""
		if logFile != nil {
			logPath = logFile.Name()
		}

		// Helper: write a timestamped line to the log file.
		logLine := func(line string) {
			if logFile != nil {
				fmt.Fprintln(logFile, time.Now().Format("15:04:05")+" "+line)
			}
		}

		// Insert a synthetic worker record so the Workers panel shows progress.
		if db != nil {
			if err := db.InsertWorker(&state.Worker{
				ID:        workerID,
				BeadID:    "",
				Anvil:     anvilName,
				Branch:    "",
				PID:       0, // synthetic depupdate worker; no daemon-managed PID to signal
				Status:    state.WorkerRunning,
				Phase:     "depupdate",
				Title:     "Applying dependency updates",
				StartedAt: time.Now(),
				LogPath:   logPath,
			}); err != nil {
				logLine(fmt.Sprintf("warning: failed to register worker: %v", err))
			}
			if err := db.LogEvent(state.EventDepupdateStarted, "Applying dependency updates for: "+anvilName, "", anvilName); err != nil {
				logLine(fmt.Sprintf("warning: failed to log start event: %v", err))
			}
		}
		logLine("Starting dependency updates for: " + anvilName)

		applied, failed, skipped, anvilsUpdated := 0, 0, 0, 0
		var prErrors []string
		for _, report := range reports {
			var groups []depupdate.UpdateGroup
			anvilSkipped := 0
			if selectedKeys != nil {
				// user-selected groups: ignore filter, pick by key
				for _, g := range report.Groups {
					if selectedKeys[groupKey(report.Anvil.Name, g.Name)] {
						groups = append(groups, g)
					} else {
						anvilSkipped++
					}
				}
			} else {
				all := depupdate.FilterGroups(report.Groups, depupdate.Options{})
				groups = depupdate.FilterGroups(report.Groups, opts)
				anvilSkipped = len(all) - len(groups)
			}
			skipped += anvilSkipped
			if len(groups) == 0 {
				logLine(fmt.Sprintf("[%s] no groups to apply (skipped %d)", report.Anvil.Name, anvilSkipped))
				continue
			}

			// Step 1: Checkout (or create) the batch-update branch so that
			// commits land on a dedicated branch rather than main.
			logLine(fmt.Sprintf("[%s] checking out update branch...", report.Anvil.Name))
			branch, err := depupdate.CheckoutUpdateBranch(ctx, report.Anvil.Path)
			if err != nil {
				logLine(fmt.Sprintf("[%s] branch error: %v", report.Anvil.Name, err))
				failed += len(groups)
				continue
			}
			logLine(fmt.Sprintf("[%s] on branch %s, applying %d group(s)...", report.Anvil.Name, branch, len(groups)))

			// Step 2: Install, verify (Temper), and commit each group.
			results, err := depupdate.Apply(ctx, report.Anvil.Path, report.Anvil.Config, groups)
			if err != nil {
				// Apply-level error (e.g. context cancellation); count all groups as failed.
				logLine(fmt.Sprintf("[%s] apply error: %v", report.Anvil.Name, err))
				failed += len(groups)
				continue
			}
			var appliedGroups []depupdate.UpdateGroup
			for _, r := range results {
				if r.Applied {
					logLine(fmt.Sprintf("[%s] applied: %s", report.Anvil.Name, r.Group.Name))
					applied++
					appliedGroups = append(appliedGroups, r.Group)
				} else {
					logLine(fmt.Sprintf("[%s] failed: %s", report.Anvil.Name, r.Group.Name))
					failed++
				}
			}
			if len(appliedGroups) == 0 {
				continue
			}
			anvilsUpdated++

			// Step 3: Generate and commit a changelog fragment.
			isBilingual := depupdate.DetectBilingual(report.Anvil.Path)
			if err := depupdate.GenerateChangelog(report.Anvil.Path, appliedGroups, isBilingual); err != nil {
				logLine(fmt.Sprintf("[%s] changelog warning: %v", report.Anvil.Name, err))
			}

			// Step 4: Push the branch and open a GitHub PR.
			logLine(fmt.Sprintf("[%s] creating PR...", report.Anvil.Name))
			prURL, err := depupdate.CreatePR(ctx, report.Anvil.Path, report.Anvil.Name, branch, appliedGroups)
			if err != nil {
				logLine(fmt.Sprintf("[%s] PR error: %v", report.Anvil.Name, err))
				prErrors = append(prErrors, fmt.Sprintf("%s: %v", report.Anvil.Name, err))
			} else {
				logLine(fmt.Sprintf("[%s] PR created: %s", report.Anvil.Name, prURL))
			}
		}

		// Update worker status and log the completion event.
		if db != nil {
			if failed > 0 && applied == 0 {
				if err := db.UpdateWorkerStatus(workerID, state.WorkerFailed); err != nil {
					logLine(fmt.Sprintf("warning: failed to update worker status: %v", err))
				}
				if err := db.LogEvent(state.EventDepupdateFailed,
					fmt.Sprintf("Dependency updates failed: %d applied, %d failed, %d skipped", applied, failed, skipped),
					"", anvilName); err != nil {
					logLine(fmt.Sprintf("warning: failed to log failure event: %v", err))
				}
			} else {
				if err := db.UpdateWorkerStatus(workerID, state.WorkerDone); err != nil {
					logLine(fmt.Sprintf("warning: failed to update worker status: %v", err))
				}
				if err := db.LogEvent(state.EventDepupdateCompleted,
					fmt.Sprintf("Dependency updates completed: %d applied, %d failed, %d skipped across %d anvil(s)", applied, failed, skipped, anvilsUpdated),
					"", anvilName); err != nil {
					logLine(fmt.Sprintf("warning: failed to log completion event: %v", err))
				}
			}
		}
		logLine(fmt.Sprintf("Done: %d applied, %d failed, %d skipped", applied, failed, skipped))
		if logFile != nil {
			logFile.Close()
			// Remove the temp file after a short delay so the TUI has time to read
			// the final lines before the file disappears.
			go func() {
				time.Sleep(30 * time.Second)
				os.Remove(logPath)
			}()
		}

		return updateApplyDoneMsg{applied: applied, failed: failed, skipped: skipped, anvils: anvilsUpdated, prErrors: prErrors}
	}
}

// buildGroupSelectForm builds a multi-select huh form that lets the user pick
// individual update groups to apply. selectedKeys is the slice the form writes
// its selected values into.
func buildGroupSelectForm(reports []depupdate.AnvilReport, selectedKeys *[]string) *huh.Form {
	var opts []huh.Option[string]
	for _, report := range reports {
		for _, g := range report.Groups {
			key := groupKey(report.Anvil.Name, g.Name)
			label := fmt.Sprintf("[%s] %s (%s)", report.Anvil.Name, g.Name, g.Kind)
			opts = append(opts, huh.NewOption(label, key))
		}
	}
	return huh.NewForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select groups to apply").
				Options(opts...).
				Value(selectedKeys),
		),
	).WithTheme(huh.ThemeCharm()).WithWidth(70)
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
					huh.NewOption("Select groups...", updateFilterSelectGroups),
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
		lines = append(lines, dimStyle.Render("  Press Esc to close this overlay"))

	case m.updateRunning:
		lines = append(lines, dimStyle.Render("  Applying updates... (working in background)"))
		lines = append(lines, "")
		lines = append(lines, dimStyle.Render("  Results will appear as a notification when complete"))

	case m.updateGroupSelectForm != nil:
		lines = append(lines, m.updateGroupSelectForm.View())

	case m.updateForm != nil:
		// Grouped scan results: one section per anvil, then the selection form.
		for _, report := range m.updateReports {
			if len(report.Groups) == 0 {
				if len(report.Errors) > 0 {
					errorStyle := lipgloss.NewStyle().Foreground(colorDanger)
					lines = append(lines, errorStyle.Render(fmt.Sprintf("  %s: scan failed: %v", report.Anvil.Name, report.Errors)))
				} else {
					lines = append(lines, dimStyle.Render(fmt.Sprintf("  %s: no updates found", report.Anvil.Name)))
				}
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
