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

// DecodeJSON decodes one JSON value from subprocess output that may contain
// leading or trailing non-JSON noise (log lines, diagnostics, etc.).
//
// It first tries to decode the whole output. On failure it walks every '{'
// position (then every '[' position) and attempts to decode starting there,
// returning the first candidate that succeeds. This handles cases where bd
// emits diagnostic lines like "[mysql] ... i/o timeout" or Go map prints
// such as "debug: map[key:{inner}]" before the real JSON payload.
func DecodeJSON(data []byte, v any) error {
	// Fast path: output is well-formed JSON (json.Decoder tolerates trailing
	// data after a complete value, so trailing log lines are fine).
	if err := tryDecodeJSON(data, v); err == nil {
		return nil
	}

	// Slow path: scan for candidate JSON start positions. Prefer '{' over '['
	// because bd emits JSON objects for create/update/show operations; arrays
	// are only used by a handful of list endpoints.
	for _, idx := range jsonCandidateOffsets(data, '{') {
		if err := tryDecodeJSON(data[idx:], v); err == nil {
			return nil
		}
	}
	for _, idx := range jsonCandidateOffsets(data, '[') {
		if err := tryDecodeJSON(data[idx:], v); err == nil {
			return nil
		}
	}

	return fmt.Errorf("decoding JSON from subprocess output: no valid JSON found; output snippet: %q", jsonSnippet(data, jsonSnippetLimit))
}

func tryDecodeJSON(data []byte, v any) error {
	return json.NewDecoder(bytes.NewReader(data)).Decode(v)
}

func jsonCandidateOffsets(data []byte, b byte) []int {
	var out []int
	for i := 0; i < len(data); i++ {
		if data[i] == b {
			out = append(out, i)
		}
	}
	return out
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
