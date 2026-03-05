package hearth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/state"
)

// TickInterval is how often the TUI refreshes data.
const TickInterval = 2 * time.Second

// TickMsg triggers a data refresh cycle.
type TickMsg time.Time

// DataSource holds the dependencies needed to feed the TUI panels.
type DataSource struct {
	DB     *state.DB
	Anvils map[string]config.AnvilConfig
	Ctx    context.Context
}

// Tick returns a Bubbletea command that sends a TickMsg after the interval.
func Tick() tea.Cmd {
	return tea.Tick(TickInterval, func(t time.Time) tea.Msg {
		return TickMsg(t)
	})
}

// FetchQueue polls bd ready for each anvil and returns a queue update message.
// Results are sorted by priority (lowest number = highest priority).
func FetchQueue(ctx context.Context, anvils map[string]config.AnvilConfig) tea.Cmd {
	return func() tea.Msg {
		p := poller.New(anvils)
		beads, _ := p.Poll(ctx)

		var items []QueueItem
		for _, b := range beads {
			items = append(items, QueueItem{
				BeadID:   b.ID,
				Title:    b.Title,
				Anvil:    b.Anvil,
				Priority: b.Priority,
				Status:   b.Status,
			})
		}

		// Already sorted by poller, but ensure priority order
		sort.Slice(items, func(i, j int) bool {
			if items[i].Priority != items[j].Priority {
				return items[i].Priority < items[j].Priority
			}
			return items[i].BeadID < items[j].BeadID
		})

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
			// Gemini-style delta message
			if event.Role == "assistant" && event.Content != "" {
				text := strings.ReplaceAll(event.Content, "\n", " ")
				text = strings.TrimSpace(text)
				if text != "" {
					if len(text) > 70 {
						text = text[:67] + "..."
					}
					entries = append(entries, fmt.Sprintf("[text] %s", text))
				}
			}
		case "rate_limit_event":
			// Claude-style informational event — status is inside rate_limit_info
			if event.RateLimitInfo != nil && event.RateLimitInfo.Status != "" {
				entries = append(entries, fmt.Sprintf("[rate] %s", event.RateLimitInfo.Status))
			}
		case "result":
			subtype := event.Subtype
			if subtype == "" {
				subtype = "done"
			}
			entries = append(entries, fmt.Sprintf("[result] %s", subtype))
		}
	}

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
func FetchAll(ctx context.Context, db *state.DB, anvils map[string]config.AnvilConfig) tea.Cmd {
	return tea.Batch(
		FetchQueue(ctx, anvils),
		FetchWorkers(db),
		FetchEvents(db, 100),
	)
}

// FormatCost formats a USD cost for display.
func FormatCost(usd float64) string {
	if usd < 0.01 {
		return fmt.Sprintf("$%.4f", usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}
