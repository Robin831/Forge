// ledger-test launches the Ledger TUI with dummy data for visual layout testing.
// No bd CLI, database, or daemon required — just pure eye candy.
//
// Usage:
//
//	go run ./cmd/ledger-test
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Robin831/Forge/internal/ledger"
)

// wrapperModel wraps the real ledger.Model and injects mock data after the
// first WindowSizeMsg so that the model has valid dimensions before we send
// beads. It also simulates selecting the first anvil by sending an "enter"
// key press to transition from the anvil picker to the list view.
type wrapperModel struct {
	inner     *ledger.Model
	injected  bool
	sized     bool
	mockBeads []ledger.Bead
}

func (w *wrapperModel) Init() tea.Cmd {
	return w.inner.Init()
}

func (w *wrapperModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Track whether we've received a WindowSizeMsg so the model has dimensions.
	if _, ok := msg.(tea.WindowSizeMsg); ok {
		w.sized = true
	}

	// Forward the message to the real model first.
	updated, cmd := w.inner.Update(msg)
	// The ledger returns *Model from Update; keep our wrapper pointing at it.
	if m, ok := updated.(*ledger.Model); ok {
		w.inner = m
	}

	// After the first WindowSizeMsg, simulate selecting the first anvil
	// (press Enter on the anvil picker) and then inject mock beads.
	if w.sized && !w.injected {
		w.injected = true

		// Simulate pressing Enter to select the first anvil in the picker.
		enterMsg := tea.KeyMsg{Type: tea.KeyEnter}
		updated2, cmd2 := w.inner.Update(enterMsg)
		if m, ok := updated2.(*ledger.Model); ok {
			w.inner = m
		}

		// Now inject our mock beads via UpdateBeadsMsg — this is the same
		// message type that FetchAnvilBeads returns, so the model will handle
		// it identically to real data arriving.
		injectCmd := func() tea.Msg {
			return ledger.UpdateBeadsMsg{Beads: w.mockBeads, Err: nil}
		}

		return w, tea.Batch(cmd, cmd2, injectCmd)
	}

	return w, cmd
}

func (w *wrapperModel) View() string {
	return w.inner.View()
}

// timePtr is a helper that returns a pointer to a time.Time value.
// Why did the Go developer refuse to use raw time literals?
// Because they didn't want to waste any time! ...I'll see myself out.
func timePtr(t time.Time) *time.Time {
	return &t
}

