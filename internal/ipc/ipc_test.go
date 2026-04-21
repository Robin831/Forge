package ipc

import (
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewQueuedResponse(t *testing.T) {
	resp, err := NewQueuedResponse("req-123", "accepted")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Type != "queued" {
		t.Fatalf("expected type queued, got %s", resp.Type)
	}
	if resp.RequestID != "req-123" {
		t.Fatalf("expected request_id req-123, got %s", resp.RequestID)
	}

	var payload QueuedPayload
	if err := json.Unmarshal(resp.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Message != "accepted" {
		t.Fatalf("payload message mismatch: %s", payload.Message)
	}
}

func TestResponse_IsQueued(t *testing.T) {
	queued := &Response{Type: "queued"}
	if !queued.IsQueued() {
		t.Fatal("expected IsQueued true for type=queued")
	}

	ok := &Response{Type: "ok"}
	if ok.IsQueued() {
		t.Fatal("expected IsQueued false for type=ok")
	}

	var nilResp *Response
	if nilResp.IsQueued() {
		t.Fatal("expected IsQueued false for nil response")
	}
}

func TestRequestTracker_TrackAndComplete(t *testing.T) {
	rt := NewRequestTracker("test-")

	id, ch := rt.Track()
	if id == "" {
		t.Fatal("expected non-empty request ID")
	}
	if rt.Pending() != 1 {
		t.Fatalf("expected 1 pending, got %d", rt.Pending())
	}

	result := CompletionResult{
		Response: Response{Type: "ok"},
	}
	if !rt.Complete(id, result) {
		t.Fatal("Complete returned false for tracked ID")
	}

	select {
	case got := <-ch:
		if got.Response.Type != "ok" {
			t.Fatalf("expected ok, got %s", got.Response.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for completion")
	}

	if rt.Pending() != 0 {
		t.Fatalf("expected 0 pending after completion, got %d", rt.Pending())
	}
}

func TestRequestTracker_CompleteUnknown(t *testing.T) {
	rt := NewRequestTracker("test-")
	if rt.Complete("nonexistent", CompletionResult{}) {
		t.Fatal("Complete should return false for unknown ID")
	}
}

func TestRequestTracker_Cancel(t *testing.T) {
	rt := NewRequestTracker("test-")
	id, ch := rt.Track()

	rt.Cancel(id)

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to be closed without value")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out; channel should be closed")
	}

	if rt.Pending() != 0 {
		t.Fatalf("expected 0 pending after cancel, got %d", rt.Pending())
	}
}

func TestRequestTracker_CancelUnknown(t *testing.T) {
	rt := NewRequestTracker("test-")
	rt.Cancel("nonexistent") // should not panic
}

func TestRequestTracker_UniqueIDs(t *testing.T) {
	rt := NewRequestTracker("u-")
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id, _ := rt.Track()
		if seen[id] {
			t.Fatalf("duplicate ID: %s", id)
		}
		seen[id] = true
	}
}

func TestRequestTracker_Concurrent(t *testing.T) {
	rt := NewRequestTracker("c-")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, ch := rt.Track()
			go func() {
				rt.Complete(id, CompletionResult{Response: Response{Type: "ok"}})
			}()
			select {
			case <-ch:
			case <-time.After(5 * time.Second):
				t.Errorf("timed out waiting for completion of request %s", id)
			}
		}()
	}
	wg.Wait()
	if rt.Pending() != 0 {
		t.Fatalf("expected 0 pending, got %d", rt.Pending())
	}
}

func TestQueuedResponse_EmptyID(t *testing.T) {
	_, err := NewQueuedResponse("", "msg")
	if err == nil {
		t.Fatal("expected error for empty requestID, got nil")
	}
}

// newPipeClient wires an in-memory net.Pipe to a Client. The returned server
// side is the far end of the pipe — tests can read the command off it, sleep,
// and write the response to exercise the Client.Send read deadline.
func newPipeClient() (*Client, net.Conn) {
	clientSide, serverSide := net.Pipe()
	return &Client{conn: clientSide}, serverSide
}

// drainOne reads a single chunk from the pipe so Client.Send's write can
// proceed. The synchronous net.Pipe blocks the write until a reader drains
// it, so any test that doesn't intend to parse the command still needs this.
// Errors are reported via the returned channel so we don't call t.Fatalf from
// a non-test goroutine.
func drainOne(conn net.Conn, timeout time.Duration) <-chan error {
	ch := make(chan error, 1)
	go func() {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
		buf := make([]byte, 4096)
		_, err := conn.Read(buf)
		ch <- err
	}()
	return ch
}

