// Package ipc provides inter-process communication between the Forge daemon
// and CLI/TUI clients via platform-native mechanisms (named pipe on Windows,
// Unix domain socket on Linux/macOS).
//
// Protocol: newline-delimited JSON messages.
//
// Server side (daemon):
//
//	svr := ipc.NewServer()
//	svr.OnCommand(func(cmd Command) Response { ... })
//	svr.Start(ctx)
//
// Client side (CLI/TUI):
//
//	client := ipc.NewClient()
//	resp, err := client.Send(Command{Type: "status"})
//	events := client.Subscribe(ctx) // stream events
package ipc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/provider"
)

// Command is a message sent from a client to the daemon.
type Command struct {
	Type    string          `json:"type"`    // "status", "kill_worker", "refresh", "queue"
	Payload json.RawMessage `json:"payload"` // Type-specific data
}

// Response is a message sent from the daemon to a client.
type Response struct {
	Type    string          `json:"type"`    // "ok", "error", "status", "event"
	Payload json.RawMessage `json:"payload"` // Type-specific data
}

// Event is a push notification from daemon to subscribed clients.
type Event struct {
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// StatusPayload is the response for a "status" command.
type StatusPayload struct {
	Running    bool                      `json:"running"`
	PID        int                       `json:"pid"`
	Uptime     string                    `json:"uptime"`
	Workers    int                       `json:"workers"`
	QueueSize  int                       `json:"queue_size"`
	OpenPRs    int                       `json:"open_prs"`
	LastPoll   string                    `json:"last_poll"`
	Quotas     map[string]provider.Quota `json:"quotas,omitempty"`
}

// KillWorkerPayload is the payload for a "kill_worker" command.
type KillWorkerPayload struct {
	WorkerID string `json:"worker_id"`
	PID      int    `json:"pid"`
}

// CommandHandler is called by the server for each incoming command.
type CommandHandler func(cmd Command) Response

// Server listens for IPC connections from CLI/TUI clients.
type Server struct {
	listener net.Listener
	handler  CommandHandler
	clients  map[net.Conn]bool
	mu       sync.RWMutex
}

// NewServer creates a new IPC server.
func NewServer() *Server {
	return &Server{
		clients: make(map[net.Conn]bool),
	}
}

// OnCommand sets the handler for incoming commands.
func (s *Server) OnCommand(h CommandHandler) {
	s.handler = h
}

// Start begins listening for IPC connections. Blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	listener, err := listen()
	if err != nil {
		return fmt.Errorf("ipc listen: %w", err)
	}
	s.listener = listener

	// Close listener on context cancellation
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil // Normal shutdown
			default:
				return fmt.Errorf("ipc accept: %w", err)
			}
		}
		s.mu.Lock()
		s.clients[conn] = true
		s.mu.Unlock()

		go s.handleConn(ctx, conn)
	}
}

// Broadcast sends an event to all connected clients.
func (s *Server) Broadcast(evt Event) {
	data, err := json.Marshal(Response{
		Type:    "event",
		Payload: mustMarshal(evt),
	})
	if err != nil {
		return
	}
	data = append(data, '\n')

	s.mu.RLock()
	defer s.mu.RUnlock()

	for conn := range s.clients {
		// Non-blocking write with short deadline
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = conn.Write(data)
	}
}

// Close shuts down the server and all client connections.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for conn := range s.clients {
		conn.Close()
		delete(s.clients, conn)
	}

	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.clients, conn)
		s.mu.Unlock()
		conn.Close()
	}()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 64*1024) // 64KB max message

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var cmd Command
		if err := json.Unmarshal(scanner.Bytes(), &cmd); err != nil {
			resp := Response{
				Type:    "error",
				Payload: mustMarshal(map[string]string{"message": "invalid JSON"}),
			}
			writeResponse(conn, resp)
			continue
		}

		var resp Response
		if s.handler != nil {
			resp = s.handler(cmd)
		} else {
			resp = Response{
				Type:    "error",
				Payload: mustMarshal(map[string]string{"message": "no handler"}),
			}
		}
		writeResponse(conn, resp)
	}
}

// Client connects to the daemon's IPC socket.
type Client struct {
	conn net.Conn
}

// NewClient creates a new IPC client connected to the daemon.
func NewClient() (*Client, error) {
	conn, err := dial()
	if err != nil {
		return nil, fmt.Errorf("ipc connect: %w", err)
	}
	return &Client{conn: conn}, nil
}

// Send sends a command and waits for a response.
func (c *Client) Send(cmd Command) (*Response, error) {
	data, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("marshaling command: %w", err)
	}
	data = append(data, '\n')

	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Write(data); err != nil {
		return nil, fmt.Errorf("sending command: %w", err)
	}

	_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("reading response: %w", err)
		}
		return nil, fmt.Errorf("no response from daemon")
	}

	var resp Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	return &resp, nil
}

// Subscribe returns a channel that receives events from the daemon.
// The channel is closed when ctx is cancelled or the connection drops.
func (c *Client) Subscribe(ctx context.Context) <-chan Event {
	ch := make(chan Event, 32)
	go func() {
		defer close(ch)

		// Send subscribe command
		_, _ = c.Send(Command{Type: "subscribe"})

		scanner := bufio.NewScanner(c.conn)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}

			var resp Response
			if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
				continue
			}
			if resp.Type != "event" {
				continue
			}

			var evt Event
			if err := json.Unmarshal(resp.Payload, &evt); err != nil {
				continue
			}

			select {
			case ch <- evt:
			default:
				// Drop event if channel full
			}
		}
	}()
	return ch
}

// Close closes the client connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// --- Helpers ---

func writeResponse(conn net.Conn, resp Response) {
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write(data)
}

func mustMarshal(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
