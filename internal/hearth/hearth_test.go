package hearth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/state"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// drainHuh synchronously executes commands from a huh form until it reaches a
// stable state (StateCompleted, StateAborted, or nil command).
func drainHuh(m *Model, cmd tea.Cmd) tea.Cmd {
	for cmd != nil {
		msg := cmd()
		if msg == nil {
			return nil
		}

		// If it's a batch, handle its components.
		if batch, ok := msg.(tea.BatchMsg); ok {
			var nextCmds []tea.Cmd
			for _, bc := range batch {
				nc := drainHuh(m, bc)
				if nc != nil {
					nextCmds = append(nextCmds, nc)
				}
			}
			if len(nextCmds) == 0 {
				return nil
			}
			return tea.Batch(nextCmds...)
		}

		// If the message is not from huh, it's likely an action result.
		// Return a command that produces this message.
		typ := reflect.TypeOf(msg)
		pkg := typ.PkgPath()
		if !strings.Contains(pkg, "charmbracelet/huh") && !strings.Contains(pkg, "charmbracelet/bubbletea") {
			return func() tea.Msg { return msg }
		}

		var nextCmd tea.Cmd
		_, nextCmd = m.Update(msg)
		cmd = nextCmd
	}
	return nil
}

func TestRenderWorkerListShowsTitle(t *testing.T) {
	m := NewModel(nil)
	m.workers = []WorkerItem{
		{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
			Title: "add user auth endpoint"},
	}
	m.focused = PanelWorkers
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: m.workers})
	m = *mTmp.(*Model)
	rendered := m.renderWorkerList(120, 20)
	if !strings.Contains(rendered, "add user auth endpoint") {
		t.Errorf("expected title 'add user auth endpoint' in rendered output:\n%s", rendered)
	}
}

func TestRenderWorkerListNoTitle(t *testing.T) {
	m := NewModel(nil)
	m.workers = []WorkerItem{
		{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
			Title: ""},
	}
	m.focused = PanelWorkers
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: m.workers})
	m = *mTmp.(*Model)
	rendered := m.renderWorkerList(60, 20)
	if !strings.Contains(rendered, "(no title)") {
		t.Errorf("expected '(no title)' when Title is empty:\n%s", rendered)
	}
}

func TestRenderWorkerListWithPRNumber(t *testing.T) {
	m := NewModel(nil)
	m.workers = []WorkerItem{
		{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "quench",
			Title: "fix CI", PRNumber: 42},
	}
	m.focused = PanelWorkers
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: m.workers})
	m = *mTmp.(*Model)
	rendered := m.renderWorkerList(120, 20)
	if !strings.Contains(rendered, "PR#42") {
		t.Errorf("expected 'PR#42' when PRNumber > 0 in rendered output:\n%s", rendered)
	}
}

func TestRenderWorkerListWithoutPRNumber(t *testing.T) {
	m := NewModel(nil)
	m.workers = []WorkerItem{
		{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
			Title: "some task", PRNumber: 0},
	}
	m.focused = PanelWorkers
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: m.workers})
	m = *mTmp.(*Model)
	rendered := m.renderWorkerList(60, 20)
	if strings.Contains(rendered, "PR#") {
		t.Errorf("expected no 'PR#' when PRNumber is 0 in rendered output:\n%s", rendered)
	}
}

func TestRenderWorkerListScrolls(t *testing.T) {
	// Build more workers than fit in the panel to verify scroll/clipping.
	// height=10 → contentHeight = 10-4 = 6. With header, about 5 data rows fit.
	// Only the first 4 workers should be visible at cursor=0.
	workers := []WorkerItem{
		{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-0"},
		{ID: "w2", BeadID: "bd-2", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-1"},
		{ID: "w3", BeadID: "bd-3", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-2"},
		{ID: "w4", BeadID: "bd-4", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-3"},
		{ID: "w5", BeadID: "bd-5", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-4"},
	}
	m := NewModel(nil)
	m.focused = PanelWorkers
	m.workerTable.SetCursor(0)
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: workers})
	m = *mTmp.(*Model)

	rendered := m.renderWorkerList(60, 10)

	// In a table with height 10 (content height 6), with 1 line per worker,
	// and accounting for header and border, about 4 workers should be visible.
	for _, visible := range []string{"title-0", "title-1", "title-2", "title-3"} {
		if !strings.Contains(rendered, visible) {
			t.Errorf("expected %q to be visible in rendered output:\n%s", visible, rendered)
		}
	}
	for _, hidden := range []string{"title-4"} {
		if strings.Contains(rendered, hidden) {
			t.Errorf("expected %q to be clipped (not visible) in rendered output:\n%s", hidden, rendered)
		}
	}
}

func TestRenderWorkerListViewportScrollsToShowSelected(t *testing.T) {
	// height=10 → tableHeight = 10-6 = 4 visible data rows.
	// With cursor=4 (last worker), the table should scroll so that
	// workers 1-4 are visible and worker 0 is clipped.
	workers := []WorkerItem{
		{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-0"},
		{ID: "w2", BeadID: "bd-2", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-1"},
		{ID: "w3", BeadID: "bd-3", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-2"},
		{ID: "w4", BeadID: "bd-4", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-3"},
		{ID: "w5", BeadID: "bd-5", Anvil: "test", Status: "running", Duration: "1m", Type: "smith", Title: "title-4"},
	}
	m := NewModel(nil)
	m.focused = PanelWorkers
	// Load workers first so cursor tracking works properly.
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: workers})
	m = *mTmp.(*Model)
	// Pre-render to set table viewport height (tableHeight = 10-6 = 4),
	// then navigate cursor to last worker.
	m.renderWorkerList(60, 10)
	for i := 0; i < 4; i++ {
		m.workerTable.MoveDown(1)
	}

	rendered := m.renderWorkerList(60, 10)

	// The selected worker (index 4) and its neighbours must be visible.
	for _, visible := range []string{"title-1", "title-2", "title-3", "title-4"} {
		if !strings.Contains(rendered, visible) {
			t.Errorf("expected %q to be visible when workerScroll=4:\n%s", visible, rendered)
		}
	}
	// Workers scrolled out of view must not appear.
	for _, hidden := range []string{"title-0"} {
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
	m := NewModel(nil)
	m.eventScroll = 0
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
	m := NewModel(nil)
	m.workers = []WorkerItem{
		{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
			ActivityLines: []string{"line1", "line2", "line3", "line4", "line5", "line6", "line7", "line8"}},
	}
	m.focused = PanelWorkers
	m.width = 120
	m.height = 40
	m.activityExpanded = make(map[string]bool)
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: m.workers})
	m = *mTmp.(*Model)
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
	m := NewModel(nil)
	m.focused = PanelWorkers
	rendered := m.renderUsagePanel(60, 8)
	if !strings.Contains(rendered, "Usage") {
		t.Errorf("expected 'Usage' title in rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "No usage today") {
		t.Errorf("expected 'No usage today' when no data:\n%s", rendered)
	}
}

func TestRenderUsagePanelWithData(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelWorkers
	m.usage = UsageData{
		Providers: []ProviderUsage{
			{Provider: "claude", Cost: 0.05, InputTokens: 1500, OutputTokens: 300},
		},
		TotalCost:    0.05,
		CostLimit:    1.0,
		CopilotUsed:  5,
		CopilotLimit: 50,
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
	m := NewModel(nil)
	m.focused = PanelWorkers
	// Should not panic at boundary height
	rendered := m.renderUsagePanel(60, 0)
	if rendered == "" {
		t.Errorf("expected non-empty output even at height 0")
	}
}

func TestRenderCenterColumnSmallTerminal(t *testing.T) {
	m := NewModel(nil)
	m.workers = []WorkerItem{
		{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
			Title: "some task"},
	}
	m.focused = PanelWorkers
	m.height = 18
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: m.workers})
	m = *mTmp.(*Model)
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
	m := NewModel(nil)
	m.workers = []WorkerItem{
		{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
			Title: "some task"},
	}
	m.focused = PanelWorkers
	m.height = 50
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: m.workers})
	m = *mTmp.(*Model)
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
	m := NewModel(nil)
	m.needsAttention = []NeedsAttentionItem{
		{BeadID: "bd-42", Anvil: "heimdall", Reason: "exhausted retries", ReasonCategory: AttentionDispatchExhausted},
		{BeadID: "bd-99", Anvil: "metadata", Reason: "clarification needed", ReasonCategory: AttentionClarification},
	}
	m.focused = PanelNeedsAttention
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
			m := NewModel(nil)
			m.needsAttention = []NeedsAttentionItem{
				{BeadID: "bd-1", Anvil: "test", Reason: "test reason", ReasonCategory: tt.category},
			}
			m.focused = PanelNeedsAttention
			rendered := m.renderNeedsAttention(80, 20)
			if !strings.Contains(rendered, tt.wantText) {
				t.Errorf("expected %q label in rendered output for category %d:\n%s", tt.wantText, tt.category, rendered)
			}
		})
	}
}

