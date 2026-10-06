//go:build !windows

package temper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsHeavyDotnetStep(t *testing.T) {
	cases := []struct {
		command string
		args    []string
		heavy   bool
	}{
		{"dotnet", []string{"build", "-c", "Release"}, true},
		{"dotnet", []string{"test", "--no-build"}, true},
		{"/usr/share/dotnet/dotnet", []string{"format"}, true},
		{"dotnet", []string{"--nologo", "build"}, true},
		{"dotnet", []string{"run"}, false},
		{"dotnet", []string{"restore"}, false},
		{"dotnet", []string{"exec", "testhost.dll"}, false},
		{"dotnet", []string{"--info"}, false},
		{"npm", []string{"run", "build"}, false},
	}
	for _, c := range cases {
		got := isHeavyDotnetStep(Step{Command: c.command, Args: c.args})
		assert.Equal(t, c.heavy, got, "%s %v", c.command, c.args)
	}
}

func TestAcquireHeavyLock_WithoutTheEnvTakesNoLock(t *testing.T) {
	t.Setenv(heavyLockEnv, "")
	release, held := acquireHeavyLock(context.Background(), Step{Command: "dotnet", Args: []string{"build"}})
	defer release()
	assert.False(t, held, "an install that sets no lock path must behave exactly as before")
}

func TestAcquireHeavyLock_SerialisesTwoHolders(t *testing.T) {
	t.Setenv(heavyLockEnv, filepath.Join(t.TempDir(), "lock"))
	defer func(p time.Duration) { heavyLockPoll = p }(heavyLockPoll)
	heavyLockPoll = 10 * time.Millisecond
	step := Step{Name: "api-build", Command: "dotnet", Args: []string{"build"}}

	releaseFirst, held := acquireHeavyLock(context.Background(), step)
	require.True(t, held)

	acquired := make(chan time.Time, 1)
	go func() {
		release, ok := acquireHeavyLock(context.Background(), step)
		if ok {
			acquired <- time.Now()
		}
		release()
	}()

	select {
	case <-acquired:
		t.Fatal("a second heavy step took the lock while the first still held it")
	case <-time.After(200 * time.Millisecond):
	}

	released := time.Now()
	releaseFirst()
	select {
	case at := <-acquired:
		assert.False(t, at.Before(released), "the second holder must get the lock only after the first releases it")
	case <-time.After(2 * time.Second):
		t.Fatal("the second heavy step never got the lock after the first released it")
	}
}

func TestAcquireHeavyLock_CancelledWhileWaitingReturnsUnheld(t *testing.T) {
	t.Setenv(heavyLockEnv, filepath.Join(t.TempDir(), "lock"))
	defer func(p time.Duration) { heavyLockPoll = p }(heavyLockPoll)
	heavyLockPoll = 10 * time.Millisecond
	step := Step{Name: "api-test", Command: "dotnet", Args: []string{"test"}}

	releaseFirst, held := acquireHeavyLock(context.Background(), step)
	require.True(t, held)
	defer releaseFirst()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	release, ok := acquireHeavyLock(ctx, step)
	release()
	assert.False(t, ok)
	assert.Less(t, time.Since(start), time.Second, "a cancelled wait must return promptly, not after the max wait")
}

func TestAcquireHeavyLock_GivesUpLoudlyAfterTheMaxWait(t *testing.T) {
	t.Setenv(heavyLockEnv, filepath.Join(t.TempDir(), "lock"))
	defer func(p, w time.Duration) { heavyLockPoll, heavyLockMaxWait = p, w }(heavyLockPoll, heavyLockMaxWait)
	heavyLockPoll, heavyLockMaxWait = 10*time.Millisecond, 100*time.Millisecond
	step := Step{Name: "api-build", Command: "dotnet", Args: []string{"build"}}

	releaseFirst, _ := acquireHeavyLock(context.Background(), step)
	defer releaseFirst()

	release, held := acquireHeavyLock(context.Background(), step)
	release()
	assert.True(t, held, "past the max wait the step runs, still marked held so the wrapper does not wait a second time")
}

// fakeDotnet writes a `dotnet` stand-in that prints whether Temper told it the lock is held.
func fakeDotnet(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dotnet")
	script := "#!/bin/sh\necho \"held=${" + heavyLockHeldEnv + ":-no} args=$*\"\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func TestRunStep_HeavyDotnetStepTellsTheChildTheLockIsHeld(t *testing.T) {
	t.Setenv(heavyLockEnv, filepath.Join(t.TempDir(), "lock"))
	dotnet := fakeDotnet(t)

	heavy := runStep(context.Background(), t.TempDir(), Step{Name: "api-build", Command: dotnet, Args: []string{"build"}}, time.Minute, DefaultOutputCap)
	assert.True(t, heavy.Passed)
	assert.Contains(t, heavy.Output, "held=1")

	light := runStep(context.Background(), t.TempDir(), Step{Name: "preview", Command: dotnet, Args: []string{"run"}}, time.Minute, DefaultOutputCap)
	assert.True(t, light.Passed)
	assert.Contains(t, light.Output, "held=no", "dotnet run is not locked, so its child must not be told otherwise")
}

func TestRunStep_WithoutTheEnvLeavesTheChildEnvAlone(t *testing.T) {
	t.Setenv(heavyLockEnv, "")
	res := runStep(context.Background(), t.TempDir(), Step{Name: "api-build", Command: fakeDotnet(t), Args: []string{"build"}}, time.Minute, DefaultOutputCap)
	assert.True(t, res.Passed)
	assert.True(t, strings.Contains(res.Output, "held=no"), res.Output)
}
