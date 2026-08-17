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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/Robin831/Forge/internal/ingot"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/termtext"
)

// Panel identifies a TUI panel.
type Panel int

const (
	PanelQueue Panel = iota
	PanelCrucibles
	PanelReadyToMerge
	PanelNeedsAttention
	PanelWorkers
	PanelWicket
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
	// PRNumber is the GitHub PR number for this bead, or 0 when no PR exists yet.
	PRNumber int
	// PRURL is the full https://github.com/<owner>/<repo>/pull/<n> link for the
	// bead's PR, or empty when no PR exists or the anvil's repo URL is unknown.
	PRURL string
}

// queueNavItem represents a navigable item in the grouped queue panel.
// It is either a collapsible anvil header or a reference to a bead.
type queueNavItem struct {
	isAnvil   bool
	anvilName string
	beadIdx   int // index into Model.queue; -1 for anvil headers
}

// activityNavItem represents a single display row in the Live Activity panel.
type activityNavItem struct {
	lineType string // "tool", "think", "text", or "" for blank separators
	text     string // raw display text (unstyled)
}

// AttentionReason categorizes why a bead needs human attention.
type AttentionReason int

const (
	AttentionUnknown            AttentionReason = iota
	AttentionDispatchExhausted                  // Circuit breaker tripped after repeated dispatch failures
	AttentionRecoveryExhausted                  // Recovery failed after repeated attempts
	AttentionCIFixExhausted                     // CI fix attempts exhausted
	AttentionReviewFixExhausted                 // Review fix attempts exhausted
	AttentionRebaseExhausted                    // Rebase attempts exhausted
	AttentionClarification                      // Bead flagged as needing clarification
	AttentionStalled                            // Worker stalled (no log activity)
	AttentionAnvilWedged                        // Anvil's beads database is mid-merge with unresolved conflicts
	AttentionSelfDeploy                         // Self-deploy deferred, failed, or rolled back — the binary is not the merged code
)

// NeedsAttentionItem represents a bead requiring human attention.
type NeedsAttentionItem struct {
	BeadID           string
	Title            string
	Description      string
	Anvil            string
	Reason           string
	ReasonCategory   AttentionReason
	FailureCount     int
	PRID             int // Non-zero when item originates from an exhausted PR
	PRNumber         int
	LastWardenReject string // Most recent warden_reject message, if any
	// Kind mirrors state.NeedsAttentionBead.Kind. state.AttentionKindAnvil marks
	// an anvil-level entry (no bead, no PR) whose only resolution is fixing the
	// anvil itself, and state.AttentionKindDeploy a self-deploy failure, so the
	// bead-scoped actions are suppressed for both.
	Kind string
}

// IsAnvil reports whether this entry describes an unusable anvil rather than a
// bead or PR.
func (i NeedsAttentionItem) IsAnvil() bool { return i.Kind == state.AttentionKindAnvil }

// IsDeploy reports whether this entry describes a self-deploy that left the
// daemon running something other than the merged code.
func (i NeedsAttentionItem) IsDeploy() bool { return i.Kind == state.AttentionKindDeploy }

// IsBeadScoped reports whether this entry identifies a bead (or its PR) and so
// supports the bead-scoped actions: retry, dismiss, notes, description. The
// non-bead kinds carry an empty bead ID, so every action path must branch on
// this rather than on a bead ID being present.
func (i NeedsAttentionItem) IsBeadScoped() bool { return i.Kind == state.AttentionKindBead }

// nonBeadAttentionHint explains why a non-bead entry offers no bead actions and
// what actually resolves it. Both kinds clear themselves, so the operator's job
// is to fix the underlying condition, not to dismiss a row.
func nonBeadAttentionHint(item NeedsAttentionItem) string {
	if item.IsDeploy() {
		return fmt.Sprintf("%s — fix the deploy and merge again; this clears on the next successful self-deploy", item.Title)
	}
	return fmt.Sprintf("Anvil %s is wedged — resolve the beads merge conflict; this clears automatically", item.Anvil)
}

// ReadyToMergeItem represents a PR ready to merge.
type ReadyToMergeItem struct {
	PRID      int
	PRNumber  int
	BeadID    string
	Anvil     string
	Branch    string
	Title     string
	AutoMerge bool // true when the anvil has auto_merge enabled
}

// PRItem represents an open PR in the PR panel overlay.
type PRItem struct {
	PRID                 int
	PRNumber             int
	Anvil                string
	BeadID               string
	Branch               string
	Status               string // "open", "approved", "needs_fix"
	Title                string
	CIPassing            bool
	IsConflicting        bool
	HasUnresolvedThreads bool
	HasPendingReviews    bool
	HasApproval          bool
	CIFixCount           int
	ReviewFixCount       int
	RebaseCount          int
	IsExternal           bool // true for PRs discovered via GitHub reconciliation
	BellowsManaged       bool // true when bellows runs lifecycle workers for this PR
}

// PRActionMenuChoice represents an action the user can take on an open PR.
type PRActionMenuChoice int

const (
	PRActionOpenBrowser      PRActionMenuChoice = iota // Open in GitHub
	PRActionMerge                                      // Merge the PR
	PRActionFixCI                                      // Trigger quench worker
	PRActionFixComments                                // Trigger burnish worker
	PRActionResolveConflicts                           // Trigger rebase worker
	PRActionClosePR                                    // Close the PR
	PRActionAssignBellows                              // Assign bellows to monitor & autofix
)

