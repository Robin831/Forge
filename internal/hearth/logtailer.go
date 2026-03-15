package hearth

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
)

// LogTailerCache manages incremental log file reading for all active workers.
// Instead of re-reading entire log files on each tick, each tailer tracks its
// read offset and only parses newly appended lines.
type LogTailerCache struct {
	mu      sync.Mutex
	tailers map[string]*logTailer // keyed by log file path
}

// NewLogTailerCache creates an empty cache.
func NewLogTailerCache() *LogTailerCache {
	return &LogTailerCache{
		tailers: make(map[string]*logTailer),
	}
}

// retentionCap is the maximum number of entries kept in a logTailer.
// When exceeded, the oldest entries are discarded and toolIndex is adjusted.
// This prevents unbounded memory growth for long-running workers.
const retentionCap = 500

// maxPendingTools is the maximum number of unenriched tool_use entries
// tracked in toolIndex/toolNames. Older entries are evicted when exceeded.
const maxPendingTools = 100

// logTailer tracks incremental reading state for a single worker's log file.
type logTailer struct {
	offset  int64  // bytes read so far
	partial string // leftover bytes from an incomplete line at the end of the last read

	// Accumulated parsed activity entries, capped at retentionCap.
	entries []string

	// Tool result correlation state — persists across reads so results
	// arriving in a later chunk can enrich tool_use entries from earlier.
	toolIndex map[string]int    // tool_use_id → index in entries
	toolNames map[string]string // tool_use_id → tool name

	// Gemini delta text accumulator.
	geminiTextBuf strings.Builder

	// Last non-empty raw JSON line (used for LastLog display).
	lastRawLine string
}

// ReadIncremental reads only newly appended bytes from logPath, parses them
// into activity entries, and returns the last maxEntries lines plus the last
// raw JSON line. It is safe for concurrent use (the cache is mutex-protected).
func (c *LogTailerCache) ReadIncremental(logPath string, maxEntries int) (entries []string, lastRaw string) {
	if logPath == "" || maxEntries <= 0 {
		return nil, ""
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	t := c.tailers[logPath]
	if t == nil {
		t = &logTailer{
			toolIndex: make(map[string]int),
			toolNames: make(map[string]string),
		}
		c.tailers[logPath] = t
	}

	// Open file and check size.
	f, err := os.Open(logPath)
	if err != nil {
		return t.tail(maxEntries), t.lastRawLine
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return t.tail(maxEntries), t.lastRawLine
	}

	size := info.Size()

	// If file shrank (truncated/replaced), reset state.
	if size < t.offset {
		*t = logTailer{
			toolIndex: make(map[string]int),
			toolNames: make(map[string]string),
		}
	}

	// Nothing new to read.
	if size == t.offset {
		return t.tail(maxEntries), t.lastRawLine
	}

	// Seek to last known position and read new bytes.
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return t.tail(maxEntries), t.lastRawLine
	}

	newBytes, err := io.ReadAll(f)
	if err != nil {
		return t.tail(maxEntries), t.lastRawLine
	}

	t.offset += int64(len(newBytes))

	// Prepend any partial line from the previous read.
	chunk := t.partial + string(newBytes)
	t.partial = ""

	// Split into lines. The last element may be a partial line if
	// the file didn't end with a newline.
	lines := strings.Split(chunk, "\n")
	if len(lines) > 0 {
		last := lines[len(lines)-1]
		if last != "" {
			// Last line is incomplete — buffer it for the next read.
			t.partial = last
		}
		lines = lines[:len(lines)-1]
	}

	// Parse each new complete line.
	t.parseLines(lines)
	t.compact()

	return t.tail(maxEntries), t.lastRawLine
}

// Prune removes tailers for log paths not in the active set.
func (c *LogTailerCache) Prune(activePaths map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for path := range c.tailers {
		if !activePaths[path] {
			delete(c.tailers, path)
		}
	}
}

// tail returns the last maxEntries from the accumulated entries.
func (t *logTailer) tail(maxEntries int) []string {
	if len(t.entries) <= maxEntries {
		// Return a copy to avoid aliasing.
		out := make([]string, len(t.entries))
		copy(out, t.entries)
		return out
	}
	out := make([]string, maxEntries)
	copy(out, t.entries[len(t.entries)-maxEntries:])
	return out
}

