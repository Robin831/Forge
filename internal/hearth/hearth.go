// Package hearth provides The Forge's TUI dashboard using Bubbletea.
//
// The TUI has three columns:
//   - Queue / Ready to Merge / Needs Attention (left column, stacked): Pending, mergeable, and stuck beads
//   - Workers (center column): Active Smith processes
//   - Live Activity / Events (right column, stacked): Streaming log + event log
//
// Tab switches focus between panels, j/k scrolls the focused panel,
// q quits the app.
package hearth

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// Panel identifies a TUI panel.
type Panel int

const (
	PanelQueue Panel = iota
	PanelCrucibles
	PanelReadyToMerge
	PanelNeedsAttention
	PanelWorkers
	PanelUsage
	PanelLiveActivity
	PanelEvents

	panelCount = PanelEvents + 1 // NOTE: update if panels are added/removed

	// Event panel rendering constants
	eventPanelInteriorPadding = 4
	eventPanelMinWidth        = 1
	eventTimestampWidth       = 9  // "HH:MM:SS "
	eventMsgMinWidth          = 20 // Minimum width before msg moves to next line

)

// QueueItem represents a bead in the queue panel.
type QueueItem struct {
	BeadID      string
	Title       string
	Description string
	Anvil       string
	Priority    int
	Status      string
	Section     string // "ready", "unlabeled", "in_progress"
	Assignee    string
}

// queueNavItem represents a navigable item in the grouped queue panel.
// It is either a collapsible anvil header or a reference to a bead.
type queueNavItem struct {
	isAnvil   bool
	anvilName string
	beadIdx   int // index into Model.queue; -1 for anvil headers
}

// activityNavItem represents a single display row in the Live Activity panel.
// It is either a collapsed group header (togglable) or an individual line from
// an expanded group.
type activityNavItem struct {
	isGroupHeader bool   // true for collapsed/collapsible summary lines
	groupType     string // event type key for expansion toggling (e.g. "tool", "think")
	text          string // raw display text (unstyled)
}

// AttentionReason categorizes why a bead needs human attention.
type AttentionReason int

const (
	AttentionUnknown          AttentionReason = iota
	AttentionDispatchExhausted                // Circuit breaker tripped after repeated dispatch failures
	AttentionCIFixExhausted                   // CI fix attempts exhausted
	AttentionReviewFixExhausted               // Review fix attempts exhausted
	AttentionRebaseExhausted                  // Rebase attempts exhausted
	AttentionClarification                    // Bead flagged as needing clarification
	AttentionStalled                          // Worker stalled (no log activity)
)

// NeedsAttentionItem represents a bead requiring human attention.
type NeedsAttentionItem struct {
	BeadID         string
	Title          string
	Anvil          string
	Reason         string
	ReasonCategory AttentionReason
	PRID           int // Non-zero when item originates from an exhausted PR
	PRNumber       int
}

// ReadyToMergeItem represents a PR ready to merge.
type ReadyToMergeItem struct {
	PRID     int
	PRNumber int
	BeadID   string
	Anvil    string
	Branch   string
	Title    string
}

// WorkerItem represents a worker in the workers panel.
type WorkerItem struct {
	ID            string
	BeadID        string
	Title         string // Bead title for display
	Anvil         string
	Status        string
	Duration      string
	CostUSD       float64
	Type          string   // "smith", "warden", "temper", "cifix", "reviewfix", "rebase"
	PRNumber      int      // PR number for bellows-triggered workers
	LastLog       string   // Last line from the worker log
	PID           int      // Process ID for kill
	LogPath       string   // Path to the worker's log file
	ActivityLines []string // Recent parsed activity from the log (tool calls, thinking, text)
}

// EventItem represents an event in the event log panel.
type EventItem struct {
	Timestamp string
	Type      string
	Message   string
	BeadID    string
}

// CrucibleItem represents an active Crucible in the TUI.
type CrucibleItem struct {
	ParentID          string
	ParentTitle       string
	Anvil             string
	Branch            string
	Phase             string // "started", "dispatching", "waiting", "final_pr", "complete", "paused"
	TotalChildren     int
	CompletedChildren int
	CurrentChild      string
	StartedAt         string
}

// ActionMenuChoice represents an action the user can take on a Needs Attention bead.
type ActionMenuChoice int

const (
	ActionRetry ActionMenuChoice = iota
	ActionDismiss
	ActionViewLogs

	actionMenuCount = ActionViewLogs + 1
)

// actionMenuLabels returns the display labels for the action menu.
func actionMenuLabels() [actionMenuCount]string {
	return [actionMenuCount]string{
		"Retry       — Clear flags, put back in queue",
		"Dismiss     — Remove from Needs Attention",
		"View Logs   — Show last worker log",
	}
}

// QueueActionMenuChoice represents an action the user can take on an unlabeled queue bead.
type QueueActionMenuChoice int

const (
	QueueActionLabel QueueActionMenuChoice = iota
	QueueActionClose

	queueActionMenuCount = QueueActionClose + 1
)

// queueActionMenuLabels returns the display labels for the queue action menu.
func queueActionMenuLabels() [queueActionMenuCount]string {
	return [queueActionMenuCount]string{
		"Label for dispatch — Tag bead for auto-dispatch",
		"Close             — Close this bead",
	}
}

// MergeMenuChoice represents an action on a ready-to-merge PR.
type MergeMenuChoice int

const (
	MergeActionMerge MergeMenuChoice = iota

	mergeMenuCount = MergeActionMerge + 1
)

func mergeMenuLabels() [mergeMenuCount]string {
	return [mergeMenuCount]string{
		"Merge — Merge this PR",
	}
}

// Model is the Bubbletea model for the Hearth TUI.
type Model struct {
	// Panels
	queue          []QueueItem
	crucibles      []CrucibleItem
	needsAttention []NeedsAttentionItem
	readyToMerge   []ReadyToMergeItem
	workers        []WorkerItem
	events         []EventItem
	usage          UsageData

	// Data source for polling
	data *DataSource

	// Callback for killing a worker (set by the caller)
	OnKill func(workerID string, pid int)

	// Callbacks for Needs Attention actions (set by the caller)
	OnRetryBead   func(beadID, anvil string, prID int) error
	OnDismissBead func(beadID, anvil string, prID int) error
	OnViewLogs    func(beadID string) (logPath string, lines []string)

	// Callback for tagging a bead (set by the caller).
	// Called with (beadID, anvil) when user presses 'l' on an unlabeled bead.
	OnTagBead func(beadID, anvil string) error

	// Callback for closing a bead (set by the caller).
	OnCloseBead func(beadID, anvil string) error

	// Callback for merging a PR (set by the caller).
	OnMergePR func(prID, prNumber int, anvil string) error

	// State
	focused        Panel
	queueVP        scrollViewport
	crucibleVP     scrollViewport
	needsAttnVP    scrollViewport
	readyToMergeVP scrollViewport
	workerVP         scrollViewport
	activityVP       scrollViewport
	activityExpanded map[string]bool   // event type → expanded override (nil = default)
	activityNavItems []activityNavItem // flat display items for Live Activity
	eventScroll      int
	eventAutoScroll      bool // true = follow new events
	prevEventCount       int  // track event count for auto-scroll
	width                int
	height               int
	ready                bool

	// Action menu overlay state (Needs Attention)
	showActionMenu bool
	actionMenuIdx  int
	actionTarget   *NeedsAttentionItem // bead the menu is open for

	// Queue action menu overlay state (Unlabeled beads)
	showQueueActionMenu bool
	queueActionMenuIdx  int
	queueActionTarget   *QueueItem // bead the queue menu is open for

	// Merge menu overlay state
	showMergeMenu bool
	mergeMenuIdx  int
	mergeTarget   *ReadyToMergeItem

	// Log viewer overlay state
	showLogViewer  bool
	logViewerTitle string
	logViewerEmpty bool // true when the log has no lines; use viewport as content source of truth
	logViewPort    viewport.Model

	// Daemon health indicator
	daemonConnected bool      // true when last IPC status check succeeded
	daemonLastPoll  string    // e.g. "30s ago" or "n/a"
	daemonWorkers   int       // active worker count from daemon
	daemonQueueSize int       // queue size from daemon
	daemonUptime    string    // daemon uptime string
	healthTickCount int       // counts ticks; health IPC fires every healthTickDivisor ticks

	// Status message (flashes briefly after an action)
	statusMsg     string
	statusMsgTime    time.Time
	statusMsgIsError bool

	// Queue anvil grouping state — groups beads by anvil when 2+ anvils present.
	queueExpandedAnvils map[string]bool   // per-anvil expanded/collapsed state
	queueNavItems       []queueNavItem    // navigable items (anvil headers + beads)
	queueGrouped        bool              // true when 2+ anvils trigger grouping

	// Spinner animation frame index (advances every SpinnerInterval).
	spinnerFrame int

	// Event rendering cache
	eventLinesCache       []string
	eventWidthCache       int
	eventSelectedIdxCache int
	eventCountCache       int
	eventRevision         int // incremented on every UpdateEventsMsg to detect content changes
	eventRevisionCache    int
}

