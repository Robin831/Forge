package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Robin831/Forge/internal/ipc"
)

// trackerHandler is a CommandHandler backed by a real ipc.RequestTracker. It
// answers action commands with a "queued" response and resolves the eventual
// outcome through the tracker, so the tests exercise the same correlation path
// the daemon uses instead of a hand-rolled stand-in.
type trackerHandler struct {
	tracker *ipc.RequestTracker
	// finish is invoked with the minted request ID as soon as a command is
	// queued, letting each test decide the async result.
	finish func(tracker *ipc.RequestTracker, requestID string)
}

func (h *trackerHandler) handle(cmd ipc.Command) ipc.Response {
	if cmd.Type == "request_status" {
		var p ipc.RequestStatusPayload
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return ipc.Response{Type: "error", Payload: []byte(`{"message":"invalid payload"}`)}
		}
		out := ipc.RequestStatusResponse{RequestID: p.RequestID, State: ipc.RequestStateUnknown}
		if outcome, ok := h.tracker.Outcome(p.RequestID); ok {
			out.State = outcome.State
			out.Message = outcome.Message
		}
		data, _ := json.Marshal(out)
		return ipc.Response{Type: "ok", Payload: data}
	}
	id, _ := h.tracker.Track()
	if h.finish != nil {
		h.finish(h.tracker, id)
	}
	resp, err := ipc.NewQueuedResponse(id, "queued")
	if err != nil {
		return ipc.Response{Type: "error", Payload: []byte(`{"message":"queue failed"}`)}
	}
	return resp
}

func failAsync(message string) func(*ipc.RequestTracker, string) {
	return func(tracker *ipc.RequestTracker, requestID string) {
		payload, _ := json.Marshal(map[string]string{"message": message})
		tracker.Complete(requestID, ipc.CompletionResult{
			Response: ipc.Response{Type: "error", Payload: payload},
		})
	}
}

func succeedAsync(message string) func(*ipc.RequestTracker, string) {
	return func(tracker *ipc.RequestTracker, requestID string) {
		payload, _ := json.Marshal(map[string]string{"message": message})
		tracker.Complete(requestID, ipc.CompletionResult{
			Response: ipc.Response{Type: "ok", Payload: payload},
		})
	}
}

// queuedCallSites covers both endpoints that can return a 202: the
// apply-dispatch-tag action (its own switch in actions.go) and the generic
// queued path in writeIPCResponse.
var queuedCallSites = []struct {
	name string
	path string
	body map[string]any
}{
	{
		name: "apply-dispatch-tag",
		path: "/api/queue/Forge-abc1/apply-dispatch-tag",
		body: map[string]any{"anvil": "forge"},
	},
	{
		name: "generic-queued-path",
		path: "/api/bead/Forge-abc1/close",
		body: map[string]any{"anvil": "forge"},
	},
}

