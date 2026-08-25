package smith

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResumeUnavailable(t *testing.T) {
	tests := []struct {
		name string
		r    *Result
		want bool
	}{
		{"nil result is unavailable", nil, true},
		{
			"genuine success is available",
			&Result{ResultSubtype: "success", FullOutput: "done"},
			false,
		},
		{
			"success is available even if output mentions a session",
			&Result{ResultSubtype: "success", FullOutput: "resumed session abc"},
			false,
		},
		{
			"missing transcript in stderr is unavailable",
			&Result{ExitCode: 1, ErrorOutput: "Error: No conversation found with session ID: sess-1"},
			true,
		},
		{
			"session not found in full output is unavailable",
			&Result{ResultSubtype: "error", FullOutput: "session not found"},
			true,
		},
		{
			"case-insensitive marker match",
			&Result{ErrorOutput: "UNABLE TO RESUME the requested session"},
			true,
		},
		{
			"rate limit is not a missing transcript",
			&Result{RateLimited: true, ErrorOutput: "no conversation found"},
			false,
		},
		{
			"auth failure is not a missing transcript",
			&Result{AuthFailed: true, ErrorOutput: "session not found"},
			false,
		},
		{
			"an unrelated failure without a marker is not reported unavailable",
			&Result{ExitCode: 1, ErrorOutput: "compilation failed"},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ResumeUnavailable(tt.r))
		})
	}
}

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

// TestReadStreamJSONEvents_CallbackFiresPerEventInOrder verifies the streaming
// callback added for the Beads-Forge chat SSE stream: every successfully-parsed
// event is delivered to onEvent in arrival order, before the aggregate Result
// is finalised, so a consumer can forward text deltas and tool events live.
func TestReadStreamJSONEvents_CallbackFiresPerEventInOrder(t *testing.T) {
	input := `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"a"}]}}
not-json-should-be-skipped
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","id":"t1"}]}}
{"type":"result","subtype":"success","result":"a"}
`
	var seen []string
	var buf strings.Builder
	result := &Result{}
	readStreamJSONEvents(strings.NewReader(input), &buf, newTestLogFile(t), result, func(ev StreamEvent) {
		seen = append(seen, ev.Type)
	})

	// The non-JSON line is skipped; only the four parseable events fire.
	assert.Equal(t, []string{"system", "assistant", "assistant", "result"}, seen)
	// The aggregate Result is still populated as before.
	assert.Equal(t, "success", result.ResultSubtype)
	assert.Equal(t, "a", result.FullOutput)
}

// TestReadStreamJSON_NilCallbackUnchanged confirms the nil-callback wrapper is
// a pure passthrough to the historical aggregation behaviour.
func TestReadStreamJSON_NilCallbackUnchanged(t *testing.T) {
	input := `{"type":"result","subtype":"success","result":"done"}`
	var buf strings.Builder
	result := &Result{}
	readStreamJSONEvents(strings.NewReader(input), &buf, newTestLogFile(t), result, nil)
	assert.Equal(t, "done", result.FullOutput)
}

func TestReadStreamJSON_ResultEvent(t *testing.T) {
	// The usage object carries the provider's prompt-cache accounting under
	// cache_creation_input_tokens/cache_read_input_tokens. Those two field
	// names are the origin of every cache number Forge reports, and getting
	// them wrong fails silently — the telemetry line omits cache_w/cache_r
	// when both are zero, so a typo reverts the log to its old shape rather
	// than to anything that looks broken.
	input := `{"type":"result","subtype":"success","result":"All done.","total_cost_usd":0.0123,"num_turns":4,` +
		`"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":41500,"cache_read_input_tokens":900}}`

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.Equal(t, "success", result.ResultSubtype)
	assert.Equal(t, 4, result.NumTurns)
	assert.InDelta(t, 0.0123, result.CostUSD, 1e-6)
	assert.Equal(t, 100, result.TokensIn)
	assert.Equal(t, 50, result.TokensOut)
	assert.Equal(t, 41500, result.CacheCreationTokens)
	assert.Equal(t, 900, result.CacheReadTokens)
	assert.Equal(t, "All done.", result.FullOutput)
	assert.False(t, result.RateLimited)
}

// TestReadStreamJSON_ResultEvent_NoCacheUsage pins the other half: a provider
// whose usage object reports no cache accounting leaves both counters at zero,
// which is what every downstream surface reads as "this backend reports none"
// and renders as the line it always did.
func TestReadStreamJSON_ResultEvent_NoCacheUsage(t *testing.T) {
	input := `{"type":"result","subtype":"success","result":"All done.","usage":{"input_tokens":100,"output_tokens":50}}`

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.Equal(t, 0, result.CacheCreationTokens)
	assert.Equal(t, 0, result.CacheReadTokens)
}