// NewModel creates a new Hearth TUI model.
// Pass nil for DataSource to run in display-only mode (no polling).
func NewModel(ds *DataSource) Model {
	return Model{
		focused:             PanelQueue,
		data:                ds,
		eventAutoScroll:     true,
		queueExpandedAnvils: make(map[string]bool),
		activityExpanded:    make(map[string]bool),
	}
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{tea.SetWindowTitle("The Forge — Hearth")}

	// Start the data tick cycle, spinner animation, and do an initial fetch when
	// a data source is present. In display-only mode (m.data == nil), avoid
	// scheduling periodic ticks to reduce unnecessary CPU usage.
	if m.data != nil {
		cmds = append(cmds, SpinnerTick())
		cmds = append(cmds, Tick())
		cmds = append(cmds, FetchAll(m.data))
		cmds = append(cmds, FetchDaemonHealth())
	}

	return tea.Batch(cmds...)
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Log viewer overlay intercepts all keys
		if m.showLogViewer {
			switch msg.String() {
			case "q", "esc":
				m.showLogViewer = false
			default:
				var cmd tea.Cmd
				m.logViewPort, cmd = m.logViewPort.Update(msg)
				return m, cmd
			}
			return m, nil
		}

		// Action menu overlay intercepts keys when open
		if m.showActionMenu {
			switch msg.String() {
			case "esc", "q":
				m.showActionMenu = false
			case "j", "down":
				m.actionMenuIdx = (m.actionMenuIdx + 1) % int(actionMenuCount)
			case "k", "up":
				m.actionMenuIdx = (m.actionMenuIdx + int(actionMenuCount) - 1) % int(actionMenuCount)
			case "enter":
				cmd := m.executeAction(ActionMenuChoice(m.actionMenuIdx))
				m.showActionMenu = false
				return m, cmd
			}
			return m, nil
		}

		// Queue action menu overlay intercepts keys when open
		if m.showQueueActionMenu {
			switch msg.String() {
			case "esc", "q":
				m.showQueueActionMenu = false
			case "j", "down":
				m.queueActionMenuIdx = (m.queueActionMenuIdx + 1) % int(queueActionMenuCount)
			case "k", "up":
				m.queueActionMenuIdx = (m.queueActionMenuIdx + int(queueActionMenuCount) - 1) % int(queueActionMenuCount)
			case "enter":
				cmd := m.executeQueueAction(QueueActionMenuChoice(m.queueActionMenuIdx))
				m.showQueueActionMenu = false
				return m, cmd
			}
			return m, nil
		}

		// Merge menu overlay intercepts keys when open
		if m.showMergeMenu {
			switch msg.String() {
			case "esc", "q":
				m.showMergeMenu = false
			case "j", "down":
				m.mergeMenuIdx = (m.mergeMenuIdx + 1) % int(mergeMenuCount)
			case "k", "up":
				m.mergeMenuIdx = (m.mergeMenuIdx + int(mergeMenuCount) - 1) % int(mergeMenuCount)
			case "enter":
				cmd := m.executeMergeAction(MergeMenuChoice(m.mergeMenuIdx))
				m.showMergeMenu = false
				return m, cmd
			}
			return m, nil
		}

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit

		case "tab":
			m.focused = (m.focused + 1) % panelCount
			if m.focused == PanelCrucibles && len(m.crucibles) == 0 {
				m.focused = (m.focused + 1) % panelCount
			}

		case "shift+tab":
			m.focused = (m.focused + panelCount - 1) % panelCount
			if m.focused == PanelCrucibles && len(m.crucibles) == 0 {
				m.focused = (m.focused + panelCount - 1) % panelCount
			}

		case "j", "down":
			m.scrollDown()
			// Disable auto-scroll when user manually scrolls events
			if m.focused == PanelEvents {
				m.eventAutoScroll = false
			}

		case "k", "up":
			m.scrollUp()
			if m.focused == PanelEvents {
				m.eventAutoScroll = false
			}

		case "f", "F":
			// Toggle follow mode (auto-scroll) for events or activity
			if m.focused == PanelEvents {
				m.eventAutoScroll = !m.eventAutoScroll
				if m.eventAutoScroll {
					m.eventScroll = 0
				}
			} else if m.focused == PanelLiveActivity {
				// Reset activity scroll to top (follow latest — newest items first)
				m.activityVP.cursor = 0
				m.activityVP.viewStart = 0
			}

		case "K":
			// Kill selected worker
			if m.focused == PanelWorkers && len(m.workers) > 0 &&
				m.workerVP.cursor < len(m.workers) {
				w := m.workers[m.workerVP.cursor]
				if m.OnKill != nil {
					m.OnKill(w.ID, w.PID)
				}
			}

		case "enter":
			// Open action menu for selected Needs Attention bead
			if m.focused == PanelNeedsAttention && len(m.needsAttention) > 0 &&
				m.needsAttnVP.cursor < len(m.needsAttention) {
				item := m.needsAttention[m.needsAttnVP.cursor]
				m.actionTarget = &item
				m.actionMenuIdx = 0
				m.showActionMenu = true
			}
			// Queue panel: toggle anvil expand/collapse or open action menu for unlabeled beads
			if m.focused == PanelQueue {
				m.ensureQueueNav()
				if len(m.queueNavItems) > 0 && m.queueVP.cursor < len(m.queueNavItems) {
					nav := m.queueNavItems[m.queueVP.cursor]
					if nav.isAnvil {
						if m.queueExpandedAnvils == nil {
							m.queueExpandedAnvils = make(map[string]bool)
						}
						m.queueExpandedAnvils[nav.anvilName] = !m.queueExpandedAnvils[nav.anvilName]
						m.rebuildQueueNav()
					} else if nav.beadIdx >= 0 && nav.beadIdx < len(m.queue) {
						item := m.queue[nav.beadIdx]
						if item.Section == "unlabeled" {
							m.queueActionTarget = &item
							m.queueActionMenuIdx = 0
							m.showQueueActionMenu = true
						}
					}
				}
			}
			// Live Activity: toggle group expand/collapse
			if m.focused == PanelLiveActivity && len(m.activityNavItems) > 0 &&
				m.activityVP.cursor < len(m.activityNavItems) {
				nav := m.activityNavItems[m.activityVP.cursor]
				if nav.groupType != "" {
					if m.activityExpanded == nil {
						m.activityExpanded = make(map[string]bool)
					}
					cur := m.isActivityGroupExpanded(nav.groupType)
					m.activityExpanded[nav.groupType] = !cur
					m.rebuildActivityNav()
				}
			}
			// Open merge menu for selected Ready to Merge PR
			if m.focused == PanelReadyToMerge && len(m.readyToMerge) > 0 &&
				m.readyToMergeVP.cursor < len(m.readyToMerge) {
				item := m.readyToMerge[m.readyToMergeVP.cursor]
				m.mergeTarget = &item
				m.mergeMenuIdx = 0
				m.showMergeMenu = true
			}

		case "l":
			// Label (tag) selected bead in the queue for auto-dispatch
			if m.focused == PanelQueue {
				if bead := m.selectedQueueBead(); bead != nil && bead.Section == "unlabeled" {
					m.queueActionTarget = bead
					cmd := m.executeQueueAction(QueueActionLabel)
					return m, cmd
				}
			}

		case "esc":
			// Live Activity: collapse the group at the cursor
			if m.focused == PanelLiveActivity && len(m.activityNavItems) > 0 &&
				m.activityVP.cursor < len(m.activityNavItems) {
				nav := m.activityNavItems[m.activityVP.cursor]
				if !nav.isGroupHeader {
					if m.activityExpanded == nil {
						m.activityExpanded = make(map[string]bool)
					}
					m.activityExpanded[nav.groupType] = false
					m.rebuildActivityNav()
				}
			}
			// Collapse the anvil containing the selected bead
			if m.focused == PanelQueue && m.queueGrouped {
				m.ensureQueueNav()
				if m.queueVP.cursor < len(m.queueNavItems) {
					nav := m.queueNavItems[m.queueVP.cursor]
					if !nav.isAnvil && nav.anvilName != "" {
						target := nav.anvilName
						m.queueExpandedAnvils[target] = false
						m.rebuildQueueNav()
						for i, n := range m.queueNavItems {
							if n.isAnvil && n.anvilName == target {
								m.queueVP.cursor = i
								break
							}
						}
					}
				}
			}
		}

	case tea.MouseMsg:
		// Dismiss overlays on left or right mouse button press
		if msg.Action == tea.MouseActionPress &&
			(msg.Button == tea.MouseButtonLeft || msg.Button == tea.MouseButtonRight) {
			if m.showLogViewer || m.showActionMenu || m.showQueueActionMenu || m.showMergeMenu {
				m.showLogViewer = false
				m.showActionMenu = false
				m.showQueueActionMenu = false
				m.showMergeMenu = false
				return m, nil
			}
		}
		switch {
		case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft:
			// Click to focus the panel under the cursor
			m.focused = m.panelAtPos(msg.X, msg.Y)
		case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelUp:
			// Scroll the panel under the cursor up
			m.focused = m.panelAtPos(msg.X, msg.Y)
			m.scrollUp()
			if m.focused == PanelEvents {
				m.eventAutoScroll = false
			}
		case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonWheelDown:
			// Scroll the panel under the cursor down
			m.focused = m.panelAtPos(msg.X, msg.Y)
			m.scrollDown()
			if m.focused == PanelEvents {
				m.eventAutoScroll = false
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		if m.showLogViewer {
			vpWidth, vpHeight := m.logViewerDimensions()
			m.logViewPort.Width = vpWidth
			m.logViewPort.Height = vpHeight
		}

	case tea.MouseMsg:
		if m.showLogViewer {
			var cmd tea.Cmd
			m.logViewPort, cmd = m.logViewPort.Update(msg)
			return m, cmd
		}

	case UpdateQueueMsg:
		m.queue = msg.Items
		m.rebuildQueueNav()
		// Close the queue action menu if the target bead is no longer in the unlabeled section.
		if m.showQueueActionMenu && m.queueActionTarget != nil {
			found := false
			for _, qi := range m.queue {
				if qi.BeadID == m.queueActionTarget.BeadID && qi.Anvil == m.queueActionTarget.Anvil && qi.Section == "unlabeled" {
					found = true
					break
				}
			}
			if !found {
				m.showQueueActionMenu = false
				m.queueActionTarget = nil
			}
		}

	case QueueErrorMsg:
		// Preserve previous queue; surface the error in the events panel
		errEvent := EventItem{
			Timestamp: time.Now().Format("15:04:05"),
			Type:      "error",
			Message:   fmt.Sprintf("queue cache read failed: %v", msg.Err),
		}
		// Prepend so the synthetic error appears as the newest event
		m.events = append([]EventItem{errEvent}, m.events...)
		m.eventRevision++
		// In follow mode, keep view pinned to newest events so the error is visible
		if m.eventAutoScroll {
			m.eventScroll = 0
		}

	case UpdateCruciblesMsg:
		m.crucibles = msg.Items
		m.crucibleVP.ClampToTotal(len(msg.Items))

	case UpdateNeedsAttentionMsg:
		m.needsAttention = msg.Items
		m.needsAttnVP.ClampToTotal(len(msg.Items))

	case UpdateReadyToMergeMsg:
		m.readyToMerge = msg.Items
		m.readyToMergeVP.ClampToTotal(len(msg.Items))

	case NeedsAttentionErrorMsg:
		errEvent := EventItem{
			Timestamp: time.Now().Format("15:04:05"),
			Type:      "error",
			Message:   fmt.Sprintf("needs attention read failed: %v", msg.Err),
		}
		m.events = append([]EventItem{errEvent}, m.events...)
		m.eventRevision++
		if m.eventAutoScroll {
			m.eventScroll = 0
		}

	case ReadyToMergeErrorMsg:
		errEvent := EventItem{
			Timestamp: time.Now().Format("15:04:05"),
			Type:      "error",
			Message:   fmt.Sprintf("ready-to-merge read failed: %v", msg.Err),
		}
		m.events = append([]EventItem{errEvent}, m.events...)
		m.eventRevision++
		if m.eventAutoScroll {
			m.eventScroll = 0
		}

	case UpdateWorkersMsg:
		// Capture the currently selected worker ID before clamping, so we can
		// detect when ClampToTotal shifts the cursor to a different worker.
		prevCursor := m.workerVP.cursor
		var prevWorkerID string
		if prevCursor >= 0 && prevCursor < len(m.workers) {
			prevWorkerID = m.workers[prevCursor].ID
		}

		m.workers = msg.Items
		m.workerVP.ClampToTotal(len(msg.Items))

		// Reset live activity state if the selected worker implicitly changed.
		newCursor := m.workerVP.cursor
		var newWorkerID string
		if newCursor >= 0 && newCursor < len(m.workers) {
			newWorkerID = m.workers[newCursor].ID
		}
		if prevWorkerID != "" && newWorkerID != "" && prevWorkerID != newWorkerID {
			m.resetActivityState()
		} else {
			m.rebuildActivityNav()
		}

	case UpdateUsageMsg:
		m.usage = msg.Data

	case UpdateDaemonHealthMsg:
		m.daemonConnected = msg.Connected
		m.daemonLastPoll = msg.LastPoll
		m.daemonWorkers = msg.Workers
		m.daemonQueueSize = msg.QueueSize
		m.daemonUptime = msg.Uptime

	case UpdateEventsMsg:
		m.events = msg.Items
		m.eventRevision++
		// Auto-scroll to bottom if enabled and new events arrived
		if m.eventAutoScroll && len(msg.Items) > m.prevEventCount {
			if len(msg.Items) > 0 {
				m.eventScroll = 0 // Events are newest-first from DB
			}
		}
		m.prevEventCount = len(msg.Items)

	case QueueActionResultMsg:
		if msg.Err != nil {
			if msg.Action == "tag" {
				m.setStatus(fmt.Sprintf("Failed to tag %s: %v", msg.BeadID, msg.Err), true)
			} else {
				m.setStatus(fmt.Sprintf("Failed to close %s: %v", msg.BeadID, msg.Err), true)
			}
		} else {
			if msg.Action == "tag" {
				m.setStatus(fmt.Sprintf("Tagged %s for dispatch", msg.BeadID), false)
			} else {
				m.setStatus(fmt.Sprintf("Closed %s", msg.BeadID), false)
			}
		}

	case MergeResultMsg:
		if msg.Err != nil {
			errSummary := strings.SplitN(msg.Err.Error(), "\n", 2)[0]
			m.setStatus(fmt.Sprintf("Failed to merge PR #%d: %s", msg.PRNumber, errSummary), true)
			errEvent := EventItem{
				Timestamp: time.Now().Format("15:04:05"),
				Type:      "pr_merge_failed",
				Message:   fmt.Sprintf("PR #%d merge failed: %s", msg.PRNumber, errSummary),
			}
			m.events = append([]EventItem{errEvent}, m.events...)
			m.eventRevision++
			if m.eventAutoScroll {
				m.eventScroll = 0
			}
		} else {
			m.setStatus(fmt.Sprintf("Merged PR #%d", msg.PRNumber), false)
		}

	case TickMsg:
		// On each tick, refresh all panels and schedule the next tick.
		// Daemon health is checked every healthTickDivisor ticks to avoid
		// issuing a full IPC status round-trip on every 2s cycle.
		if m.data != nil {
			m.healthTickCount++
			cmds := []tea.Cmd{Tick(), FetchAll(m.data)}
			if m.healthTickCount%healthTickDivisor == 0 {
				cmds = append(cmds, FetchDaemonHealth())
			}
			return m, tea.Batch(cmds...)
		}

	case SpinnerTickMsg:
		// Advance spinner frame and schedule the next spinner tick.
		m.spinnerFrame = (m.spinnerFrame + 1) % len(SpinnerFrames)
		return m, SpinnerTick()
	}

	return m, nil
}

// View implements tea.Model.
// computeHeaderH returns the rendered height of the header bar.
// It is called by both View() and panelAtPos() to keep hit-testing in sync
// without mutating model state inside View().
func (m *Model) computeHeaderH() int {
	headerText := "🔥 The Forge — Hearth Dashboard"
	if m.daemonConnected {
		indicator := daemonConnectedStyle.Render("● Connected")
		if m.daemonLastPoll != "" && m.daemonLastPoll != "n/a" {
			indicator += dimStyle.Render(" (polled " + m.daemonLastPoll + ")")
		}
		headerText += "  " + indicator
	} else {
		headerText += "  " + daemonDisconnectedStyle.Render("○ Disconnected")
	}
	return lipgloss.Height(headerStyle.Width(m.width).Render(headerText))
}

// computeFooterH returns the rendered height of the footer bar.
// It uses the default hint text (status messages are at most 1 line).
// It is called by panelAtPos() to mirror the layout used by View().
func (m *Model) computeFooterH() int {
	footerText := "Tab: switch panel \u2022 j/k/wheel: scroll \u2022 K: kill worker \u2022 Enter: actions/merge \u2022 l: label bead \u2022 f: follow \u2022 q: quit"
	return lipgloss.Height(footerStyle.Width(m.width).Render(footerText))
}

