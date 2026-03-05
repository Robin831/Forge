package smith

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestLogFile creates a temp log file for readStreamJSON calls.
func newTestLogFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "smith.log"))
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	return f
}

func TestReadStreamJSON_ResultEvent(t *testing.T) {
	input := `{"type":"result","subtype":"success","result":"All done.","total_cost_usd":0.0123,"usage":{"input_tokens":100,"output_tokens":50}}`

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.Equal(t, "success", result.ResultSubtype)
	assert.InDelta(t, 0.0123, result.CostUSD, 1e-6)
	assert.Equal(t, 100, result.TokensIn)
	assert.Equal(t, 50, result.TokensOut)
	assert.Equal(t, "All done.", result.FullOutput)
	assert.False(t, result.RateLimited)
}

func TestReadStreamJSON_ResultEvent_ErrorSubtype(t *testing.T) {
	// error_max_turns: no "result" field, is_error=false — not a rate limit
	input := `{"type":"result","subtype":"error_max_turns","is_error":false}`

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.Equal(t, "error_max_turns", result.ResultSubtype)
	assert.False(t, result.RateLimited)
}

func TestReadStreamJSON_ResultEvent_IsErrorRateLimit(t *testing.T) {
	// is_error=true + rate-limit text in result → RateLimited
	input := `{"type":"result","subtype":"success","is_error":true,"result":"rate limit exceeded"}`

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.True(t, result.RateLimited)
}

func TestReadStreamJSON_RateLimitEvent_Warning(t *testing.T) {
	// status=warning → should NOT set RateLimited
	input := `{"type":"rate_limit_event","rate_limit_info":{"status":"warning","requests_remaining":5,"requests_limit":100}}`

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.False(t, result.RateLimited)
	// Quota should still be populated
	require.NotNil(t, result.Quota)
	assert.Equal(t, 100, result.Quota.RequestsLimit)
	assert.Equal(t, 5, result.Quota.RequestsRemaining)
}

func TestReadStreamJSON_RateLimitEvent_Blocked(t *testing.T) {
	// status=blocked → should set RateLimited
	input := `{"type":"rate_limit_event","rate_limit_info":{"status":"blocked","requests_remaining":0,"requests_limit":100}}`

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.True(t, result.RateLimited)
}

func TestReadStreamJSON_RateLimitEvent_EmptyStatus(t *testing.T) {
	// status="" (unknown) → treat as blocking (conservative)
	input := `{"type":"rate_limit_event","rate_limit_info":{"status":""}}`

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.True(t, result.RateLimited)
}

func TestReadStreamJSON_RateLimitEvent_ResetAt(t *testing.T) {
	// reset_at is an RFC3339 timestamp
	input := `{"type":"rate_limit_event","rate_limit_info":{"status":"blocked","reset_at":"2025-01-01T12:00:00Z","requests_limit":100,"requests_remaining":0}}`

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.True(t, result.RateLimited)
	require.NotNil(t, result.Quota)
	require.NotNil(t, result.Quota.RequestsReset)
	assert.Equal(t, 2025, result.Quota.RequestsReset.Year())
}

func TestReadStreamJSON_AssistantMessage(t *testing.T) {
	// Assistant message events accumulate text for FullOutput fallback
	input := `{"type":"assistant","message":{"content":[{"type":"text","text":"Hello world"}]}}`

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	// FullOutput should come from accumulated assistant text when no result event
	assert.Equal(t, "Hello world", result.FullOutput)
}

func TestReadStreamJSON_AssistantMessage_MultipleBlocks(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Part1"},{"type":"tool_use","id":"x"},{"type":"text","text":"Part2"}]}}`,
		`{"type":"result","subtype":"success","result":""}`,
	}
	input := strings.Join(lines, "\n")

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	// result event has empty "result" → fall back to accumulated assistant text
	assert.Equal(t, "Part1Part2", result.FullOutput)
}

func TestReadStreamJSON_GeminiDeltaMessage(t *testing.T) {
	// Gemini emits {type:"message",role:"assistant",content:"..."}
	lines := []string{
		`{"type":"message","role":"assistant","content":"Hello from Gemini"}`,
		`{"type":"message","role":"assistant","content":" and more"}`,
	}
	input := strings.Join(lines, "\n")

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.Equal(t, "Hello from Gemini and more", result.FullOutput)
}

func TestReadStreamJSON_GeminiResultStats(t *testing.T) {
	input := `{"type":"result","subtype":"success","stats":{"requests_limit":60,"requests_used":5,"tokens_limit":1000000,"tokens_used":500}}`

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	require.NotNil(t, result.Quota)
	assert.Equal(t, 60, result.Quota.RequestsLimit)
	assert.Equal(t, 55, result.Quota.RequestsRemaining) // 60 - 5
	assert.Equal(t, 1000000, result.Quota.TokensLimit)
	assert.Equal(t, 999500, result.Quota.TokensRemaining) // 1000000 - 500
}

func TestReadStreamJSON_NonJSONLinesIgnored(t *testing.T) {
	// Non-JSON lines should be buffered but not panic
	input := "not json at all\n{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"ok\"}"

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.Equal(t, "ok", result.FullOutput)
}

func TestReadStreamJSON_ContentFieldSetsLastContent(t *testing.T) {
	// content field on an event is used as summary
	input := `{"type":"content","content":"Some visible content"}`

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.Equal(t, "Some visible content", result.Summary)
}

func TestReadStreamJSON_EmptyInput(t *testing.T) {
	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(""), &buf, newTestLogFile(t), result)

	assert.Empty(t, result.FullOutput)
	assert.Empty(t, result.Summary)
	assert.False(t, result.RateLimited)
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"short string unchanged", "hello", 10, "hello"},
		{"exact length unchanged", "hello", 5, "hello"},
		{"long string truncated with ellipsis", "hello world", 8, "hello..."},
		{"empty string", "", 10, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, truncate(tt.input, tt.maxLen))
		})
	}
}
