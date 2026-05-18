package queueactions

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/state"
)

// fakeHandle is a hand-rolled QueueHandle that records every call so tests
// can assert on the exact mutations and events the shared functions produce.
type fakeHandle struct {
	localForgeID string

	clarifications []clarifyCall
	cleared        []clearCall
	resets         []resetCall

	retryRecords map[string]*state.RetryRecord
	workers      map[string]*state.Worker
	workerStatus []workerStatusCall

	events []state.Event

	failReset bool
}

type clarifyCall struct {
	BeadID string
	Anvil  string
	Needed bool
	Reason string
}

type clearCall struct {
	BeadID string
	Anvil  string
}

type resetCall struct {
	BeadID string
	Anvil  string
}

type workerStatusCall struct {
	WorkerID string
	Status   state.WorkerStatus
}

func newFakeHandle() *fakeHandle {
	return &fakeHandle{
		localForgeID: "forge-a",
		retryRecords: map[string]*state.RetryRecord{},
		workers:      map[string]*state.Worker{},
	}
}

func (f *fakeHandle) LocalForgeID() string { return f.localForgeID }

func (f *fakeHandle) SetClarificationNeeded(beadID, anvil string, needed bool, reason string) error {
	f.clarifications = append(f.clarifications, clarifyCall{beadID, anvil, needed, reason})
	return nil
}

func (f *fakeHandle) ClearNeedsAttention(beadID, anvil string) error {
	f.cleared = append(f.cleared, clearCall{beadID, anvil})
	return nil
}

func (f *fakeHandle) GetRetry(beadID, anvil string) (*state.RetryRecord, error) {
	return f.retryRecords[beadID+"/"+anvil], nil
}

func (f *fakeHandle) ResetRetry(beadID, anvil string) error {
	if f.failReset {
		return errors.New("boom: ResetRetry")
	}
	f.resets = append(f.resets, resetCall{beadID, anvil})
	delete(f.retryRecords, beadID+"/"+anvil)
	return nil
}

func (f *fakeHandle) ActiveWorkerByBeadAndAnvil(beadID, anvil string) (*state.Worker, error) {
	return f.workers[beadID+"/"+anvil], nil
}

func (f *fakeHandle) UpdateWorkerStatus(workerID string, status state.WorkerStatus) error {
	f.workerStatus = append(f.workerStatus, workerStatusCall{workerID, status})
	return nil
}

func (f *fakeHandle) LogEvent(typ state.EventType, message, beadID, anvil string) error {
	f.events = append(f.events, state.Event{Type: typ, Message: message, BeadID: beadID, Anvil: anvil})
	return nil
}