func (m *Model) View() string {
	if !m.ready {
		return "Initializing The Forge..."
	}

	// Header with daemon health indicator
	headerText := "🔥 The Forge — Hearth Dashboard"
	if m.daemonConnected {
		indicator := daemonConnectedStyle.Render("● Connected")
		if m.daemonLastPoll != "" && m.daemonLastPoll != "n/a" {
			indicator += dimStyle.Render(" (polled " + m.daemonLastPoll + ")")
		}
		headerText += "  " + indicator
	} else {
		headerText += "  " + daemonDisconnectedStyle.Render("○ Disconnected")
	}
	header := headerStyle.Width(m.width).Render(headerText)
	headerH := lipgloss.Height(header)

	// Footer with status message or default hints
	footerText := "Tab: switch panel • j/k/wheel: scroll • K: kill worker • Enter: actions/merge • l: label bead • f: follow • q: quit"
	statusDuration := 5 * time.Second
	if m.statusMsgIsError {
		statusDuration = 10 * time.Second
	}
	if m.statusMsg != "" && time.Since(m.statusMsgTime) < statusDuration {
		if m.statusMsgIsError {
			footerText = statusErrorStyle.Render(m.statusMsg)
		} else {
			footerText = statusMsgStyle.Render(m.statusMsg)
		}
	}
	footer := footerStyle.Width(m.width).Render(footerText)
	footerH := lipgloss.Height(footer)

	queueWidth, workerWidth, activityWidth := m.getTopPanelWidths()
	topHeight, bottomHeight := m.getVerticalSplit(headerH, footerH)

	// Left column: Queue (top) + Ready to Merge (middle) + Needs Attention (bottom)
	leftColumn := m.renderLeftColumn(queueWidth, topHeight, bottomHeight)
	// Center: worker list (top ~85%) + Usage panel (bottom ~15%)
	centerColumn := m.renderCenterColumn(workerWidth, topHeight, bottomHeight)
	// Right column: Live Activity (top) + Events (bottom)
	rightColumn := m.renderRightColumn(activityWidth, topHeight, bottomHeight)

	// Final assembly
	columns := lipgloss.JoinHorizontal(lipgloss.Top, leftColumn, centerColumn, rightColumn)
	view := lipgloss.JoinVertical(lipgloss.Left,
		header,
		columns,
		footer,
	)

	// Render overlays on top
	if m.showLogViewer {
		overlay := m.renderLogViewer()
		view = placeOverlay(m.width, m.height, overlay, view)
	} else if m.showActionMenu {
		overlay := m.renderActionMenu()
		view = placeOverlay(m.width, m.height, overlay, view)
	} else if m.showQueueActionMenu {
		overlay := m.renderQueueActionMenu()
		view = placeOverlay(m.width, m.height, overlay, view)
	} else if m.showMergeMenu {
		overlay := m.renderMergeMenu()
		view = placeOverlay(m.width, m.height, overlay, view)
	}

	return view
}

func (m *Model) getTopPanelWidths() (queueWidth, workerWidth, activityWidth int) {
	remainingWidth := m.width - 4 // borders/gaps
	if remainingWidth < 0 {
		remainingWidth = 0
	}
	queueWidth = remainingWidth / 5
	workerWidth = remainingWidth / 4
	activityWidth = remainingWidth - queueWidth - workerWidth
	return
}

func (m *Model) getVerticalSplit(headerH, footerH int) (topHeight, bottomHeight int) {
	// contentHeight is the total vertical space available for the top panels and
	// the events panel, including their borders. We subtract the global
	// header and footer rows, AND the panel borders(4) here to ensure
	// stacked panels (2 borders each) fit.
	contentHeight := m.height - headerH - footerH - 4
	if contentHeight < 0 {
		contentHeight = 0
	}
	// Give top panels 60%, bottom panels 40% (same split for left and right columns).
	topHeight = contentHeight * 6 / 10
	if contentHeight >= 10 {
		topHeight = max(topHeight, 5)
	}
	bottomHeight = contentHeight - topHeight
	// Enforce a minimum bottom panel height of 4 lines so bordered panels
	// remain renderable at small terminal sizes.
	const minBottomHeight = 4
	if bottomHeight < minBottomHeight && contentHeight >= 10 {
		bottomHeight = minBottomHeight
		topHeight = contentHeight - bottomHeight
	}
	return
}

// panelBorderEachSide is the number of terminal columns consumed by one side
// of a panel border (lipgloss RoundedBorder = 1 char per side).
const panelBorderEachSide = 1

// panelAtPos returns the Panel at the given terminal (x, y) coordinate.
// Used for mouse click focus and scroll targeting. Out-of-range positions
// (header, footer, or outside the terminal) return the current focused panel.
// The vertical split is computed using the same getVerticalSplit helper as
// View() to keep hit-testing in sync with the rendered layout.
func (m *Model) panelAtPos(x, y int) Panel {
	queueWidth, workerWidth, _ := m.getTopPanelWidths()

	// Determine column x boundaries.
	// Each panel has panelBorderEachSide chars on both left and right.
	// leftEnd is the inclusive rightmost column of the left panel (0-indexed).
	// centerEnd is the inclusive rightmost column of the center panel.
	leftColumnWidth := queueWidth + 2*panelBorderEachSide
	centerColumnWidth := workerWidth + 2*panelBorderEachSide
	leftEnd := leftColumnWidth - 1
	centerEnd := leftColumnWidth + centerColumnWidth - 1

	// Compute header and footer heights without mutating model state.
	hh := m.computeHeaderH()
	if hh <= 0 {
		hh = 1
	}
	fh := m.computeFooterH()
	if fh <= 0 {
		fh = 1
	}

	// Rows in the header or footer region return the current focused panel.
	if y < hh || y >= m.height-fh {
		return m.focused
	}

	// Use the same vertical split as View() for accurate hit-testing.
	topH, bottomH := m.getVerticalSplit(hh, fh)
	if topH <= 0 {
		return m.focused
	}

	contentY := y - hh
	// The top panel occupies topH inner rows + 2 border rows (top + bottom border).
	topRegionH := topH + 2

	switch {
	case x <= leftEnd:
		// Left column: Queue/Crucibles (top region) or ReadyToMerge/NeedsAttention (bottom region).
		if contentY < topRegionH {
			if len(m.crucibles) > 0 {
				// When crucibles are present, split the top region between Queue (upper)
				// and Crucibles (lower) so both can be focused via mouse.
				queueTopH := topRegionH / 2
				if queueTopH < 1 {
					queueTopH = 1
				}
				if contentY < queueTopH {
					return PanelQueue
				}
				return PanelCrucibles
			}
			// No crucibles: entire top region is Queue.
			return PanelQueue
		}
		// Bottom section: split evenly between ReadyToMerge (upper) and NeedsAttention (lower).
		bottomRegionY := contentY - topRegionH
		bottomRegionH := bottomH + 2 // inner + 2 border rows
		if bottomRegionH <= 0 {
			return PanelReadyToMerge
		}
		halfBottom := bottomRegionH / 2
		if bottomRegionY < halfBottom {
			return PanelReadyToMerge
		}
		return PanelNeedsAttention
	case x <= centerEnd:
		// Center column: Workers (top) + Usage panel (bottom, fixed height 10).
		fullH := topH + bottomH
		if fullH >= 20 {
			const usagePanelHeight = 10
			workerH := fullH - usagePanelHeight
			// Worker panel occupies workerH inner rows + 2 border rows.
			if contentY < workerH+2 {
				return PanelWorkers
			}
			return PanelUsage
		}
		return PanelWorkers
	default:
		// Right column: LiveActivity (top) or Events (bottom).
		if contentY < topRegionH {
			return PanelLiveActivity
		}
		return PanelEvents
	}
}

// scrollDown scrolls the focused panel down.
func (m *Model) scrollDown() {
	switch m.focused {
	case PanelQueue:
		m.ensureQueueNav()
		m.queueVP.ScrollDown(len(m.queueNavItems))
	case PanelCrucibles:
		m.crucibleVP.ScrollDown(len(m.crucibles))
	case PanelNeedsAttention:
		m.needsAttnVP.ScrollDown(len(m.needsAttention))
	case PanelReadyToMerge:
		m.readyToMergeVP.ScrollDown(len(m.readyToMerge))
	case PanelWorkers:
		prev := m.workerVP.cursor
		m.workerVP.ScrollDown(len(m.workers))
		if m.workerVP.cursor != prev {
			m.resetActivityState()
		}
	case PanelLiveActivity:
		m.activityVP.ScrollDown(len(m.activityNavItems))
	case PanelEvents:
		_, _, aw := m.getTopPanelWidths()
		totalLines := m.eventTotalLineCount(aw)
		if m.eventScroll < totalLines-1 {
			m.eventScroll++
		}
	}
}

// scrollUp scrolls the focused panel up.
func (m *Model) scrollUp() {
	switch m.focused {
	case PanelQueue:
		m.ensureQueueNav()
		m.queueVP.ScrollUp()
	case PanelCrucibles:
		m.crucibleVP.ScrollUp()
	case PanelNeedsAttention:
		m.needsAttnVP.ScrollUp()
	case PanelReadyToMerge:
		m.readyToMergeVP.ScrollUp()
	case PanelWorkers:
		prev := m.workerVP.cursor
		m.workerVP.ScrollUp()
		if m.workerVP.cursor != prev {
			m.resetActivityState()
		}
	case PanelLiveActivity:
		m.activityVP.ScrollUp()
	case PanelEvents:
		if m.eventScroll > 0 {
			m.eventScroll--
		}
	}
}

// selectedWorkerActivity returns the activity lines for the currently selected worker.
func (m *Model) selectedWorkerActivity() []string {
	if len(m.workers) > 0 && m.workerVP.cursor < len(m.workers) {
		return m.workers[m.workerVP.cursor].ActivityLines
	}
	return nil
}

// setStatus sets the status message, error flag, and timestamp together.
// Callers should pass isError=true only for genuine failures; all other messages use false.
func (m *Model) setStatus(msg string, isError bool) {
	m.statusMsg = msg
	m.statusMsgIsError = isError
	m.statusMsgTime = time.Now()
}

// rebuildQueueNav rebuilds the navigable item list for the queue panel.
// When 2+ anvils are present, items are grouped under collapsible anvil headers.
// When only 1 anvil exists, items are listed flat (no headers).
func (m *Model) rebuildQueueNav() {
	m.queueNavItems = nil
	if m.queueExpandedAnvils == nil {
		m.queueExpandedAnvils = make(map[string]bool)
	}

	// Collect unique anvils. Start with registered anvils from config (sorted),
	// then append any anvils found in queue data that weren't in the config.
	seen := map[string]bool{}
	var anvilOrder []string
	if m.data != nil {
		for _, name := range m.data.AnvilNames {
			if !seen[name] {
				seen[name] = true
				anvilOrder = append(anvilOrder, name)
			}
		}
	}
	for _, item := range m.queue {
		if !seen[item.Anvil] {
			seen[item.Anvil] = true
			anvilOrder = append(anvilOrder, item.Anvil)
		}
	}

	m.queueGrouped = len(anvilOrder) > 1

	if !m.queueGrouped {
		// Single anvil: nav items are direct bead references (no headers).
		for i := range m.queue {
			m.queueNavItems = append(m.queueNavItems, queueNavItem{beadIdx: i})
		}
	} else {
		// Multiple anvils: group under collapsible headers.
		anvilBeads := map[string][]int{} // anvil → indices into m.queue
		for i, item := range m.queue {
			anvilBeads[item.Anvil] = append(anvilBeads[item.Anvil], i)
		}
		for _, anvil := range anvilOrder {
			m.queueNavItems = append(m.queueNavItems, queueNavItem{
				isAnvil:   true,
				anvilName: anvil,
				beadIdx:   -1,
			})
			if m.queueExpandedAnvils[anvil] {
				for _, idx := range anvilBeads[anvil] {
					m.queueNavItems = append(m.queueNavItems, queueNavItem{
						anvilName: anvil,
						beadIdx:   idx,
					})
				}
			}
		}
	}

	m.queueVP.ClampToTotal(len(m.queueNavItems))
}

// ensureQueueNav lazily builds queueNavItems if nav hasn't been built yet
// (e.g. when queue is set directly in tests or when registered anvils exist
// but have no beads).
func (m *Model) ensureQueueNav() {
	hasAnvils := m.data != nil && len(m.data.AnvilNames) > 1
	if len(m.queueNavItems) == 0 && (len(m.queue) > 0 || hasAnvils) {
		m.rebuildQueueNav()
	}
}

// selectedQueueBead returns the QueueItem under the cursor, or nil if the
// cursor is on an anvil header or out of range.
func (m *Model) selectedQueueBead() *QueueItem {
	m.ensureQueueNav()
	if len(m.queueNavItems) == 0 || m.queueVP.cursor >= len(m.queueNavItems) {
		return nil
	}
	nav := m.queueNavItems[m.queueVP.cursor]
	if nav.isAnvil || nav.beadIdx < 0 || nav.beadIdx >= len(m.queue) {
		return nil
	}
	item := m.queue[nav.beadIdx]
	return &item
}

