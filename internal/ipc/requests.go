package ipc

import (
	"encoding/json"
	"time"
)

// Request outcome states. A queued command starts as pending and ends as
// either ok or error. Unknown is never stored — it is what a lookup surface
// (e.g. GET /api/requests/{id}) reports when the ID has been evicted or was
// never tracked, so a stale browser tab reads as "outcome unknown" rather
// than as a failure.
const (
	RequestStatePending = "pending"
	RequestStateOK      = "ok"
	RequestStateError   = "error"
	RequestStateUnknown = "unknown"
)

// Bounds on the retained outcome records. A few hundred entries covers every
// realistic "operator clicked something and the SPA polls for the result"
// window; the TTL keeps a long-running daemon from pinning memory on requests
// nobody will ever ask about.
const (
	DefaultMaxRequestOutcomes = 500
	DefaultRequestOutcomeTTL  = time.Hour
)

// RequestOutcome is the observable state of an asynchronously queued command.
// It is what turns the request_id handed back with a "queued" response into
// something a client can resolve: pending while the goroutine runs, then ok or
// error with the daemon's message.
type RequestOutcome struct {
	RequestID string    `json:"request_id"`
	State     string    `json:"state"`
	Message   string    `json:"message,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RequestStatusPayload is the payload for a "request_status" command.
type RequestStatusPayload struct {
	RequestID string `json:"request_id"`
}

// RequestStatusResponse is the response payload for a "request_status"
// command. State is one of pending/ok/error/unknown; unknown means the ID is
// not (or is no longer) tracked, which is not itself a failure.
type RequestStatusResponse struct {
	RequestID string `json:"request_id"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// initOutcomesLocked lazily initialises the outcome store so a zero-value
// RequestTracker behaves like one built by NewRequestTracker.
func (rt *RequestTracker) initOutcomesLocked() {
	if rt.outcomes == nil {
		rt.outcomes = make(map[string]RequestOutcome)
	}
	if rt.maxOutcomes <= 0 {
		rt.maxOutcomes = DefaultMaxRequestOutcomes
	}
	if rt.outcomeTTL <= 0 {
		rt.outcomeTTL = DefaultRequestOutcomeTTL
	}
}

func (rt *RequestTracker) nowFunc() time.Time {
	if rt.now != nil {
		return rt.now()
	}
	return time.Now()
}

// recordOutcomeLocked stores (or overwrites) an outcome, expiring stale
// entries and evicting the oldest once the cap is reached. Callers must hold
// rt.mu.
func (rt *RequestTracker) recordOutcomeLocked(o RequestOutcome) {
	rt.initOutcomesLocked()
	rt.expireOutcomesLocked()
	if _, exists := rt.outcomes[o.RequestID]; !exists {
		rt.order = append(rt.order, o.RequestID)
	}
	rt.outcomes[o.RequestID] = o
	// Ring eviction: drop oldest-first until we are back within the cap.
	for len(rt.order) > rt.maxOutcomes {
		oldest := rt.order[0]
		rt.order = rt.order[1:]
		delete(rt.outcomes, oldest)
	}
}

// expireOutcomesLocked drops entries older than the TTL. Entries are appended
// in insertion order but refreshed in place on completion, so the scan cannot
// stop at the first live entry — it filters the whole slice. With a 500-entry
// cap that is trivially cheap.
func (rt *RequestTracker) expireOutcomesLocked() {
	if len(rt.order) == 0 {
		return
	}
	cutoff := rt.nowFunc().Add(-rt.outcomeTTL)
	kept := rt.order[:0]
	for _, id := range rt.order {
		o, ok := rt.outcomes[id]
		if !ok {
			continue
		}
		if o.UpdatedAt.Before(cutoff) {
			delete(rt.outcomes, id)
			continue
		}
		kept = append(kept, id)
	}
	rt.order = kept
}

// dropOutcomeLocked removes a retained outcome entirely. Used when a request
// is cancelled: no result was ever delivered, so "unknown" is the honest
// answer rather than a stuck "pending".
func (rt *RequestTracker) dropOutcomeLocked(requestID string) {
	if rt.outcomes == nil {
		return
	}
	if _, ok := rt.outcomes[requestID]; !ok {
		return
	}
	delete(rt.outcomes, requestID)
	for i, id := range rt.order {
		if id == requestID {
			rt.order = append(rt.order[:i], rt.order[i+1:]...)
			break
		}
	}
}

// Outcome returns the retained state for a request ID. The second return is
// false when the ID is unknown or has been evicted; callers should report that
// as RequestStateUnknown, never as a failure.
func (rt *RequestTracker) Outcome(requestID string) (RequestOutcome, bool) {
	if requestID == "" {
		return RequestOutcome{}, false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.initOutcomesLocked()
	rt.expireOutcomesLocked()
	o, ok := rt.outcomes[requestID]
	return o, ok
}

// OutcomeCount returns the number of retained outcome records (live entries
// only). Exposed for tests and diagnostics.
func (rt *RequestTracker) OutcomeCount() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.initOutcomesLocked()
	rt.expireOutcomesLocked()
	return len(rt.outcomes)
}

// OutcomeFromResult derives the terminal outcome of an async request from the
// completion result. A transport-level Err or an "error" response both land as
// RequestStateError so no failure can be reported as success.
func OutcomeFromResult(requestID string, result CompletionResult) RequestOutcome {
	o := RequestOutcome{RequestID: requestID, State: RequestStateOK}
	if result.Err != nil {
		o.State = RequestStateError
		o.Message = result.Err.Error()
		return o
	}
	o.Message = responseMessage(result.Response)
	if result.Response.Type == "error" {
		o.State = RequestStateError
		if o.Message == "" {
			o.Message = "command failed"
		}
	}
	return o
}

// responseMessage extracts the conventional {"message": "..."} field carried
// by both okResponse and errorResponse payloads. Payloads without one yield
// an empty string.
func responseMessage(resp Response) string {
	if len(resp.Payload) == 0 {
		return ""
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(resp.Payload, &body); err != nil {
		return ""
	}
	return body.Message
}
