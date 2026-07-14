package forgechat

import (
	"encoding/json"
	"testing"

	"github.com/Robin831/Forge/internal/smith"
)

// TestDecodeStreamEvent_InterleavesTextAndTools feeds a realistic Claude
// stream-json sequence — assistant text, a text+tool_use block, a user
// tool_result, then more text — and asserts decodeStreamEvent yields the
// chunks in arrival order with text and tool events correctly interleaved.
// Events with no consumer payload (result, system) yield nothing.
func TestDecodeStreamEvent_InterleavesTextAndTools(t *testing.T) {
	var got []StreamChunk
	onChunk := func(c StreamChunk) { got = append(got, c) }

	events := []smith.StreamEvent{
		{Type: "system", Subtype: "init"},
		{Type: "assistant", Message: json.RawMessage(`{"content":[{"type":"text","text":"Hel"}]}`)},
		{Type: "assistant", Message: json.RawMessage(`{"content":[{"type":"text","text":"lo"},{"type":"tool_use","name":"Read","id":"t1"}]}`)},
		{Type: "user", Message: json.RawMessage(`{"content":[{"type":"tool_result","tool_use_id":"t1"}]}`)},
		{Type: "assistant", Message: json.RawMessage(`{"content":[{"type":"text","text":" world"}]}`)},
		{Type: "result", Subtype: "success", Result: "Hello world"},
	}
	for _, ev := range events {
		decodeStreamEvent(ev, onChunk)
	}

	want := []StreamChunk{
		{Kind: StreamChunkText, Text: "Hel"},
		{Kind: StreamChunkText, Text: "lo"},
		{Kind: StreamChunkToolUse, ToolName: "Read", ToolID: "t1"},
		{Kind: StreamChunkToolResult, ToolID: "t1"},
		{Kind: StreamChunkText, Text: " world"},
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d chunks, got %d: %+v", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("chunk %d: want %+v, got %+v", i, w, got[i])
		}
	}
}

// TestDecodeStreamEvent_GeminiDelta covers the Gemini-style flat delta event
// shape ({type:"message",role:"assistant",content:"..."}).
func TestDecodeStreamEvent_GeminiDelta(t *testing.T) {
	var got []StreamChunk
	decodeStreamEvent(smith.StreamEvent{Type: "message", Role: "assistant", Content: "partial"}, func(c StreamChunk) {
		got = append(got, c)
	})
	if len(got) != 1 || got[0].Kind != StreamChunkText || got[0].Text != "partial" {
		t.Fatalf("expected one text chunk %q, got %+v", "partial", got)
	}
}

// TestDecodeStreamEvent_MalformedAndEmpty ensures a garbled message payload is
// skipped (not panicked on) and a nil callback is a no-op.
func TestDecodeStreamEvent_MalformedAndEmpty(t *testing.T) {
	// Nil callback must not panic.
	decodeStreamEvent(smith.StreamEvent{Type: "assistant", Message: json.RawMessage(`{"content":[{"type":"text","text":"x"}]}`)}, nil)

	var got []StreamChunk
	onChunk := func(c StreamChunk) { got = append(got, c) }
	// Malformed JSON in the message → skipped.
	decodeStreamEvent(smith.StreamEvent{Type: "assistant", Message: json.RawMessage(`{not json`)}, onChunk)
	// Empty assistant message → skipped.
	decodeStreamEvent(smith.StreamEvent{Type: "assistant"}, onChunk)
	if len(got) != 0 {
		t.Fatalf("malformed/empty events should yield no chunks, got %+v", got)
	}
}