func buildMockBeads() []ledger.Bead {
	now := time.Now()
	yesterday := now.AddDate(0, 0, -1)
	twoDaysAgo := now.AddDate(0, 0, -2)
	lastWeek := now.AddDate(0, 0, -5)

	return []ledger.Bead{
		// --- forge anvil: open beads ---
		{
			ID: "Forge-a1b2", Title: "Pipeline hangs when Smith subprocess exits with signal 9",
			Description: "When the claude CLI process is killed by the OOM killer (signal 9), the pipeline goroutine blocks indefinitely on stdout.Read(). Need to add a context-aware timeout wrapper around the subprocess I/O.\n\nReproduction steps:\n1. Set max memory to 512MB\n2. Run a large bead with many files\n3. Observe the pipeline never completes\n\nExpected: Pipeline detects the dead process and fails gracefully.",
			Status: "open", Priority: 1, IssueType: "bug", Anvil: "forge",
			Labels: []string{"forgeReady", "critical"}, UpdatedAt: timePtr(yesterday),
		},
		{
			ID: "Forge-c3d4", Title: "Add configurable retry backoff for rate-limited API calls",
			Description: "The current retry logic uses a fixed 30-second delay. We should support exponential backoff with jitter, configurable via forge.yaml.",
			Status: "open", Priority: 2, IssueType: "feature", Anvil: "forge",
			Labels: []string{"forgeReady"}, UpdatedAt: timePtr(twoDaysAgo),
		},
		{
			ID: "Forge-e5f6", Title: "Bellows should detect force-pushed branches and re-trigger CI checks automatically",
			Description: "When a force push happens on a monitored PR branch, Bellows currently doesn't notice until the next poll interval. It should detect the push event and immediately re-evaluate CI status.",
			Status: "open", Priority: 2, IssueType: "feature", Anvil: "forge",
			Labels: []string{"enhancement"}, UpdatedAt: timePtr(lastWeek),
		},
		{
			ID: "Forge-g7h8", Title: "Extremely long title that should definitely be truncated in the list view because it contains way too many words for a single line and keeps going and going",
			Description: "This bead exists to test how the UI handles very long titles. The detail panel should word-wrap, while the list view should truncate with an ellipsis.",
			Status: "open", Priority: 3, IssueType: "task", Anvil: "forge",
			Labels: []string{"forgeReady", "ui"}, UpdatedAt: timePtr(now),
		},
		{
			ID: "Forge-i9j0", Title: "Warden learned rules not persisting across daemon restarts",
			Description: "Rules learned via `forge warden learn` are stored in memory but lost when the daemon restarts. Need to persist them to state.db.",
			Status: "in_progress", Priority: 1, IssueType: "bug", Anvil: "forge",
			Labels: []string{"bug"}, Assignee: "robin", HasPR: true, UpdatedAt: timePtr(yesterday),
		},
		{
			ID: "Forge-k1l2", Title: "Implement daily cost summary notification via Teams webhook",
			Description: "At end of day, send a Teams notification with total tokens used, USD cost, and bead throughput.",
			Status: "in_progress", Priority: 3, IssueType: "feature", Anvil: "forge",
			Labels: []string{"notify"}, Assignee: "robin", UpdatedAt: timePtr(twoDaysAgo),
		},
		{
			ID: "Forge-m3n4", Title: "Refactor temper package to support custom test commands per anvil",
			Description: "Currently Temper auto-detects Go/Node/.NET. Allow per-anvil override in forge.yaml like:\n\n```yaml\nanvils:\n  myrepo:\n    temper:\n      commands:\n        - make lint\n        - make test-unit\n```\n\nThis would let teams with non-standard build systems use Forge effectively.\n\nAcceptance criteria:\n- Config schema updated\n- Temper reads per-anvil config\n- Falls back to auto-detect if not specified\n- Documentation updated",
			Status: "open", Priority: 2, IssueType: "feature", Anvil: "forge",
			Labels: []string{"forgeReady", "refactor"}, UpdatedAt: timePtr(lastWeek),
			Blocks: []string{"Forge-x1y2"},
		},
		{
			ID: "Forge-o5p6", Title: "Closed: Fix IPC named pipe leak on Windows",
			Description: "Named pipe handles were not being closed properly on client disconnect, leading to handle exhaustion after ~500 connections.",
			Status: "closed", Priority: 2, IssueType: "bug", Anvil: "forge",
			ClosedAt: timePtr(yesterday), UpdatedAt: timePtr(yesterday),
		},
		{
			ID: "Forge-q7r8", Title: "Closed: Add govulncheck integration",
			Description: "Integrated govulncheck as a background scanner that creates prioritized beads for discovered vulnerabilities.",
			Status: "closed", Priority: 2, IssueType: "feature", Anvil: "forge",
			ClosedAt: timePtr(twoDaysAgo), UpdatedAt: timePtr(twoDaysAgo),
		},
		// in_review beads for kanban testing
		{
			ID: "Forge-rv01", Title: "Hearth: add webhook notification panel",
			Description: "PR is up and waiting for review. Adds a panel in the Hearth TUI for configuring and testing webhook notifications.",
			Status: "in_review", Priority: 2, IssueType: "feature", Anvil: "forge",
			Labels: []string{"hearth"}, Assignee: "robin", HasPR: true, UpdatedAt: timePtr(now),
		},
		{
			ID: "Forge-rv02", Title: "Fix race condition in worktree cleanup",
			Description: "The worktree remove function has a TOCTOU race when checking if the directory exists before removal.",
			Status: "in_review", Priority: 1, IssueType: "bug", Anvil: "forge",
			Labels: []string{"bug"}, Assignee: "robin", HasPR: true, UpdatedAt: timePtr(yesterday),
		},

		// --- forge anvil: epic with children and grandchildren ---
		{
			ID: "Forge-ep01", Title: "Wicket: GitHub Issues Intake Worker",
			Description: "Epic: Build the Wicket worker that monitors GitHub issues and triages them into beads.\n\nSee .forge/plans/github-issues-worker.md for the full plan.",
			Status: "open", Priority: 2, IssueType: "epic", Anvil: "forge",
			Labels: []string{"epic", "wicket"}, UpdatedAt: timePtr(now),
			Blocks: []string{"Forge-ep02", "Forge-ep03", "Forge-ep04"},
		},
		{
			ID: "Forge-ep02", Title: "Wicket Phase 1: Core Scaffolding (MVP)",
			Description: "Config types, internal/wicket package, ghClient wrapper, wicket_issues table, event types, poll loop, AI triage.",
			Status: "in_progress", Priority: 2, IssueType: "task", Anvil: "forge",
			Labels: []string{"wicket"}, Assignee: "robin", UpdatedAt: timePtr(now),
		},
		{
			ID: "Forge-ep03", Title: "Wicket Phase 2: Non-Trusted Users + Labels",
			Description: "Limited triage for non-trusted users, label management, trigger label, bot ignore list.",
			Status: "open", Priority: 2, IssueType: "feature", Anvil: "forge",
			Labels: []string{"wicket", "forgeReady"}, UpdatedAt: timePtr(yesterday),
			DependsOn: []string{"Forge-ep02"},
		},
		{
			ID: "Forge-ep04", Title: "Wicket Phase 3: Dispatch Confirmation + Follow-Up",
			Description: "Dispatch confirmation, clarification re-triage, issue-to-PR linking, auto-close on merge.",
			Status: "open", Priority: 2, IssueType: "feature", Anvil: "forge",
			Labels: []string{"wicket", "forgeReady"}, UpdatedAt: timePtr(lastWeek),
			DependsOn: []string{"Forge-ep03"},
		},
		// Another epic: i18n with grandchildren
		{
			ID: "Hytte-epic1", Title: "i18n: Internationalization for Norwegian and Thai",
			Description: "Add multi-language support to Hytte using react-i18next.\n\nPlan: .forge/plans/hytte-i18n.md",
			Status: "open", Priority: 2, IssueType: "epic", Anvil: "hytte",
			Labels: []string{"epic", "i18n"}, UpdatedAt: timePtr(now),
			Blocks: []string{"Hytte-i18n1", "Hytte-i18n2", "Hytte-i18n3"},
		},
		{
			ID: "Hytte-i18n1", Title: "i18n: Setup react-i18next infrastructure",
			Description: "Install packages, configure i18n, create translation file structure.",
			Status: "closed", Priority: 2, IssueType: "task", Anvil: "hytte",
			Labels: []string{"i18n"}, ClosedAt: timePtr(yesterday), UpdatedAt: timePtr(yesterday),
		},
		{
			ID: "Hytte-i18n2", Title: "i18n: Language switcher UI",
			Description: "Globe icon in sidebar, full selector in settings, persist to localStorage.",
			Status: "in_progress", Priority: 2, IssueType: "feature", Anvil: "hytte",
			Labels: []string{"i18n"}, Assignee: "robin", HasPR: true, UpdatedAt: timePtr(now),
			DependsOn: []string{"Hytte-i18n1"},
			Blocks: []string{"Hytte-i18n2a", "Hytte-i18n2b"},
		},
		{
			ID: "Hytte-i18n2a", Title: "i18n: Compact globe icon for collapsed sidebar",
			Description: "Render just a globe icon when sidebar is collapsed.",
			Status: "open", Priority: 3, IssueType: "task", Anvil: "hytte",
			Labels: []string{"i18n"}, UpdatedAt: timePtr(now),
		},
		{
			ID: "Hytte-i18n2b", Title: "i18n: Full dropdown with native script names",
			Description: "Full language selector showing English, Norsk (Bokmål), ไทย.",
			Status: "open", Priority: 3, IssueType: "task", Anvil: "hytte",
			Labels: []string{"i18n"}, UpdatedAt: timePtr(now),
		},
		{
			ID: "Hytte-i18n3", Title: "i18n: Extract strings from core pages",
			Description: "Extract hardcoded strings from Dashboard, Sidebar, Home, Weather widget.",
			Status: "open", Priority: 2, IssueType: "task", Anvil: "hytte",
			Labels: []string{"i18n", "forgeReady"}, UpdatedAt: timePtr(twoDaysAgo),
			DependsOn: []string{"Hytte-i18n2"},
		},

		// --- hytte anvil: mix of statuses ---
		{
			ID: "Hytte-a1a1", Title: "Cabin booking calendar shows wrong month after timezone change",
			Description: "When the user changes timezone in their browser settings, the booking calendar jumps to the wrong month. This is because we're using local time instead of UTC for date calculations.",
			Status: "open", Priority: 1, IssueType: "bug", Anvil: "hytte",
			Labels: []string{"forgeReady", "frontend"}, UpdatedAt: timePtr(now),
		},
		{
			ID: "Hytte-b2b2", Title: "Implement guest WiFi voucher generation for cabin check-in",
			Description: "Generate time-limited WiFi access codes when guests check in. Codes should expire at checkout time and be printed on the welcome sheet.",
			Status: "open", Priority: 3, IssueType: "feature", Anvil: "hytte",
			Labels: []string{"backend"}, UpdatedAt: timePtr(lastWeek),
		},
		{
			ID: "Hytte-c3c3", Title: "Database migration fails on PostgreSQL 16 with new reserved keyword",
			Description: "The `user` column name conflicts with a newly reserved keyword in PG16. Need to quote all column identifiers or rename.",
			Status: "in_progress", Priority: 1, IssueType: "bug", Anvil: "hytte",
			Labels: []string{"database", "breaking"}, Assignee: "erik", HasPR: true, UpdatedAt: timePtr(yesterday),
		},
		{
			ID: "Hytte-d4d4", Title: "Add weather forecast widget to cabin dashboard",
			Description: "Pull weather data from MET Norway API (api.met.no) and display a 5-day forecast on the cabin's dashboard page. Should cache responses for 1 hour to respect rate limits.",
			Status: "open", Priority: 4, IssueType: "feature", Anvil: "hytte",
			Labels: []string{"nice-to-have"}, UpdatedAt: timePtr(lastWeek),
		},
		{
			ID: "Hytte-e5e5", Title: "Upgrade Node dependencies (security patch batch)",
			Description: "Several transitive dependencies have known CVEs. Run npm audit fix and verify all tests pass.",
			Status: "open", Priority: 2, IssueType: "task", Anvil: "hytte",
			Labels: []string{"forgeReady", "security"}, UpdatedAt: timePtr(twoDaysAgo),
		},
		{
			ID: "Hytte-rv01", Title: "i18n: Setup react-i18next infrastructure",
			Description: "Install packages, configure i18n instance, create translation file structure.",
			Status: "in_review", Priority: 2, IssueType: "feature", Anvil: "hytte",
			Labels: []string{"i18n"}, Assignee: "robin", HasPR: true, UpdatedAt: timePtr(now),
		},
		{
			ID: "Hytte-f6f6", Title: "Closed: Fix CORS headers for mobile app API access",
			Description: "Mobile app was getting CORS errors when calling the booking API from a different origin.",
			Status: "closed", Priority: 2, IssueType: "bug", Anvil: "hytte",
			ClosedAt: timePtr(lastWeek), UpdatedAt: timePtr(lastWeek),
		},

		// --- heimdall anvil: monitoring/security focused ---
		{
			ID: "Heim-a1a1", Title: "Alert deduplication producing false negatives when source field is missing",
			Description: "The dedup logic uses source+message as composite key. When source is empty string, unrelated alerts with the same message body get incorrectly deduped.\n\nThis affects approximately 12% of incoming alerts based on last week's logs.\n\nProposed fix: treat empty source as unique (never dedup).",
			Status: "open", Priority: 1, IssueType: "bug", Anvil: "heimdall",
			Labels: []string{"forgeReady", "critical"}, UpdatedAt: timePtr(now),
		},
		{
			ID: "Heim-b2b2", Title: "Implement Prometheus metrics endpoint for alert pipeline throughput",
			Description: "Expose /metrics endpoint with:\n- alerts_received_total (counter)\n- alerts_processed_total (counter, by status)\n- alert_processing_duration_seconds (histogram)\n- alert_queue_depth (gauge)",
			Status: "in_progress", Priority: 2, IssueType: "feature", Anvil: "heimdall",
			Labels: []string{"observability"}, Assignee: "sigrid", UpdatedAt: timePtr(twoDaysAgo),
		},
		{
			ID: "Heim-c3c3", Title: "Rate limiter allows burst above configured maximum during clock skew",
			Description: "The token bucket implementation doesn't account for NTP adjustments. When system clock jumps forward, the bucket refills instantly allowing a burst.",
			Status: "open", Priority: 2, IssueType: "bug", Anvil: "heimdall",
			Labels: []string{"forgeReady"}, UpdatedAt: timePtr(yesterday),
			DependsOn: []string{"Heim-a1a1"},
		},
		{
			ID: "Heim-d4d4", Title: "Add PagerDuty integration for P1 alerts",
			Description: "When a P1 alert fires and isn't acknowledged within 5 minutes, escalate to PagerDuty. Requires API key configuration in config.yaml.",
			Status: "open", Priority: 3, IssueType: "feature", Anvil: "heimdall",
			Labels: []string{"integration"}, UpdatedAt: timePtr(lastWeek),
		},
		{
			ID: "Heim-e5e5", Title: "Closed: Fix TLS certificate rotation causing 30s downtime",
			Description: "Certificate hot-reload was closing the listener before the new cert was ready. Switched to atomic pointer swap.",
			Status: "closed", Priority: 1, IssueType: "bug", Anvil: "heimdall",
			ClosedAt: timePtr(twoDaysAgo), UpdatedAt: timePtr(twoDaysAgo),
		},
	}
}

