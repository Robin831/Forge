package depcheck

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestNewScannerHasAUsableTimeout: Scanner.timeout is a DEADLINE, not a
// descriptor — scanGo derives its context from it and the .NET and npm scans
// hand it to their runners — so a scanner built without one runs every
// ecosystem scan against an already-expired deadline and reports "context
// deadline exceeded" for all of them. The on-demand dispatch path builds its
// scanner here rather than through New, so the floor lives here too.
func TestNewScannerHasAUsableTimeout(t *testing.T) {
	assert.GreaterOrEqual(t, newScanner(nil).timeout, minScanTimeout,
		"a scanner built without a configured timeout must still be able to scan")
}

// TestNewClampsTheConfiguredTimeout: the same floor applies to a configured
// value, and a value above it is kept as configured.
func TestNewClampsTheConfiguredTimeout(t *testing.T) {
	assert.Equal(t, minScanTimeout, New(nil, time.Hour, 0, nil).timeout)
	assert.Equal(t, minScanTimeout, New(nil, time.Hour, time.Second, nil).timeout)
	assert.Equal(t, 10*time.Minute, New(nil, time.Hour, 10*time.Minute, nil).timeout)
	assert.Equal(t, time.Hour, New(nil, time.Minute, 0, nil).interval, "the interval keeps its own floor")
}
