package hearth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Robin831/Forge/internal/ingot"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

// classifyAttentionReason determines the AttentionReason category from a
// NeedsAttentionBead's fields. The classification is ordered by specificity:
// clarification flag, circuit breaker prefix, reason text patterns, stalled.
func classifyAttentionReason(b state.NeedsAttentionBead) AttentionReason {
	// Non-bead entries are classified by kind, not by reason text: they describe
	// an unusable anvil or a self-deploy that never went live, not a bead that
	// failed.
	switch b.Kind {
	case state.AttentionKindAnvil:
		return AttentionAnvilWedged
	case state.AttentionKindDeploy:
		return AttentionSelfDeploy
	}
	if b.ClarificationNeeded {
		return AttentionClarification
	}
	reason := strings.ToLower(b.Reason)
	if strings.HasPrefix(reason, "circuit breaker:") {
		return AttentionDispatchExhausted
	}
	if strings.HasPrefix(reason, "recovery failed:") {
		return AttentionRecoveryExhausted
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
const TickInterval = 5 * time.Second

// healthTickDivisor controls how often the daemon health IPC check runs relative
// to the main tick. At TickInterval=5s and divisor=5, health is checked every 25s.
const healthTickDivisor = 5

// EventFetchLimit is the maximum number of events retrieved for the Events panel.
const EventFetchLimit = 100

// EventFilterMatchLimit is the maximum number of events returned when an event
// filter is active. The filter is applied at the SQL level so the whole event
// log is searched (not just the most recent EventFetchLimit rows), but the
// result count is still capped to protect the TUI render.
const EventFilterMatchLimit = 500

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
	// AutoMergeAnvils returns the set of anvil names that have auto_merge
	// enabled. The map is built once at Hearth startup from the loaded config;
	// reflecting config changes requires restarting Hearth.
	// When nil, no PRs are tagged [auto].
	AutoMergeAnvils func() map[string]bool
	// WicketEnabled controls whether the Wicket panel is shown in the TUI.
	// Set to true when wicket_enabled is true in the loaded config.
	WicketEnabled bool
	// AnvilRepoURLs maps an anvil name to its GitHub repository base URL
	// (e.g. "https://github.com/owner/repo"), derived once at startup from the
	// anvil's git remote. The Queue panel uses it to build clickable PR links.
	// An anvil missing from the map (or a blank value) renders PR numbers as
	// plain text without a hyperlink.
	AnvilRepoURLs map[string]string
}

// prURLForBead builds the GitHub PR URL for a bead's PR from the anvil's
// repository base URL. Returns an empty string when the base URL is unknown
// or the PR number is not positive, so callers fall back to plain text.
func prURLForBead(anvilRepoURLs map[string]string, anvil string, prNumber int) string {
	if prNumber <= 0 {
		return ""
	}
	base := anvilRepoURLs[anvil]
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/pull/%d", strings.TrimRight(base, "/"), prNumber)
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
// Each queued bead is enriched with its open PR number and a clickable GitHub
// URL by joining the prs table on (anvil, bead ID).
func FetchQueue(ds *DataSource) tea.Cmd {
	return func() tea.Msg {
		cached, err := ds.DB.QueueCache()
		if err != nil {
			return QueueErrorMsg{Err: err}
		}

		// Build a lookup of (anvil, bead ID) → PR number from open PRs so each
		// queued bead can show its PR. Keyed by anvil+\x00+beadID to avoid
		// collisions between anvils that reuse bead IDs.
		prByBead := make(map[string]int)
		if prs, prErr := ds.DB.OpenPRs(); prErr == nil {
			for _, p := range prs {
				if p.BeadID == "" || p.Number == 0 {
					continue
				}
				prByBead[p.Anvil+"\x00"+p.BeadID] = p.Number
			}
		}

		var items []QueueItem
		for _, c := range cached {
			prNumber := prByBead[c.Anvil+"\x00"+c.BeadID]
			items = append(items, QueueItem{
				BeadID:      c.BeadID,
				Title:       c.Title,
				Description: c.Description,
				Anvil:       c.Anvil,
				Priority:    c.Priority,
				Status:      c.Status,
				Section:     string(c.Section),
				Assignee:    c.Assignee,
				PRNumber:    prNumber,
				PRURL:       prURLForBead(ds.AnvilRepoURLs, c.Anvil, prNumber),
			})
		}

		return UpdateQueueMsg{Items: items}
	}
}

// FetchWorkers reads active workers from the state DB and enriches with
// log activity via incremental log tailing. When logCache is nil, falls
// back to full-file reading (for tests or one-shot use).
func FetchWorkers(db *state.DB, logCache *LogTailerCache) tea.Cmd {
	return func() tea.Msg {
		workers, err := db.ActiveWorkers()
		if err != nil {
			return UpdateWorkersMsg{Items: nil}
		}

		// Track active log paths so we can prune stale tailers.
		activePaths := make(map[string]bool, len(workers))

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

			var lastLog string
			var activityLines []string
			if logCache != nil && w.LogPath != "" {
				activePaths[w.LogPath] = true
				activityLines, lastLog = logCache.ReadIncremental(w.LogPath, 100)
			} else {
				lastLog = readLastLogLine(w.LogPath)
				activityLines = parseWorkerActivity(w.LogPath, 100)
			}

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

		// Clean up tailers for workers that are no longer active.
		if logCache != nil {
			logCache.Prune(activePaths)
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
	case len(id) > 7 && id[:7] == "quench-":
		return "quench"
	case len(id) > 8 && id[:8] == "burnish-":
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

// formatToolCall produces a rich one-line summary for a tool invocation.
// Known tools get purpose-specific formatting; unknown tools fall back to
// a truncated JSON dump of their parameters.
func formatToolCall(name string, rawInput json.RawMessage) string {
	var params map[string]json.RawMessage
	if len(rawInput) > 0 {
		_ = json.Unmarshal(rawInput, &params)
	}

	getString := func(key string) string {
		v, ok := params[key]
		if !ok || len(v) == 0 {
			return ""
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
		// Handle numbers and other JSON primitives by stripping quotes.
		raw := strings.TrimSpace(string(v))
		if raw != "null" {
			return raw
		}
		return ""
	}

	shortenPath := func(p string) string {
		if p == "" {
			return ""
		}
		return filepath.Base(p)
	}

	switch name {
	case "Read":
		fp := getString("file_path")
		if fp == "" {
			return "[tool] Read"
		}
		detail := shortenPath(fp)
		if offset := getString("offset"); offset != "" {
			if limit := getString("limit"); limit != "" {
				detail += ":" + offset + "-" + limit
			} else {
				detail += ":" + offset
			}
		}
		return fmt.Sprintf("[tool] Read %s", detail)

	case "Edit":
		fp := getString("file_path")
		if fp == "" {
			break
		}
		detail := shortenPath(fp)
		old := getString("old_string")
		if old != "" {
			// Show first line of the old string as context
			if idx := strings.IndexByte(old, '\n'); idx > 0 {
				old = old[:idx]
			}
			old = strings.TrimSpace(old)
			if len([]rune(old)) > 40 {
				old = string([]rune(old)[:37]) + "..."
			}
			detail += " «" + old + "»"
		}
		return fmt.Sprintf("[tool] Edit %s", detail)

	case "Write":
		fp := getString("file_path")
		if fp == "" {
			break
		}
		return fmt.Sprintf("[tool] Write %s", shortenPath(fp))

	case "Bash":
		cmd := getString("command")
		if cmd == "" {
			break
		}
		// Show first line of command, truncated
		if idx := strings.IndexByte(cmd, '\n'); idx > 0 {
			cmd = cmd[:idx]
		}
		cmd = strings.TrimSpace(cmd)
		if len([]rune(cmd)) > 46 {
			cmd = string([]rune(cmd)[:46]) + "..."
		}
		return fmt.Sprintf("[tool] Bash $ %s", cmd)

	case "Grep":
		pattern := getString("pattern")
		if pattern == "" {
			break
		}
		detail := fmt.Sprintf("/%s/", pattern)
		if glob := getString("glob"); glob != "" {
			detail += " " + glob
		} else if tp := getString("type"); tp != "" {
			detail += " **/*." + tp
		}
		if len([]rune(detail)) > 50 {
			detail = string([]rune(detail)[:47]) + "..."
		}
		return fmt.Sprintf("[tool] Grep %s", detail)

	case "Glob":
		pattern := getString("pattern")
		if pattern == "" {
			break
		}
		detail := pattern
		if len([]rune(detail)) > 50 {
			detail = string([]rune(detail)[:47]) + "..."
		}
		return fmt.Sprintf("[tool] Glob %s", detail)

	case "Agent":
		desc := getString("description")
		if desc == "" {
			break
		}
		if len([]rune(desc)) > 50 {
			desc = string([]rune(desc)[:47]) + "..."
		}
		return fmt.Sprintf("[tool] Agent %s", desc)
	}

	// Fallback: name + truncated raw params
	if len(rawInput) > 0 {
		fallback := string(rawInput)
		if len([]rune(fallback)) > 50 {
			fallback = string([]rune(fallback)[:47]) + "..."
		}
		return fmt.Sprintf("[tool] %s %s", name, fallback)
	}
	return fmt.Sprintf("[tool] %s", name)
}

// toolResultEnrichment extracts a short status suffix from a tool_result's
// content string and is_error flag. The suffix is appended to the original
// [tool] entry so users can see the outcome at a glance.
func toolResultEnrichment(toolName string, content string, isError bool) string {
	if isError {
		// Show a concise error message
		msg := strings.TrimSpace(content)
		if msg == "" {
			return " → ✗ error"
		}
		// Take first line, truncated
		if idx := strings.IndexByte(msg, '\n'); idx > 0 {
			msg = msg[:idx]
		}
		if len([]rune(msg)) > 30 {
			msg = string([]rune(msg)[:27]) + "..."
		}
		return " → ✗ " + msg
	}

	content = strings.TrimSpace(content)

	switch toolName {
	case "Bash":
		if content == "" {
			return " → ✓"
		}
		// Show line count of output as a proxy for verbosity
		lines := strings.Count(content, "\n") + 1
		if lines == 1 && len(content) < 40 {
			return " → ✓ " + content
		}
		if lines == 1 {
			return " → ✓ 1 line"
		}
		return fmt.Sprintf(" → ✓ %d lines", lines)
	case "Grep":
		// Count result lines (files or matches)
		lines := strings.Count(content, "\n") + 1
		if lines == 1 && strings.TrimSpace(content) == "" {
			return " → 0 matches"
		}
		if strings.HasPrefix(content, "No files found") || strings.HasPrefix(content, "No matches found") {
			return " → 0 matches"
		}
		if strings.HasPrefix(content, "Found ") {
			// e.g. "Found 5 files"
			if idx := strings.IndexByte(content, '\n'); idx > 0 {
				return " → " + content[:idx]
			}
			return " → " + content
		}
		if lines == 1 {
			return " → 1 match"
		}
		return fmt.Sprintf(" → %d matches", lines)
	case "Glob":
		if strings.TrimSpace(content) == "" {
			return " → 0 files"
		}
		lines := strings.Count(content, "\n") + 1
		if lines == 1 {
			return " → 1 file"
		}
		return fmt.Sprintf(" → %d files", lines)
	case "Edit":
		return " → ✓"
	case "Write":
		return " → ✓"
	case "Read":
		return "" // Success is implicit for Read
	case "Agent":
		return " → done"
	}
	return " → ✓"
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

	// toolIndex maps tool_use_id → index in entries slice for result correlation.
	toolIndex := make(map[string]int)
	// toolNames maps tool_use_id → tool name for enrichment formatting.
	toolNames := make(map[string]string)

	flushGeminiText := func() {
		if geminiTextBuf.Len() == 0 {
			return
		}
		raw := geminiTextBuf.String()
		geminiTextBuf.Reset()
		entries = append(entries, formatMultiLineEntry("[text] ", "       ", raw, 20)...)
	}

	// enrichToolEntry correlates a tool_result back to its tool_use entry
	// and appends a status suffix.
	enrichToolEntry := func(toolUseID, content string, isError bool) {
		idx, ok := toolIndex[toolUseID]
		if !ok {
			return
		}
		name := toolNames[toolUseID]
		suffix := toolResultEnrichment(name, content, isError)
		if suffix != "" {
			entries[idx] += suffix
		}
		delete(toolIndex, toolUseID)
		delete(toolNames, toolUseID)
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
			// Gemini top-level tool_use/tool_result fields
			ToolName      string          `json:"tool_name,omitempty"`
			ToolID        string          `json:"tool_id,omitempty"`
			Parameters    json.RawMessage `json:"parameters,omitempty"`
			Output        string          `json:"output,omitempty"`
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
					ID       string          `json:"id,omitempty"`
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
					idx := len(entries)
					entries = append(entries, formatToolCall(block.Name, block.Input))
					if block.ID != "" {
						toolIndex[block.ID] = idx
						toolNames[block.ID] = block.Name
					}
				case "text":
					entries = append(entries, formatMultiLineEntry("[text] ", "       ", block.Text, 20)...)
				case "thinking":
					entries = append(entries, formatMultiLineEntry("[think] ", "        ", block.Thinking, 20)...)
				}
			}
		case "user":
			// Claude-style user message containing tool_result blocks.
			if len(event.Message) == 0 {
				continue
			}
			var msg struct {
				Content []struct {
					Type      string `json:"type"`
					ToolUseID string `json:"tool_use_id,omitempty"`
					Content   string `json:"content,omitempty"`
					IsError   bool   `json:"is_error,omitempty"`
				} `json:"content"`
			}
			if err := json.Unmarshal(event.Message, &msg); err != nil {
				continue
			}
			for _, block := range msg.Content {
				if block.Type == "tool_result" && block.ToolUseID != "" {
					enrichToolEntry(block.ToolUseID, block.Content, block.IsError)
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
			name := event.ToolName
			if name == "" {
				name = "unknown"
			}
			idx := len(entries)
			entries = append(entries, formatToolCall(name, event.Parameters))
			if event.ToolID != "" {
				toolIndex[event.ToolID] = idx
				toolNames[event.ToolID] = name
			}
		case "tool_result":
			// Gemini tool_result — flush any buffered text, then enrich the tool entry.
			flushGeminiText()
			if event.ToolID != "" {
				enrichToolEntry(event.ToolID, event.Output, false)
			}
		case "rate_limit_event":
			// Claude-style informational event — status is inside rate_limit_info
			if event.RateLimitInfo != nil && event.RateLimitInfo.Status != "" {
				entries = append(entries, fmt.Sprintf("[rate] %s", event.RateLimitInfo.Status))
			}
		case "result":
			flushGeminiText()
			// Session-end marker — dropped from Live Activity. The event log
			// already surfaces completion; showing it here caused a persistent
			// "[result] success" line at startup that stayed the whole session.
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
// get contPrefix (spaces matching the prefix width). Line length is not
// truncated here — the rendering layer applies word-wrap to fit the panel.
// Returns nil if the text is empty.
func formatMultiLineEntry(prefix, contPrefix, raw string, maxLines int) []string {
	var kept []string
	for tl := range strings.SplitSeq(raw, "\n") {
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

// formatEventTimestamp renders an event timestamp for the feed. Events pushed
// over the IPC subscribe stream carry an RFC3339 timestamp string; the feed
// shows the wall-clock "15:04:05" form used by the polled FetchEvents path so
// streamed and polled rows render identically. A value that does not parse as
// RFC3339 is returned unchanged as a best-effort fallback.
func formatEventTimestamp(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.Format("15:04:05")
	}
	return ts
}

// FetchEventsMatching searches the entire event log for events matching the
// filter and returns them as an UpdateFilteredEventsMsg. The search runs at the
// SQL level (via RecentEventsMatching) so events older than the EventFetchLimit
// window are also matched — fixing the case where filtering only ever scanned
// the most recent ~100 in-memory events. The same poll/poll_error exclusions as
// FetchEvents apply, and results are capped at EventFilterMatchLimit.
//
// The originating filter text is echoed back in the message so the model can
// discard results from a stale query (the user may have typed further since).
func FetchEventsMatching(db *state.DB, filter string) tea.Cmd {
	return func() tea.Msg {
		events, err := db.RecentEventsMatching(filter, EventFilterMatchLimit,
			[]state.EventType{state.EventPoll, state.EventPollError})
		if err != nil {
			return UpdateFilteredEventsMsg{Filter: filter, Items: nil}
		}

		items := make([]EventItem, 0, len(events))
		for _, e := range events {
			items = append(items, EventItem{
				Timestamp: e.Timestamp.Format("15:04:05"),
				Type:      string(e.Type),
				Message:   e.Message,
				BeadID:    e.BeadID,
			})
		}

		return UpdateFilteredEventsMsg{Filter: filter, Items: items}
	}
}

// FetchAnvilHealth returns per-anvil poll health status for the Queue panel
// headers. The daemon tracks successful polls only in memory (to keep the
// events table free of per-poll noise), so the fresh source of truth is the
// IPC status response. When IPC is unreachable we fall back to the events
// table, which now only surfaces poll_error rows.
func FetchAnvilHealth(ds *DataSource) tea.Cmd {
	return func() tea.Msg {
		if items, ok := fetchAnvilHealthFromIPC(); ok {
			return UpdateAnvilHealthMsg{Items: items}
		}

		var (
			statuses []state.AnvilPollStatus
			err      error
		)
		if len(ds.AnvilNames) == 0 {
			statuses, err = ds.DB.LastPollAllAnvils()
		} else {
			statuses, err = ds.DB.LastPollPerAnvil(ds.AnvilNames)
		}
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

// fetchAnvilHealthFromIPC reads the daemon's in-memory per-anvil last-poll
// snapshot via the existing "status" IPC command. Returns (items, true) on
// success — including the empty-but-connected case — and (nil, false) when
// the daemon is unreachable or returns malformed data so the caller can fall
// back to the events table.
func fetchAnvilHealthFromIPC() ([]AnvilHealth, bool) {
	client, err := ipc.NewClient()
	if err != nil {
		return nil, false
	}
	defer client.Close()

	resp, err := client.Send(ipc.Command{Type: "status"})
	if err != nil || resp.Type != "status" {
		return nil, false
	}
	var s ipc.StatusPayload
	if err := json.Unmarshal(resp.Payload, &s); err != nil {
		return nil, false
	}
	// nil AnvilLastPoll means the connected daemon is an older version that
	// does not include this field, or has not yet completed any poll.  In
	// either case we cannot distinguish "field absent" from "no data yet", so
	// fall back to the DB so the caller gets the best available information.
	if s.AnvilLastPoll == nil {
		return nil, false
	}
	now := time.Now()
	var items []AnvilHealth
	for _, p := range s.AnvilLastPoll {
		items = append(items, AnvilHealth{
			Anvil:     p.Anvil,
			OK:        p.OK,
			Message:   p.Message,
			Timestamp: p.Timestamp.Format("15:04:05"),
			Age:       shortDuration(now.Sub(p.Timestamp)),
		})
	}
	return items, true
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
			item := NeedsAttentionItem{
				BeadID:         b.BeadID,
				Title:          b.Title,
				Description:    b.Description,
				Anvil:          b.Anvil,
				Reason:         b.Reason,
				ReasonCategory: classifyAttentionReason(b),
				FailureCount:   b.FailureCount,
				PRID:           b.PRID,
				PRNumber:       b.PRNumber,
				Kind:           b.Kind,
			}
			// Anvil- and deploy-level entries have no bead, so skip the per-row
			// warden lookup.
			if item.IsBeadScoped() {
				item.LastWardenReject = ds.DB.LatestWardenRejectMessage(b.BeadID)
			}
			items = append(items, item)
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
		var autoMerge map[string]bool
		if ds.AutoMergeAnvils != nil {
			autoMerge = ds.AutoMergeAnvils()
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
				AutoMerge: autoMerge[p.Anvil],
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

// wicketScanResultMsg is sent after a manual Wicket scan request completes.
type wicketScanResultMsg struct{ Err error }

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

// UpdateIngotCountsMsg carries aggregated ingot status counts to the TUI.
type UpdateIngotCountsMsg struct {
	Counts map[ingot.Status]int
	Total  int
}

// FetchIngotCounts queries the state DB for ingot counts grouped by status.
// It runs a single GROUP BY status aggregation query directly against the DB.
func FetchIngotCounts(db *state.DB) tea.Cmd {
	return func() tea.Msg {
		conn := db.Conn()
		if conn == nil {
			return UpdateIngotCountsMsg{}
		}

		rows, err := conn.Query(`SELECT status, COUNT(*) FROM ingots GROUP BY status`)
		if err != nil {
			return UpdateIngotCountsMsg{}
		}
		defer rows.Close()

		counts := make(map[ingot.Status]int)
		total := 0
		for rows.Next() {
			var status string
			var count int
			if err := rows.Scan(&status, &count); err != nil {
				continue
			}
			counts[ingot.Status(status)] = count
			total += count
		}
		if err := rows.Err(); err != nil {
			return UpdateIngotCountsMsg{}
		}

		return UpdateIngotCountsMsg{Counts: counts, Total: total}
	}
}

// FetchWicketSummary queries the state DB for per-repo open and needs-human
// issue counts from the wicket_issues table.
func FetchWicketSummary(db *state.DB) tea.Cmd {
	return func() tea.Msg {
		summaries, err := db.GetWicketSummary()
		if err != nil {
			return UpdateWicketSummaryMsg{}
		}
		items := make([]WicketRepoSummary, 0, len(summaries))
		for _, s := range summaries {
			items = append(items, WicketRepoSummary{
				Repo:            s.Repo,
				OpenCount:       s.OpenCount,
				NeedsHumanCount: s.NeedsHumanCount,
			})
		}
		return UpdateWicketSummaryMsg{Items: items}
	}
}

// FetchAll returns a batch command that refreshes all panels.
// Daemon health is NOT included here; it is fetched on a slower cadence
// controlled by healthTickDivisor in the TickMsg handler.
// The event feed is likewise NOT fetched here: it rides the IPC subscribe
// stream once active, and the TickMsg handler only falls back to FetchEvents
// while streaming is inactive (see Model.eventStreamActive).
func FetchAll(ds *DataSource, logCache *LogTailerCache) tea.Cmd {
	return tea.Batch(
		FetchQueue(ds),
		FetchNeedsAttention(ds),
		FetchReadyToMerge(*ds),
		FetchWorkers(ds.DB, logCache),
		FetchCrucibles(),
		FetchUsage(ds),
		FetchPendingOrphans(ds.DB),
		FetchAnvilHealth(ds),
		FetchOpenPRs(ds.DB),
		FetchIngotCounts(ds.DB),
		FetchWicketSummary(ds.DB),
	)
}