func renderOnly(width, height int) {
	anvils := map[string]string{
		"forge":    "C:\\source\\fhigit\\Forge",
		"hytte":    "C:\\source\\fhigit\\Hytte",
		"heimdall": "C:\\source\\fhigit\\Heimdall",
	}
	model := ledger.NewModel(anvils, nil, nil)

	// Send WindowSizeMsg to set dimensions.
	sizeMsg := tea.WindowSizeMsg{Width: width, Height: height}
	updated, _ := model.Update(sizeMsg)
	m := updated.(*ledger.Model)

	// Simulate pressing Enter to select the first anvil.
	enterMsg := tea.KeyMsg{Type: tea.KeyEnter}
	updated, _ = m.Update(enterMsg)
	m = updated.(*ledger.Model)

	// Inject mock beads.
	beadsMsg := ledger.UpdateBeadsMsg{Beads: buildMockBeads(), Err: nil}
	updated, _ = m.Update(beadsMsg)
	m = updated.(*ledger.Model)

	view := m.View()
	fmt.Print(view)
	fmt.Fprintf(os.Stderr, "\n--- %d lines ---\n", len(strings.Split(view, "\n")))
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--render" {
		w, h := 180, 45
		if len(os.Args) > 3 {
			fmt.Sscanf(os.Args[2], "%d", &w)
			fmt.Sscanf(os.Args[3], "%d", &h)
		}
		renderOnly(w, h)
		return
	}

	anvils := map[string]string{
		"forge":    "C:\\source\\fhigit\\Forge",
		"hytte":    "C:\\source\\fhigit\\Hytte",
		"heimdall": "C:\\source\\fhigit\\Heimdall",
	}

	model := ledger.NewModel(anvils, nil, nil)
	mockBeads := buildMockBeads()

	wrapper := &wrapperModel{
		inner:     model,
		mockBeads: mockBeads,
	}

	// Suppress Go's default logger to avoid corrupting the alt-screen.
	prevLogOut := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(prevLogOut)

	p := tea.NewProgram(
		wrapper,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
		os.Exit(1)
	}
}