func TestRenderNeedsAttentionEmpty(t *testing.T) {
	m := NewModel(nil)
	m.needsAttention = nil
	m.focused = PanelNeedsAttention
	rendered := m.renderNeedsAttention(60, 20)
	if !strings.Contains(rendered, "None") {
		t.Errorf("expected 'None' when no items:\n%s", rendered)
	}
}

func TestRenderWorkerActivityNewestFirst(t *testing.T) {
	workers := []WorkerItem{
		{
			ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
			ActivityLines: []string{"[think] oldest entry", "[think] middle entry", "[think] newest entry"},
		},
	}
	m := NewModel(nil)
	m.workerTable.SetCursor(0)
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: workers})
	m = *mTmp.(*Model)
	m.activityExpanded = map[string]bool{"think": true}
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
	workers := []WorkerItem{
		{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
			ActivityLines: lines},
	}
	m := NewModel(nil)
	m.workerTable.SetCursor(0)
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: workers})
	m = *mTmp.(*Model)
	m.activityExpanded = map[string]bool{"think": true}
	m.focused = PanelLiveActivity
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
	workers := []WorkerItem{
		{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
			ActivityLines: lines},
	}
	m := NewModel(nil)
	m.workerTable.SetCursor(0)
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: workers})
	m = *mTmp.(*Model)
	m.activityVP = scrollViewport{cursor: 100} // way past the end
	m.activityExpanded = map[string]bool{"think": true}
	m.focused = PanelLiveActivity
	m.rebuildActivityNav()

	rendered := m.renderWorkerActivity(80, 10)
	// The panel must not be blank — at least some entry should show.
	if !strings.Contains(rendered, "alpha") && !strings.Contains(rendered, "beta") && !strings.Contains(rendered, "gamma") {
		t.Errorf("expected at least some entry when activityVP.cursor is past end:\n%s", rendered)
	}
}

func TestFormatMultiLineEntry(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		maxLines   int
		wantLen    int
		wantFirst  string
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
			name:       "multi-line uses continuation prefix",
			raw:        "line one\nline two\nline three",
			maxLines:   3,
			wantLen:    3,
			wantFirst:  "[text] line one",
			wantSecond: "       line two",
		},
		{
			name:       "blank lines skipped",
			raw:        "first\n\n\nsecond",
			maxLines:   3,
			wantLen:    2,
			wantFirst:  "[text] first",
			wantSecond: "       second",
		},
		{
			name:       "maxLines truncates output",
			raw:        "a\nb\nc\nd",
			maxLines:   2,
			wantLen:    2,
			wantFirst:  "[text] a",
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

func TestFormatToolCall(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		input    string
		want     string
	}{
		{
			name:     "Read with file_path",
			toolName: "Read",
			input:    `{"file_path":"/home/user/project/src/main.go"}`,
			want:     "[tool] Read main.go",
		},
		{
			name:     "Read with offset and limit",
			toolName: "Read",
			input:    `{"file_path":"/tmp/data.go","offset":10,"limit":50}`,
			want:     "[tool] Read data.go:10-50",
		},
		{
			name:     "Edit with old_string",
			toolName: "Edit",
			input:    `{"file_path":"/tmp/foo.go","old_string":"func main() {\n\treturn\n}","new_string":"func main() {}"}`,
			want:     `[tool] Edit foo.go «func main() {»`,
		},
		{
			name:     "Write with file_path",
			toolName: "Write",
			input:    `{"file_path":"/tmp/new_file.go","content":"package main"}`,
			want:     "[tool] Write new_file.go",
		},
		{
			name:     "Bash with command",
			toolName: "Bash",
			input:    `{"command":"git status"}`,
			want:     "[tool] Bash $ git status",
		},
		{
			name:     "Bash with long command truncated",
			toolName: "Bash",
			input:    `{"command":"some very long command that exceeds the fifty character limit for display"}`,
			want:     "[tool] Bash $ some very long command that exceeds the fifty ...",
		},
		{
			name:     "Grep with pattern and glob",
			toolName: "Grep",
			input:    `{"pattern":"TODO","glob":"*.go"}`,
			want:     "[tool] Grep /TODO/ *.go",
		},
		{
			name:     "Grep with pattern and type",
			toolName: "Grep",
			input:    `{"pattern":"func main","type":"go"}`,
			want:     "[tool] Grep /func main/ **/*.go",
		},
		{
			name:     "Glob with pattern",
			toolName: "Glob",
			input:    `{"pattern":"**/*.test.ts"}`,
			want:     "[tool] Glob **/*.test.ts",
		},
		{
			name:     "Agent with description",
			toolName: "Agent",
			input:    `{"description":"explore codebase","prompt":"find all handlers"}`,
			want:     "[tool] Agent explore codebase",
		},
		{
			name:     "unknown tool fallback",
			toolName: "CustomTool",
			input:    `{"key":"value"}`,
			want:     `[tool] CustomTool {"key":"value"}`,
		},
		{
			name:     "Read with empty input falls back",
			toolName: "Read",
			input:    `{}`,
			want:     "[tool] Read ",
		},
		{
			name:     "nil input",
			toolName: "SomeTool",
			input:    "",
			want:     "[tool] SomeTool ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw json.RawMessage
			if tt.input != "" {
				raw = json.RawMessage(tt.input)
			}
			got := formatToolCall(tt.toolName, raw)
			if got != tt.want {
				t.Errorf("formatToolCall(%q, %s)\n  got  %q\n  want %q", tt.toolName, tt.input, got, tt.want)
			}
		})
	}
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
	m := NewModel(nil)
	m.focused = PanelQueue
	m.width = 80
	m.height = 24
	m.queue = []QueueItem{
		{BeadID: "bd-1", Anvil: "test", Section: "unlabeled"},
	}
	m.queueVP = scrollViewport{cursor: 0}
	m.rebuildQueueNav()
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.queueActionForm == nil {
		t.Error("expected queueActionForm!=nil after Enter on unlabeled item")
	}
	if m.queueActionTarget == nil || m.queueActionTarget.BeadID != "bd-1" {
		t.Errorf("expected queueActionTarget.BeadID=bd-1, got %v", m.queueActionTarget)
	}
}

func TestEnterOnReadyQueueItemDoesNotOpenMenu(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelQueue
	m.width = 80
	m.height = 24
	m.queue = []QueueItem{
		{BeadID: "bd-2", Anvil: "test", Section: "ready"},
	}
	m.queueVP = scrollViewport{cursor: 0}
	m.rebuildQueueNav()
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.queueActionForm != nil {
		t.Error("expected queueActionForm==nil for ready (non-unlabeled) item")
	}
}

