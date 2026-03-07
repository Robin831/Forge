package hearth

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/state"
)

func TestRenderWorkerListShowsTitle(t *testing.T) {
	m := Model{
		workers: []WorkerItem{
			{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
				Title: "add user auth endpoint"},
		},
		focused: PanelWorkers,
	}
	rendered := m.renderWorkerList(60, 20)
	if !strings.Contains(rendered, "add user auth endpoint") {
		t.Errorf("expected title 'add user auth endpoint' in rendered output:\n%s", rendered)
	}
}

func TestRenderWorkerListNoTitle(t *testing.T) {
	m := Model{
		workers: []WorkerItem{
			{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
				Title: ""},
		},
		focused: PanelWorkers,
	}
	rendered := m.renderWorkerList(60, 20)
	if !strings.Contains(rendered, "(no title)") {
		t.Errorf("expected '(no title)' when Title is empty:\n%s", rendered)
	}
}

func TestRenderWorkerListScrollRespectsTwoLinesPerWorker(t *testing.T) {
	// Build more workers than fit in the panel to verify scroll/clipping.
	// height=10 → maxLines = 10-4 = 6, slotsPerWorker=2, maxWorkers=3.
	// Only the first 3 workers should be visible at scroll=0.
	workers := []WorkerItem{
		{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-0"},
		{ID: "w2", BeadID: "bd-2", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-1"},
		{ID: "w3", BeadID: "bd-3", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-2"},
		{ID: "w4", BeadID: "bd-4", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-3"},
		{ID: "w5", BeadID: "bd-5", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-4"},
	}
	m := Model{workers: workers, focused: PanelWorkers, workerScroll: 0}

	rendered := m.renderWorkerList(60, 10)

	// Workers 0-2 should be visible; worker 3 and 4 must be clipped.
	for _, visible := range []string{"title-0", "title-1", "title-2"} {
		if !strings.Contains(rendered, visible) {
			t.Errorf("expected %q to be visible in rendered output:\n%s", visible, rendered)
		}
	}
	for _, hidden := range []string{"title-3", "title-4"} {
		if strings.Contains(rendered, hidden) {
			t.Errorf("expected %q to be clipped (not visible) in rendered output:\n%s", hidden, rendered)
		}
	}
}

func TestSanitizeTitle(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"plain title", "plain title"},
		{"line\none", "line one"},
		{"line\r\none", "line  one"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"tab\there", "tabhere"}, // \t is a control char
		{"", ""},
	}
	for _, tt := range tests {
		got := sanitizeTitle(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeTitle(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestWordWrapCount(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		want     int
	}{
		{"short text", "hello world", 20, 1},
		{"simple wrap", "hello world", 5, 2},
		{"wrap at space", "this is a test", 7, 2},
		{"force wrap no spaces", "alongwordthatmustwrap", 10, 3},
		{"newlines respected", "line one\nline two", 20, 2},
		{"empty input", "", 10, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wordWrapCount(tt.input, tt.maxWidth)
			want := len(wordWrap(tt.input, tt.maxWidth))
			if got != want {
				t.Errorf("wordWrapCount() = %d, wordWrap produced %d lines (expected %d)", got, want, tt.want)
			}
			if got != tt.want {
				t.Errorf("wordWrapCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEventLineCountMatchesRender(t *testing.T) {
	m := Model{eventScroll: 0}
	events := []EventItem{
		{Timestamp: "12:00:00", Type: "bead_claimed", Message: "short message", BeadID: "bd-1"},
		{Timestamp: "12:00:01", Type: "smith_done", Message: "a longer message that might wrap depending on the panel width", BeadID: ""},
		{Timestamp: "12:00:02", Type: "warden_pass", Message: "", BeadID: "bd-2"},
	}

	for _, width := range []int{40, 80, 120} {
		for _, ev := range events {
			rendered := m.renderEventLines(ev, false, width)
			counted := m.eventLineCount(ev, width)
			if len(rendered) != counted {
				t.Errorf("width=%d event=%q: renderEventLines=%d lines, eventLineCount=%d",
					width, ev.Message, len(rendered), counted)
			}
		}
	}
}

func TestRenderWorkersDoesNotExceedLeftColumn(t *testing.T) {
	m := Model{
		workers: []WorkerItem{
			{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
				ActivityLines: []string{"line1", "line2", "line3", "line4", "line5", "line6", "line7", "line8"}},
		},
		focused: PanelWorkers,
		width:   120,
		height:  40,
	}

	contentHeight := 30
	width := 40

	leftColumn := m.renderLeftColumn(width, contentHeight)
	workerPanel := m.renderWorkers(width, contentHeight)

	leftLines := strings.Count(strings.TrimRight(leftColumn, "\n"), "\n") + 1
	workerLines := strings.Count(strings.TrimRight(workerPanel, "\n"), "\n") + 1

	if workerLines != leftLines {
		t.Errorf("renderWorkers produced %d lines, expected same as renderLeftColumn's %d lines (contentHeight=%d)",
			workerLines, leftLines, contentHeight)
	}
}

func TestRenderNeedsAttentionShowsItems(t *testing.T) {
	m := Model{
		needsAttention: []NeedsAttentionItem{
			{BeadID: "bd-42", Anvil: "heimdall", Reason: "exhausted retries", ReasonCategory: AttentionDispatchExhausted},
			{BeadID: "bd-99", Anvil: "metadata", Reason: "clarification needed", ReasonCategory: AttentionClarification},
		},
		focused: PanelNeedsAttention,
	}
	rendered := m.renderNeedsAttention(60, 20)
	if !strings.Contains(rendered, "bd-42") {
		t.Errorf("expected bd-42 in rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "exhausted retries") {
		t.Errorf("expected reason 'exhausted retries' in rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Needs Attention (2)") {
		t.Errorf("expected 'Needs Attention (2)' title in rendered output:\n%s", rendered)
	}
	// Verify category-specific labels appear
	if !strings.Contains(rendered, "DISPATCH") {
		t.Errorf("expected DISPATCH label for dispatch-exhausted item:\n%s", rendered)
	}
	if !strings.Contains(rendered, "CLARIFY") {
		t.Errorf("expected CLARIFY label for clarification item:\n%s", rendered)
	}
}

func TestRenderNeedsAttentionReasonCategories(t *testing.T) {
	tests := []struct {
		name     string
		category AttentionReason
		wantText string
	}{
		{"dispatch exhausted", AttentionDispatchExhausted, "DISPATCH"},
		{"CI fix exhausted", AttentionCIFixExhausted, "CI FIX"},
		{"review fix exhausted", AttentionReviewFixExhausted, "REVIEW"},
		{"rebase exhausted", AttentionRebaseExhausted, "REBASE"},
		{"clarification", AttentionClarification, "CLARIFY"},
		{"stalled", AttentionStalled, "STALLED"},
		{"unknown", AttentionUnknown, "UNKNOWN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Model{
				needsAttention: []NeedsAttentionItem{
					{BeadID: "bd-1", Anvil: "test", Reason: "test reason", ReasonCategory: tt.category},
				},
				focused: PanelNeedsAttention,
			}
			rendered := m.renderNeedsAttention(80, 20)
			if !strings.Contains(rendered, tt.wantText) {
				t.Errorf("expected %q label in rendered output for category %d:\n%s", tt.wantText, tt.category, rendered)
			}
		})
	}
}

func TestRenderNeedsAttentionEmpty(t *testing.T) {
	m := Model{
		needsAttention: nil,
		focused:        PanelNeedsAttention,
	}
	rendered := m.renderNeedsAttention(60, 20)
	if !strings.Contains(rendered, "None") {
		t.Errorf("expected 'None' when no items:\n%s", rendered)
	}
}

func TestRenderWorkerActivityNewestFirst(t *testing.T) {
	m := Model{
		workers: []WorkerItem{
			{
				ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
				ActivityLines: []string{"oldest entry", "middle entry", "newest entry"},
			},
		},
		workerScroll: 0,
	}

	// Use a large height so all lines are visible
	rendered := m.renderWorkerActivity(80, 20)

	oldestIdx := strings.Index(rendered, "oldest entry")
	newestIdx := strings.Index(rendered, "newest entry")
	if oldestIdx == -1 || newestIdx == -1 {
		t.Fatal("expected both oldest and newest entries in rendered output")
	}
	if newestIdx >= oldestIdx {
		t.Errorf("newest entry (pos %d) should appear before oldest entry (pos %d) in rendered output", newestIdx, oldestIdx)
	}
}

func TestParseWorkerActivityClaude(t *testing.T) {
	// Claude stream-json format: assistant events with nested content blocks
	logContent := `{"type":"system","subtype":"init","session_id":"abc"}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"Let me analyze this code"}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/tmp/test.go"}}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"I found the issue in the code."}]}}
{"type":"result","subtype":"success"}
`
	logPath := filepath.Join(t.TempDir(), "smith.log")
	if err := os.WriteFile(logPath, []byte(logContent), 0o644); err != nil {
		t.Fatal(err)
	}

	entries := parseWorkerActivity(logPath, 100)
	if len(entries) == 0 {
		t.Fatal("expected entries from Claude log, got none")
	}

	// Should have: [think], [tool], [text], [result]
	wantPrefixes := []string{"[think]", "[tool]", "[text]", "[result]"}
	if len(entries) != len(wantPrefixes) {
		t.Fatalf("got %d entries, want %d: %v", len(entries), len(wantPrefixes), entries)
	}
	for i, prefix := range wantPrefixes {
		if !strings.HasPrefix(entries[i], prefix) {
			t.Errorf("entry[%d] = %q, want prefix %q", i, entries[i], prefix)
		}
	}
}

func TestParseWorkerActivityGemini(t *testing.T) {
	// Gemini format: top-level message, tool_use, tool_result events
	logContent := `{"type":"init","timestamp":"2026-01-01T00:00:00Z","session_id":"xyz","model":"gemini-3"}
{"type":"message","timestamp":"2026-01-01T00:00:01Z","role":"user","content":"Do the task"}
{"type":"message","timestamp":"2026-01-01T00:00:02Z","role":"assistant","content":"I will ","delta":true}
{"type":"message","timestamp":"2026-01-01T00:00:03Z","role":"assistant","content":"read the file.","delta":true}
{"type":"tool_use","timestamp":"2026-01-01T00:00:04Z","tool_name":"read_file","tool_id":"t1","parameters":{"path":"/tmp/test.go"}}
{"type":"tool_result","timestamp":"2026-01-01T00:00:05Z","tool_id":"t1","output":"file contents"}
{"type":"message","timestamp":"2026-01-01T00:00:06Z","role":"assistant","content":"Done fixing it.","delta":true}
{"type":"tool_use","timestamp":"2026-01-01T00:00:07Z","tool_name":"write_file","tool_id":"t2","parameters":{"path":"/tmp/test.go","content":"fixed"}}
{"type":"tool_result","timestamp":"2026-01-01T00:00:08Z","tool_id":"t2","output":"ok"}
`
	logPath := filepath.Join(t.TempDir(), "smith.log")
	if err := os.WriteFile(logPath, []byte(logContent), 0o644); err != nil {
		t.Fatal(err)
	}

	entries := parseWorkerActivity(logPath, 100)
	if len(entries) == 0 {
		t.Fatal("expected entries from Gemini log, got none")
	}

	// Should have: [text] (accumulated deltas), [tool] read_file, [text], [tool] write_file
	wantPrefixes := []string{"[text]", "[tool] read_file", "[text]", "[tool] write_file"}
	if len(entries) != len(wantPrefixes) {
		t.Fatalf("got %d entries, want %d: %v", len(entries), len(wantPrefixes), entries)
	}
	for i, prefix := range wantPrefixes {
		if !strings.HasPrefix(entries[i], prefix) {
			t.Errorf("entry[%d] = %q, want prefix %q", i, entries[i], prefix)
		}
	}

	// Verify accumulated text: "I will read the file."
	if !strings.Contains(entries[0], "I will read the file.") {
		t.Errorf("expected accumulated text in entry[0], got %q", entries[0])
	}
}

func TestClassifyAttentionReason(t *testing.T) {
	tests := []struct {
		name string
		bead state.NeedsAttentionBead
		want AttentionReason
	}{
		{
			name: "clarification takes priority",
			bead: state.NeedsAttentionBead{ClarificationNeeded: true, NeedsHuman: true, Reason: "circuit breaker: too many failures"},
			want: AttentionClarification,
		},
		{
			name: "circuit breaker prefix",
			bead: state.NeedsAttentionBead{NeedsHuman: true, Reason: "circuit breaker: dispatch failed 5 times"},
			want: AttentionDispatchExhausted,
		},
		{
			name: "CI fix exhausted",
			bead: state.NeedsAttentionBead{Reason: "CI fix exhausted (5/5)"},
			want: AttentionCIFixExhausted,
		},
		{
			name: "review fix exhausted",
			bead: state.NeedsAttentionBead{Reason: "Review fix exhausted (3/3)"},
			want: AttentionReviewFixExhausted,
		},
		{
			name: "rebase exhausted",
			bead: state.NeedsAttentionBead{Reason: "Rebase exhausted (2/2)"},
			want: AttentionRebaseExhausted,
		},
		{
			name: "worker stalled",
			bead: state.NeedsAttentionBead{Reason: "Worker stalled (no log activity)"},
			want: AttentionStalled,
		},
		{
			name: "needs_human without circuit breaker",
			bead: state.NeedsAttentionBead{NeedsHuman: true, Reason: "max retries exceeded"},
			want: AttentionDispatchExhausted,
		},
		{
			name: "unknown reason",
			bead: state.NeedsAttentionBead{Reason: "something else entirely"},
			want: AttentionUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyAttentionReason(tt.bead)
			if got != tt.want {
				t.Errorf("classifyAttentionReason() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseWorkerActivityEmptyLogPath(t *testing.T) {
	entries := parseWorkerActivity("", 100)
	if entries != nil {
		t.Errorf("expected nil for empty log path, got %v", entries)
	}
}

func TestWordWrap(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		want     []string
	}{
		{
			name:     "short text",
			input:    "hello world",
			maxWidth: 20,
			want:     []string{"hello world"},
		},
		{
			name:     "simple wrap",
			input:    "hello world",
			maxWidth: 5,
			want:     []string{"hello", "world"},
		},
		{
			name:     "wrap at space",
			input:    "this is a test",
			maxWidth: 7,
			want:     []string{"this is", "a test"},
		},
		{
			name:     "force wrap no spaces",
			input:    "alongwordthatmustwrap",
			maxWidth: 10,
			want:     []string{"alongwordt", "hatmustwra", "p"},
		},
		{
			name:     "newlines respected",
			input:    "line one\nline two",
			maxWidth: 20,
			want:     []string{"line one", "line two"},
		},
		{
			name:     "empty input",
			input:    "",
			maxWidth: 10,
			want:     []string{""},
		},
		{
			name:     "UTF-8 safe wrap",
			input:    "hallo verden 🌍",
			maxWidth: 13,
			want:     []string{"hallo verden", "🌍"},
		},
		{
			name:     "UTF-8 multi-byte characters",
			input:    "日本語のテキスト",
			maxWidth: 5,
			want:     []string{"日本語のテ", "キスト"},
		},
		{
			name:     "UTF-8 multi-byte with spaces",
			input:    "こんにちは 世界",
			maxWidth: 5,
			want:     []string{"こんにちは", "世界"},
		},
		{
			name:     "trim leading spaces on new lines",
			input:    "word1 word2 word3",
			maxWidth: 5,
			want:     []string{"word1", "word2", "word3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wordWrap(tt.input, tt.maxWidth)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("wordWrap() = %q, want %q", got, tt.want)
			}
		})
	}
}