// WorkerItem represents a worker in the workers panel.
type WorkerItem struct {
	ID            string
	BeadID        string
	Title         string // Bead title for display
	Anvil         string
	Status        string
	Duration      string
	CostUSD       float64
	Type          string   // "smith", "warden", "temper", "quench", "burnish", "rebase"
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

// WicketRepoSummary holds per-repo issue counts for the Wicket panel.
type WicketRepoSummary struct {
	// Repo is the "owner/repo" string. DisplayName is derived as the part after "/".
	Repo            string
	OpenCount       int
	NeedsHumanCount int
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
	ActionWardenRerun
	ActionApproveAsIs
	ActionForceSmith

	actionMenuCount = ActionForceSmith + 1
)

// actionMenuLabels returns the display labels for the action menu.
func actionMenuLabels() [actionMenuCount]string {
	return [actionMenuCount]string{
		"Retry          — Clear flags, put back in queue",
		"Dismiss        — Remove from Needs Attention",
		"View Logs      — Show last worker log",
		"Re-run Warden  — Re-review with current rules",
		"Approve as-is  — Skip warden, create PR now",
		"Force Smith    — Push smith into another iteration",
	}
}

// QueueActionMenuChoice represents an action the user can take on an unlabeled queue bead.
type QueueActionMenuChoice int

const (
	QueueActionLabel    QueueActionMenuChoice = iota
	QueueActionForceRun                       // Run independently — bypass bd ready, skip crucible
	QueueActionClose
	QueueActionStop

	queueActionMenuCount = QueueActionStop + 1
)

// queueActionMenuLabels returns the display labels for the queue action menu.
func queueActionMenuLabels() [queueActionMenuCount]string {
	return [queueActionMenuCount]string{
		"Label for dispatch — Tag bead for auto-dispatch",
		"Run independently  — Bypass bd ready, skip crucible",
		"Close             — Close this bead",
		"Stop              — Prevent all processing",
	}
}

// CrucibleActionMenuChoice represents an action the user can take on a paused Crucible.
type CrucibleActionMenuChoice int

const (
	CrucibleActionResume CrucibleActionMenuChoice = iota // Resume — retry parent to re-enter crucible loop
	CrucibleActionStop                                   // Stop — close the parent bead

	crucibleActionMenuCount = CrucibleActionStop + 1
)

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

// OrphanDialogChoice represents an action for an orphaned bead.
type OrphanDialogChoice int

const (
	OrphanActionRecover OrphanDialogChoice = iota
	OrphanActionClose
	OrphanActionDiscard

	orphanDialogChoiceCount = OrphanActionDiscard + 1
)

func orphanDialogLabels() [orphanDialogChoiceCount]string {
	return [orphanDialogChoiceCount]string{
		"Recover — reopen and re-queue for work",
		"Close   — mark done (work already completed)",
		"Discard — close without retry",
	}
}

// OrphanResolveResultMsg is delivered asynchronously when an orphan resolve action completes.
type OrphanResolveResultMsg struct {
	BeadID string
	Action string
	Err    error
}

// pendingOp describes an in-flight async IPC operation awaiting completion.
type pendingOp struct {
	Description string    // e.g. "Tagging Forge-abc1"
	StartedAt   time.Time // for timeout detection
}

// AsyncCompletionMsg is delivered when a daemon async_complete event is received.
type AsyncCompletionMsg struct {
	RequestID string
	Success   bool
	Message   string
}

// LoggedEventMsg is delivered when the daemon pushes a real logged event over
// the IPC subscribe stream (event bus → Broadcast). The event feed prepends it
// live instead of waiting for the next poll tick.
type LoggedEventMsg struct {
	Event ipc.EventInfo
}

// EventsGapMsg is delivered when the daemon's event bus overflowed and dropped
// events for this subscriber. The TUI re-syncs its event feed from the DB once
// to recover the missed rows, then resumes streaming.
type EventsGapMsg struct{}

// streamClosedMsg is delivered when the IPC subscribe channel closes (daemon
// restart, dropped connection, or a failed initial connect). It drops the
// streaming latch so the tick's poll fallback resumes — the feed never freezes —
// and schedules a reconnect attempt.
type streamClosedMsg struct{}

// reconnectSubscriptionMsg fires after a short delay following a dropped
// subscription, prompting a fresh connect attempt.
type reconnectSubscriptionMsg struct{}

// asyncSubscriptionStartedMsg signals that the event subscription is ready.
type asyncSubscriptionStartedMsg struct {
	events <-chan ipc.Event
	cancel context.CancelFunc
	client *ipc.Client
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
	ingotCounts    map[ingot.Status]int
	ingotTotal     int
	wicketSummary  []WicketRepoSummary

	// Data source for polling
	data *DataSource

	// Callback for killing a worker (set by the caller)
	OnKill func(workerID string, pid int)

	// Callback for stopping a bead entirely: kills worker, sets clarification,
	// releases to open (set by the caller).
	// Returns (requestID, error) — requestID is non-empty when the daemon queued
	// the operation for async processing.
	OnStopBead func(beadID, anvil string) (string, error)

	// Callbacks for Needs Attention actions (set by the caller)
	OnRetryBead   func(beadID, anvil string, prID int) (string, error)
	OnDismissBead func(beadID, anvil string, prID int) error
	OnViewLogs    func(beadID string) (logPath string, lines []string)
	OnWardenRerun func(beadID, anvil string) error
	OnApproveAsIs func(beadID, anvil string) error
	OnForceSmith  func(beadID, anvil, userNote string) error

	// Callback for tagging a bead (set by the caller).
	// Called with (beadID, anvil) when user presses 'l' on an unlabeled bead.
	OnTagBead func(beadID, anvil string) (string, error)

	// Callback for closing a bead (set by the caller).
	OnCloseBead func(beadID, anvil string) (string, error)

	// Callback for force-running a bead independently (set by the caller).
	// Dispatches via bd show, bypassing bd ready and crucible/parent checks.
	OnForceRunBead func(beadID, anvil string) error

	// Callback for merging a PR (set by the caller).
	OnMergePR func(prID, prNumber int, anvil string) error

	// Callback for PR panel actions (set by the caller).
	// Called with (prID, prNumber, anvil, beadID, branch, action).
	OnPRAction func(prID, prNumber int, anvil, beadID, branch, action string) (string, error)

	// Callback for triggering PR reconciliation with GitHub (set by the caller).
	OnReconcilePRs func() error

	// Callback for triggering an immediate Wicket issue scan (set by the caller).
	// Only available when wicket is enabled.
	OnWicketScan func() error

	// Callback for resolving an orphaned bead (set by the caller).
	// Called with (beadID, anvil, action) where action is "recover", "close", or "discard".
	OnResolveOrphan func(beadID, anvil, action string) error

	// Callback for crucible actions (resume/stop) on a paused crucible (set by caller).
	// Called with (parentID, anvil, action) where action is "resume" or "stop".
	OnCrucibleAction func(parentID, anvil, action string) error

	// Callback for appending notes to a bead via bd update --append-notes (set by caller).
	// Called with (beadID, anvil, notes).
	OnAppendNotes func(beadID, anvil, notes string) (string, error)

	// State
	focused           Panel
	queueVP           scrollViewport
	crucibleVP        scrollViewport
	needsAttnVP       scrollViewport
	readyToMergeVP    scrollViewport
	workerTable       table.Model
	activityVP        scrollViewport
	activityNavItems  []activityNavItem // flat display items for Live Activity
	activityLineCount int               // total rendered lines (line-based scrolling)
	eventScroll       int
	eventAutoScroll   bool // true = follow new events
	prevEventCount    int  // track event count for auto-scroll
	width             int
	height            int
	ready             bool
	refreshing        bool   // true while a FetchAll is in flight; prevents concurrent refreshes
	focusedBeadID     string // BeadID of the queue item under the cursor; restored after refresh

	// Action menu overlay state (Needs Attention)
	actionForm   *huh.Form
	actionChoice ActionMenuChoice
	actionTarget *NeedsAttentionItem // bead the menu is open for

	// Queue action menu overlay state (Unlabeled beads)
	queueActionForm   *huh.Form
	queueActionChoice QueueActionMenuChoice
	queueActionTarget *QueueItem // bead the queue menu is open for

	// Merge menu overlay state
	mergeForm   *huh.Form
	mergeChoice MergeMenuChoice
	mergeTarget *ReadyToMergeItem

	// Crucible action menu overlay state (paused Crucibles)
	crucibleActionForm   *huh.Form
	crucibleActionChoice CrucibleActionMenuChoice
	crucibleActionTarget *CrucibleItem // crucible the menu is open for

	// PR panel overlay state — full-screen PR management panel toggled with 'p'.
	showPRPanel    bool
	prItems        []PRItem
	prVP           scrollViewport
	prActionForm   *huh.Form
	prActionChoice PRActionMenuChoice
	prActionTarget *PRItem

	// Force Smith note form overlay state — collects an optional user note
	// before dispatching a force_smith action.
	forceSmithNoteForm   *huh.Form
	forceSmithNote       string
	forceSmithNoteTarget *NeedsAttentionItem

	// Orphan dialog overlay state — shown when orphaned beads need user decision.
	orphanQueue        []PendingOrphanItem // beads awaiting user decision
	orphanDialogForm   *huh.Form
	orphanDialogChoice OrphanDialogChoice
	orphanTarget       *PendingOrphanItem // bead currently shown in dialog

	// Log viewer overlay state
	showLogViewer  bool
	logViewerTitle string
	logViewerEmpty bool // true when the log has no lines; use viewport as content source of truth
	logViewerVP    viewport.Model

	// Description viewer overlay state — shows glamour-rendered markdown description.
	showDescriptionViewer  bool
	descriptionViewerTitle string
	descriptionViewerRaw   string
	descriptionViewerEmpty bool
	descriptionViewerVP    viewport.Model

	// Daemon health indicator
	daemonConnected bool   // true when last IPC status check succeeded
	daemonLastPoll  string // e.g. "30s ago" or "n/a"
	daemonWorkers   int    // active worker count from daemon
	daemonQueueSize int    // queue size from daemon
	daemonUptime    string // daemon uptime string
	// daemonPause is the rendered dispatch-pause line from the last health
	// check (empty when dispatch is running). It names the cause, so a pause
	// taken by a self-deploy drain is not shown as an operator action.
	daemonPause     string
	healthTickCount int // counts ticks; health IPC fires every healthTickDivisor ticks

	// Status message (flashes briefly after an action)
	statusMsg        string
	statusMsgTime    time.Time
	statusMsgIsError bool

	// Async IPC tracking — maps request IDs to pending operation descriptions
	// so completion events can update the status line.
	pendingAsync    map[string]pendingOp
	asyncEvents     <-chan ipc.Event
	asyncCancelFunc context.CancelFunc
	asyncClient     *ipc.Client

	// Cooldown for manual Wicket scans — prevents rapid repeated triggers.
	wicketScanCooldownUntil time.Time

	// Queue anvil grouping state — groups beads by anvil when 2+ anvils present.
	queueExpandedAnvils map[string]bool // per-anvil expanded/collapsed state
	queueNavItems       []queueNavItem  // navigable items (anvil headers + beads)
	queueGrouped        bool            // true when 2+ anvils trigger grouping

	// Per-anvil poll health status, keyed by anvil name.
	anvilHealth map[string]AnvilHealth

	// Spinner animation frame index (advances every SpinnerInterval).
	spinnerFrame int

	// Event rendering cache
	eventLinesCache       []string
	eventWidthCache       int
	eventSelectedIdxCache int
	eventCountCache       int
	eventRevision         int // incremented on every UpdateEventsMsg to detect content changes
	eventRevisionCache    int
	// eventStreamActive flips true once the first real event arrives over the IPC
	// subscribe stream, proving the daemon's event bus is forwarding. From then
	// on the periodic tick stops polling the events table (status/queue polls
	// remain) and the feed rides pushed events. It stays false against a daemon
	// with the bus disabled, preserving the legacy poll-every-tick behaviour.
	eventStreamActive bool

	// Event filter — text search toggled with '/'
	eventFilter       textinput.Model
	eventFilterActive bool   // true when the filter input is focused
	eventFilterText   string // last applied filter text (for cache invalidation)
	filteredEvents    []EventItem

	// Toast notifications
	toasts           []toast // active toasts, newest first
	nextToastID      int     // monotonically increasing ID for dismissal matching
	lastSeenEventKey string  // fingerprint of most-recent event from last poll cycle

	// Help component — renders context-sensitive keybinding hints in the footer.
	helpModel help.Model

	// Notes overlay state — inline textarea for appending notes to a bead.
	showNotesOverlay bool
	notesTA          textarea.Model
	notesTarget      *notesTarget

	// Mouse mode — when true, click-to-focus is active but terminal text selection
	// is disabled. Toggle with 'm'. Initial value set by the caller via SetMouseEnabled.
	mouseEnabled bool

	// Incremental log file reader — avoids re-reading entire log files on each tick.
	logCache *LogTailerCache
}

// NewModel creates a new Hearth TUI model.
// Pass nil for DataSource to run in display-only mode (no polling).
func NewModel(ds *DataSource) Model {
	h := help.New()
	h.ShowAll = false // use short (single-line) mode by default

	columns := []table.Column{
		{Title: " ", Width: 1},
		{Title: " Type", Width: 8},
		{Title: " Bead", Width: 12},
		{Title: " Task", Width: 20},
		{Title: " Anvil", Width: 10},
		{Title: " Time", Width: 6},
	}
	t := table.New(
		table.WithColumns(columns),
		table.WithFocused(true),
	)

	s := table.DefaultStyles()
	// Use fresh styles without Padding(0,1) for Header, Cell, and Selected.
	// The default Cell/Header padding adds 2 chars per column outside of Width,
	// which causes rows to overflow the panel border. All three styles must
	// agree on padding so every row renders at the same width.
	s.Header = lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(colorMuted).
		BorderBottom(true).
		Bold(false)
	s.Cell = lipgloss.NewStyle()
	s.Selected = lipgloss.NewStyle().
		Foreground(colorFg).
		Background(lipgloss.AdaptiveColor{Dark: "236", Light: "254"}).
		Bold(true)
	t.SetStyles(s)

	ti := textinput.New()
	ti.Placeholder = "Filter events..."
	ti.CharLimit = 100

	return Model{
		focused:             PanelQueue,
		data:                ds,
		eventAutoScroll:     true,
		queueExpandedAnvils: make(map[string]bool),
		helpModel:           h,
		workerTable:         t,
		eventFilter:         ti,
		logCache:            NewLogTailerCache(),
	}
}

// SetMouseEnabled records whether mouse reporting was initially enabled so the
// toggle keybind ('m') can flip the state at runtime.
func (m *Model) SetMouseEnabled(enabled bool) {
	m.mouseEnabled = enabled
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
		cmds = append(cmds, FetchAll(m.data, m.logCache))
		// Prime the event feed once. Live updates then arrive over the subscribe
		// stream; the tick only re-polls events while streaming stays inactive.
		cmds = append(cmds, FetchEvents(m.data.DB, EventFetchLimit))
		cmds = append(cmds, FetchDaemonHealth())
		cmds = append(cmds, startAsyncSubscription())
	}

	return tea.Batch(cmds...)
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Action menu overlays intercept all keys/mouse when open.
	if m.orphanDialogForm != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			if k.Type == tea.KeyCtrlC || k.String() == "ctrl+c" {
				m.cleanupAsyncSubscription()
				return m, tea.Quit
			}
			if k.Type == tea.KeyEsc {
				m.orphanDialogForm = nil
				m.orphanTarget = nil
				return m, m.dequeueNextOrphan()
			}
		}
		cmd := m.driveHuhForm(&m.orphanDialogForm, msg)
		if m.orphanDialogForm.State == huh.StateCompleted {
			actionCmd := m.executeOrphanAction(m.orphanDialogChoice)
			if cmd == nil {
				return m, actionCmd
			}
			return m, tea.Batch(cmd, actionCmd)
		} else if m.orphanDialogForm.State == huh.StateAborted {
			m.orphanDialogForm = nil
			m.orphanTarget = nil
			return m, tea.Batch(cmd, m.dequeueNextOrphan())
		}
		if isTerminalMsg(msg) {
			return m, cmd
		}
	}

	if m.mergeForm != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			if k.Type == tea.KeyEsc {
				m.mergeForm = nil
				return m, nil
			}
		}
		cmd := m.driveHuhForm(&m.mergeForm, msg)
		if m.mergeForm.State == huh.StateCompleted {
			actionCmd := m.executeMergeAction(m.mergeChoice)
			m.mergeForm = nil
			if cmd == nil {
				return m, actionCmd
			}
			return m, tea.Batch(cmd, actionCmd)
		} else if m.mergeForm.State == huh.StateAborted {
			m.mergeForm = nil
			return m, cmd
		}
		if isTerminalMsg(msg) {
			return m, cmd
		}
	}

	if m.queueActionForm != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			if k.Type == tea.KeyEsc {
				m.queueActionForm = nil
				return m, nil
			}
		}
		cmd := m.driveHuhForm(&m.queueActionForm, msg)
		if m.queueActionForm.State == huh.StateCompleted {
			actionCmd := m.executeQueueAction(m.queueActionChoice)
			m.queueActionForm = nil
			if cmd == nil {
				return m, actionCmd
			}
			return m, tea.Batch(cmd, actionCmd)
		} else if m.queueActionForm.State == huh.StateAborted {
			m.queueActionForm = nil
			return m, cmd
		}
		if isTerminalMsg(msg) {
			return m, cmd
		}
	}

	if m.crucibleActionForm != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			if k.Type == tea.KeyEsc {
				m.crucibleActionForm = nil
				return m, nil
			}
		}
		cmd := m.driveHuhForm(&m.crucibleActionForm, msg)
		if m.crucibleActionForm.State == huh.StateCompleted {
			actionCmd := m.executeCrucibleAction(m.crucibleActionChoice)
			m.crucibleActionForm = nil
			if cmd == nil {
				return m, actionCmd
			}
			return m, tea.Batch(cmd, actionCmd)
		} else if m.crucibleActionForm.State == huh.StateAborted {
			m.crucibleActionForm = nil
			return m, cmd
		}
		if isTerminalMsg(msg) {
			return m, cmd
		}
	}

	if m.forceSmithNoteForm != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			if k.Type == tea.KeyEsc {
				m.forceSmithNoteForm = nil
				m.forceSmithNoteTarget = nil
				m.forceSmithNote = ""
				return m, nil
			}
		}
		cmd := m.driveHuhForm(&m.forceSmithNoteForm, msg)
		if m.forceSmithNoteForm.State == huh.StateCompleted {
			target := m.forceSmithNoteTarget
			note := m.forceSmithNote
			m.forceSmithNoteForm = nil
			m.forceSmithNoteTarget = nil
			m.forceSmithNote = ""
			if target != nil && m.OnForceSmith != nil {
				if err := m.OnForceSmith(target.BeadID, target.Anvil, note); err != nil {
					m.setStatus(fmt.Sprintf("Failed to force smith for %s: %v", target.BeadID, err), true)
				} else {
					m.setStatus(fmt.Sprintf("Force smith started for %s", target.BeadID), false)
					m.removeNeedsAttentionItem(target.BeadID, target.Anvil, target.PRID)
					if m.data != nil {
						return m, tea.Batch(cmd, FetchNeedsAttention(m.data))
					}
				}
			}
			return m, cmd
		} else if m.forceSmithNoteForm.State == huh.StateAborted {
			m.forceSmithNoteForm = nil
			m.forceSmithNoteTarget = nil
			m.forceSmithNote = ""
			return m, cmd
		}
		if isTerminalMsg(msg) {
			return m, cmd
		}
	}

	if m.actionForm != nil {
		if k, ok := msg.(tea.KeyMsg); ok {
			if k.Type == tea.KeyEsc {
				m.actionForm = nil
				return m, nil
			}
		}
		cmd := m.driveHuhForm(&m.actionForm, msg)
		if m.actionForm.State == huh.StateCompleted {
			actionCmd := m.executeAction(m.actionChoice)
			m.actionForm = nil
			if cmd == nil {
				return m, actionCmd
			}
			return m, tea.Batch(cmd, actionCmd)
		} else if m.actionForm.State == huh.StateAborted {
			m.actionForm = nil
			return m, cmd
		}
		if isTerminalMsg(msg) {
			return m, cmd
		}
	}

	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Log viewer overlay intercepts all keys
		if m.showLogViewer {
			switch msg.String() {
			case "ctrl+c":
				m.cleanupAsyncSubscription()
				return m, tea.Quit
			case "q", "esc":
				m.showLogViewer = false
			default:
				var cmd tea.Cmd
				m.logViewerVP, cmd = m.logViewerVP.Update(msg)
				return m, cmd
			}
			return m, nil
		}

		// Description viewer overlay intercepts all keys
		if m.showDescriptionViewer {
			switch msg.String() {
			case "ctrl+c":
				m.cleanupAsyncSubscription()
				return m, tea.Quit
			case "q", "esc":
				m.showDescriptionViewer = false
			default:
				var cmd tea.Cmd
				m.descriptionViewerVP, cmd = m.descriptionViewerVP.Update(msg)
				return m, cmd
			}
			return m, nil
		}

		// Notes overlay intercepts all keys when open
		if m.showNotesOverlay {
			switch msg.String() {
			case "ctrl+c":
				m.cleanupAsyncSubscription()
				return m, tea.Quit
			case "esc":
				m.showNotesOverlay = false
				m.notesTarget = nil
				m.notesTA = textarea.Model{}
			case "ctrl+d":
				cmd := m.submitNotes()
				return m, cmd
			default:
				var cmd tea.Cmd
				m.notesTA, cmd = m.notesTA.Update(msg)
				return m, cmd
			}
			return m, nil
		}

		// PR panel overlay intercepts all keys when open
		if m.showPRPanel {
			// PR action form intercepts keys when active
			if m.prActionForm != nil {
				if k := msg.String(); k == "esc" {
					m.prActionForm = nil
					return m, nil
				}
				cmd := m.driveHuhForm(&m.prActionForm, msg)
				if m.prActionForm.State == huh.StateCompleted {
					actionCmd := m.executePRAction(m.prActionChoice)
					m.prActionForm = nil
					if cmd == nil {
						return m, actionCmd
					}
					return m, tea.Batch(cmd, actionCmd)
				} else if m.prActionForm.State == huh.StateAborted {
					m.prActionForm = nil
					return m, cmd
				}
				if isTerminalMsg(msg) {
					return m, cmd
				}
			}

			switch msg.String() {
			case "ctrl+c":
				m.cleanupAsyncSubscription()
				return m, tea.Quit
			case "p", "q", "esc":
				m.showPRPanel = false
			case "j", "down":
				if len(m.prItems) > 0 {
					m.prVP.cursor++
					if m.prVP.cursor >= len(m.prItems) {
						m.prVP.cursor = len(m.prItems) - 1
					}
				}
			case "k", "up":
				if m.prVP.cursor > 0 {
					m.prVP.cursor--
				}
			case "enter":
				if len(m.prItems) > 0 && m.prVP.cursor < len(m.prItems) {
					m.prActionTarget = new(PRItem)
					*m.prActionTarget = m.prItems[m.prVP.cursor]
					m.prActionChoice = PRActionOpenBrowser
					m.prActionForm = m.buildPRActionForm(m.prActionTarget, &m.prActionChoice)
					return m, m.prActionForm.Init()
				}
			}
			return m, nil
		}

		// Event filter input interception — when the filter textinput is
		// focused, route keys to it instead of the normal key handling.
		if m.eventFilterActive {
			switch msg.String() {
			case "ctrl+c":
				m.cleanupAsyncSubscription()
				return m, tea.Quit
			case "esc":
				m.eventFilterActive = false
				m.eventFilter.Blur()
				m.eventFilter.SetValue("")
				m.eventFilterText = ""
				filterCmd := m.applyEventFilter()
				m.eventScroll = 0
				m.eventRevision++
				return m, filterCmd
			case "enter":
				m.eventFilterActive = false
				m.eventFilter.Blur()
				m.eventFilterText = m.eventFilter.Value()
				filterCmd := m.applyEventFilter()
				m.eventScroll = 0
				m.eventRevision++
				return m, filterCmd
			default:
				var cmd tea.Cmd
				m.eventFilter, cmd = m.eventFilter.Update(msg)
				// Live-filter as user types
				newText := m.eventFilter.Value()
				if newText != m.eventFilterText {
					m.eventFilterText = newText
					filterCmd := m.applyEventFilter()
					m.eventScroll = 0
					m.eventRevision++
					return m, tea.Batch(cmd, filterCmd)
				}
				return m, cmd
			}
		}

		switch msg.String() {
		case "q", "ctrl+c":
			m.cleanupAsyncSubscription()
			return m, tea.Quit

		case "tab":
			m.focused = (m.focused + 1) % panelCount
			if m.focused == PanelCrucibles && len(m.crucibles) == 0 {
				m.focused = (m.focused + 1) % panelCount
			}
			if m.focused == PanelWicket && !m.wicketVisible() {
				m.focused = (m.focused + 1) % panelCount
			}

		case "shift+tab":
			m.focused = (m.focused + panelCount - 1) % panelCount
			if m.focused == PanelCrucibles && len(m.crucibles) == 0 {
				m.focused = (m.focused + panelCount - 1) % panelCount
			}
			if m.focused == PanelWicket && !m.wicketVisible() {
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
				// Reset activity scroll to bottom (follow latest — newest items last)
				if m.activityLineCount > 0 {
					m.activityVP.cursor = m.activityLineCount - 1
				}
			}

		case "/":
			// Activate event filter when events panel is focused
			if m.focused == PanelEvents {
				m.eventFilterActive = true
				cmd := m.eventFilter.Focus()
				return m, cmd
			}

		case "K":
			// Kill selected worker
			if m.focused == PanelWorkers && len(m.workers) > 0 &&
				m.workerTable.Cursor() < len(m.workers) {
				w := m.workers[m.workerTable.Cursor()]
				if m.OnKill != nil {
					m.OnKill(w.ID, w.PID)
				}
			}

		case "S":
			// Stop bead: kill worker, prevent re-dispatch, release to open
			if m.focused == PanelWorkers && len(m.workers) > 0 &&
				m.workerTable.Cursor() < len(m.workers) {
				w := m.workers[m.workerTable.Cursor()]
				if m.OnStopBead != nil {
					reqID, err := m.OnStopBead(w.BeadID, w.Anvil)
					if err != nil {
						m.setStatus(fmt.Sprintf("Stop failed: %v", err), true)
					} else if reqID != "" {
						m.setStatus(fmt.Sprintf("Stopping %s…", w.BeadID), false)
						m.trackPending(reqID, fmt.Sprintf("Stopping %s", w.BeadID))
					} else {
						m.setStatus(fmt.Sprintf("Stopped bead %s", w.BeadID), false)
					}
				}
			}

		case "o":
			// Open log viewer for the selected worker
			if m.focused == PanelWorkers && len(m.workers) > 0 &&
				m.workerTable.Cursor() < len(m.workers) {
				w := m.workers[m.workerTable.Cursor()]
				if w.LogPath == "" {
					m.setStatus(fmt.Sprintf("No log file for %s", w.BeadID), false)
				} else {
					data, err := os.ReadFile(w.LogPath)
					var content string
					if err != nil {
						content = fmt.Sprintf("(error reading log: %v)", err)
					} else {
						content = strings.TrimRight(string(data), "\n")
					}
					m.openLogViewer(fmt.Sprintf("Log: %s — %s", w.BeadID, w.LogPath), content)
				}
			}

		case "enter":
			// Open action menu for selected Needs Attention bead
			if m.focused == PanelNeedsAttention && len(m.needsAttention) > 0 &&
				m.needsAttnVP.cursor < len(m.needsAttention) {
				// Non-bead entries (a wedged beads database, a failed
				// self-deploy) have no bead to retry, dismiss or re-review: the
				// only fix is resolving the underlying condition, after which
				// Forge clears the entry on its own. Explain that instead of
				// offering dead actions.
				if sel := m.needsAttention[m.needsAttnVP.cursor]; !sel.IsBeadScoped() {
					m.setStatus(nonBeadAttentionHint(sel), false)
					return m, nil
				}
				m.actionTarget = new(NeedsAttentionItem)
				*m.actionTarget = m.needsAttention[m.needsAttnVP.cursor]
				item := m.actionTarget
				// Reset choice to default so reopening doesn't reuse a stale selection.
				m.actionChoice = ActionRetry
				// Use the item title as the menu label when there is no bead ID
				// (e.g. warden-learn PRs and other non-bead exhausted PRs).
				menuLabel := item.BeadID
				if menuLabel == "" {
					// Sanitize and truncate the title before rendering into the TUI header
					safeTitle := sanitizeTitle(item.Title)
					menuLabel = runewidth.Truncate(safeTitle, 80, "…")
				}
				// Non-bead PRs (PRID set, no BeadID) only support retry/dismiss —
				// warden rerun, approve-as-is, and force smith all require a worktree.
				actionOptions := []huh.Option[ActionMenuChoice]{
					huh.NewOption("Retry          — Clear flags, put back in queue", ActionRetry),
					huh.NewOption("Dismiss        — Remove from Needs Attention", ActionDismiss),
				}
				if item.BeadID != "" {
					actionOptions = append(actionOptions,
						huh.NewOption("View Logs      — Show last worker log", ActionViewLogs),
						huh.NewOption("Re-run Warden  — Re-review with current rules", ActionWardenRerun),
						huh.NewOption("Approve as-is  — Skip warden, create PR now", ActionApproveAsIs),
						huh.NewOption("Force Smith    — Push smith into another iteration", ActionForceSmith),
					)
				}
				m.actionForm = huh.NewForm(
					huh.NewGroup(
						huh.NewSelect[ActionMenuChoice]().
							Title(fmt.Sprintf("Actions for %s", menuLabel)).
							Description(sanitizeTitle(item.Title)).
							Options(actionOptions...).
							Value(&m.actionChoice),
					),
				).WithTheme(huh.ThemeCharm()).WithWidth(60)
				return m, m.actionForm.Init()
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
						if m.queue[nav.beadIdx].Section == "unlabeled" {
							m.queueActionTarget = new(QueueItem)
							*m.queueActionTarget = m.queue[nav.beadIdx]
							item := m.queueActionTarget
							// Reset choice to default so reopening doesn't reuse a stale selection.
							m.queueActionChoice = QueueActionLabel
							m.queueActionForm = buildQueueActionForm(item, &m.queueActionChoice)
							return m, m.queueActionForm.Init()
						}
					}
				}
			}
			// Open action menu for selected Crucible (only when paused)
			if m.focused == PanelCrucibles && len(m.crucibles) > 0 &&
				m.crucibleVP.cursor < len(m.crucibles) {
				item := m.crucibles[m.crucibleVP.cursor]
				if item.Phase == "paused" {
					m.crucibleActionTarget = new(CrucibleItem)
					*m.crucibleActionTarget = item
					m.crucibleActionChoice = CrucibleActionResume
					m.crucibleActionForm = buildCrucibleActionForm(m.crucibleActionTarget, &m.crucibleActionChoice)
					return m, m.crucibleActionForm.Init()
				}
			}
			// Open merge menu for selected Ready to Merge PR
			if m.focused == PanelReadyToMerge && len(m.readyToMerge) > 0 &&
				m.readyToMergeVP.cursor < len(m.readyToMerge) {
				m.mergeTarget = new(ReadyToMergeItem)
				*m.mergeTarget = m.readyToMerge[m.readyToMergeVP.cursor]
				item := m.mergeTarget
				// Reset choice to default so reopening doesn't reuse a stale selection.
				m.mergeChoice = MergeActionMerge
				m.mergeForm = buildMergeForm(item, &m.mergeChoice)
				return m, m.mergeForm.Init()
			}

		case "l":
			// Label (tag) selected bead in the queue for auto-dispatch
			if m.focused == PanelQueue {
				if bead := m.selectedQueueBead(); bead != nil && bead.Section == "unlabeled" {
					m.queueActionTarget = new(QueueItem)
					*m.queueActionTarget = *bead
					cmd := m.executeQueueAction(QueueActionLabel)
					return m, cmd
				}
			}

		case "d":
			// Show glamour-rendered description for the selected bead in Queue or Needs Attention.
			if m.focused == PanelQueue {
				if bead := m.selectedQueueBead(); bead != nil {
					m.openDescriptionViewer(bead.BeadID, bead.Title, bead.Description)
				}
			} else if m.focused == PanelNeedsAttention && len(m.needsAttention) > 0 &&
				m.needsAttnVP.cursor < len(m.needsAttention) {
				item := m.needsAttention[m.needsAttnVP.cursor]
				if !item.IsBeadScoped() {
					// No bead description exists; show the full detail (conflict
					// tables, or the deploy's attempted/restored builds), which is
					// what the operator needs and is truncated in the row.
					m.openDescriptionViewer(item.Anvil, item.Title, item.Reason)
				} else {
					m.openDescriptionViewer(item.BeadID, item.Title, item.Description)
				}
			}

		case "m":
			// Toggle mouse reporting on/off.
			// When mouse is off, the terminal handles clicks natively — text is selectable.
			// When mouse is on, click-to-focus and wheel scrolling work in Hearth.
			m.mouseEnabled = !m.mouseEnabled
			if m.mouseEnabled {
				return m, tea.EnableMouseCellMotion
			}
			return m, tea.DisableMouse

		case "p":
			// Toggle PR panel overlay
			m.showPRPanel = !m.showPRPanel
			m.prVP.cursor = 0
			m.prVP.viewStart = 0
			if m.showPRPanel && m.OnReconcilePRs != nil {
				// Trigger GitHub PR reconciliation and refresh the panel
				return m, func() tea.Msg {
					_ = m.OnReconcilePRs()
					return reconcilePRsDoneMsg{}
				}
			}
			return m, nil

		case "w":
			// Trigger an immediate Wicket scan (with cooldown to prevent spam).
			if m.OnWicketScan == nil {
				return m, nil
			}
			const wicketScanCooldown = 30 * time.Second
			if time.Now().Before(m.wicketScanCooldownUntil) {
				remaining := time.Until(m.wicketScanCooldownUntil).Round(time.Second)
				m.setStatus("Wicket scan on cooldown — try again in "+remaining.String(), false)
				return m, nil
			}
			m.wicketScanCooldownUntil = time.Now().Add(wicketScanCooldown)
			m.setStatus("Wicket scan triggered", false)
			cb := m.OnWicketScan
			return m, func() tea.Msg {
				return wicketScanResultMsg{Err: cb()}
			}

		case "esc":
			// Clear event filter when Events panel is focused and filter is applied
			if m.focused == PanelEvents && m.eventFilterText != "" {
				m.eventFilter.SetValue("")
				m.eventFilterText = ""
				m.applyEventFilter()
				m.eventScroll = 0
				m.eventRevision++
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

		case "n":
			// Open inline notes overlay for selected Queue or NeedsAttention bead.
			if m.focused == PanelQueue {
				if bead := m.selectedQueueBead(); bead != nil {
					m.openNotesOverlay(bead.BeadID, bead.Anvil, bead.Title)
				}
			} else if m.focused == PanelNeedsAttention && len(m.needsAttention) > 0 &&
				m.needsAttnVP.cursor < len(m.needsAttention) {
				item := m.needsAttention[m.needsAttnVP.cursor]
				// Anvil- and deploy-level entries have no bead to annotate.
				if !item.IsBeadScoped() {
					m.setStatus(item.Title+" — no bead to annotate", false)
					return m, nil
				}
				m.openNotesOverlay(item.BeadID, item.Anvil, item.Title)
			}

		default:
			if m.focused == PanelWorkers {
				var cmd tea.Cmd
				m.workerTable, cmd = m.workerTable.Update(msg)
				return m, cmd
			}
		}

	case tea.MouseMsg:
		// Dismiss log viewer on left or right click; forward wheel events to viewport
		if m.showLogViewer {
			if msg.Action == tea.MouseActionPress &&
				(msg.Button == tea.MouseButtonLeft || msg.Button == tea.MouseButtonRight) {
				m.showLogViewer = false
				return m, nil
			}
			var cmd tea.Cmd
			m.logViewerVP, cmd = m.logViewerVP.Update(msg)
			return m, cmd
		}
		// Dismiss description viewer on left or right click; forward wheel events to viewport
		if m.showDescriptionViewer {
			if msg.Action == tea.MouseActionPress &&
				(msg.Button == tea.MouseButtonLeft || msg.Button == tea.MouseButtonRight) {
				m.showDescriptionViewer = false
				return m, nil
			}
			var cmd tea.Cmd
			m.descriptionViewerVP, cmd = m.descriptionViewerVP.Update(msg)
			return m, cmd
		}
		// Notes overlay intercepts all mouse events when open
		if m.showNotesOverlay {
			return m, nil
		}
		// Orphan dialog requires explicit keyboard action; consume all mouse events.
		if m.orphanDialogForm != nil {
			return m, nil
		}
		// Dismiss overlays on left or right mouse button press
		if msg.Action == tea.MouseActionPress &&
			(msg.Button == tea.MouseButtonLeft || msg.Button == tea.MouseButtonRight) {
			if m.actionForm != nil || m.queueActionForm != nil || m.mergeForm != nil || m.orphanDialogForm != nil {
				m.actionForm = nil
				m.queueActionForm = nil
				m.mergeForm = nil
				m.orphanDialogForm = nil
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
		m.helpModel.Width = msg.Width
		if m.showLogViewer {
			vpWidth, vpHeight := m.logViewerDimensions()
			m.logViewerVP.Width = vpWidth
			m.logViewerVP.Height = vpHeight
		}
		if m.showDescriptionViewer {
			vpWidth, vpHeight := m.descriptionViewerDimensions()
			m.descriptionViewerVP.Width = vpWidth
			m.descriptionViewerVP.Height = vpHeight
			// Re-render description content so glamour wraps to the new width.
			m.descriptionViewerVP.SetContent(m.renderDescriptionViewerContent())
		}
		if m.showNotesOverlay {
			taW, taH := m.notesOverlayTextareaDimensions()
			m.notesTA.SetWidth(taW)
			m.notesTA.SetHeight(taH)
		}

	case UpdateQueueMsg:
		m.queue = msg.Items
		m.refreshing = false // fetch cycle complete; allow the next tick to refresh
		m.rebuildQueueNav()
		// Restore the cursor to the previously focused bead by ID (not by index) so
		// that a list reorder or insertion does not shift focus to a different bead.
		if m.focusedBeadID != "" {
			if beadIdx := findBeadIndexByID(m.queue, m.focusedBeadID); beadIdx >= 0 {
				for navIdx, nav := range m.queueNavItems {
					if !nav.isAnvil && nav.beadIdx == beadIdx {
						m.queueVP.cursor = navIdx
						break
					}
				}
			}
		}
		// Close the queue action menu or notes overlay if the target bead is no longer in the queue.
		if m.queueActionForm != nil {
			found := false
			for _, qi := range m.queue {
				if qi.BeadID == m.queueActionTarget.BeadID && qi.Anvil == m.queueActionTarget.Anvil && qi.Section == "unlabeled" {
					found = true
					break
				}
			}
			if !found {
				m.queueActionForm = nil
				m.queueActionTarget = nil
			}
		}

	case QueueErrorMsg:
		// Preserve previous queue; clear the refresh guard so the next tick
		// can retry rather than being permanently blocked.
		m.refreshing = false
		// Surface the error in the events panel
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
		// Close the crucible action menu if the target crucible is no longer present.
		if m.crucibleActionForm != nil && m.crucibleActionTarget != nil {
			found := false
			for _, c := range m.crucibles {
				if c.ParentID == m.crucibleActionTarget.ParentID && c.Anvil == m.crucibleActionTarget.Anvil {
					found = true
					break
				}
			}
			if !found {
				m.crucibleActionForm = nil
				m.crucibleActionTarget = nil
			}
		}

	case UpdateNeedsAttentionMsg:
		m.needsAttention = msg.Items
		m.needsAttnVP.ClampToTotal(len(msg.Items))

	case UpdateReadyToMergeMsg:
		m.readyToMerge = msg.Items
		m.readyToMergeVP.ClampToTotal(len(msg.Items))

	case UpdateOpenPRsMsg:
		m.prItems = msg.Items
		m.prVP.ClampToTotal(len(msg.Items))

	case wicketScanResultMsg:
		if msg.Err != nil {
			m.setStatus(fmt.Sprintf("Wicket scan failed: %v", msg.Err), true)
		}

	case reconcilePRsDoneMsg:
		// After reconciliation, refresh the PR list from the DB
		if m.data != nil {
			return m, FetchOpenPRs(m.data.DB)
		}

	case OpenPRsErrorMsg:
		// Preserve previous PR list; surface the error in the events panel.
		errEvent := EventItem{
			Timestamp: time.Now().Format("15:04:05"),
			Type:      "error",
			Message:   fmt.Sprintf("open PRs read failed: %v", msg.Err),
		}
		m.events = append([]EventItem{errEvent}, m.events...)
		m.eventRevision++
		if m.eventAutoScroll {
			m.eventScroll = 0
		}

	case UpdatePendingOrphansMsg:
		// Merge newly discovered orphans into the queue, avoiding duplicates.
		existing := make(map[string]bool, len(m.orphanQueue))
		for _, o := range m.orphanQueue {
			existing[o.Anvil+"/"+o.BeadID] = true
		}
		if m.orphanDialogForm != nil {
			existing[m.orphanTarget.Anvil+"/"+m.orphanTarget.BeadID] = true
		}
		for _, item := range msg.Items {
			if !existing[item.Anvil+"/"+item.BeadID] {
				m.orphanQueue = append(m.orphanQueue, item)
			}
		}
		// Show dialog if not already visible and we have queued orphans.
		if m.orphanDialogForm == nil && len(m.orphanQueue) > 0 {
			return m, m.dequeueNextOrphan()
		}

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
		// Capture the currently selected worker ID before updating, so we can
		// try to stay on the same worker if it's still present in the list.
		prevCursor := m.workerTable.Cursor()
		var prevWorkerID string
		if prevCursor >= 0 && prevCursor < len(m.workers) {
			prevWorkerID = m.workers[prevCursor].ID
		}

		m.workers = msg.Items

		// Update table rows
		rows := make([]table.Row, len(msg.Items))
		frame := SpinnerFrames[m.spinnerFrame%len(SpinnerFrames)]
		for i, w := range msg.Items {
			status := workerStatusIndicator(w.Status, frame)
			beadCol := w.BeadID
			if w.PRNumber > 0 {
				beadCol = fmt.Sprintf("%s (PR#%d)", w.BeadID, w.PRNumber)
			}
			taskCol := sanitizeTitle(w.Title)
			if taskCol == "" {
				taskCol = "(no title)"
			}
			rows[i] = table.Row{
				status,
				" " + w.Type,
				" " + beadCol,
				" " + taskCol,
				" " + w.Anvil,
				" " + w.Duration,
			}
		}
		m.workerTable.SetRows(rows)

		// Stay on the same worker ID if it still exists.
		if prevWorkerID != "" {
			for i, w := range m.workers {
				if w.ID == prevWorkerID {
					// Move relative to the current cursor so the viewport scrolls
					// into view without iterating from 0 every refresh (O(delta) not O(n)).
					cur := m.workerTable.Cursor()
					if i > cur {
						for range i - cur {
							m.workerTable.MoveDown(1)
						}
					} else if i < cur {
						for range cur - i {
							m.workerTable.MoveUp(1)
						}
					}
					break
				}
			}
		}

		// Clamp table cursor in case the ID vanished and the old index is out of bounds.
		if m.workerTable.Cursor() >= len(m.workers) {
			m.workerTable.SetCursor(max(0, len(m.workers)-1))
		}
		if m.workerTable.Cursor() < 0 && len(m.workers) > 0 {
			m.workerTable.SetCursor(0)
		}

		// Reset live activity state if the selected worker implicitly changed.
		newCursor := m.workerTable.Cursor()
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
		m.daemonPause = msg.DispatchPause

	case toastDismissMsg:
		newToasts := m.toasts[:0]
		for _, t := range m.toasts {
			if t.id != msg.id {
				newToasts = append(newToasts, t)
			}
		}
		m.toasts = newToasts

	case UpdateEventsMsg:
		// Capture the fingerprint of the most-recent event before updating, so
		// we can detect which events are new this cycle.
		prevKey := m.lastSeenEventKey

		m.events = msg.Items
		newKey := ""
		if len(msg.Items) > 0 {
			newKey = toastEventKey(msg.Items[0])
		}
		var filterCmd tea.Cmd
		if newKey != prevKey {
			filterCmd = m.applyEventFilter()
		}
		m.eventRevision++
		// Auto-scroll to bottom if enabled and new events arrived
		if m.eventAutoScroll && len(msg.Items) > m.prevEventCount {
			if len(msg.Items) > 0 {
				m.eventScroll = 0 // Events are newest-first from DB
			}
		}
		m.prevEventCount = len(msg.Items)

		// Update the last-seen key for the next cycle.
		if newKey != "" {
			m.lastSeenEventKey = newKey
		}

		// Detect new events and fire toast notifications for notable ones.
		// prevKey == "" means this is the first poll; skip toasting on startup.
		var toastCmds []tea.Cmd
		if prevKey != "" {
			for _, ev := range msg.Items {
				if toastEventKey(ev) == prevKey {
					break // reached already-seen events
				}
				if tmsg, isError, ok := toastForEvent(ev); ok {
					t := toast{id: m.nextToastID, message: tmsg, isError: isError}
					m.nextToastID++
					// Append so newest is last (closest to footer); cap at maxToasts by dropping oldest.
					m.toasts = append(m.toasts, t)
					if len(m.toasts) > maxToasts {
						m.toasts = m.toasts[1:]
					}
					toastCmds = append(toastCmds, scheduleToastDismiss(t.id))
				}
			}
		}
		if filterCmd != nil {
			toastCmds = append(toastCmds, filterCmd)
		}
		if len(toastCmds) > 0 {
			return m, tea.Batch(toastCmds...)
		}

	case UpdateFilteredEventsMsg:
		// Ignore results from a stale query — the filter text may have changed
		// while this DB search was in flight. Scroll position is left untouched
		// here so periodic refreshes don't jump the view; the user-facing filter
		// actions reset the scroll themselves.
		if msg.Filter == m.eventFilterText {
			m.filteredEvents = msg.Items
			m.eventRevision++
		}

	case UpdateWicketSummaryMsg:
		// Items is nil when the fetch failed. Preserve previous data on transient errors.
		if msg.Items != nil {
			m.wicketSummary = msg.Items
		}
		// Normalize focus: if the Wicket panel is focused but is no longer visible
		// (disabled or summary became empty), move focus to the nearest visible neighbor.
		if m.focused == PanelWicket && !m.wicketVisible() {
			m.focused = PanelWorkers
		}

	case UpdateIngotCountsMsg:
		// Counts is nil when the fetch failed (DB error, nil conn, etc.).
		// Skip the update to preserve the previously displayed values rather
		// than wiping them on a transient failure.
		if msg.Counts != nil {
			m.ingotCounts = msg.Counts
			m.ingotTotal = msg.Total
		}

	case UpdateAnvilHealthMsg:
		if m.anvilHealth == nil {
			m.anvilHealth = make(map[string]AnvilHealth)
		}
		for _, h := range msg.Items {
			m.anvilHealth[h.Anvil] = h
		}

	case CrucibleActionResultMsg:
		if msg.Err != nil {
			m.setStatus(fmt.Sprintf("Crucible %s %s failed: %v", msg.ParentID, msg.Action, msg.Err), true)
		} else {
			switch msg.Action {
			case "resume":
				m.setStatus(fmt.Sprintf("Crucible %s resumed", msg.ParentID), false)
			case "stop":
				m.setStatus(fmt.Sprintf("Crucible %s stopped", msg.ParentID), false)
			}
		}

	case QueueActionResultMsg:
		if msg.Err != nil {
			switch msg.Action {
			case "tag":
				m.setStatus(fmt.Sprintf("Failed to tag %s: %v", msg.BeadID, msg.Err), true)
			case "force_run":
				m.setStatus(fmt.Sprintf("Failed to force-run %s: %v", msg.BeadID, msg.Err), true)
			case "stop":
				m.setStatus(fmt.Sprintf("Failed to stop %s: %v", msg.BeadID, msg.Err), true)
			default:
				m.setStatus(fmt.Sprintf("Failed to close %s: %v", msg.BeadID, msg.Err), true)
			}
		} else if msg.RequestID != "" {
			var desc string
			switch msg.Action {
			case "tag":
				desc = fmt.Sprintf("Tagging %s", msg.BeadID)
			case "stop":
				desc = fmt.Sprintf("Stopping %s", msg.BeadID)
			case "close":
				desc = fmt.Sprintf("Closing %s", msg.BeadID)
			default:
				desc = fmt.Sprintf("%s %s", msg.Action, msg.BeadID)
			}
			m.trackPending(msg.RequestID, desc)
		} else {
			switch msg.Action {
			case "tag":
				m.setStatus(fmt.Sprintf("Tagged %s for dispatch", msg.BeadID), false)
			case "force_run":
				m.setStatus(fmt.Sprintf("Dispatched %s independently", msg.BeadID), false)
			case "stop":
				m.setStatus(fmt.Sprintf("Stopped %s", msg.BeadID), false)
			default:
				m.setStatus(fmt.Sprintf("Closed %s", msg.BeadID), false)
			}
		}

	case OrphanResolveResultMsg:
		if msg.Err != nil {
			m.setStatus(fmt.Sprintf("Failed to resolve orphan %s: %v", msg.BeadID, msg.Err), true)
		} else {
			switch msg.Action {
			case "recover":
				m.setStatus(fmt.Sprintf("Orphan %s recovered — re-queued for work", msg.BeadID), false)
			case "close":
				m.setStatus(fmt.Sprintf("Orphan %s closed (work completed)", msg.BeadID), false)
			case "discard":
				m.setStatus(fmt.Sprintf("Orphan %s discarded", msg.BeadID), false)
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

	case PRActionResultMsg:
		if msg.Err != nil {
			errSummary := strings.SplitN(msg.Err.Error(), "\n", 2)[0]
			m.setStatus(fmt.Sprintf("PR #%d %s failed: %s", msg.PRNumber, msg.Action, errSummary), true)
		} else if msg.RequestID != "" {
			m.trackPending(msg.RequestID, fmt.Sprintf("PR #%d: %s", msg.PRNumber, msg.Action))
		} else {
			m.setStatus(fmt.Sprintf("PR #%d: %s dispatched", msg.PRNumber, msg.Action), false)
		}

	case NotesResultMsg:
		if msg.Err != nil {
			m.setStatus(fmt.Sprintf("Failed to save notes for %s: %v", msg.BeadID, msg.Err), true)
		} else if msg.RequestID != "" {
			m.trackPending(msg.RequestID, fmt.Sprintf("Saving notes for %s", msg.BeadID))
			if m.notesTarget != nil && m.notesTarget.BeadID == msg.BeadID && m.notesTarget.Anvil == msg.Anvil {
				m.showNotesOverlay = false
				m.notesTarget = nil
				m.notesTA = textarea.Model{}
			}
		} else {
			m.setStatus(fmt.Sprintf("Notes saved for %s", msg.BeadID), false)
			if m.notesTarget != nil && m.notesTarget.BeadID == msg.BeadID && m.notesTarget.Anvil == msg.Anvil {
				m.showNotesOverlay = false
				m.notesTarget = nil
				m.notesTA = textarea.Model{}
			}
		}

	case asyncSubscriptionStartedMsg:
		m.asyncEvents = msg.events
		m.asyncCancelFunc = msg.cancel
		m.asyncClient = msg.client
		return m, waitForStreamEvent(msg.events)

	case AsyncCompletionMsg:
		if op, ok := m.pendingAsync[msg.RequestID]; ok {
			delete(m.pendingAsync, msg.RequestID)
			if msg.Success {
				detail := msg.Message
				if detail == "" {
					detail = "done"
				}
				m.setStatus(fmt.Sprintf("%s: %s", op.Description, detail), false)
			} else {
				detail := msg.Message
				if detail == "" {
					detail = "unknown error"
				}
				m.setStatus(fmt.Sprintf("%s failed: %s", op.Description, detail), true)
			}
		}
		if m.asyncEvents != nil {
			return m, waitForStreamEvent(m.asyncEvents)
		}

	case LoggedEventMsg:
		// A real event arrived over the subscribe stream. Mark streaming active
		// (the tick stops polling events from here) and re-arm the wait so the
		// channel keeps draining. Prepend the event and route it through the
		// UpdateEventsMsg path so toast/filter/auto-scroll bookkeeping is shared
		// with the poll path — no divergent update logic.
		m.eventStreamActive = true
		var cmds []tea.Cmd
		if m.asyncEvents != nil {
			cmds = append(cmds, waitForStreamEvent(m.asyncEvents))
		}
		item := EventItem{
			Timestamp: formatEventTimestamp(msg.Event.Timestamp),
			Type:      msg.Event.Type,
			Message:   msg.Event.Message,
			BeadID:    msg.Event.BeadID,
		}
		merged := append([]EventItem{item}, m.events...)
		if len(merged) > EventFetchLimit {
			merged = merged[:EventFetchLimit]
		}
		cmds = append(cmds, func() tea.Msg { return UpdateEventsMsg{Items: merged} })
		return m, tea.Batch(cmds...)

	case EventsGapMsg:
		// The bus dropped events for us (slow consumer). Re-sync the feed from
		// the DB once and re-arm the wait to resume live streaming.
		m.eventStreamActive = true
		var cmds []tea.Cmd
		if m.asyncEvents != nil {
			cmds = append(cmds, waitForStreamEvent(m.asyncEvents))
		}
		if m.data != nil {
			cmds = append(cmds, FetchEvents(m.data.DB, EventFetchLimit))
		}
		return m, tea.Batch(cmds...)

	case streamClosedMsg:
		// The IPC subscribe stream ended (daemon restart, dropped connection, or
		// a failed initial connect). Tear down the dead subscription, drop the
		// streaming latch so the tick's poll fallback resumes immediately — the
		// feed keeps updating instead of freezing — and schedule a reconnect.
		m.cleanupAsyncSubscription()
		m.asyncEvents = nil
		m.eventStreamActive = false
		return m, scheduleSubscriptionReconnect()

	case reconnectSubscriptionMsg:
		// Delay elapsed after a dropped stream; attempt a fresh subscription. A
		// failed connect yields another streamClosedMsg, continuing the retry
		// loop while the poll fallback covers the feed.
		return m, startAsyncSubscription()

	case TickMsg:
		// On each tick, refresh all panels and schedule the next tick.
		// Daemon health is checked every healthTickDivisor ticks to avoid
		// issuing a full IPC status round-trip on every 5s cycle.
		// The refreshing guard prevents concurrent FetchAll calls when a
		// previous refresh is still in flight.
		if m.data != nil {
			m.healthTickCount++
			m.expirePendingOps()
			cmds := []tea.Cmd{Tick()}
			if !m.refreshing {
				m.refreshing = true
				// Capture the focused bead now, before the refresh, so
				// cursor restoration always reflects the current selection
				// even if the user hasn't scrolled since startup.
				m.ensureQueueNav()
				m.captureFocusedBeadID()
				cmds = append(cmds, FetchAll(m.data, m.logCache))
				// Event feed rides the subscribe stream once active; only fall
				// back to polling it while streaming is inactive (bus disabled or
				// no event pushed yet). status/queue polls always run above.
				if !m.eventStreamActive {
					cmds = append(cmds, FetchEvents(m.data.DB, EventFetchLimit))
				}
			}
			if m.healthTickCount%healthTickDivisor == 0 {
				cmds = append(cmds, FetchDaemonHealth())
			}
			return m, tea.Batch(cmds...)
		}

	case SpinnerTickMsg:
		// Advance spinner frame and schedule the next spinner tick.
		m.spinnerFrame = (m.spinnerFrame + 1) % len(SpinnerFrames)

		// Only animate status cells for workers that use a spinner frame
		// (running/reviewing). Skip SetRows entirely when no spinner is active.
		if len(m.workers) > 0 {
			frame := SpinnerFrames[m.spinnerFrame%len(SpinnerFrames)]
			var hasSpinner bool
			for _, w := range m.workers {
				if w.Status == "running" || w.Status == "reviewing" {
					hasSpinner = true
					break
				}
			}
			if hasSpinner {
				rows := m.workerTable.Rows()
				for i, w := range m.workers {
					if i < len(rows) && (w.Status == "running" || w.Status == "reviewing") {
						rows[i][0] = workerStatusIndicator(w.Status, frame)
					}
				}
				m.workerTable.SetRows(rows)
			}
		}

		return m, SpinnerTick()

	default:
		// Forward unhandled messages (e.g. cursor blink) to the event filter
		// textinput when it is active.
		if m.eventFilterActive {
			var cmd tea.Cmd
			m.eventFilter, cmd = m.eventFilter.Update(msg)
			if cmd != nil {
				return m, cmd
			}
		}
	}

	return m, nil
}

// View implements tea.Model.
// computeHeaderH returns the rendered height of the header bar.
// It is called by both View() and panelAtPos() to keep hit-testing in sync
// without mutating model state inside View().
func (m *Model) computeHeaderH() int {
	return lipgloss.Height(headerStyle.Width(m.width).Render(m.headerText()))
}

// headerText builds the header bar contents: title, daemon connectivity and,
// when auto-dispatch is paused, the pause line naming its cause. Shared by
// View() and computeHeaderH() so hit-testing measures what is rendered.
func (m *Model) headerText() string {
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
	if m.daemonPause != "" {
		headerText += "  " + lipgloss.NewStyle().Foreground(colorWarning).Render("⏸ "+m.daemonPause)
	}
	return headerText
}

// defaultFooterHints returns the help component's keybinding hint line for the
// currently focused panel.
//
// The Bubbles help.Model is used for both layout width tracking (helpModel.Width)
// and for rendering the actual text output based on the focused panel's key map.
func (m *Model) defaultFooterHints() string {
	return m.helpModel.View(m.keyMapForPanel())
}

// computeFooterH returns the rendered height of the footer bar.
// It uses the default hint text (status messages are at most 1 line).
// It is called by panelAtPos() to mirror the layout used by View().
func (m *Model) computeFooterH() int {
	return lipgloss.Height(footerStyle.Width(m.width).Render(m.defaultFooterHints()))
}

func (m *Model) View() string {
	if !m.ready {
		return "Initializing The Forge..."
	}

	// Header with daemon health indicator
	header := headerStyle.Width(m.width).Render(m.headerText())
	headerH := lipgloss.Height(header)

	// Footer with status message or default hints
	footerText := m.defaultFooterHints()
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
	} else if len(m.pendingAsync) > 0 {
		ops := make([]pendingOp, 0, len(m.pendingAsync))
		for _, op := range m.pendingAsync {
			ops = append(ops, op)
		}
		slices.SortFunc(ops, func(a, b pendingOp) int {
			return a.StartedAt.Compare(b.StartedAt)
		})
		var parts []string
		for _, op := range ops {
			parts = append(parts, op.Description+"…")
		}
		footerText = statusMsgStyle.Render(strings.Join(parts, " | "))
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

	// Render toast notifications above the footer, below any action menus.
	if toastStr := m.renderToasts(); toastStr != "" {
		view = placeToastsOverlay(m.width, m.height, footerH, toastStr, view)
	}

	// Render overlays on top
	if m.showPRPanel {
		overlay := m.renderPRPanel()
		view = placeOverlay(m.width, m.height, overlay, view)
		if m.prActionForm != nil {
			formOverlay := m.renderPRActionMenu()
			view = placeOverlay(m.width, m.height, formOverlay, view)
		}
	} else if m.showLogViewer {
		overlay := m.renderLogViewer()
		view = placeOverlay(m.width, m.height, overlay, view)
	} else if m.showDescriptionViewer {
		overlay := m.renderDescriptionViewer()
		view = placeOverlay(m.width, m.height, overlay, view)
	} else if m.showNotesOverlay {
		overlay := m.renderNotesOverlay()
		view = placeOverlay(m.width, m.height, overlay, view)
	} else if m.orphanDialogForm != nil {
		overlay := m.renderOrphanDialog()
		view = placeOverlay(m.width, m.height, overlay, view)
	} else if m.crucibleActionForm != nil {
		overlay := m.renderCrucibleActionMenu()
		view = placeOverlay(m.width, m.height, overlay, view)
	} else if m.actionForm != nil {
		overlay := m.renderActionMenu()
		view = placeOverlay(m.width, m.height, overlay, view)
	} else if m.forceSmithNoteForm != nil {
		overlay := m.renderForceSmithNoteForm()
		view = placeOverlay(m.width, m.height, overlay, view)
	} else if m.queueActionForm != nil {
		overlay := m.renderQueueActionMenu()
		view = placeOverlay(m.width, m.height, overlay, view)
	} else if m.mergeForm != nil {
		overlay := m.renderMergeMenu()
		view = placeOverlay(m.width, m.height, overlay, view)
	}

	return view
}

func (m *Model) getTopPanelWidths() (queueWidth, workerWidth, activityWidth int) {
	// panelStyle.Width(w) includes padding but NOT border.
	// Each panel adds 2 border chars (left + right) to its rendered width.
	// Three columns → subtract 3×2 = 6 so the total fits exactly in m.width.
	remainingWidth := m.width - 6
	if remainingWidth < 0 {
		remainingWidth = 0
	}
	queueWidth = remainingWidth / 5
	workerWidth = remainingWidth * 2 / 5
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
		// Center column: Workers (top) + optional Wicket (middle) + Usage (bottom).
		fullH := topH + bottomH
		if fullH >= 20 {
			const usagePanelHeight = 11 // matches renderCenterColumn: inner 9 + 2 border rows
			if m.wicketVisible() {
				numRows := len(m.wicketSummary)
				if numRows > 4 {
					numRows = 4
				}
				wicketTotalH := numRows + 1 + 2 // header + rows + border
				workerH := fullH - usagePanelHeight - wicketTotalH
				if workerH >= 5 {
					if contentY < workerH+2 {
						return PanelWorkers
					}
					if contentY < workerH+2+wicketTotalH {
						return PanelWicket
					}
					return PanelUsage
				}
			}
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
		m.captureFocusedBeadID()
	case PanelCrucibles:
		m.crucibleVP.ScrollDown(len(m.crucibles))
	case PanelNeedsAttention:
		m.needsAttnVP.ScrollDown(len(m.needsAttention))
	case PanelReadyToMerge:
		m.readyToMergeVP.ScrollDown(len(m.readyToMerge))
	case PanelWorkers:
		prev := m.workerTable.Cursor()
		m.workerTable.MoveDown(1)
		if m.workerTable.Cursor() != prev {
			m.resetActivityState()
		}
	case PanelLiveActivity:
		m.activityVP.ScrollDown(m.activityLineCount)
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
		m.captureFocusedBeadID()
	case PanelCrucibles:
		m.crucibleVP.ScrollUp()
	case PanelNeedsAttention:
		m.needsAttnVP.ScrollUp()
	case PanelReadyToMerge:
		m.readyToMergeVP.ScrollUp()
	case PanelWorkers:
		prev := m.workerTable.Cursor()
		m.workerTable.MoveUp(1)
		if m.workerTable.Cursor() != prev {
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
	if len(m.workers) > 0 && m.workerTable.Cursor() < len(m.workers) {
		return m.workers[m.workerTable.Cursor()].ActivityLines
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

// addToast adds a toast notification to the active toasts list and returns
// a Cmd that auto-dismisses it after toastDuration. Oldest toast is dropped
// when the cap (maxToasts) is exceeded.
func (m *Model) addToast(message string, isError bool) tea.Cmd {
	t := toast{id: m.nextToastID, message: message, isError: isError}
	m.nextToastID++
	m.toasts = append(m.toasts, t)
	if len(m.toasts) > maxToasts {
		m.toasts = m.toasts[1:]
	}
	return scheduleToastDismiss(t.id)
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
	return &m.queue[nav.beadIdx]
}

// findBeadIndexByID returns the index of the first QueueItem with the given BeadID
// in items, or -1 if not found.
func findBeadIndexByID(items []QueueItem, id string) int {
	for i, item := range items {
		if item.BeadID == id {
			return i
		}
	}
	return -1
}

// captureFocusedBeadID records the BeadID of the queue nav item currently under
// the cursor into m.focusedBeadID. Called after any queue cursor movement so
// that the next refresh can restore the cursor to the same logical bead.
func (m *Model) captureFocusedBeadID() {
	if len(m.queueNavItems) == 0 || m.queueVP.cursor >= len(m.queueNavItems) {
		// No valid nav item under the cursor; clear focused bead so refresh
		// doesn't restore a stale selection.
		m.focusedBeadID = ""
		return
	}
	nav := m.queueNavItems[m.queueVP.cursor]
	// If the cursor is on an anvil header or an invalid bead index, treat this
	// as "no bead focused" and clear the stored bead ID.
	if nav.isAnvil || nav.beadIdx < 0 || nav.beadIdx >= len(m.queue) {
		m.focusedBeadID = ""
		return
	}
	m.focusedBeadID = m.queue[nav.beadIdx].BeadID
}

// executeAction runs the selected action menu choice against the target bead.
func (m *Model) executeAction(choice ActionMenuChoice) tea.Cmd {
	if m.actionTarget == nil {
		return nil
	}
	bead := *m.actionTarget
	// Use title as label for non-bead PRs (e.g. warden-learn PRs) so status
	// messages don't render blank when BeadID is empty.
	label := bead.BeadID
	if label == "" {
		label = sanitizeTitle(bead.Title)
	}
	switch choice {
	case ActionRetry:
		if m.OnRetryBead != nil {
			reqID, err := m.OnRetryBead(bead.BeadID, bead.Anvil, bead.PRID)
			if err != nil {
				m.setStatus(fmt.Sprintf("Failed to retry %s: %v", label, err), true)
			} else if reqID != "" {
				m.trackPending(reqID, fmt.Sprintf("Retrying %s", label))
				m.removeNeedsAttentionItem(bead.BeadID, bead.Anvil, bead.PRID)
				if m.data != nil {
					return FetchNeedsAttention(m.data)
				}
				return nil
			} else {
				m.setStatus(fmt.Sprintf("Retry queued for %s", label), false)
				m.removeNeedsAttentionItem(bead.BeadID, bead.Anvil, bead.PRID)
				if m.data != nil {
					return FetchNeedsAttention(m.data)
				}
				return nil
			}
		} else {
			m.setStatus(fmt.Sprintf("Retry action unavailable for %s", label), false)
		}
	case ActionDismiss:
		if m.OnDismissBead != nil {
			if err := m.OnDismissBead(bead.BeadID, bead.Anvil, bead.PRID); err != nil {
				m.setStatus(fmt.Sprintf("Failed to dismiss %s: %v", label, err), true)
			} else {
				m.setStatus(fmt.Sprintf("Dismissed %s", label), false)
				m.removeNeedsAttentionItem(bead.BeadID, bead.Anvil, bead.PRID)
				if m.data != nil {
					return FetchNeedsAttention(m.data)
				}
				return nil
			}
		} else {
			m.setStatus(fmt.Sprintf("Dismiss action unavailable for %s", label), false)
		}
	case ActionViewLogs:
		if m.OnViewLogs != nil {
			logPath, lines := m.OnViewLogs(bead.BeadID)
			if logPath == "" {
				m.setStatus(fmt.Sprintf("No logs found for %s", bead.BeadID), false)
				return nil
			}
			m.openLogViewer(fmt.Sprintf("Log: %s — %s", bead.BeadID, logPath), strings.Join(lines, "\n"))
		}
	case ActionWardenRerun:
		if m.OnWardenRerun != nil {
			if err := m.OnWardenRerun(bead.BeadID, bead.Anvil); err != nil {
				m.setStatus(fmt.Sprintf("Failed to re-run warden for %s: %v", bead.BeadID, err), true)
			} else {
				m.setStatus(fmt.Sprintf("Warden re-review started for %s", bead.BeadID), false)
				m.removeNeedsAttentionItem(bead.BeadID, bead.Anvil, bead.PRID)
				if m.data != nil {
					return FetchNeedsAttention(m.data)
				}
				return nil
			}
		} else {
			m.setStatus(fmt.Sprintf("Warden re-run action unavailable for %s", bead.BeadID), false)
		}
	case ActionApproveAsIs:
		if m.OnApproveAsIs != nil {
			if err := m.OnApproveAsIs(bead.BeadID, bead.Anvil); err != nil {
				m.setStatus(fmt.Sprintf("Failed to approve %s: %v", bead.BeadID, err), true)
			} else {
				m.setStatus(fmt.Sprintf("Approve as-is started for %s", bead.BeadID), false)
				m.removeNeedsAttentionItem(bead.BeadID, bead.Anvil, bead.PRID)
				if m.data != nil {
					return FetchNeedsAttention(m.data)
				}
				return nil
			}
		} else {
			m.setStatus(fmt.Sprintf("Approve as-is action unavailable for %s", bead.BeadID), false)
		}
	case ActionForceSmith:
		if m.OnForceSmith != nil {
			// Pre-fill the note with the last warden rejection so the user can
			// edit/augment it before re-dispatching Smith.
			m.forceSmithNote = bead.LastWardenReject
			m.forceSmithNoteTarget = &bead
			desc := "Edit the warden feedback or add your own note (Enter to confirm):"
			if m.forceSmithNote == "" {
				desc = "Optional note to prepend to warden feedback (Enter to skip):"
			}
			m.forceSmithNoteForm = huh.NewForm(
				huh.NewGroup(
					huh.NewInput().
						Title(fmt.Sprintf("Force smith for %s", bead.BeadID)).
						Description(desc).
						Value(&m.forceSmithNote),
				),
			).WithTheme(huh.ThemeBase())
			return m.forceSmithNoteForm.Init()
		} else {
			m.setStatus(fmt.Sprintf("Force smith action unavailable for %s", bead.BeadID), false)
		}
	}
	return nil
}

// openLogViewer initialises and displays the viewport log viewer overlay with
// the given title and pre-joined content string.
func (m *Model) openLogViewer(title, content string) {
	m.logViewerTitle = title
	m.logViewerEmpty = len(content) == 0
	vpWidth, vpHeight := m.logViewerDimensions()
	m.logViewerVP = viewport.New(vpWidth, vpHeight)
	m.logViewerVP.SetContent(content)
	m.showLogViewer = true
}

// removeNeedsAttentionItem removes the item with the given beadID and anvil from the
// needsAttention list and adjusts the scroll position if necessary.
// For non-bead PRs (beadID == ""), matching falls back to prID so that multiple
// exhausted PRs in the same anvil are removed individually.
func (m *Model) removeNeedsAttentionItem(beadID, anvil string, prID ...int) {
	prid := 0
	if len(prID) > 0 {
		prid = prID[0]
	}
	for i, item := range m.needsAttention {
		// Non-bead entries (anvil, self-deploy) are never the target of a bead/PR
		// action and also carry an empty bead ID, so they must not be matched by
		// the beadID=="" fallback below.
		if !item.IsBeadScoped() {
			continue
		}
		var matched bool
		// For non-bead PRs, prefer matching by PRID (and anvil) so that multiple
		// exhausted PRs in the same anvil are removed deterministically.
		if beadID == "" && prid > 0 {
			matched = item.PRID == prid && item.Anvil == anvil
		} else {
			matched = item.BeadID == beadID && item.Anvil == anvil
		}
		if matched {
			m.needsAttention = append(m.needsAttention[:i], m.needsAttention[i+1:]...)
			m.needsAttnVP.ClampToTotal(len(m.needsAttention))
			return
		}
	}
}

// dequeueNextOrphan shows the next orphan from the queue, or hides the dialog
// if the queue is empty. It returns the Init() command of the new form.
func (m *Model) dequeueNextOrphan() tea.Cmd {
	if len(m.orphanQueue) == 0 {
		m.orphanDialogForm = nil
		m.orphanTarget = nil
		return nil
	}
	item := m.orphanQueue[0]
	m.orphanQueue = m.orphanQueue[1:]

	// Store a stable heap-allocated copy of the item for the dialog target.
	m.orphanTarget = new(PendingOrphanItem)
	*m.orphanTarget = item

	// Always start each orphan dialog with the default choice (Recover)
	m.orphanDialogChoice = OrphanActionRecover

	m.orphanDialogForm = buildOrphanDialogForm(m.orphanTarget, &m.orphanDialogChoice)
	return m.orphanDialogForm.Init()
}

// buildOrphanDialogForm creates a huh form for the orphan resolution dialog.
func buildOrphanDialogForm(item *PendingOrphanItem, choice *OrphanDialogChoice) *huh.Form {
	return huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[OrphanDialogChoice]().
				Title(fmt.Sprintf("Orphan Worker Detected: %s", item.BeadID)).
				Options(
					huh.NewOption("Recover — reopen and re-queue for work", OrphanActionRecover),
					huh.NewOption("Close   — mark done (work already completed)", OrphanActionClose),
					huh.NewOption("Discard — close without retry", OrphanActionDiscard),
				).
				Value(choice),
		),
	).WithTheme(huh.ThemeCharm())
}

// renderOrphanDialog renders the orphan dialog overlay with bead info and pending count hint.
func (m *Model) renderOrphanDialog() string {
	if m.orphanDialogForm == nil || m.orphanTarget == nil {
		return ""
	}
	item := m.orphanTarget
	const maxWidth = 60

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Orphan Worker Detected: %s", item.BeadID))

	if item.Title != "" {
		wrapped := wordWrap(sanitizeTitle(item.Title), maxWidth)
		for _, line := range wrapped {
			sb.WriteByte('\n')
			sb.WriteString(line)
		}
	}

	sb.WriteByte('\n')
	sb.WriteString("No active worker found. What should happen?")
	sb.WriteByte('\n')
	sb.WriteString(m.orphanDialogForm.View())

	if n := len(m.orphanQueue); n > 0 {
		sb.WriteString(fmt.Sprintf("\n(%d more pending)", n))
	}

	return actionMenuStyle.Render(sb.String())
}

// executeOrphanAction sends the user's resolution choice to the daemon.
func (m *Model) executeOrphanAction(choice OrphanDialogChoice) tea.Cmd {
	if m.orphanTarget == nil {
		return nil
	}
	target := *m.orphanTarget
	m.orphanDialogForm = nil
	m.orphanTarget = nil
	nextCmd := m.dequeueNextOrphan()

	var action string
	switch choice {
	case OrphanActionRecover:
		action = "recover"
	case OrphanActionClose:
		action = "close"
	case OrphanActionDiscard:
		action = "discard"
	default:
		return nextCmd
	}

	if m.OnResolveOrphan == nil {
		m.setStatus(fmt.Sprintf("Orphan resolution unavailable for %s", target.BeadID), true)
		return nextCmd
	}

	beadID, anvil := target.BeadID, target.Anvil
	cb := m.OnResolveOrphan
	actionCmd := func() tea.Msg {
		return OrphanResolveResultMsg{BeadID: beadID, Action: action, Err: cb(beadID, anvil, action)}
	}
	return tea.Batch(nextCmd, actionCmd)
}

// notesTarget holds the bead context for the inline notes overlay.
type notesTarget struct {
	BeadID string
	Anvil  string
	Title  string
}

// NotesResultMsg is delivered asynchronously when an append-notes action completes.
type NotesResultMsg struct {
	BeadID    string
	Anvil     string
	Err       error
	RequestID string
}

// CrucibleActionResultMsg is delivered asynchronously when a crucible action completes.
type CrucibleActionResultMsg struct {
	ParentID string
	Action   string // "resume" or "stop"
	Err      error
}

// buildCrucibleActionForm creates a huh form for the crucible action menu.
func buildCrucibleActionForm(item *CrucibleItem, choice *CrucibleActionMenuChoice) *huh.Form {
	return huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[CrucibleActionMenuChoice]().
				Title(fmt.Sprintf("Actions for Crucible %s", item.ParentID)).
				Options(
					huh.NewOption("Resume — Retry parent to re-enter crucible loop", CrucibleActionResume),
					huh.NewOption("Stop   — Close the parent bead", CrucibleActionStop),
				).
				Value(choice),
		),
	).WithTheme(huh.ThemeCharm()).WithWidth(60)
}

// renderCrucibleActionMenu renders the crucible action overlay with crucible info
// header followed by the huh form's action select.
func (m *Model) renderCrucibleActionMenu() string {
	if m.crucibleActionForm == nil || m.crucibleActionTarget == nil {
		return ""
	}
	item := m.crucibleActionTarget
	const maxWidth = 60

	var sb strings.Builder
	sb.WriteString(actionMenuTitleStyle.Render(fmt.Sprintf("Actions for Crucible %s", item.ParentID)))

	if item.ParentTitle != "" {
		sb.WriteByte('\n')
		title := truncate(sanitizeTitle(item.ParentTitle), maxWidth-4)
		sb.WriteString(dimStyle.Render(title))
	}

	// Show phase and progress info
	sb.WriteByte('\n')
	sb.WriteString(dimStyle.Render(fmt.Sprintf("Phase: %s  Children: %d/%d  Anvil: %s",
		item.Phase, item.CompletedChildren, item.TotalChildren, item.Anvil)))

	sb.WriteString("\n\n")
	sb.WriteString(m.crucibleActionForm.View())
	sb.WriteString("\n\n" + dimStyle.Render("esc: dismiss"))
	return actionMenuStyle.Render(sb.String())
}

// executeCrucibleAction dispatches the selected crucible action asynchronously.
func (m *Model) executeCrucibleAction(choice CrucibleActionMenuChoice) tea.Cmd {
	if m.crucibleActionTarget == nil {
		return nil
	}
	item := *m.crucibleActionTarget

	var action string
	switch choice {
	case CrucibleActionResume:
		action = "resume"
	case CrucibleActionStop:
		action = "stop"
	default:
		return nil
	}

	if m.OnCrucibleAction == nil {
		m.setStatus("Crucible action unavailable (daemon not connected)", true)
		return nil
	}

	m.setStatus(fmt.Sprintf("Crucible %s: %s...", item.ParentID, action), false)
	parentID, anvil := item.ParentID, item.Anvil
	cb := m.OnCrucibleAction
	return func() tea.Msg {
		return CrucibleActionResultMsg{ParentID: parentID, Action: action, Err: cb(parentID, anvil, action)}
	}
}

// QueueActionResultMsg is delivered asynchronously when a queue action completes.
type QueueActionResultMsg struct {
	BeadID    string
	Action    string // "tag", "force_run", "close", or "stop"
	Err       error
	RequestID string // non-empty when the action was queued for async processing
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
	case QueueActionForceRun:
		return m.forceRunSelectedQueueItem()
	case QueueActionClose:
		return m.closeSelectedQueueItem()
	case QueueActionStop:
		return m.stopSelectedQueueItem()
	}
	return nil
}

// tagSelectedQueueItem tags the bead stored in queueActionTarget for dispatch.
func (m *Model) tagSelectedQueueItem() tea.Cmd {
	if m.queueActionTarget == nil {
		return nil
	}
	item := *m.queueActionTarget
	if m.OnTagBead == nil {
		m.setStatus(fmt.Sprintf("Label action unavailable for %s", item.BeadID), false)
		return nil
	}
	m.setStatus(fmt.Sprintf("Tagging %s…", item.BeadID), false)
	beadID, anvil := item.BeadID, item.Anvil
	cb := m.OnTagBead
	return func() tea.Msg {
		reqID, err := cb(beadID, anvil)
		return QueueActionResultMsg{BeadID: beadID, Action: "tag", Err: err, RequestID: reqID}
	}
}

// closeSelectedQueueItem closes the bead stored in queueActionTarget.
func (m *Model) closeSelectedQueueItem() tea.Cmd {
	if m.queueActionTarget == nil {
		return nil
	}
	item := *m.queueActionTarget
	if m.OnCloseBead == nil {
		m.setStatus(fmt.Sprintf("Close action unavailable for %s", item.BeadID), false)
		return nil
	}
	m.setStatus(fmt.Sprintf("Closing %s…", item.BeadID), false)
	beadID, anvil := item.BeadID, item.Anvil
	cb := m.OnCloseBead
	return func() tea.Msg {
		reqID, err := cb(beadID, anvil)
		return QueueActionResultMsg{BeadID: beadID, Action: "close", Err: err, RequestID: reqID}
	}
}

// stopSelectedQueueItem stops all processing of the bead stored in queueActionTarget.
func (m *Model) stopSelectedQueueItem() tea.Cmd {
	if m.queueActionTarget == nil {
		return nil
	}
	item := *m.queueActionTarget
	if m.OnStopBead == nil {
		m.setStatus(fmt.Sprintf("Stop action unavailable for %s", item.BeadID), false)
		return nil
	}
	m.setStatus(fmt.Sprintf("Stopping %s…", item.BeadID), false)
	beadID, anvil := item.BeadID, item.Anvil
	cb := m.OnStopBead
	return func() tea.Msg {
		reqID, err := cb(beadID, anvil)
		return QueueActionResultMsg{BeadID: beadID, Action: "stop", Err: err, RequestID: reqID}
	}
}

// forceRunSelectedQueueItem dispatches the bead independently, bypassing
// bd ready and skipping crucible/parent checks.
func (m *Model) forceRunSelectedQueueItem() tea.Cmd {
	if m.queueActionTarget == nil {
		return nil
	}
	item := *m.queueActionTarget
	if m.OnForceRunBead == nil {
		m.setStatus(fmt.Sprintf("Force-run action unavailable for %s", item.BeadID), false)
		return nil
	}
	m.setStatus(fmt.Sprintf("Force-running %s…", item.BeadID), false)
	beadID, anvil := item.BeadID, item.Anvil
	cb := m.OnForceRunBead
	return func() tea.Msg {
		return QueueActionResultMsg{BeadID: beadID, Action: "force_run", Err: cb(beadID, anvil)}
	}
}

// buildMergeForm creates a huh form for the merge menu.
func buildMergeForm(item *ReadyToMergeItem, choice *MergeMenuChoice) *huh.Form {
	return huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[MergeMenuChoice]().
				Title(fmt.Sprintf("Actions for PR #%d", item.PRNumber)).
				Options(
					huh.NewOption("Merge — Merge this PR", MergeActionMerge),
				).
				Value(choice),
		),
	).WithTheme(huh.ThemeCharm()).WithWidth(60)
}

