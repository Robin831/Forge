//go:build !windows

package temper

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The failure these cover: exec.CommandContext cancels by calling
// cmd.Process.Kill() on the DIRECT child only, and because Stdout/Stderr are
// io.Writers, os/exec wires them through an os.Pipe whose write end every
// descendant inherits — so cmd.Wait blocked on the surviving grandchild, not
// on the process the deadline named. Measured on a real anvil: a `npm run
// lint` step killed at its deadline returned 168.7s late, recorded exit -1,
// and retained a complete eslint report that would have exited 0.
//
// Assertions are on the DURATION rather than on the call merely returning:
// before the fix it returned too, eventually, which is the whole problem.

// stepGraceBudget is what a killed step's wall clock is checked against — its
// own deadline plus the kill grace plus slack for a loaded CI host. It is
// deliberately far below the grandchild's own lifetime, so a step that waits
// for the orphan fails this rather than passing slowly.
const stepGraceBudget = 20 * time.Second

// runStepBounded is runStep with the regression made to fail rather than hang.
// The bug being covered makes runStep block for as long as the grandchild
// wants to live (300s here), so a plain call would wedge the package's tests
// until go test's own panic timeout and report nothing about which assertion
// was violated. The returned result is only ever the bounded one.
func runStepBounded(t *testing.T, ctx context.Context, dir string, step Step) (StepResult, time.Duration) {
	t.Helper()
	type outcome struct {
		res     StepResult
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		res := runStep(ctx, dir, step, DefaultStepTimeout, DefaultOutputCap)
		done <- outcome{res: res, elapsed: time.Since(start)}
	}()
	select {
	case o := <-done:
		return o.res, o.elapsed
	case <-time.After(stepGraceBudget):
		t.Fatalf("runStep(%q) did not return within %s of a %s deadline — cancellation is not reaching the step's descendants",
			step.Name, stepGraceBudget, step.Timeout)
		return StepResult{}, 0
	}
}

func TestStepKilledWithinGraceOfDeadline(t *testing.T) {
	dir := t.TempDir()
	timeout := 500 * time.Millisecond

	// `sleep 300 &` then `wait`: the shell is the direct child os/exec kills,
	// and the sleep is the descendant that inherits the pipe and outlives it.
	res, elapsed := runStepBounded(t, context.Background(), dir, Step{
		Name:    "spawns-grandchild",
		Command: "sh",
		Args:    []string{"-c", "sleep 300 & wait"},
		Timeout: timeout,
	})

	assert.False(t, res.Passed, "a step killed at its deadline is not a pass")
	assert.Less(t, elapsed, stepGraceBudget,
		"runStep must return within the kill grace of the step deadline, not when the grandchild finishes")
	assert.Less(t, res.Duration, stepGraceBudget,
		"the RECORDED duration must reflect the bounded run — an unattributable 206.3s against a 37.6s budget reads as a misconfigured timeout")
	assert.Equal(t, ClassificationTimeout, res.Classification)
	assert.True(t, res.Terminated)
}

func TestStepLeavesNoOrphanedDescendants(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pgid")

	// The shell records its own PID. runStep starts it with Setpgid, so the
	// process group id equals that PID and every descendant that did not call
	// setsid is in the group — which is what the assertion below probes.
	res, _ := runStepBounded(t, context.Background(), dir, Step{
		Name:    "spawns-grandchild",
		Command: "sh",
		Args:    []string{"-c", "echo $$ > " + pidFile + "; sleep 300 & wait"},
		Timeout: 500 * time.Millisecond,
	})
	require.True(t, res.Terminated)

	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err, "the step must have got far enough to record its pgid")
	pgid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	require.Positive(t, pgid)

	// Poll: SIGKILL is delivered promptly but the orphaned grandchild is
	// reparented and reaped asynchronously, and a zombie still answers
	// signal 0. ESRCH means the whole group is gone — the direct child alone
	// having exited would not produce it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := syscall.Kill(-pgid, 0)
		if err == syscall.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still exists after the step was killed (kill(-pgid, 0) = %v) — a descendant outlived the step", pgid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTimeoutRecordedDistinguishablyFromExitNonZero(t *testing.T) {
	dir := t.TempDir()

	killed, _ := runStepBounded(t, context.Background(), dir, Step{
		Name:    "killed",
		Command: "sh",
		Args:    []string{"-c", "echo all checks passed; sleep 300 & wait"},
		Timeout: 500 * time.Millisecond,
	})

	failed, _ := runStepBounded(t, context.Background(), dir, Step{
		Name:    "failed",
		Command: "sh",
		Args:    []string{"-c", "echo all checks passed; exit 3"},
		Timeout: 30 * time.Second,
	})

	require.False(t, killed.Passed)
	require.False(t, failed.Passed)

	// Both carry output that reads like a clean run; only one of them is a
	// verdict the command actually reached.
	assert.True(t, killed.Terminated, "a step killed by its deadline must be recorded as killed")
	assert.False(t, failed.Terminated, "a command that chose to exit 3 exited on its own")

	assert.Equal(t, ClassificationTimeout, killed.Classification)
	assert.Equal(t, ClassificationTestFailure, failed.Classification)

	assert.Equal(t, 3, failed.ExitCode)

	// The exit code alone cannot tell them apart (a signalled process is -1,
	// and so is a command that failed to start), so the retained output has to
	// say which it was — that pairing of exit -1 with a clean report is what
	// made a passing lint read as a real failure.
	assert.Contains(t, killed.Output, "TERMINATED by Forge")
	assert.NotContains(t, failed.Output, "TERMINATED by Forge")

	assert.Equal(t, "TIMEOUT", stepStatus(killed))
	assert.Equal(t, "FAIL", stepStatus(failed))
}

