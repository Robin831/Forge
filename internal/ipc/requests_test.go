package ipc

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func okResp(message string) Response {
	payload, _ := json.Marshal(map[string]string{"message": message})
	return Response{Type: "ok", Payload: payload}
}

func errResp(message string) Response {
	payload, _ := json.Marshal(map[string]string{"message": message})
	return Response{Type: "error", Payload: payload}
}

func TestRequestTracker_TrackRecordsPending(t *testing.T) {
	rt := NewRequestTracker("forge-")
	id, _ := rt.Track()

	outcome, ok := rt.Outcome(id)
	if !ok {
		t.Fatalf("expected a retained outcome for %s", id)
	}
	if outcome.State != RequestStatePending {
		t.Fatalf("state: got %q, want %q", outcome.State, RequestStatePending)
	}
	if outcome.UpdatedAt.IsZero() {
		t.Error("expected UpdatedAt to be stamped")
	}
}

func TestRequestTracker_CompleteOverwritesPending(t *testing.T) {
	tests := []struct {
		name        string
		result      CompletionResult
		wantState   string
		wantMessage string
	}{
		{
			name:        "success",
			result:      CompletionResult{Response: okResp("label added")},
			wantState:   RequestStateOK,
			wantMessage: "label added",
		},
		{
			name:        "daemon error",
			result:      CompletionResult{Response: errResp("bd update failed: exit status 1")},
			wantState:   RequestStateError,
			wantMessage: "bd update failed: exit status 1",
		},
		{
			name:        "transport error",
			result:      CompletionResult{Err: errors.New("connection reset")},
			wantState:   RequestStateError,
			wantMessage: "connection reset",
		},
		{
			name:        "error without message",
			result:      CompletionResult{Response: Response{Type: "error"}},
			wantState:   RequestStateError,
			wantMessage: "command failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := NewRequestTracker("forge-")
			id, ch := rt.Track()
			if !rt.Complete(id, tc.result) {
				t.Fatal("Complete returned false for a tracked id")
			}
			<-ch // drain so the channel path is exercised as in production

			outcome, ok := rt.Outcome(id)
			if !ok {
				t.Fatal("expected a retained outcome after completion")
			}
			if outcome.State != tc.wantState {
				t.Errorf("state: got %q, want %q", outcome.State, tc.wantState)
			}
			if outcome.Message != tc.wantMessage {
				t.Errorf("message: got %q, want %q", outcome.Message, tc.wantMessage)
			}
		})
	}
}

func TestRequestTracker_OutcomeUnknownID(t *testing.T) {
	rt := NewRequestTracker("forge-")
	if _, ok := rt.Outcome("forge-nope"); ok {
		t.Error("expected an unknown id to report not-found")
	}
	if _, ok := rt.Outcome(""); ok {
		t.Error("expected an empty id to report not-found")
	}
}

func TestRequestTracker_CancelDropsOutcome(t *testing.T) {
	rt := NewRequestTracker("forge-")
	id, _ := rt.Track()
	rt.Cancel(id)

	if _, ok := rt.Outcome(id); ok {
		t.Error("a cancelled request must read as unknown, not pending")
	}
}

func TestRequestTracker_EvictsOldestBeyondCap(t *testing.T) {
	const limit = 3
	rt := NewRequestTrackerWithLimits("forge-", limit, time.Hour)

	ids := make([]string, 0, limit+2)
	for i := 0; i < limit+2; i++ {
		id, _ := rt.Track()
		rt.Complete(id, CompletionResult{Response: okResp(fmt.Sprintf("done %d", i))})
		ids = append(ids, id)
	}

	if got := rt.OutcomeCount(); got != limit {
		t.Fatalf("retained outcomes: got %d, want %d", got, limit)
	}
	for _, id := range ids[:2] {
		if _, ok := rt.Outcome(id); ok {
			t.Errorf("expected %s to be evicted", id)
		}
	}
	for _, id := range ids[2:] {
		if _, ok := rt.Outcome(id); !ok {
			t.Errorf("expected %s to be retained", id)
		}
	}
}

func TestRequestTracker_ExpiresByTTL(t *testing.T) {
	rt := NewRequestTrackerWithLimits("forge-", DefaultMaxRequestOutcomes, 30*time.Minute)
	now := time.Now()
	rt.now = func() time.Time { return now }

	id, _ := rt.Track()
	rt.Complete(id, CompletionResult{Response: okResp("done")})
	if _, ok := rt.Outcome(id); !ok {
		t.Fatal("expected the fresh outcome to be retained")
	}

	// Advance past the TTL: the record ages out and reads as unknown, which
	// the API reports as "unknown", never as a failure.
	now = now.Add(31 * time.Minute)
	if _, ok := rt.Outcome(id); ok {
		t.Error("expected the outcome to expire after the TTL")
	}
	if got := rt.OutcomeCount(); got != 0 {
		t.Errorf("retained outcomes after expiry: got %d, want 0", got)
	}
}

func TestRequestTracker_ZeroValueUsesDefaults(t *testing.T) {
	var rt RequestTracker
	id, _ := rt.Track()
	rt.Complete(id, CompletionResult{Response: errResp("boom")})

	outcome, ok := rt.Outcome(id)
	if !ok {
		t.Fatal("a zero-value tracker must still retain outcomes")
	}
	if outcome.State != RequestStateError || outcome.Message != "boom" {
		t.Errorf("unexpected outcome: %+v", outcome)
	}
}