// renderMergeMenu renders the merge action overlay with PR info header followed by the huh form.
func (m *Model) renderMergeMenu() string {
	if m.mergeForm == nil || m.mergeTarget == nil {
		return ""
	}
	item := m.mergeTarget
	const maxWidth = 60
	const maxTitleLines = 2

	var sb strings.Builder
	sb.WriteString(actionMenuTitleStyle.Render(fmt.Sprintf("Actions for %s — PR #%d", item.BeadID, item.PRNumber)))

	if item.Title != "" {
		sb.WriteByte('\n')
		wrapped := wordWrap(sanitizeTitle(item.Title), maxWidth)
		if len(wrapped) > maxTitleLines {
			last := []rune(wrapped[maxTitleLines-1])
			if len(last) > maxWidth-3 {
				last = last[:maxWidth-3]
			}
			wrapped = append(wrapped[:maxTitleLines-1], string(last)+"...")
		}
		for _, line := range wrapped {
			sb.WriteByte('\n')
			sb.WriteString(dimStyle.Render(line))
		}
	}

	sb.WriteString("\n\n")
	sb.WriteString(m.mergeForm.View())
	sb.WriteString("\n\n" + dimStyle.Render("esc: dismiss"))
	return actionMenuStyle.Render(sb.String())
}

