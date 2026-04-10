// Package executil provides helpers for spawning subprocesses.
package executil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
)

// DecodeJSON decodes one JSON value from subprocess output that may contain
// leading or trailing non-JSON noise (log lines, diagnostics, etc.).
// It uses json.NewDecoder which tolerates trailing data after the JSON value,
// and falls back to scanning for the first '{' or '[' to handle leading noise.
func DecodeJSON(data []byte, v any) error {
	// Fast path: output starts with JSON (tolerates trailing noise).
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err == nil {
		return nil
	}

	// Slow path: skip leading noise by finding the first JSON delimiter.
	if idx := bytes.IndexAny(data, "{["); idx > 0 {
		dec = json.NewDecoder(bytes.NewReader(data[idx:]))
		if err := dec.Decode(v); err == nil {
			return nil
		}
	}

	return fmt.Errorf("no valid JSON value found in %d bytes of output", len(data))
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
