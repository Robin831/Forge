// Package smith manages Claude Code CLI process spawning for Smith workers.
//
// Each Smith is a Claude Code process running in a worktree directory,
// executing autonomously against a bead's description/prompt.
package smith

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/provider"
	"github.com/Robin831/Forge/internal/worktree"
)

// Result captures the outcome of a Smith session.
type Result struct {
	// ExitCode is the process exit code.
	ExitCode int
	// Duration is how long the process ran.
	Duration time.Duration
	// Output is the raw stdout collected.
	Output string
	// ErrorOutput is the raw stderr collected.
	ErrorOutput string
	// Summary is extracted from the stream-json output (last assistant message).
	Summary string
	// FullOutput is the complete text response from the AI (from the result event).
	FullOutput string
	// CostUSD is the total cost if extractable from output.
	CostUSD float64
	// TokensIn is the total input tokens if extractable.
	TokensIn int
	// TokensOut is the total output tokens if extractable.
	TokensOut int
	// CacheCreationTokens is how many input tokens the provider wrote into its
	// prompt cache for this session, and CacheReadTokens how many it served
	// from a cache another (or an earlier) session had already written. Both
	// are zero for providers that report no cache accounting.
	//
	// They are telemetry, not billing: cost still comes from the provider's own
	// total_cost_usd. What they measure is whether a set of sessions sharing a
	// prompt prefix is actually sharing it — a fan-out that writes the same
	// prefix N times reports N large CacheCreationTokens and no reads, which is
	// invisible in a per-session cost number and was the bulk of Assay's
	// cache-write spend before its pass prompts were ordered to share one (the
	// measurement lives on assay.buildPassPrompt).
	CacheCreationTokens int
	CacheReadTokens     int
	// RateLimited is true when the provider refused the request due to quota.
	RateLimited bool
	// AuthFailed is true when the provider rejected the credentials (invalid API
	// key, unauthorized, expired token). Distinct from RateLimited: an auth
	// failure must be escalated for human attention rather than retried/failed
	// over, since a bad credential fails identically on every attempt.
	AuthFailed bool
	// IsError is true when the result event had is_error:true (session aborted,
	// e.g. hard rate-limit rejection). A success subtype with is_error:true
	// means Claude returned the rate-limit message as the "result" text, and we
	// must NOT treat this as a successful session.
	IsError bool
	// ResultSubtype is the stream-json result event subtype (e.g. "success",
	// "error_max_turns", "error_rate_limit_exceeded").
	ResultSubtype string
	// NumTurns is the number of agent turns the session consumed, as reported
	// by the provider on its result event. Zero for providers that report none.
	// It is the only in-band measure of how close a session came to its
	// --max-turns budget, which is what makes a turn budget tunable against
	// real runs rather than guessed at.
	NumTurns int
	// ProviderUsed records which provider produced this result.
	ProviderUsed provider.Kind
	// Quota contains the latest known quota information from the provider.
	Quota *provider.Quota
	// GeminiStats holds the raw stats block from a Gemini result event.
	// Nil for non-Gemini providers or when no stats were emitted.
	GeminiStats *StreamStats
	// SessionID is the provider session identifier captured from the stream
	// (Claude emits it on the initial system init event and again on result).
	// Empty for providers that do not emit one (gemini/copilot/codex).
	SessionID string
	// Model is the model reported by the provider in its stream output
	// (Claude emits it on the initial system init event). Empty when the
	// provider does not report a model in-band.
	Model string
}

// Process represents a running or completed Smith (Claude Code) process.
type Process struct {
	// Cmd is the underlying exec.Cmd (nil after completion).
	Cmd *exec.Cmd
	// LogPath is the path to the session log file.
	LogPath string
	// PID is the process ID once started.
	PID int

	mu     sync.Mutex
	done   chan struct{}
	ioDone chan struct{} // closed when stdout/stderr reading completes (before cmd.Wait)
	result *Result
	onKill func() // optional hook called after Kill() attempt; used in tests for deterministic synchronization
}