// renderPRPanel renders the full-screen PR panel overlay.
func (m *Model) renderPRPanel() string {
	viewerWidth := m.width - 8
	if viewerWidth < 50 {
		viewerWidth = 50
	}
	viewerHeight := m.height - 6
	if viewerHeight < 12 {
		viewerHeight = 12
	}

	title := actionMenuTitleStyle.Render(fmt.Sprintf("Open Pull Requests (%d)", len(m.prItems)))
	var lines []string
	lines = append(lines, title)
	lines = append(lines, "")

	if len(m.prItems) == 0 {
		lines = append(lines, dimStyle.Render("No open PRs"))
	} else {
		maxItems := (viewerHeight - 6) / 2 // each item renders up to 2 lines (main + title)
		if maxItems < 1 {
			maxItems = 1
		}
		m.prVP.AdjustViewport(maxItems, len(m.prItems))
		start, end := m.prVP.VisibleRange(maxItems, len(m.prItems))

		// Header
		innerW := viewerWidth - logViewerStyle.GetHorizontalFrameSize()
		if innerW < 30 {
			innerW = 30
		}
		header := dimStyle.Render(fmt.Sprintf("  %-6s %-12s %-10s %-6s %s", "PR", "Bead", "Anvil", "Status", "CI  Comments  Conflicts"))
		lines = append(lines, header)

		for i := start; i < end; i++ {
			pr := m.prItems[i]
			selected := i == m.prVP.cursor

			// Three fixed status flags: CI, Comments (unresolved), Conflicts
			ciFlag := lipgloss.NewStyle().Foreground(colorGreen).Render("CI✓")
			if !pr.CIPassing {
				ciFlag = lipgloss.NewStyle().Foreground(colorDanger).Render("CI✗")
			}
			commentsFlag := lipgloss.NewStyle().Foreground(colorGreen).Render("comments✓")
			if pr.HasUnresolvedThreads {
				commentsFlag = lipgloss.NewStyle().Foreground(colorDanger).Render("comments✗")
			}
			conflictsFlag := lipgloss.NewStyle().Foreground(colorGreen).Render("conflicts✓")
			if pr.IsConflicting {
				conflictsFlag = lipgloss.NewStyle().Foreground(colorDanger).Render("conflicts✗")
			}
			flagStr := ciFlag + "  " + commentsFlag + "  " + conflictsFlag

			prNum := fmt.Sprintf("#%-5d", pr.PRNumber)
			extTag := ""
			if pr.IsExternal && !pr.BellowsManaged {
				extTag = dimStyle.Render(" [ext]")
			} else if pr.IsExternal && pr.BellowsManaged {
				extTag = lipgloss.NewStyle().Foreground(colorBlue).Render(" [ext+bellows]")
			}
			line := fmt.Sprintf("  %s %-12s %-10s %-10s %s", prNum, pr.BeadID, pr.Anvil, pr.Status, flagStr) + extTag
			if pr.Title != "" {
				titleDisplay := sanitizeTitle(pr.Title)
				maxTitleLen := innerW - 10
				if maxTitleLen > 0 {
					titleDisplay = truncate(titleDisplay, maxTitleLen)
				}
				line += "\n    " + dimStyle.Render(titleDisplay)
			}
			if selected {
				line = selectedStyle.Render("▸" + line[1:])
			}
			lines = append(lines, line)
		}
	}

	lines = append(lines, "")
	lines = append(lines, dimStyle.Render("j/k: scroll • enter: actions • p/esc: close"))

	content := strings.Join(lines, "\n")
	return logViewerStyle.Width(viewerWidth).Height(viewerHeight).Render(content)
}

