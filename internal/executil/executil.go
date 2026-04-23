// Package executil provides helpers for spawning subprocesses.
package executil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// DefaultBdTimeout is the default timeout for bd subprocess invocations.
// bd operations on anvils with remote Dolt (e.g. via kubectl port-forward)
// and GitHub auto-sync can routinely take 20-30 seconds per write, so the
// timeout must be generous enough to accommodate that latency.
const DefaultBdTimeout = 5 * time.Minute

// jsonSnippetLimit caps the raw output included in DecodeJSON error messages
// so a misbehaving subprocess can't blow up logs or downstream UIs.
const jsonSnippetLimit = 200

// maxJSONCandidates bounds the number of decode attempts in the slow path to
// avoid O(n*m) work on outputs with many braces/brackets.
const maxJSONCandidates = 16

// maxJSONScanBytes bounds how many bytes are scanned for JSON candidates so a
// misbehaving subprocess can't cause unbounded memory/CPU use.
const maxJSONScanBytes = 64 * 1024

// DecodeJSON decodes one JSON value from subprocess output that may contain
// leading or trailing non-JSON noise (log lines, diagnostics, etc.).
//
// It first tries to decode the whole output. On failure it walks top-level
// '{' positions (then top-level '[' positions) using bytes.IndexByte in a
// streaming loop, returning the first candidate that succeeds. Only positions
// at start-of-input or preceded by whitespace are considered, so nested
// objects inside arrays are not treated as standalone top-level values.
// The scan is bounded to maxJSONScanBytes bytes and maxJSONCandidates attempts.
func DecodeJSON(data []byte, v any) error {
	// Fast path: output is well-formed JSON (json.Decoder tolerates trailing
	// data after a complete value, so trailing log lines are fine).
	fastErr := tryDecodeJSON(data, v)
	if fastErr == nil {
		return nil
	}

	// Slow path: scan for candidate top-level JSON start positions. Prefer
	// '{' over '[' because bd emits JSON objects for most operations; arrays
	// are only used by a handful of list endpoints.
	if scanForJSON(data, '{', v) || scanForJSON(data, '[', v) {
		return nil
	}

	return fmt.Errorf("decoding JSON from subprocess output: no valid JSON found (%w); output snippet: %q",
		fastErr, jsonSnippet(data, jsonSnippetLimit))
}

// scanForJSON iterates over top-level occurrences of byte b in data using
// bytes.IndexByte, attempting to decode each as JSON into v. Returns true on
// first success. Bounded by maxJSONScanBytes and maxJSONCandidates.
func scanForJSON(data []byte, b byte, v any) bool {
	scan := data
	if len(scan) > maxJSONScanBytes {
		scan = scan[:maxJSONScanBytes]
	}
	attempts := 0
	for off := 0; off < len(scan); {
		rel := bytes.IndexByte(scan[off:], b)
		if rel < 0 {
			break
		}
		abs := off + rel
		if isTopLevelCandidate(scan, abs) {
			if tryDecodeJSON(scan[abs:], v) == nil {
				return true
			}
			attempts++
			if attempts >= maxJSONCandidates {
				break
			}
		}
		off = abs + 1
	}
	return false
}

// isTopLevelCandidate reports whether the byte at idx could be the start of a
// top-level JSON value: position 0 or preceded by whitespace. This prevents
// nested objects (e.g. '{' inside '[{...}]') from being decoded as standalone
// top-level values, which would mask upstream contract regressions.
func isTopLevelCandidate(data []byte, idx int) bool {
	if idx == 0 {
		return true
	}
	prev := data[idx-1]
	return prev == ' ' || prev == '\t' || prev == '\n' || prev == '\r'
}

func tryDecodeJSON(data []byte, v any) error {
	return json.NewDecoder(bytes.NewReader(data)).Decode(v)
}

func jsonSnippet(data []byte, max int) string {
	if len(data) <= max {
		return string(data)
	}
	return string(data[:max]) + "..."
}

// HideWindow configures cmd to not create a visible console window.
// On Windows this sets CREATE_NO_WINDOW. On other platforms it is a no-op.
func HideWindow(cmd *exec.Cmd) *exec.Cmd {
	hideWindow(cmd)
	return cmd
}

// SetProcessGroup configures cmd to start in its own process group.
// On Unix this sets Setpgid so signals can be sent to the entire group
// via kill(-pid, sig). On Windows this sets CREATE_NEW_PROCESS_GROUP.
func SetProcessGroup(cmd *exec.Cmd) *exec.Cmd {
	setProcessGroup(cmd)
	return cmd
}

// KillProcessTree forcibly terminates cmd's root process and every descendant.
// It is safe to call after cmd has already exited — in that case it best-effort
// reaps any background children that escaped the parent's lifetime (for example
// `npx http-server` started in the background by a build script).
//
// On Unix it requires cmd to have been started with SetProcessGroup so the
// descendants share a process group that can be signalled via kill(-pgid, sig).
// On Windows it shells out to `taskkill /T /F /PID <pid>` which walks the
// process tree by ParentProcessID and terminates every descendant.
//
// Returns nil when there is nothing to kill (cmd or cmd.Process is nil).
// Errors from the underlying signal/taskkill invocation are returned so callers
// can log them, but it is generally safe to ignore the error in teardown paths.
func KillProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return killProcessTree(cmd.Process.Pid)
}