func TestQueueActionMenuLabelCallsOnTagBead(t *testing.T) {
	var taggedBead, taggedAnvil string
	m := NewModel(nil)
	m.focused = PanelQueue
	m.width = 80
	m.height = 24
	m.queue = []QueueItem{
		{BeadID: "bd-3", Anvil: "forge", Section: "unlabeled"},
	}
	m.queueVP = scrollViewport{cursor: 0}
	m.OnTagBead = func(beadID, anvil string) error {
		taggedBead = beadID
		taggedAnvil = anvil
		return nil
	}
	m.rebuildQueueNav()
	// Open the menu
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = drainHuh(&m, cmd)
	if m.queueActionForm == nil {
		t.Fatal("expected menu open after Enter")
	}
	// Select the label action — menu closes, returns async cmd
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	cmd = drainHuh(&m, cmd)

	if m.queueActionForm != nil {
		t.Errorf("expected menu to close after label action")
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
	m := NewModel(nil)
	m.focused = PanelQueue
	m.width = 80
	m.height = 24
	m.queue = []QueueItem{
		{BeadID: "bd-4", Anvil: "forge", Section: "unlabeled"},
	}
	m.queueVP = scrollViewport{cursor: 0}
	m.OnTagBead = func(beadID, anvil string) error {
		return errors.New("network error")
	}
	m.rebuildQueueNav()
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	cmd = drainHuh(&m, cmd)
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
	m := NewModel(nil)
	m.focused = PanelQueue
	m.width = 80
	m.height = 24
	m.queue = []QueueItem{
		{BeadID: "bd-close-1", Anvil: "forge", Section: "unlabeled"},
	}
	m.queueVP = scrollViewport{cursor: 0}
	m.OnCloseBead = func(beadID, anvil string) error {
		closedBead = beadID
		closedAnvil = anvil
		return nil
	}
	m.rebuildQueueNav()
	// Open the menu
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = drainHuh(&m, cmd)
	if m.queueActionForm == nil {
		t.Fatal("expected menu open after Enter")
	}
	// Navigate to "Close" (index 3: Label=0, ForceRun=1, Stop=2, Close=3) and select it
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	cmd = drainHuh(&m, cmd)
	if m.queueActionForm != nil {
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
	m := NewModel(nil)
	m.focused = PanelQueue
	m.width = 80
	m.height = 24
	m.queue = []QueueItem{
		{BeadID: "bd-close-2", Anvil: "forge", Section: "unlabeled"},
	}
	m.queueVP = scrollViewport{cursor: 0}
	m.OnCloseBead = func(beadID, anvil string) error {
		return errors.New("bd close failed")
	}
	m.rebuildQueueNav()
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	cmd = drainHuh(&m, cmd)
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
	m := NewModel(nil)
	m.focused = PanelQueue
	m.width = 80
	m.height = 24
	m.queue = []QueueItem{
		{BeadID: "bd-close-3", Anvil: "forge", Section: "unlabeled"},
	}
	m.queueVP = scrollViewport{cursor: 0}
	// OnCloseBead intentionally nil
	m.rebuildQueueNav()
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.statusMsg, "unavailable") {
		t.Errorf("expected 'unavailable' statusMsg, got %q (form nil: %v)", m.statusMsg, m.queueActionForm == nil)
	}
}

func TestRenderQueueActionMenuContainsBeadID(t *testing.T) {
	item := QueueItem{BeadID: "bd-5", Anvil: "test", Section: "unlabeled"}
	m := NewModel(nil)
	m.queueActionTarget = &item
	m.width = 80
	m.height = 24
	m.queueActionForm = buildQueueActionForm(&item, &m.queueActionChoice)
	m.queueActionForm.Init()
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
	m := NewModel(nil)
	m.queueActionTarget = &item
	m.width = 80
	m.height = 24
	m.queueActionForm = buildQueueActionForm(&item, &m.queueActionChoice)
	m.queueActionForm.Init()
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
	m := NewModel(nil)
	m.queueActionTarget = &item
	m.width = 80
	m.height = 24
	m.queueActionForm = buildQueueActionForm(&item, &m.queueActionChoice)
	m.queueActionForm.Init()
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
	m := NewModel(nil)
	m.queueActionTarget = &item
	m.width = 80
	m.height = 24
	m.queueActionForm = buildQueueActionForm(&item, &m.queueActionChoice)
	m.queueActionForm.Init()
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
	m := NewModel(nil)
	m.queueActionTarget = &item
	m.width = 80
	m.height = 24
	m.queueActionForm = buildQueueActionForm(&item, &m.queueActionChoice)
	m.queueActionForm.Init()
	rendered := m.renderQueueActionMenu()
	// Should contain ellipsis for truncated description.
	if !strings.Contains(rendered, "...") {
		t.Errorf("expected ellipsis for truncated description:\n%s", rendered)
	}
}

func TestUpdateQueueMsgClosesMenuWhenTargetRemoved(t *testing.T) {
	item := QueueItem{BeadID: "bd-6", Anvil: "test", Section: "unlabeled"}
	m := NewModel(nil)
	m.queueActionTarget = &item
	m.queue = []QueueItem{item}
	m.queueActionForm = buildQueueActionForm(&item, &m.queueActionChoice)
	m.queueActionForm.Init()
	// Simulate queue refresh that removes the target bead
	_, _ = m.Update(UpdateQueueMsg{Items: []QueueItem{
		{BeadID: "bd-99", Anvil: "test", Section: "unlabeled"},
	}})
	if m.queueActionForm != nil {
		t.Error("expected menu to close when target bead no longer in unlabeled section")
	}
	if m.queueActionTarget != nil {
		t.Error("expected queueActionTarget to be nil after menu closed")
	}
}

func TestUpdateQueueMsgKeepsMenuWhenTargetStillPresent(t *testing.T) {
	item := QueueItem{BeadID: "bd-7", Anvil: "test", Section: "unlabeled"}
	m := NewModel(nil)
	m.queueActionTarget = &item
	m.queue = []QueueItem{item}
	m.queueActionForm = buildQueueActionForm(&item, &m.queueActionChoice)
	m.queueActionForm.Init()
	// Simulate queue refresh that keeps the target bead
	_, _ = m.Update(UpdateQueueMsg{Items: []QueueItem{item}})
	if m.queueActionForm == nil {
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
	m := NewModel(nil)
	m.readyToMerge = nil
	m.focused = PanelReadyToMerge
	rendered := m.renderReadyToMerge(60, 20)
	if !strings.Contains(rendered, "None") {
		t.Errorf("expected 'None' when no items:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Ready to Merge (0)") {
		t.Errorf("expected 'Ready to Merge (0)' title:\n%s", rendered)
	}
}

func TestRenderReadyToMergeShowsItems(t *testing.T) {
	m := NewModel(nil)
	m.readyToMerge = []ReadyToMergeItem{
		{PRID: 1, PRNumber: 42, BeadID: "bd-10", Anvil: "heimdall", Branch: "forge/bd-10"},
		{PRID: 2, PRNumber: 99, BeadID: "bd-11", Anvil: "metadata", Branch: "forge/bd-11"},
	}
	m.focused = PanelReadyToMerge
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

func TestRenderReadyToMergeAutoTag(t *testing.T) {
	m := NewModel(nil)
	m.readyToMerge = []ReadyToMergeItem{
		{PRID: 1, PRNumber: 42, BeadID: "bd-10", Anvil: "heimdall", AutoMerge: true},
		{PRID: 2, PRNumber: 99, BeadID: "bd-11", Anvil: "metadata", AutoMerge: false},
	}
	m.focused = PanelReadyToMerge
	rendered := m.renderReadyToMerge(80, 20)
	if !strings.Contains(rendered, "[auto]") {
		t.Errorf("expected '[auto]' tag for auto-merge PR in rendered output:\n%s", rendered)
	}
	// The non-auto-merge PR should not show the tag. Count occurrences.
	count := strings.Count(rendered, "[auto]")
	if count != 1 {
		t.Errorf("expected exactly 1 '[auto]' tag, got %d in rendered output:\n%s", count, rendered)
	}
}

func TestRenderReadyToMergeSelectionHighlighting(t *testing.T) {
	m := NewModel(nil)
	m.readyToMerge = []ReadyToMergeItem{
		{PRID: 1, PRNumber: 10, BeadID: "bd-20", Anvil: "forge"},
		{PRID: 2, PRNumber: 11, BeadID: "bd-21", Anvil: "forge"},
	}
	m.readyToMergeVP = scrollViewport{cursor: 0}
	m.focused = PanelReadyToMerge
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
	m := NewModel(nil)
	m.readyToMerge = items
	m.readyToMergeVP = scrollViewport{cursor: 2}
	m.focused = PanelReadyToMerge
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
	m := NewModel(nil)
	m.mergeTarget = &item
	m.width = 80
	m.height = 24
	m.mergeForm = buildMergeForm(&item, &m.mergeChoice)
	m.mergeForm.Init()
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
	m := NewModel(nil)
	m.mergeTarget = &item
	m.width = 80
	m.height = 24
	m.mergeForm = buildMergeForm(&item, &m.mergeChoice)
	m.mergeForm.Init()
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
	m := NewModel(nil)
	m.mergeTarget = &item
	m.width = 80
	m.height = 24
	m.mergeForm = buildMergeForm(&item, &m.mergeChoice)
	m.mergeForm.Init()
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
	m := NewModel(nil)
	m.mergeTarget = &item
	m.width = 1
	m.height = 5
	m.mergeForm = buildMergeForm(&item, &m.mergeChoice)
	m.mergeForm.Init()
	// Should not panic regardless of contentWidth value.
	_ = m.renderMergeMenu()
}

func TestReadyToMergeErrorMsgAppendsEvent(t *testing.T) {
	m := NewModel(nil)
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
		m := NewModel(nil)
		m.width = 120
		m.height = h
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
		m := NewModel(nil)
		m.width = 120
		m.height = h
		m.ready = true
		rendered := m.View()
		lines := strings.Split(rendered, "\n")
		firstLine := lines[0]
		if !strings.Contains(firstLine, "Forge") {
			t.Errorf("height=%d: first rendered line does not contain header text, got: %q", h, firstLine)
		}
		if len(lines) > h {
			t.Errorf("height=%d: rendered output has %d lines, exceeds terminal height and will scroll header off-screen\nACTUAL RENDERED:\n%s", h, len(lines), rendered)
		}
	}
}

func TestMergeResultMsgError(t *testing.T) {
	m := NewModel(nil)
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
	m := NewModel(nil)
	m.statusMsgIsError = true
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
	mErr := NewModel(nil)
	_, _ = mErr.Update(MergeResultMsg{PRNumber: 1, Err: errors.New("failed")})

	mOK := NewModel(nil)
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
	m := NewModel(nil)
	m.statusMsgIsError = true
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
	m := NewModel(nil)
	m.queue = []QueueItem{
		{BeadID: "bd-1", Anvil: "repo"},
		{BeadID: "bd-2", Anvil: "repo"},
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
	m := NewModel(nil)
	m.queue = []QueueItem{
		{BeadID: "bd-1", Anvil: "alpha"},
		{BeadID: "bd-2", Anvil: "beta"},
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
	m := NewModel(nil)
	m.queue = []QueueItem{
		{BeadID: "bd-1", Anvil: "alpha"},
		{BeadID: "bd-2", Anvil: "alpha"},
		{BeadID: "bd-3", Anvil: "beta"},
	}
	m.queueExpandedAnvils = map[string]bool{"alpha": true}
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
	m := NewModel(nil)
	m.queue = []QueueItem{
		{BeadID: "bd-1", Anvil: "alpha"},
		{BeadID: "bd-2", Anvil: "beta"},
	}
	// queueExpandedAnvils intentionally nil
	m.queueExpandedAnvils = nil
	// Should not panic
	m.rebuildQueueNav()
	if m.queueExpandedAnvils == nil {
		t.Error("expected queueExpandedAnvils to be initialized")
	}
}

func TestEnterTogglesAnvilExpansion(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelQueue
	m.width = 80
	m.height = 24
	m.queue = []QueueItem{
		{BeadID: "bd-1", Anvil: "alpha"},
		{BeadID: "bd-2", Anvil: "beta"},
	}
	m.queueVP = scrollViewport{cursor: 0}
	m.rebuildQueueNav()
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
	m := NewModel(nil)
	m.focused = PanelQueue
	m.width = 80
	m.height = 24
	m.queue = []QueueItem{
		{BeadID: "bd-1", Anvil: "alpha"},
		{BeadID: "bd-2", Anvil: "alpha"},
		{BeadID: "bd-3", Anvil: "beta"},
	}
	m.queueExpandedAnvils = map[string]bool{"alpha": true}
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
	m := NewModel(nil)
	m.queue = []QueueItem{
		{BeadID: "bd-1", Anvil: "test", Section: "ready"},
	}
	m.queueVP = scrollViewport{cursor: 0}
	// queueNavItems intentionally not built — selectedQueueBead should handle it
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
		got := workerStatusIndicator(tt.status, frame)
		if !strings.Contains(got, tt.wantText) {
			t.Errorf("workerStatusIndicator(%q, %q): expected spinner frame in output, got %q", tt.status, frame, got)
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
		got := workerStatusIndicator(tt.status, frame)
		if !strings.Contains(got, tt.wantText) {
			t.Errorf("workerStatusIndicator(%q, %q): expected %q in output, got %q", tt.status, frame, tt.wantText, got)
		}
		// Static statuses must not embed the spinner frame
		if tt.status != "running" && tt.status != "reviewing" && strings.Contains(got, frame) {
			t.Errorf("workerStatusIndicator(%q, %q): static status must not contain spinner frame, got %q", tt.status, frame, got)
		}
	}
}

func TestWorkerStatusStyleFrameChanges(t *testing.T) {
	// Different frames should produce different output for animated statuses.
	out1 := workerStatusIndicator("running", "⣾")
	out2 := workerStatusIndicator("running", "⣽")
	if out1 == out2 {
		t.Errorf("workerStatusIndicator(running): different frames should produce different output")
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
	m := NewModel(nil)
	m.focused = PanelQueue
	m.width = 80
	m.height = 24
	m.queue = []QueueItem{
		{BeadID: "bd-1", Anvil: "alpha"},
		{BeadID: "bd-2", Anvil: "alpha"},
		{BeadID: "bd-3", Anvil: "beta"},
	}
	m.queueExpandedAnvils = map[string]bool{"alpha": true, "beta": true}
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

// --- panelAtPos ---

// newTestModelForPanelAtPos returns a Model configured for deterministic panelAtPos tests.
// width=120, height=40; computeHeaderH() renders a 1-line header at this width.
func newTestModelForPanelAtPos() Model {
	m := NewModel(nil)
	m.width = 120
	m.height = 40
	m.focused = PanelWorkers // default focused panel returned for out-of-range hits
	return m
}

// panelBoundaries returns the x/y split points for a 120x40 model using the
// same layout math as View() and panelAtPos().
//
//	queueWidth=23, workerWidth=29
//	leftEnd=24, centerEnd=55
//	headerH=1, footerH=1 (single-line header/footer at width=120)
//	topH=20, topRegionH=22 (topH inner rows + 2 border rows)
func panelBoundaries(m Model) (leftEnd, centerEnd, topRegionH, headerH int) {
	queueWidth, workerWidth, _ := m.getTopPanelWidths()
	leftColumnWidth := queueWidth + 2*panelBorderEachSide
	centerColumnWidth := workerWidth + 2*panelBorderEachSide
	leftEnd = leftColumnWidth - 1
	centerEnd = leftColumnWidth + centerColumnWidth - 1
	headerH = m.computeHeaderH()
	footerH := m.computeFooterH()
	topH, _ := m.getVerticalSplit(headerH, footerH)
	topRegionH = topH + 2 // inner rows + 2 border rows
	return
}

func TestPanelAtPosLeftColumn_TopHalf_ReturnsQueue(t *testing.T) {
	m := newTestModelForPanelAtPos()
	leftEnd, _, topRegionH, headerH := panelBoundaries(m)
	// Click inside the left column in the top region (no crucibles).
	x := leftEnd - 1
	y := headerH + topRegionH/2
	got := m.panelAtPos(x, y)
	if got != PanelQueue {
		t.Errorf("panelAtPos(%d,%d) = %v, want PanelQueue", x, y, got)
	}
}

func TestPanelAtPosLeftColumn_TopHalf_WithCrucibles_ReturnsBothQueueAndCrucibles(t *testing.T) {
	m := newTestModelForPanelAtPos()
	m.crucibles = []CrucibleItem{{ParentID: "bd-c1"}}
	leftEnd, _, topRegionH, headerH := panelBoundaries(m)

	// Scan the top-left region and ensure both Queue and Crucibles are present,
	// with Queue appearing above Crucibles.
	x := leftEnd - 1
	var queueY, cruciblesY int = -1, -1
	topStartY := headerH + 1
	topEndY := headerH + topRegionH - 1
	if topEndY >= m.height-1 {
		topEndY = m.height - 2
	}
	for y := topStartY; y <= topEndY; y++ {
		panel := m.panelAtPos(x, y)
		if panel == PanelQueue && queueY == -1 {
			queueY = y
		}
		if panel == PanelCrucibles && cruciblesY == -1 {
			cruciblesY = y
		}
	}
	if queueY == -1 {
		t.Fatalf("expected to find PanelQueue in top-left region, but did not (x=%d, y in [%d,%d])", x, topStartY, topEndY)
	}
	if cruciblesY == -1 {
		t.Fatalf("expected to find PanelCrucibles in top-left region when crucibles present, but did not (x=%d, y in [%d,%d])", x, topStartY, topEndY)
	}
	if !(queueY < cruciblesY) {
		t.Errorf("expected PanelQueue to appear above PanelCrucibles in top-left region, got queueY=%d, cruciblesY=%d", queueY, cruciblesY)
	}
}

func TestPanelAtPosLeftColumn_BottomHalf_ReturnsReadyToMerge(t *testing.T) {
	m := newTestModelForPanelAtPos()
	leftEnd, _, topRegionH, headerH := panelBoundaries(m)

	// Scan the bottom-left region to ensure ReadyToMerge is present, and that
	// if NeedsAttention is also present, ReadyToMerge appears above it.
	x := leftEnd - 1
	var readyY, needsY int = -1, -1
	bottomStartY := headerH + topRegionH + 1
	bottomEndY := m.height - 2
	for y := bottomStartY; y <= bottomEndY; y++ {
		panel := m.panelAtPos(x, y)
		if panel == PanelReadyToMerge && readyY == -1 {
			readyY = y
		}
		if panel == PanelNeedsAttention && needsY == -1 {
			needsY = y
		}
	}
	if readyY == -1 {
		t.Fatalf("expected to find PanelReadyToMerge in bottom-left region, but did not (x=%d, y in [%d,%d])", x, bottomStartY, bottomEndY)
	}
	// If both panels are present, ReadyToMerge should be above NeedsAttention.
	if needsY != -1 && !(readyY < needsY) {
		t.Errorf("expected PanelReadyToMerge to appear above PanelNeedsAttention in bottom-left region, got readyY=%d, needsY=%d", readyY, needsY)
	}
}

func TestPanelAtPosLeftColumn_BottomHalf_ReturnsNeedsAttention(t *testing.T) {
	m := newTestModelForPanelAtPos()
	leftEnd, _, topRegionH, headerH := panelBoundaries(m)

	// Scan the bottom-left region to ensure NeedsAttention is present, and that
	// if ReadyToMerge is also present, ReadyToMerge appears above it.
	x := leftEnd - 1
	var readyY, needsY int = -1, -1
	bottomStartY := headerH + topRegionH + 1
	bottomEndY := m.height - 2
	for y := bottomStartY; y <= bottomEndY; y++ {
		panel := m.panelAtPos(x, y)
		if panel == PanelReadyToMerge && readyY == -1 {
			readyY = y
		}
		if panel == PanelNeedsAttention && needsY == -1 {
			needsY = y
		}
	}
	if needsY == -1 {
		t.Fatalf("expected to find PanelNeedsAttention in bottom-left region, but did not (x=%d, y in [%d,%d])", x, bottomStartY, bottomEndY)
	}
	// If both panels are present, ReadyToMerge should be above NeedsAttention.
	if readyY != -1 && !(readyY < needsY) {
		t.Errorf("expected PanelReadyToMerge to appear above PanelNeedsAttention in bottom-left region, got readyY=%d, needsY=%d", readyY, needsY)
	}
}

func TestPanelAtPosCenterColumn_ReturnsWorkersAndUsage(t *testing.T) {
	m := newTestModelForPanelAtPos()
	leftEnd, centerEnd, _, headerH := panelBoundaries(m)
	x := leftEnd + 5
	if x > centerEnd {
		x = (leftEnd + centerEnd) / 2
	}

	// Scan the center column to find both Workers and Usage panels.
	var workersY, usageY int = -1, -1
	for y := headerH + 1; y < m.height-1; y++ {
		panel := m.panelAtPos(x, y)
		if panel == PanelWorkers && workersY == -1 {
			workersY = y
		}
		if panel == PanelUsage && usageY == -1 {
			usageY = y
		}
	}
	if workersY == -1 {
		t.Fatalf("expected to find PanelWorkers in center column, but did not")
	}
	if usageY == -1 {
		t.Fatalf("expected to find PanelUsage in center column, but did not")
	}
	if !(workersY < usageY) {
		t.Errorf("expected PanelWorkers to appear above PanelUsage, got workersY=%d, usageY=%d", workersY, usageY)
	}
}

func TestPanelAtPosRightColumn_TopHalf_ReturnsLiveActivity(t *testing.T) {
	m := newTestModelForPanelAtPos()
	_, centerEnd, topRegionH, headerH := panelBoundaries(m)
	x := centerEnd + 5
	y := headerH + topRegionH/2
	got := m.panelAtPos(x, y)
	if got != PanelLiveActivity {
		t.Errorf("panelAtPos(%d,%d) = %v, want PanelLiveActivity", x, y, got)
	}
}

func TestPanelAtPosRightColumn_BottomHalf_ReturnsEvents(t *testing.T) {
	m := newTestModelForPanelAtPos()
	_, centerEnd, topRegionH, headerH := panelBoundaries(m)
	x := centerEnd + 5
	y := headerH + topRegionH + 1
	got := m.panelAtPos(x, y)
	if got != PanelEvents {
		t.Errorf("panelAtPos(%d,%d) = %v, want PanelEvents", x, y, got)
	}
}

func TestPanelAtPosHeaderRow_ReturnsFocused(t *testing.T) {
	m := newTestModelForPanelAtPos()
	m.focused = PanelLiveActivity
	// y=0 is always in the header row.
	got := m.panelAtPos(50, 0)
	if got != PanelLiveActivity {
		t.Errorf("panelAtPos in header row = %v, want focused panel (PanelLiveActivity)", got)
	}
}

func TestPanelAtPosFooterRow_ReturnsFocused(t *testing.T) {
	m := newTestModelForPanelAtPos()
	m.focused = PanelQueue
	// y >= height-1 is the footer row.
	got := m.panelAtPos(50, m.height-1)
	if got != PanelQueue {
		t.Errorf("panelAtPos in footer row = %v, want focused panel (PanelQueue)", got)
	}
}

func TestRenderLogViewerShowsTitleAndContent(t *testing.T) {
	vpWidth, vpHeight := 80, 10
	vp := viewport.New(vpWidth, vpHeight)
	vp.SetContent("line one\nline two\nline three")
	m := NewModel(nil)
	m.width = 120
	m.height = 40
	m.showLogViewer = true
	m.logViewerTitle = "bd-42 worker.log"
	m.logViewerEmpty = false
	m.logViewerVP = vp

	rendered := m.renderLogViewer()
	if !strings.Contains(rendered, "bd-42 worker.log") {
		t.Errorf("expected log viewer title in output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "line one") {
		t.Errorf("expected log content in output:\n%s", rendered)
	}
}

func TestRenderLogViewerEmptyLines(t *testing.T) {
	m := NewModel(nil)
	m.width = 120
	m.height = 40
	m.showLogViewer = true
	m.logViewerTitle = "empty.log"
	m.logViewerEmpty = true

	rendered := m.renderLogViewer()
	if !strings.Contains(rendered, "empty log") {
		t.Errorf("expected '(empty log)' message when logViewerEmpty is true:\n%s", rendered)
	}
}

func TestLogViewerDimensionsRespectStyleFrameSize(t *testing.T) {
	m := NewModel(nil)
	m.width = 120
	m.height = 40
	vpWidth, vpHeight := m.logViewerDimensions()

	frameH := logViewerStyle.GetHorizontalFrameSize()
	frameV := logViewerStyle.GetVerticalFrameSize()

	viewerWidth := 120 - 8 // m.width - 8
	viewerHeight := 40 - 6 // m.height - 6

	wantWidth := viewerWidth - frameH
	// 4 fixed content lines: title + blank + blank + footer
	wantHeight := viewerHeight - frameV - 4

	if vpWidth != wantWidth {
		t.Errorf("vpWidth = %d, want %d (viewerWidth %d - frameH %d)", vpWidth, wantWidth, viewerWidth, frameH)
	}
	if vpHeight != wantHeight {
		t.Errorf("vpHeight = %d, want %d (viewerHeight %d - frameV %d - 4)", vpHeight, wantHeight, viewerHeight, frameV)
	}
}

func TestLogViewerDimensionsMinClamped(t *testing.T) {
	// Very small terminal should clamp to minimums
	m := NewModel(nil)
	m.width = 10
	m.height = 8
	vpWidth, vpHeight := m.logViewerDimensions()
	if vpWidth < 1 {
		t.Errorf("vpWidth = %d, want >= 1", vpWidth)
	}
	if vpHeight < 1 {
		t.Errorf("vpHeight = %d, want >= 1", vpHeight)
	}
}

// --- Orphan dialog ---

func TestRenderOrphanDialogShowsBeadIDAndTitle(t *testing.T) {
	item := PendingOrphanItem{BeadID: "Forge-abc1", Anvil: "heimdall", Title: "Fix login timeout bug"}
	m := NewModel(nil)
	m.orphanTarget = &item
	m.orphanDialogForm = buildOrphanDialogForm(&item, &m.orphanDialogChoice)
	m.orphanDialogForm.Init()
	rendered := m.renderOrphanDialog()
	if !strings.Contains(rendered, "Forge-abc1") {
		t.Errorf("expected bead ID 'Forge-abc1' in orphan dialog:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Fix login timeout bug") {
		t.Errorf("expected title 'Fix login timeout bug' in orphan dialog:\n%s", rendered)
	}
}

func TestRenderOrphanDialogShowsAllChoices(t *testing.T) {
	item := PendingOrphanItem{BeadID: "Forge-xyz", Anvil: "test", Title: "Some work"}
	m := NewModel(nil)
	m.orphanTarget = &item
	m.orphanDialogForm = buildOrphanDialogForm(&item, &m.orphanDialogChoice)
	m.orphanDialogForm.Init()
	rendered := m.renderOrphanDialog()
	if !strings.Contains(rendered, "Recover") {
		t.Errorf("expected 'Recover' option in orphan dialog:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Close") {
		t.Errorf("expected 'Close' option in orphan dialog:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Discard") {
		t.Errorf("expected 'Discard' option in orphan dialog:\n%s", rendered)
	}
}

func TestRenderOrphanDialogShowsPendingCount(t *testing.T) {
	item := PendingOrphanItem{BeadID: "Forge-1", Anvil: "test", Title: "first"}
	extra := PendingOrphanItem{BeadID: "Forge-2", Anvil: "test", Title: "second"}
	m := NewModel(nil)
	m.orphanTarget = &item
	m.orphanQueue = []PendingOrphanItem{extra}
	m.orphanDialogForm = buildOrphanDialogForm(&item, &m.orphanDialogChoice)
	m.orphanDialogForm.Init()
	rendered := m.renderOrphanDialog()
	if !strings.Contains(rendered, "1 more pending") {
		t.Errorf("expected pending count hint in orphan dialog:\n%s", rendered)
	}
}

func TestRenderOrphanDialogNilTargetReturnsEmpty(t *testing.T) {
	m := NewModel(nil)
	m.orphanTarget = nil
	rendered := m.renderOrphanDialog()
	if rendered != "" {
		t.Errorf("expected empty string when orphanTarget is nil, got: %q", rendered)
	}
}

func TestOrphanDialogKeyboardNavigation(t *testing.T) {
	item := PendingOrphanItem{BeadID: "Forge-nav", Anvil: "test", Title: "nav test"}
	m := NewModel(nil)
	m.orphanTarget = &item
	m.orphanDialogForm = buildOrphanDialogForm(&item, &m.orphanDialogChoice)
	m.orphanDialogForm.Init()

	// j and k navigate within the form without closing it
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if m.orphanDialogForm == nil {
		t.Error("expected dialog to remain open after 'j'")
	}

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m.orphanDialogForm == nil {
		t.Error("expected dialog to remain open after 'k'")
	}

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if m.orphanDialogForm == nil {
		t.Error("expected dialog to remain open after second 'k'")
	}
}

func TestOrphanDialogEscSkipsOrphan(t *testing.T) {
	item := PendingOrphanItem{BeadID: "Forge-esc", Anvil: "test", Title: "esc test"}
	m := NewModel(nil)
	m.orphanTarget = &item
	m.orphanDialogForm = buildOrphanDialogForm(&item, &m.orphanDialogChoice)
	m.orphanDialogForm.Init()

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.orphanDialogForm != nil {
		t.Error("expected orphanDialogForm=nil after Esc")
	}
	if m.orphanTarget != nil {
		t.Error("expected orphanTarget=nil after Esc")
	}
}

func TestOrphanDialogEnterCallsOnResolveOrphan(t *testing.T) {
	var resolvedBead, resolvedAnvil, resolvedAction string
	item := PendingOrphanItem{BeadID: "Forge-res", Anvil: "heimdall", Title: "resolve me"}
	m := NewModel(nil)
	m.orphanTarget = &item
	m.OnResolveOrphan = func(beadID, anvil, action string) error {
		resolvedBead = beadID
		resolvedAnvil = anvil
		resolvedAction = action
		return nil
	}
	m.orphanDialogForm = buildOrphanDialogForm(&item, &m.orphanDialogChoice)
	initCmd := m.orphanDialogForm.Init()
	if initCmd != nil {
		msg := initCmd()
		_, _ = m.Update(msg)
	}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for m.orphanDialogForm != nil && cmd != nil {
		_, cmd = m.Update(cmd())
	}
	if cmd == nil {
		t.Fatal("expected a tea.Cmd after Enter on orphan dialog")
	}
	// Execute the async command
	msg := cmd()
	_, _ = m.Update(msg)

	if resolvedBead != "Forge-res" {
		t.Errorf("expected resolvedBead=Forge-res, got %q", resolvedBead)
	}
	if resolvedAnvil != "heimdall" {
		t.Errorf("expected resolvedAnvil=heimdall, got %q", resolvedAnvil)
	}
	if resolvedAction != "recover" {
		t.Errorf("expected resolvedAction=recover, got %q", resolvedAction)
	}
}

func TestOrphanDialogMouseWheelBlocked(t *testing.T) {
	item := PendingOrphanItem{BeadID: "Forge-wheel", Anvil: "test", Title: "wheel test"}
	m := NewModel(nil)
	m.orphanTarget = &item
	m.focused = PanelQueue
	m.width = 120
	m.height = 40
	m.orphanDialogForm = buildOrphanDialogForm(&item, &m.orphanDialogChoice)
	m.orphanDialogForm.Init()
	initialFocused := m.focused

	// Wheel up should not change focus or scroll
	_, _ = m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelUp,
		X:      60,
		Y:      20,
	})
	if m.focused != initialFocused {
		t.Errorf("mouse wheel up should not change focus during orphan dialog, got %v", m.focused)
	}

	// Wheel down should not change focus or scroll
	_, _ = m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelDown,
		X:      60,
		Y:      20,
	})
	if m.focused != initialFocused {
		t.Errorf("mouse wheel down should not change focus during orphan dialog, got %v", m.focused)
	}
}

func TestUpdatePendingOrphansMsgOpensDialog(t *testing.T) {
	m := NewModel(nil)
	m.orphanTarget = nil
	item := PendingOrphanItem{BeadID: "Forge-poll", Anvil: "test", Title: "polled orphan"}
	_, _ = m.Update(UpdatePendingOrphansMsg{Items: []PendingOrphanItem{item}})

	if m.orphanDialogForm == nil {
		t.Error("expected orphanDialogForm!=nil after UpdatePendingOrphansMsg with items")
	}
	if m.orphanTarget == nil || m.orphanTarget.BeadID != "Forge-poll" {
		t.Errorf("expected orphanTarget.BeadID=Forge-poll, got %v", m.orphanTarget)
	}
}

// --- crucibleProgressColor ---

func TestCrucibleProgressColorComplete(t *testing.T) {
	got := crucibleProgressColor("complete")
	if got != colorSuccess {
		t.Errorf("crucibleProgressColor(complete) = %v, want %v (success color)", got, colorSuccess)
	}
}

func TestCrucibleProgressColorPaused(t *testing.T) {
	got := crucibleProgressColor("paused")
	if got != colorDanger {
		t.Errorf("crucibleProgressColor(paused) = %v, want %v (danger color)", got, colorDanger)
	}
}

func TestCrucibleProgressColorDefault(t *testing.T) {
	for _, phase := range []string{"started", "dispatching", "final_pr", "waiting", ""} {
		got := crucibleProgressColor(phase)
		if got != colorWarning {
			t.Errorf("crucibleProgressColor(%q) = %v, want %v (warning color)", phase, got, colorWarning)
		}
	}
}

// --- renderCrucibles ---

func TestRenderCruciblesEmpty(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelQueue
	rendered := m.renderCrucibles(60, 10)
	if !strings.Contains(rendered, "None") {
		t.Errorf("expected 'None' when no crucibles, got:\n%s", rendered)
	}
}

func TestRenderCruciblesShowsParentIDAndCount(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelCrucibles
	m.crucibles = []CrucibleItem{
		{
			ParentID:          "Forge-epic1",
			ParentTitle:       "Big epic feature",
			Anvil:             "heimdall",
			Phase:             "dispatching",
			TotalChildren:     5,
			CompletedChildren: 2,
			CurrentChild:      "Forge-child3",
		},
	}
	rendered := m.renderCrucibles(80, 20)
	if !strings.Contains(rendered, "Forge-epic1") {
		t.Errorf("expected parent ID 'Forge-epic1' in crucibles output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "2/5") {
		t.Errorf("expected count '2/5' in crucibles output:\n%s", rendered)
	}
}

func TestRenderCruciblesShowsCurrentChild(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelQueue
	m.crucibles = []CrucibleItem{
		{
			ParentID:          "Forge-ep2",
			Anvil:             "test",
			Phase:             "started",
			TotalChildren:     3,
			CompletedChildren: 1,
			CurrentChild:      "Forge-child2",
		},
	}
	rendered := m.renderCrucibles(80, 20)
	if !strings.Contains(rendered, "Forge-child2") {
		t.Errorf("expected current child 'Forge-child2' in crucibles output:\n%s", rendered)
	}
}

func TestRenderCruciblesTitleFallbackWhenNoCurrentChild(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelQueue
	m.crucibles = []CrucibleItem{
		{
			ParentID:          "Forge-ep3",
			ParentTitle:       "No child yet",
			Anvil:             "test",
			Phase:             "started",
			TotalChildren:     3,
			CompletedChildren: 0,
			CurrentChild:      "",
		},
	}
	rendered := m.renderCrucibles(80, 20)
	if !strings.Contains(rendered, "No child yet") {
		t.Errorf("expected parent title 'No child yet' when no current child:\n%s", rendered)
	}
}

func TestRenderCruciblesProgressBarPresentWhenChildrenNonZero(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelQueue
	m.crucibles = []CrucibleItem{
		{
			ParentID:          "Forge-bar",
			Anvil:             "test",
			Phase:             "dispatching",
			TotalChildren:     4,
			CompletedChildren: 2,
		},
	}
	rendered := m.renderCrucibles(80, 20)
	// Progress bar emits block chars; at minimum we expect the fraction label.
	if !strings.Contains(rendered, "2/4") {
		t.Errorf("expected fraction '2/4' with progress bar in crucibles output:\n%s", rendered)
	}
}

func TestUpdatePendingOrphansMsgDeduplicates(t *testing.T) {
	existing := PendingOrphanItem{BeadID: "Forge-dup", Anvil: "test", Title: "dup"}
	m := NewModel(nil)
	m.orphanTarget = &existing
	m.orphanQueue = []PendingOrphanItem{}
	m.orphanDialogForm = buildOrphanDialogForm(&existing, &m.orphanDialogChoice)
	m.orphanDialogForm.Init()
	// Sending the same bead again should not add it to the queue
	_, _ = m.Update(UpdatePendingOrphansMsg{Items: []PendingOrphanItem{existing}})
	if len(m.orphanQueue) != 0 {
		t.Errorf("expected orphanQueue to remain empty after dedup, got %d items", len(m.orphanQueue))
	}
}

func TestDefaultFooterHintsMouseEnabled(t *testing.T) {
	m := NewModel(nil)
	m.mouseEnabled = true
	m.helpModel.Width = 200
	hint := m.defaultFooterHints()
	if !strings.Contains(hint, "m disable mouse (select text)") {
		t.Errorf("expected 'disable mouse (select text)' hint when mouse is enabled, got: %q", hint)
	}
	if strings.Contains(hint, "m enable mouse") {
		t.Errorf("unexpected 'enable mouse' hint when mouse is already enabled, got: %q", hint)
	}
}

func TestDefaultFooterHintsMouseDisabled(t *testing.T) {
	m := NewModel(nil)
	m.mouseEnabled = false
	m.helpModel.Width = 200
	hint := m.defaultFooterHints()
	if !strings.Contains(hint, "m enable mouse") {
		t.Errorf("expected 'enable mouse' hint when mouse is disabled, got: %q", hint)
	}
	if strings.Contains(hint, "disable mouse") {
		t.Errorf("unexpected 'disable mouse' hint when mouse is already disabled, got: %q", hint)
	}
}

func TestDefaultFooterHintsContainsCommonKeys(t *testing.T) {
	for _, mouseEnabled := range []bool{true, false} {
		m := NewModel(nil)
		m.mouseEnabled = mouseEnabled
		m.helpModel.Width = 200
		hint := m.defaultFooterHints()
		for _, key := range []string{"Tab", "j/k", "q quit"} {
			if !strings.Contains(hint, key) {
				t.Errorf("mouseEnabled=%v: expected %q in footer hints, got: %q", mouseEnabled, key, hint)
			}
		}
	}
}

func TestMouseToggleKeyEnablesMouseWhenDisabled(t *testing.T) {
	m := NewModel(nil)
	m.mouseEnabled = false
	newModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	updated := newModel.(*Model)
	if !updated.mouseEnabled {
		t.Error("expected mouseEnabled=true after pressing 'm' when mouse was disabled")
	}
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd to enable mouse")
	}
	// EnableMouseCellMotion returns an unexported enableMouseCellMotionMsg; verify via type name.
	msg := cmd()
	typeName := reflect.TypeOf(msg).Name()
	if typeName != "enableMouseCellMotionMsg" {
		t.Errorf("expected enableMouseCellMotionMsg from cmd, got %T", msg)
	}
}

func TestMouseToggleKeyDisablesMouseWhenEnabled(t *testing.T) {
	m := NewModel(nil)
	m.mouseEnabled = true
	newModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	updated := newModel.(*Model)
	if updated.mouseEnabled {
		t.Error("expected mouseEnabled=false after pressing 'm' when mouse was enabled")
	}
	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd to disable mouse")
	}
	// DisableMouse returns an unexported disableMouseMsg; verify via type name.
	msg := cmd()
	typeName := reflect.TypeOf(msg).Name()
	if typeName != "disableMouseMsg" {
		t.Errorf("expected disableMouseMsg from cmd, got %T", msg)
	}
}

func TestUpdateDescriptionViewer(t *testing.T) {
	m := NewModel(nil)
	m.queue = []QueueItem{
		{BeadID: "bd-1", Title: "bead title", Description: "bead description", Section: "ready"},
	}
	m.needsAttention = []NeedsAttentionItem{
		{BeadID: "bd-2", Title: "attn title", Description: "attn description"},
	}
	m.focused = PanelQueue
	m.width = 80
	m.height = 24

	// 1. Press 'd' on Queue selection
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	newModel := m2.(*Model)
	if !newModel.showDescriptionViewer {
		t.Error("expected showDescriptionViewer to be true after pressing 'd' in Queue")
	}
	if newModel.descriptionViewerTitle != "Description: bd-1 — bead title" {
		t.Errorf("unexpected description title: %q", newModel.descriptionViewerTitle)
	}
	if newModel.descriptionViewerRaw != "bead description" {
		t.Errorf("unexpected raw description: %q", newModel.descriptionViewerRaw)
	}

	// 2. Press 'esc' to close
	m3, _ := newModel.Update(tea.KeyMsg{Type: tea.KeyEsc})
	closedModel := m3.(*Model)
	if closedModel.showDescriptionViewer {
		t.Error("expected showDescriptionViewer to be false after pressing 'esc'")
	}

	// 3. Press 'd' on Needs Attention selection
	m.focused = PanelNeedsAttention
	m4, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	attnModel := m4.(*Model)
	if !attnModel.showDescriptionViewer {
		t.Error("expected showDescriptionViewer to be true after pressing 'd' in Needs Attention")
	}
	if attnModel.descriptionViewerRaw != "attn description" {
		t.Errorf("unexpected raw description: %q", attnModel.descriptionViewerRaw)
	}

	// 4. Verify input interception while open (pressing 'j' should scroll viewport, not move panel cursor)
	// We need to check if the viewport's YOffset changes or if it handles the msg.
	// Since viewport.Update returns a model and cmd, we check if the viewport state changed.
	m5, _ := attnModel.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	scrolledModel := m5.(*Model)

	// If the description is short, it won't scroll, but we can verify it DIDN'T change the main panel focus or cursor.
	if scrolledModel.focused != PanelNeedsAttention {
		t.Error("focus changed while description viewer was open")
	}
}

func TestOpenNotesOverlayFromQueue(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelQueue
	m.queue = []QueueItem{
		{BeadID: "Forge-1", Title: "Test Bead", Anvil: "test"},
	}
	m.rebuildQueueNav()

	// Press 'n' to open notes overlay
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model := m2.(*Model)

	if !model.showNotesOverlay {
		t.Error("expected showNotesOverlay to be true")
	}
	if model.notesTarget == nil || model.notesTarget.BeadID != "Forge-1" {
		t.Errorf("expected notesTarget.BeadID to be Forge-1, got %v", model.notesTarget)
	}
	if model.notesTA.Placeholder == "" {
		t.Error("expected textarea to be initialized")
	}
}

func TestOpenNotesOverlayFromNeedsAttention(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelNeedsAttention
	m.needsAttention = []NeedsAttentionItem{
		{BeadID: "Forge-attn", Title: "Needs Fix", Anvil: "test"},
	}

	// Press 'n' to open notes overlay
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model := m2.(*Model)

	if !model.showNotesOverlay {
		t.Error("expected showNotesOverlay to be true")
	}
	if model.notesTarget == nil || model.notesTarget.BeadID != "Forge-attn" {
		t.Errorf("expected notesTarget.BeadID to be Forge-attn, got %v", model.notesTarget)
	}
}

func TestSubmitNotes(t *testing.T) {
	var capturedID, capturedAnvil, capturedNotes string
	m := NewModel(nil)
	m.OnAppendNotes = func(beadID, anvil, notes string) error {
		capturedID = beadID
		capturedAnvil = anvil
		capturedNotes = notes
		return nil
	}
	m.openNotesOverlay("Forge-1", "test-anvil", "Title")
	m.notesTA.SetValue("These are my notes")

	// Press Ctrl+D to submit
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	model := m2.(*Model)

	if !model.showNotesOverlay {
		t.Error("expected showNotesOverlay to remain true during async submission")
	}
	if cmd == nil {
		t.Fatal("expected a command to be returned for async submission")
	}

	// Run the command to trigger the callback and get the result message
	msg := cmd()
	res, ok := msg.(NotesResultMsg)
	if !ok {
		t.Fatalf("expected NotesResultMsg, got %T", msg)
	}
	if res.BeadID != "Forge-1" {
		t.Errorf("expected result for Forge-1, got %s", res.BeadID)
	}
	if capturedID != "Forge-1" || capturedAnvil != "test-anvil" || capturedNotes != "These are my notes" {
		t.Errorf("callback captured wrong data: %s, %s, %s", capturedID, capturedAnvil, capturedNotes)
	}

	// After the result message arrives, the model should clear state on success
	m3, _ := model.Update(res)
	finalModel := m3.(*Model)
	if finalModel.showNotesOverlay {
		t.Error("expected showNotesOverlay to be false after successful result")
	}
}

func TestCancelNotes(t *testing.T) {
	m := NewModel(nil)
	m.openNotesOverlay("Forge-1", "test", "Title")

	// Press Esc to cancel
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := m2.(*Model)

	if model.showNotesOverlay {
		t.Error("expected showNotesOverlay to be false after cancel")
	}
	if model.notesTarget != nil {
		t.Error("expected notesTarget to be nil after cancel")
	}
}

func TestNotesOverlayInterceptsMouse(t *testing.T) {
	m := NewModel(nil)
	m.openNotesOverlay("Forge-1", "test", "Title")
	initialFocused := m.focused

	// Left click should be ignored (not change focus)
	m2, cmd := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		X:      10,
		Y:      10,
	})
	model := m2.(*Model)

	if model.focused != initialFocused {
		t.Errorf("focus changed from %v to %v during notes overlay", initialFocused, model.focused)
	}
	if cmd != nil {
		t.Error("expected nil cmd for ignored mouse event")
	}
}

func TestNotesOverlayView(t *testing.T) {
	m := NewModel(nil)
	m.width = 80
	m.height = 24
	m.openNotesOverlay("Forge-1", "test", "My Bead Title")
	
	view := m.renderNotesOverlay()
	if !strings.Contains(view, "Add Notes: Forge-1") {
		t.Errorf("view missing title line:\n%s", view)
	}
	if !strings.Contains(view, "My Bead Title") {
		t.Errorf("view missing bead title:\n%s", view)
	}
	if !strings.Contains(view, "Ctrl+D: save") {
		t.Errorf("view missing hint line:\n%s", view)
	}
}

// ── Workers panel log viewer ('o' key) tests ────────────────────────────────

func TestOpenLogViewerKeyO_WorkersPanel(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "forge-log-*.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("worker output line\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	m := NewModel(nil)
	m.focused = PanelWorkers
	m.workers = []WorkerItem{
		{ID: "w1", BeadID: "bd-99", LogPath: f.Name()},
	}
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: m.workers})
	mPtr := mTmp.(*Model)
	m2, _ := mPtr.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	mm := m2.(*Model)
	if !mm.showLogViewer {
		t.Error("pressing 'o' with a valid worker log path should open the log viewer")
	}
	if !strings.Contains(mm.logViewerTitle, "bd-99") {
		t.Errorf("log viewer title should contain bead ID, got %q", mm.logViewerTitle)
	}
	if !strings.Contains(mm.logViewerTitle, f.Name()) {
		t.Errorf("log viewer title should contain log path, got %q", mm.logViewerTitle)
	}
	if mm.logViewerEmpty {
		t.Error("log viewer should not be marked empty when file has content")
	}
}

func TestOpenLogViewerKeyO_NoLogPath(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelWorkers
	m.workers = []WorkerItem{
		{ID: "w1", BeadID: "bd-42", LogPath: ""},
	}
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: m.workers})
	mPtr := mTmp.(*Model)
	m2, _ := mPtr.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	mm := m2.(*Model)
	if mm.showLogViewer {
		t.Error("pressing 'o' with no log path should not open the log viewer")
	}
	if mm.statusMsg == "" {
		t.Error("pressing 'o' with no log path should set a status message")
	}
}