// renderPRActionMenu renders the action menu overlay for a selected PR.
func (m *Model) renderPRActionMenu() string {
	if m.prActionForm == nil || m.prActionTarget == nil {
		return ""
	}
	item := m.prActionTarget
	var sb strings.Builder
	sb.WriteString(actionMenuTitleStyle.Render(fmt.Sprintf("Actions for PR #%d — %s", item.PRNumber, item.BeadID)))
	if item.Title != "" {
		sb.WriteByte('\n')
		title := truncate(sanitizeTitle(item.Title), 55)
		sb.WriteString(dimStyle.Render(title))
	}
	sb.WriteString("\n\n")
	sb.WriteString(m.prActionForm.View())
	sb.WriteString("\n\n" + dimStyle.Render("esc: dismiss"))
	return actionMenuStyle.Render(sb.String())
}

// buildPRActionForm creates the huh form for PR actions.
func (m *Model) buildPRActionForm(item *PRItem, choice *PRActionMenuChoice) *huh.Form {
	opts := []huh.Option[PRActionMenuChoice]{
		huh.NewOption("Open in browser  — View on GitHub", PRActionOpenBrowser),
	}
	if !item.IsConflicting {
		opts = append(opts, huh.NewOption("Merge            — Merge this pull request", PRActionMerge))
	}
	// Lifecycle actions (quench, burnish, rebase) only for forge-managed or bellows-assigned PRs
	if item.BellowsManaged {
		if !item.CIPassing {
			opts = append(opts, huh.NewOption("Fix CI           — Run quench worker", PRActionFixCI))
		}
		if item.HasUnresolvedThreads {
			opts = append(opts, huh.NewOption("Fix comments     — Run burnish worker", PRActionFixComments))
		}
		if item.IsConflicting {
			opts = append(opts, huh.NewOption("Fix conflict     — Run rebase worker", PRActionResolveConflicts))
		}
	}
	// External PRs that aren't bellows-managed can be assigned
	if item.IsExternal && !item.BellowsManaged {
		opts = append(opts, huh.NewOption("Assign bellows   — Auto-monitor & fix CI/reviews", PRActionAssignBellows))
	}
	opts = append(opts, huh.NewOption("Close PR         — Close this pull request", PRActionClosePR))

	return huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[PRActionMenuChoice]().
				Title(fmt.Sprintf("PR #%d", item.PRNumber)).
				Options(opts...).
				Value(choice),
		),
	).WithTheme(huh.ThemeCharm()).WithWidth(60)
}

