package forgechat

import "context"

// StreamChunkKind classifies one incremental slice of a turn's live output.
type StreamChunkKind string

const (
	// StreamChunkText is an incremental slice of assistant prose. Concatenating
	// every StreamChunkText in arrival order reconstructs the streamed reply.
	StreamChunkText StreamChunkKind = "text"
	// StreamChunkToolUse marks the start of a tool invocation by the assistant.
	StreamChunkToolUse StreamChunkKind = "tool_use"
	// StreamChunkToolResult marks a tool result flowing back into the session.
	StreamChunkToolResult StreamChunkKind = "tool_result"
)

// StreamChunk is one incremental event delivered to a StreamFunc as the
// provider streams its response. Text is populated for StreamChunkText;
// ToolName / ToolID identify the tool for the tool_use / tool_result kinds
// (ToolID lets a consumer correlate a result back to its invocation).
type StreamChunk struct {
	Kind     StreamChunkKind
	Text     string
	ToolName string
	ToolID   string
}

// StreamFunc consumes incremental chunks. It is invoked synchronously in
// provider arrival order from the runner's stdout reader, so implementations
// must return promptly and must not block on the turn completing.
type StreamFunc func(StreamChunk)

// StreamingRunner is an optional extension of Runner. A Runner that implements
// it delivers incremental chunks via onChunk as they arrive and still returns
// the same final TurnResponse that Turn would. Callers detect support with a
// type assertion and fall back to Runner.Turn when it is absent, so streaming
// is purely additive — the final/complete semantics are unchanged.
type StreamingRunner interface {
	Runner
	TurnStream(ctx context.Context, req TurnRequest, onChunk StreamFunc) (*TurnResponse, error)
}
