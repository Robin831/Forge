package hearth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helper to write a JSON log line for an assistant text message.
func assistantTextLine(text string) string {
	msg := map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	}
	msgBytes, _ := json.Marshal(msg)
	line, _ := json.Marshal(map[string]any{
		"type":    "assistant",
		"message": json.RawMessage(msgBytes),
	})
	return string(line)
}

// helper to write a JSON log line for a tool_use inside an assistant message.
func assistantToolUseLine(id, name string, params map[string]any) string {
	paramBytes, _ := json.Marshal(params)
	msg := map[string]any{
		"content": []map[string]any{
			{"type": "tool_use", "id": id, "name": name, "input": json.RawMessage(paramBytes)},
		},
	}
	msgBytes, _ := json.Marshal(msg)
	line, _ := json.Marshal(map[string]any{
		"type":    "assistant",
		"message": json.RawMessage(msgBytes),
	})
	return string(line)
}

// helper for a user tool_result line.
func userToolResultLine(toolUseID, content string, isError bool) string {
	msg := map[string]any{
		"content": []map[string]any{
			{"type": "tool_result", "tool_use_id": toolUseID, "content": content, "is_error": isError},
		},
	}
	msgBytes, _ := json.Marshal(msg)
	line, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": json.RawMessage(msgBytes),
	})
	return string(line)
}

// helper for a result line.
func resultLine(subtype string) string {
	line, _ := json.Marshal(map[string]any{
		"type":    "result",
		"subtype": subtype,
	})
	return string(line)
}

func TestLogTailerCache_BasicIncremental(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	// Write initial lines.
	lines := []string{
		assistantTextLine("hello world"),
		assistantTextLine("second line"),
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := NewLogTailerCache()
	entries, lastRaw := cache.ReadIncremental(logPath, 100)

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(entries), entries)
	}
	if !strings.Contains(entries[0], "hello world") {
		t.Errorf("first entry should contain 'hello world': %s", entries[0])
	}
	if lastRaw == "" {
		t.Error("lastRaw should not be empty")
	}

	// Append more data.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, assistantTextLine("third line"))
	f.Close()

	entries, _ = cache.ReadIncremental(logPath, 100)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries after append, got %d", len(entries))
	}
	if !strings.Contains(entries[2], "third line") {
		t.Errorf("third entry should contain 'third line': %s", entries[2])
	}
}

func TestLogTailerCache_PartialLine(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	// Write a line without trailing newline (partial).
	content := assistantTextLine("partial")
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := NewLogTailerCache()
	entries, _ := cache.ReadIncremental(logPath, 100)
	// Partial line should be buffered, not parsed yet.
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries for partial line, got %d", len(entries))
	}

	// Complete the line.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, "") // just a newline to complete the line
	f.Close()

	entries, _ = cache.ReadIncremental(logPath, 100)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after completing line, got %d", len(entries))
	}
}