// PRActionResultMsg is delivered asynchronously when a PR action IPC call completes.
type PRActionResultMsg struct {
	PRNumber  int
	Action    string
	Err       error
	RequestID string
}

// executePRAction dispatches the selected PR action.
func (m *Model) executePRAction(choice PRActionMenuChoice) tea.Cmd {
	if m.prActionTarget == nil {
		return nil
	}
	item := *m.prActionTarget

	var action string
	switch choice {
	case PRActionOpenBrowser:
		action = "open_browser"
	case PRActionMerge:
		action = "merge"
	case PRActionFixCI:
		action = "quench"
	case PRActionFixComments:
		action = "burnish"
	case PRActionResolveConflicts:
		action = "rebase"
	case PRActionClosePR:
		action = "close"
	case PRActionAssignBellows:
		action = "assign_bellows"
	default:
		return nil
	}

	if m.OnPRAction == nil {
		m.setStatus("PR action unavailable (daemon not connected)", true)
		return nil
	}

	m.setStatus(fmt.Sprintf("PR #%d: %s...", item.PRNumber, action), false)

	return func() tea.Msg {
		reqID, err := m.OnPRAction(item.PRID, item.PRNumber, item.Anvil, item.BeadID, item.Branch, action)
		return PRActionResultMsg{PRNumber: item.PRNumber, Action: action, Err: err, RequestID: reqID}
	}
}

// The choice pointer is bound to the form's value so huh updates it on selection.
func buildQueueActionForm(item *QueueItem, choice *QueueActionMenuChoice) *huh.Form {
	return huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[QueueActionMenuChoice]().
				Title(fmt.Sprintf("Actions for %s", item.BeadID)).
				Options(
					huh.NewOption("Label for dispatch  — Tag bead for auto-dispatch", QueueActionLabel),
					huh.NewOption("Run independently   — Bypass ready check, skip crucible", QueueActionForceRun),
					huh.NewOption("Stop                — Prevent all processing", QueueActionStop),
					huh.NewOption("Close               — Close this bead", QueueActionClose),
				).
				Value(choice),
		),
	).WithTheme(huh.ThemeCharm()).WithWidth(60)
}

// renderQueueActionMenu renders the queue action overlay with a bead info header
// (truncated title and description) followed by the huh form's action select.
func (m *Model) renderQueueActionMenu() string {
	if m.queueActionForm == nil || m.queueActionTarget == nil {
		return ""
	}
	item := m.queueActionTarget
	const maxWidth = 60
	const maxTitleLines = 2
	const maxDescLines = 5

	var sb strings.Builder
	sb.WriteString(actionMenuTitleStyle.Render(fmt.Sprintf("Actions for %s", item.BeadID)))

	if item.Title != "" {
		sb.WriteByte('\n')
		wrapped := wordWrap(sanitizeTitle(item.Title), maxWidth)
		if len(wrapped) > maxTitleLines {
			last := []rune(wrapped[maxTitleLines-1])
			if len(last) > maxWidth-3 {
				last = last[:maxWidth-3]
			}
			wrapped = append(wrapped[:maxTitleLines-1], string(last)+"...")
		}
		for _, line := range wrapped {
			sb.WriteByte('\n')
			sb.WriteString(dimStyle.Render(line))
		}
	}

	if item.Description != "" {
		sb.WriteByte('\n')
		wrapped := wordWrap(sanitizeTitle(item.Description), maxWidth)
		if len(wrapped) > maxDescLines {
			last := []rune(wrapped[maxDescLines-1])
			if len(last) > maxWidth-3 {
				last = last[:maxWidth-3]
			}
			wrapped = append(wrapped[:maxDescLines-1], string(last)+"...")
		}
		for _, line := range wrapped {
			sb.WriteByte('\n')
			sb.WriteString(dimStyle.Render(line))
		}
	}

	sb.WriteString("\n\n")
	sb.WriteString(m.queueActionForm.View())
	sb.WriteString("\n\n" + dimStyle.Render("esc: dismiss"))
	return actionMenuStyle.Render(sb.String())
}

// renderActionMenu renders the Needs Attention action overlay.
func (m *Model) renderActionMenu() string {
	if m.actionForm == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(m.actionForm.View())
	sb.WriteString("\n\n" + dimStyle.Render("esc: dismiss"))
	return actionMenuStyle.Render(sb.String())
}

// renderForceSmithNoteForm renders the note input overlay for Force Smith.
func (m *Model) renderForceSmithNoteForm() string {
	if m.forceSmithNoteForm == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(m.forceSmithNoteForm.View())
	sb.WriteString("\n\n" + dimStyle.Render("enter: submit  esc: cancel"))
	return actionMenuStyle.Render(sb.String())
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
		lines = append(lines, m.logViewerVP.View())
	}

	lines = append(lines, "")
	scrollPct := int(m.logViewerVP.ScrollPercent() * 100)
	lines = append(lines, dimStyle.Render(fmt.Sprintf("j/k/mouse: scroll • Esc: close  %d%%", scrollPct)))

	content := strings.Join(lines, "\n")
	return logViewerStyle.Width(viewerWidth).Height(viewerHeight).Render(content)
}

// notesOverlayTextareaDimensions returns the (width, height) for the notes textarea.
func (m *Model) notesOverlayTextareaDimensions() (int, int) {
	overlayWidth := m.width - 8
	if overlayWidth < 40 {
		overlayWidth = 40
	}
	overlayHeight := m.height / 2
	if overlayHeight < 10 {
		overlayHeight = 10
	}
	taW := overlayWidth - logViewerStyle.GetHorizontalFrameSize()
	// Fixed lines: title + blank + hint = 3
	taH := overlayHeight - logViewerStyle.GetVerticalFrameSize() - 3
	if taW < 1 {
		taW = 1
	}
	if taH < 3 {
		taH = 3
	}
	return taW, taH
}

// openNotesOverlay initialises and displays the inline textarea for beadID.
func (m *Model) openNotesOverlay(beadID, anvil, title string) {
	m.notesTarget = &notesTarget{BeadID: beadID, Anvil: anvil, Title: title}
	ta := textarea.New()
	ta.Placeholder = "Type your notes here…"
	ta.ShowLineNumbers = false
	taW, taH := m.notesOverlayTextareaDimensions()
	ta.SetWidth(taW)
	ta.SetHeight(taH)
	ta.Focus()
	m.notesTA = ta
	m.showNotesOverlay = true
}

// submitNotes saves the textarea content via OnAppendNotes and returns a command.
// The overlay is only cleared in the Update loop upon successful completion.
func (m *Model) submitNotes() tea.Cmd {
	if m.notesTarget == nil {
		m.showNotesOverlay = false
		return nil
	}
	notes := strings.TrimSpace(m.notesTA.Value())
	if notes == "" {
		m.showNotesOverlay = false
		m.notesTarget = nil
		m.notesTA = textarea.Model{}
		return nil
	}
	if m.OnAppendNotes == nil {
		m.setStatus(fmt.Sprintf("Notes unavailable for %s", m.notesTarget.BeadID), true)
		m.showNotesOverlay = false
		m.notesTarget = nil
		m.notesTA = textarea.Model{}
		return nil
	}
	target := m.notesTarget
	cb := m.OnAppendNotes
	// We do NOT clear showNotesOverlay/notesTA here; we wait for success in Update.
	return func() tea.Msg {
		reqID, err := cb(target.BeadID, target.Anvil, notes)
		return NotesResultMsg{BeadID: target.BeadID, Anvil: target.Anvil, Err: err, RequestID: reqID}
	}
}

// renderNotesOverlay renders the inline notes textarea overlay.
func (m *Model) renderNotesOverlay() string {
	if m.notesTarget == nil {
		return ""
	}
	overlayWidth := m.width - 8
	if overlayWidth < 40 {
		overlayWidth = 40
	}
	overlayHeight := m.height / 2
	if overlayHeight < 10 {
		overlayHeight = 10
	}

	contentWidth := overlayWidth - logViewerStyle.GetHorizontalFrameSize()

	titleLine := fmt.Sprintf("Add Notes: %s", m.notesTarget.BeadID)
	if m.notesTarget.Title != "" {
		titleLine += " — " + truncate(m.notesTarget.Title, contentWidth-len(titleLine)-3)
	}

	var lines []string
	lines = append(lines, actionMenuTitleStyle.Render(truncate(titleLine, contentWidth)))
	lines = append(lines, m.notesTA.View())
	lines = append(lines, "")
	lines = append(lines, dimStyle.Render("Ctrl+D: save • Esc: cancel"))

	content := strings.Join(lines, "\n")
	return logViewerStyle.Width(overlayWidth).Height(overlayHeight).Render(content)
}

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

	// For single-anvil setups, append health badge to the panel title.
	if !m.queueGrouped && m.data != nil && len(m.data.AnvilNames) == 1 {
		if h, ok := m.anvilHealth[m.data.AnvilNames[0]]; ok {
			if h.OK {
				title += " " + lipgloss.NewStyle().Foreground(colorSuccess).Render("● "+h.Age)
			} else {
				title += " " + lipgloss.NewStyle().Foreground(colorDanger).Render("⊘ "+h.Age)
			}
		}
	}

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
				// Collapsible anvil header with health badge.
				arrow := "▸"
				if m.queueExpandedAnvils[nav.anvilName] {
					arrow = "▾"
				}
				headerText := fmt.Sprintf("%s %s (%d)", arrow, nav.anvilName, anvilCounts[nav.anvilName])
				// Append health badge if available.
				if h, ok := m.anvilHealth[nav.anvilName]; ok {
					if h.OK {
						headerText += " " + lipgloss.NewStyle().Foreground(colorSuccess).Render("● "+h.Age)
					} else {
						headerText += " " + lipgloss.NewStyle().Foreground(colorDanger).Render("⊘ "+h.Age)
					}
				}
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

			// Main bead line: priority + bead ID, then the PR column, then the
			// anvil suffix on the right. The PR cell is rendered outside the
			// selection highlight because it may carry an OSC 8 hyperlink escape
			// that must not be wrapped by lipgloss styling.
			priority := priorityStyle(item.Priority)
			anvilSuffix := ""
			if !m.queueGrouped {
				anvilSuffix = " " + dimStyle.Render(item.Anvil)
			}
			beadPart := fmt.Sprintf("%s%s %s", indent, priority, item.BeadID)
			if ni == m.queueVP.cursor {
				beadPart = selectedStyle.Render(beadPart)
			}
			prCell := renderQueuePRCell(item.PRNumber, item.PRURL)
			mainLine := beadPart + "  " + prCell + anvilSuffix
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
	// Count panels to render: Queue + ReadyToMerge + NeedsAttention + optionally Crucibles.
	hasCrucibles := len(m.crucibles) > 0

	if hasCrucibles {
		// Total inner height available for all 4 panels is topHeight + bottomHeight - 6.
		// The center column has 1 panel (2 border lines). Each additional panel adds 2 more
		// border lines; 4 panels = 8 border lines total, so subtract 6 extra vs the baseline.
		totalInner := topHeight + bottomHeight - 6
		if totalInner < 0 {
			totalInner = 0
		}
		queueHeight := totalInner * 4 / 10
		crucibleHeight := totalInner * 2 / 10
		mergeHeight := totalInner * 15 / 100
		if totalInner < 12 {
			queueHeight = totalInner
			crucibleHeight = 0
			mergeHeight = 0
		} else {
			queueHeight = max(queueHeight, 5)
			crucibleHeight = max(crucibleHeight, 4)
			mergeHeight = max(mergeHeight, 3)
		}
		attentionHeight := totalInner - queueHeight - crucibleHeight - mergeHeight

		top := m.renderQueue(width, queueHeight)
		cruc := m.renderCrucibles(width, crucibleHeight)
		mid := m.renderReadyToMerge(width, mergeHeight)
		bot := m.renderNeedsAttention(width, attentionHeight)
		return lipgloss.JoinVertical(lipgloss.Left, top, cruc, mid, bot)
	}

	// No active crucibles — original 3-panel layout.
	// Each sub-panel adds 2 border lines. Deduct extra borders beyond one panel.
	innerHeight := topHeight + bottomHeight - 4
	if innerHeight < 0 {
		innerHeight = 0
	}
	queueHeight := innerHeight * 6 / 10
	if innerHeight < 8 {
		queueHeight = innerHeight
	} else {
		queueHeight = max(queueHeight, 5)
	}
	remaining := innerHeight - queueHeight
	mergeHeight := remaining / 3
	if remaining < 6 {
		mergeHeight = 0
	} else {
		mergeHeight = max(mergeHeight, 3)
	}
	attentionHeight := remaining - mergeHeight

	top := m.renderQueue(width, queueHeight)
	middle := m.renderReadyToMerge(width, mergeHeight)
	bottom := m.renderNeedsAttention(width, attentionHeight)
	return lipgloss.JoinVertical(lipgloss.Left, top, middle, bottom)
}