// TestRequestStatus_QueuedFailureIsNotSuccess is the regression test for
// Forge-4r2n: a command accepted with a 202 whose async execution fails must
// be resolvable to a terminal error. Before the fix the 202 was the end of the
// story and the failure was silently discarded.
func TestRequestStatus_QueuedFailureIsNotSuccess(t *testing.T) {
	for _, site := range queuedCallSites {
		t.Run(site.name, func(t *testing.T) {
			h := &trackerHandler{
				tracker: ipc.NewRequestTracker("forge-"),
				finish:  failAsync("bd update failed: exit status 1"),
			}
			srv := newServerWithDefaults(t, h.handle)
			srv.SetAnvilDispatchTagLister(func() map[string]string {
				return map[string]string{"forge": "forgeReady"}
			})
			cookie := loginAndGetCookie(t, srv)

			rec := postAction(t, srv, cookie, site.path, site.body)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
			}
			var queued struct {
				Queued    bool   `json:"queued"`
				RequestID string `json:"request_id"`
				PollURL   string `json:"poll_url"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &queued); err != nil {
				t.Fatalf("parse 202 body: %v", err)
			}
			if !queued.Queued || queued.RequestID == "" {
				t.Fatalf("202 body must carry a request_id: %s", rec.Body.String())
			}
			if queued.PollURL == "" {
				t.Fatalf("202 body must carry a poll_url: %s", rec.Body.String())
			}

			var status ipc.RequestStatusResponse
			statusRec := getJSON(t, srv, cookie, queued.PollURL, &status)
			if statusRec.Code != http.StatusOK {
				t.Fatalf("expected 200 from %s, got %d", queued.PollURL, statusRec.Code)
			}
			if status.State != ipc.RequestStateError {
				t.Fatalf("state: got %q, want %q (body=%s)", status.State, ipc.RequestStateError, statusRec.Body.String())
			}
			if status.Message != "bd update failed: exit status 1" {
				t.Errorf("message: got %q, want the daemon's failure message", status.Message)
			}
		})
	}
}

// TestRequestStatus_QueuedSuccessResolvesOK guards the no-regression half: a
// queued command that succeeds still confirms success, with no spurious
// warning.
func TestRequestStatus_QueuedSuccessResolvesOK(t *testing.T) {
	for _, site := range queuedCallSites {
		t.Run(site.name, func(t *testing.T) {
			h := &trackerHandler{
				tracker: ipc.NewRequestTracker("forge-"),
				finish:  succeedAsync("label \"forgeReady\" added"),
			}
			srv := newServerWithDefaults(t, h.handle)
			srv.SetAnvilDispatchTagLister(func() map[string]string {
				return map[string]string{"forge": "forgeReady"}
			})
			cookie := loginAndGetCookie(t, srv)

			rec := postAction(t, srv, cookie, site.path, site.body)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
			}
			var queued struct {
				RequestID string `json:"request_id"`
				PollURL   string `json:"poll_url"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &queued); err != nil {
				t.Fatalf("parse 202 body: %v", err)
			}

			var status ipc.RequestStatusResponse
			getJSON(t, srv, cookie, queued.PollURL, &status)
			if status.State != ipc.RequestStateOK {
				t.Fatalf("state: got %q, want %q", status.State, ipc.RequestStateOK)
			}
		})
	}
}

// TestRequestStatus_PendingWhileInFlight verifies an unfinished command reads
// as pending — the SPA keeps polling rather than claiming either outcome.
func TestRequestStatus_PendingWhileInFlight(t *testing.T) {
	h := &trackerHandler{tracker: ipc.NewRequestTracker("forge-")}
	srv := newServerWithDefaults(t, h.handle)
	cookie := loginAndGetCookie(t, srv)

	rec := postAction(t, srv, cookie, "/api/bead/Forge-abc1/close", map[string]any{"anvil": "forge"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	var queued struct {
		PollURL string `json:"poll_url"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &queued)

	var status ipc.RequestStatusResponse
	getJSON(t, srv, cookie, queued.PollURL, &status)
	if status.State != ipc.RequestStatePending {
		t.Fatalf("state: got %q, want %q", status.State, ipc.RequestStatePending)
	}
}

// TestRequestStatus_UnknownIDIsNotAnError checks that an evicted or bogus id
// reports "unknown" with a 200 — a stale tab must not render a dropped record
// as a failure.
func TestRequestStatus_UnknownIDIsNotAnError(t *testing.T) {
	h := &trackerHandler{tracker: ipc.NewRequestTracker("forge-")}
	srv := newServerWithDefaults(t, h.handle)
	cookie := loginAndGetCookie(t, srv)

	var status ipc.RequestStatusResponse
	rec := getJSON(t, srv, cookie, "/api/requests/forge-does-not-exist", &status)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if status.State != ipc.RequestStateUnknown {
		t.Fatalf("state: got %q, want %q", status.State, ipc.RequestStateUnknown)
	}
}

func TestRequestStatus_RequiresAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	req := httptest.NewRequest("GET", "/api/requests/forge-1", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}
