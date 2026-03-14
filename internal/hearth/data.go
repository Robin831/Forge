package hearth

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

// classifyAttentionReason determines the AttentionReason category from a
// NeedsAttentionBead's fields. The classification is ordered by specificity:
// clarification flag, circuit breaker prefix, reason text patterns, stalled.
func classifyAttentionReason(b state.NeedsAttentionBead) AttentionReason {
	if b.ClarificationNeeded {
		return AttentionClarification
	}
	reason := strings.ToLower(b.Reason)
	if strings.HasPrefix(reason, "circuit breaker:") {
		return AttentionDispatchExhausted
	}
	if strings.Contains(reason, "ci fix exhausted") {
		return AttentionCIFixExhausted
	}
	if strings.Contains(reason, "review fix exhausted") {
		return AttentionReviewFixExhausted
	}
	if strings.Contains(reason, "rebase exhausted") {
		return AttentionRebaseExhausted
	}
	if strings.Contains(reason, "stalled") {
		return AttentionStalled
	}
	if b.NeedsHuman {
		return AttentionDispatchExhausted
	}
	return AttentionUnknown
}

// TickInterval is how often the TUI refreshes data.
const TickInterval = 2 * time.Second

// healthTickDivisor controls how often the daemon health IPC check runs relative
// to the main tick. At TickInterval=2s and divisor=5, health is checked every 10s.
const healthTickDivisor = 5

// EventFetchLimit is the maximum number of events retrieved for the Events panel.
const EventFetchLimit = 100

// TickMsg triggers a data refresh cycle.
type TickMsg time.Time

// SpinnerInterval is how often the spinner animation advances.
const SpinnerInterval = 100 * time.Millisecond

// SpinnerTickMsg advances the spinner animation frame.
type SpinnerTickMsg time.Time

// SpinnerFrames are the animation frames for active workers (braille dots pattern).
var SpinnerFrames = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}

// SpinnerTick returns a Bubbletea command that sends a SpinnerTickMsg after SpinnerInterval.
func SpinnerTick() tea.Cmd {
	return tea.Tick(SpinnerInterval, func(t time.Time) tea.Msg {
		return SpinnerTickMsg(t)
	})
}

// DataSource holds the dependencies needed to feed the TUI panels.
type DataSource struct {
	DB *state.DB
	// Exhaustion thresholds from config. Zero values fall back to state package defaults.
	MaxCIFixAttempts     int
	MaxReviewFixAttempts int
	MaxRebaseAttempts    int
	// AnvilNames lists all registered anvil names (sorted) so the Queue panel
	// can show empty anvils with a (0) count.
	AnvilNames []string
	// Cost limits from config for the Usage panel display.
	DailyCostLimit           float64
	CopilotDailyRequestLimit int
	// AutoMergeAnvils is the set of anvil names that have auto_merge enabled.
	AutoMergeAnvils map[string]bool
}

// Tick returns a Bubbletea command that sends a TickMsg after the interval.
func Tick() tea.Cmd {
	return tea.Tick(TickInterval, func(t time.Time) tea.Msg {
		return TickMsg(t)
	})
}

// FetchQueue reads the daemon's cached queue from the state DB.
// The daemon writes queue data on each poll cycle, so the Hearth TUI
// always reflects the daemon's view without running its own bd ready calls.
func FetchQueue(db *state.DB) tea.Cmd {
	return func() tea.Msg {
		cached, err := db.QueueCache()
		if err != nil {
			return QueueErrorMsg{Err: err}
		}

		var items []QueueItem
		for _, c := range cached {
			items = append(items, QueueItem{
				BeadID:      c.BeadID,
				Title:       c.Title,
				Description: c.Description,
				Anvil:       c.Anvil,
				Priority:    c.Priority,
				Status:      c.Status,
				Section:     string(c.Section),
				Assignee:    c.Assignee,
			})
		}

		return UpdateQueueMsg{Items: items}
	}
}

