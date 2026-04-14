package ipc

import (
	"encoding/json"
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
