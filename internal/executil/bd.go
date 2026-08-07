package executil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// DefaultBdTimeout is the default timeout for bd subprocess invocations.
// bd operations on anvils with remote Dolt (e.g. via kubectl port-forward)
// and GitHub auto-sync can routinely take 20-30 seconds per write, so the
// timeout must be generous enough to accommodate that latency.
const DefaultBdTimeout = 5 * time.Minute

// bdTimeoutNanos holds the configured bd timeout in nanoseconds. Zero means
// "use DefaultBdTimeout". It is package-level (rather than a field on some
// runner) because bd is invoked from a dozen packages that share no type.
var bdTimeoutNanos atomic.Int64

// SetBdTimeout configures the deadline applied to bd invocations
// (settings.bd_timeout). A value <= 0 restores DefaultBdTimeout. Safe to call
// at any time, including from the config hot-reload path.
func SetBdTimeout(d time.Duration) {
	if d <= 0 {
		if d < 0 {
			slog.Warn("executil: ignoring negative bd timeout",
				"value", d, "using", DefaultBdTimeout)
		}
		bdTimeoutNanos.Store(0)
		return
	}
	bdTimeoutNanos.Store(int64(d))
}

// BdTimeout returns the effective bd timeout.
func BdTimeout() time.Duration {
	if n := bdTimeoutNanos.Load(); n > 0 {
		return time.Duration(n)
	}
	return DefaultBdTimeout
}

// BdTimeoutError reports a bd command that was killed because it exceeded its
// deadline. It exists so callers see which bd command ran out of time and how
// long it got, instead of the bare "signal: killed" that exec.CommandContext
// produces. Unwrap returns context.DeadlineExceeded so callers can classify the
// failure with errors.Is.
type BdTimeoutError struct {
	// Args is the bd argument vector (without the leading "bd").
	Args []string
	// Elapsed is how long the command actually ran before being killed.
	Elapsed time.Duration
	// Limit is the deadline that fired — the configured bd timeout, or the
	// caller's own deadline when that one was closer.
	Limit time.Duration
}

func (e *BdTimeoutError) Error() string {
	return fmt.Sprintf("bd %s timed out after %s (limit %s)",
		strings.Join(e.Args, " "),
		e.Elapsed.Round(time.Millisecond),
		e.Limit.Round(time.Millisecond))
}

func (e *BdTimeoutError) Unwrap() error { return context.DeadlineExceeded }

// BdCmd is a bd invocation bounded by a deadline. It embeds the *exec.Cmd so
// callers can still set Dir, Env, Stdin and the output writers; Run, Output and
// CombinedOutput are overridden to translate a deadline kill into a
// *BdTimeoutError.
//
// Unlike the git helpers in internal/worktree, no WaitDelay is set: bd can
// leave short-lived background helpers (auto-sync/export) holding the inherited
// pipes, and a WaitDelay would turn those otherwise successful runs into
// exec.ErrWaitDelay failures.
type BdCmd struct {
	*exec.Cmd

	ctx   context.Context
	args  []string
	limit time.Duration
}

// BdCommand builds a bd command bounded by the configured bd timeout
// (settings.bd_timeout, default DefaultBdTimeout). The caller owns the returned
// CancelFunc and must call it — typically `defer cancel()` — once the command
// has finished.
func BdCommand(parent context.Context, args ...string) (*BdCmd, context.CancelFunc) {
	return BdCommandTimeout(parent, BdTimeout(), args...)
}

// BdCommandTimeout is BdCommand with an explicit timeout, for the few callers
// that deliberately hold bd to a tighter budget than the global setting. A
// timeout <= 0 falls back to the configured bd timeout.
func BdCommandTimeout(parent context.Context, timeout time.Duration, args ...string) (*BdCmd, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		timeout = BdTimeout()
	}
	// The caller's own deadline wins when it is closer; report that as the
	// limit so the message matches the deadline that actually fired.
	limit := timeout
	if dl, ok := parent.Deadline(); ok {
		if remaining := time.Until(dl); remaining < limit {
			limit = remaining
		}
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	cmd := HideWindow(exec.CommandContext(ctx, "bd", args...))
	return &BdCmd{
		Cmd:   cmd,
		ctx:   ctx,
		args:  append([]string(nil), args...),
		limit: limit,
	}, cancel
}

// Run runs the command, reporting a deadline kill as a *BdTimeoutError.
func (c *BdCmd) Run() error {
	start := time.Now()
	return c.classify(c.Cmd.Run(), start)
}

// Output runs the command and returns its stdout, reporting a deadline kill as
// a *BdTimeoutError.
func (c *BdCmd) Output() ([]byte, error) {
	start := time.Now()
	out, err := c.Cmd.Output()
	return out, c.classify(err, start)
}

// CombinedOutput runs the command and returns its combined stdout+stderr,
// reporting a deadline kill as a *BdTimeoutError.
func (c *BdCmd) CombinedOutput() ([]byte, error) {
	start := time.Now()
	out, err := c.Cmd.CombinedOutput()
	return out, c.classify(err, start)
}

func (c *BdCmd) classify(err error, start time.Time) error {
	if err == nil {
		return nil
	}
	if errors.Is(c.ctx.Err(), context.DeadlineExceeded) {
		return &BdTimeoutError{
			Args:    c.args,
			Elapsed: time.Since(start),
			Limit:   c.limit,
		}
	}
	return err
}
