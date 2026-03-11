package hearth

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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

func TestRenderWorkerListWithPRNumber(t *testing.T) {
	m := Model{
		workers: []WorkerItem{
			{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "cifix",
				Title: "fix CI", PRNumber: 42},
		},
		focused: PanelWorkers,
	}
	rendered := m.renderWorkerList(60, 20)
	if !strings.Contains(rendered, "PR#42") {
		t.Errorf("expected 'PR#42' when PRNumber > 0 in rendered output:\n%s", rendered)
	}
}

func TestRenderWorkerListWithoutPRNumber(t *testing.T) {
	m := Model{
		workers: []WorkerItem{
			{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
				Title: "some task", PRNumber: 0},
		},
		focused: PanelWorkers,
	}
	rendered := m.renderWorkerList(60, 20)
	if strings.Contains(rendered, "PR#") {
		t.Errorf("expected no 'PR#' when PRNumber is 0 in rendered output:\n%s", rendered)
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
	m := Model{workers: workers, focused: PanelWorkers, workerVP: scrollViewport{cursor: 0}}

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

func TestRenderWorkerListViewportScrollsToShowSelected(t *testing.T) {
	// height=10 → maxLines=6, slotsPerWorker=2, maxWorkers=3.
	// With workerScroll=4 (last worker), the viewport should shift so that
	// workers 2-4 are visible and workers 0-1 are clipped.
	workers := []WorkerItem{
		{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-0"},
		{ID: "w2", BeadID: "bd-2", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-1"},
		{ID: "w3", BeadID: "bd-3", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-2"},
		{ID: "w4", BeadID: "bd-4", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-3"},
		{ID: "w5", BeadID: "bd-5", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-4"},
	}
	m := Model{workers: workers, focused: PanelWorkers, workerVP: scrollViewport{cursor: 4}}

	rendered := m.renderWorkerList(60, 10)

	// The selected worker (index 4) and its neighbours must be visible.
	for _, visible := range []string{"title-2", "title-3", "title-4"} {
		if !strings.Contains(rendered, visible) {
			t.Errorf("expected %q to be visible when workerScroll=4:\n%s", visible, rendered)
		}
	}
	// Workers scrolled out of view must not appear.
	for _, hidden := range []string{"title-0", "title-1"} {
		if strings.Contains(rendered, hidden) {
			t.Errorf("expected %q to be clipped (not visible) when workerScroll=4:\n%s", hidden, rendered)
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

func TestRenderColumnsAlignedHeight(t *testing.T) {
	m := Model{
		workers: []WorkerItem{
			{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
				ActivityLines: []string{"line1", "line2", "line3", "line4", "line5", "line6", "line7", "line8"}},
		},
		focused:          PanelWorkers,
		width:            120,
		height:           40,
		activityExpanded: make(map[string]bool),
	}
	m.rebuildActivityNav()

	topHeight, bottomHeight := m.getVerticalSplit(1, 1)
	width := 40

	leftColumn := m.renderLeftColumn(width, topHeight, bottomHeight)
	centerColumn := m.renderCenterColumn(width, topHeight, bottomHeight)
	rightColumn := m.renderRightColumn(width, topHeight, bottomHeight)

	leftLines := strings.Count(strings.TrimRight(leftColumn, "\n"), "\n") + 1
	centerLines := strings.Count(strings.TrimRight(centerColumn, "\n"), "\n") + 1
	rightLines := strings.Count(strings.TrimRight(rightColumn, "\n"), "\n") + 1

	if leftLines != centerLines {
		t.Errorf("left column produced %d lines, center produced %d lines — should match", leftLines, centerLines)
	}
	if rightLines != centerLines {
		t.Errorf("right column produced %d lines, center produced %d lines — should match", rightLines, centerLines)
	}
}

func TestRenderUsagePanelNoData(t *testing.T) {
	m := Model{focused: PanelWorkers}
	rendered := m.renderUsagePanel(60, 8)
	if !strings.Contains(rendered, "Usage") {
		t.Errorf("expected 'Usage' title in rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "No usage today") {
		t.Errorf("expected 'No usage today' when no data:\n%s", rendered)
	}
}

func TestRenderUsagePanelWithData(t *testing.T) {
	m := Model{
		focused: PanelWorkers,
		usage: UsageData{
			Providers: []ProviderUsage{
				{Provider: "claude", Cost: 0.05, InputTokens: 1500, OutputTokens: 300},
			},
			TotalCost:   0.05,
			CostLimit:   1.0,
			CopilotUsed: 5,
			CopilotLimit: 50,
		},
	}
	rendered := m.renderUsagePanel(60, 8)
	if !strings.Contains(rendered, "Claude") {
		t.Errorf("expected provider 'Claude' in rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Total") {
		t.Errorf("expected 'Total' line in rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Copilot") {
		t.Errorf("expected 'Copilot' line in rendered output:\n%s", rendered)
	}
}

func TestRenderUsagePanelZeroHeight(t *testing.T) {
	m := Model{focused: PanelWorkers}
	// Should not panic at boundary height
	rendered := m.renderUsagePanel(60, 0)
	if rendered == "" {
		t.Errorf("expected non-empty output even at height 0")
	}
}

func TestRenderCenterColumnSmallTerminal(t *testing.T) {
	m := Model{
		workers: []WorkerItem{
			{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
				Title: "some task"},
		},
		focused: PanelWorkers,
		height:  18,
	}
	// fullHeight < 20 should fall back to workers-only
	rendered := m.renderCenterColumn(60, 9, 9)
	if !strings.Contains(rendered, "Workers") {
		t.Errorf("expected 'Workers' panel in small terminal output:\n%s", rendered)
	}
	if strings.Contains(rendered, "Usage") {
		t.Errorf("expected no 'Usage' panel when terminal is too small:\n%s", rendered)
	}
}

func TestRenderCenterColumnLargeTerminal(t *testing.T) {
	m := Model{
		workers: []WorkerItem{
			{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
				Title: "some task"},
		},
		focused: PanelWorkers,
		height:  50,
	}
	// fullHeight >= 20 should show both Workers and Usage panels
	rendered := m.renderCenterColumn(60, 25, 25)
	if !strings.Contains(rendered, "Workers") {
		t.Errorf("expected 'Workers' panel in large terminal output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Usage") {
		t.Errorf("expected 'Usage' panel in large terminal output:\n%s", rendered)
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
				ActivityLines: []string{"[think] oldest entry", "[think] middle entry", "[think] newest entry"},
			},
		},
		workerVP:         scrollViewport{cursor: 0},
		activityExpanded: map[string]bool{"think": true},
	}
	m.rebuildActivityNav()

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

func TestActivityScrollChangesVisibleLine(t *testing.T) {
	lines := []string{"[think] line-0", "[think] line-1", "[think] line-2", "[think] line-3", "[think] line-4"}
	m := Model{
		workers: []WorkerItem{
			{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
				ActivityLines: lines},
		},
		workerVP:         scrollViewport{cursor: 0},
		activityExpanded: map[string]bool{"think": true},
		focused:          PanelLiveActivity,
	}
	m.rebuildActivityNav()

	// With cursor at 0, the newest entry (line-4) should be visible.
	rendered0 := m.renderWorkerActivity(80, 10)
	if !strings.Contains(rendered0, "line-4") {
		t.Errorf("expected newest entry 'line-4' visible at cursor=0:\n%s", rendered0)
	}

	// Scroll to the bottom (oldest entries) so that line-0 is visible.
	m.activityVP.cursor = len(m.activityNavItems) - 1
	renderedScrolled := m.renderWorkerActivity(80, 10)
	if !strings.Contains(renderedScrolled, "line-0") {
		t.Errorf("expected oldest entry 'line-0' visible when scrolled to end:\n%s", renderedScrolled)
	}
}

func TestActivityScrollClampPastEnd(t *testing.T) {
	lines := []string{"[think] alpha", "[think] beta", "[think] gamma"}
	// activityVP.cursor larger than total simulates a stale scroll after
	// the worker's activity list shrinks on refresh.
	m := Model{
		workers: []WorkerItem{
			{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
				ActivityLines: lines},
		},
		workerVP:         scrollViewport{cursor: 0},
		activityVP:       scrollViewport{cursor: 100}, // way past the end
		activityExpanded: map[string]bool{"think": true},
		focused:          PanelLiveActivity,
	}
	m.rebuildActivityNav()

	rendered := m.renderWorkerActivity(80, 10)
	// The panel must not be blank — at least some entry should show.
	if !strings.Contains(rendered, "alpha") && !strings.Contains(rendered, "beta") && !strings.Contains(rendered, "gamma") {
		t.Errorf("expected at least some entry when activityVP.cursor is past end:\n%s", rendered)
	}
}

func TestFormatMultiLineEntry(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		maxLines  int
		wantLen   int
		wantFirst string
		wantSecond string
	}{
		{
			name:      "single line",
			raw:       "hello world",
			maxLines:  3,
			wantLen:   1,
			wantFirst: "[text] hello world",
		},
		{
			name:      "multi-line uses continuation prefix",
			raw:       "line one\nline two\nline three",
			maxLines:  3,
			wantLen:   3,
			wantFirst: "[text] line one",
			wantSecond: "       line two",
		},
		{
			name:      "blank lines skipped",
			raw:       "first\n\n\nsecond",
			maxLines:  3,
			wantLen:   2,
			wantFirst: "[text] first",
			wantSecond: "       second",
		},
		{
			name:      "maxLines truncates output",
			raw:       "a\nb\nc\nd",
			maxLines:  2,
			wantLen:   2,
			wantFirst: "[text] a",
			wantSecond: "       b",
		},
		{
			name:     "empty raw returns nil",
			raw:      "",
			maxLines: 3,
			wantLen:  0,
		},
		{
			name:     "only blank lines returns nil",
			raw:      "\n\n\n",
			maxLines: 3,
			wantLen:  0,
		},
		{
			name:      "long line truncated by runes not bytes",
			raw:       string([]rune("日本語テキスト abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ 1234567890 end")),
			maxLines:  1,
			wantLen:   1,
			wantFirst: "[text] " + string([]rune("日本語テキスト abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ 1234567890 end")[:67]) + "...",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatMultiLineEntry("[text] ", "       ", tt.raw, tt.maxLines)
			if len(got) != tt.wantLen {
				t.Errorf("got %d lines, want %d: %v", len(got), tt.wantLen, got)
				return
			}
			if tt.wantLen > 0 && got[0] != tt.wantFirst {
				t.Errorf("first line = %q, want %q", got[0], tt.wantFirst)
			}
			if tt.wantLen > 1 && got[1] != tt.wantSecond {
				t.Errorf("second line = %q, want %q", got[1], tt.wantSecond)
			}
		})
	}
}

func TestGroupActivityLines(t *testing.T) {
	t.Run("empty input returns nil", func(t *testing.T) {
		got := groupActivityLines(nil)
		if got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("single group is returned unchanged", func(t *testing.T) {
		lines := []string{"[tool] Read x", "[tool] Edit y"}
		got := groupActivityLines(lines)
		if !reflect.DeepEqual(got, lines) {
			t.Errorf("got %v, want %v", got, lines)
		}
	})

	t.Run("last group is expanded, older groups collapsed", func(t *testing.T) {
		lines := []string{
			"[tool] Read x",
			"[tool] Edit y",
			"[text] some text",
			"[text] more text",
		}
		got := groupActivityLines(lines)
		// Expect: 1 collapsed tool summary + 2 expanded text lines
		if len(got) != 3 {
			t.Fatalf("got %d lines, want 3: %v", len(got), got)
		}
		if !strings.HasPrefix(got[0], "▸ [tool] x2") {
			t.Errorf("collapsed summary = %q, want prefix '▸ [tool] x2'", got[0])
		}
		if got[1] != "[text] some text" {
			t.Errorf("got[1] = %q, want '[text] some text'", got[1])
		}
		if got[2] != "[text] more text" {
			t.Errorf("got[2] = %q, want '[text] more text'", got[2])
		}
	})

	t.Run("continuation lines stay with their group", func(t *testing.T) {
		lines := []string{
			"[text] first line",
			"       continuation",
			"[tool] Read x",
		}
		got := groupActivityLines(lines)
		// text group (with continuation) is collapsed; tool group expanded.
		if len(got) != 2 {
			t.Fatalf("got %d lines, want 2: %v", len(got), got)
		}
		if !strings.HasPrefix(got[0], "▸ [text] x1") {
			t.Errorf("collapsed summary = %q, want prefix '▸ [text] x1'", got[0])
		}
		if got[1] != "[tool] Read x" {
			t.Errorf("got[1] = %q, want '[tool] Read x'", got[1])
		}
	})
}

func TestCollapseActivityGroup(t *testing.T) {
	t.Run("tool group extracts names", func(t *testing.T) {
		g := activityGroup{
			eventType: "tool",
			lines: []string{
				"[tool] Read /tmp/foo",
				"[tool] Edit /tmp/bar",
				"[tool] Grep pattern",
			},
		}
		got := collapseActivityGroup(g)
		want := "▸ [tool] x3 — Read, Edit, Grep"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("tool group deduplicates consecutive names", func(t *testing.T) {
		g := activityGroup{
			eventType: "tool",
			lines: []string{
				"[tool] Read /a",
				"[tool] Read /b",
				"[tool] Edit /c",
			},
		}
		got := collapseActivityGroup(g)
		want := "▸ [tool] x3 — Read, Edit"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("non-tool group no names", func(t *testing.T) {
		g := activityGroup{
			eventType: "text",
			lines:     []string{"[text] hello", "[text] world"},
		}
		got := collapseActivityGroup(g)
		want := "▸ [text] x2"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("continuation lines not counted as entries", func(t *testing.T) {
		g := activityGroup{
			eventType: "text",
			lines:     []string{"[text] hello", "       more text"},
		}
		got := collapseActivityGroup(g)
		want := "▸ [text] x1"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("result is plain text without ANSI escapes", func(t *testing.T) {
		g := activityGroup{
			eventType: "tool",
			lines:     []string{"[tool] Read /x"},
		}
		got := collapseActivityGroup(g)
		// Plain text should contain no ESC bytes (no ANSI escapes).
		if strings.ContainsRune(got, '\x1b') {
			t.Errorf("collapseActivityGroup returned ANSI-styled string; want plain text: %q", got)
		}
	})
}

func TestParseWorkerActivityMultiLineText(t *testing.T) {
	// Text and thinking blocks with embedded newlines should produce
	// continuation-indented entries (up to 3 lines each).
	logContent := `{"type":"system","subtype":"init","session_id":"ml"}
{"type":"assistant","message":{"content":[{"type":"text","text":"First line\nSecond line\nThird line\nFourth line (should be dropped)"}]}}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"Think line 1\nThink line 2"}]}}
{"type":"result","subtype":"success"}
`
	logPath := filepath.Join(t.TempDir(), "smith.log")
	if err := os.WriteFile(logPath, []byte(logContent), 0o644); err != nil {
		t.Fatal(err)
	}

	entries := parseWorkerActivity(logPath, 100)

	// Expect: 3 text lines + 2 think lines + 1 result line = 6
	if len(entries) != 6 {
		t.Fatalf("got %d entries, want 6: %v", len(entries), entries)
	}
	if !strings.HasPrefix(entries[0], "[text] ") {
		t.Errorf("entries[0] = %q, want '[text] ' prefix", entries[0])
	}
	if !strings.HasPrefix(entries[1], "       ") {
		t.Errorf("entries[1] = %q, want continuation indent", entries[1])
	}
	if !strings.HasPrefix(entries[2], "       ") {
		t.Errorf("entries[2] = %q, want continuation indent", entries[2])
	}
	// Fourth line should have been dropped (maxLines=3).
	for _, e := range entries {
		if strings.Contains(e, "Fourth") {
			t.Errorf("fourth line should have been dropped, but found in entries: %v", entries)
		}
	}
	if !strings.HasPrefix(entries[3], "[think] ") {
		t.Errorf("entries[3] = %q, want '[think] ' prefix", entries[3])
	}
	if !strings.HasPrefix(entries[4], "        ") {
		t.Errorf("entries[4] = %q, want think continuation indent", entries[4])
	}
}

func TestEnterOnUnlabeledQueueItemOpensMenu(t *testing.T) {
	m := Model{
		focused: PanelQueue,
		queue: []QueueItem{
			{BeadID: "bd-1", Anvil: "test", Section: "unlabeled"},
		},
		queueVP: scrollViewport{cursor: 0},
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.showQueueActionMenu {
		t.Error("expected showQueueActionMenu=true after Enter on unlabeled item")
	}
	if m.queueActionTarget == nil || m.queueActionTarget.BeadID != "bd-1" {
		t.Errorf("expected queueActionTarget.BeadID=bd-1, got %v", m.queueActionTarget)
	}
}

func TestEnterOnReadyQueueItemDoesNotOpenMenu(t *testing.T) {
	m := Model{
		focused: PanelQueue,
		queue: []QueueItem{
			{BeadID: "bd-2", Anvil: "test", Section: "ready"},
		},
		queueVP: scrollViewport{cursor: 0},
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.showQueueActionMenu {
		t.Error("expected showQueueActionMenu=false for ready (non-unlabeled) item")
	}
}

func TestQueueActionMenuLabelCallsOnTagBead(t *testing.T) {
	var taggedBead, taggedAnvil string
	m := Model{
		focused: PanelQueue,
		queue: []QueueItem{
			{BeadID: "bd-3", Anvil: "forge", Section: "unlabeled"},
		},
		queueVP: scrollViewport{cursor: 0},
		OnTagBead: func(beadID, anvil string) error {
			taggedBead = beadID
			taggedAnvil = anvil
			return nil
		},
	}
	// Open the menu
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.showQueueActionMenu {
		t.Fatal("expected menu open after Enter")
	}
	// Select the label action — menu closes immediately, returns async cmd
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.showQueueActionMenu {
		t.Error("expected menu to close immediately after label action")
	}
	// Execute the async command and deliver result
	if cmd == nil {
		t.Fatal("expected a tea.Cmd for async tag operation")
	}
	msg := cmd()
	_, _ = m.Update(msg)
	if taggedBead != "bd-3" || taggedAnvil != "forge" {
		t.Errorf("OnTagBead called with (%q, %q), want (bd-3, forge)", taggedBead, taggedAnvil)
	}
	if !strings.Contains(m.statusMsg, "bd-3") {
		t.Errorf("expected statusMsg to mention bd-3, got %q", m.statusMsg)
	}
}

func TestQueueActionMenuLabelOnTagBeadError(t *testing.T) {
	m := Model{
		focused: PanelQueue,
		queue: []QueueItem{
			{BeadID: "bd-4", Anvil: "forge", Section: "unlabeled"},
		},
		queueVP: scrollViewport{cursor: 0},
		OnTagBead: func(beadID, anvil string) error {
			return errors.New("network error")
		},
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a tea.Cmd for async tag operation")
	}
	msg := cmd()
	_, _ = m.Update(msg)
	if !strings.Contains(m.statusMsg, "Failed to tag") {
		t.Errorf("expected failure statusMsg, got %q", m.statusMsg)
	}
}

func TestQueueActionMenuCloseCallsOnCloseBead(t *testing.T) {
	var closedBead, closedAnvil string
	m := Model{
		focused: PanelQueue,
		queue: []QueueItem{
			{BeadID: "bd-close-1", Anvil: "forge", Section: "unlabeled"},
		},
		queueVP: scrollViewport{cursor: 0},
		OnCloseBead: func(beadID, anvil string) error {
			closedBead = beadID
			closedAnvil = anvil
			return nil
		},
	}
	// Open the menu
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.showQueueActionMenu {
		t.Fatal("expected menu open after Enter")
	}
	// Navigate to "Close" (index 1) and select it
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.showQueueActionMenu {
		t.Error("expected menu to close immediately after close action")
	}
	if cmd == nil {
		t.Fatal("expected a tea.Cmd for async close operation")
	}
	msg := cmd()
	_, _ = m.Update(msg)
	if closedBead != "bd-close-1" || closedAnvil != "forge" {
		t.Errorf("OnCloseBead called with (%q, %q), want (bd-close-1, forge)", closedBead, closedAnvil)
	}
	if !strings.Contains(m.statusMsg, "bd-close-1") {
		t.Errorf("expected statusMsg to mention bd-close-1, got %q", m.statusMsg)
	}
}

func TestQueueActionMenuCloseOnCloseBeadError(t *testing.T) {
	m := Model{
		focused: PanelQueue,
		queue: []QueueItem{
			{BeadID: "bd-close-2", Anvil: "forge", Section: "unlabeled"},
		},
		queueVP: scrollViewport{cursor: 0},
		OnCloseBead: func(beadID, anvil string) error {
			return errors.New("bd close failed")
		},
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected a tea.Cmd for async close operation")
	}
	msg := cmd()
	_, _ = m.Update(msg)
	if !strings.Contains(m.statusMsg, "Failed to close") {
		t.Errorf("expected failure statusMsg, got %q", m.statusMsg)
	}
}

func TestQueueActionMenuCloseNilOnCloseBead(t *testing.T) {
	m := Model{
		focused: PanelQueue,
		queue: []QueueItem{
			{BeadID: "bd-close-3", Anvil: "forge", Section: "unlabeled"},
		},
		queueVP: scrollViewport{cursor: 0},
		// OnCloseBead intentionally nil
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.statusMsg, "unavailable") {
		t.Errorf("expected 'unavailable' statusMsg, got %q", m.statusMsg)
	}
}

func TestRenderQueueActionMenuContainsBeadID(t *testing.T) {
	item := QueueItem{BeadID: "bd-5", Anvil: "test", Section: "unlabeled"}
	m := Model{
		showQueueActionMenu: true,
		queueActionTarget:   &item,
		queueActionMenuIdx:  0,
		width:               80,
		height:              24,
	}
	rendered := m.renderQueueActionMenu()
	if !strings.Contains(rendered, "bd-5") {
		t.Errorf("expected bead ID bd-5 in renderQueueActionMenu output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Label for dispatch") {
		t.Errorf("expected 'Label for dispatch' action in menu:\n%s", rendered)
	}
}

func TestRenderQueueActionMenuShowsTitle(t *testing.T) {
	item := QueueItem{BeadID: "bd-t1", Anvil: "test", Title: "Implement feature X", Section: "unlabeled"}
	m := Model{
		showQueueActionMenu: true,
		queueActionTarget:   &item,
		queueActionMenuIdx:  0,
		width:               80,
		height:              24,
	}
	rendered := m.renderQueueActionMenu()
	if !strings.Contains(rendered, "Implement feature X") {
		t.Errorf("expected title in renderQueueActionMenu output:\n%s", rendered)
	}
}

func TestRenderQueueActionMenuTruncatesLongTitle(t *testing.T) {
	// A title that wraps to more than 2 lines should show 2 lines with ellipsis on the second.
	longTitle := strings.Repeat("word ", 30) // ~150 chars, will wrap well beyond 2 lines at 60 cols
	item := QueueItem{
		BeadID:  "bd-lt1",
		Anvil:   "test",
		Title:   longTitle,
		Section: "unlabeled",
	}
	m := Model{
		showQueueActionMenu: true,
		queueActionTarget:   &item,
		queueActionMenuIdx:  0,
		width:               80,
		height:              24,
	}
	rendered := m.renderQueueActionMenu()
	// Should contain ellipsis indicating title was truncated.
	if !strings.Contains(rendered, "...") {
		t.Errorf("expected ellipsis for truncated title:\n%s", rendered)
	}
	// Should still contain the bead ID.
	if !strings.Contains(rendered, "bd-lt1") {
		t.Errorf("expected bead ID in output:\n%s", rendered)
	}
}

func TestRenderQueueActionMenuWrapsDescription(t *testing.T) {
	// Description longer than content width should be word-wrapped.
	item := QueueItem{
		BeadID:      "bd-t2",
		Anvil:       "test",
		Title:       "Test bead",
		Description: "This is a fairly long description that should be wrapped across multiple lines in the popup",
		Section:     "unlabeled",
	}
	m := Model{
		showQueueActionMenu: true,
		queueActionTarget:   &item,
		queueActionMenuIdx:  0,
		width:               80,
		height:              24,
	}
	rendered := m.renderQueueActionMenu()
	if !strings.Contains(rendered, "Test bead") {
		t.Errorf("expected title in output:\n%s", rendered)
	}
	// The description text should appear (at least partially).
	if !strings.Contains(rendered, "fairly long") {
		t.Errorf("expected description text in output:\n%s", rendered)
	}
}

func TestRenderQueueActionMenuTruncatesLongDescription(t *testing.T) {
	// Very long description should be capped at 5 lines with ellipsis on the last line.
	longDesc := strings.Repeat("word ", 100) // 500 chars
	item := QueueItem{
		BeadID:      "bd-t3",
		Anvil:       "test",
		Title:       "Test",
		Description: longDesc,
		Section:     "unlabeled",
	}
	m := Model{
		showQueueActionMenu: true,
		queueActionTarget:   &item,
		queueActionMenuIdx:  0,
		width:               80,
		height:              24,
	}
	rendered := m.renderQueueActionMenu()
	// Should contain ellipsis for truncated description.
	if !strings.Contains(rendered, "...") {
		t.Errorf("expected ellipsis for truncated description:\n%s", rendered)
	}
}

func TestUpdateQueueMsgClosesMenuWhenTargetRemoved(t *testing.T) {
	item := QueueItem{BeadID: "bd-6", Anvil: "test", Section: "unlabeled"}
	m := Model{
		showQueueActionMenu: true,
		queueActionTarget:   &item,
		queue:               []QueueItem{item},
	}
	// Simulate queue refresh that removes the target bead
	_, _ = m.Update(UpdateQueueMsg{Items: []QueueItem{
		{BeadID: "bd-99", Anvil: "test", Section: "unlabeled"},
	}})
	if m.showQueueActionMenu {
		t.Error("expected menu to close when target bead no longer in unlabeled section")
	}
	if m.queueActionTarget != nil {
		t.Error("expected queueActionTarget to be nil after menu closed")
	}
}

func TestUpdateQueueMsgKeepsMenuWhenTargetStillPresent(t *testing.T) {
	item := QueueItem{BeadID: "bd-7", Anvil: "test", Section: "unlabeled"}
	m := Model{
		showQueueActionMenu: true,
		queueActionTarget:   &item,
		queue:               []QueueItem{item},
	}
	// Simulate queue refresh that keeps the target bead
	_, _ = m.Update(UpdateQueueMsg{Items: []QueueItem{item}})
	if !m.showQueueActionMenu {
		t.Error("expected menu to remain open when target bead still in unlabeled section")
	}
}

// --- removeNeedsAttentionItem ---

func TestRemoveNeedsAttentionItem_MiddleElement(t *testing.T) {
	m := NewModel(nil)
	m.needsAttention = []NeedsAttentionItem{
		{BeadID: "a", Anvil: "repo"}, {BeadID: "b", Anvil: "repo"}, {BeadID: "c", Anvil: "repo"},
	}
	m.needsAttnVP.cursor = 1
	m.removeNeedsAttentionItem("b", "repo")
	if len(m.needsAttention) != 2 {
		t.Fatalf("expected 2 items, got %d", len(m.needsAttention))
	}
	if m.needsAttention[0].BeadID != "a" || m.needsAttention[1].BeadID != "c" {
		t.Errorf("unexpected items after removal: %v", m.needsAttention)
	}
}

func TestRemoveNeedsAttentionItem_LastElement_AdjustsScroll(t *testing.T) {
	m := NewModel(nil)
	m.needsAttention = []NeedsAttentionItem{{BeadID: "a", Anvil: "repo"}, {BeadID: "b", Anvil: "repo"}}
	m.needsAttnVP.cursor = 1 // pointing at last element "b"
	m.removeNeedsAttentionItem("b", "repo")
	if len(m.needsAttention) != 1 {
		t.Fatalf("expected 1 item, got %d", len(m.needsAttention))
	}
	if m.needsAttnVP.cursor != 0 {
		t.Errorf("expected scroll decremented to 0, got %d", m.needsAttnVP.cursor)
	}
}

func TestRemoveNeedsAttentionItem_OnlyElement_ScrollStaysZero(t *testing.T) {
	m := NewModel(nil)
	m.needsAttention = []NeedsAttentionItem{{BeadID: "only", Anvil: "repo"}}
	m.needsAttnVP.cursor = 0
	m.removeNeedsAttentionItem("only", "repo")
	if len(m.needsAttention) != 0 {
		t.Fatalf("expected 0 items, got %d", len(m.needsAttention))
	}
	if m.needsAttnVP.cursor != 0 {
		t.Errorf("expected scroll to stay 0, got %d", m.needsAttnVP.cursor)
	}
}

func TestRemoveNeedsAttentionItem_NotFound_NoChange(t *testing.T) {
	m := NewModel(nil)
	m.needsAttention = []NeedsAttentionItem{{BeadID: "a", Anvil: "repo"}, {BeadID: "b", Anvil: "repo"}}
	m.removeNeedsAttentionItem("missing", "repo")
	if len(m.needsAttention) != 2 {
		t.Fatalf("expected 2 items unchanged, got %d", len(m.needsAttention))
	}
}

func TestRemoveNeedsAttentionItem_SameBeadID_DifferentAnvils_OnlyRemovesMatching(t *testing.T) {
	m := NewModel(nil)
	m.needsAttention = []NeedsAttentionItem{
		{BeadID: "x", Anvil: "repo-a"},
		{BeadID: "x", Anvil: "repo-b"},
	}
	m.removeNeedsAttentionItem("x", "repo-a")
	if len(m.needsAttention) != 1 {
		t.Fatalf("expected 1 item, got %d", len(m.needsAttention))
	}
	if m.needsAttention[0].Anvil != "repo-b" {
		t.Errorf("expected repo-b to remain, got %q", m.needsAttention[0].Anvil)
	}
}

// --- executeAction ---

func TestExecuteAction_NoActionTarget_ReturnsNil(t *testing.T) {
	m := NewModel(nil)
	m.actionTarget = nil
	if cmd := m.executeAction(ActionRetry); cmd != nil {
		t.Error("expected nil cmd when actionTarget is nil")
	}
}

func TestExecuteAction_RetrySuccess_DataNil_ReturnsNil(t *testing.T) {
	m := NewModel(nil) // display-only mode: data == nil
	target := NeedsAttentionItem{BeadID: "forge-1", Anvil: "test"}
	m.actionTarget = &target
	m.needsAttention = []NeedsAttentionItem{target}
	m.OnRetryBead = func(_, _ string, _ int) error { return nil }

	cmd := m.executeAction(ActionRetry)
	if cmd != nil {
		t.Error("expected nil cmd when m.data is nil (no panic)")
	}
	if len(m.needsAttention) != 0 {
		t.Errorf("expected item removed on success, got %d items", len(m.needsAttention))
	}
}

func TestExecuteAction_DismissSuccess_DataNil_ReturnsNil(t *testing.T) {
	m := NewModel(nil)
	target := NeedsAttentionItem{BeadID: "forge-2", Anvil: "test"}
	m.actionTarget = &target
	m.needsAttention = []NeedsAttentionItem{target}
	m.OnDismissBead = func(_, _ string, _ int) error { return nil }

	cmd := m.executeAction(ActionDismiss)
	if cmd != nil {
		t.Error("expected nil cmd when m.data is nil (no panic)")
	}
	if len(m.needsAttention) != 0 {
		t.Errorf("expected item removed on success, got %d items", len(m.needsAttention))
	}
}

func TestExecuteAction_RetryError_ItemNotRemoved(t *testing.T) {
	m := NewModel(nil)
	target := NeedsAttentionItem{BeadID: "forge-3", Anvil: "test"}
	m.actionTarget = &target
	m.needsAttention = []NeedsAttentionItem{target}
	m.OnRetryBead = func(_, _ string, _ int) error { return errors.New("retry failed") }

	m.executeAction(ActionRetry)
	if len(m.needsAttention) != 1 {
		t.Errorf("expected item to remain on error, got %d items", len(m.needsAttention))
	}
}

func TestExecuteAction_DismissError_ItemNotRemoved(t *testing.T) {
	m := NewModel(nil)
	target := NeedsAttentionItem{BeadID: "forge-4", Anvil: "test"}
	m.actionTarget = &target
	m.needsAttention = []NeedsAttentionItem{target}
	m.OnDismissBead = func(_, _ string, _ int) error { return errors.New("dismiss failed") }

	m.executeAction(ActionDismiss)
	if len(m.needsAttention) != 1 {
		t.Errorf("expected item to remain on error, got %d items", len(m.needsAttention))
	}
}

func TestRenderReadyToMergeEmpty(t *testing.T) {
	m := Model{
		readyToMerge: nil,
		focused:      PanelReadyToMerge,
	}
	rendered := m.renderReadyToMerge(60, 20)
	if !strings.Contains(rendered, "None") {
		t.Errorf("expected 'None' when no items:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Ready to Merge (0)") {
		t.Errorf("expected 'Ready to Merge (0)' title:\n%s", rendered)
	}
}

func TestRenderReadyToMergeShowsItems(t *testing.T) {
	m := Model{
		readyToMerge: []ReadyToMergeItem{
			{PRID: 1, PRNumber: 42, BeadID: "bd-10", Anvil: "heimdall", Branch: "forge/bd-10"},
			{PRID: 2, PRNumber: 99, BeadID: "bd-11", Anvil: "metadata", Branch: "forge/bd-11"},
		},
		focused: PanelReadyToMerge,
	}
	rendered := m.renderReadyToMerge(80, 20)
	if !strings.Contains(rendered, "PR #42") {
		t.Errorf("expected 'PR #42' in rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "bd-10") {
		t.Errorf("expected bead ID bd-10 in rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Ready to Merge (2)") {
		t.Errorf("expected 'Ready to Merge (2)' title:\n%s", rendered)
	}
}

func TestRenderReadyToMergeSelectionHighlighting(t *testing.T) {
	m := Model{
		readyToMerge: []ReadyToMergeItem{
			{PRID: 1, PRNumber: 10, BeadID: "bd-20", Anvil: "forge"},
			{PRID: 2, PRNumber: 11, BeadID: "bd-21", Anvil: "forge"},
		},
		readyToMergeVP: scrollViewport{cursor: 0},
		focused:            PanelReadyToMerge,
	}
	// With scroll=0, first item (bd-20) should be selected/visible.
	rendered0 := m.renderReadyToMerge(80, 20)
	if !strings.Contains(rendered0, "bd-20") {
		t.Errorf("expected selected item bd-20 visible at scroll=0:\n%s", rendered0)
	}

	// With scroll=1, second item (bd-21) should be selected/visible.
	m.readyToMergeVP.cursor = 1
	rendered1 := m.renderReadyToMerge(80, 20)
	if !strings.Contains(rendered1, "bd-21") {
		t.Errorf("expected selected item bd-21 visible at scroll=1:\n%s", rendered1)
	}
}

func TestRenderReadyToMergeViewportRegression(t *testing.T) {
	// Regression test: with a small panel height, setting the cursor to a middle
	// index must keep the selected item visible and show adjacent neighbours
	// (not just the selected item). Also verifies that viewport clamping works
	// when the list later shrinks.

	items := []ReadyToMergeItem{
		{PRID: 1, PRNumber: 10, BeadID: "bd-10", Anvil: "forge"},
		{PRID: 2, PRNumber: 11, BeadID: "bd-11", Anvil: "forge"},
		{PRID: 3, PRNumber: 12, BeadID: "bd-12", Anvil: "forge"},
		{PRID: 4, PRNumber: 13, BeadID: "bd-13", Anvil: "forge"},
		{PRID: 5, PRNumber: 14, BeadID: "bd-14", Anvil: "forge"},
	}

	// height=7 → maxItems = 7-3 = 4. Cursor at index 2 (middle).
	// Viewport should show items 0-3 (cursor visible, multiple adjacents present).
	m := Model{
		readyToMerge:     items,
		readyToMergeVP:   scrollViewport{cursor: 2},
		focused:          PanelReadyToMerge,
	}
	rendered := m.renderReadyToMerge(80, 7)

	// Selected item must be visible.
	if !strings.Contains(rendered, "bd-12") {
		t.Errorf("selected item bd-12 (cursor=2) not visible in small-height render:\n%s", rendered)
	}
	// At least one neighbour must also be visible (not just the selected item).
	neighbourVisible := strings.Contains(rendered, "bd-10") ||
		strings.Contains(rendered, "bd-11") ||
		strings.Contains(rendered, "bd-13")
	if !neighbourVisible {
		t.Errorf("no adjacent neighbours visible alongside selected item (bd-12):\n%s", rendered)
	}

	// Now simulate the list shrinking: only 2 items remain, viewStart may be
	// stale. The viewport should clamp so both remaining items are shown.
	m.readyToMerge = items[:2]
	m.readyToMergeVP = scrollViewport{cursor: 1, viewStart: 3} // stale viewStart from before shrink
	rendered2 := m.renderReadyToMerge(80, 7)
	for _, want := range []string{"bd-10", "bd-11"} {
		if !strings.Contains(rendered2, want) {
			t.Errorf("after shrink, expected %q visible but got:\n%s", want, rendered2)
		}
	}
}

func TestRenderMergeMenuContainsBeadIDAndActions(t *testing.T) {
	item := ReadyToMergeItem{PRID: 1, PRNumber: 55, BeadID: "bd-30", Anvil: "test"}
	m := Model{
		mergeTarget:  &item,
		mergeMenuIdx: 0,
		width:        80,
		height:       24,
	}
	rendered := m.renderMergeMenu()
	if !strings.Contains(rendered, "bd-30") {
		t.Errorf("expected bead ID bd-30 in renderMergeMenu output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "PR #55") {
		t.Errorf("expected PR #55 in renderMergeMenu output:\n%s", rendered)
	}
}

func TestRenderMergeMenuShowsPRTitle(t *testing.T) {
	item := ReadyToMergeItem{
		PRID: 1, PRNumber: 55, BeadID: "bd-30", Anvil: "test",
		Title: "fix: resolve flaky timeout in auth middleware",
	}
	m := Model{
		mergeTarget:  &item,
		mergeMenuIdx: 0,
		width:        80,
		height:       24,
	}
	rendered := m.renderMergeMenu()
	if !strings.Contains(rendered, "fix: resolve flaky timeout in auth middleware") {
		t.Errorf("expected PR title in renderMergeMenu output:\n%s", rendered)
	}
}

func TestRenderMergeMenuLongTitleTruncated(t *testing.T) {
	// A very long title should be truncated to 2 lines with ellipsis.
	longTitle := "fix: this is a very long pull request title that definitely exceeds the popup width and should be word-wrapped and then truncated with an ellipsis on the second line"
	item := ReadyToMergeItem{
		PRID: 1, PRNumber: 55, BeadID: "bd-30", Anvil: "test",
		Title: longTitle,
	}
	m := Model{
		mergeTarget:  &item,
		mergeMenuIdx: 0,
		width:        80,
		height:       24,
	}
	rendered := m.renderMergeMenu()
	if !strings.Contains(rendered, "...") {
		t.Errorf("expected ellipsis for long title in renderMergeMenu output:\n%s", rendered)
	}
}

func TestRenderMergeMenuNarrowContentWidthNoSlicePanic(t *testing.T) {
	// With an unusually large border/padding, contentWidth can be negative.
	// renderMergeMenu must not panic in this case.
	longTitle := "a b c d e f g h i j k l m n o p q r s t u v w x y z alpha beta gamma delta epsilon"
	item := ReadyToMergeItem{
		PRID: 1, PRNumber: 1, BeadID: "bd-1", Anvil: "test",
		Title: longTitle,
	}
	m := Model{
		mergeTarget:  &item,
		mergeMenuIdx: 0,
		width:        1,
		height:       5,
	}
	// Should not panic regardless of contentWidth value.
	_ = m.renderMergeMenu()
}

func TestReadyToMergeErrorMsgAppendsEvent(t *testing.T) {
	m := Model{}
	_, _ = m.Update(ReadyToMergeErrorMsg{Err: errors.New("db connection lost")})
	if len(m.events) == 0 {
		t.Fatal("expected an event to be appended on ReadyToMergeErrorMsg")
	}
	if m.events[0].Type != "error" {
		t.Errorf("expected event type 'error', got %q", m.events[0].Type)
	}
	if !strings.Contains(m.events[0].Message, "ready-to-merge read failed") {
		t.Errorf("expected 'ready-to-merge read failed' in event message, got %q", m.events[0].Message)
	}
}

func TestGetVerticalSplitSumsToContentHeight(t *testing.T) {
	for _, h := range []int{10, 24, 40, 80} {
		m := Model{width: 120, height: h}
		topHeight, bottomHeight := m.getVerticalSplit(1, 1)
		contentHeight := h - 1 - 1 - 4 // header(1) + footer(1) + panel borders(4)
		if contentHeight < 0 {
			contentHeight = 0
		}
		if topHeight < 0 {
			t.Errorf("height=%d: topHeight=%d is negative", h, topHeight)
		}
		if bottomHeight < 0 {
			t.Errorf("height=%d: bottomHeight=%d is negative", h, bottomHeight)
		}
		if got := topHeight + bottomHeight; got != contentHeight {
			t.Errorf("height=%d: topHeight(%d) + bottomHeight(%d) = %d, want %d",
				h, topHeight, bottomHeight, got, contentHeight)
		}
	}
}

func TestViewHeaderVisibleAtTightTerminalHeight(t *testing.T) {
	// Regression test: the Hearth header must appear first in the rendered output
	// at any reasonable terminal height so it is not scrolled off-screen.
	// Also verifies the rendered output does not exceed the terminal height,
	// which would cause the terminal to scroll the header off-screen.
	for _, h := range []int{10, 24, 40} {
		m := Model{
			width:  120,
			height: h,
			ready:  true,
		}
		rendered := m.View()
		lines := strings.Split(rendered, "\n")
		firstLine := lines[0]
		if !strings.Contains(firstLine, "Forge") {
			t.Errorf("height=%d: first rendered line does not contain header text, got: %q", h, firstLine)
		}
		if len(lines) > h {
			t.Errorf("height=%d: rendered output has %d lines, exceeds terminal height and will scroll header off-screen", h, len(lines))
		}
	}
}

func TestMergeResultMsgError(t *testing.T) {
	m := Model{}
	msg := MergeResultMsg{PRNumber: 42, Err: errors.New("exit status 1\nsome stderr detail")}
	_, _ = m.Update(msg)

	if !m.statusMsgIsError {
		t.Error("expected statusMsgIsError=true after merge failure")
	}
	if !strings.Contains(m.statusMsg, "42") {
		t.Errorf("expected PR number in status message, got %q", m.statusMsg)
	}
	// Error must be single-line (no embedded newlines in the status bar)
	if strings.ContainsAny(m.statusMsg, "\n\r") {
		t.Errorf("status message must not contain newlines, got %q", m.statusMsg)
	}
	// Error message should show only the first line of the error
	if strings.Contains(m.statusMsg, "some stderr detail") {
		t.Errorf("status message should not contain the second error line, got %q", m.statusMsg)
	}
	if m.statusMsgTime.IsZero() {
		t.Error("expected statusMsgTime to be set")
	}
}

func TestMergeResultMsgSuccess(t *testing.T) {
	// Prime the model with an error state to verify it gets cleared on success.
	m := Model{statusMsgIsError: true}
	msg := MergeResultMsg{PRNumber: 7, Err: nil}
	_, _ = m.Update(msg)

	if m.statusMsgIsError {
		t.Error("expected statusMsgIsError=false after successful merge")
	}
	if !strings.Contains(m.statusMsg, "7") {
		t.Errorf("expected PR number in status message, got %q", m.statusMsg)
	}
	if m.statusMsgTime.IsZero() {
		t.Error("expected statusMsgTime to be set")
	}
}

func TestMergeResultMsgErrorDurationLonger(t *testing.T) {
	// Verify that error status messages use a longer display duration than non-error ones.
	mErr := Model{}
	_, _ = mErr.Update(MergeResultMsg{PRNumber: 1, Err: errors.New("failed")})

	mOK := Model{}
	_, _ = mOK.Update(MergeResultMsg{PRNumber: 2, Err: nil})

	// Both should have a non-zero statusMsg and statusMsgTime set.
	if mErr.statusMsgTime.IsZero() || mOK.statusMsgTime.IsZero() {
		t.Fatal("expected statusMsgTime set in both cases")
	}
	// The error flag difference drives the duration branching in View(); confirm flags differ.
	if !mErr.statusMsgIsError {
		t.Error("error result must set statusMsgIsError=true")
	}
	if mOK.statusMsgIsError {
		t.Error("success result must not set statusMsgIsError")
	}
}

func TestSetStatusResetsErrorFlag(t *testing.T) {
	// After a merge failure sets isError=true, a subsequent non-error setStatus call
	// must reset the flag so the message no longer renders as an error.
	m := Model{statusMsgIsError: true}
	m.setStatus("all good", false)

	if m.statusMsgIsError {
		t.Error("expected statusMsgIsError=false after non-error setStatus call")
	}
	if m.statusMsg != "all good" {
		t.Errorf("expected statusMsg %q, got %q", "all good", m.statusMsg)
	}
	if m.statusMsgTime.IsZero() {
		t.Error("expected statusMsgTime to be set")
	}
}

// --- Queue grouping / navigation tests ---

func TestRebuildQueueNav_SingleAnvil_NoHeaders(t *testing.T) {
	m := Model{
		queue: []QueueItem{
			{BeadID: "bd-1", Anvil: "repo"},
			{BeadID: "bd-2", Anvil: "repo"},
		},
	}
	m.rebuildQueueNav()
	if m.queueGrouped {
		t.Error("expected queueGrouped=false for single anvil")
	}
	if len(m.queueNavItems) != 2 {
		t.Fatalf("expected 2 nav items, got %d", len(m.queueNavItems))
	}
	for i, nav := range m.queueNavItems {
		if nav.isAnvil {
			t.Errorf("nav item %d should not be an anvil header", i)
		}
		if nav.beadIdx != i {
			t.Errorf("nav item %d: expected beadIdx=%d, got %d", i, i, nav.beadIdx)
		}
	}
}

func TestRebuildQueueNav_MultiAnvil_CollapsedByDefault(t *testing.T) {
	m := Model{
		queue: []QueueItem{
			{BeadID: "bd-1", Anvil: "alpha"},
			{BeadID: "bd-2", Anvil: "beta"},
		},
	}
	m.rebuildQueueNav()
	if !m.queueGrouped {
		t.Error("expected queueGrouped=true for multiple anvils")
	}
	// Collapsed: should only have 2 anvil headers, no bead items.
	if len(m.queueNavItems) != 2 {
		t.Fatalf("expected 2 nav items (headers only), got %d", len(m.queueNavItems))
	}
	for _, nav := range m.queueNavItems {
		if !nav.isAnvil {
			t.Error("expected all nav items to be anvil headers when collapsed")
		}
	}
}

func TestRebuildQueueNav_MultiAnvil_ExpandedShowsBeads(t *testing.T) {
	m := Model{
		queue: []QueueItem{
			{BeadID: "bd-1", Anvil: "alpha"},
			{BeadID: "bd-2", Anvil: "alpha"},
			{BeadID: "bd-3", Anvil: "beta"},
		},
		queueExpandedAnvils: map[string]bool{"alpha": true},
	}
	m.rebuildQueueNav()
	// alpha header + 2 beads + beta header = 4
	if len(m.queueNavItems) != 4 {
		t.Fatalf("expected 4 nav items, got %d", len(m.queueNavItems))
	}
	if !m.queueNavItems[0].isAnvil || m.queueNavItems[0].anvilName != "alpha" {
		t.Error("expected first item to be alpha header")
	}
	if m.queueNavItems[1].beadIdx != 0 || m.queueNavItems[2].beadIdx != 1 {
		t.Error("expected alpha beads at indices 0,1")
	}
	if !m.queueNavItems[3].isAnvil || m.queueNavItems[3].anvilName != "beta" {
		t.Error("expected last item to be beta header")
	}
}

func TestRebuildQueueNav_NilExpandedAnvils_NoPanic(t *testing.T) {
	m := Model{
		queue: []QueueItem{
			{BeadID: "bd-1", Anvil: "alpha"},
			{BeadID: "bd-2", Anvil: "beta"},
		},
		// queueExpandedAnvils intentionally nil
	}
	// Should not panic
	m.rebuildQueueNav()
	if m.queueExpandedAnvils == nil {
		t.Error("expected queueExpandedAnvils to be initialized")
	}
}

func TestEnterTogglesAnvilExpansion(t *testing.T) {
	m := Model{
		focused: PanelQueue,
		queue: []QueueItem{
			{BeadID: "bd-1", Anvil: "alpha"},
			{BeadID: "bd-2", Anvil: "beta"},
		},
		queueVP: scrollViewport{cursor: 0},
	}
	// First Enter: expand alpha
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.queueExpandedAnvils["alpha"] {
		t.Error("expected alpha to be expanded after Enter")
	}
	// Should now have: alpha header + bd-1 + beta header = 3
	if len(m.queueNavItems) != 3 {
		t.Fatalf("expected 3 nav items after expand, got %d", len(m.queueNavItems))
	}
	// Second Enter on alpha header: collapse
	m.queueVP.cursor = 0
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.queueExpandedAnvils["alpha"] {
		t.Error("expected alpha to be collapsed after second Enter")
	}
	if len(m.queueNavItems) != 2 {
		t.Fatalf("expected 2 nav items after collapse, got %d", len(m.queueNavItems))
	}
}

func TestEscCollapsesToAnvilHeader(t *testing.T) {
	m := Model{
		focused: PanelQueue,
		queue: []QueueItem{
			{BeadID: "bd-1", Anvil: "alpha"},
			{BeadID: "bd-2", Anvil: "alpha"},
			{BeadID: "bd-3", Anvil: "beta"},
		},
		queueExpandedAnvils: map[string]bool{"alpha": true},
	}
	m.rebuildQueueNav()
	// Cursor on a bead inside alpha (index 1 = first bead under alpha)
	m.queueVP.cursor = 1
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.queueExpandedAnvils["alpha"] {
		t.Error("expected alpha to be collapsed after Esc")
	}
	// Cursor should jump to alpha header
	if m.queueVP.cursor != 0 {
		t.Errorf("expected cursor at 0 (alpha header), got %d", m.queueVP.cursor)
	}
}

func TestSelectedQueueBead_DirectQueueSet_NoExplicitNav(t *testing.T) {
	m := Model{
		queue: []QueueItem{
			{BeadID: "bd-1", Anvil: "test", Section: "ready"},
		},
		queueVP: scrollViewport{cursor: 0},
		// queueNavItems intentionally not built — selectedQueueBead should handle it
	}
	bead := m.selectedQueueBead()
	if bead == nil {
		t.Fatal("expected selectedQueueBead to return a bead after lazy nav build")
	}
	if bead.BeadID != "bd-1" {
		t.Errorf("expected BeadID=bd-1, got %s", bead.BeadID)
	}
}

func TestWorkerStatusStyleAnimated(t *testing.T) {
	frame := "⣾"
	tests := []struct {
		status   string
		wantText string
	}{
		{"running", frame},
		{"reviewing", frame},
	}
	for _, tt := range tests {
		got := workerStatusStyle(tt.status, frame)
		if !strings.Contains(got, tt.wantText) {
			t.Errorf("workerStatusStyle(%q, %q): expected spinner frame in output, got %q", tt.status, frame, got)
		}
	}
}

func TestWorkerStatusStyleStatic(t *testing.T) {
	frame := "⣾" // should be ignored for static statuses
	tests := []struct {
		status   string
		wantText string
	}{
		{"monitoring", "○"},
		{"done", "✓"},
		{"failed", "✗"},
		{"unknown", "○"},
	}
	for _, tt := range tests {
		got := workerStatusStyle(tt.status, frame)
		if !strings.Contains(got, tt.wantText) {
			t.Errorf("workerStatusStyle(%q, %q): expected %q in output, got %q", tt.status, frame, tt.wantText, got)
		}
		// Static statuses must not embed the spinner frame
		if tt.status != "running" && tt.status != "reviewing" && strings.Contains(got, frame) {
			t.Errorf("workerStatusStyle(%q, %q): static status must not contain spinner frame, got %q", tt.status, frame, got)
		}
	}
}

func TestWorkerStatusStyleFrameChanges(t *testing.T) {
	// Different frames should produce different output for animated statuses.
	out1 := workerStatusStyle("running", "⣾")
	out2 := workerStatusStyle("running", "⣽")
	if out1 == out2 {
		t.Errorf("workerStatusStyle(running): different frames should produce different output")
	}
}

func TestCruciblePhaseStyleAnimated(t *testing.T) {
	frame := "⣾"
	tests := []struct {
		phase    string
		wantText string
	}{
		{"dispatching", frame},
		{"final_pr", frame},
		{"started", frame},
	}
	for _, tt := range tests {
		got := cruciblePhaseStyle(tt.phase, frame)
		if !strings.Contains(got, tt.wantText) {
			t.Errorf("cruciblePhaseStyle(%q, %q): expected spinner frame in output, got %q", tt.phase, frame, got)
		}
	}
}

func TestCruciblePhaseStyleStatic(t *testing.T) {
	frame := "⣾" // should be ignored for static phases
	tests := []struct {
		phase    string
		wantText string
	}{
		{"complete", "✓"},
		{"paused", "⏸"},
		{"weird_phase", "weird_phase"},
	}
	for _, tt := range tests {
		got := cruciblePhaseStyle(tt.phase, frame)
		if !strings.Contains(got, tt.wantText) {
			t.Errorf("cruciblePhaseStyle(%q, %q): expected %q in output, got %q", tt.phase, frame, tt.wantText, got)
		}
	}
}

func TestCruciblePhaseStyleLabels(t *testing.T) {
	frame := "⣾"
	tests := []struct {
		phase     string
		wantLabel string
	}{
		{"dispatching", "DISPATCH"},
		{"final_pr", "FINAL PR"},
		{"complete", "COMPLETE"},
		{"paused", "PAUSED"},
		{"started", "STARTED"},
	}
	for _, tt := range tests {
		got := cruciblePhaseStyle(tt.phase, frame)
		if !strings.Contains(got, tt.wantLabel) {
			t.Errorf("cruciblePhaseStyle(%q): expected label %q in output, got %q", tt.phase, tt.wantLabel, got)
		}
	}
}

func TestCruciblePhaseStyleFrameChanges(t *testing.T) {
	// Different frames should produce different output for animated phases.
	out1 := cruciblePhaseStyle("dispatching", "⣾")
	out2 := cruciblePhaseStyle("dispatching", "⣽")
	if out1 == out2 {
		t.Errorf("cruciblePhaseStyle(dispatching): different frames should produce different output")
	}
}

func TestCursorClampedOnCollapse(t *testing.T) {
	m := Model{
		focused: PanelQueue,
		queue: []QueueItem{
			{BeadID: "bd-1", Anvil: "alpha"},
			{BeadID: "bd-2", Anvil: "alpha"},
			{BeadID: "bd-3", Anvil: "beta"},
		},
		queueExpandedAnvils: map[string]bool{"alpha": true, "beta": true},
	}
	m.rebuildQueueNav()
	// alpha header, bd-1, bd-2, beta header, bd-3 = 5 items
	m.queueVP.cursor = 4 // pointing at bd-3
	// Collapse beta via Esc
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	// After collapse: alpha header, bd-1, bd-2, beta header = 4 items
	// Cursor should be at beta header (index 3)
	if m.queueVP.cursor >= len(m.queueNavItems) {
		t.Errorf("cursor %d out of range (nav has %d items)", m.queueVP.cursor, len(m.queueNavItems))
	}
}