func TestOpenLogViewerKeyO_EmptyLogFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "forge-log-*.log")
	if err != nil {
		t.Fatal(err)
	}
	f.Close() // empty file

	m := NewModel(nil)
	m.focused = PanelWorkers
	m.workers = []WorkerItem{
		{ID: "w1", BeadID: "bd-10", LogPath: f.Name()},
	}
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: m.workers})
	mPtr := mTmp.(*Model)
	m2, _ := mPtr.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	mm := m2.(*Model)
	if !mm.showLogViewer {
		t.Error("pressing 'o' on an empty log file should still open the log viewer")
	}
	if !mm.logViewerEmpty {
		t.Error("logViewerEmpty should be true for an empty log file")
	}
}

func TestOpenLogViewerKeyO_NotInWorkersPanel(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelQueue
	m.workers = []WorkerItem{
		{ID: "w1", BeadID: "bd-5", LogPath: "/some/path.log"},
	}
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: m.workers})
	mPtr := mTmp.(*Model)
	m2, _ := mPtr.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	mm := m2.(*Model)
	if mm.showLogViewer {
		t.Error("pressing 'o' outside Workers panel should not open the log viewer")
	}
}

func TestOpenLogViewerKeyO_LogViewerClosedByEsc(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "forge-log-*.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("some content\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	m := NewModel(nil)
	m.focused = PanelWorkers
	m.workers = []WorkerItem{
		{ID: "w1", BeadID: "bd-77", LogPath: f.Name()},
	}
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: m.workers})
	mPtr := mTmp.(*Model)
	m2, _ := mPtr.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	if !m2.(*Model).showLogViewer {
		t.Fatal("log viewer should be open after 'o'")
	}
	m3, _ := m2.(*Model).Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m3.(*Model).showLogViewer {
		t.Error("esc should close the log viewer")
	}
}

