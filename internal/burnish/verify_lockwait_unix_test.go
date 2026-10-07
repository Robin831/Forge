//go:build !windows

package burnish

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/temper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowDotnetConfig is one heavy dotnet step whose fake `dotnet` sleeps for work.
func slowDotnetConfig(t *testing.T, work time.Duration) temper.Config {
	t.Helper()
	dotnet := filepath.Join(t.TempDir(), "dotnet")
	script := "#!/bin/sh\nsleep " + work.String()[:len(work.String())-1] + "\n"
	require.NoError(t, os.WriteFile(dotnet, []byte(script), 0o755))
	return temper.Config{Steps: []temper.Step{{Name: "api-build", Command: dotnet, Args: []string{"build"}, Timeout: time.Minute}}}
}

func TestRunVerifyWithTimeout_LockWaitDoesNotSpendTheDeadline(t *testing.T) {
	// Two verifications contend for the dotnet lock. The second queues ~2s behind the first, then
	// works 2s: 4s+ in all against a 3s deadline, which it must not be charged for. (Fhi.Metadata-8py4x)
	t.Setenv("FORGE_DOTNET_LOCK", filepath.Join(t.TempDir(), "lock"))
	h := newTestHarness()
	defer h.restore()
	temperRunFn = temper.Run
	cfg := slowDotnetConfig(t, 2*time.Second)

	var wg sync.WaitGroup
	outcomes := make([]verifyOutcome, 2)
	for i := range outcomes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i] = runVerifyWithTimeout(context.Background(), 1, "bead", "anvil", t.TempDir(), cfg, nil, 3*time.Second)
		}(i)
		if i == 0 {
			time.Sleep(300 * time.Millisecond) // let the first take the lock
		}
	}
	wg.Wait()

	for i, o := range outcomes {
		assert.False(t, o.timedOut, "verification %d was charged for waiting on the lock", i)
		require.NotNil(t, o.result, "verification %d", i)
		assert.True(t, o.result.Passed, "verification %d", i)
	}
}

func TestRunVerifyWithTimeout_SlowWorkStillTimesOut(t *testing.T) {
	// No contention: genuinely slow work must still hit the deadline, not be excused by the meter.
	t.Setenv("FORGE_DOTNET_LOCK", filepath.Join(t.TempDir(), "lock"))
	h := newTestHarness()
	defer h.restore()
	temperRunFn = temper.Run

	start := time.Now()
	o := runVerifyWithTimeout(context.Background(), 1, "bead", "anvil", t.TempDir(), slowDotnetConfig(t, 5*time.Second), nil, time.Second)
	assert.True(t, o.timedOut)
	assert.Less(t, time.Since(start), 3*time.Second, "the deadline must fire on time when nothing waited")
}