// StreamEvent represents a single event from a provider's stream-json output.
type StreamEvent struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	Message json.RawMessage `json:"message,omitempty"`
	Content string          `json:"content,omitempty"`
	// Role is present on Gemini delta message events (role: "assistant" or "user").
	Role string `json:"role,omitempty"`
	// SessionID is emitted by Claude on the initial system init event and again
	// on the result event. Non-Claude providers do not emit it.
	SessionID string `json:"session_id,omitempty"`
	// Model is emitted by Claude on the initial system init event.
	Model string `json:"model,omitempty"`
	// Fields present when type == "result":
	Result       string       `json:"result,omitempty"`
	IsError      bool         `json:"is_error,omitempty"`
	TotalCostUSD float64      `json:"total_cost_usd,omitempty"`
	NumTurns     int          `json:"num_turns,omitempty"`
	Usage        *StreamUsage `json:"usage,omitempty"`
	// Stats from Gemini result event
	Stats *StreamStats `json:"stats,omitempty"`
	// rate_limit_event fields
	RateLimitInfo *RateLimitInfo `json:"rate_limit_info,omitempty"`
}

// StreamStats from Gemini result event.
type StreamStats struct {
	TotalTokens     int `json:"total_tokens,omitempty"`
	InputTokens     int `json:"input_tokens,omitempty"`
	OutputTokens    int `json:"output_tokens,omitempty"`
	Cached          int `json:"cached,omitempty"`
	Input           int `json:"input,omitempty"`
	DurationMs      int `json:"duration_ms,omitempty"`
	ToolCalls       int `json:"tool_calls,omitempty"`
	RequestsLimit   int `json:"requests_limit,omitempty"`
	RequestsUsed    int `json:"requests_used,omitempty"`
	RequestsResetMs int `json:"requests_reset_ms,omitempty"`
	TokensLimit     int `json:"tokens_limit,omitempty"`
	TokensUsed      int `json:"tokens_used,omitempty"`
	TokensResetMs   int `json:"tokens_reset_ms,omitempty"`
}

// RateLimitInfo is the payload of a Claude rate_limit_event.
type RateLimitInfo struct {
	Status            string `json:"status"`
	ResetAt           string `json:"reset_at,omitempty"` // RFC3339 timestamp from Claude
	ResetsAt          int64  `json:"resetsAt,omitempty"` // Unix epoch seconds (observed in real rate_limit_event payloads)
	RequestsRemaining int    `json:"requests_remaining,omitempty"`
	RequestsLimit     int    `json:"requests_limit,omitempty"`
	RequestsReset     string `json:"requests_reset,omitempty"` // RFC3339 or similar
	TokensRemaining   int    `json:"tokens_remaining,omitempty"`
	TokensLimit       int    `json:"tokens_limit,omitempty"`
	TokensReset       string `json:"tokens_reset,omitempty"`
}