// executeAction runs the selected action menu choice against the target bead.
func (m *Model) executeAction(choice ActionMenuChoice) tea.Cmd {
	if m.actionTarget == nil {
		return nil
	}
	bead := m.actionTarget
	switch choice {
	case ActionRetry:
		if m.OnRetryBead != nil {
			if err := m.OnRetryBead(bead.BeadID, bead.Anvil, bead.PRID); err != nil {
				m.setStatus(fmt.Sprintf("Failed to retry %s: %v", bead.BeadID, err), true)
			} else {
				m.setStatus(fmt.Sprintf("Retry queued for %s", bead.BeadID), false)
				m.removeNeedsAttentionItem(bead.BeadID, bead.Anvil)
				if m.data != nil {
					return FetchNeedsAttention(m.data)
				}
				return nil
			}
		} else {
			m.setStatus(fmt.Sprintf("Retry action unavailable for %s", bead.BeadID), false)
		}
	case ActionDismiss:
		if m.OnDismissBead != nil {
			if err := m.OnDismissBead(bead.BeadID, bead.Anvil, bead.PRID); err != nil {
				m.setStatus(fmt.Sprintf("Failed to dismiss %s: %v", bead.BeadID, err), true)
			} else {
				m.setStatus(fmt.Sprintf("Dismissed %s", bead.BeadID), false)
				m.removeNeedsAttentionItem(bead.BeadID, bead.Anvil)
				if m.data != nil {
					return FetchNeedsAttention(m.data)
				}
				return nil
			}
		} else {
			m.setStatus(fmt.Sprintf("Dismiss action unavailable for %s", bead.BeadID), false)
		}
	case ActionViewLogs:
		if m.OnViewLogs != nil {
			logPath, lines := m.OnViewLogs(bead.BeadID)
			if logPath == "" {
				m.setStatus(fmt.Sprintf("No logs found for %s", bead.BeadID), false)
				return nil
			}
			m.logViewerTitle = fmt.Sprintf("Logs: %s — %s", bead.BeadID, logPath)
			m.logViewerEmpty = len(lines) == 0
			vpWidth, vpHeight := m.logViewerDimensions()
			m.logViewPort = viewport.New(vpWidth, vpHeight)
			m.logViewPort.SetContent(strings.Join(lines, "\n"))
			m.showLogViewer = true
		}
	}
	return nil
}

// removeNeedsAttentionItem removes the item with the given beadID and anvil from the
// needsAttention list and adjusts the scroll position if necessary.
func (m *Model) removeNeedsAttentionItem(beadID, anvil string) {
	for i, item := range m.needsAttention {
		if item.BeadID == beadID && item.Anvil == anvil {
			m.needsAttention = append(m.needsAttention[:i], m.needsAttention[i+1:]...)
			m.needsAttnVP.ClampToTotal(len(m.needsAttention))
			return
		}
	}
}

// renderActionMenu renders the action menu overlay centered on screen.
func (m *Model) renderActionMenu() string {
	if m.actionTarget == nil {
		return ""
	}

	menuWidth := 52
	contentWidth := menuWidth - actionMenuStyle.GetHorizontalFrameSize()
	labels := actionMenuLabels()

	var lines []string
	title := fmt.Sprintf("Actions for %s", m.actionTarget.BeadID)
	lines = append(lines, actionMenuTitleStyle.Render(title))

	if m.actionTarget.Title != "" {
		lines = append(lines, dimStyle.Render(truncate(m.actionTarget.Title, contentWidth)))
	}

	lines = append(lines, "")

	for i, label := range labels {
		cursor := "  "
		if i == m.actionMenuIdx {
			cursor = "> "
			label = actionMenuSelectedStyle.Render(label)
		} else {
			label = dimStyle.Render(label)
		}
		lines = append(lines, cursor+label)
	}

	lines = append(lines, "")
	lines = append(lines, dimStyle.Render("Enter: select • Esc: close"))

	content := strings.Join(lines, "\n")
	popup := actionMenuStyle.Width(menuWidth).Render(content)

	return popup
}

// QueueActionResultMsg is delivered asynchronously when a queue action (tag/close) completes.
type QueueActionResultMsg struct {
	BeadID string
	Action string // "tag" or "close"
	Err    error
}

// executeQueueAction returns a tea.Cmd that runs the queue action asynchronously,
// keeping the Bubbletea UI responsive during the IPC round-trip.
func (m *Model) executeQueueAction(choice QueueActionMenuChoice) tea.Cmd {
	if m.queueActionTarget == nil {
		return nil
	}

	switch choice {
	case QueueActionLabel:
		return m.tagSelectedQueueItem()
	case QueueActionClose:
		return m.closeSelectedQueueItem()
	}
	return nil
}

// tagSelectedQueueItem tags the bead stored in queueActionTarget for dispatch.
func (m *Model) tagSelectedQueueItem() tea.Cmd {
	if m.queueActionTarget == nil {
		return nil
	}
	item := m.queueActionTarget
	if m.OnTagBead == nil {
		m.setStatus(fmt.Sprintf("Label action unavailable for %s", item.BeadID), false)
		return nil
	}
	m.setStatus(fmt.Sprintf("Tagging %s…", item.BeadID), false)
	beadID, anvil := item.BeadID, item.Anvil
	cb := m.OnTagBead
	return func() tea.Msg {
		return QueueActionResultMsg{BeadID: beadID, Action: "tag", Err: cb(beadID, anvil)}
	}
}

// closeSelectedQueueItem closes the bead stored in queueActionTarget.
func (m *Model) closeSelectedQueueItem() tea.Cmd {
	if m.queueActionTarget == nil {
		return nil
	}
	item := m.queueActionTarget
	if m.OnCloseBead == nil {
		m.setStatus(fmt.Sprintf("Close action unavailable for %s", item.BeadID), false)
		return nil
	}
	m.setStatus(fmt.Sprintf("Closing %s…", item.BeadID), false)
	beadID, anvil := item.BeadID, item.Anvil
	cb := m.OnCloseBead
	return func() tea.Msg {
		return QueueActionResultMsg{BeadID: beadID, Action: "close", Err: cb(beadID, anvil)}
	}
}

// renderQueueActionMenu renders the queue action menu overlay centered on screen.
func (m *Model) renderQueueActionMenu() string {
	if m.queueActionTarget == nil {
		return ""
	}

	menuWidth := 68
	contentWidth := menuWidth - actionMenuStyle.GetHorizontalFrameSize()
	labels := queueActionMenuLabels()

	var lines []string
	title := fmt.Sprintf("Actions for %s", m.queueActionTarget.BeadID)
	lines = append(lines, actionMenuTitleStyle.Render(title))

	if m.queueActionTarget.Title != "" {
		// Sanitize and word-wrap title to fit popup width, max 2 lines.
		safeTitle := sanitizeTitle(m.queueActionTarget.Title)
		wrapped := wordWrap(safeTitle, contentWidth)
		if len(wrapped) <= 2 {
			for _, line := range wrapped {
				lines = append(lines, dimStyle.Render(line))
			}
		} else {
			// Show first line as-is, truncate second line and append ellipsis.
			lines = append(lines, dimStyle.Render(wrapped[0]))
			second := []rune(wrapped[1])
			if len(second) > contentWidth-3 {
				second = second[:contentWidth-3]
			}
			lines = append(lines, dimStyle.Render(string(second)+"..."))
		}
	}
	if m.queueActionTarget.Description != "" {
		lines = append(lines, "")
		// Word-wrap description to fit popup width, max 5 lines.
		wrapped := wordWrap(m.queueActionTarget.Description, contentWidth)
		if len(wrapped) <= 5 {
			for _, line := range wrapped {
				lines = append(lines, dimStyle.Render(line))
			}
		} else {
			for i := 0; i < 4; i++ {
				lines = append(lines, dimStyle.Render(wrapped[i]))
			}
			// Truncate 5th line and append ellipsis to indicate more text.
			fifth := []rune(wrapped[4])
			if len(fifth) > contentWidth-3 {
				fifth = fifth[:contentWidth-3]
			}
			lines = append(lines, dimStyle.Render(string(fifth)+"..."))
		}
	}

	lines = append(lines, "")

	for i, label := range labels {
		cursor := "  "
		if i == m.queueActionMenuIdx {
			cursor = "> "
			label = actionMenuSelectedStyle.Render(label)
		} else {
			label = dimStyle.Render(label)
		}
		lines = append(lines, cursor+label)
	}

	lines = append(lines, "")
	lines = append(lines, dimStyle.Render("Enter: select • Esc: close"))

	content := strings.Join(lines, "\n")
	popup := actionMenuStyle.Width(menuWidth).Render(content)

	return popup
}

// logViewerDimensions returns the (viewportWidth, viewportHeight) for the log viewer.
func (m *Model) logViewerDimensions() (int, int) {
	viewerWidth := m.width - 8
	if viewerWidth < 40 {
		viewerWidth = 40
	}
	viewerHeight := m.height - 6
	if viewerHeight < 10 {
		viewerHeight = 10
	}
	// Use style frame size methods instead of hardcoded offsets so the layout
	// stays correct if border or padding values change.
	vpWidth := viewerWidth - logViewerStyle.GetHorizontalFrameSize()
	// Fixed content lines: title + blank line + blank line + footer = 4
	vpHeight := viewerHeight - logViewerStyle.GetVerticalFrameSize() - 4
	if vpWidth < 1 {
		vpWidth = 1
	}
	if vpHeight < 1 {
		vpHeight = 1
	}
	return vpWidth, vpHeight
}

// renderLogViewer renders the log viewer overlay.
func (m *Model) renderLogViewer() string {
	viewerWidth := m.width - 8
	if viewerWidth < 40 {
		viewerWidth = 40
	}
	viewerHeight := m.height - 6
	if viewerHeight < 10 {
		viewerHeight = 10
	}

	var lines []string
	lines = append(lines, actionMenuTitleStyle.Render(truncate(m.logViewerTitle, viewerWidth-logViewerStyle.GetHorizontalFrameSize())))
	lines = append(lines, "")

	if m.logViewerEmpty {
		lines = append(lines, dimStyle.Render("(empty log)"))
	} else {
		lines = append(lines, m.logViewPort.View())
	}

	lines = append(lines, "")
	scrollPct := int(m.logViewPort.ScrollPercent() * 100)
	lines = append(lines, dimStyle.Render(fmt.Sprintf("j/k/mouse: scroll • Esc: close  %d%%", scrollPct)))

	content := strings.Join(lines, "\n")
	return logViewerStyle.Width(viewerWidth).Height(viewerHeight).Render(content)
}

// Precompute section header strings once to avoid per-render allocations.
var (
	sectionHeaderReady      = lipgloss.NewStyle().Bold(true).Foreground(colorSuccess).Render("── Ready ──")
	sectionHeaderUnlabeled  = lipgloss.NewStyle().Bold(true).Foreground(colorWarning).Render("── Unlabeled ──")
	sectionHeaderInProgress = lipgloss.NewStyle().Bold(true).Foreground(colorInfo).Render("── In Progress ──")
)

// sectionHeaders maps section identifiers to their precomputed styled header strings.
var sectionHeaders = map[string]string{
	"ready":       sectionHeaderReady,
	"unlabeled":   sectionHeaderUnlabeled,
	"in_progress": sectionHeaderInProgress,
}

