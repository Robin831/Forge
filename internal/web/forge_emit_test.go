package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Robin831/Forge/internal/forgechat"
	"github.com/Robin831/Forge/internal/state"
)

// fakeEmitRunner is a forgechat.Runner that returns a canned emission
// envelope. Used to avoid spinning up claude in unit tests.
type fakeEmitRunner struct {
	mu       sync.Mutex
	calls    []forgechat.TurnRequest
	envelope *forgechat.EmissionEnvelope
	err      error
}

func (f *fakeEmitRunner) Turn(_ context.Context, req forgechat.TurnRequest) (*forgechat.TurnResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.err != nil {
		return nil, f.err
	}
	return &forgechat.TurnResponse{Emission: f.envelope}, nil
}

// fakeBdRunner records every bd invocation and returns canned create ids.
type fakeBdRunner struct {
	mu        sync.Mutex
	calls     [][]string
	createIDs []string
	idIdx     int
	failOn    map[int]error
	createN   int
}

func (f *fakeBdRunner) run(_ context.Context, dir string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{"@" + dir}, args...))
	if len(args) == 0 {
		return nil, errors.New("fake bd: no args")
	}
	switch args[0] {
	case "create":
		idx := f.createN
		f.createN++
		if err, ok := f.failOn[idx]; ok {
			return nil, err
		}
		if f.idIdx >= len(f.createIDs) {
			return nil, fmt.Errorf("fake bd: out of ids")
		}
		id := f.createIDs[f.idIdx]
		f.idIdx++
		return []byte(fmt.Sprintf(`{"id":"%s"}`, id)), nil
	case "close":
		return []byte(`{"closed":true}`), nil
	default:
		return nil, fmt.Errorf("fake bd: unhandled %q", args[0])
	}
}

// seedReadySession creates a session, drives it to stage=ready with a plan,
// and returns the id. The chat history is intentionally minimal — emission
// only needs a plan to be present.
func seedReadySession(t *testing.T, srv *Server, cookie string) int64 {
	t.Helper()
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)
	stage := state.ForgeStageReady
	plan := "# Plan\n- step 1\n- step 2\n"
	if _, err := srv.db.UpdateForgeSessionStageAndPlan(id, &stage, &plan); err != nil {
		t.Fatalf("seed ready session: %v", err)
	}
	return id
}