// TestClientSend_CustomReadTimeoutSucceedsAfterSlowResponse mirrors the
// failure mode in the bead: a bd-backed handler that takes longer than the
// old hardcoded 3s. With an explicit ReadTimeout the client waits long enough
// to see the successful response.
func TestClientSend_CustomReadTimeoutSucceedsAfterSlowResponse(t *testing.T) {
	// Shrink the default so this test fails correctly if Client.Send forgets
	// to honor the per-command ReadTimeout override.
	prev := testOverrideDefaultReadTimeout(50 * time.Millisecond)
	defer testOverrideDefaultReadTimeout(prev)

	client, server := newPipeClient()
	defer client.Close()
	defer server.Close()

	writeDone := make(chan error, 1)
	go func() {
		_ = server.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 4096)
		if _, err := server.Read(buf); err != nil {
			writeDone <- err
			return
		}
		// Wait longer than the (shrunken) default — proves ReadTimeout applied.
		time.Sleep(300 * time.Millisecond)
		resp := Response{Type: "ok", Payload: json.RawMessage(`{"message":"slow ok"}`)}
		data, _ := json.Marshal(resp)
		data = append(data, '\n')
		_ = server.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, err := server.Write(data)
		writeDone <- err
	}()

	resp, err := client.Send(Command{Type: "slow", ReadTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Send with generous ReadTimeout failed: %v", err)
	}
	if resp.Type != "ok" {
		t.Fatalf("expected ok, got %s", resp.Type)
	}
	select {
	case srvErr := <-writeDone:
		if srvErr != nil {
			t.Fatalf("server write: %v", srvErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server goroutine did not finish")
	}
}

// TestClientSend_DefaultReadTimeoutFiresOnSlowResponse verifies the default
// remains snappy so trivial commands fail quickly when the daemon is hung
// rather than waiting for the bd-backed timeout.
func TestClientSend_DefaultReadTimeoutFiresOnSlowResponse(t *testing.T) {
	// Shrink the default just for this test so we don't wait 3 real seconds.
	prev := testOverrideDefaultReadTimeout(150 * time.Millisecond)
	defer testOverrideDefaultReadTimeout(prev)

	client, server := newPipeClient()
	defer client.Close()
	defer server.Close()

	// Drain the command from the pipe so Client.Send's write unblocks; never
	// write a response so the read deadline must fire on its own.
	drainErr := drainOne(server, 2*time.Second)

	start := time.Now()
	_, err := client.Send(Command{Type: "status"}) // no ReadTimeout → default
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timeout") && !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("default timeout fired after %v, expected well under 1s", elapsed)
	}
	if de := <-drainErr; de != nil {
		// Expected: once the client closes the conn in defer, the drain read
		// may return EOF; we only care that it didn't hang.
		_ = de
	}
}

// TestClientSend_ShortExplicitTimeoutIsBounded ensures a truly unresponsive
// daemon fails within the caller's requested budget rather than hanging
// indefinitely — the scenario the bead calls out as "genuinely dead daemon".
func TestClientSend_ShortExplicitTimeoutIsBounded(t *testing.T) {
	client, server := newPipeClient()
	defer client.Close()
	defer server.Close()

	drainErr := drainOne(server, 2*time.Second)

	start := time.Now()
	_, err := client.Send(Command{Type: "status", ReadTimeout: 250 * time.Millisecond})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > time.Second {
		t.Fatalf("explicit 250ms timeout fired after %v — should be bounded", elapsed)
	}
	<-drainErr
}

// TestCommand_ReadTimeoutNotSerialized keeps the new field client-only:
// the daemon (and the JSON on the wire) must not see a nonzero field for
// commands that don't care about it.
func TestCommand_ReadTimeoutNotSerialized(t *testing.T) {
	cmd := Command{Type: "run_bead", ReadTimeout: BdBackedReadTimeout}
	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "ReadTimeout") || strings.Contains(string(data), "read_timeout") {
		t.Fatalf("ReadTimeout must not appear on the wire, got %s", string(data))
	}
}

func TestQueuedResponseJSON(t *testing.T) {
	resp, err := NewQueuedResponse("abc", "queued for processing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Response
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != "queued" {
		t.Fatalf("expected type queued, got %s", decoded.Type)
	}
	if decoded.RequestID != "abc" {
		t.Fatalf("expected request_id abc, got %s", decoded.RequestID)
	}
}
