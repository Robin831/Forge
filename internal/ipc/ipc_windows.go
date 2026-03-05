//go:build windows

package ipc

import (
	"net"
	"os"
	"path/filepath"

	"github.com/Microsoft/go-winio"
)

const pipeName = `\\.\pipe\forge`

// listen creates a Windows named pipe listener.
func listen() (net.Listener, error) {
	// Remove stale pipe (no-op if not exists)
	return winio.ListenPipe(pipeName, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;WD)", // Allow everyone
		MessageMode:        false,              // Byte mode for line-delim JSON
		InputBufferSize:    65536,
		OutputBufferSize:   65536,
	})
}

// dial connects to the daemon's named pipe.
func dial() (net.Conn, error) {
	return winio.DialPipe(pipeName, nil)
}

// socketPath returns the pipe name (for status display).
func socketPath() string {
	return pipeName
}

// cleanup removes the socket file. Named pipes are auto-cleaned on Windows.
func cleanup() error {
	return nil
}

// SocketExists checks if the daemon's IPC endpoint is available.
func SocketExists() bool {
	conn, err := winio.DialPipe(pipeName, nil)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ForgeDir returns the Forge data directory.
func ForgeDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".forge")
}