func TestCreateBeads_NoRunnerReturns503(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	srv.SetAnvilLister(func() map[string]string { return map[string]string{"alpha": "/tmp/alpha"} })
	cookie := loginAndGetCookie(t, srv)
	id := seedReadySession(t, srv, cookie)

	rec := forgeRequest(t, srv, http.MethodPost,
		"/api/forge/sessions/"+itoa(id)+"/create-beads", "", cookie)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without runner, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateBeads_RequiresReadyStage(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	srv.SetChatRunner(&fakeEmitRunner{})
	srv.SetAnvilLister(func() map[string]string { return map[string]string{"alpha": "/tmp/alpha"} })
	cookie := loginAndGetCookie(t, srv)
	// Default stage is "drafting" — emission must reject this.
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"foo"}`)

	rec := forgeRequest(t, srv, http.MethodPost,
		"/api/forge/sessions/"+itoa(id)+"/create-beads", "", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when not in ready, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateBeads_NoAnvilsConfigured(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	srv.SetChatRunner(&fakeEmitRunner{})
	srv.SetAnvilLister(func() map[string]string { return map[string]string{} })
	cookie := loginAndGetCookie(t, srv)
	id := seedReadySession(t, srv, cookie)

	rec := forgeRequest(t, srv, http.MethodPost,
		"/api/forge/sessions/"+itoa(id)+"/create-beads", "", cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when no anvils, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateBeads_HappyPathPersistsAssistantMessageAndStatus(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &fakeEmitRunner{
		envelope: &forgechat.EmissionEnvelope{
			Summary: "two clean beads",
			Beads: []forgechat.BeadProposal{
				{ProposalID: "p1", Anvil: "alpha", Title: "First", Type: "feature", Priority: 1},
				{ProposalID: "p2", Anvil: "alpha", Title: "Second", Type: "task", Priority: 2, DependsOn: []string{"p1"}},
			},
		},
	}
	bd := &fakeBdRunner{createIDs: []string{"forge-aaa", "forge-bbb"}}
	srv.SetChatRunner(runner)
	srv.SetAnvilLister(func() map[string]string { return map[string]string{"alpha": "/anvils/alpha"} })
	srv.SetBdRunner(bd.run)
	cookie := loginAndGetCookie(t, srv)
	id := seedReadySession(t, srv, cookie)

	rec := forgeRequest(t, srv, http.MethodPost,
		"/api/forge/sessions/"+itoa(id)+"/create-beads", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp createBeadsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Beads) != 2 {
		t.Fatalf("expected 2 created beads, got %d", len(resp.Beads))
	}
	if resp.Beads[0].BeadID != "forge-aaa" || resp.Beads[1].BeadID != "forge-bbb" {
		t.Errorf("unexpected bead ids: %+v", resp.Beads)
	}
	if resp.Summary != "two clean beads" {
		t.Errorf("expected summary in response, got %q", resp.Summary)
	}
	// Two messages should have been appended: an assistant beads_created
	// and a system status.
	foundAssistant := false
	foundStatus := false
	for _, m := range resp.Messages {
		if m.Kind == state.ForgeMessageKindBeadsCreated {
			foundAssistant = true
			if !strings.Contains(m.Metadata, "forge-aaa") {
				t.Errorf("metadata should hold bead ids, got %q", m.Metadata)
			}
		}
		if m.Kind == state.ForgeMessageKindStatus {
			foundStatus = true
		}
	}
	if !foundAssistant {
		t.Error("expected a beads_created assistant message")
	}
	if !foundStatus {
		t.Error("expected a status message summarising the emission")
	}
	// Runner must be invoked in StageReady / ModeEmit.
	if len(runner.calls) != 1 {
		t.Fatalf("expected one runner call, got %d", len(runner.calls))
	}
	if runner.calls[0].Stage != forgechat.StageReady || runner.calls[0].Mode != forgechat.ModeEmit {
		t.Errorf("runner invoked with wrong stage/mode: %+v", runner.calls[0])
	}
	if runner.calls[0].Anvils["alpha"] == "" {
		t.Errorf("anvil registry should be passed to runner, got %+v", runner.calls[0].Anvils)
	}
}

func TestCreateBeads_RollsBackOnBdFailure(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &fakeEmitRunner{
		envelope: &forgechat.EmissionEnvelope{
			Beads: []forgechat.BeadProposal{
				{ProposalID: "p1", Anvil: "alpha", Title: "First", Type: "task", Priority: 2},
				{ProposalID: "p2", Anvil: "alpha", Title: "Second", Type: "task", Priority: 2, DependsOn: []string{"p1"}},
			},
		},
	}
	// Second create fails — we expect the first to be rolled back.
	bd := &fakeBdRunner{
		createIDs: []string{"forge-aaa"},
		failOn:    map[int]error{1: errors.New("dolt is down")},
	}
	srv.SetChatRunner(runner)
	srv.SetAnvilLister(func() map[string]string { return map[string]string{"alpha": "/anvils/alpha"} })
	srv.SetBdRunner(bd.run)
	cookie := loginAndGetCookie(t, srv)
	id := seedReadySession(t, srv, cookie)

	rec := forgeRequest(t, srv, http.MethodPost,
		"/api/forge/sessions/"+itoa(id)+"/create-beads", "", cookie)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 on bd failure, got %d body=%s", rec.Code, rec.Body.String())
	}
	// One create succeeded, then was rolled back.
	closeCount := 0
	for _, c := range bd.calls {
		if len(c) >= 2 && c[1] == "close" {
			closeCount++
		}
	}
	if closeCount != 1 {
		t.Errorf("expected one rollback close, got %d (%+v)", closeCount, bd.calls)
	}
	// A status message describing the failure should have been recorded.
	msgs, err := srv.db.ListForgeSessionMessages(id)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	foundFailure := false
	for _, m := range msgs {
		if m.Kind == state.ForgeMessageKindStatus &&
			strings.Contains(m.Content, "Bead emission failed") {
			foundFailure = true
		}
	}
	if !foundFailure {
		t.Error("expected a status message recording the emission failure")
	}
}

func TestCreateBeads_ValidationFailureReturns422(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &fakeEmitRunner{
		envelope: &forgechat.EmissionEnvelope{
			Beads: []forgechat.BeadProposal{
				// Cycle: p1 -> p2 -> p1.
				{ProposalID: "p1", Anvil: "alpha", Title: "1", Type: "task", Priority: 2, DependsOn: []string{"p2"}},
				{ProposalID: "p2", Anvil: "alpha", Title: "2", Type: "task", Priority: 2, DependsOn: []string{"p1"}},
			},
		},
	}
	bd := &fakeBdRunner{}
	srv.SetChatRunner(runner)
	srv.SetAnvilLister(func() map[string]string { return map[string]string{"alpha": "/anvils/alpha"} })
	srv.SetBdRunner(bd.run)
	cookie := loginAndGetCookie(t, srv)
	id := seedReadySession(t, srv, cookie)

	rec := forgeRequest(t, srv, http.MethodPost,
		"/api/forge/sessions/"+itoa(id)+"/create-beads", "", cookie)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 on cycle, got %d body=%s", rec.Code, rec.Body.String())
	}
	// No bd subprocess should have been invoked.
	if len(bd.calls) != 0 {
		t.Errorf("expected zero bd calls when validation rejects emission, got %d", len(bd.calls))
	}
}