// crucibleProgressColor returns the terminal color code for the bubbles progress
// bar based on crucible phase: green for complete, red for paused (failure), yellow otherwise.
func crucibleProgressColor(phase string) lipgloss.AdaptiveColor {
	switch phase {
	case "complete":
		return colorSuccess // green / success
	case "paused":
		return colorDanger // red / danger
	default:
		return colorWarning // yellow / warning
	}
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
	if height <= 0 {
		return ""
	}
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
			progressLine := fmt.Sprintf("  Children: %d/%d", c.CompletedChildren, c.TotalChildren)
			if c.TotalChildren > 0 {
				barWidth := width - 22
				if barWidth < 5 {
					barWidth = 5
				}
				fraction := float64(c.CompletedChildren) / float64(c.TotalChildren)
				pColor := crucibleProgressColor(c.Phase)
				fillColor := pColor.Dark
				if !lipgloss.HasDarkBackground() {
					fillColor = pColor.Light
				}
				pb := progress.New(
					progress.WithSolidFill(fillColor),
					progress.WithoutPercentage(),
					progress.WithWidth(barWidth),
				)
				progressLine = fmt.Sprintf("  %s %d/%d", pb.ViewAs(fraction), c.CompletedChildren, c.TotalChildren)
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

			lines = append(lines, line1, progressLine, line3)
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
	case AttentionRecoveryExhausted:
		return lipgloss.NewStyle().Foreground(colorDanger).Render("⊘ RECOVERY")
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
	case AttentionAnvilWedged:
		return lipgloss.NewStyle().Foreground(colorDanger).Render("⛔ WEDGED")
	case AttentionSelfDeploy:
		return lipgloss.NewStyle().Foreground(colorDanger).Render("⚙ DEPLOY")
	default:
		return lipgloss.NewStyle().Foreground(colorMuted).Render("⚠ UNKNOWN")
	}
}

// renderNeedsAttention renders the Needs Attention sub-panel showing beads
// that require human intervention (e.g. exhausted dispatch/CI-fix/review-fix/rebase
// attempts, clarification requests, or stalled workers).
func (m *Model) renderNeedsAttention(width, height int) string {
	if height <= 0 {
		return ""
	}
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
			} else if item.Title != "" {
				label += " " + item.Title
			}
			// Anvil-level entries have no bead ID; trim so the row does not
			// start with a stray space.
			label = strings.TrimSpace(label)
			if item.FailureCount > 0 {
				label += dimStyle.Render(fmt.Sprintf(" [%d failures]", item.FailureCount))
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
			autoTag := ""
			if item.AutoMerge {
				autoTag = " [auto]"
			}
			line := fmt.Sprintf("PR #%d %s %s%s", item.PRNumber, item.BeadID, anvil, autoTag)
			if i == m.readyToMergeVP.cursor {
				line = selectedStyle.Render(line)
			}
			lines = append(lines, line)
		}
	}

	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// MergeResultMsg is delivered asynchronously when a merge IPC call completes.
type MergeResultMsg struct {
	PRNumber int
	Err      error
}

// renderWorkers delegates to renderWorkerList for the center column.
func (m *Model) renderWorkers(width, height int) string {
	return m.renderWorkerList(width, height)
}

func (m *Model) renderWorkerList(width, height int) string {
	if height <= 0 {
		return ""
	}
	style := panelStyle.Width(width)
	if m.focused == PanelWorkers {
		style = focusedPanelStyle.Width(width)
	}

	// Render the title WITHOUT MarginBottom. The default panelTitleStyle uses
	// MarginBottom(1), which pads the margin line to the title width. When
	// concatenated with the table, that padding joins with the header's first
	// line, creating an oversized line (title_width + header_width) that wraps.
	workerTitleStyle := panelTitleStyle.MarginBottom(0)
	title := workerTitleStyle.Render(fmt.Sprintf("Workers (%d)", len(m.workers)))

	// panelStyle.Width(w) includes padding but not border.
	// Inner content width = w - border(2). Padding is already inside w.
	innerWidth := width - 2
	if innerWidth < 0 {
		innerWidth = 0
	}
	m.workerTable.SetWidth(innerWidth)
	// table.SetHeight(h) internally sets viewport.Height = h - headerHeight.
	// With a 2-line header (text + bottom border), viewport shows h-2 rows.
	// title(1) + table(h rows via SetHeight) = h+1 content lines; height-4 gives 4 slack lines.
	tableHeight := height - 4
	if tableHeight < 1 {
		tableHeight = 1
	}
	m.workerTable.SetHeight(tableHeight)

	// Dynamically adjust column widths to fit the panel.
	cols := m.workerTable.Columns()
	if len(cols) == 6 {
		avail := innerWidth
		if avail < 1 {
			avail = 1
		}

		// Ratios: Status 3%, Type 10%, Bead 20%, Task 37%, Anvil 20%, Time 10%
		// Cell style has no padding, so column widths include visual spacing.
		cols[0].Width = max(2, avail*3/100)
		cols[1].Width = max(5, avail*10/100)
		cols[2].Width = max(8, avail*20/100)
		cols[3].Width = max(12, avail*37/100)
		cols[4].Width = max(8, avail*20/100)
		cols[5].Width = max(5, avail*10/100)

		// Trim columns in reverse-priority order (least important first) until
		// total <= avail. Each column is only reduced to a floor of 1.
		// This guarantees sum(widths) <= avail for any avail >= 1.
		trimOrder := [6]int{0, 5, 1, 4, 2, 3} // Status, Time, Type, Anvil, Bead, Task
		total := 0
		for _, c := range cols {
			total += c.Width
		}
		for _, idx := range trimOrder {
			if total <= avail {
				break
			}
			excess := total - avail
			reduction := min(excess, cols[idx].Width-1)
			cols[idx].Width -= reduction
			total -= reduction
		}
		// Distribute any remaining slack to the Task column so the
		// table fills the panel exactly — no gap before the right border.
		if slack := avail - total; slack > 0 {
			cols[3].Width += slack
		}

		m.workerTable.SetColumns(cols)
	}

	// Use explicit "\n" instead of relying on panelTitleStyle.MarginBottom.
	// MarginBottom pads the margin line to the title width, and string
	// concatenation joins that padding with the table header's first line,
	// creating an oversized line that wraps inside the panel.
	content := title + "\n" + m.workerTable.View()
	return style.Height(height).Render(content)
}

// renderCenterColumn renders Workers (top) + optional Wicket (middle) + Usage (bottom).
func (m *Model) renderCenterColumn(width, topHeight, bottomHeight int) string {
	fullHeight := topHeight + bottomHeight

	// Usage panel gets a compact fixed height (~9 lines content + 2 border = 11).
	const usagePanelHeight = 11
	if fullHeight < 20 {
		// Terminal too small for a split — render workers only.
		return m.renderWorkerList(width, fullHeight)
	}

	// Show Wicket panel when enabled and there is data to display.
	if m.wicketVisible() {
		// Wicket inner height: 1 header + 1 row per repo (capped at 4 repos).
		numRows := len(m.wicketSummary)
		if numRows > 4 {
			numRows = 4
		}
		wicketInnerH := numRows + 1      // header(1) + data rows
		wicketTotalH := wicketInnerH + 2 // inner + border(2)
		workerHeight := fullHeight - usagePanelHeight - wicketTotalH
		if workerHeight < 5 {
			// Not enough vertical space — fall back to two-panel layout.
			workerHeight = fullHeight - usagePanelHeight
			top := m.renderWorkerList(width, workerHeight)
			bottom := m.renderUsagePanel(width, usagePanelHeight-2)
			return lipgloss.JoinVertical(lipgloss.Left, top, bottom)
		}
		top := m.renderWorkerList(width, workerHeight)
		mid := m.renderWicketPanel(width, wicketInnerH)
		bottom := m.renderUsagePanel(width, usagePanelHeight-2)
		return lipgloss.JoinVertical(lipgloss.Left, top, mid, bottom)
	}

	workerHeight := fullHeight - usagePanelHeight
	top := m.renderWorkerList(width, workerHeight)
	bottom := m.renderUsagePanel(width, usagePanelHeight-2) // -2 for inner content (border adds 2)
	return lipgloss.JoinVertical(lipgloss.Left, top, bottom)
}

// wicketVisible reports whether the Wicket panel should be shown and is navigable.
func (m *Model) wicketVisible() bool {
	return m.data != nil && m.data.WicketEnabled && len(m.wicketSummary) > 0
}

// renderWicketPanel renders a compact summary of GitHub issues monitored by Wicket.
// Each row shows the repo short name, open issue count, and needs-human count.
func (m *Model) renderWicketPanel(width, height int) string {
	style := panelStyle.Width(width)
	if m.focused == PanelWicket {
		style = focusedPanelStyle.Width(width)
	}

	title := panelTitleStyle.Render("Wicket")

	var lines []string
	lines = append(lines, title)

	// Inner content width available for data rows derived from the style's actual frame size.
	innerWidth := width - panelStyle.GetHorizontalFrameSize()
	if innerWidth < 1 {
		innerWidth = 1
	}

	// Warn style for non-zero needs-human counts.
	warnStyle := lipgloss.NewStyle().Foreground(colorWarning).Bold(true)

	// Display up to 4 repos; skip repos that somehow have 0 open issues.
	shown := 0
	for _, s := range m.wicketSummary {
		if shown >= 4 {
			break
		}
		// Derive short display name from "owner/repo" → "repo".
		displayName := s.Repo
		if idx := strings.LastIndex(s.Repo, "/"); idx >= 0 {
			displayName = s.Repo[idx+1:]
		}

		// Format the ⚠ count, highlighted when non-zero.
		warnStr := "0⚠"
		if s.NeedsHumanCount > 0 {
			warnStr = warnStyle.Render(fmt.Sprintf("%d⚠", s.NeedsHumanCount))
		} else {
			warnStr = dimStyle.Render(warnStr)
		}

		openStr := fmt.Sprintf("%d open", s.OpenCount)

		// Build the row: name padded to 12 runes, then counts.
		// Use rune slicing to correctly handle multi-byte Unicode characters.
		nameRunes := []rune(displayName)
		if len(nameRunes) > 12 {
			nameRunes = nameRunes[:12]
		}
		for len(nameRunes) < 12 {
			nameRunes = append(nameRunes, ' ')
		}
		nameField := string(nameRunes)
		row := nameField + " " + openStr + "  " + warnStr

		// Truncate to available width using rune counts (not byte lengths).
		plainRow := nameField + " " + openStr + "  " + fmt.Sprintf("%d⚠", s.NeedsHumanCount)
		plainRunes := []rune(plainRow)
		if len(plainRunes) > innerWidth {
			// Trim displayName runes to fit.
			excess := len(plainRunes) - innerWidth
			if excess < len(nameRunes) {
				nameField = string(nameRunes[:len(nameRunes)-excess])
				row = nameField + " " + openStr + "  " + warnStr
			}
		}

		lines = append(lines, row)
		shown++
	}

	if height <= 0 {
		return style.Render("")
	}
	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
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

	// Ingot status counts
	if m.ingotTotal > 0 {
		lines = append(lines, m.renderIngotCountsLine())
	}

	if height <= 0 {
		return style.Render("")
	}
	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// renderIngotCountsLine returns a compact summary of ingot status counts.
// Example: "Ingots   3 smith │ 1 temper │ 2 pr_open │ 45 merged"
func (m *Model) renderIngotCountsLine() string {
	// Display order: active pipeline stages first, then terminal states.
	type statusEntry struct {
		status ingot.Status
		label  string
	}
	order := []statusEntry{
		{ingot.StatusSmith, "smith"},
		{ingot.StatusTemper, "temper"},
		{ingot.StatusWarden, "warden"},
		{ingot.StatusApproved, "approved"},
		{ingot.StatusPROpen, "pr_open"},
		{ingot.StatusPRMerged, "merged"},
		{ingot.StatusFailed, "failed"},
		{ingot.StatusStalled, "stalled"},
		{ingot.StatusInit, "init"},
	}

	// Track which statuses are handled by the ordered list.
	known := make(map[ingot.Status]struct{}, len(order))
	for _, e := range order {
		known[e.status] = struct{}{}
	}

	var parts []string
	for _, e := range order {
		if c, ok := m.ingotCounts[e.status]; ok && c > 0 {
			part := fmt.Sprintf("%d %s", c, e.label)
			// Highlight failure states
			if e.status == ingot.StatusFailed || e.status == ingot.StatusStalled {
				part = lipgloss.NewStyle().Foreground(colorDanger).Render(part)
			}
			parts = append(parts, part)
		}
	}

	// Append any statuses not in the known ordered list so the breakdown
	// always accounts for every status in the total (prevents misleading totals).
	for status, c := range m.ingotCounts {
		if _, handled := known[status]; !handled && c > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c, string(status)))
		}
	}

	if len(parts) == 0 {
		return dimStyle.Render(fmt.Sprintf("Ingots   %d total", m.ingotTotal))
	}
	return fmt.Sprintf("Ingots   %s  (%d total)", strings.Join(parts, " │ "), m.ingotTotal)
}