func TestLogTailerCache_TruncatedFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	// Write some lines.
	lines := []string{
		assistantTextLine("line 1"),
		assistantTextLine("line 2"),
		assistantTextLine("line 3"),
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := NewLogTailerCache()
	entries, _ := cache.ReadIncremental(logPath, 100)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	// Truncate file (simulates log rotation).
	if err := os.WriteFile(logPath, []byte(assistantTextLine("fresh")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, _ = cache.ReadIncremental(logPath, 100)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after truncation, got %d", len(entries))
	}
	if !strings.Contains(entries[0], "fresh") {
		t.Errorf("entry should contain 'fresh': %s", entries[0])
	}
}

func TestLogTailerCache_ToolCorrelation(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	lines := []string{
		assistantToolUseLine("tu-1", "Bash", map[string]any{"command": "ls"}),
		userToolResultLine("tu-1", "file1\nfile2\nfile3", false),
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := NewLogTailerCache()
	entries, _ := cache.ReadIncremental(logPath, 100)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (tool_use enriched by result), got %d: %v", len(entries), entries)
	}
	// Should contain the tool call with enrichment suffix.
	if !strings.Contains(entries[0], "Bash") {
		t.Errorf("entry should contain 'Bash': %s", entries[0])
	}
	if !strings.Contains(entries[0], "3 lines") {
		t.Errorf("entry should be enriched with '3 lines': %s", entries[0])
	}
}

func TestLogTailerCache_ToolCorrelationAcrossReads(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	// First write: tool_use only.
	if err := os.WriteFile(logPath, []byte(assistantToolUseLine("tu-2", "Read", map[string]any{"file_path": "/tmp/foo.go"})+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := NewLogTailerCache()
	entries, _ := cache.ReadIncremental(logPath, 100)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	// Second write: tool_result arrives.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, userToolResultLine("tu-2", "file contents here", false))
	f.Close()

	entries, _ = cache.ReadIncremental(logPath, 100)
	// Read tool results don't add a suffix (implicit success), so still 1 entry.
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
}

func TestLogTailerCache_MaxEntriesTail(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	var lines []string
	for i := range 20 {
		lines = append(lines, assistantTextLine(fmt.Sprintf("line %d", i)))
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := NewLogTailerCache()
	entries, _ := cache.ReadIncremental(logPath, 5)
	if len(entries) != 5 {
		t.Fatalf("expected 5 entries (maxEntries), got %d", len(entries))
	}
	// Should be the last 5.
	if !strings.Contains(entries[0], "line 15") {
		t.Errorf("first returned entry should be 'line 15': %s", entries[0])
	}
	if !strings.Contains(entries[4], "line 19") {
		t.Errorf("last returned entry should be 'line 19': %s", entries[4])
	}
}

func TestLogTailerCache_Prune(t *testing.T) {
	cache := NewLogTailerCache()
	cache.mu.Lock()
	cache.tailers["a.log"] = &logTailer{toolIndex: make(map[string]int), toolNames: make(map[string]string)}
	cache.tailers["b.log"] = &logTailer{toolIndex: make(map[string]int), toolNames: make(map[string]string)}
	cache.tailers["c.log"] = &logTailer{toolIndex: make(map[string]int), toolNames: make(map[string]string)}
	cache.mu.Unlock()

	cache.Prune(map[string]bool{"b.log": true})

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.tailers) != 1 {
		t.Fatalf("expected 1 tailer after prune, got %d", len(cache.tailers))
	}
	if cache.tailers["b.log"] == nil {
		t.Error("expected b.log to survive prune")
	}
}

func TestLogTailerCache_EmptyAndInvalidInputs(t *testing.T) {
	cache := NewLogTailerCache()

	// Empty path.
	entries, lastRaw := cache.ReadIncremental("", 100)
	if entries != nil || lastRaw != "" {
		t.Error("empty path should return nil, empty string")
	}

	// Zero maxEntries.
	entries, lastRaw = cache.ReadIncremental("/nonexistent", 0)
	if entries != nil || lastRaw != "" {
		t.Error("zero maxEntries should return nil, empty string")
	}

	// Nonexistent file.
	entries, lastRaw = cache.ReadIncremental("/nonexistent/path/log.json", 100)
	if len(entries) != 0 {
		t.Errorf("nonexistent file should return empty entries, got %d", len(entries))
	}
}

func TestLogTailer_Compact(t *testing.T) {
	tailer := &logTailer{
		toolIndex: make(map[string]int),
		toolNames: make(map[string]string),
	}

	// Add more than retentionCap entries.
	for i := range retentionCap + 200 {
		tailer.entries = append(tailer.entries, fmt.Sprintf("entry-%d", i))
	}

	// Add tool correlation entries: some in the discard range, some in the keep range.
	tailer.toolIndex["old-tool"] = 10    // will be discarded
	tailer.toolNames["old-tool"] = "Bash"
	tailer.toolIndex["new-tool"] = retentionCap + 100 // will be kept, adjusted
	tailer.toolNames["new-tool"] = "Read"

	tailer.compact()

	if len(tailer.entries) != retentionCap {
		t.Fatalf("expected %d entries after compact, got %d", retentionCap, len(tailer.entries))
	}

	// Old tool should be evicted.
	if _, ok := tailer.toolIndex["old-tool"]; ok {
		t.Error("old-tool should have been evicted from toolIndex")
	}
	if _, ok := tailer.toolNames["old-tool"]; ok {
		t.Error("old-tool should have been evicted from toolNames")
	}

	// New tool should be adjusted.
	newIdx, ok := tailer.toolIndex["new-tool"]
	if !ok {
		t.Fatal("new-tool should still be in toolIndex")
	}
	expectedIdx := retentionCap + 100 - 200 // discard = (retentionCap+200) - retentionCap = 200
	if newIdx != expectedIdx {
		t.Errorf("expected adjusted index %d, got %d", expectedIdx, newIdx)
	}

	// Verify first entry is the one that survived compaction.
	expected := fmt.Sprintf("entry-%d", 200) // first 200 were discarded
	if tailer.entries[0] != expected {
		t.Errorf("expected first entry %q, got %q", expected, tailer.entries[0])
	}
}

func TestLogTailer_CompactPrunesStalePendingTools(t *testing.T) {
	tailer := &logTailer{
		toolIndex: make(map[string]int),
		toolNames: make(map[string]string),
	}

	// Add entries below retentionCap but add many pending tools.
	for i := range 100 {
		tailer.entries = append(tailer.entries, fmt.Sprintf("entry-%d", i))
	}

	// Add more than maxPendingTools tool entries.
	for i := range maxPendingTools + 50 {
		id := fmt.Sprintf("tool-%d", i)
		tailer.toolIndex[id] = i % 100
		tailer.toolNames[id] = "Bash"
	}

	tailer.compact()

	if len(tailer.toolIndex) > maxPendingTools {
		t.Errorf("expected toolIndex to be pruned to <= %d, got %d", maxPendingTools, len(tailer.toolIndex))
	}
	if len(tailer.toolNames) != len(tailer.toolIndex) {
		t.Error("toolNames and toolIndex should have same length after prune")
	}
}

func TestLogTailerCache_ResultLine(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	lines := []string{
		assistantTextLine("working..."),
		resultLine("success"),
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := NewLogTailerCache()
	entries, _ := cache.ReadIncremental(logPath, 100)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(entries), entries)
	}
	if entries[1] != "[result] success" {
		t.Errorf("expected '[result] success', got %q", entries[1])
	}
}

func TestLogTailerCache_NoNewData(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	if err := os.WriteFile(logPath, []byte(assistantTextLine("hello")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := NewLogTailerCache()
	entries1, _ := cache.ReadIncremental(logPath, 100)

	// Read again without changes — should return same data.
	entries2, _ := cache.ReadIncremental(logPath, 100)
	if len(entries1) != len(entries2) {
		t.Fatalf("repeated read should return same count: %d vs %d", len(entries1), len(entries2))
	}
}

func TestLogTailerCache_GeminiDelta(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	lines := []string{
		`{"type":"message","role":"assistant","content":"Hello "}`,
		`{"type":"message","role":"assistant","content":"world!"}`,
		`{"type":"tool_use","tool_name":"Bash","tool_id":"g1","parameters":{"command":"echo hi"}}`,
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := NewLogTailerCache()
	entries, _ := cache.ReadIncremental(logPath, 100)
	// Gemini text should be flushed before tool_use, producing a [text] entry + a [tool] entry.
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 entries, got %d: %v", len(entries), entries)
	}
	if !strings.Contains(entries[0], "Hello") || !strings.Contains(entries[0], "world") {
		t.Errorf("first entry should contain accumulated Gemini text: %s", entries[0])
	}
}

func TestLogTailerCache_ToolError(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	lines := []string{
		assistantToolUseLine("err-1", "Bash", map[string]any{"command": "false"}),
		userToolResultLine("err-1", "command failed", true),
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := NewLogTailerCache()
	entries, _ := cache.ReadIncremental(logPath, 100)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if !strings.Contains(entries[0], "✗") {
		t.Errorf("error tool result should contain ✗: %s", entries[0])
	}
}