// StreamUsage holds token counts from the result event.
// It supports both Anthropic-style fields (input_tokens/output_tokens) and
// OpenAI API-style fields (prompt_tokens/completion_tokens) so that Codex CLI
// output is handled correctly regardless of which naming convention it uses.
type StreamUsage struct {
	// Anthropic-style (Claude, Gemini-compat, newer OpenAI SDKs)
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// OpenAI API-style (Codex CLI may emit these)
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// Anthropic prompt-cache accounting. CacheCreationInputTokens is what this
	// request paid to write into the cache; CacheReadInputTokens is what it
	// served from a prefix already there. Providers that do not do prefix
	// caching omit both and they stay zero.
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// effectiveInputTokens returns the input token count, preferring
// the Anthropic-style field but falling back to the OpenAI-style field.
func (u *StreamUsage) effectiveInputTokens() int {
	if u.InputTokens > 0 {
		return u.InputTokens
	}
	return u.PromptTokens
}

// effectiveOutputTokens returns the output token count, preferring
// the Anthropic-style field but falling back to the OpenAI-style field.
func (u *StreamUsage) effectiveOutputTokens() int {
	if u.OutputTokens > 0 {
		return u.OutputTokens
	}
	return u.CompletionTokens
}

// resumeUnavailableMarkers are lowercased substrings that indicate a
// `claude --resume <id>` attempt could not attach to its session — the
// transcript for that session id is missing, or the CLI rejected the resume.
// They are matched case-insensitively against the combined result text so a
// caller can fall back to a fresh session instead of failing the worker.
var resumeUnavailableMarkers = []string{
	"no conversation found",
	"no conversation with session",
	"conversation not found",
	"session not found",
	"no session found",
	"could not find session",
	"could not find conversation",
	"unable to resume",
	"failed to resume",
	"transcript not found",
	"no transcript",
	"invalid session id",
	"unknown session",
}

// ResumeUnavailable reports whether a resumed Smith result indicates the prior
// session could not be continued — the transcript is missing or the provider
// rejected the `--resume` — as opposed to a genuine session that ran (and, say,
// ran out of turns). Callers use it to decide whether to fall back to a fresh
// session seeded with full context instead of failing the worker.
//
// A nil result (the resume produced nothing at all) is treated as unavailable.
// A genuine success (subtype "success", not is_error) is never unavailable.
// Rate-limit and auth failures have dedicated handling upstream and are NOT
// reported here so a quota block is not mistaken for a missing transcript.
func ResumeUnavailable(r *Result) bool {
	if r == nil {
		return true
	}
	if r.ResultSubtype == "success" && !r.IsError {
		return false
	}
	if r.RateLimited || r.AuthFailed {
		return false
	}
	combined := strings.ToLower(strings.Join([]string{
		r.FullOutput, r.Summary, r.Output, r.ErrorOutput,
	}, "\n"))
	for _, m := range resumeUnavailableMarkers {
		if strings.Contains(combined, m) {
			return true
		}
	}
	return false
}

// SessionModel returns the model to record for a spawn: the model reported
// in-band by the provider stream when present (Claude reports it on the system
// init event), otherwise the provider's configured model.
func SessionModel(r *Result, pv provider.Provider) string {
	if r != nil && r.Model != "" {
		return r.Model
	}
	return pv.Model
}

// Spawn starts a Claude Code process in the given worktree directory.
// This is a convenience wrapper around SpawnWithProvider using provider.Claude.
//
// logDir is where the session log file is written.
func Spawn(ctx context.Context, worktreePath, promptText, logDir string, extraFlags []string) (*Process, error) {
	return SpawnWithProvider(ctx, worktreePath, promptText, logDir, provider.Provider{Kind: provider.Claude}, extraFlags)
}

// SpawnOptions configures optional behaviour for SpawnWithOptions.
type SpawnOptions struct {
	// LogPrefix is the filename prefix for the session log file written into
	// logDir (e.g. "warden" produces warden-<ts>-<seq>.log). This lets each
	// pipeline stage that reuses the Smith spawn machinery emit a
	// stage-identifiable log file. An empty value defaults to "smith",
	// producing smith-<ts>-<seq>.log.
	LogPrefix string

	// ResumeSessionID, when non-empty, resumes a prior provider session instead
	// of starting a fresh one. The provider's ResumeFlag is appended to the
	// argument list (Claude: --resume <id>); providers that do not support
	// resumption ignore it. Used by steer mode to continue an interrupted
	// Claude session with a new steering message delivered on stdin.
	ResumeSessionID string

	// OnStreamEvent, when non-nil, is invoked for each parsed stream-json event
	// as it arrives from the provider's stdout, before the process exits. It
	// lets incremental consumers (e.g. the Beads-Forge chat SSE stream) forward
	// text deltas and tool events live instead of waiting for the final Result.
	// It is called synchronously from the single stdout reader goroutine in
	// provider arrival order, so callbacks must return promptly and must not
	// block on the process. Only fires for StreamJSON-format providers; plain
	// text providers never emit structured events. Nil disables streaming.
	OnStreamEvent func(StreamEvent)
}

// logFileName builds the session log filename for the given stage prefix and
// timestamp (milliseconds since epoch). An empty prefix defaults to "smith" so
// callers that do not supply a stage keep the historical smith-*.log naming.
// The prefix is sanitised to a safe basename to prevent path traversal.
var logSeq atomic.Int64

func logFileName(prefix string, ts int64) string {
	if prefix == "" {
		prefix = "smith"
	}
	prefix = strings.ReplaceAll(prefix, "\\", "/")
	prefix = filepath.Base(prefix)
	if prefix == "." || prefix == ".." || prefix == "/" || prefix == string(filepath.Separator) {
		prefix = "smith"
	}
	seq := logSeq.Add(1)
	return fmt.Sprintf("%s-%d-%d.log", prefix, ts, seq)
}

// SpawnWithProvider starts an AI coding agent process for the given provider.
// The provider determines which binary is executed and how arguments are built.
//
// logDir is where the session log file is written. The log file is named
// smith-<ts>-<seq>.log; callers that need a stage-specific prefix should use
// SpawnWithOptions instead.
func SpawnWithProvider(ctx context.Context, worktreePath, promptText, logDir string, pv provider.Provider, extraFlags []string) (*Process, error) {
	return SpawnWithOptions(ctx, worktreePath, promptText, logDir, pv, extraFlags, SpawnOptions{})
}

// SpawnWithOptions starts an AI coding agent process for the given provider,
// with additional behaviour controlled by opts. It is the underlying
// implementation shared by SpawnWithProvider (and the various pipeline stages
// that reuse the Smith spawn machinery), differing only in the log filename
// prefix chosen via SpawnOptions.LogPrefix.
//
// logDir is where the session log file is written.
func SpawnWithOptions(ctx context.Context, worktreePath, promptText, logDir string, pv provider.Provider, extraFlags []string, opts SpawnOptions) (*Process, error) {
	if err := worktree.ValidateWorktreeDir(worktreePath); err != nil {
		return nil, fmt.Errorf("smith pre-flight: working directory is not a valid worktree — refusing to run to prevent editing the main checkout: %w", err)
	}

	args := pv.BuildArgs(extraFlags)
	// Steer mode: resume an existing provider session when requested. Only
	// providers that report a session_id support this (Claude); others return
	// nil from ResumeFlag and start a fresh session.
	if opts.ResumeSessionID != "" {
		args = append(args, pv.ResumeFlag(opts.ResumeSessionID)...)
	}

	cmd := exec.CommandContext(ctx, pv.Cmd(), args...)
	cmd.Dir = worktreePath

	// Deliver the prompt via stdin. Claude uses -p -, Gemini reads stdin
	// when no positional prompt is given, and Copilot reads stdin when -p
	// is omitted (detects piped input and runs non-interactively).
	cmd.Stdin = strings.NewReader(promptText)

	cmd.Env = buildChildEnv(os.Environ(), pv.Env, worktree.GitEnv(worktreePath))
	executil.HideWindow(cmd)
	executil.SetProcessGroup(cmd)

	// Set up log file
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating log directory: %w", err)
	}
	logPath := filepath.Join(logDir, logFileName(opts.LogPrefix, time.Now().UnixMilli()))
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("creating log file: %w", err)
	}

	// Capture stdout (stream-json) and stderr
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		logFile.Close()
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		logFile.Close()
		return nil, fmt.Errorf("creating stderr pipe: %w", err)
	}

	// Start the process
	startTime := time.Now()
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("starting %s process: %w", pv.Cmd(), err)
	}

	// Contain the worker so it is reaped if the daemon exits without cleanup.
	// On Windows this assigns the process to a kill-on-close Job Object; on Unix
	// it is a no-op (process-group signalling handles this). Non-fatal: log and
	// continue so a containment failure never blocks the worker.
	if err := executil.ContainProcess(cmd); err != nil {
		slog.Warn("failed to contain worker process; orphan prevention degraded",
			"pid", cmd.Process.Pid, "error", err)
	}

	p := &Process{
		Cmd:     cmd,
		LogPath: logPath,
		PID:     cmd.Process.Pid,
		done:    make(chan struct{}),
		ioDone:  make(chan struct{}),
	}

	// Collect output in background
	pvFormat := pv.Format()
	go func() {
		defer close(p.done)
		defer logFile.Close()

		result := &Result{
			ProviderUsed: pv.Kind,
		}

		// Read stdout and stderr concurrently
		var wg sync.WaitGroup
		var stdoutBuf, stderrBuf strings.Builder

		wg.Add(2)

		// Read stdout — branch on provider output format.
		go func() {
			defer wg.Done()
			if pvFormat == provider.StreamJSON {
				readStreamJSONEvents(stdoutPipe, &stdoutBuf, logFile, result, opts.OnStreamEvent)
			} else {
				// PlainText (Copilot CLI --silent): raw response in stdout.
				readAll(stdoutPipe, &stdoutBuf, logFile)
			}
		}()

		// Read stderr
		go func() {
			defer wg.Done()
			readAll(stderrPipe, &stderrBuf, logFile)
		}()

		wg.Wait()

		// Signal that stream I/O is complete. WaitWithExitTimeout waits on this
		// channel before starting its exit deadline, so it can safely determine
		// success from the parsed stream result regardless of process-exit timing.
		close(p.ioDone)

		// Wait for process to exit
		err := cmd.Wait()
		result.Duration = time.Since(startTime)
		result.Output = stdoutBuf.String()
		result.ErrorOutput = stderrBuf.String()

		// For plain-text providers the full response IS the raw stdout.
		if pvFormat == provider.PlainText && result.FullOutput == "" {
			result.FullOutput = result.Output
		}

		// Estimate cost for Copilot when the JSONL didn't include total_cost_usd
		// (Copilot is subscription-based so the field is often zero).
		if pv.Kind == provider.Copilot && result.CostUSD == 0 && (result.TokensIn > 0 || result.TokensOut > 0) {
			u := cost.Usage{
				InputTokens:  result.TokensIn,
				OutputTokens: result.TokensOut,
			}
			u.Calculate(cost.FallbackPricing(pv.Kind, pv.Model))
			result.CostUSD = u.EstimatedCostUSD
		}

		// Estimate cost for OpenAI when total_cost_usd is absent.
		if pv.Kind == provider.OpenAI && result.CostUSD == 0 && (result.TokensIn > 0 || result.TokensOut > 0) {
			u := cost.Usage{
				InputTokens:  result.TokensIn,
				OutputTokens: result.TokensOut,
			}
			u.Calculate(cost.FallbackPricing(pv.Kind, pv.Model))
			result.CostUSD = u.EstimatedCostUSD
		}

		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				result.ExitCode = exitErr.ExitCode()
			} else {
				result.ExitCode = -1
			}
		}

		// Classify the failure — OR rate-limit with any flag already set by
		// readStreamJSON (e.g. from a rate_limit_event seen mid-stream) so we never
		// lose it. Auth failures are tracked separately so the pipeline can escalate
		// them instead of retrying/failing over forever (Forge-d5ns).
		switch provider.ClassifyProviderError(result.ExitCode, result.ErrorOutput, result.ResultSubtype) {
		case provider.FailureAuth:
			result.AuthFailed = true
		case provider.FailureRateLimit:
			result.RateLimited = true
		}
		// A genuine success (subtype "success", is_error false) means the AI
		// completed the task. Claude may exit 2 (its rate-limit code) even after
		// recovering internally — don't fall back in that case.
		// IMPORTANT: Do NOT clear RateLimited when is_error is true. Claude returns
		// subtype:"success" with is_error:true when the session was rate-limit
		// rejected before any work was done — that is NOT a successful session.
		if result.ResultSubtype == "success" && !result.IsError {
			result.RateLimited = false
			result.AuthFailed = false
		}

		p.mu.Lock()
		p.result = result
		p.Cmd = nil
		p.mu.Unlock()
	}()

	return p, nil
}