func TestClarify(t *testing.T) {
	t.Run("success records clarification and event", func(t *testing.T) {
		h := newFakeHandle()
		err := Clarify(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin", Note: "which auth lib?"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := len(h.clarifications); got != 1 {
			t.Fatalf("expected 1 clarification call, got %d", got)
		}
		c := h.clarifications[0]
		if c.BeadID != "BD-1" || c.Anvil != "munin" || !c.Needed || c.Reason != "which auth lib?" {
			t.Fatalf("unexpected clarification call: %+v", c)
		}
		if len(h.events) != 1 || h.events[0].Type != state.EventClarificationNeeded {
			t.Fatalf("expected EventClarificationNeeded, got %+v", h.events)
		}
		if !strings.Contains(h.events[0].Message, "which auth lib?") {
			t.Fatalf("expected note in event message, got %q", h.events[0].Message)
		}
	})

	t.Run("requires bead id", func(t *testing.T) {
		h := newFakeHandle()
		err := Clarify(context.Background(), h, Params{AnvilName: "munin", Note: "x"})
		if !errors.Is(err, ErrMissingBeadID) {
			t.Fatalf("expected ErrMissingBeadID, got %v", err)
		}
	})

	t.Run("requires anvil", func(t *testing.T) {
		h := newFakeHandle()
		err := Clarify(context.Background(), h, Params{BeadID: "BD-1", Note: "x"})
		if !errors.Is(err, ErrMissingAnvil) {
			t.Fatalf("expected ErrMissingAnvil, got %v", err)
		}
	})

	t.Run("requires note", func(t *testing.T) {
		h := newFakeHandle()
		err := Clarify(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin"})
		if !errors.Is(err, ErrMissingReason) {
			t.Fatalf("expected ErrMissingReason, got %v", err)
		}
	})

	t.Run("forge mismatch rejected", func(t *testing.T) {
		h := newFakeHandle()
		err := Clarify(context.Background(), h, Params{
			BeadID:    "BD-1",
			AnvilName: "munin",
			Note:      "x",
			ForgeID:   "forge-b",
		})
		if !errors.Is(err, ErrForgeMismatch) {
			t.Fatalf("expected ErrForgeMismatch, got %v", err)
		}
		if len(h.clarifications) != 0 || len(h.events) != 0 {
			t.Fatalf("no state should have been mutated; got clarifications=%d events=%d", len(h.clarifications), len(h.events))
		}
	})

	t.Run("forge match accepted", func(t *testing.T) {
		h := newFakeHandle()
		err := Clarify(context.Background(), h, Params{
			BeadID:    "BD-1",
			AnvilName: "munin",
			Note:      "x",
			ForgeID:   "forge-a",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("empty forge id skips check", func(t *testing.T) {
		h := newFakeHandle()
		h.localForgeID = "forge-a"
		err := Clarify(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin", Note: "x"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestUnclarify(t *testing.T) {
	t.Run("success clears flag and logs event with note", func(t *testing.T) {
		h := newFakeHandle()
		err := Unclarify(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin", Note: "resolved offline"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(h.clarifications) != 1 {
			t.Fatalf("expected 1 clarification call, got %d", len(h.clarifications))
		}
		c := h.clarifications[0]
		if c.Needed || c.Reason != "" {
			t.Fatalf("expected unset clarification, got %+v", c)
		}
		if len(h.events) != 1 || h.events[0].Type != state.EventClarificationCleared {
			t.Fatalf("expected EventClarificationCleared, got %+v", h.events)
		}
		if !strings.Contains(h.events[0].Message, "resolved offline") {
			t.Fatalf("expected note in event message, got %q", h.events[0].Message)
		}
	})

	t.Run("note is optional", func(t *testing.T) {
		h := newFakeHandle()
		err := Unclarify(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(h.events) != 1 {
			t.Fatalf("expected 1 event, got %d", len(h.events))
		}
	})

	t.Run("forge mismatch rejected", func(t *testing.T) {
		h := newFakeHandle()
		err := Unclarify(context.Background(), h, Params{
			BeadID:    "BD-1",
			AnvilName: "munin",
			ForgeID:   "forge-b",
		})
		if !errors.Is(err, ErrForgeMismatch) {
			t.Fatalf("expected ErrForgeMismatch, got %v", err)
		}
	})
}

func TestRetry(t *testing.T) {
	t.Run("with circuit breaker", func(t *testing.T) {
		h := newFakeHandle()
		h.retryRecords["BD-1/munin"] = &state.RetryRecord{BeadID: "BD-1", Anvil: "munin", DispatchFailures: 3}
		err := Retry(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin", Note: "manual retry"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(h.resets) != 1 {
			t.Fatalf("expected 1 reset, got %d", len(h.resets))
		}
		if len(h.events) != 1 || h.events[0].Type != state.EventRetryReset {
			t.Fatalf("expected EventRetryReset, got %+v", h.events)
		}
		if !strings.Contains(h.events[0].Message, "manual retry") {
			t.Fatalf("expected note in event message, got %q", h.events[0].Message)
		}
		if !strings.Contains(h.events[0].Message, "Retry state reset") {
			t.Fatalf("expected circuit-breaker phrasing, got %q", h.events[0].Message)
		}
	})

	t.Run("without circuit breaker", func(t *testing.T) {
		h := newFakeHandle()
		err := Retry(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(h.events) != 1 || h.events[0].Type != state.EventRetryReset {
			t.Fatalf("expected EventRetryReset, got %+v", h.events)
		}
		if !strings.Contains(h.events[0].Message, "Retry reset") {
			t.Fatalf("expected no-circuit phrasing, got %q", h.events[0].Message)
		}
	})

	t.Run("circuit breaker reset failure surfaces error", func(t *testing.T) {
		h := newFakeHandle()
		h.retryRecords["BD-1/munin"] = &state.RetryRecord{BeadID: "BD-1", Anvil: "munin", DispatchFailures: 2}
		h.failReset = true
		err := Retry(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin"})
		if err == nil {
			t.Fatalf("expected error from failed reset")
		}
		if len(h.events) != 0 {
			t.Fatalf("no event should be logged on failed reset; got %+v", h.events)
		}
	})

	t.Run("forge mismatch rejected", func(t *testing.T) {
		h := newFakeHandle()
		h.retryRecords["BD-1/munin"] = &state.RetryRecord{BeadID: "BD-1", Anvil: "munin", DispatchFailures: 1}
		err := Retry(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin", ForgeID: "forge-b"})
		if !errors.Is(err, ErrForgeMismatch) {
			t.Fatalf("expected ErrForgeMismatch, got %v", err)
		}
		if len(h.resets) != 0 {
			t.Fatalf("expected no resets on mismatch, got %d", len(h.resets))
		}
	})
}

func TestClear(t *testing.T) {
	t.Run("success clears flags and logs event", func(t *testing.T) {
		h := newFakeHandle()
		err := Clear(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin", Note: "pr merged"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(h.cleared) != 1 {
			t.Fatalf("expected 1 clear call, got %d", len(h.cleared))
		}
		if len(h.events) != 1 || h.events[0].Type != state.EventRetryCleared {
			t.Fatalf("expected EventRetryCleared, got %+v", h.events)
		}
		if !strings.Contains(h.events[0].Message, "pr merged") {
			t.Fatalf("expected note in event message, got %q", h.events[0].Message)
		}
	})

	t.Run("forge mismatch rejected", func(t *testing.T) {
		h := newFakeHandle()
		err := Clear(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin", ForgeID: "forge-b"})
		if !errors.Is(err, ErrForgeMismatch) {
			t.Fatalf("expected ErrForgeMismatch, got %v", err)
		}
	})
}

func TestStop(t *testing.T) {
	t.Run("kills worker and sets clarification", func(t *testing.T) {
		h := newFakeHandle()
		h.workers["BD-1/munin"] = &state.Worker{ID: "w-1", BeadID: "BD-1", Anvil: "munin", PID: 0}
		err := Stop(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin", Note: "wrong approach"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(h.workerStatus) != 1 || h.workerStatus[0].WorkerID != "w-1" || h.workerStatus[0].Status != state.WorkerFailed {
			t.Fatalf("expected worker w-1 marked failed, got %+v", h.workerStatus)
		}
		if len(h.clarifications) != 1 || !h.clarifications[0].Needed || h.clarifications[0].Reason != "wrong approach" {
			t.Fatalf("expected clarification with reason 'wrong approach', got %+v", h.clarifications)
		}
		if len(h.events) != 1 || h.events[0].Type != state.EventBeadStopped {
			t.Fatalf("expected EventBeadStopped, got %+v", h.events)
		}
		if !strings.Contains(h.events[0].Message, "wrong approach") {
			t.Fatalf("expected note in event message, got %q", h.events[0].Message)
		}
	})

	t.Run("falls back to default reason", func(t *testing.T) {
		h := newFakeHandle()
		err := Stop(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h.clarifications[0].Reason != "manually stopped" {
			t.Fatalf("expected default reason, got %q", h.clarifications[0].Reason)
		}
	})

	t.Run("strips control characters from note", func(t *testing.T) {
		h := newFakeHandle()
		err := Stop(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin", Note: "bad\x07stuff\nok"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h.clarifications[0].Reason != "badstuff\nok" {
			t.Fatalf("expected control chars stripped, got %q", h.clarifications[0].Reason)
		}
	})

	t.Run("no worker is fine", func(t *testing.T) {
		h := newFakeHandle()
		err := Stop(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin", Note: "x"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(h.workerStatus) != 0 {
			t.Fatalf("expected no worker status updates, got %+v", h.workerStatus)
		}
	})

	t.Run("forge mismatch rejected", func(t *testing.T) {
		h := newFakeHandle()
		h.workers["BD-1/munin"] = &state.Worker{ID: "w-1", BeadID: "BD-1", Anvil: "munin"}
		err := Stop(context.Background(), h, Params{BeadID: "BD-1", AnvilName: "munin", ForgeID: "forge-b", Note: "x"})
		if !errors.Is(err, ErrForgeMismatch) {
			t.Fatalf("expected ErrForgeMismatch, got %v", err)
		}
		if len(h.workerStatus) != 0 || len(h.clarifications) != 0 || len(h.events) != 0 {
			t.Fatalf("no state should have been mutated; got status=%d clarifications=%d events=%d",
				len(h.workerStatus), len(h.clarifications), len(h.events))
		}
	})
}