func TestCancelledParentClassifiesAsInfra(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	res, elapsed := runStepBounded(t, ctx, dir, Step{
		Name:    "cancelled",
		Command: "sh",
		Args:    []string{"-c", "sleep 300 & wait"},
		Timeout: 5 * time.Minute,
	})

	assert.Less(t, elapsed, stepGraceBudget)
	assert.True(t, res.Terminated)
	// Not a test failure: nothing about the code was established. Infra is
	// retryable without looping Smith over a failure that never happened.
	assert.Equal(t, ClassificationInfra, res.Classification)
	assert.True(t, res.Classification.IsRetryableWithoutSmith())
}

func TestKilledStepIsNotToleratedAsAHostCrash(t *testing.T) {
	dir := t.TempDir()

	// Output that satisfies the .NET host-crash carve-out exactly — an
	// all-passed summary plus a crash marker — printed by a step we then kill.
	// The carve-out reads that summary as evidence the run finished; a run we
	// ended part-way through has not finished.
	res, _ := runStepBounded(t, context.Background(), dir, Step{
		Name:              "dotnet-test",
		Command:           "sh",
		Args:              []string{"-c", "echo 'Passed!  - Failed:     0, Passed:   455'; echo 'Test host process crashed : Out of memory'; sleep 300 & wait"},
		Timeout:           500 * time.Millisecond,
		TolerateHostCrash: true,
	})

	assert.False(t, res.Passed, "a step Forge killed must never be tolerated into a PASS on output it printed before the kill")
	assert.True(t, res.Terminated)
	assert.Equal(t, ClassificationTimeout, res.Classification)
}

// The mirror image of the bug above, and the one the fix for it introduces if
// the verdict is read off the error rather than off the process: cmd.WaitDelay
// makes Wait give up on output pipes an orphan is holding and return
// exec.ErrWaitDelay — even when the command itself exited 0. Read as a failure
// that is once again "a passing lint recorded as a killed, failed step",
// arriving from the other side.
func TestCommandExitingZeroWithLingeringDescendantPasses(t *testing.T) {
	dir := t.TempDir()

	// The shell exits 0 immediately; the `sleep` it spawned inherits the write
	// end of the stdout pipe and holds it open well past the kill grace.
	res, elapsed := runStepBounded(t, context.Background(), dir, Step{
		Name:    "exits-clean-leaves-server",
		Command: "sh",
		Args:    []string{"-c", "echo 3 problems '(0 errors, 3 warnings)'; sleep 300 &"},
		Timeout: 30 * time.Second,
	})

	assert.True(t, res.Passed, "the command's own exit status is the verdict — a descendant on the pipe is not a failure")
	assert.False(t, res.Terminated, "nothing killed this command; it exited on its own")
	assert.Equal(t, 0, res.ExitCode)
	assert.Empty(t, res.Classification, "a passing step carries no failure classification")
	assert.Equal(t, "PASS", stepStatus(res))
	assert.NotContains(t, res.Output, "TERMINATED by Forge")

	// Still bounded: the wait delay is what stops Wait blocking on the orphan,
	// so the step returns shortly after the grace rather than after 300s.
	assert.Less(t, elapsed, stepGraceBudget,
		"WaitDelay must stop the wait on the inherited pipe rather than blocking until the descendant exits")
}