// Wait blocks until the process completes and returns the result.
func (p *Process) Wait() *Result {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.result
}

// WaitWithExitTimeout waits for I/O reading to complete, then gives the
// process exitTimeout to exit gracefully. If the process does not exit within
// the deadline it is killed so the caller can advance (e.g. to resolve review
// threads and emit a completion event). When the stream result indicates
// success (ResultSubtype "success", IsError false) AND the process had to be
// killed, the exit code is normalized to 0 so downstream checks
// (ExitCode != 0) don't misclassify a successful push as a failure.
func (p *Process) WaitWithExitTimeout(exitTimeout time.Duration) *Result {
	// Wait for stdout/stderr reading to complete. After this point the stream
	// has been fully parsed and ResultSubtype/RateLimited/IsError are stable.
	<-p.ioDone

	timer := time.NewTimer(exitTimeout)
	defer timer.Stop()

	killed := false
	select {
	case <-p.done:
		// Process exited normally within the deadline.
	case <-timer.C:
		// Process is slow to exit. Kill it so the worker can proceed to
		// thread resolution and completion without waiting indefinitely.
		killed = true
		_ = p.Kill()
		<-p.done // wait for goroutine to finish and assign p.result
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.result
	// Only normalize exit code when we actually killed the process; a normal
	// exit preserves whatever exit code the process reported.
	if killed && r != nil && r.ResultSubtype == "success" && !r.IsError {
		r.ExitCode = 0
	}
	return r
}

// Done returns a channel that is closed when the process completes.
func (p *Process) Done() <-chan struct{} {
	return p.done
}

// IsRunning returns true if the process is still running.
func (p *Process) IsRunning() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Interrupt gracefully stops a running Smith spawn so its session can be
// resumed (steer mode A). It mirrors the daemon's killWorkerProcess signaling
// pattern — SIGINT to the process group, wait a grace period for a clean exit,
// then SIGKILL if still alive — but deliberately does NOT touch any state.db
// worker row. The caller reaps the Result via Wait() and reuses the captured
// session_id to resume the session with a new message.
//
// It is safe to call on an already-finished process (and on the test stub
// returned by NewRunningProcessForTest, which has no real PID): in that case it
// falls back to Kill so any onKill hook fires and the done channel closes.
func (p *Process) Interrupt(grace time.Duration) {
	p.mu.Lock()
	cmd := p.Cmd
	pid := p.PID
	p.mu.Unlock()

	// No live OS process to signal (already exited, or a test stub). Fall back
	// to Kill so the done channel is closed / onKill hook fires.
	if pid <= 0 || cmd == nil || cmd.Process == nil {
		_ = p.Kill()
		return
	}

	// Phase 1: request a graceful exit via SIGINT to the process group.
	interruptProcessGroup(pid)

	// Phase 2: wait up to grace for the process to exit on its own; the stream
	// reader goroutine closes done once the process is reaped. If it overstays
	// the grace period, force-kill the whole group.
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		killProcessGroup(pid)
	}
}

