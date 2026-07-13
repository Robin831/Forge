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
	// RateLimited is true when the provider refused the request due to quota.
	RateLimited bool
	// IsError is true when the result event had is_error:true (session aborted,
	// e.g. hard rate-limit rejection). A success subtype with is_error:true
	// means Claude returned the rate-limit message as the "result" text, and we
	// must NOT treat this as a successful session.
	IsError bool
	// ResultSubtype is the stream-json result event subtype (e.g. "success",
	// "error_max_turns", "error_rate_limit_exceeded").
	ResultSubtype string
	// ProviderUsed records which provider produced this result.
	ProviderUsed provider.Kind
	// Quota contains the latest known quota information from the provider.
	Quota *provider.Quota
	// GeminiStats holds the raw stats block from a Gemini result event.
	// Nil for non-Gemini providers or when no stats were emitted.
	GeminiStats *StreamStats
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
	// Fields present when type == "result":
	Result       string       `json:"result,omitempty"`
	IsError      bool         `json:"is_error,omitempty"`
	TotalCostUSD float64      `json:"total_cost_usd,omitempty"`
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
	// stage-identifiable log file. An empty value defaults to "smith" so
	// existing callers keep their historical filenames.
	LogPrefix string
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
				readStreamJSON(stdoutPipe, &stdoutBuf, logFile, result)
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
			u.Calculate(cost.CopilotPricing())
			result.CostUSD = u.EstimatedCostUSD
		}

		// Estimate cost for OpenAI when total_cost_usd is absent.
		if pv.Kind == provider.OpenAI && result.CostUSD == 0 && (result.TokensIn > 0 || result.TokensOut > 0) {
			u := cost.Usage{
				InputTokens:  result.TokensIn,
				OutputTokens: result.TokensOut,
			}
			u.Calculate(cost.OpenAIPricing())
			result.CostUSD = u.EstimatedCostUSD
		}

		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				result.ExitCode = exitErr.ExitCode()
			} else {
				result.ExitCode = -1
			}
		}

		// Detect rate limit — OR with any flag already set by readStreamJSON
		// (e.g. from a rate_limit_event seen mid-stream) so we never lose it.
		result.RateLimited = result.RateLimited || provider.IsRateLimitError(
			result.ExitCode, result.ErrorOutput, result.ResultSubtype)
		// A genuine success (subtype "success", is_error false) means the AI
		// completed the task. Claude may exit 2 (its rate-limit code) even after
		// recovering internally — don't fall back in that case.
		// IMPORTANT: Do NOT clear RateLimited when is_error is true. Claude returns
		// subtype:"success" with is_error:true when the session was rate-limit
		// rejected before any work was done — that is NOT a successful session.
		if result.ResultSubtype == "success" && !result.IsError {
			result.RateLimited = false
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
			// Extract content for summary
			if event.Content != "" {
				lastContent = event.Content
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
				if event.Usage != nil {
					result.TokensIn = event.Usage.effectiveInputTokens()
					result.TokensOut = event.Usage.effectiveOutputTokens()
				}
				if event.Result != "" {
					result.FullOutput = event.Result
				}
				// is_error:true means the session was hard-aborted (e.g. rate-limit
				// rejection). Flag it if the result text confirms the cause.
				if event.IsError && provider.IsRateLimitError(0, event.Result, event.Subtype) {
					result.RateLimited = true
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
						u.Calculate(cost.GeminiPricing())
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
// duplicate-entry ambiguity), and unconditionally strips GIT_DIR /
// GIT_WORK_TREE / GIT_CEILING_DIRECTORIES so an inherited value cannot leak
// into the child:
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
		if strings.HasPrefix(e, "GIT_DIR=") ||
			strings.HasPrefix(e, "GIT_WORK_TREE=") ||
			strings.HasPrefix(e, "GIT_CEILING_DIRECTORIES=") {
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
