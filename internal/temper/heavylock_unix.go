//go:build !windows

package temper

import (
	"context"
	"log"
	"os"
	"syscall"
	"time"
)

// heavyLockPoll is how often a waiting step retries the lock.
var heavyLockPoll = time.Second

// heavyLockMaxWait bounds the wait. Past it the step runs unserialised with a loud log line, as
// a wedged holder must not stall Temper forever.
var heavyLockMaxWait = 30 * time.Minute

// acquireHeavyLock takes the forge-wide dotnet lock for a heavy dotnet step, before the step's
// deadline starts, so time spent queueing behind another worker's build is not charged to the
// step's own timeout. held reports whether the step's child should be told the lock is taken;
// release is always safe to call.
func acquireHeavyLock(ctx context.Context, step Step) (release func(), held bool) {
	noop := func() {}
	path := os.Getenv(heavyLockEnv)
	if path == "" || !isHeavyDotnetStep(step) {
		return noop, false
	}

	// os.OpenFile sets O_CLOEXEC, so the step's descendants never inherit the lock: it is held by
	// this daemon alone and released below, however long a child lingers.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		log.Printf("[temper] step %q runs without the dotnet lock: %v", step.Name, err)
		return noop, false
	}

	start := time.Now()
	waiting := false
	meter := lockWaitMeterFrom(ctx)
	defer func() {
		if waiting {
			meter.endWait()
		}
	}()
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		}
		if !waiting {
			log.Printf("[temper] step %q is waiting for another worker's dotnet build/test (lock %s)", step.Name, path)
			waiting = true
			meter.beginWait()
		}
		if time.Since(start) >= heavyLockMaxWait {
			log.Printf("[temper] step %q gave up on the dotnet lock after %s and runs unserialised", step.Name, heavyLockMaxWait)
			_ = f.Close()
			// Still tell the wrapper it is held, so the step does not wait a second time.
			return noop, true
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return noop, false
		case <-time.After(heavyLockPoll):
		}
	}
	if waiting {
		log.Printf("[temper] step %q waited %s for the dotnet lock", step.Name, time.Since(start).Round(time.Second))
	}

	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true
}