// FetchWorkers reads active workers from the state DB and enriches with
// last log line from the worker log file.
func FetchWorkers(db *state.DB) tea.Cmd {
	return func() tea.Msg {
		workers, err := db.ActiveWorkers()
		if err != nil {
			return UpdateWorkersMsg{Items: nil}
		}

		var items []WorkerItem
		for _, w := range workers {
			duration := ""
			if !w.StartedAt.IsZero() {
				duration = time.Since(w.StartedAt).Truncate(time.Second).String()
			}

			// Use explicit phase if set, otherwise infer from ID prefix or status
			wType := w.Phase
			if wType == "" {
				wType = inferWorkerType(w.ID, w.Status)
			}

			// Read last log line
			lastLog := readLastLogLine(w.LogPath)

			activityLines := parseWorkerActivity(w.LogPath, 100)

			items = append(items, WorkerItem{
				ID:            w.ID,
				BeadID:        w.BeadID,
				Title:         w.Title,
				Anvil:         w.Anvil,
				Status:        string(w.Status),
				Duration:      duration,
				Type:          wType,
				PRNumber:      w.PRNumber,
				LastLog:       lastLog,
				PID:           w.PID,
				LogPath:       w.LogPath,
				ActivityLines: activityLines,
			})
		}

		return UpdateWorkersMsg{Items: items}
	}
}

// inferWorkerType guesses the worker type from its ID or status.
func inferWorkerType(id string, status state.WorkerStatus) string {
	// Convention: worker IDs are prefixed with type
	switch {
	case len(id) > 6 && id[:6] == "smith-":
		return "smith"
	case len(id) > 7 && id[:7] == "warden-":
		return "warden"
	case len(id) > 7 && id[:7] == "temper-":
		return "temper"
	case len(id) > 6 && id[:6] == "cifix-":
		return "quench"
	case len(id) > 10 && id[:10] == "reviewfix-":
		return "burnish"
	case len(id) > 7 && id[:7] == "rebase-":
		return "rebase"
	}
	// Fall back to status-based guess
	if status == state.WorkerReviewing {
		return "warden"
	}
	return "smith"
}

// readLastLogLine reads the last non-empty line from a log file.
func readLastLogLine(logPath string) string {
	if logPath == "" {
		return ""
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		return ""
	}
	// Return last non-empty line
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}

