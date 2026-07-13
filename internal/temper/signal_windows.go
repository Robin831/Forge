//go:build windows

package temper

import "os"

// processSignaled always reports false on Windows: the Unix signal model does
// not apply, so infrastructure failures on Windows are detected via output
// markers (infraFailureRE) and the context-deadline check rather than the
// process exit signal.
func processSignaled(_ *os.ProcessState) (bool, string) {
	return false, ""
}