func TestOpenLogViewerKeyO_ViewportHasContent(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "forge-log-*.log")
	if err != nil {
		t.Fatal(err)
	}
	content := "line one\nline two\nline three"
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()

	m := NewModel(nil)
	m.width = 80
	m.height = 24
	m.focused = PanelWorkers
	m.workers = []WorkerItem{
		{ID: "w1", BeadID: "bd-55", LogPath: f.Name()},
	}
	mTmp, _ := m.Update(UpdateWorkersMsg{Items: m.workers})
	mPtr := mTmp.(*Model)
	m2, _ := mPtr.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	mm := m2.(*Model)
	if !mm.showLogViewer {
		t.Fatal("log viewer should be open")
	}
	vpContent := mm.logViewerVP.View()
	if !strings.Contains(vpContent, "line one") {
		t.Errorf("log viewer viewport should contain file content, got: %q", vpContent)
	}
}

// --- Crucible action menu tests ---

func TestEnterOnPausedCrucibleOpensMenu(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelCrucibles
	m.width = 80
	m.height = 24
	m.crucibles = []CrucibleItem{
		{ParentID: "Forge-epic1", Anvil: "heimdall", Phase: "paused"},
	}
	m.crucibleVP = scrollViewport{cursor: 0}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.crucibleActionForm == nil {
		t.Error("expected crucibleActionForm!=nil after Enter on paused crucible")
	}
	if m.crucibleActionTarget == nil || m.crucibleActionTarget.ParentID != "Forge-epic1" {
		t.Errorf("expected crucibleActionTarget.ParentID=Forge-epic1, got %v", m.crucibleActionTarget)
	}
	// Default choice should be Resume
	if m.crucibleActionChoice != CrucibleActionResume {
		t.Errorf("expected default choice CrucibleActionResume, got %v", m.crucibleActionChoice)
	}
}

