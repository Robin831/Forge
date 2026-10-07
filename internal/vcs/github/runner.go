package github

import (
	"bytes"
	"context"
	"os/exec"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// Runner executes name (gh or git) with args in dir and returns its stdout and
// stderr. The error is the process error, exactly as exec reports it.
type Runner func(ctx context.Context, dir, name string, args ...string) (stdout, stderr []byte, err error)

func execRunner(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	cmd := executil.HideWindow(exec.CommandContext(ctx, name, args...))
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func (p *Provider) run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	if p.runner != nil {
		return p.runner(ctx, dir, name, args...)
	}
	return execRunner(ctx, dir, name, args...)
}

func (p *Provider) nowFn() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}
