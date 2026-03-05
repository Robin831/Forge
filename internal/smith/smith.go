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

	"github.com/Robin831/Forge/internal/executil"
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
	// FullOutput is the complete text response from Claude (from the result event).
	FullOutput string
	// CostUSD is the total cost if extractable from output.
	CostUSD float64
	// TokensIn is the total input tokens if extractable.
	TokensIn int
	// TokensOut is the total output tokens if extractable.
	TokensOut int
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

// StreamEvent represents a single event from Claude's stream-json output.
type StreamEvent struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message,omitempty"`
	Content string          `json:"content,omitempty"`
	// Fields present when type == "result":
	Result       string       `json:"result,omitempty"`
	TotalCostUSD float64      `json:"total_cost_usd,omitempty"`
	Usage        *StreamUsage `json:"usage,omitempty"`
}

// StreamUsage holds token counts from the result event.
type StreamUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Spawn starts a new Claude Code process in the given worktree directory.
// The prompt is sent via -p flag. The process runs with --dangerously-skip-permissions
// for autonomous operation.
//
// logDir is where the session log file is written.
func Spawn(ctx context.Context, worktreePath, prompt, logDir string, extraFlags []string) (*Process, error) {
	// Build command arguments
	args := []string{
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--output-format", "stream-json",
		"--verbose",
	}
	args = append(args, extraFlags...)

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = worktreePath

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
		return nil, fmt.Errorf("starting claude process: %w", err)
	}

	p := &Process{
		Cmd:     cmd,
		LogPath: logPath,
		PID:     cmd.Process.Pid,
		done:    make(chan struct{}),
	}

	// Collect output in background
	go func() {
		defer close(p.done)
		defer logFile.Close()

		result := &Result{}

		// Read stdout and stderr concurrently
		var wg sync.WaitGroup
		var stdoutBuf, stderrBuf strings.Builder

		wg.Add(2)

		// Read stream-json stdout
		go func() {
			defer wg.Done()
			readStreamJSON(stdoutPipe, &stdoutBuf, logFile, result)
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

		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				result.ExitCode = exitErr.ExitCode()
			} else {
				result.ExitCode = -1
			}
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

// readStreamJSON reads Claude's stream-json output line by line,
// writing to both the buffer and log file, extracting result metadata.
func readStreamJSON(r io.Reader, buf *strings.Builder, logFile *os.File, result *Result) {
	scanner := bufio.NewScanner(r)
	// Claude can produce long lines
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	var lastContent string

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

			// Capture the final result event (contains full text and cost)
			if event.Type == "result" && event.Result != "" {
				result.FullOutput = event.Result
				result.CostUSD = event.TotalCostUSD
				if event.Usage != nil {
					result.TokensIn = event.Usage.InputTokens
					result.TokensOut = event.Usage.OutputTokens
				}
			}
		}
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
