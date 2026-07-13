package smith

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogFileName(t *testing.T) {
	logSeq.Store(0)

	// Empty prefix defaults to "smith" to preserve historical naming.
	assert.Equal(t, "smith-42-1.log", logFileName("", 42))
	// Stage callers get a stage-identifiable filename.
	assert.Equal(t, "warden-100-2.log", logFileName("warden", 100))
	assert.Equal(t, "quench-7-3.log", logFileName("quench", 7))
	// Path-traversal prefixes are sanitised to a safe basename.
	assert.Equal(t, "foo-1-4.log", logFileName("../foo", 1))
	assert.Equal(t, "bar-2-5.log", logFileName("a/b/bar", 2))
	assert.Equal(t, "smith-3-6.log", logFileName("../..", 3))
	assert.Equal(t, "smith-4-7.log", logFileName(".", 4))
	// Bare path separators must not escape the log directory.
	assert.Equal(t, "smith-5-8.log", logFileName("/", 5))
	assert.Equal(t, "smith-6-9.log", logFileName("///", 6))
}

func TestLogFileName_NoDuplicates(t *testing.T) {
	logSeq.Store(0)
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		name := logFileName("smith", 1000)
		if seen[name] {
			t.Fatalf("duplicate log filename: %s", name)
		}
		seen[name] = true
	}
}

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

func TestReadStreamJSON_NonJSONRateLimitDetected(t *testing.T) {
	// Non-JSON plain-text lines containing quota/rate-limit phrases (e.g. from
	// the Copilot CLI when premium request quota is exhausted) must set RateLimited
	// so the provider fallback chain triggers instead of treating this as a generic
	// failure.
	cases := []struct {
		name  string
		input string
	}{
		{"copilot premium request", "You've exceeded your premium request quota for this month."},
		{"copilot request quota", "Your request quota has been exceeded. Please try again later."},
		{"generic rate limit", "rate limit exceeded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			result := &Result{}
			readStreamJSON(strings.NewReader(tc.input), &buf, newTestLogFile(t), result)
			assert.True(t, result.RateLimited, "expected RateLimited=true for input: %q", tc.input)
		})
	}
}

func TestReadStreamJSON_NonJSONNonRateLimitNotFlagged(t *testing.T) {
	// Ordinary non-JSON output (e.g. a startup banner) must NOT set RateLimited.
	input := "Initializing Copilot extension...\nnot json"

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.False(t, result.RateLimited)
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

// TestWaitWithExitTimeout_NormalExit verifies that when the process exits
// before the timeout, WaitWithExitTimeout returns the result immediately.
func TestWaitWithExitTimeout_NormalExit(t *testing.T) {
	result := &Result{ResultSubtype: "success", IsError: false, ExitCode: 0}
	p := NewProcessForTest(result) // both ioDone and done are already closed

	got := p.WaitWithExitTimeout(5 * time.Second)

	assert.Equal(t, result, got)
	assert.Equal(t, 0, got.ExitCode)
}

// TestWaitWithExitTimeout_TimeoutKillSuccess verifies that when the process
// exceeds the exit deadline and is killed, a success-subtype result has its
// exit code normalized to 0 so downstream checks don't misclassify the push.
func TestWaitWithExitTimeout_TimeoutKillSuccess(t *testing.T) {
	result := &Result{ResultSubtype: "success", IsError: false, ExitCode: 1}
	doneCh := make(chan struct{})
	p := &Process{
		done:   doneCh,
		ioDone: make(chan struct{}),
		result: result,
		// onKill closes doneCh deterministically when Kill() is invoked,
		// eliminating the need for time.Sleep-based synchronization.
		onKill: func() { close(doneCh) },
	}
	close(p.ioDone) // I/O is complete; process has not exited yet

	got := p.WaitWithExitTimeout(10 * time.Millisecond) // timeout fires, Kill() fires onKill

	assert.Equal(t, "success", got.ResultSubtype)
	assert.Equal(t, 0, got.ExitCode, "exit code must be normalized to 0 for a successful session that was killed")
}

// TestWaitWithExitTimeout_TimeoutKillError verifies that a non-success result
// does NOT have its exit code normalized when the process is killed after the
// deadline, preserving error diagnostics.
func TestWaitWithExitTimeout_TimeoutKillError(t *testing.T) {
	result := &Result{ResultSubtype: "error_max_turns", IsError: false, ExitCode: 1}
	doneCh := make(chan struct{})
	p := &Process{
		done:   doneCh,
		ioDone: make(chan struct{}),
		result: result,
		// onKill closes doneCh deterministically when Kill() is invoked.
		onKill: func() { close(doneCh) },
	}
	close(p.ioDone)

	got := p.WaitWithExitTimeout(10 * time.Millisecond)

	assert.Equal(t, "error_max_turns", got.ResultSubtype)
	assert.Equal(t, 1, got.ExitCode, "exit code must not be normalized for a non-success session")
}

// TestBuildChildEnv_StripsInheritedGitVars is the core regression assertion
// for Forge-v48n: GIT_DIR / GIT_WORK_TREE / GIT_CEILING_DIRECTORIES from the
// daemon's own environment must NEVER reach the AI agent subprocess. If they
// did, a child running git from any cwd could resolve back to whatever repo
// the daemon was started in (or a stale worktree) instead of the worktree we
// just bound it to.
func TestBuildChildEnv_StripsInheritedGitVars(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"GIT_DIR=/some/stale/.git",
		"GIT_WORK_TREE=/some/stale",
		"GIT_CEILING_DIRECTORIES=/",
		"CLAUDECODE=1",
		"HOME=/home/user",
	}
	got := buildChildEnv(parent, nil, nil)

	for _, e := range got {
		if strings.HasPrefix(e, "GIT_DIR=") ||
			strings.HasPrefix(e, "GIT_WORK_TREE=") ||
			strings.HasPrefix(e, "GIT_CEILING_DIRECTORIES=") ||
			strings.HasPrefix(e, "CLAUDECODE=") {
			t.Errorf("inherited variable %q must be stripped", e)
		}
	}

	want := map[string]bool{"PATH=/usr/bin": false, "HOME=/home/user": false}
	for _, e := range got {
		if _, ok := want[e]; ok {
			want[e] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("expected %q to survive, but it was filtered out", k)
		}
	}
}

