package kiln

import (
	"fmt"
	"strings"
	"time"
)

// This file is the one renderer for "a preview service died": the phrase the
// daemon log, the feed event, `forge preview list` and the withheld-entry-URL
// note all use. It lives here rather than in each surface because the whole
// point of the state is that every surface agrees — a service that reads
// `exited (exit 1, lived 7m31s)` in the CLI must not read `failed` in the panel.

// CleanExit reports whether a death was the service deciding it was finished
// rather than something going wrong: status 0, and an actual status — a process
// killed by a signal has no exit code, and nothing about a preview service asks
// to be SIGKILLed.
//
// It is the restart policy's first refusal (claimRestartLocked) and it is
// exported because the surfaces that explain a *refusal* need the same test:
// "restart attempts exhausted" is only true of a death the budget was the
// reason for, and a clean exit is never restarted however much budget is left.
func CleanExit(exitCode *int) bool { return exitCode != nil && *exitCode == 0 }

// FormatExitCause renders why a service's process is gone: its exit status when
// it had one, else the cause from the wait error (a signal, typically), else a
// bare "exited".
func FormatExitCause(exitCode *int, exitErr error) string {
	if exitCode != nil {
		return fmt.Sprintf("exit %d", *exitCode)
	}
	if exitErr != nil {
		if cause := strings.TrimSpace(exitErr.Error()); cause != "" {
			return cause
		}
	}
	return "exited"
}

// FormatServiceExit renders a service's death as one clause:
// `exited (exit 1, lived 7m31s)`. The lifetime is part of it because it is what
// separates the two failures that look identical in a status column — a service
// that never worked, and one that worked for seven minutes and then stopped.
//
// A zero or negative lifetime is dropped rather than rendered as `lived 0s`:
// nothing was measured, and an invented number reads as a measurement.
func FormatServiceExit(exitCode *int, exitErr error, lifetime time.Duration) string {
	cause := FormatExitCause(exitCode, exitErr)
	if lifetime <= 0 {
		return fmt.Sprintf("exited (%s)", cause)
	}
	return fmt.Sprintf("exited (%s, lived %s)", cause, FormatLifetime(lifetime))
}

// FormatRestartAttempt renders which relaunch this is out of the budget:
// `attempt 1 of 3`. Both numbers are there because either alone is unreadable —
// "attempt 3" does not say whether anything follows, and a bare count does not
// say which one just happened.
func FormatRestartAttempt(attempt, max int) string {
	return fmt.Sprintf("attempt %d of %d", attempt, max)
}

// FormatServiceRestart renders the outcome of one relaunch under `restart:
// on-failure`, as the one clause the log line, the feed event and any panel
// share: `restarted (attempt 1 of 3): healthy`, or
// `restart failed (attempt 3 of 3): not healthy within 60s: ...`.
//
// The failure text is whatever the readiness check or the spawn reported,
// trimmed but otherwise verbatim — it is the only part of the sentence that
// says what to fix.
func FormatServiceRestart(attempt, max int, health string, err error) string {
	when := FormatRestartAttempt(attempt, max)
	if err == nil {
		return fmt.Sprintf("restarted (%s): %s", when, health)
	}
	cause := strings.TrimSpace(err.Error())
	if cause == "" {
		cause = "did not come back"
	}
	return fmt.Sprintf("restart failed (%s): %s", when, cause)
}

// FormatLifetime renders how long a service ran, at the resolution an operator
// reading a post-mortem needs: seconds up to a minute, `7m31s` up to an hour,
// `2h05m` beyond. It rounds to the second, so a preview log timestamp and this
// number describe the same moment.
func FormatLifetime(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
	default:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	}
}
