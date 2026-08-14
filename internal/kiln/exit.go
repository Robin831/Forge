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
