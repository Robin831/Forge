package hearth

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Robin831/Forge/internal/state"
)

// TickInterval is how often the TUI refreshes data.
const TickInterval = 2 * time.Second

// EventFetchLimit is the maximum number of events retrieved for the Events panel.
const EventFetchLimit = 100

// TickMsg triggers a data refresh cycle.
type TickMsg time.Time

// DataSource holds the dependencies needed to feed the TUI panels.
type DataSource struct {
	DB *state.DB
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
				BeadID:   c.BeadID,
				Title:    c.Title,
				Anvil:    c.Anvil,
				Priority: c.Priority,
				Status:   c.Status,
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
				Anvil:         w.Anvil,
				Status:        string(w.Status),
				Duration:      duration,
				Type:          wType,
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
		return "cifix"
	case len(id) > 10 && id[:10] == "reviewfix-":
		return "reviewfix"
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
		text := strings.ReplaceAll(geminiTextBuf.String(), "\n", " ")
		text = strings.TrimSpace(text)
		geminiTextBuf.Reset()
		if text == "" {
			return
		}
		if len(text) > 70 {
			text = text[:67] + "..."
		}
		entries = append(entries, fmt.Sprintf("[text] %s", text))
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
			ToolName   string          `json:"tool_name,omitempty"`
			Parameters json.RawMessage `json:"parameters,omitempty"`
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
					text := strings.ReplaceAll(block.Text, "\n", " ")
					text = strings.TrimSpace(text)
					if text != "" {
						if len(text) > 70 {
							text = text[:67] + "..."
						}
						entries = append(entries, fmt.Sprintf("[text] %s", text))
					}
				case "thinking":
					thinking := strings.ReplaceAll(block.Thinking, "\n", " ")
					thinking = strings.TrimSpace(thinking)
					if thinking != "" {
						if len(thinking) > 60 {
							thinking = thinking[:57] + "..."
						}
						entries = append(entries, fmt.Sprintf("[think] %s", thinking))
					}
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
			entries = append(entries, fmt.Sprintf("[tool] %s %s", name, paramStr))
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

// FetchEvents reads recent events from the state DB.
func FetchEvents(db *state.DB, limit int) tea.Cmd {
	return func() tea.Msg {
		if limit <= 0 {
			limit = 50
		}
		events, err := db.RecentEvents(limit)
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

// FetchAll returns a batch command that refreshes all three panels.
func FetchAll(db *state.DB) tea.Cmd {
	return tea.Batch(
		FetchQueue(db),
		FetchWorkers(db),
		FetchEvents(db, EventFetchLimit),
	)
}

// FormatCost formats a USD cost for display.
func FormatCost(usd float64) string {
	if usd < 0.01 {
		return fmt.Sprintf("$%.4f", usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}
