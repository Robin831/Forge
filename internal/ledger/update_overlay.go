package ledger

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/Robin831/Forge/internal/depupdate"
)

// updateFilterChoice controls which update groups are included when applying.
type updateFilterChoice int

const (
	updateFilterAll          updateFilterChoice = iota // apply all updates
	updateFilterPatchMinor                             // skip major version bumps
	updateFilterSelectGroups                           // let user pick groups individually
	updateFilterCancel                                 // close without applying
)

// updateScanDoneMsg is delivered when the background depupdate.Scan call completes.
// generation matches m.updateScanGeneration at the time the scan started; stale
// results (generation mismatch) are discarded.
type updateScanDoneMsg struct {
	reports    []depupdate.AnvilReport
	err        error
	generation int
}

// updateApplyDoneMsg is delivered when dependency updates have been applied across all anvils.
type updateApplyDoneMsg struct {
	applied         int             // groups successfully installed, verified, and committed
	failed          int             // groups that failed or were rolled back
	skipped         int             // groups excluded by the filter
	anvils          int             // distinct anvils with at least one applied group
	appliedPackages map[string]bool // package paths that were successfully applied
	prErrors        []string        // per-anvil PR creation errors (non-fatal; updates were applied but PR failed)
}

// depBeadsCloseDoneMsg is delivered when the dep-bead bulk close completes.
type depBeadsCloseDoneMsg struct {
	closed int
	failed int
}

// openUpdateOverlay opens the update overlay and kicks off a background scan.
// Returns a Cmd that starts depupdate.Scan or emits an error toast when no
// anvils are configured.
func (m *Model) openUpdateOverlay() tea.Cmd {
	anvils := m.buildDepUpdateAnvils()
	if len(anvils) == 0 {
		return m.addToast("No anvils configured for dependency updates", true)
	}
	m.showUpdateOverlay = true
	m.updateScanning = true
	m.updateRunning = false
	m.updateReports = nil
	m.updateFilterForm = nil
	m.updateFilterKind = updateFilterAll
	m.updateGroupSelectForm = nil
	m.updateSelectedKeys = nil
	m.depBeadCloseForm = nil
	m.depBeadsToClose = nil
	m.depBeadCloseConfirm = false
	m.updateScanGeneration++
	return m.runUpdateScan(anvils)
}

// closeUpdateOverlay resets all update overlay state.
func (m *Model) closeUpdateOverlay() {
	m.showUpdateOverlay = false
	m.updateScanning = false
	m.updateRunning = false
	m.updateReports = nil
	m.updateFilterForm = nil
	m.updateGroupSelectForm = nil
	m.updateSelectedKeys = nil
	m.depBeadCloseForm = nil
	m.depBeadsToClose = nil
	m.depBeadCloseConfirm = false
}

// buildDepUpdateAnvils constructs a []depupdate.Anvil from m.anvils, using
// the real per-anvil config from m.anvilConfigs so updates respect settings
// like GoRaceDetection and GolangciLint from forge.yaml.
func (m *Model) buildDepUpdateAnvils() []depupdate.Anvil {
	if len(m.anvils) == 0 {
		return nil
	}
	names := make([]string, 0, len(m.anvils))
	for name := range m.anvils {
		names = append(names, name)
	}
	sort.Strings(names)

	anvils := make([]depupdate.Anvil, 0, len(m.anvils))
	for _, name := range names {
		cfg := m.anvilConfigs[name] // zero-value if not present (safe default)
		anvils = append(anvils, depupdate.Anvil{
			Name:   name,
			Path:   m.anvils[name],
			Config: cfg,
			DB:     m.db,
		})
	}
	return anvils
}

// runUpdateScan runs depupdate.Scan in a background goroutine and delivers the
// result as an updateScanDoneMsg.
func (m *Model) runUpdateScan(anvils []depupdate.Anvil) tea.Cmd {
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
// filter controls which groups to include; it is ignored when selectedKeys is
// non-nil, in which case only groups whose key is present are applied.
func runUpdateApply(reports []depupdate.AnvilReport, filter updateFilterChoice, selectedKeys map[string]bool) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		opts := depupdate.Options{}
		if filter == updateFilterPatchMinor {
			opts.NoMajor = true
		}

		applied, failed, skipped, anvilsUpdated := 0, 0, 0, 0
		appliedPackages := make(map[string]bool)
		var prErrors []string
		for _, report := range reports {
			var groups []depupdate.UpdateGroup
			if selectedKeys != nil {
				for _, g := range report.Groups {
					if selectedKeys[groupKey(report.Anvil.Name, g.Name)] {
						groups = append(groups, g)
					} else {
						skipped++
					}
				}
			} else {
				all := depupdate.FilterGroups(report.Groups, depupdate.Options{})
				groups = depupdate.FilterGroups(report.Groups, opts)
				skipped += len(all) - len(groups)
			}
			if len(groups) == 0 {
				continue
			}

			// Step 1: Checkout (or create) the batch-update branch so that
			// commits land on a dedicated branch rather than main.
			branch, err := depupdate.CheckoutUpdateBranch(ctx, report.Anvil.Path)
			if err != nil {
				failed += len(groups)
				continue
			}

			// Step 2: Install, verify (Temper), and commit each group.
			results, err := depupdate.Apply(ctx, report.Anvil.Path, report.Anvil.Config, groups)
			if err != nil {
				failed += len(groups)
				continue
			}
			var appliedGroups []depupdate.UpdateGroup
			for _, r := range results {
				if r.Applied {
					applied++
					appliedGroups = append(appliedGroups, r.Group)
					for _, u := range r.Group.Updates {
						appliedPackages[u.Path] = true
					}
				} else {
					failed++
				}
			}
			if len(appliedGroups) == 0 {
				continue
			}
			anvilsUpdated++

			// Step 3: Generate and commit a changelog fragment (non-fatal on error).
			isBilingual := depupdate.DetectBilingual(report.Anvil.Path)
			_ = depupdate.GenerateChangelog(report.Anvil.Path, appliedGroups, isBilingual)

			// Step 4: Push the branch and open a GitHub PR.
			_, err = depupdate.CreatePR(ctx, report.Anvil.Path, report.Anvil.Name, branch, appliedGroups)
			if err != nil {
				prErrors = append(prErrors, fmt.Sprintf("%s: %v", report.Anvil.Name, err))
			}
		}
		return updateApplyDoneMsg{applied: applied, failed: failed, skipped: skipped, anvils: anvilsUpdated, appliedPackages: appliedPackages, prErrors: prErrors}
	}
}