// parseLines processes new raw JSON lines and appends activity entries.
// This mirrors the logic in parseWorkerActivity but operates incrementally.
func (t *logTailer) parseLines(lines []string) {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		t.lastRawLine = line

		var event struct {
			Type    string          `json:"type"`
			Subtype string          `json:"subtype,omitempty"`
			Message json.RawMessage `json:"message,omitempty"`
			Content string          `json:"content,omitempty"`
			Role    string          `json:"role,omitempty"`
			Status  string          `json:"status,omitempty"`
			// Gemini top-level tool fields
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
					idx := len(t.entries)
					t.entries = append(t.entries, formatToolCall(block.Name, block.Input))
					if block.ID != "" {
						t.toolIndex[block.ID] = idx
						t.toolNames[block.ID] = block.Name
					}
				case "text":
					t.entries = append(t.entries, formatMultiLineEntry("[text] ", "       ", block.Text, 20)...)
				case "thinking":
					t.entries = append(t.entries, formatMultiLineEntry("[think] ", "        ", block.Thinking, 20)...)
				}
			}

		case "user":
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
					t.enrichToolEntry(block.ToolUseID, block.Content, block.IsError)
				}
			}

		case "message":
			// Gemini-style delta — accumulate fragments.
			if event.Role == "assistant" && event.Content != "" {
				t.geminiTextBuf.WriteString(event.Content)
			}

		case "tool_use":
			t.flushGeminiText()
			name := event.ToolName
			if name == "" {
				name = "unknown"
			}
			idx := len(t.entries)
			t.entries = append(t.entries, formatToolCall(name, event.Parameters))
			if event.ToolID != "" {
				t.toolIndex[event.ToolID] = idx
				t.toolNames[event.ToolID] = name
			}

		case "tool_result":
			t.flushGeminiText()
			if event.ToolID != "" {
				t.enrichToolEntry(event.ToolID, event.Output, false)
			}

		case "rate_limit_event":
			if event.RateLimitInfo != nil && event.RateLimitInfo.Status != "" {
				t.entries = append(t.entries, fmt.Sprintf("[rate] %s", event.RateLimitInfo.Status))
			}

		case "result":
			t.flushGeminiText()
			subtype := event.Subtype
			if subtype == "" {
				subtype = "done"
			}
			t.entries = append(t.entries, fmt.Sprintf("[result] %s", subtype))
		}
	}

	// Flush any remaining Gemini text at the end of this batch.
	t.flushGeminiText()
}

// flushGeminiText writes buffered Gemini delta text as a [text] entry.
func (t *logTailer) flushGeminiText() {
	if t.geminiTextBuf.Len() == 0 {
		return
	}
	raw := t.geminiTextBuf.String()
	t.geminiTextBuf.Reset()
	t.entries = append(t.entries, formatMultiLineEntry("[text] ", "       ", raw, 20)...)
}

// enrichToolEntry correlates a tool_result back to its tool_use entry
// and appends a status suffix.
func (t *logTailer) enrichToolEntry(toolUseID, content string, isError bool) {
	idx, ok := t.toolIndex[toolUseID]
	if !ok {
		return
	}
	name := t.toolNames[toolUseID]
	suffix := toolResultEnrichment(name, content, isError)
	if suffix != "" && idx < len(t.entries) {
		t.entries[idx] += suffix
	}
	delete(t.toolIndex, toolUseID)
	delete(t.toolNames, toolUseID)
}

// compact trims the entries slice to retentionCap and adjusts toolIndex
// so that correlation still works after discarding old entries. Stale
// toolIndex entries (pointing to discarded indices) are evicted.
func (t *logTailer) compact() {
	if len(t.entries) <= retentionCap {
		// Even within cap, prune toolIndex if it grew too large.
		if len(t.toolIndex) > maxPendingTools {
			t.pruneOldestTools()
		}
		return
	}

	discard := len(t.entries) - retentionCap

	// Keep only the most recent entries.
	kept := make([]string, retentionCap)
	copy(kept, t.entries[discard:])
	t.entries = kept

	// Adjust or remove toolIndex entries.
	for id, idx := range t.toolIndex {
		if idx < discard {
			// This tool_use was discarded — remove correlation.
			delete(t.toolIndex, id)
			delete(t.toolNames, id)
		} else {
			t.toolIndex[id] = idx - discard
		}
	}

	// Final safety: prune if still too many pending tools.
	if len(t.toolIndex) > maxPendingTools {
		t.pruneOldestTools()
	}
}

// pruneOldestTools removes the oldest pending tool entries until len(toolIndex) <= maxPendingTools.
func (t *logTailer) pruneOldestTools() {
	if len(t.toolIndex) <= maxPendingTools {
		return
	}
	// Collect and sort tool IDs by their entry index (ascending = oldest first).
	type toolEntry struct {
		id  string
		idx int
	}
	pending := make([]toolEntry, 0, len(t.toolIndex))
	for id, idx := range t.toolIndex {
		pending = append(pending, toolEntry{id, idx})
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].idx < pending[j].idx })
	// Evict oldest entries until we are within the cap.
	for i := 0; i < len(pending) && len(t.toolIndex) > maxPendingTools; i++ {
		delete(t.toolIndex, pending[i].id)
		delete(t.toolNames, pending[i].id)
	}
}