// renderQueue renders the queue panel. When multiple anvils are present,
// beads are grouped under collapsible anvil headers. With a single anvil
// the flat section-header layout is preserved.
func (m *Model) renderQueue(width, height int) string {
	m.ensureQueueNav()

	style := panelStyle.Width(width)
	if m.focused == PanelQueue {
		style = focusedPanelStyle.Width(width)
	}

	title := panelTitleStyle.Render(fmt.Sprintf("Queue (%d)", len(m.queue)))

	var lines []string
	lines = append(lines, title)

	m.ensureQueueNav()

	if len(m.queueNavItems) == 0 {
		lines = append(lines, dimStyle.Render("No pending beads"))
	} else {
		// Count beads per anvil for header display.
		anvilCounts := map[string]int{}
		for _, item := range m.queue {
			anvilCounts[item.Anvil]++
		}

		type displayRow struct {
			text   string
			navIdx int // index into queueNavItems; -1 for non-selectable rows
		}
		var rows []displayRow
		lastSection := ""

		for ni, nav := range m.queueNavItems {
			if nav.isAnvil {
				// Collapsible anvil header.
				arrow := "▸"
				if m.queueExpandedAnvils[nav.anvilName] {
					arrow = "▾"
				}
				headerText := fmt.Sprintf("%s %s (%d)", arrow, nav.anvilName, anvilCounts[nav.anvilName])
				if ni == m.queueVP.cursor {
					headerText = selectedStyle.Render(headerText)
				}
				rows = append(rows, displayRow{text: headerText, navIdx: ni})
				lastSection = "" // reset for each anvil
				continue
			}

			item := m.queue[nav.beadIdx]
			indent := ""
			if m.queueGrouped {
				indent = "  "
			}

			// Section header within the (expanded) group.
			if item.Section != lastSection {
				if hdr, ok := sectionHeaders[item.Section]; ok {
					rows = append(rows, displayRow{text: indent + hdr, navIdx: -1})
				}
				lastSection = item.Section
			}

			// Main bead line.
			priority := priorityStyle(item.Priority)
			anvilSuffix := ""
			if !m.queueGrouped {
				anvilSuffix = " " + dimStyle.Render(item.Anvil)
			}
			mainLine := fmt.Sprintf("%s%s %s%s", indent, priority, item.BeadID, anvilSuffix)
			if ni == m.queueVP.cursor {
				mainLine = selectedStyle.Render(mainLine)
			}
			rows = append(rows, displayRow{text: mainLine, navIdx: ni})

			// Title line.
			titleText := sanitizeTitle(item.Title)
			if titleText == "" {
				titleText = "(no title)"
			}
			titleIndent := indent + "    "
			titleLine := titleIndent + dimStyle.Render(truncate(titleText, width-8))
			if ni == m.queueVP.cursor {
				titleLine = titleIndent + selectedStyle.Render(truncate(titleText, width-8))
			}
			rows = append(rows, displayRow{text: titleLine, navIdx: -1})

			// Assignee line for in-progress beads.
			if item.Section == "in_progress" && item.Assignee != "" {
				assigneeLine := titleIndent + dimStyle.Render("@ "+item.Assignee)
				if ni == m.queueVP.cursor {
					assigneeLine = titleIndent + selectedStyle.Render("@ "+item.Assignee)
				}
				rows = append(rows, displayRow{text: assigneeLine, navIdx: -1})
			}
		}

		// Find display offset of the selected item so the view scrolls with selection.
		selectedDisplayIdx := 0
		for di, row := range rows {
			if row.navIdx == m.queueVP.cursor {
				selectedDisplayIdx = di
				break
			}
		}

		maxVisible := height - 3
		if maxVisible < 1 {
			maxVisible = 1
		}
		start := selectedDisplayIdx - maxVisible/2
		if start < 0 {
			start = 0
		}
		end := start + maxVisible
		if end > len(rows) {
			end = len(rows)
			start = end - maxVisible
			if start < 0 {
				start = 0
			}
		}
		for i := start; i < end; i++ {
			lines = append(lines, rows[i].text)
		}
	}

	if height <= 0 {
		return style.Render("")
	}
	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// renderLeftColumn splits the left column into Queue (top), optionally Crucibles
// (when active), Ready to Merge, and Needs Attention (bottom). When crucibles are
// active, it renders 4 stacked panels; otherwise 3.
func (m *Model) renderLeftColumn(width, topHeight, bottomHeight int) string {
	height := topHeight + bottomHeight

	// Count panels to render: Queue + ReadyToMerge + NeedsAttention + optionally Crucibles.
	hasCrucibles := len(m.crucibles) > 0
	panelN := 3
	if hasCrucibles {
		panelN = 4
	}

	// Each sub-panel adds 2 border lines. Deduct extra borders beyond one panel.
	innerHeight := height - (panelN-1)*2
	if innerHeight < 0 {
		innerHeight = 0
	}

	if hasCrucibles {
		// Queue 40%, Crucible 20%, ReadyToMerge 15%, NeedsAttention 25%.
		queueHeight := innerHeight * 4 / 10
		crucibleHeight := innerHeight * 2 / 10
		mergeHeight := innerHeight * 15 / 100
		if innerHeight < 12 {
			queueHeight = innerHeight
			crucibleHeight = 0
			mergeHeight = 0
		} else {
			queueHeight = max(queueHeight, 5)
			crucibleHeight = max(crucibleHeight, 4)
			mergeHeight = max(mergeHeight, 3)
		}
		attentionHeight := innerHeight - queueHeight - crucibleHeight - mergeHeight

		top := m.renderQueue(width, queueHeight)
		cruc := m.renderCrucibles(width, crucibleHeight)
		mid := m.renderReadyToMerge(width, mergeHeight)
		bot := m.renderNeedsAttention(width, attentionHeight)
		return lipgloss.JoinVertical(lipgloss.Left, top, cruc, mid, bot)
	}

	// No active crucibles — original 3-panel layout.
	innerHeight = height - 4
	if innerHeight < 0 {
		innerHeight = 0
	}
	queueHeight := innerHeight * 5 / 10
	mergeHeight := innerHeight * 2 / 10
	if innerHeight < 8 {
		queueHeight = innerHeight
		mergeHeight = 0
	} else {
		queueHeight = max(queueHeight, 5)
		mergeHeight = max(mergeHeight, 3)
	}
	attentionHeight := innerHeight - queueHeight - mergeHeight

	top := m.renderQueue(width, queueHeight)
	middle := m.renderReadyToMerge(width, mergeHeight)
	bottom := m.renderNeedsAttention(width, attentionHeight)
	return lipgloss.JoinVertical(lipgloss.Left, top, middle, bottom)
}

// cruciblePhaseStyle returns a styled phase label for Crucible status display.
// frame is the current spinner animation frame, used for active phases.
func cruciblePhaseStyle(phase, frame string) string {
	switch phase {
	case "dispatching":
		return lipgloss.NewStyle().Foreground(colorBlue).Render(frame + " DISPATCH")
	case "final_pr":
		return lipgloss.NewStyle().Foreground(colorBlueCyan).Render(frame + " FINAL PR")
	case "complete":
		return lipgloss.NewStyle().Foreground(colorGreen).Render("✓ COMPLETE")
	case "paused":
		return lipgloss.NewStyle().Foreground(colorDanger).Render("⏸ PAUSED")
	case "started":
		return lipgloss.NewStyle().Foreground(colorWarning).Render(frame + " STARTED")
	default:
		return lipgloss.NewStyle().Foreground(colorMuted).Render("? " + phase)
	}
}

// renderCrucibles renders the Crucibles sub-panel showing active epic orchestrations.
func (m *Model) renderCrucibles(width, height int) string {
	style := panelStyle.Width(width)
	if m.focused == PanelCrucibles {
		style = focusedPanelStyle.Width(width)
	}

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	title := titleStyle.Render(fmt.Sprintf("Crucibles (%d)", len(m.crucibles)))

	var lines []string
	lines = append(lines, title)

	if len(m.crucibles) == 0 {
		lines = append(lines, dimStyle.Render("None"))
	} else {
		// Each crucible uses 3 display lines: phase+parent, progress, current child.
		maxLines := height - 3
		linesPerItem := 3
		maxItems := maxLines / linesPerItem
		if maxItems < 1 {
			maxItems = 1
		}

		m.crucibleVP.AdjustViewport(maxItems, len(m.crucibles))
		start, end := m.crucibleVP.VisibleRange(maxItems, len(m.crucibles))
		frame := SpinnerFrames[m.spinnerFrame%len(SpinnerFrames)]

		for i := start; i < end; i++ {
			c := m.crucibles[i]
			selected := m.focused == PanelCrucibles && i == m.crucibleVP.cursor

			// Line 1: phase icon + parent ID + anvil
			baseLine1 := fmt.Sprintf("%s %s %s", cruciblePhaseStyle(c.Phase, frame), c.ParentID, dimStyle.Render(c.Anvil))
			line1 := baseLine1
			if selected {
				line1 = selectedStyle.Render(fmt.Sprintf("▸ %s", baseLine1))
			}

			// Line 2: progress bar + fraction
			progress := fmt.Sprintf("  Children: %d/%d", c.CompletedChildren, c.TotalChildren)
			if c.TotalChildren > 0 {
				barWidth := width - 22
				if barWidth < 5 {
					barWidth = 5
				}
				filled := barWidth * c.CompletedChildren / c.TotalChildren
				bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
				progress = fmt.Sprintf("  %s %d/%d", bar, c.CompletedChildren, c.TotalChildren)
			}

			// Line 3: current child or title (truncated)
			var line3 string
			if c.CurrentChild != "" {
				line3 = dimStyle.Render(fmt.Sprintf("  → %s", c.CurrentChild))
			} else {
				titleText := c.ParentTitle
				maxTitle := width - 6
				if maxTitle < 4 {
					maxTitle = 4
				}
				if len(titleText) > maxTitle {
					titleText = titleText[:maxTitle-3] + "..."
				}
				line3 = dimStyle.Render(fmt.Sprintf("  %s", titleText))
			}

			lines = append(lines, line1, progress, line3)
		}
	}

	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// renderRightColumn renders Live Activity (top) + Events (bottom), mirroring
// the left column's Queue + Needs Attention layout.
func (m *Model) renderRightColumn(width, topHeight, bottomHeight int) string {
	return m.renderStackedColumn(width, topHeight, bottomHeight,
		m.renderWorkerActivity, m.renderEvents)
}

// renderStackedColumn renders two sub-panels stacked vertically.
// Each lipgloss panel adds 2 border lines (top + bottom) to its height parameter.
// Two stacked panels therefore produce 4 border lines total, whereas the single
// center column produces only 2. The bottom panel's height is reduced by 2 so
// that topHeight+bottomHeight+2 (single-panel rendered lines) equals
// (topHeight+2)+(bottomHeight-2+2) = topHeight+bottomHeight+2 for the stacked column.
func (m *Model) renderStackedColumn(width, topHeight, bottomHeight int,
	renderTop, renderBottom func(int, int) string) string {
	innerBottom := bottomHeight - 2
	if innerBottom < 0 {
		innerBottom = 0
	}
	top := renderTop(width, topHeight)
	bottom := renderBottom(width, innerBottom)
	return lipgloss.JoinVertical(lipgloss.Left, top, bottom)
}

// attentionReasonIcon returns a distinct icon and short label for each attention reason category.
func attentionReasonIcon(cat AttentionReason) string {
	switch cat {
	case AttentionDispatchExhausted:
		return lipgloss.NewStyle().Foreground(colorDanger).Render("⊘ DISPATCH")
	case AttentionCIFixExhausted:
		return lipgloss.NewStyle().Foreground(colorAccent).Render("🔧 CI FIX")
	case AttentionReviewFixExhausted:
		return lipgloss.NewStyle().Foreground(colorPink).Render("📝 REVIEW")
	case AttentionRebaseExhausted:
		return lipgloss.NewStyle().Foreground(colorWarning).Render("↻ REBASE")
	case AttentionClarification:
		return lipgloss.NewStyle().Foreground(colorInfo).Render("? CLARIFY")
	case AttentionStalled:
		return lipgloss.NewStyle().Foreground(colorDanger).Render("◼ STALLED")
	default:
		return lipgloss.NewStyle().Foreground(colorMuted).Render("⚠ UNKNOWN")
	}
}

// renderNeedsAttention renders the Needs Attention sub-panel showing beads
// that require human intervention (e.g. exhausted dispatch/CI-fix/review-fix/rebase
// attempts, clarification requests, or stalled workers).
func (m *Model) renderNeedsAttention(width, height int) string {
	style := panelStyle.Width(width)
	if m.focused == PanelNeedsAttention {
		style = focusedPanelStyle.Width(width)
	}

	title := needsAttentionTitleStyle.Render(fmt.Sprintf("Needs Attention (%d)", len(m.needsAttention)))

	var lines []string
	lines = append(lines, title)

	if len(m.needsAttention) == 0 {
		lines = append(lines, dimStyle.Render("None"))
	} else {
		// Each item uses 2 lines (bead + reason), so halve the visible slot count.
		maxLines := height - 3
		maxItems := maxLines / 2
		if maxItems < 1 {
			maxItems = 1
		}
		m.needsAttnVP.AdjustViewport(maxItems, len(m.needsAttention))
		start, end := m.needsAttnVP.VisibleRange(maxItems, len(m.needsAttention))
		for i := start; i < end; i++ {
			item := m.needsAttention[i]
			anvil := dimStyle.Render(item.Anvil)
			label := item.BeadID
			if item.PRNumber > 0 {
				label = fmt.Sprintf("PR #%d %s", item.PRNumber, item.BeadID)
			}
			icon := attentionReasonIcon(item.ReasonCategory)
			beadLine := fmt.Sprintf("%s %s %s", icon, label, anvil)
			if i == m.needsAttnVP.cursor {
				beadLine = selectedStyle.Render(beadLine)
			}
			lines = append(lines, beadLine)

			// Second line: reason detail (truncated)
			reason := item.Reason
			if reason == "" {
				reason = "(no reason)"
			}
			reasonLine := "  " + dimStyle.Render(truncate(reason, width-6))
			if i == m.needsAttnVP.cursor {
				reasonLine = "  " + selectedStyle.Render(truncate(reason, width-6))
			}
			lines = append(lines, reasonLine)
		}
	}

	if height <= 0 {
		return style.Render("")
	}
	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// renderReadyToMerge renders the Ready to Merge sub-panel showing PRs that
// meet all conditions for merging.
func (m *Model) renderReadyToMerge(width, height int) string {
	if height <= 0 {
		return ""
	}

	style := panelStyle.Width(width)
	if m.focused == PanelReadyToMerge {
		style = focusedPanelStyle.Width(width)
	}

	title := readyToMergeTitleStyle.Render(fmt.Sprintf("Ready to Merge (%d)", len(m.readyToMerge)))

	var lines []string
	lines = append(lines, title)

	if len(m.readyToMerge) == 0 {
		lines = append(lines, dimStyle.Render("None"))
	} else if height > 3 {
		maxItems := height - 3
		if maxItems < 1 {
			maxItems = 1
		}
		m.readyToMergeVP.AdjustViewport(maxItems, len(m.readyToMerge))
		start, end := m.readyToMergeVP.VisibleRange(maxItems, len(m.readyToMerge))
		for i := start; i < end; i++ {
			item := m.readyToMerge[i]
			anvil := dimStyle.Render(item.Anvil)
			line := fmt.Sprintf("PR #%d %s %s", item.PRNumber, item.BeadID, anvil)
			if i == m.readyToMergeVP.cursor {
				line = selectedStyle.Render(line)
			}
			lines = append(lines, line)
		}
	}

	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// renderMergeMenu renders the merge action menu overlay.
func (m *Model) renderMergeMenu() string {
	if m.mergeTarget == nil {
		return ""
	}

	menuWidth := 68
	if m.width > 0 && m.width < menuWidth {
		menuWidth = m.width
	}
	contentWidth := menuWidth - actionMenuStyle.GetHorizontalFrameSize()
	labels := mergeMenuLabels()

	var lines []string
	title := fmt.Sprintf("PR #%d — %s", m.mergeTarget.PRNumber, m.mergeTarget.BeadID)
	lines = append(lines, actionMenuTitleStyle.Render(title))

	if m.mergeTarget.Title != "" {
		// Sanitize and word-wrap PR title to fit popup width, max 2 lines.
		safeTitle := sanitizeTitle(m.mergeTarget.Title)
		wrapped := wordWrap(safeTitle, contentWidth)
		if len(wrapped) <= 2 {
			for _, line := range wrapped {
				lines = append(lines, dimStyle.Render(line))
			}
		} else {
			// Show first line as-is, truncate second line and append ellipsis.
			lines = append(lines, dimStyle.Render(wrapped[0]))
			second := []rune(wrapped[1])
			maxSecond := contentWidth - 3
			if maxSecond < 0 {
				maxSecond = 0
			}
			if len(second) > maxSecond {
				second = second[:maxSecond]
			}
			lines = append(lines, dimStyle.Render(string(second)+"..."))
		}
	}

	lines = append(lines, "")

	for i, label := range labels {
		cursor := "  "
		if i == m.mergeMenuIdx {
			cursor = "> "
			label = actionMenuSelectedStyle.Render(label)
		} else {
			label = dimStyle.Render(label)
		}
		lines = append(lines, cursor+label)
	}

	lines = append(lines, "")
	lines = append(lines, dimStyle.Render("Enter: select • Esc: close"))

	content := strings.Join(lines, "\n")
	return actionMenuStyle.Width(menuWidth).Render(content)
}

// MergeResultMsg is delivered asynchronously when a merge IPC call completes.
type MergeResultMsg struct {
	PRNumber int
	Err      error
}

// executeMergeAction returns a tea.Cmd that runs the merge IPC call asynchronously,
// keeping the Bubbletea UI responsive during the (potentially 60s) operation.
func (m *Model) executeMergeAction(choice MergeMenuChoice) tea.Cmd {
	if m.mergeTarget == nil {
		return nil
	}
	pr := m.mergeTarget
	switch choice {
	case MergeActionMerge:
		if m.OnMergePR == nil {
			m.setStatus(fmt.Sprintf("Merge action unavailable for PR #%d", pr.PRNumber), false)
			return nil
		}
		m.setStatus(fmt.Sprintf("Merging PR #%d…", pr.PRNumber), false)
		prID, prNumber, anvil := pr.PRID, pr.PRNumber, pr.Anvil
		cb := m.OnMergePR
		return func() tea.Msg {
			return MergeResultMsg{PRNumber: prNumber, Err: cb(prID, prNumber, anvil)}
		}
	}
	return nil
}

// renderWorkers delegates to renderWorkerList for the center column.
func (m *Model) renderWorkers(width, height int) string {
	return m.renderWorkerList(width, height)
}

func (m *Model) renderWorkerList(width, height int) string {
	style := panelStyle.Width(width)
	if m.focused == PanelWorkers {
		style = focusedPanelStyle.Width(width)
	}

	title := panelTitleStyle.Render(fmt.Sprintf("Workers (%d)", len(m.workers)))

	var lines []string
	lines = append(lines, title)

	if len(m.workers) == 0 {
		lines = append(lines, dimStyle.Render("No active workers"))
	} else {
		// Each worker uses 2 lines (main + title), so halve the visible slot count.
		maxLines := height - 4 // height-2 (borders) - 2 (title + margin)
		slotsPerWorker := 2
		maxWorkers := maxLines / slotsPerWorker
		if maxWorkers < 1 {
			maxWorkers = 1
		}
		m.workerVP.AdjustViewport(maxWorkers, len(m.workers))
		start, end := m.workerVP.VisibleRange(maxWorkers, len(m.workers))
		frame := SpinnerFrames[m.spinnerFrame%len(SpinnerFrames)]
		for i := start; i < end; i++ {
			item := m.workers[i]
			status := workerStatusStyle(item.Status, frame)
			phase := phaseTag(item.Type)
			beadAndPR := item.BeadID
			if item.PRNumber > 0 {
				beadAndPR = fmt.Sprintf("%s PR#%d", item.BeadID, item.PRNumber)
			}
			mainLine := fmt.Sprintf("%s %s %s %s %s",
				status, phase, beadAndPR,
				dimStyle.Render(item.Anvil), item.Duration)
			if i == m.workerVP.cursor {
				mainLine = selectedStyle.Render(mainLine)
			}
			lines = append(lines, mainLine)

			// Second line: indented bead title (sanitized to strip control chars)
			titleText := sanitizeTitle(item.Title)
			if titleText == "" {
				titleText = "(no title)"
			}
			titleLine := "    " + dimStyle.Render(truncate(titleText, width-8))
			if i == m.workerVP.cursor {
				titleLine = "    " + selectedStyle.Render(truncate(titleText, width-8))
			}
			lines = append(lines, titleLine)
		}
	}

	if height <= 0 {
		return style.Render("")
	}
	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// renderCenterColumn renders Workers (top) + Usage (bottom), splitting the
// center column using the same stacked pattern as the right column.
func (m *Model) renderCenterColumn(width, topHeight, bottomHeight int) string {
	fullHeight := topHeight + bottomHeight

	// Usage panel gets a compact fixed height (~8 lines content + 2 border = 10).
	usagePanelHeight := 10
	if fullHeight < 20 {
		// Terminal too small for a split — render workers only.
		return m.renderWorkerList(width, fullHeight)
	}

	workerHeight := fullHeight - usagePanelHeight

	top := m.renderWorkerList(width, workerHeight)
	bottom := m.renderUsagePanel(width, usagePanelHeight-2) // -2 for inner content (border adds 2)
	return lipgloss.JoinVertical(lipgloss.Left, top, bottom)
}

// renderUsagePanel renders a compact panel showing today's per-provider costs,
// copilot premium requests, and total cost vs limit.
func (m *Model) renderUsagePanel(width, height int) string {
	style := panelStyle.Width(width)
	if m.focused == PanelUsage {
		style = focusedPanelStyle.Width(width)
	}

	title := panelTitleStyle.Render("Usage")

	var lines []string
	lines = append(lines, title)

	if len(m.usage.Providers) == 0 && m.usage.TotalCost == 0 && m.usage.CopilotUsed == 0 {
		lines = append(lines, dimStyle.Render("No usage today"))
	} else {
		// Per-provider lines
		for _, p := range m.usage.Providers {
			name := p.Provider
			if len(name) > 0 {
				name = strings.ToUpper(name[:1]) + name[1:]
			}
			if len(name) > 8 {
				name = name[:8]
			}
			// Pad provider name to 8 chars for alignment
			for len(name) < 8 {
				name += " "
			}
			line := fmt.Sprintf("%s %s  %s in / %s out",
				name, FormatCost(p.Cost),
				FormatTokens(p.InputTokens), FormatTokens(p.OutputTokens))
			lines = append(lines, line)
		}

		// Copilot premium requests (if any)
		if m.usage.CopilotUsed > 0 || m.usage.CopilotLimit > 0 {
			copilotLine := fmt.Sprintf("Copilot  %s", formatCopilotRequests(m.usage.CopilotUsed))
			if m.usage.CopilotLimit > 0 {
				copilotLine += fmt.Sprintf("/%d premium req", m.usage.CopilotLimit)
			} else {
				copilotLine += " premium req"
			}
			lines = append(lines, copilotLine)
		}

		// Total line
		totalLine := fmt.Sprintf("Total    %s", FormatCost(m.usage.TotalCost))
		if m.usage.CostLimit > 0 {
			totalLine += fmt.Sprintf(" / %s limit", FormatCost(m.usage.CostLimit))
		}
		lines = append(lines, totalLine)
	}

	if height <= 0 {
		return style.Render("")
	}
	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// renderWorkerActivity renders the activity panel: a live log view for the
// currently selected worker, parsed from its stream-json log file.
// Groups of consecutive same-type events are collapsible; press Enter to expand
// and Esc to collapse. The newest activity appears at the top.
func (m *Model) renderWorkerActivity(width, height int) string {
	style := panelStyle.Width(width)
	if m.focused == PanelLiveActivity {
		style = focusedPanelStyle.Width(width)
	}

	// Build title with selected worker info
	titleText := "Live Activity"
	if len(m.workers) > 0 && m.workerVP.cursor < len(m.workers) {
		w := m.workers[m.workerVP.cursor]
		titleText = fmt.Sprintf("Live Activity — %s %s", w.BeadID, dimStyle.Render(w.Anvil))
	}
	title := activityPanelTitleStyle.Render(titleText)

	var lines []string
	lines = append(lines, title)

	if len(m.activityNavItems) == 0 {
		if len(m.workers) == 0 {
			lines = append(lines, dimStyle.Render("No active workers"))
		} else {
			lines = append(lines, dimStyle.Render("Waiting for output..."))
		}
	} else {
		// height-2 (borders) - 2 (title + margin) = height-4
		maxVisible := height - 4
		if maxVisible < 1 {
			maxVisible = 1
		}
		total := len(m.activityNavItems)
		m.activityVP.ClampToTotal(total)
		m.activityVP.AdjustViewport(maxVisible, total)
		start, end := m.activityVP.VisibleRange(maxVisible, total)
		contentWidth := width - 4
		if contentWidth < 10 {
			contentWidth = 10
		}
		for i := start; i < end; i++ {
			nav := m.activityNavItems[i]
			isCursor := m.focused == PanelLiveActivity && i == m.activityVP.cursor
			if nav.isGroupHeader {
				// Group header (▸ collapsed / ▾ expanded)
				line := truncate(nav.text, contentWidth)
				if isCursor {
					line = selectedStyle.Render(line)
				} else {
					line = activityGroupHeaderStyle.Render(line)
				}
				lines = append(lines, line)
			} else {
				// Expanded child line — wrap to reduced width, then indent each line
				childWidth := contentWidth - 2
				if childWidth < 10 {
					childWidth = 10
				}
				wrapped := wordWrap(nav.text, childWidth)
				for wi, wl := range wrapped {
					indented := "  " + wl
					if isCursor && wi == 0 {
						indented = selectedStyle.Render(indented)
					}
					lines = append(lines, indented)
				}
			}
		}
	}

	if height <= 0 {
		return style.Render("")
	}
	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// renderEvents renders the event log panel with word-wrapped messages.
func (m *Model) renderEvents(width, height int) string {
	style := panelStyle.Width(width)
	if m.focused == PanelEvents {
		style = focusedPanelStyle.Width(width)
	}

	scrollIndicator := ""
	if !m.eventAutoScroll {
		scrollIndicator = dimStyle.Render(" ⏸")
	}
	title := panelTitleStyle.Render(fmt.Sprintf("Events (%d)%s", len(m.events), scrollIndicator))

	var lines []string
	lines = append(lines, title)

	contentHeight := height - 3 // title + border rows

	if len(m.events) == 0 {
		lines = append(lines, dimStyle.Render("No events"))
	} else {
		allLines := m.renderAllEventLines(width)
		visible := visibleItems(m.eventScroll, len(allLines), contentHeight)
		for i := visible.start; i < visible.end; i++ {
			lines = append(lines, allLines[i])
		}
	}

	if height <= 0 {
		return style.Render("")
	}
	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// renderAllEventLines flattens all events into a single slice of rendered lines.
// It uses eventLineCount for the selection-mapping pass to avoid a double full render.
// Caches the results to avoid redundant work.
func (m *Model) renderAllEventLines(width int) []string {
	// Calculate what the selected event index WOULD be.
	selectedEventIdx := -1
	cumulative := 0
	for i, event := range m.events {
		count := m.eventLineCount(event, width)
		if selectedEventIdx == -1 && cumulative+count > m.eventScroll {
			selectedEventIdx = i
		}
		cumulative += count
	}

	// Check if cache is valid.
	if m.eventWidthCache == width &&
		m.eventCountCache == len(m.events) &&
		m.eventSelectedIdxCache == selectedEventIdx &&
		m.eventRevisionCache == m.eventRevision &&
		m.eventLinesCache != nil {
		return m.eventLinesCache
	}

	// Cache invalid, perform full render.
	var allLines []string
	for i, event := range m.events {
		allLines = append(allLines, m.renderEventLines(event, i == selectedEventIdx, width)...)
	}

	// Update cache
	m.eventLinesCache = allLines
	m.eventWidthCache = width
	m.eventCountCache = len(m.events)
	m.eventSelectedIdxCache = selectedEventIdx
	m.eventRevisionCache = m.eventRevision

	return allLines
}

type eventLayout struct {
	interiorWidth int
	prefixVisLen  int
	msgWidth      int
	beadTag       string
}

func (m *Model) getEventLayout(item EventItem, panelWidth int) eventLayout {
	beadTag := ""
	if item.BeadID != "" {
		beadTag = "[" + item.BeadID + "] "
	}

	// Interior width: subtract border (1 each side = 2) + padding (1 each side = 2) = 4 total
	interiorWidth := panelWidth - eventPanelInteriorPadding
	if interiorWidth < eventPanelMinWidth {
		interiorWidth = eventPanelMinWidth
	}

	// Visual prefix length: "HH:MM:SS "(9) + type + " " + beadTag
	prefixVisLen := eventTimestampWidth + len(item.Type) + 1 + len(beadTag)
	msgWidth := interiorWidth - prefixVisLen

	return eventLayout{
		interiorWidth: interiorWidth,
		prefixVisLen:  prefixVisLen,
		msgWidth:      msgWidth,
		beadTag:       beadTag,
	}
}

// eventTotalLineCount returns the total number of rendered lines across all events
// without allocating styled strings. Used by scrollDown for cheap bounds checking.
func (m *Model) eventTotalLineCount(width int) int {
	if m.eventWidthCache == width && m.eventCountCache == len(m.events) && m.eventRevisionCache == m.eventRevision && m.eventLinesCache != nil {
		return len(m.eventLinesCache)
	}

	total := 0
	for _, event := range m.events {
		total += m.eventLineCount(event, width)
	}
	return total
}

// eventLineCount returns the number of lines renderEventLines would produce for item
// without performing any string formatting or allocation beyond wrap counting.
func (m *Model) eventLineCount(item EventItem, width int) int {
	layout := m.getEventLayout(item, width)

	var n int
	if layout.msgWidth < eventMsgMinWidth {
		// header line + wrapped message lines
		n = 1 + wordWrapCount(item.Message, layout.interiorWidth-2)
	} else {
		n = wordWrapCount(item.Message, layout.msgWidth)
	}
	if n > maxEventLines {
		n = maxEventLines
	}
	return n
}

// renderEventLines renders a single event as one or more wrapped lines.
// The timestamp and event type stay on the first line; the message body wraps
// onto continuation lines if it exceeds the available width.
// At most maxEventLines lines are produced to prevent long error messages from
// overflowing into adjacent hearth panels.
const maxEventLines = 3

func (m *Model) renderEventLines(item EventItem, selected bool, panelWidth int) []string {
	layout := m.getEventLayout(item, panelWidth)

	capWrapped := func(wrapped []string) []string {
		if len(wrapped) > maxEventLines {
			wrapped = append(wrapped[:maxEventLines-1], "…")
		}
		return wrapped
	}

	var lines []string
	if layout.msgWidth < eventMsgMinWidth {
		// Prefix is too wide for the current panel width, put message on its own lines
		header := fmt.Sprintf("%s %s %s",
			dimStyle.Render(item.Timestamp),
			eventTypeStyle(item.Type),
			dimStyle.Render(layout.beadTag))
		if selected {
			header = selectedStyle.Render(header)
		}
		lines = append(lines, header)

		// Message starts on next line, indented slightly
		wrapped := capWrapped(wordWrap(item.Message, layout.interiorWidth-2))
		for _, part := range wrapped {
			line := "  " + dimStyle.Render(part)
			if selected {
				line = selectedStyle.Render(line)
			}
			lines = append(lines, line)
		}
	} else {
		// Message starts on the same line as the prefix
		wrapped := capWrapped(wordWrap(item.Message, layout.msgWidth))
		if len(wrapped) == 0 {
			wrapped = []string{""}
		}

		indent := strings.Repeat(" ", layout.prefixVisLen)
		for i, part := range wrapped {
			var line string
			if i == 0 {
				line = fmt.Sprintf("%s %s %s%s",
					dimStyle.Render(item.Timestamp),
					eventTypeStyle(item.Type),
					dimStyle.Render(layout.beadTag),
					part)
			} else {
				line = indent + dimStyle.Render(part)
			}
			if selected {
				line = selectedStyle.Render(line)
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// --- Messages for updating panel data ---

// UpdateQueueMsg updates the queue panel.
type UpdateQueueMsg struct{ Items []QueueItem }

// QueueErrorMsg signals that reading the queue cache failed.
// The model preserves the previous queue data so the UI doesn't
// flip to "No pending beads" on a transient DB error.
type QueueErrorMsg struct{ Err error }

// UpdateCruciblesMsg updates the crucibles panel.
type UpdateCruciblesMsg struct{ Items []CrucibleItem }

// UpdateNeedsAttentionMsg updates the needs attention panel.
type UpdateNeedsAttentionMsg struct{ Items []NeedsAttentionItem }

// UpdateReadyToMergeMsg updates the ready to merge panel.
type UpdateReadyToMergeMsg struct{ Items []ReadyToMergeItem }

// NeedsAttentionErrorMsg signals that reading the needs-attention beads failed.
type NeedsAttentionErrorMsg struct{ Err error }

// ReadyToMergeErrorMsg signals that reading the ready-to-merge PRs failed.
type ReadyToMergeErrorMsg struct{ Err error }

// UpdateWorkersMsg updates the workers panel.
type UpdateWorkersMsg struct{ Items []WorkerItem }

// UpdateEventsMsg updates the event log panel.
type UpdateEventsMsg struct{ Items []EventItem }

// KillWorkerMsg requests killing the selected worker by PID.
type KillWorkerMsg struct {
	WorkerID string
	PID      int
}

// --- Styles ---

var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorAccent).
			Align(lipgloss.Center).
			Padding(0, 1)

	footerStyle = lipgloss.NewStyle().
			Foreground(colorMuted).
			Align(lipgloss.Center).
			Padding(0, 1)

	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorMuted).
			Padding(0, 1)

	focusedPanelStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorAccent).
				Padding(0, 1)

	panelTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorFg).
			MarginBottom(1)

	selectedStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorAccent)

	dimStyle = lipgloss.NewStyle().
			Foreground(colorMuted)

	activityPanelTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorSubtle).
				MarginBottom(1)

	activityGroupHeaderStyle = lipgloss.NewStyle().
					Foreground(colorSubtle).
					Bold(true)

	needsAttentionTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorDanger).
				MarginBottom(1)

	readyToMergeTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorSuccess).
				MarginBottom(1)

	actionMenuStyle = lipgloss.NewStyle().
			Border(lipgloss.DoubleBorder()).
			BorderForeground(colorAccent).
			Padding(1, 2)

	actionMenuTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorAccent)

	actionMenuSelectedStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorFg)

	logViewerStyle = lipgloss.NewStyle().
			Border(lipgloss.DoubleBorder()).
			BorderForeground(colorInfo).
			Padding(1, 2)

	statusMsgStyle = lipgloss.NewStyle().
			Foreground(colorSuccess).
			Bold(true)

	statusErrorStyle = lipgloss.NewStyle().
				Foreground(colorDanger).
				Bold(true)

	daemonConnectedStyle = lipgloss.NewStyle().
				Foreground(colorSuccess)

	daemonDisconnectedStyle = lipgloss.NewStyle().
				Foreground(colorDanger).
				Bold(true)
)

// priorityStyle returns a colored priority indicator.
func priorityStyle(p int) string {
	var c lipgloss.TerminalColor
	switch p {
	case 0:
		c = colorDanger // red (critical)
	case 1:
		c = colorAccent // orange (high)
	case 2:
		c = colorWarning // yellow (medium)
	case 3:
		c = colorInfo // blue (low)
	default:
		c = colorMuted // gray (backlog)
	}
	return lipgloss.NewStyle().Foreground(c).Render(fmt.Sprintf("P%d", p))
}

// workerStatusStyle returns a colored status indicator.
// frame is the current spinner animation frame, used for active (running/reviewing) states.
func workerStatusStyle(status, frame string) string {
	switch status {
	case "running":
		return lipgloss.NewStyle().Foreground(colorSuccess).Render(frame)
	case "reviewing":
		return lipgloss.NewStyle().Foreground(colorWarning).Render(frame)
	case "monitoring":
		return lipgloss.NewStyle().Foreground(colorBlue).Render("○")
	case "done":
		return lipgloss.NewStyle().Foreground(colorSuccess).Render("✓")
	case "failed":
		return lipgloss.NewStyle().Foreground(colorDanger).Render("✗")
	default:
		return lipgloss.NewStyle().Foreground(colorMuted).Render("○")
	}
}

// workerTypeIcon returns a short icon for the worker type.
func workerTypeIcon(t string) string {
	switch t {
	case "smith":
		return lipgloss.NewStyle().Foreground(colorAccent).Render("⚒")
	case "warden":
		return lipgloss.NewStyle().Foreground(colorInfo).Render("⛨")
	case "temper":
		return lipgloss.NewStyle().Foreground(colorWarning).Render("🔥")
	case "cifix":
		return lipgloss.NewStyle().Foreground(colorDanger).Render("🔧")
	case "reviewfix":
		return lipgloss.NewStyle().Foreground(colorPink).Render("📝")
	case "rebase":
		return lipgloss.NewStyle().Foreground(colorAccent).Render("🔀")
	default:
		return lipgloss.NewStyle().Foreground(colorMuted).Render("?")
	}
}

// phaseTag returns a colored [phase] tag for the active pipeline component.
// Colors: smith=yellow, temper=cyan, warden=magenta, bellows=blue, idle=gray.
func phaseTag(phase string) string {
	switch phase {
	case "smith":
		return lipgloss.NewStyle().Foreground(colorWarning).Render("[smith]")
	case "temper":
		return lipgloss.NewStyle().Foreground(colorCyan).Render("[temper]")
	case "warden":
		return lipgloss.NewStyle().Foreground(colorMagenta).Render("[warden]")
	case "bellows":
		return lipgloss.NewStyle().Foreground(colorBlue).Render("[bellows]")
	case "cifix":
		return lipgloss.NewStyle().Foreground(colorDanger).Render("[cifix]")
	case "reviewfix":
		return lipgloss.NewStyle().Foreground(colorPink).Render("[reviewfix]")
	case "rebase":
		return lipgloss.NewStyle().Foreground(colorAccent).Render("[rebase]")
	case "schematic":
		return lipgloss.NewStyle().Foreground(colorSkyBlue).Render("[schematic]")
	case "crucible":
		return lipgloss.NewStyle().Foreground(colorOrangeAlt).Render("[crucible]")
	default:
		return lipgloss.NewStyle().Foreground(colorMuted).Render("[idle]")
	}
}

// eventTypeStyle returns a styled event type.
func eventTypeStyle(t string) string {
	switch {
	case strings.Contains(t, "pass") || strings.Contains(t, "done") || strings.Contains(t, "merged"):
		return lipgloss.NewStyle().Foreground(colorSuccess).Render(t)
	case strings.Contains(t, "fail") || strings.Contains(t, "reject") || strings.Contains(t, "error"):
		return lipgloss.NewStyle().Foreground(colorDanger).Render(t)
	default:
		return lipgloss.NewStyle().Foreground(colorInfo).Render(t)
	}
}

// sanitizeTitle removes ANSI escape sequences, replaces newlines/carriage
// returns with spaces, and strips non-printable control characters from a
// bead title before rendering it in the TUI.
func sanitizeTitle(s string) string {
	// Replace newlines/CR with a space so the second title line stays single-line.
	s = strings.NewReplacer("\n", " ", "\r", " ").Replace(s)

	// Strip ANSI escape sequences (ESC [ ... m and similar).
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	runes := []rune(s)
	for i < len(runes) {
		if runes[i] == '\x1b' && i+1 < len(runes) && runes[i+1] == '[' {
			// Skip until the final byte of the CSI sequence (a letter).
			i += 2
			for i < len(runes) && !unicode.IsLetter(runes[i]) {
				i++
			}
			i++ // consume the terminating letter
			continue
		}
		// Skip other C0/C1 control characters (except space).
		if runes[i] < 0x20 || (runes[i] >= 0x7f && runes[i] < 0xa0) {
			i++
			continue
		}
		b.WriteRune(runes[i])
		i++
	}
	return b.String()
}

// --- Helpers ---

type visibleRange struct {
	start, end int
}

func visibleItems(scroll, total, viewHeight int) visibleRange {
	if viewHeight <= 0 {
		viewHeight = 10
	}
	start := scroll
	end := start + viewHeight
	if end > total {
		end = total
	}
	if start > total {
		start = total
	}
	return visibleRange{start: start, end: end}
}

// clampViewStart returns an updated viewStart that keeps cursor inside the
// visible [viewStart, viewStart+maxItems) window and prevents the viewport
// from leaving gaps at the end when the list shrinks.
func clampViewStart(cursor, viewStart, maxItems, total int) int {
	if cursor < viewStart {
		viewStart = cursor
	}
	if cursor >= viewStart+maxItems {
		viewStart = cursor - maxItems + 1
	}
	if viewStart < 0 {
		viewStart = 0
	}
	if total <= maxItems {
		viewStart = 0
	} else {
		maxStart := total - maxItems
		if viewStart > maxStart {
			viewStart = maxStart
		}
	}
	return viewStart
}

// ansiEscapeLen returns the number of runes consumed by an ANSI CSI escape
// sequence starting at runes[i], or 0 if no escape sequence starts there.
func ansiEscapeLen(runes []rune, i int) int {
	if i >= len(runes) || runes[i] != '\x1b' {
		return 0
	}
	if i+1 >= len(runes) || runes[i+1] != '[' {
		return 0
	}
	j := i + 2
	for j < len(runes) {
		r := runes[j]
		j++
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			return j - i
		}
	}
	return 0
}