// TestBuildChildEnv_WorktreeGitEnvWins verifies that when the parent process
// has a stale GIT_DIR but a worktree gitEnv is provided, the child sees ONLY
// the worktree's git env — not the parent's stale value.
func TestBuildChildEnv_WorktreeGitEnvWins(t *testing.T) {
	parent := []string{
		"GIT_DIR=/parent/repo/.git",
		"GIT_WORK_TREE=/parent/repo",
	}
	gitEnv := []string{
		"GIT_DIR=/anvil/.git/worktrees/bead",
		"GIT_WORK_TREE=/anvil/.workers/bead",
		"GIT_CEILING_DIRECTORIES=/anvil/.workers",
	}
	got := buildChildEnv(parent, nil, gitEnv)

	gitDirCount := 0
	gitWorkTreeCount := 0
	for _, e := range got {
		if strings.HasPrefix(e, "GIT_DIR=") {
			gitDirCount++
			if e != "GIT_DIR=/anvil/.git/worktrees/bead" {
				t.Errorf("unexpected GIT_DIR value: %q", e)
			}
		}
		if strings.HasPrefix(e, "GIT_WORK_TREE=") {
			gitWorkTreeCount++
			if e != "GIT_WORK_TREE=/anvil/.workers/bead" {
				t.Errorf("unexpected GIT_WORK_TREE value: %q", e)
			}
		}
	}
	if gitDirCount != 1 {
		t.Errorf("expected exactly one GIT_DIR entry, got %d", gitDirCount)
	}
	if gitWorkTreeCount != 1 {
		t.Errorf("expected exactly one GIT_WORK_TREE entry, got %d", gitWorkTreeCount)
	}
}

// TestBuildChildEnv_ProviderEnvOverrides verifies that provider-supplied env
// values replace any inherited entry with the same key, ensuring per-provider
// overrides (e.g. Ollama backend host) reach the child cleanly.
func TestBuildChildEnv_ProviderEnvOverrides(t *testing.T) {
	parent := []string{"OLLAMA_HOST=stale", "PATH=/usr/bin"}
	providerEnv := map[string]string{"OLLAMA_HOST": "http://localhost:11434"}
	got := buildChildEnv(parent, providerEnv, nil)

	hostCount := 0
	for _, e := range got {
		if strings.HasPrefix(e, "OLLAMA_HOST=") {
			hostCount++
			if e != "OLLAMA_HOST=http://localhost:11434" {
				t.Errorf("unexpected OLLAMA_HOST value: %q", e)
			}
		}
	}
	if hostCount != 1 {
		t.Errorf("expected exactly one OLLAMA_HOST entry, got %d", hostCount)
	}
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