// renderWorkerActivity renders the activity panel: a live log view for the
// currently selected worker, parsed from its stream-json log file.
// Activity flows as continuous lines with blank-line separators between
// logical blocks, similar to Claude CLI terminal output. The newest
// activity appears at the top.
func (m *Model) renderWorkerActivity(width, height int) string {
	style := panelStyle.Width(width)
	if m.focused == PanelLiveActivity {
		style = focusedPanelStyle.Width(width)
	}

	// Build title with selected worker info
	titleText := "Live Activity"
	if len(m.workers) > 0 && m.workerTable.Cursor() < len(m.workers) {
		w := m.workers[m.workerTable.Cursor()]
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
		contentWidth := width - 2
		if contentWidth < 10 {
			contentWidth = 10
		}

		// Flatten all nav items into rendered lines so the viewport operates on
		// rendered line indices rather than item indices. This ensures wrapped
		// entries are fully scrollable without truncation.
		var flatLines []string
		for _, nav := range m.activityNavItems {
			if nav.text == "" {
				flatLines = append(flatLines, "")
				continue
			}
			isThinking := nav.lineType == "think"
			isTool := nav.lineType == "tool" || nav.lineType == "rate"
			for _, wl := range wordWrap(nav.text, contentWidth) {
				flatLines = append(flatLines, applyMarkdownLite(wl, isThinking, isTool))
			}
		}

		total := len(flatLines)
		m.activityLineCount = total
		m.activityVP.ClampToTotal(total)
		m.activityVP.AdjustViewport(maxVisible, total)
		start, end := m.activityVP.VisibleRange(maxVisible, total)

		for i := start; i < end; i++ {
			styled := flatLines[i]
			if m.focused == PanelLiveActivity && i == m.activityVP.cursor {
				styled = selectedStyle.Render(styled)
			}
			lines = append(lines, styled)
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

	display := m.displayEvents()

	scrollIndicator := ""
	if !m.eventAutoScroll {
		scrollIndicator = dimStyle.Render(" ⏸")
	}

	// Show "filtered/total" when a filter is active, otherwise just total
	var countLabel string
	if m.eventFilterText != "" {
		countLabel = fmt.Sprintf("%d/%d", len(display), len(m.events))
	} else {
		countLabel = fmt.Sprintf("%d", len(m.events))
	}
	title := panelTitleStyle.Render(fmt.Sprintf("Events (%s)%s", countLabel, scrollIndicator))

	var lines []string
	lines = append(lines, title)

	// Show filter input or active filter indicator
	if m.eventFilterActive {
		lines = append(lines, m.eventFilter.View())
	} else if m.eventFilterText != "" {
		lines = append(lines, dimStyle.Render(fmt.Sprintf("🔍 %s  (/ edit, Esc clear)", m.eventFilterText)))
	}

	contentHeight := height - 3 // title + border rows
	if m.eventFilterActive || m.eventFilterText != "" {
		contentHeight-- // account for the filter line
	}

	if len(display) == 0 {
		if m.eventFilterText != "" {
			lines = append(lines, dimStyle.Render("No matching events"))
		} else {
			lines = append(lines, dimStyle.Render("No events"))
		}
	} else {
		allLines := m.renderAllEventLines(width)
		visible := visibleItems(m.eventScroll, len(allLines), contentHeight)
		for i := visible.start; i < visible.end; i++ {
			lines = append(lines, allLines[i])
		}
	}

	if height <= 0 {
		return ""
	}
	content := strings.Join(lines, "\n")
	return style.Height(height).Render(content)
}

// renderAllEventLines flattens all events into a single slice of rendered lines.
// It uses eventLineCount for the selection-mapping pass to avoid a double full render.
// Caches the results to avoid redundant work.
func (m *Model) renderAllEventLines(width int) []string {
	display := m.displayEvents()

	// Calculate what the selected event index WOULD be.
	selectedEventIdx := -1
	cumulative := 0
	for i, event := range display {
		count := m.eventLineCount(event, width)
		if selectedEventIdx == -1 && cumulative+count > m.eventScroll {
			selectedEventIdx = i
		}
		cumulative += count
	}

	// Check if cache is valid.
	if m.eventWidthCache == width &&
		m.eventCountCache == len(display) &&
		m.eventSelectedIdxCache == selectedEventIdx &&
		m.eventRevisionCache == m.eventRevision &&
		m.eventLinesCache != nil {
		return m.eventLinesCache
	}

	// Cache invalid, perform full render.
	var allLines []string
	for i, event := range display {
		allLines = append(allLines, m.renderEventLines(event, i == selectedEventIdx, width)...)
	}

	// Update cache
	m.eventLinesCache = allLines
	m.eventWidthCache = width
	m.eventCountCache = len(display)
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
	display := m.displayEvents()
	if m.eventWidthCache == width && m.eventCountCache == len(display) && m.eventRevisionCache == m.eventRevision && m.eventLinesCache != nil {
		return len(m.eventLinesCache)
	}

	total := 0
	for _, event := range display {
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

// applyEventFilter updates the filtered event view based on m.eventFilterText.
// Called whenever events or the filter text change.
//
// When a filter is active, the search runs at the DB level so the entire event
// log is queried — not just the most recent EventFetchLimit events held in
// m.events. It returns the command that performs that query; the result arrives
// asynchronously as an UpdateFilteredEventsMsg. When the filter is empty (or no
// data source is available, e.g. display-only mode), it falls back to filtering
// the in-memory slice synchronously and returns nil.
func (m *Model) applyEventFilter() tea.Cmd {
	query := strings.ToLower(m.eventFilterText)
	if query == "" {
		m.filteredEvents = m.events
		return nil
	}
	// Prefer a DB-backed search so events older than the loaded window match.
	if m.data != nil && m.data.DB != nil {
		m.filteredEvents = nil
		return FetchEventsMatching(m.data.DB, m.eventFilterText)
	}
	// Fallback: filter the in-memory slice (display-only mode).
	m.filteredEvents = m.filterEventsInMemory(query)
	return nil
}

// filterEventsInMemory returns the subset of m.events matching the given
// lower-cased query. Used as a fallback when no DB is available.
func (m *Model) filterEventsInMemory(query string) []EventItem {
	filtered := make([]EventItem, 0, len(m.events))
	for _, ev := range m.events {
		line := strings.ToLower(ev.Timestamp + " " + ev.Type + " " + ev.BeadID + " " + ev.Message)
		if strings.Contains(line, query) {
			filtered = append(filtered, ev)
		}
	}
	return filtered
}

// displayEvents returns the events to render — filtered if a filter is active,
// otherwise all events.
func (m *Model) displayEvents() []EventItem {
	if m.eventFilterText != "" {
		return m.filteredEvents
	}
	return m.events
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

// UpdateFilteredEventsMsg delivers the result of a DB-backed event filter
// search. Filter echoes the query the search was issued for, so the model can
// ignore results that no longer match the current filter text (the user may
// have kept typing while the query was in flight).
type UpdateFilteredEventsMsg struct {
	Filter string
	Items  []EventItem
}

// AnvilHealth holds per-anvil poll health status for the Queue panel.
type AnvilHealth struct {
	Anvil     string
	OK        bool   // true = last poll succeeded
	Message   string // e.g. "5 ready" or error text
	Timestamp string // "15:04:05" format
	Age       string // e.g. "2m", "1h"
}

// UpdateAnvilHealthMsg delivers per-anvil health data.
type UpdateAnvilHealthMsg struct{ Items []AnvilHealth }

// UpdateWicketSummaryMsg delivers per-repo Wicket issue counts.
type UpdateWicketSummaryMsg struct{ Items []WicketRepoSummary }

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

// workerStatusIndicator returns a plain-text status indicator.
// The Bubbles table internally calls runewidth.Truncate on cell values which
// does not handle ANSI escape sequences — it counts escape bytes as visible
// characters, corrupting styled strings and breaking row alignment.
// Return plain characters only; color is not possible per-cell in this table.
func workerStatusIndicator(status, frame string) string {
	switch status {
	case "running":
		return frame
	case "reviewing":
		return frame
	case "monitoring":
		return "○"
	case "done":
		return "✓"
	case "partial":
		// Half-filled: an Assay run some of whose passes never reviewed the
		// head — not a tick, not a cross.
		return "◐"
	case "failed":
		return "✗"
	default:
		return "○"
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
	case "quench":
		return lipgloss.NewStyle().Foreground(colorDanger).Render("🔧")
	case "burnish":
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
	case "quench":
		return lipgloss.NewStyle().Foreground(colorDanger).Render("[quench]")
	case "burnish":
		return lipgloss.NewStyle().Foreground(colorPink).Render("[burnish]")
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

// renderOSC8Link wraps text in an OSC 8 terminal hyperlink escape sequence,
// making the text clickable in supporting terminals (e.g. Windows Terminal,
// iTerm2). When url is empty the text is returned unchanged.
//
// To avoid terminal escape injection, the OSC 8 sequence is only emitted when
// both url and text contain no ASCII control characters (e.g. ESC, BEL).
func renderOSC8Link(url, text string) string {
	if url == "" {
		return text
	}
	if !isSafeForOSC8(url) || !isSafeForOSC8(text) {
		return text
	}
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// isSafeForOSC8 reports whether s can be safely embedded into an OSC 8
// hyperlink sequence. It rejects ASCII control characters (0x00-0x1F, 0x7F),
// including ESC and BEL, which could otherwise be used for terminal escape
// injection.
func isSafeForOSC8(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b < 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

// prColumnWidth is the fixed display width reserved for the queue PR column
// (e.g. "PR #1234"). It keeps the bead and PR columns aligned across rows
// whether or not a given bead has a PR yet.
const prColumnWidth = 8

// renderQueuePRCell returns the styled, fixed-width PR cell for a queue row.
// When the bead has a PR it shows "PR #N" as a clickable OSC 8 hyperlink (when
// a URL is known); otherwise it returns blank padding of the same width so the
// surrounding columns stay aligned. The visible text is padded to
// prColumnWidth before the hyperlink escape is applied, so the OSC 8 control
// bytes do not count toward the column width.
func renderQueuePRCell(prNumber int, prURL string) string {
	if prNumber <= 0 {
		return strings.Repeat(" ", prColumnWidth)
	}
	label := fmt.Sprintf("PR #%d", prNumber)
	if w := lipgloss.Width(label); w < prColumnWidth {
		label += strings.Repeat(" ", prColumnWidth-w)
	}
	linked := renderOSC8Link(prURL, label)
	return lipgloss.NewStyle().Foreground(colorInfo).Render(linked)
}

// eventTypeStyle returns a styled event type.
func eventTypeStyle(t string) string {
	return lipgloss.NewStyle().Foreground(eventTypeColor(t)).Render(t)
}

// eventTypeColor maps an event type onto its feed colour. It is split out of
// eventTypeStyle so the mapping can be asserted directly: the rendered string
// carries no escape codes under a test terminal profile, so a test of the
// styled output would pass whatever colour it chose.
func eventTypeColor(t string) lipgloss.AdaptiveColor {
	switch {
	// Partial before the success and failure matches: assay_partial is neither
	// — some passes reviewed the head and some never did — and the substring
	// heuristics below would otherwise leave it in the neutral default, reading
	// exactly like an informational row.
	case strings.Contains(t, "partial"):
		return colorWarning
	// "complete" rather than "completed" so crucible_complete lands here too:
	// toast.go already classifies it as a success, and the two surfaces
	// colouring the same event differently is the drift worth closing. No other
	// event type in state/db.go contains the substring.
	case strings.Contains(t, "pass") || strings.Contains(t, "done") ||
		strings.Contains(t, "merged") || strings.Contains(t, "complete"):
		return colorSuccess
	case strings.Contains(t, "fail") || strings.Contains(t, "reject") || strings.Contains(t, "error"):
		return colorDanger
	default:
		return colorInfo
	}
}

// sanitizeTitle removes ANSI escape sequences, replaces newlines/carriage
// returns/tabs with spaces, and strips non-printable runes from a bead title
// before rendering it in the TUI.
//
// It delegates to internal/termtext, which is also what Assay's terminal event
// messages go through. The hand-rolled loop this replaced recognised only CSI
// sequences, so an OSC title write (ESC ] 0 ; … BEL) in a bead title lost its
// ESC to the control-character branch and left the rest as visible residue —
// two strippers with different coverage over equally untrusted text.
func sanitizeTitle(s string) string {
	return termtext.Line(s)
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

// placeOverlayAt composites overlayLines onto bgLines at position (startX, startY).
// It is the shared ANSI-safe implementation used by placeOverlay and placeToastsOverlay,
// preventing the two callers from drifting apart over time.
func placeOverlayAt(startX, startY, overlayWidth int, overlayLines, bgLines []string) {
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

	placeOverlayAt(startX, startY, overlayWidth, overlayLines, bgLines)
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

// stripActivityPrefix removes type-tag prefixes from an activity line,
// returning the bare content. Tags like [tool], [text], [think], [rate] are
// stripped; visual distinction is provided by colour in the render layer.
func stripActivityPrefix(line string) string {
	for _, pfx := range []string{"[text] ", "[think] ", "[tool] ", "[rate] "} {
		if strings.HasPrefix(line, pfx) {
			return line[len(pfx):]
		}
	}
	return line
}

// rebuildActivityNav rebuilds the flat display items for the Live Activity panel
// from the currently selected worker's activity. Lines flow oldest-first so
// the newest output is always at the bottom, matching terminal behaviour.
func (m *Model) rebuildActivityNav() {
	rawLines := m.selectedWorkerActivity()

	// Remember whether we were following the bottom before the rebuild so we
	// can re-pin the cursor after new lines are appended.
	prevNavCount := len(m.activityNavItems)
	atBottom := prevNavCount == 0 || m.activityVP.cursor >= prevNavCount-3

	m.activityNavItems = nil
	// Walk lines oldest-first (index 0 → end). Continuation lines (no type
	// tag) inherit the type of the most-recent typed line above them.
	var prevType string
	var lastTypedType string
	for _, line := range rawLines {
		typ := activityLineType(line)
		if typ == "" {
			// Continuation — inherit from the last explicitly-typed line.
			typ = lastTypedType
			if typ == "" {
				typ = "text"
			}
		} else {
			lastTypedType = typ
		}

		// Blank separator: on type transitions AND between every tool call so
		// consecutive tool lines are not visually bundled.
		if prevType != "" && (typ != prevType || typ == "tool") {
			m.activityNavItems = append(m.activityNavItems, activityNavItem{text: ""})
		}
		prevType = typ

		m.activityNavItems = append(m.activityNavItems, activityNavItem{
			lineType: typ,
			text:     stripActivityPrefix(line),
		})
	}

	// Clamp using rendered line count (line-based scrolling); fall back to nav
	// item count as a conservative lower bound before the first render.
	lineCount := m.activityLineCount
	if lineCount < len(m.activityNavItems) {
		lineCount = len(m.activityNavItems)
	}
	m.activityVP.ClampToTotal(lineCount)

	// Auto-follow: if cursor was at or near the bottom (or this is the first
	// load), pin it to the new end so the latest output stays visible.
	if atBottom && lineCount > 0 {
		m.activityVP.cursor = lineCount - 1
	}
}

// resetActivityState clears the activity viewport, used when the selected
// worker changes.
func (m *Model) resetActivityState() {
	m.activityVP = scrollViewport{}
	m.rebuildActivityNav()
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

// Precompiled patterns for markdown-lite inline styling.
var (
	reBold = regexp.MustCompile(`(?U)\*\*(.+)\*\*`)
	reCode = regexp.MustCompile("`([^`]+)`")
)

// applyMarkdownLite applies lightweight inline styling to a plain text line:
//   - **bold** → lipgloss bold
//   - `code`  → dimmed code style
//
// Thinking lines are rendered in dimStyle; tool/rate lines in a muted blue
// so they stand out from prose without needing a "[tool]" label.
func applyMarkdownLite(line string, isThinking, isTool bool) string {
	// Replace **bold** spans with lipgloss bold rendering.
	line = reBold.ReplaceAllStringFunc(line, func(m string) string {
		inner := m[2 : len(m)-2]
		return lipgloss.NewStyle().Bold(true).Render(inner)
	})
	// Replace `code` spans with a muted/dim style.
	line = reCode.ReplaceAllStringFunc(line, func(m string) string {
		inner := m[1 : len(m)-1]
		return lipgloss.NewStyle().Foreground(colorMuted).Render(inner)
	})
	switch {
	case isThinking:
		// Wrap the entire line in dim styling. Because the bold/code spans
		// already injected ANSI sequences the dim applies to the surrounding
		// text while nested sequences override it where appropriate.
		line = dimStyle.Render(line)
	case isTool:
		// Render tool calls in a muted blue — visually distinct from prose
		// without needing a "[tool]" label prefix.
		line = lipgloss.NewStyle().Foreground(colorInfo).Render(line)
	}
	return line
}

// driveHuhForm is a helper that processes a message against a huh form and
// updates the form pointer with the result. It returns any command produced
// by the form's Update so Bubble Tea can execute it asynchronously.
// It synchronously drives simple internal transition commands (like nextFieldMsg)
// to help the form reach its next state in a single turn where possible.
func (m *Model) driveHuhForm(form **huh.Form, msg tea.Msg) tea.Cmd {
	f, cmd := (*form).Update(msg)
	if f != nil {
		if hf, ok := f.(*huh.Form); ok {
			*form = hf
		}
	}

	return m.driveHuhSync(form, cmd)
}

// driveHuhSync recursively drives internal commands produced by a huh form.
// It expands batches and stops synchronously driving once it hits a non-huh
// or non-bubbletea command, or when the form is no longer in StateNormal.
func (m *Model) driveHuhSync(form **huh.Form, cmd tea.Cmd) tea.Cmd {
	if cmd == nil || (*form).State != huh.StateNormal {
		return cmd
	}

	var pendingCmds []tea.Cmd
	pendingCmds = append(pendingCmds, cmd) // Start with the initial command

	var externalCmds []tea.Cmd // Commands that are not internal to huh/bubbletea

	for len(pendingCmds) > 0 {
		currentCmd := pendingCmds[0]
		pendingCmds = pendingCmds[1:]

		if currentCmd == nil {
			continue
		}

		// Execute the command with a short timeout. Commands like cursor-blink
		// ticks block for hundreds of milliseconds — if a command doesn't return
		// immediately, treat it as external so Bubbletea handles it async.
		var nextMsg tea.Msg
		done := make(chan tea.Msg, 1)
		go func(c tea.Cmd) { done <- c() }(currentCmd)
		select {
		case nextMsg = <-done:
		case <-time.After(2 * time.Millisecond):
			externalCmds = append(externalCmds, currentCmd)
			continue
		}
		if nextMsg == nil {
			continue
		}

		// If the command produced a batch, add its components to the pending queue
		if batch, ok := nextMsg.(tea.BatchMsg); ok {
			for _, bc := range batch {
				if bc != nil {
					pendingCmds = append(pendingCmds, bc)
				}
			}
			continue
		}

		// If it's not a batch, it's a single message.
		// Determine if it's an internal huh/bubbletea message or an external one.
		typ := reflect.TypeOf(nextMsg)
		pkg := typ.PkgPath()
		if !strings.Contains(pkg, "charmbracelet/huh") && !strings.Contains(pkg, "charmbracelet/bubbletea") {
			msg := nextMsg
			externalCmds = append(externalCmds, func() tea.Msg { return msg })
			continue
		}

		// It's an internal message, feed it back into the form.
		f, newCmd := (*form).Update(nextMsg)
		if f != nil {
			if hf, ok := f.(*huh.Form); ok {
				*form = hf
			}
		}
		// If the update generated a new command, add it to the pending queue for synchronous processing.
		if newCmd != nil {
			pendingCmds = append(pendingCmds, newCmd)
		}

		// If the form transitioned out of StateNormal, stop synchronous processing.
		if (*form).State != huh.StateNormal {
			break
		}
	}

	if len(externalCmds) > 0 {
		return tea.Batch(externalCmds...)
	}
	return nil
}

// isTerminalMsg returns true if the message is a user input event (key or mouse).
func isTerminalMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case tea.KeyMsg, tea.MouseMsg:
		return true
	}
	return false
}

// startAsyncSubscription returns a Cmd that connects to the daemon's event
// stream. On success it delivers an asyncSubscriptionStartedMsg. On a failed
// connect it delivers a streamClosedMsg so the reconnect loop keeps retrying
// (and the tick's poll fallback stays active) instead of giving up forever.
func startAsyncSubscription() tea.Cmd {
	return func() tea.Msg {
		client, err := ipc.NewClient()
		if err != nil {
			return streamClosedMsg{}
		}
		ctx, cancel := context.WithCancel(context.Background())
		events := client.Subscribe(ctx)
		return asyncSubscriptionStartedMsg{events: events, cancel: cancel, client: client}
	}
}

// waitForStreamEvent blocks until the next relevant event arrives on the IPC
// subscribe channel and translates it into a Bubbletea message: async_complete
// → AsyncCompletionMsg, event_logged → LoggedEventMsg, events_gap →
// EventsGapMsg. Unknown event types are skipped. Returns a streamClosedMsg when
// the channel closes (subscription torn down) so the model can drop the
// streaming latch and reconnect. Each handler re-arms the wait so the stream is
// drained continuously.
func waitForStreamEvent(events <-chan ipc.Event) tea.Cmd {
	return func() tea.Msg {
		for evt := range events {
			switch evt.Type {
			case "async_complete":
				var data struct {
					RequestID string `json:"request_id"`
					Type      string `json:"type"`
					Response  struct {
						Type    string          `json:"type"`
						Payload json.RawMessage `json:"payload"`
					} `json:"response"`
				}
				if json.Unmarshal(evt.Data, &data) != nil {
					continue
				}
				msg := AsyncCompletionMsg{
					RequestID: data.RequestID,
					Success:   data.Response.Type == "ok",
				}
				var payload struct{ Message string }
				if json.Unmarshal(data.Response.Payload, &payload) == nil && payload.Message != "" {
					msg.Message = payload.Message
				}
				return msg
			case "event_logged":
				var info ipc.EventInfo
				if json.Unmarshal(evt.Data, &info) != nil {
					continue
				}
				return LoggedEventMsg{Event: info}
			case "events_gap":
				return EventsGapMsg{}
			default:
				continue
			}
		}
		return streamClosedMsg{}
	}
}

// trackPending registers an async operation for status-line correlation.
func (m *Model) trackPending(requestID, description string) {
	if requestID == "" {
		return
	}
	if m.pendingAsync == nil {
		m.pendingAsync = make(map[string]pendingOp)
	}
	m.pendingAsync[requestID] = pendingOp{
		Description: description,
		StartedAt:   time.Now(),
	}
}

const asyncTimeout = 60 * time.Second

// subscribeReconnectDelay is the pause before retrying the IPC subscribe stream
// after it drops, so a briefly-unavailable daemon (e.g. a restart) is not
// hammered with connect attempts. While disconnected the tick's poll fallback
// keeps the event feed live.
const subscribeReconnectDelay = 3 * time.Second

// scheduleSubscriptionReconnect returns a Cmd that emits a
// reconnectSubscriptionMsg after subscribeReconnectDelay.
func scheduleSubscriptionReconnect() tea.Cmd {
	return tea.Tick(subscribeReconnectDelay, func(time.Time) tea.Msg {
		return reconnectSubscriptionMsg{}
	})
}

// expirePendingOps removes stale pending operations older than asyncTimeout.
func (m *Model) expirePendingOps() {
	for id, op := range m.pendingAsync {
		if time.Since(op.StartedAt) > asyncTimeout {
			m.setStatus(fmt.Sprintf("%s: timed out", op.Description), true)
			delete(m.pendingAsync, id)
		}
	}
}

// cleanupAsyncSubscription cancels the event subscription if active.
func (m *Model) cleanupAsyncSubscription() {
	if m.asyncCancelFunc != nil {
		m.asyncCancelFunc()
		m.asyncCancelFunc = nil
	}
	if m.asyncClient != nil {
		m.asyncClient.Close()
		m.asyncClient = nil
	}
}