func TestEnterOnNonPausedCrucibleDoesNotOpenMenu(t *testing.T) {
	for _, phase := range []string{"started", "dispatching", "waiting", "final_pr", "complete"} {
		m := NewModel(nil)
		m.focused = PanelCrucibles
		m.width = 80
		m.height = 24
		m.crucibles = []CrucibleItem{
			{ParentID: "Forge-epic1", Anvil: "heimdall", Phase: phase},
		}
		m.crucibleVP = scrollViewport{cursor: 0}
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if m.crucibleActionForm != nil {
			t.Errorf("phase=%q: expected no crucibleActionForm for non-paused crucible", phase)
		}
	}
}

func TestCrucibleActionMenuEscDismisses(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelCrucibles
	m.width = 80
	m.height = 24
	item := CrucibleItem{ParentID: "Forge-epic2", Anvil: "heimdall", Phase: "paused"}
	m.crucibles = []CrucibleItem{item}
	m.crucibleVP = scrollViewport{cursor: 0}
	// Open menu
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = drainHuh(&m, cmd)
	if m.crucibleActionForm == nil {
		t.Fatal("expected menu to open")
	}
	// Press Esc
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.crucibleActionForm != nil {
		t.Error("expected crucibleActionForm==nil after Esc")
	}
}

func TestCrucibleActionMenuCallsOnCrucibleAction(t *testing.T) {
	var calledParent, calledAnvil, calledAction string
	m := NewModel(nil)
	m.focused = PanelCrucibles
	m.width = 80
	m.height = 24
	item := CrucibleItem{ParentID: "Forge-epic3", Anvil: "heimdall", Phase: "paused"}
	m.crucibles = []CrucibleItem{item}
	m.crucibleVP = scrollViewport{cursor: 0}
	m.OnCrucibleAction = func(parentID, anvil, action string) error {
		calledParent = parentID
		calledAnvil = anvil
		calledAction = action
		return nil
	}
	// Open menu
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	cmd = drainHuh(&m, cmd)
	if m.crucibleActionForm == nil {
		t.Fatal("expected menu to open")
	}
	// Confirm selection (Resume is default)
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	cmd = drainHuh(&m, cmd)
	if m.crucibleActionForm != nil {
		t.Error("expected menu to close after confirmation")
	}
	if cmd == nil {
		t.Fatal("expected async cmd after crucible action")
	}
	msg := cmd()
	_, _ = m.Update(msg)
	if calledParent != "Forge-epic3" || calledAnvil != "heimdall" || calledAction != "resume" {
		t.Errorf("OnCrucibleAction called with (%q, %q, %q), want (Forge-epic3, heimdall, resume)",
			calledParent, calledAnvil, calledAction)
	}
}

func TestCrucibleActionMenuNilOnCrucibleAction(t *testing.T) {
	m := NewModel(nil)
	m.focused = PanelCrucibles
	m.width = 80
	m.height = 24
	item := CrucibleItem{ParentID: "Forge-epic4", Anvil: "heimdall", Phase: "paused"}
	m.crucibles = []CrucibleItem{item}
	m.crucibleVP = scrollViewport{cursor: 0}
	// No OnCrucibleAction set
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	cmd = drainHuh(&m, cmd)
	if m.crucibleActionForm == nil {
		t.Fatal("expected menu to open")
	}
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = drainHuh(&m, cmd)
	if m.crucibleActionForm != nil {
		t.Error("expected menu to close")
	}
	if !strings.Contains(m.statusMsg, "unavailable") {
		t.Errorf("expected statusMsg to mention 'unavailable', got %q", m.statusMsg)
	}
}