// Kill forcefully terminates the process.
func (p *Process) Kill() error {
	p.mu.Lock()
	cmd := p.Cmd
	hook := p.onKill
	p.mu.Unlock()

	var err error
	if cmd != nil && cmd.Process != nil {
		err = cmd.Process.Kill()
	}
	if hook != nil {
		hook()
	}
	return err
}

// assistantMessage is used to extract text from Claude's assistant events.
type assistantMessage struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// readStreamJSON reads Claude's stream-json output line by line,
// writing to both the buffer and log file, extracting result metadata.
func readStreamJSON(r io.Reader, buf *strings.Builder, logFile *os.File, result *Result) {
	readStreamJSONEvents(r, buf, logFile, result, nil)
}

// readStreamJSONEvents is readStreamJSON with an optional per-event callback.
// When onEvent is non-nil it is invoked for every successfully-parsed stream
// event, in arrival order, before the aggregate Result is finalised — this is
// what lets streaming consumers forward text deltas and tool events live. A
// nil callback reduces to the historical batch-only behaviour.
func readStreamJSONEvents(r io.Reader, buf *strings.Builder, logFile *os.File, result *Result, onEvent func(StreamEvent)) {
	scanner := bufio.NewScanner(r)
	// Claude can produce long lines
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	var lastContent string
	var assistantText strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		buf.WriteString(line)
		buf.WriteString("\n")

		// Write to log file
		fmt.Fprintln(logFile, line)

		// Try to parse as stream event
		var event StreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			// Non-JSON line — may be a plain-text error from providers that output
			// quota/rate-limit messages as unstructured text (e.g., Copilot CLI when
			// premium request quota is exhausted). Detect early so the caller can
			// fall back to another provider rather than treating it as a generic failure.
			if provider.IsRateLimitError(0, line, "") {
				result.RateLimited = true
			}
		} else {
			// Forward the parsed event to a streaming consumer (if any) before
			// aggregating it into the Result. Deliver it in arrival order so the
			// consumer sees text deltas and tool events interleaved exactly as
			// the provider emitted them.
			if onEvent != nil {
				onEvent(event)
			}

			// Extract content for summary
			if event.Content != "" {
				lastContent = event.Content
			}

			// Capture the session_id and model from the first event that
			// carries them. Claude emits both on the initial system init event
			// and repeats session_id on the result event; recording the first
			// non-empty value is sufficient. Non-Claude providers never emit
			// these fields, so the values stay empty without any branching.
			if result.SessionID == "" && event.SessionID != "" {
				result.SessionID = event.SessionID
			}
			if result.Model == "" && event.Model != "" {
				result.Model = event.Model
			}

			// Accumulate assistant message text (fallback when result event
			// has no "result" field, e.g. subtype "error_max_turns").
			if event.Type == "assistant" && len(event.Message) > 0 {
				var msg assistantMessage
				if err := json.Unmarshal(event.Message, &msg); err == nil {
					for _, block := range msg.Content {
						if block.Type == "text" && block.Text != "" {
							assistantText.WriteString(block.Text)
						}
					}
				}
			}

			// Accumulate Gemini-style delta messages: {type:"message",role:"assistant",content:"...",delta:true}
			// Gemini does not include the full response in the result event's
			// "result" field, so we must rebuild FullOutput from streaming deltas.
			if event.Type == "message" && event.Role == "assistant" && event.Content != "" {
				assistantText.WriteString(event.Content)
			}

			// Capture the final result event (contains full text and cost).
			// When subtype is "success" the "result" field holds the complete
			// assistant response.  When subtype is "error_max_turns" the field
			// is absent — we fall back to accumulated assistant text below.
			// When is_error is true the session was aborted (e.g. hard rate-limit
			// rejection) even though subtype may still read "success".
			if event.Type == "result" {
				result.ResultSubtype = event.Subtype
				result.IsError = event.IsError
				result.CostUSD = event.TotalCostUSD
				result.NumTurns = event.NumTurns
				if event.Usage != nil {
					result.TokensIn = event.Usage.effectiveInputTokens()
					result.TokensOut = event.Usage.effectiveOutputTokens()
					result.CacheCreationTokens = event.Usage.CacheCreationInputTokens
					result.CacheReadTokens = event.Usage.CacheReadInputTokens
				}
				if event.Result != "" {
					result.FullOutput = event.Result
				}
				// is_error:true means the session was hard-aborted (e.g. rate-limit
				// rejection or auth failure). Classify the result text to flag the
				// cause — auth failures are reported in the result event text, not
				// stderr, when the provider streams JSON (Forge-d5ns).
				if event.IsError {
					switch provider.ClassifyProviderError(0, event.Result, event.Subtype) {
					case provider.FailureAuth:
						result.AuthFailed = true
					case provider.FailureRateLimit:
						result.RateLimited = true
					}
				}

				// Gemini emits stats instead of usage/total_cost_usd.
				if event.Stats != nil {
					result.GeminiStats = event.Stats
					// Populate token counts from stats when Usage is absent.
					if result.TokensIn == 0 && result.TokensOut == 0 {
						result.TokensIn = event.Stats.InputTokens
						result.TokensOut = event.Stats.OutputTokens
					}

					// Estimate cost for Gemini if it was not provided (it is usually 0).
					if result.CostUSD == 0 {
						u := cost.Usage{
							InputTokens:  event.Stats.InputTokens,
							OutputTokens: event.Stats.OutputTokens,
						}
						u.Calculate(cost.FallbackPricing(provider.Gemini, result.Model))
						result.CostUSD = u.EstimatedCostUSD
					}

					// Write a human-readable stats summary to the smith log.
					fmt.Fprintf(logFile,
						"[gemini stats] tokens_in=%d tokens_out=%d total=%d cached=%d input=%d tool_calls=%d duration_ms=%d\n",
						event.Stats.InputTokens, event.Stats.OutputTokens, event.Stats.TotalTokens,
						event.Stats.Cached, event.Stats.Input, event.Stats.ToolCalls, event.Stats.DurationMs)
				}

				// Extract quota from Gemini stats if present
				if event.Stats != nil && (event.Stats.RequestsLimit > 0 || event.Stats.TokensLimit > 0) {
					if result.Quota == nil {
						result.Quota = &provider.Quota{}
					}
					if event.Stats.RequestsLimit > 0 {
						result.Quota.RequestsLimit = event.Stats.RequestsLimit
						result.Quota.RequestsRemaining = max(0, event.Stats.RequestsLimit-event.Stats.RequestsUsed)
						if event.Stats.RequestsResetMs > 0 {
							reset := time.Now().Add(time.Duration(event.Stats.RequestsResetMs) * time.Millisecond)
							result.Quota.RequestsReset = &reset
						}
					}
					if event.Stats.TokensLimit > 0 {
						result.Quota.TokensLimit = event.Stats.TokensLimit
						result.Quota.TokensRemaining = max(0, event.Stats.TokensLimit-event.Stats.TokensUsed)
						if event.Stats.TokensResetMs > 0 {
							reset := time.Now().Add(time.Duration(event.Stats.TokensResetMs) * time.Millisecond)
							result.Quota.TokensReset = &reset
						}
					}
				}
			}

			// Detect a hard rate-limit event emitted before the result.
			// Claude emits rate_limit_event for multiple informational purposes:
			//   status:"warning"  — approaching the limit, session continues
			//   status:"allowed"  — within limits (org may have overage disabled)
			//   status:"blocked"  — hard block, quota exhausted mid-session
			//   status:"rejected" — session rejected before it started (quota full)
			// Only set RateLimited for statuses that mean real blocking.
			// "warning" and "allowed" are informational only.
			if event.Type == "rate_limit_event" {
				status := ""
				if event.RateLimitInfo != nil {
					status = strings.ToLower(strings.TrimSpace(event.RateLimitInfo.Status))
				}
				// "blocked" and "rejected" are explicit hard limits.
				// Empty/unknown status is treated conservatively as blocking.
				// "warning" and "allowed" are informational only.
				if status == "blocked" || status == "rejected" || status == "" {
					result.RateLimited = true
				}

				if event.RateLimitInfo != nil {
					if result.Quota == nil {
						result.Quota = &provider.Quota{}
					}
					if event.RateLimitInfo.RequestsLimit > 0 {
						result.Quota.RequestsLimit = event.RateLimitInfo.RequestsLimit
						result.Quota.RequestsRemaining = event.RateLimitInfo.RequestsRemaining
						if event.RateLimitInfo.RequestsReset != "" {
							if t, err := time.Parse(time.RFC3339, event.RateLimitInfo.RequestsReset); err == nil {
								result.Quota.RequestsReset = &t
							}
						}
					}
					// Claude's reset_at field (RFC3339) or resetsAt (Unix epoch seconds)
					if event.RateLimitInfo.ResetAt != "" {
						if t, err := time.Parse(time.RFC3339, event.RateLimitInfo.ResetAt); err == nil {
							result.Quota.RequestsReset = &t
						}
					} else if event.RateLimitInfo.ResetsAt > 0 {
						t := time.Unix(event.RateLimitInfo.ResetsAt, 0)
						result.Quota.RequestsReset = &t
					}
					if event.RateLimitInfo.TokensLimit > 0 {
						result.Quota.TokensLimit = event.RateLimitInfo.TokensLimit
						result.Quota.TokensRemaining = event.RateLimitInfo.TokensRemaining
						if event.RateLimitInfo.TokensReset != "" {
							if t, err := time.Parse(time.RFC3339, event.RateLimitInfo.TokensReset); err == nil {
								result.Quota.TokensReset = &t
							}
						}
					}
				}
			}
		}
	}

	// If the result event had no text (e.g. error_max_turns), use the
	// accumulated assistant message text as FullOutput so callers like the
	// warden can still attempt to parse a verdict from partial output.
	if result.FullOutput == "" {
		result.FullOutput = assistantText.String()
	}

	// Use last content as summary
	if lastContent != "" {
		result.Summary = truncate(lastContent, 500)
	}
}