// parseWorkerActivity reads the last maxEntries activity events from a
// stream-json log file (as written by the smith package) and returns
// human-readable lines suitable for the Live Activity sub-panel.
func parseWorkerActivity(logPath string, maxEntries int) []string {
	if logPath == "" || maxEntries <= 0 {
		return nil
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}

	rawLines := strings.Split(string(data), "\n")

	var entries []string
	// For Gemini delta messages, accumulate fragments into a single entry
	// rather than creating one [text] entry per tiny delta.
	var geminiTextBuf strings.Builder

	flushGeminiText := func() {
		if geminiTextBuf.Len() == 0 {
			return
		}
		raw := geminiTextBuf.String()
		geminiTextBuf.Reset()
		entries = append(entries, formatMultiLineEntry("[text] ", "       ", raw, 3)...)
	}

	for _, line := range rawLines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var event struct {
			Type    string          `json:"type"`
			Subtype string          `json:"subtype,omitempty"`
			Message json.RawMessage `json:"message,omitempty"`
			Content string          `json:"content,omitempty"`
			Role    string          `json:"role,omitempty"`
			Status  string          `json:"status,omitempty"`
			// Gemini top-level tool_use fields
			ToolName      string          `json:"tool_name,omitempty"`
			Parameters    json.RawMessage `json:"parameters,omitempty"`
			RateLimitInfo *struct {
				Status string `json:"status"`
			} `json:"rate_limit_info,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		switch event.Type {
		case "assistant":
			if len(event.Message) == 0 {
				continue
			}
			var msg struct {
				Content []struct {
					Type     string          `json:"type"`
					Text     string          `json:"text,omitempty"`
					Name     string          `json:"name,omitempty"`
					Input    json.RawMessage `json:"input,omitempty"`
					Thinking string          `json:"thinking,omitempty"`
				} `json:"content"`
			}
			if err := json.Unmarshal(event.Message, &msg); err != nil {
				continue
			}
			for _, block := range msg.Content {
				switch block.Type {
				case "tool_use":
					inputStr := ""
					if len(block.Input) > 0 {
						inputStr = string(block.Input)
						if len(inputStr) > 50 {
							inputStr = inputStr[:47] + "..."
						}
					}
					entries = append(entries, fmt.Sprintf("[tool] %s %s", block.Name, inputStr))
				case "text":
					entries = append(entries, formatMultiLineEntry("[text] ", "       ", block.Text, 3)...)
				case "thinking":
					entries = append(entries, formatMultiLineEntry("[think] ", "        ", block.Thinking, 3)...)
				}
			}
		case "message":
			// Gemini-style delta message — accumulate fragments
			if event.Role == "assistant" && event.Content != "" {
				geminiTextBuf.WriteString(event.Content)
			}
		case "tool_use":
			// Gemini top-level tool_use event — flush any buffered text first
			flushGeminiText()
			paramStr := ""
			if len(event.Parameters) > 0 {
				paramStr = string(event.Parameters)
				if len(paramStr) > 50 {
					paramStr = paramStr[:47] + "..."
				}
			}
			name := event.ToolName
			if name == "" {
				name = "unknown"
			}
			activity := fmt.Sprintf("[tool] %s", name)
			if paramStr != "" {
				activity = fmt.Sprintf("%s %s", activity, paramStr)
			}
			entries = append(entries, activity)
		case "tool_result":
			// Gemini tool_result — flush any buffered text (assistant spoke before tool ran)
			flushGeminiText()
		case "rate_limit_event":
			// Claude-style informational event — status is inside rate_limit_info
			if event.RateLimitInfo != nil && event.RateLimitInfo.Status != "" {
				entries = append(entries, fmt.Sprintf("[rate] %s", event.RateLimitInfo.Status))
			}
		case "result":
			flushGeminiText()
			subtype := event.Subtype
			if subtype == "" {
				subtype = "done"
			}
			entries = append(entries, fmt.Sprintf("[result] %s", subtype))
		}
	}

	flushGeminiText()

	if len(entries) > maxEntries {
		entries = entries[len(entries)-maxEntries:]
	}
	return entries
}

// formatMultiLineEntry splits raw text into up to maxLines non-empty lines.
// The first line gets the given prefix (e.g. "[text] "), continuation lines
// get contPrefix (spaces matching the prefix width). Each line is truncated
// to 70 characters. Returns nil if the text is empty.
func formatMultiLineEntry(prefix, contPrefix, raw string, maxLines int) []string {
	var kept []string
	for _, tl := range strings.Split(raw, "\n") {
		tl = strings.TrimSpace(tl)
		if tl == "" {
			continue
		}
		kept = append(kept, tl)
		if len(kept) >= maxLines {
			break
		}
	}
	if len(kept) == 0 {
		return nil
	}
	var result []string
	for i, line := range kept {
		if len([]rune(line)) > 70 {
			line = string([]rune(line)[:67]) + "..."
		}
		if i == 0 {
			result = append(result, prefix+line)
		} else {
			result = append(result, contPrefix+line)
		}
	}
	return result
}

// FetchEvents reads recent events from the state DB.
// Poll events (poll, poll_error) are excluded because anvil health is now
// displayed inline in the Queue panel headers. The filter is applied at the
// SQL level via RecentEventsExcluding so the LIMIT returns the expected count.
func FetchEvents(db *state.DB, limit int) tea.Cmd {
	return func() tea.Msg {
		if limit <= 0 {
			limit = 50
		}
		events, err := db.RecentEventsExcluding(limit, []state.EventType{state.EventPoll, state.EventPollError})
		if err != nil {
			return UpdateEventsMsg{Items: nil}
		}

		var items []EventItem
		for _, e := range events {
			items = append(items, EventItem{
				Timestamp: e.Timestamp.Format("15:04:05"),
				Type:      string(e.Type),
				Message:   e.Message,
				BeadID:    e.BeadID,
			})
		}

		return UpdateEventsMsg{Items: items}
	}
}

// FetchAnvilHealth queries the last poll result per anvil and returns
// health status items for the Queue panel headers.
func FetchAnvilHealth(ds *DataSource) tea.Cmd {
	return func() tea.Msg {
		statuses, err := ds.DB.LastPollPerAnvil(ds.AnvilNames)
		if err != nil {
			return UpdateAnvilHealthMsg{Items: nil}
		}
		now := time.Now()
		var items []AnvilHealth
		for _, s := range statuses {
			items = append(items, AnvilHealth{
				Anvil:     s.Anvil,
				OK:        s.OK,
				Message:   s.Message,
				Timestamp: s.Timestamp.Format("15:04:05"),
				Age:       shortDuration(now.Sub(s.Timestamp)),
			})
		}
		return UpdateAnvilHealthMsg{Items: items}
	}
}

// shortDuration formats a duration as a compact human-readable string.
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// FetchNeedsAttention reads beads that need human intervention from the state DB.
// This includes both retry-exhausted beads and PRs that have exhausted their
// CI-fix, review-fix, or rebase attempt limits. Thresholds are taken from ds
// so the TUI stays in sync with the daemon's configured limits.
func FetchNeedsAttention(ds *DataSource) tea.Cmd {
	return func() tea.Msg {
		beads, err := ds.DB.NeedsAttentionBeads(
			ds.MaxCIFixAttempts,
			ds.MaxReviewFixAttempts,
			ds.MaxRebaseAttempts,
		)
		if err != nil {
			return NeedsAttentionErrorMsg{Err: fmt.Errorf("failed to fetch needs attention beads: %w", err)}
		}
		var items []NeedsAttentionItem
		for _, b := range beads {
			items = append(items, NeedsAttentionItem{
				BeadID:         b.BeadID,
				Title:          b.Title,
				Description:    b.Description,
				Anvil:          b.Anvil,
				Reason:         b.Reason,
				ReasonCategory: classifyAttentionReason(b),
				PRID:           b.PRID,
				PRNumber:       b.PRNumber,
			})
		}

		return UpdateNeedsAttentionMsg{Items: items}
	}
}

// FetchReadyToMerge reads PRs that are ready to merge from the state DB.
func FetchReadyToMerge(ds DataSource) tea.Cmd {
	return func() tea.Msg {
		prs, err := ds.DB.ReadyToMergePRs()
		if err != nil {
			return ReadyToMergeErrorMsg{Err: fmt.Errorf("failed to fetch ready-to-merge PRs: %w", err)}
		}
		var items []ReadyToMergeItem
		for _, p := range prs {
			items = append(items, ReadyToMergeItem{
				PRID:      p.ID,
				PRNumber:  p.Number,
				BeadID:    p.BeadID,
				Anvil:     p.Anvil,
				Branch:    p.Branch,
				Title:     p.Title,
				AutoMerge: ds.AutoMergeAnvils[p.Anvil],
			})
		}
		return UpdateReadyToMergeMsg{Items: items}
	}
}

// FetchCrucibles reads active Crucible statuses from the daemon via IPC.
func FetchCrucibles() tea.Cmd {
	return func() tea.Msg {
		client, err := ipc.NewClient()
		if err != nil {
			return UpdateCruciblesMsg{Items: nil}
		}
		defer client.Close()

		resp, err := client.Send(ipc.Command{Type: "crucibles"})
		if err != nil {
			return UpdateCruciblesMsg{Items: nil}
		}
		if resp.Type != "ok" {
			return UpdateCruciblesMsg{Items: nil}
		}

		var cr ipc.CruciblesResponse
		if err := json.Unmarshal(resp.Payload, &cr); err != nil {
			return UpdateCruciblesMsg{Items: nil}
		}

		var items []CrucibleItem
		for _, c := range cr.Crucibles {
			items = append(items, CrucibleItem{
				ParentID:          c.ParentID,
				ParentTitle:       c.ParentTitle,
				Anvil:             c.Anvil,
				Branch:            c.Branch,
				Phase:             c.Phase,
				TotalChildren:     c.TotalChildren,
				CompletedChildren: c.CompletedChildren,
				CurrentChild:      c.CurrentChild,
				StartedAt:         c.StartedAt,
			})
		}

		return UpdateCruciblesMsg{Items: items}
	}
}

// FormatCost formats a USD cost for display.
func FormatCost(usd float64) string {
	if usd < 0.01 {
		return fmt.Sprintf("$%.4f", usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}

// FormatTokens formats a token count for compact display.
// Returns "1.2M" for millions, "340k" for thousands, or the raw number for small values.
func FormatTokens(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

// formatCopilotRequests formats a fractional premium request count for display.
// Shows "5" for whole numbers, "5.33" for fractional values.
func formatCopilotRequests(n float64) string {
	if n == float64(int(n)) {
		return fmt.Sprintf("%.0f", n)
	}
	return fmt.Sprintf("%.2f", n)
}

// UsageData holds the aggregated usage information for the Usage panel.
type UsageData struct {
	Providers    []ProviderUsage
	TotalCost    float64
	CostLimit    float64 // 0 = no limit
	CopilotUsed  float64
	CopilotLimit int // 0 = no limit
}

// ProviderUsage holds cost/token data for a single provider.
type ProviderUsage struct {
	Provider     string
	Cost         float64
	InputTokens  int
	OutputTokens int
}

// UpdateUsageMsg carries refreshed usage data to the TUI.
type UpdateUsageMsg struct {
	Data UsageData
}

// FetchUsage reads today's per-provider costs and copilot premium requests.
func FetchUsage(ds *DataSource) tea.Cmd {
	return func() tea.Msg {
		today := time.Now().Format("2006-01-02")

		var data UsageData
		data.CostLimit = ds.DailyCostLimit
		data.CopilotLimit = ds.CopilotDailyRequestLimit

		// Per-provider costs
		provCosts, err := ds.DB.GetProviderDailyCosts(today)
		if err == nil {
			for _, pc := range provCosts {
				data.Providers = append(data.Providers, ProviderUsage{
					Provider:     pc.Provider,
					Cost:         pc.EstimatedCost,
					InputTokens:  pc.InputTokens,
					OutputTokens: pc.OutputTokens,
				})
				data.TotalCost += pc.EstimatedCost
			}
		}

		// If no per-provider data, fall back to aggregate daily cost
		if len(data.Providers) == 0 {
			if totalCost, err := ds.DB.GetTodayCostOn(today); err == nil && totalCost > 0 {
				data.TotalCost = totalCost
			}
		}

		// Copilot premium requests
		if used, err := ds.DB.GetCopilotRequestsOn(today); err == nil {
			data.CopilotUsed = used
		}

		return UpdateUsageMsg{Data: data}
	}
}

// UpdateDaemonHealthMsg carries the result of a daemon health check to the TUI.
type UpdateDaemonHealthMsg struct {
	Connected bool
	Workers   int
	QueueSize int
	LastPoll  string
	Uptime    string
}

// FetchDaemonHealth probes the daemon via IPC and returns connectivity status.
func FetchDaemonHealth() tea.Cmd {
	return func() tea.Msg {
		client, err := ipc.NewClient()
		if err != nil {
			return UpdateDaemonHealthMsg{Connected: false}
		}
		defer client.Close()

		resp, err := client.Send(ipc.Command{Type: "status"})
		if err != nil || resp.Type != "status" {
			return UpdateDaemonHealthMsg{Connected: false}
		}

		var s ipc.StatusPayload
		if err := json.Unmarshal(resp.Payload, &s); err != nil {
			return UpdateDaemonHealthMsg{Connected: false}
		}

		return UpdateDaemonHealthMsg{
			Connected: true,
			Workers:   s.Workers,
			QueueSize: s.QueueSize,
			LastPoll:  s.LastPoll,
			Uptime:    s.Uptime,
		}
	}
}

// PendingOrphanItem represents an orphaned bead awaiting user decision in Hearth.
type PendingOrphanItem struct {
	BeadID string
	Anvil  string
	Title  string
	Branch string
}

// UpdatePendingOrphansMsg carries the list of pending orphans to the TUI.
type UpdatePendingOrphansMsg struct {
	Items []PendingOrphanItem
}

// FetchPendingOrphans reads orphaned beads awaiting user decision from the state DB.
func FetchPendingOrphans(db *state.DB) tea.Cmd {
	return func() tea.Msg {
		orphans, err := db.ListPendingOrphans()
		if err != nil {
			return UpdatePendingOrphansMsg{Items: nil}
		}
		var items []PendingOrphanItem
		for _, o := range orphans {
			items = append(items, PendingOrphanItem{
				BeadID: o.BeadID,
				Anvil:  o.Anvil,
				Title:  o.Title,
				Branch: o.Branch,
			})
		}
		return UpdatePendingOrphansMsg{Items: items}
	}
}

// UpdateOpenPRsMsg carries refreshed open PR data to the TUI.
type UpdateOpenPRsMsg struct {
	Items []PRItem
}

// OpenPRsErrorMsg signals that reading open PRs failed.
type OpenPRsErrorMsg struct{ Err error }

// reconcilePRsDoneMsg is sent after GitHub PR reconciliation completes.
type reconcilePRsDoneMsg struct{}

// FetchOpenPRs reads all non-terminal PRs with status detail from the state DB.
func FetchOpenPRs(db *state.DB) tea.Cmd {
	return func() tea.Msg {
		prs, err := db.OpenPRsWithDetail()
		if err != nil {
			return OpenPRsErrorMsg{Err: err}
		}
		var items []PRItem
		for _, p := range prs {
			items = append(items, PRItem{
				PRID:                 p.ID,
				PRNumber:             p.Number,
				Anvil:                p.Anvil,
				BeadID:               p.BeadID,
				Branch:               p.Branch,
				Status:               string(p.Status),
				Title:                p.Title,
				CIPassing:            p.CIPassing,
				IsConflicting:        p.IsConflicting,
				HasUnresolvedThreads: p.HasUnresolvedThreads,
				HasPendingReviews:    p.HasPendingReviews,
				HasApproval:          p.HasApproval,
				CIFixCount:           p.CIFixCount,
				ReviewFixCount:       p.ReviewFixCount,
				RebaseCount:          p.RebaseCount,
				IsExternal:           p.IsExternal,
				BellowsManaged:       p.BellowsManaged,
			})
		}
		return UpdateOpenPRsMsg{Items: items}
	}
}

// FetchAll returns a batch command that refreshes all panels.
// Daemon health is NOT included here; it is fetched on a slower cadence
// controlled by healthTickDivisor in the TickMsg handler.
func FetchAll(ds *DataSource) tea.Cmd {
	return tea.Batch(
		FetchQueue(ds.DB),
		FetchNeedsAttention(ds),
		FetchReadyToMerge(*ds),
		FetchWorkers(ds.DB),
		FetchEvents(ds.DB, EventFetchLimit),
		FetchCrucibles(),
		FetchUsage(ds),
		FetchPendingOrphans(ds.DB),
		FetchAnvilHealth(ds),
		FetchOpenPRs(ds.DB),
	)
}