// visualToRuneIndex returns the rune index in s that corresponds to visual
// column col, skipping ANSI CSI escape sequences and using cell-width-aware
// counting so double-width runes (e.g. CJK, many emoji) are handled correctly.
func visualToRuneIndex(s string, col int) int {
	runes := []rune(s)
	visual := 0
	i := 0
	for i < len(runes) {
		if visual >= col {
			return i
		}
		if n := ansiEscapeLen(runes, i); n > 0 {
			i += n
			continue
		}
		visual += runewidth.RuneWidth(runes[i])
		i++
	}
	return i
}

// placeOverlay centers the overlay string on top of the background string.
func placeOverlay(width, height int, overlay, background string) string {
	overlayLines := strings.Split(overlay, "\n")
	bgLines := strings.Split(background, "\n")

	// Ensure background has enough lines
	for len(bgLines) < height {
		bgLines = append(bgLines, "")
	}

	overlayHeight := len(overlayLines)
	overlayWidth := 0
	for _, l := range overlayLines {
		if w := lipgloss.Width(l); w > overlayWidth {
			overlayWidth = w
		}
	}

	startY := (height - overlayHeight) / 2
	startX := (width - overlayWidth) / 2
	if startY < 0 {
		startY = 0
	}
	if startX < 0 {
		startX = 0
	}

	for i, overlayLine := range overlayLines {
		bgIdx := startY + i
		if bgIdx >= len(bgLines) {
			break
		}
		bgLine := bgLines[bgIdx]
		bgRunes := []rune(bgLine)
		olRunes := []rune(overlayLine)

		// Convert visual column startX to a rune index, skipping ANSI escape
		// sequences so we don't slice through them or misplace the overlay.
		bgCutStart := visualToRuneIndex(bgLine, startX)

		// Build the composed line
		var result []rune
		// Copy background up to the ANSI-aware cut point.
		result = append(result, bgRunes[:bgCutStart]...)
		// Pad with spaces if the background is visually shorter than startX.
		for lipgloss.Width(string(result)) < startX {
			result = append(result, ' ')
		}
		// Insert overlay.
		result = append(result, olRunes...)
		// Append remainder of background after the overlay region.
		// Use visualToRuneIndex so we skip over any ANSI sequences that fall
		// within the overlay's visual span rather than indexing by raw rune count.
		bgCutEnd := visualToRuneIndex(bgLine, startX+overlayWidth)
		if bgCutEnd < len(bgRunes) {
			result = append(result, bgRunes[bgCutEnd:]...)
		}
		bgLines[bgIdx] = string(result)
	}

	return strings.Join(bgLines[:height], "\n")
}

