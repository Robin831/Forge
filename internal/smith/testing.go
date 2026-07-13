package smith

import "sync"

// NewProcessForTest creates a Process that has already completed and whose
// Wait() immediately returns the provided result. Intended for use in tests
// of packages that depend on smith without needing to spawn a real process.
func NewProcessForTest(result *Result) *Process {
	p := &Process{
		done:   make(chan struct{}),
		ioDone: make(chan struct{}),
		result: result,
	}
	close(p.ioDone)
	close(p.done)
	return p
}

// NewRunningProcessForTest creates a Process that appears to be running (its
// Done() channel stays open and IsRunning() reports true) until Interrupt or
// Kill is called, at which point Wait() returns the provided result. It has no
// real OS process, so Interrupt/Kill simply close the done channel via the
// onKill hook. Intended for steer-mode tests that need to observe interruption
// of an in-flight spawn without spawning a real subprocess.
func NewRunningProcessForTest(result *Result) *Process {
	p := &Process{
		done:   make(chan struct{}),
		ioDone: make(chan struct{}),
		result: result,
	}
	close(p.ioDone)
	var once sync.Once
	p.onKill = func() { once.Do(func() { close(p.done) }) }
	return p
}
