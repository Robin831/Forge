//go:build !windows

package ipc

import (
	"net"
	"os"
	"path/filepath"
)

// listen creates a Unix domain socket listener.
func listen() (net.Listener, error) {
	sock := socketPath()

	// Remove stale socket file
	_ = os.Remove(sock)

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return nil, err
	}

	return net.Listen("unix", sock)
}

// dial connects to the daemon's Unix socket.
func dial() (net.Conn, error) {
	return net.Dial("unix", socketPath())
}

// socketPath returns the path to the Unix domain socket.
func socketPath() string {
	return filepath.Join(ForgeDir(), "forge.sock")
}

// cleanup removes the socket file on shutdown.
func cleanup() error {
	return os.Remove(socketPath())
}

// SocketExists checks whether the daemon's IPC socket file exists.
func SocketExists() bool {
	_, err := os.Stat(socketPath())
	return err == nil
}

// ForgeDir returns the Forge data directory.
func ForgeDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".forge")
}