func truncate(s string, maxLen int) string {
	if maxLen < 4 {
		maxLen = 4
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-3]) + "..."
}

// activityLineType extracts the event type tag from an activity line.
// Lines like "[tool] Read ..." return "tool". Continuation lines (starting
// with spaces) return "" to indicate they belong to the previous entry.
func activityLineType(line string) string {
	if len(line) == 0 || line[0] != '[' {
		return ""
	}
	end := strings.IndexByte(line, ']')
	if end < 2 {
		return ""
	}
	return line[1:end]
}

// activityGroup represents a run of consecutive events sharing the same type.
type activityGroup struct {
	eventType string
	lines     []string
}

// groupActivityLines merges all same-type activity entries into persistent
// groups. The last group (by first-appearance order) is expanded; others
// are collapsed into a single summary line like "▸ [tool] x5 — Read, Edit".
func groupActivityLines(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}

	groups := buildActivityGroups(lines)

	// Single group or fewer — no collapsing needed.
	if len(groups) <= 1 {
		return lines
	}

	var result []string
	for i, g := range groups {
		if i == len(groups)-1 {
			// Last group: fully expanded.
			result = append(result, g.lines...)
		} else {
			result = append(result, collapseActivityGroup(g))
		}
	}
	return result
}

// collapseActivityGroup produces a single unstyled summary line for a group.
// For tool groups it extracts tool names: "▸ [tool] x5 — Read, Edit, Grep"
// For other types: "▸ [text] x3"
// The caller is responsible for applying any visual styling after truncation.
func collapseActivityGroup(g activityGroup) string {
	// Count primary entries (not continuation lines).
	count := 0
	var names []string
	for _, line := range g.lines {
		typ := activityLineType(line)
		if typ == "" {
			continue // continuation line
		}
		count++
		if g.eventType == "tool" {
			// Extract tool name: "[tool] ToolName ..."
			rest := line[len("[tool] "):]
			if sp := strings.IndexByte(rest, ' '); sp > 0 {
				rest = rest[:sp]
			}
			// Deduplicate consecutive names for readability.
			if len(names) == 0 || names[len(names)-1] != rest {
				names = append(names, rest)
			}
		}
	}
	if len(names) > 0 {
		summary := strings.Join(names, ", ")
		if len([]rune(summary)) > 40 {
			summary = string([]rune(summary)[:37]) + "..."
		}
		return fmt.Sprintf("▸ [%s] x%d — %s", g.eventType, count, summary)
	}
	return fmt.Sprintf("▸ [%s] x%d", g.eventType, count)
}