// buildGroupSelectForm builds a multi-select huh form for picking individual
// update groups to apply.
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

// buildUpdateFilterForm builds the filter selection form shown after a scan completes.
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

// depBeadPackage extracts the package path from a dep-update bead title.
// Expected format: "Deps(<Ecosystem>): update <package> <old> → <new>"
// Returns empty string when the title does not match.
func depBeadPackage(title string) string {
	const marker = ": update "
	idx := strings.Index(title, marker)
	if idx < 0 {
		return ""
	}
	rest := title[idx+len(marker):]
	if i := strings.IndexByte(rest, ' '); i > 0 {
		return rest[:i]
	}
	return rest
}

// findOpenDepBeads returns open beads that appear to be depcheck-created update beads.
// These are identified by IssueType "chore" and a title prefix of "Deps(" —
// the format emitted by depcheck.BeadTitle.
//
// When appliedPackages is non-nil, only beads whose package path appears in
// the map are returned, limiting the close offer to beads actually resolved by
// the just-applied updates.
func (m *Model) findOpenDepBeads(appliedPackages map[string]bool) []Bead {
	var result []Bead
	for _, b := range m.beads {
		if b.Status == "closed" {
			continue
		}
		if b.IssueType != "chore" || !strings.HasPrefix(b.Title, "Deps(") {
			continue
		}
		if appliedPackages != nil {
			pkg := depBeadPackage(b.Title)
			if !appliedPackages[pkg] {
				continue
			}
		}
		result = append(result, b)
	}
	return result
}

// buildDepBeadCloseForm builds a confirm form offering to bulk-close dep update beads.
func buildDepBeadCloseForm(depBeads []Bead, confirm *bool) *huh.Form {
	return huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(fmt.Sprintf("Close %d open dep update bead(s) now resolved?", len(depBeads))).
				Affirmative("Yes, close them").
				Negative("No, keep open").
				Value(confirm),
		),
	).WithTheme(huh.ThemeCharm()).WithWidth(60)
}

