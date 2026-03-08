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
		focused: PanelWorkers,
		width:   120,
		height:  40,
	}

	topHeight, bottomHeight := m.getVerticalSplit(1, 1)
	width := 40

	leftColumn := m.renderLeftColumn(width, topHeight, bottomHeight)
	workerPanel := m.renderWorkers(width, topHeight+bottomHeight)
	rightColumn := m.renderRightColumn(width, topHeight, bottomHeight)

	leftLines := strings.Count(strings.TrimRight(leftColumn, "\n"), "\n") + 1
	workerLines := strings.Count(strings.TrimRight(workerPanel, "\n"), "\n") + 1
	rightLines := strings.Count(strings.TrimRight(rightColumn, "\n"), "\n") + 1

	if leftLines != workerLines {
		t.Errorf("left column produced %d lines, workers produced %d lines — should match", leftLines, workerLines)
	}
	if rightLines != workerLines {
		t.Errorf("right column produced %d lines, workers produced %d lines — should match", rightLines, workerLines)
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
		workerVP: scrollViewport{cursor: 0},
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

func TestActivityScrollChangesVisibleLine(t *testing.T) {
	lines := []string{"line-0", "line-1", "line-2", "line-3", "line-4"}
	m := Model{
		workers: []WorkerItem{
			{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
				ActivityLines: lines},
		},
		workerVP: scrollViewport{cursor: 0},
		activityScroll: 0,
		focused:        PanelLiveActivity,
	}

	// With no scroll, the newest entry (line-4) should be visible.
	rendered0 := m.renderWorkerActivity(80, 10)
	if !strings.Contains(rendered0, "line-4") {
		t.Errorf("expected newest entry 'line-4' visible at scroll=0:\n%s", rendered0)
	}

	// Scroll down (toward older entries) so that line-4 is scrolled off.
	m.activityScroll = len(lines) - 1 // scroll to the oldest
	renderedScrolled := m.renderWorkerActivity(80, 10)
	if !strings.Contains(renderedScrolled, "line-0") {
		t.Errorf("expected oldest entry 'line-0' visible when scrolled to end:\n%s", renderedScrolled)
	}
}

func TestActivityScrollClampPastEnd(t *testing.T) {
	lines := []string{"alpha", "beta", "gamma"}
	// activityScroll larger than len(lines)-1 simulates a stale scroll after
	// the worker's activity list shrinks on refresh.
	m := Model{
		workers: []WorkerItem{
			{ID: "w1", BeadID: "bd-1", Anvil: "test", Status: "running", Duration: "1m", Type: "smith",
				ActivityLines: lines},
		},
		workerVP: scrollViewport{cursor: 0},
		activityScroll: 100, // way past the end
		focused:        PanelLiveActivity,
	}

	rendered := m.renderWorkerActivity(80, 10)
	// The panel must not be blank — at least the oldest entry should show.
	if !strings.Contains(rendered, "alpha") {
		t.Errorf("expected at least oldest entry 'alpha' when activityScroll is past end:\n%s", rendered)
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
	// Very long description should be capped at 3 lines with ellipsis on the last line.
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
