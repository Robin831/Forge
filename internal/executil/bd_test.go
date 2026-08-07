package executil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// bdShim puts a `bd` on PATH that runs the given shell script body, so tests
// can exercise the real subprocess/deadline path without a beads database.
func bdShim(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd shim is a POSIX shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bd"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writing bd shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestSetBdTimeout(t *testing.T) {
	t.Cleanup(func() { SetBdTimeout(0) })

	if got := BdTimeout(); got != DefaultBdTimeout {
		t.Fatalf("BdTimeout() = %s; want default %s", got, DefaultBdTimeout)
	}

	SetBdTimeout(90 * time.Second)
	if got := BdTimeout(); got != 90*time.Second {
		t.Errorf("BdTimeout() after SetBdTimeout(90s) = %s; want 90s", got)
	}

	// Zero and negative both restore the default rather than disabling the
	// deadline (an unbounded bd is what wedges the poll loop).
	for _, d := range []time.Duration{0, -1 * time.Second} {
		SetBdTimeout(d)
		if got := BdTimeout(); got != DefaultBdTimeout {
			t.Errorf("BdTimeout() after SetBdTimeout(%s) = %s; want default %s", d, got, DefaultBdTimeout)
		}
		SetBdTimeout(90 * time.Second) // re-arm so the next iteration is meaningful
	}
}

// TestBdCommand_ClassifiesDeadline asserts that a bd command killed by its
// deadline surfaces as a *BdTimeoutError naming the command and the time it
// got — not the bare "signal: killed" that exec.CommandContext produces and
// that made a killed `bd ready` unreadable in daemon.log.
func TestBdCommand_ClassifiesDeadline(t *testing.T) {
	dir := bdShim(t, "exec sleep 30")
	t.Cleanup(func() { SetBdTimeout(0) })
	SetBdTimeout(0)

	start := time.Now()
	cmd, cancel := BdCommandTimeout(context.Background(), 100*time.Millisecond,
		"ready", "--json", "--limit", "100")
	defer cancel()
	cmd.Dir = dir
	_, err := cmd.Output()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("command was not killed at its deadline (took %s)", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false; err = %v", err)
	}
	var timeoutErr *BdTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("errors.As(*BdTimeoutError) = false; err = %v", err)
	}
	if timeoutErr.Elapsed <= 0 {
		t.Errorf("BdTimeoutError.Elapsed = %s; want > 0", timeoutErr.Elapsed)
	}
	if timeoutErr.Limit != 100*time.Millisecond {
		t.Errorf("BdTimeoutError.Limit = %s; want 100ms", timeoutErr.Limit)
	}
	msg := err.Error()
	if !strings.Contains(msg, "bd ready") {
		t.Errorf("error message %q does not name the bd subcommand", msg)
	}
	if !strings.Contains(msg, "timed out after") {
		t.Errorf("error message %q does not report the elapsed time", msg)
	}
	if strings.Contains(msg, "signal: killed") {
		t.Errorf("error message %q still surfaces the bare kill signal", msg)
	}

	// A wrapping caller (as in poller.pollAnvil) keeps the classification.
	wrapped := fmt.Errorf("bd ready in forge (%s): %w", dir, err)
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Errorf("wrapped error lost its DeadlineExceeded classification: %v", wrapped)
	}
}

// TestBdCommand_UsesConfiguredTimeout asserts that BdCommand honours
// settings.bd_timeout rather than the built-in default.
func TestBdCommand_UsesConfiguredTimeout(t *testing.T) {
	dir := bdShim(t, "exec sleep 30")
	t.Cleanup(func() { SetBdTimeout(0) })
	SetBdTimeout(150 * time.Millisecond)

	cmd, cancel := BdCommand(context.Background(), "list", "--status=open", "--json")
	defer cancel()
	cmd.Dir = dir

	start := time.Now()
	_, err := cmd.CombinedOutput()
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("configured timeout was ignored (took %s)", elapsed)
	}
	var timeoutErr *BdTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("errors.As(*BdTimeoutError) = false; err = %v", err)
	}
	if timeoutErr.Limit != 150*time.Millisecond {
		t.Errorf("BdTimeoutError.Limit = %s; want the configured 150ms", timeoutErr.Limit)
	}
}

// TestBdCommand_CallerDeadlineWins asserts that when the caller's context
// expires before the bd timeout, the reported limit is the caller's remaining
// budget rather than the (larger) configured value — anvilhealth relies on this
// to hold its probe to a much tighter bound.
func TestBdCommand_CallerDeadlineWins(t *testing.T) {
	dir := bdShim(t, "exec sleep 30")
	t.Cleanup(func() { SetBdTimeout(0) })
	SetBdTimeout(0)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	cmd, cmdCancel := BdCommand(ctx, "sql", "--json", "SELECT 1")
	defer cmdCancel()
	cmd.Dir = dir
	_, err := cmd.Output()

	var timeoutErr *BdTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("errors.As(*BdTimeoutError) = false; err = %v", err)
	}
	if timeoutErr.Limit > time.Second {
		t.Errorf("BdTimeoutError.Limit = %s; want the caller's ~100ms budget", timeoutErr.Limit)
	}
}

// TestBdCommand_CancelIsNotATimeout asserts that a caller cancelling the
// context (e.g. daemon shutdown) is not misreported as a deadline overrun.
func TestBdCommand_CancelIsNotATimeout(t *testing.T) {
	dir := bdShim(t, "exec sleep 30")
	t.Cleanup(func() { SetBdTimeout(0) })
	SetBdTimeout(0)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	cmd, cmdCancel := BdCommand(ctx, "close", "Forge-abc1")
	defer cmdCancel()
	cmd.Dir = dir
	err := cmd.Run()

	if err == nil {
		t.Fatal("expected an error from the cancelled command, got nil")
	}
	var timeoutErr *BdTimeoutError
	if errors.As(err, &timeoutErr) {
		t.Errorf("cancellation was misreported as a deadline overrun: %v", err)
	}
}

// TestBdCommand_ExitErrorPassesThrough asserts that an ordinary bd failure
// (non-zero exit) is returned unchanged, so callers that inspect *exec.ExitError
// — ledger's "exit 1 but the JSON says closed" tolerance, schematic's "Added
// dependency" tolerance — keep working.
func TestBdCommand_ExitErrorPassesThrough(t *testing.T) {
	dir := bdShim(t, "echo 'boom' >&2\nexit 1")
	t.Cleanup(func() { SetBdTimeout(0) })
	SetBdTimeout(0)

	cmd, cancel := BdCommand(context.Background(), "show", "Forge-abc1", "--json")
	defer cancel()
	cmd.Dir = dir
	_, err := cmd.Output()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("errors.As(*exec.ExitError) = false; err = %v", err)
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("exit code = %d; want 1", exitErr.ExitCode())
	}
	var timeoutErr *BdTimeoutError
	if errors.As(err, &timeoutErr) {
		t.Errorf("a plain non-zero exit was misreported as a timeout: %v", err)
	}
}

// TestBdCommand_SuccessReturnsOutput covers the happy path: stdout is returned
// and no error classification kicks in.
func TestBdCommand_SuccessReturnsOutput(t *testing.T) {
	dir := bdShim(t, `printf '[]'`)
	t.Cleanup(func() { SetBdTimeout(0) })
	SetBdTimeout(0)

	cmd, cancel := BdCommand(context.Background(), "ready", "--json")
	defer cancel()
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("Output() error = %v", err)
	}
	if string(out) != "[]" {
		t.Errorf("Output() = %q; want %q", string(out), "[]")
	}
}