// closeDepBeadsCmd closes a set of dep beads across their respective anvils.
// The context timeout scales with the number of beads (15 s each, min 30 s)
// so a long bead list does not hit the deadline mid-loop.
func closeDepBeadsCmd(anvils map[string]string, depBeads []Bead) tea.Cmd {
	return func() tea.Msg {
		timeout := max(time.Duration(len(depBeads))*15*time.Second, 30*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		closed, failed := 0, 0
		for _, b := range depBeads {
			anvilPath, ok := anvils[b.Anvil]
			if !ok {
				failed++
				continue
			}
			_, err := bdExec(ctx, anvilPath, "close", b.ID, "--reason", "Resolved by dependency update")
			if err != nil {
				failed++
			} else {
				closed++
			}
		}
		return depBeadsCloseDoneMsg{closed: closed, failed: failed}
	}
}

// updateUpdateOverlay handles key events when the update overlay is active.
// Returns the updated model and any command to dispatch.
func (m *Model) updateUpdateOverlay(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.closeUpdateOverlay()
		return m, nil
	}

	// While scanning or applying, absorb all keys except esc (handled above).
	if m.updateScanning || m.updateRunning {
		return m, nil
	}

	// Drive the dep-bead close confirmation form.
	if m.depBeadCloseForm != nil {
		cmd := m.driveHuhForm(&m.depBeadCloseForm, msg)
		if m.depBeadCloseForm.State == huh.StateCompleted {
			confirm := m.depBeadCloseConfirm
			beads := m.depBeadsToClose
			m.closeUpdateOverlay()
			if confirm && len(beads) > 0 {
				return m, tea.Batch(cmd, closeDepBeadsCmd(m.anvils, beads))
			}
			return m, cmd
		} else if m.depBeadCloseForm.State == huh.StateAborted {
			m.closeUpdateOverlay()
			return m, cmd
		}
		if isTerminalMsg(msg) {
			return m, cmd
		}
		return m, nil
	}

	// Drive the group multi-select form (shown when "select groups" was chosen).
	if m.updateGroupSelectForm != nil {
		cmd := m.driveHuhForm(&m.updateGroupSelectForm, msg)
		if m.updateGroupSelectForm.State == huh.StateCompleted {
			keys := m.updateSelectedKeys
			m.updateGroupSelectForm = nil
			if len(keys) == 0 {
				m.closeUpdateOverlay()
				return m, tea.Batch(cmd, m.addToast("No groups selected", false))
			}
			keySet := make(map[string]bool, len(keys))
			for _, k := range keys {
				keySet[k] = true
			}
			m.updateRunning = true
			startToast := m.addToast("Applying dependency updates...", false)
			applyCmd := runUpdateApply(m.updateReports, updateFilterAll, keySet)
			return m, tea.Batch(cmd, startToast, applyCmd)
		} else if m.updateGroupSelectForm.State == huh.StateAborted {
			m.closeUpdateOverlay()
			return m, cmd
		}
		if isTerminalMsg(msg) {
			return m, cmd
		}
		return m, nil
	}

	// Drive the filter selection form.
	if m.updateFilterForm != nil {
		cmd := m.driveHuhForm(&m.updateFilterForm, msg)
		if m.updateFilterForm.State == huh.StateCompleted {
			choice := m.updateFilterKind
			m.updateFilterForm = nil
			switch choice {
			case updateFilterCancel:
				m.closeUpdateOverlay()
				return m, cmd
			case updateFilterSelectGroups:
				m.updateGroupSelectForm = buildGroupSelectForm(m.updateReports, &m.updateSelectedKeys)
				return m, tea.Batch(cmd, m.updateGroupSelectForm.Init())
			default:
				m.updateRunning = true
				startToast := m.addToast("Applying dependency updates...", false)
				applyCmd := runUpdateApply(m.updateReports, choice, nil)
				return m, tea.Batch(cmd, startToast, applyCmd)
			}
		} else if m.updateFilterForm.State == huh.StateAborted {
			m.closeUpdateOverlay()
			return m, cmd
		}
		if isTerminalMsg(msg) {
			return m, cmd
		}
	}
	return m, nil
}

// renderUpdateOverlay renders the dependency update overlay.
func (m *Model) renderUpdateOverlay() string {
	overlayWidth := max(m.width-8, 50)
	overlayHeight := max(m.height-6, 10)

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	dimStyle := lipgloss.NewStyle().Foreground(colorMuted)

	var lines []string
	lines = append(lines, titleStyle.Render("Dependency Updates"))
	lines = append(lines, "")

	switch {
	case m.updateScanning:
		lines = append(lines, dimStyle.Render("  Scanning dependencies across anvils..."))
		lines = append(lines, "")
		lines = append(lines, dimStyle.Render("  Press Esc to close"))

	case m.updateRunning:
		lines = append(lines, dimStyle.Render("  Applying updates... (working in background)"))
		lines = append(lines, "")
		lines = append(lines, dimStyle.Render("  Results will appear as a notification when complete"))

	case m.depBeadCloseForm != nil:
		lines = append(lines, m.depBeadCloseForm.View())

	case m.updateGroupSelectForm != nil:
		lines = append(lines, m.updateGroupSelectForm.View())

	case m.updateFilterForm != nil:
		// Show grouped scan results per anvil, then the filter selection form.
		for _, report := range m.updateReports {
			if len(report.Groups) == 0 {
				if len(report.Errors) > 0 {
					errorStyle := lipgloss.NewStyle().Foreground(colorDanger)
					errParts := make([]string, 0, len(report.Errors))
					for eco, err := range report.Errors {
						errParts = append(errParts, fmt.Sprintf("%s: %s", eco, err.Error()))
					}
					sort.Strings(errParts)
					lines = append(lines, errorStyle.Render(fmt.Sprintf("  %s: scan failed: %s", report.Anvil.Name, strings.Join(errParts, "; "))))
				} else {
					lines = append(lines, dimStyle.Render(fmt.Sprintf("  %s: no updates found", report.Anvil.Name)))
				}
				continue
			}
			lines = append(lines, lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("  %s:", report.Anvil.Name)))
			for _, g := range report.Groups {
				kindStyle := lipgloss.NewStyle().Foreground(colorInfo)
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
		lines = append(lines, m.updateFilterForm.View())
	}

	content := strings.Join(lines, "\n")
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorAccent).
		Padding(1, 2).
		Width(overlayWidth).
		Height(overlayHeight).
		Render(content)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box,
		lipgloss.WithWhitespaceBackground(lipgloss.AdaptiveColor{Dark: "0", Light: "15"}))
}

// isTerminalMsg reports whether msg is a key event that should terminate form
// input propagation. This mirrors the Hearth pattern for huh form driving.
func isTerminalMsg(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "enter", "esc", "ctrl+c":
		return true
	}
	return false
}
