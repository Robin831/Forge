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
	"time"

	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/provider"
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
	result *Result
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
	ResetAt           string `json:"reset_at,omitempty"`  // RFC3339 timestamp from Claude
	ResetsAt          int64  `json:"resetsAt,omitempty"`  // Unix epoch seconds (observed in real rate_limit_event payloads)
	RequestsRemaining int    `json:"requests_remaining,omitempty"`
	RequestsLimit     int    `json:"requests_limit,omitempty"`
	RequestsReset     string `json:"requests_reset,omitempty"` // RFC3339 or similar
	TokensRemaining   int    `json:"tokens_remaining,omitempty"`
	TokensLimit       int    `json:"tokens_limit,omitempty"`
	TokensReset       string `json:"tokens_reset,omitempty"`
}

// StreamUsage holds token counts from the result event.
type StreamUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Spawn starts a Claude Code process in the given worktree directory.
// This is a convenience wrapper around SpawnWithProvider using provider.Claude.
//
// logDir is where the session log file is written.
func Spawn(ctx context.Context, worktreePath, promptText, logDir string, extraFlags []string) (*Process, error) {
	return SpawnWithProvider(ctx, worktreePath, promptText, logDir, provider.Provider{Kind: provider.Claude}, extraFlags)
}

// SpawnWithProvider starts an AI coding agent process for the given provider.
// The provider determines which binary is executed and how arguments are built.
//
// logDir is where the session log file is written.
func SpawnWithProvider(ctx context.Context, worktreePath, promptText, logDir string, pv provider.Provider, extraFlags []string) (*Process, error) {
	args := pv.BuildArgs(extraFlags)

	cmd := exec.CommandContext(ctx, pv.Cmd(), args...)
	cmd.Dir = worktreePath

	// Deliver the prompt via stdin to avoid the Windows CreateProcess command-line
	// length limit (32 767 chars).  Claude uses -p -, Copilot uses -p -,
	// and Gemini reads stdin when no positional prompt argument is provided.
	cmd.Stdin = strings.NewReader(promptText)

	// Strip CLAUDECODE so claude doesn't refuse to run inside another session.
	env := os.Environ()
	filtered := env[:0:0]
	for _, e := range env {
		if !strings.HasPrefix(e, "CLAUDECODE=") {
			filtered = append(filtered, e)
		}
	}
	cmd.Env = filtered
	executil.HideWindow(cmd)

	// Set up log file
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating log directory: %w", err)
	}
	logPath := filepath.Join(logDir, fmt.Sprintf("smith-%d.log", time.Now().Unix()))
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
	p.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		return cmd.Process.Kill()
	}
	return nil
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
		if err := json.Unmarshal([]byte(line), &event); err == nil {
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
					result.TokensIn = event.Usage.InputTokens
					result.TokensOut = event.Usage.OutputTokens
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

// truncate shortens a string to maxLen, adding "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
