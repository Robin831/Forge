package smith

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