func TestReadStreamJSON_ResultEvent_ErrorSubtype(t *testing.T) {
	// error_max_turns: no "result" field, is_error=false — not a rate limit.
	// num_turns is the whole point of this subtype for a caller tuning a turn
	// budget: it says how many turns the session actually got through.
	input := `{"type":"result","subtype":"error_max_turns","is_error":false,"num_turns":12}`

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.Equal(t, "error_max_turns", result.ResultSubtype)
	assert.Equal(t, 12, result.NumTurns)
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

func TestReadStreamJSON_SessionIDAndModel(t *testing.T) {
	// Claude emits session_id + model on the initial system init event and
	// repeats session_id on the result event. The first non-empty values win.
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"abc-123","model":"claude-opus-4-8"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}`,
		`{"type":"result","subtype":"success","session_id":"abc-123","result":"done","total_cost_usd":0.01}`,
	}, "\n")

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.Equal(t, "abc-123", result.SessionID)
	assert.Equal(t, "claude-opus-4-8", result.Model)
}

func TestReadStreamJSON_SessionIDFromResultOnly(t *testing.T) {
	// Even if the init event is missing, the session_id on the result event
	// is captured.
	input := `{"type":"result","subtype":"success","session_id":"only-on-result","result":"done"}`

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.Equal(t, "only-on-result", result.SessionID)
	assert.Empty(t, result.Model)
}

func TestReadStreamJSON_NoSessionIDForNonClaude(t *testing.T) {
	// Gemini-style deltas + result carry no session_id/model — both stay empty
	// without any error.
	input := strings.Join([]string{
		`{"type":"message","role":"assistant","content":"hi","delta":true}`,
		`{"type":"result","subtype":"success","stats":{"input_tokens":10,"output_tokens":5}}`,
	}, "\n")

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.Empty(t, result.SessionID)
	assert.Empty(t, result.Model)
}

func TestSessionModel(t *testing.T) {
	// Prefers the stream-reported model when present.
	assert.Equal(t, "claude-opus-4-8",
		SessionModel(&Result{Model: "claude-opus-4-8"}, provider.Provider{Kind: provider.Claude, Model: "cfg-model"}))
	// Falls back to the provider-configured model when the stream is silent.
	assert.Equal(t, "cfg-model",
		SessionModel(&Result{}, provider.Provider{Kind: provider.Claude, Model: "cfg-model"}))
	// Nil result is safe.
	assert.Equal(t, "cfg-model",
		SessionModel(nil, provider.Provider{Model: "cfg-model"}))
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

// TestProcessInterrupt_ReapsRunningStub verifies that Interrupt unblocks a
// running test process and Wait returns the captured result — including the
// session_id — so steer mode can resume the session.
func TestProcessInterrupt_ReapsRunningStub(t *testing.T) {
	res := &Result{ExitCode: 0, SessionID: "sess-1"}
	p := NewRunningProcessForTest(res)
	if !p.IsRunning() {
		t.Fatal("stub process should report running before interrupt")
	}

	p.Interrupt(time.Second)

	got := p.Wait()
	if got.SessionID != "sess-1" {
		t.Fatalf("session_id must be preserved across interrupt, got %q", got.SessionID)
	}
	if p.IsRunning() {
		t.Fatal("process should be done after interrupt")
	}
}

// TestSpawnOptions_ResumeSessionID confirms the resume flag composes onto the
// provider args (Claude), which is what SpawnWithOptions appends before exec.
func TestSpawnOptions_ResumeSessionID(t *testing.T) {
	pv := provider.Provider{Kind: provider.Claude}
	args := pv.BuildArgs(nil)
	args = append(args, pv.ResumeFlag("sess-9")...)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--resume sess-9") {
		t.Fatalf("resume args should include --resume sess-9, got %q", joined)
	}
}

// TestResultAnswered pins the one predicate every "did the session actually
// answer" decision reads — the killed-process exit-code normalisation, the
// rate-limit/auth clearing, ResumeUnavailable, and Assay's spend-ceiling
// exception. It is a method rather than the same two conditions re-derived at
// each site so a new subtype that counts as an answer changes one place.
func TestResultAnswered(t *testing.T) {
	cases := []struct {
		name string
		res  *Result
		want bool
	}{
		{"nil", nil, false},
		{"success", &Result{ResultSubtype: "success"}, true},
		// Claude exits 2 on its own rate-limit code even after recovering
		// internally; the subtype, not the exit code, says the model answered.
		{"success despite exit code", &Result{ResultSubtype: "success", ExitCode: 2}, true},
		// subtype "success" with is_error true is a rate-limit rejection
		// wearing a success label — no work was done.
		{"success but is_error", &Result{ResultSubtype: "success", IsError: true}, false},
		{"max turns", &Result{ResultSubtype: "error_max_turns"}, false},
		{"no result event", &Result{ExitCode: -1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.res.Answered(); got != tc.want {
				t.Errorf("Answered() = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestResultUsage_MapsProviderCacheColumns pins the seam between the stream
// parser and the cost tables: the provider's cache_creation_input_tokens is a
// cache WRITE and cache_read_input_tokens a cache READ, and every stage now
// records through this one projection. A swap here would put a session's write
// spend in the read column on every surface at once, which no per-stage test
// would catch — the numbers are deliberately distinct so it cannot pass.
func TestResultUsage_MapsProviderCacheColumns(t *testing.T) {
	input := `{"type":"result","subtype":"success","result":"All done.","total_cost_usd":0.25,` +
		`"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":41500,"cache_read_input_tokens":900}}`

	var buf strings.Builder
	result := &Result{}
	readStreamJSON(strings.NewReader(input), &buf, newTestLogFile(t), result)

	assert.Equal(t, cost.Usage{
		InputTokens:      100,
		OutputTokens:     50,
		CacheWriteTokens: 41500,
		CacheReadTokens:  900,
		EstimatedCostUSD: 0.25,
	}, result.Usage())
}

// TestResultUsage_RateLimitedIsZero keeps a refused request off the books: the
// provider never ran the session, so whatever counters rode along with the
// refusal are not the bead's spend. cost.Record writes nothing for a zero
// usage, which is how every stage inherits this rule from one place.
func TestResultUsage_RateLimitedIsZero(t *testing.T) {
	r := &Result{TokensIn: 100, CacheReadTokens: 5000, CostUSD: 0.1, RateLimited: true}
	u := r.Usage()
	assert.True(t, u.IsZero())
}