// readAll reads all output from a reader into a buffer and log file.
func readAll(r io.Reader, buf *strings.Builder, logFile *os.File) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		buf.WriteString(line)
		buf.WriteString("\n")
		fmt.Fprintln(logFile, "[stderr] ", line)
	}
}

// buildChildEnv assembles the environment for an AI agent subprocess.
//
// It strips CLAUDECODE (so claude does not refuse to run inside another
// session), strips any keys that the provider will set (avoiding cross-platform
// duplicate-entry ambiguity), and unconditionally strips the git repo-location
// env vars (executil.IsGitRepoEnvVar — GIT_DIR, GIT_WORK_TREE and the rest of
// the family) so an inherited value cannot leak into the child:
//   - Worktree spawns: gitEnv re-injects values pinning git to the worktree.
//     A stray "cd .." in a tool_use bash command cannot escape into the parent
//     anvil and commit to its main branch.
//   - Non-worktree spawns (schematic/wicket tempdirs): gitEnv is nil, so no
//     git env is set. An inherited GIT_DIR pointing to the parent process's
//     repo must not survive — otherwise an escaped Smith could still target
//     the anvil's checkout.
//
// providerEnv overrides go in last so they take precedence over inherited
// values, and worktree gitEnv goes after provider env so that providers cannot
// shadow our git confinement.
func buildChildEnv(parentEnv []string, providerEnv map[string]string, gitEnv []string) []string {
	out := make([]string, 0, len(parentEnv)+len(providerEnv)+len(gitEnv))
	for _, e := range parentEnv {
		if strings.HasPrefix(e, "CLAUDECODE=") {
			continue
		}
		key, _, _ := strings.Cut(e, "=")
		if executil.IsGitRepoEnvVar(key) {
			continue
		}
		skip := false
		for k := range providerEnv {
			if strings.HasPrefix(e, k+"=") {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, e)
		}
	}
	for k, v := range providerEnv {
		out = append(out, k+"="+v)
	}
	out = append(out, gitEnv...)
	return out
}

// truncate shortens a string to maxLen, adding "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
