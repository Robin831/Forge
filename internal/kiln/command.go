package kiln

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
	"github.com/Robin831/Forge/internal/hooks"
)

const (
	// DefaultCommandTimeout bounds a manifest's setup and teardown commands.
	// It is generous because setup routinely creates and migrates a database,
	// which is slow the first time a preview runs on a cold machine.
	DefaultCommandTimeout = 5 * time.Minute

	// commandOutputTail is how much of a failed command's output is carried in
	// the returned error. The full output always goes to the log file; the tail
	// is what an operator sees in Hearth without opening it.
	commandOutputTail = 2000
)

// CommandSpec is one manifest lifecycle command — `setup` or `teardown`.
// Service commands are not run through here: they are supervised processes with
// their own logs and health, see StartService.
type CommandSpec struct {
	// Name identifies the command in logs and errors ("setup"/"teardown").
	Name string
	// Command is the expanded command line to run.
	Command string
	// BeadID selects the log directory the output is appended to.
	BeadID string
	// WorktreePath is the preview checkout the command runs in.
	WorktreePath string
	// Env is the full environment for the process (see BuildEnv).
	Env []string
	// Timeout bounds the command. Zero uses DefaultCommandTimeout.
	Timeout time.Duration
	// Logger receives diagnostics. Optional.
	Logger *slog.Logger
}

// RunCommand runs a manifest lifecycle command to completion in the preview
// worktree and returns its failure, if any.
//
// An empty command is a no-op: setup and teardown are both optional, so callers
// can invoke this unconditionally. Output is appended to
// ~/.forge/logs/<beadID>/preview.log — beside the per-service logs and under the
// same retention sweep — and a tail of it rides along on the error, because
// "setup failed" without the psql error underneath it is not actionable.
//
// The command runs in its own process group and is killed as a group on timeout
// or cancellation: a setup script that shells out to a migration tool would
// otherwise leave that tool running after the preview is gone.
func RunCommand(ctx context.Context, spec CommandSpec) error {
	if strings.TrimSpace(spec.Command) == "" {
		return nil
	}
	logger := spec.Logger
	if logger == nil {
		logger = slog.Default()
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}

	logPath, err := LifecycleLogPath(spec.BeadID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("kiln: %s: creating log directory: %w", spec.Name, err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("kiln: %s: opening log %s: %w", spec.Name, logPath, err)
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "\n=== kiln preview %s: %s started %s ===\n%s\n",
		spec.BeadID, spec.Name, time.Now().Format(time.RFC3339), spec.Command)

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell, flag := hooks.ShellArgs()
	cmd := executil.HideWindow(exec.CommandContext(cmdCtx, shell, flag, spec.Command))
	executil.SetProcessGroup(cmd)
	cmd.Dir = spec.WorktreePath
	cmd.Env = spec.Env
	// exec's default cancel kills only the leader; the migration tool a setup
	// script spawned is the process that actually needs to go.
	cmd.Cancel = func() error { return executil.KillProcessTree(cmd) }

	var tail tailBuffer
	out := io.MultiWriter(logFile, &tail)
	cmd.Stdout = out
	cmd.Stderr = out

	runErr := cmd.Run()
	if runErr == nil {
		logger.Debug("kiln: preview lifecycle command finished",
			"bead", spec.BeadID, "command", spec.Name)
		return nil
	}

	fmt.Fprintf(logFile, "\n=== kiln: %s failed: %v ===\n", spec.Name, runErr)
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("kiln: preview %s command timed out after %s (see %s)%s",
			spec.Name, timeout, logPath, tail.suffix())
	}
	return fmt.Errorf("kiln: preview %s command failed: %w (see %s)%s",
		spec.Name, runErr, logPath, tail.suffix())
}

// LifecycleLogPath returns the file setup and teardown output is appended to:
// ~/.forge/logs/<beadID>/preview.log. The name cannot collide with a service's
// preview-<name>.log because manifest validation requires service names to start
// with an alphanumeric character.
func LifecycleLogPath(beadID string) (string, error) {
	dir, err := LogDir(beadID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "preview.log"), nil
}

// tailBuffer keeps the last commandOutputTail bytes written to it. A setup
// script can be arbitrarily chatty, and the error message only wants the end of
// it — which is where the failure is.
type tailBuffer struct {
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > commandOutputTail {
		p = p[len(p)-commandOutputTail:]
	}
	t.buf.Write(p)
	if t.buf.Len() > commandOutputTail {
		trimmed := t.buf.Bytes()[t.buf.Len()-commandOutputTail:]
		next := bytes.NewBuffer(nil)
		next.Write(trimmed)
		t.buf = *next
	}
	return n, nil
}

// suffix renders the captured tail for an error message, or "" when the command
// produced no output.
func (t *tailBuffer) suffix() string {
	out := strings.TrimSpace(t.buf.String())
	if out == "" {
		return ""
	}
	return ":\n" + out
}