// expandActivityGroup produces the header line for an expanded group.
// Uses ▾ to indicate the group is open and can be collapsed.
func expandActivityGroup(g activityGroup) string {
	count := 0
	for _, line := range g.lines {
		if activityLineType(line) != "" {
			count++
		}
	}
	return fmt.Sprintf("▾ [%s] x%d", g.eventType, count)
}

// groupedWorkerActivity returns the grouped activity lines for the currently
// selected worker. The last group of consecutive same-type events is expanded
// while older groups are collapsed into summary headers.
func (m *Model) groupedWorkerActivity() []string {
	return groupActivityLines(m.selectedWorkerActivity())
}

// rebuildActivityNav rebuilds the flat display items for the Live Activity panel
// from the currently selected worker's activity. One persistent group per event
// type is shown, newest-first by last occurrence. Each group always has a header
// line (▸ collapsed / ▾ expanded); expanded groups show indented child lines.
func (m *Model) rebuildActivityNav() {
	rawLines := m.selectedWorkerActivity()
	groups := buildActivityGroups(rawLines)

	m.activityNavItems = nil
	// Iterate newest-first so the group with the most recent activity is on top.
	for ri := len(groups) - 1; ri >= 0; ri-- {
		g := groups[ri]
		expanded := m.isActivityGroupExpanded(g.eventType)
		if !expanded {
			m.activityNavItems = append(m.activityNavItems, activityNavItem{
				isGroupHeader: true,
				groupType:     g.eventType,
				text:          collapseActivityGroup(g),
			})
		} else {
			// Expanded: header with ▾, then indented child lines newest-first.
			m.activityNavItems = append(m.activityNavItems, activityNavItem{
				isGroupHeader: true,
				groupType:     g.eventType,
				text:          expandActivityGroup(g),
			})
			for li := len(g.lines) - 1; li >= 0; li-- {
				m.activityNavItems = append(m.activityNavItems, activityNavItem{
					groupType: g.eventType,
					text:      g.lines[li],
				})
			}
		}
	}
	m.activityVP.ClampToTotal(len(m.activityNavItems))
}

// isActivityGroupExpanded returns whether a group should be expanded.
// If the user has toggled the group, their choice is respected. Otherwise
// all groups default to collapsed.
func (m *Model) isActivityGroupExpanded(eventType string) bool {
	if expanded, set := m.activityExpanded[eventType]; set {
		return expanded
	}
	return false
}

// resetActivityState clears the activity viewport and expansion state,
// used when the selected worker changes.
func (m *Model) resetActivityState() {
	m.activityVP = scrollViewport{}
	m.activityExpanded = make(map[string]bool)
	m.rebuildActivityNav()
}

// buildActivityGroups merges all activity entries by event type into one
// persistent group per type. Groups are returned in the order their type
// first appeared, with a counter that grows as new events arrive.
// Lines without a [type] prefix are treated as continuation lines and
// appended to the most recent typed group; if no typed group exists yet
// they form a "text" group.
func buildActivityGroups(lines []string) []activityGroup {
	if len(lines) == 0 {
		return nil
	}
	idx := map[string]int{} // event type → index in groups
	var groups []activityGroup
	var lastType string
	for _, line := range lines {
		typ := activityLineType(line)
		if typ == "" {
			// Continuation line — append to last known type's group.
			if lastType == "" {
				// No typed group yet; create a "text" bucket.
				lastType = "text"
				idx[lastType] = len(groups)
				groups = append(groups, activityGroup{eventType: lastType})
			}
			groups[idx[lastType]].lines = append(groups[idx[lastType]].lines, line)
			continue
		}
		lastType = typ
		if i, ok := idx[typ]; ok {
			groups[i].lines = append(groups[i].lines, line)
		} else {
			idx[typ] = len(groups)
			groups = append(groups, activityGroup{eventType: typ, lines: []string{line}})
		}
	}
	return groups
}

// wordWrapCount returns the number of lines wordWrap would produce without
// allocating the line strings. Mirrors the logic in wordWrap exactly.
func wordWrapCount(s string, maxWidth int) int {
	if maxWidth < 1 {
		maxWidth = 1
	}

	count := 0
	paragraphs := strings.Split(s, "\n")
	for _, pStr := range paragraphs {
		if pStr == "" {
			if len(paragraphs) > 1 {
				count++
			}
			continue
		}

		p := []rune(pStr)
		for len(p) > maxWidth {
			breakAt := -1
			for i := maxWidth; i >= maxWidth/2; i-- {
				if i < len(p) && p[i] == ' ' {
					breakAt = i
					break
				}
			}
			if breakAt == -1 {
				breakAt = maxWidth
			}
			count++
			p = p[breakAt:]
			for len(p) > 0 && p[0] == ' ' {
				p = p[1:]
			}
		}
		if len(p) > 0 {
			count++
		}
	}

	if count == 0 {
		return 1 // wordWrap returns [""] for empty input
	}
	return count
}

// wordWrap splits s into lines of at most maxWidth runes (by rune count, not
// display width), preferring to break at spaces. Newlines in s are preserved
// as hard line breaks; leading indentation on each input line is kept intact.
func wordWrap(s string, maxWidth int) []string {
	if maxWidth < 1 {
		maxWidth = 1
	}

	var result []string
	paragraphs := strings.Split(s, "\n")
	for _, pStr := range paragraphs {
		if strings.TrimSpace(pStr) == "" {
			if len(paragraphs) > 1 {
				result = append(result, "")
			}
			continue
		}

		p := []rune(pStr)
		for len(p) > maxWidth {
			breakAt := -1
			// Scan backwards from maxWidth looking for a space to break at.
			// Include position maxWidth itself (the char just past the limit)
			// so that a space landing exactly there produces a clean line.
			end := maxWidth
			if end >= len(p) {
				end = len(p) - 1
			}
			for i := end; i >= maxWidth/2; i-- {
				if p[i] == ' ' {
					breakAt = i
					break
				}
			}
			if breakAt == -1 {
				breakAt = maxWidth
			}
			result = append(result, string(p[:breakAt]))
			p = p[breakAt:]
			// Consume the space we broke at.
			for len(p) > 0 && p[0] == ' ' {
				p = p[1:]
			}
		}
		if len(p) > 0 {
			result = append(result, string(p))
		}
	}

	if len(result) == 0 {
		return []string{""}
	}
	return result
}
