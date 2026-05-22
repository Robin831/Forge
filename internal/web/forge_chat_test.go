package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/forgechat"
	"github.com/Robin831/Forge/internal/state"
)

// stubRunner is a deterministic forgechat.Runner used by the web tests. It
// captures every call so assertions can verify which stage/mode was driven,
// and it returns a canned TurnResponse to drive the handler. This avoids
// shelling out to claude during unit tests.
type stubRunner struct {
	mu       sync.Mutex
	calls    []forgechat.TurnRequest
	response *forgechat.TurnResponse
	err      error
	// delay, when set, simulates a slow AI session.
	delay time.Duration
	// gate, when set, blocks Turn until the channel is closed. Lets tests
	// observe the in-flight Running state and exercise the 100ms 202
	// guarantee without racing real work.
	gate chan struct{}
}

func (s *stubRunner) Turn(ctx context.Context, req forgechat.TurnRequest) (*forgechat.TurnResponse, error) {
	s.mu.Lock()
	s.calls = append(s.calls, req)
	gate := s.gate
	delay := s.delay
	resp := s.response
	err := s.err
	s.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return &forgechat.TurnResponse{
			Messages: []forgechat.EmittedMessage{{Kind: "text", Content: "default reply"}},
		}, nil
	}
	return resp, nil
}

func (s *stubRunner) Calls() []forgechat.TurnRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]forgechat.TurnRequest, len(s.calls))
	copy(out, s.calls)
	return out
}

// acceptedResp is the JSON shape returned by the new async /turn endpoint
// when an AI turn was scheduled. The synchronous (no-AI) branches —
// mark_ready and the StageReady no-op — still return the legacy session DTO
// shape captured by turnResp below.
type acceptedResp struct {
	TurnID string `json:"turn_id"`
}

// turnResp is the JSON shape returned by the synchronous /turn paths
// (mark_ready, ready-stage no-op).
type turnResp struct {
	Session struct {
		ID    int64  `json:"id"`
		Stage string `json:"stage"`
		Plan  string `json:"plan"`
	} `json:"session"`
	Messages []struct {
		ID       int64  `json:"id"`
		Role     string `json:"role"`
		Kind     string `json:"kind"`
		Content  string `json:"content"`
		Metadata string `json:"metadata"`
	} `json:"messages"`
}

func createForgeSessionHelper(t *testing.T, srv *Server, cookie, body string) int64 {
	t.Helper()
	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions", body, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session: %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Session struct {
			ID int64 `json:"id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Session.ID == 0 {
		t.Fatal("expected session id")
	}
	return got.Session.ID
}

// waitForTurn blocks until the TurnState identified by turnID has closed its
// Done channel, or the test deadline elapses. The async handler returns 202
// before the goroutine finishes; tests must wait before asserting on
// persisted assistant messages.
func waitForTurn(t *testing.T, srv *Server, turnID string) *TurnState {
	t.Helper()
	st, ok := srv.TurnStore().Get(turnID)
	if !ok {
		t.Fatalf("turn %q not found in store", turnID)
	}
	select {
	case <-st.Done:
	case <-time.After(5 * time.Second):
		t.Fatalf("turn %q did not complete within 5s (status=%s)", turnID, st.Status())
	}
	return st
}

// decodeAccepted parses the 202 body and returns the turn id. Fails the test
// when the body is not a valid {"turn_id":"..."} envelope.
func decodeAccepted(t *testing.T, body []byte) string {
	t.Helper()
	var got acceptedResp
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode 202 body: %v (body=%s)", err, string(body))
	}
	if got.TurnID == "" {
		t.Fatalf("202 body missing turn_id (body=%s)", string(body))
	}
	return got.TurnID
}

func TestForgeTurn_NoRunnerReturns503(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without runner, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestForgeTurn_DraftingChatPersistsAssistantReply(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &stubRunner{
		response: &forgechat.TurnResponse{
			Messages: []forgechat.EmittedMessage{{Kind: "text", Content: "claude says hi"}},
		},
	}
	srv.SetChatRunner(runner)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"hello"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"second user msg"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	st := waitForTurn(t, srv, turnID)
	if st.Status() != TurnStatusComplete {
		t.Fatalf("expected complete status, got %s err=%v", st.Status(), st.Err())
	}

	msgs, err := srv.db.ListForgeSessionMessages(id)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	// Expect: initial_message (user), the new user msg, the assistant reply.
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(msgs), msgs)
	}
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" || last.Content != "claude says hi" {
		t.Fatalf("last message should be the assistant reply, got %+v", last)
	}
	if st.FinalMessageID() != last.ID {
		t.Fatalf("final_message_id should be last persisted message id %d, got %d", last.ID, st.FinalMessageID())
	}

	calls := runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(calls))
	}
	if calls[0].Stage != forgechat.StageDrafting {
		t.Fatalf("runner saw wrong stage: %q", calls[0].Stage)
	}
	if calls[0].Mode != forgechat.ModeChat {
		t.Fatalf("runner saw wrong mode: %q", calls[0].Mode)
	}
}

func TestForgeTurn_RequestPlanStoresPlanOnSession(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &stubRunner{
		response: &forgechat.TurnResponse{
			Messages: []forgechat.EmittedMessage{{Kind: "plan", Content: "# Plan\n- a\n- b"}},
			NewPlan:  "# Plan\n- a\n- b",
		},
	}
	srv.SetChatRunner(runner)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"design idea"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"request_plan":true}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	st := waitForTurn(t, srv, turnID)
	if st.Status() != TurnStatusComplete {
		t.Fatalf("expected complete, got %s err=%v", st.Status(), st.Err())
	}

	sess, err := srv.db.GetForgeSession(id)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if sess.Plan != "# Plan\n- a\n- b" {
		t.Fatalf("session.plan should hold the emitted plan, got %q", sess.Plan)
	}
	calls := runner.Calls()
	if len(calls) != 1 || calls[0].Mode != forgechat.ModePlan {
		t.Fatalf("plan request should drive ModePlan, got %+v", calls)
	}
}

func TestForgeTurn_StartGrillingWithoutPlanIsRejected(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	srv.SetChatRunner(&stubRunner{})
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"start_grilling":true}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when starting grilling with no plan, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestForgeTurn_GrillingProducesQuestionMessage(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &stubRunner{
		response: &forgechat.TurnResponse{
			Messages: []forgechat.EmittedMessage{{
				Kind:     "question",
				Content:  "Sync or async?",
				Metadata: `{"options":[{"id":"sync","label":"Sync"}],"recommendation":"sync"}`,
			}},
		},
	}
	srv.SetChatRunner(runner)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	stage := "drafting"
	plan := "# Plan\n- a"
	_, err := srv.db.UpdateForgeSessionStageAndPlan(id, &stage, &plan)
	if err != nil {
		t.Fatalf("seed plan: %v", err)
	}

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"start_grilling":true}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	st := waitForTurn(t, srv, turnID)
	if st.Status() != TurnStatusComplete {
		t.Fatalf("expected complete, got %s err=%v", st.Status(), st.Err())
	}

	sess, err := srv.db.GetForgeSession(id)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if sess.Stage != "grilling" {
		t.Fatalf("expected stage grilling, got %q", sess.Stage)
	}
	msgs, err := srv.db.ListForgeSessionMessages(id)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	foundQuestion := false
	for _, m := range msgs {
		if m.Kind == "question" && m.Content == "Sync or async?" {
			foundQuestion = true
		}
	}
	if !foundQuestion {
		t.Fatalf("expected question message in persisted history, got %+v", msgs)
	}
	calls := runner.Calls()
	if len(calls) != 1 || calls[0].Stage != forgechat.StageGrilling || calls[0].Mode != forgechat.ModeGrill {
		t.Fatalf("runner should be invoked in grilling/grill mode, got %+v", calls)
	}
}

func TestForgeTurn_AnswerWithOptionPersistsAnswerKindAndMetadata(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &stubRunner{
		response: &forgechat.TurnResponse{
			Messages: []forgechat.EmittedMessage{{Kind: "status", Content: "noted"}},
		},
	}
	srv.SetChatRunner(runner)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	stage := "grilling"
	plan := "# plan"
	if _, err := srv.db.UpdateForgeSessionStageAndPlan(id, &stage, &plan); err != nil {
		t.Fatalf("seed grilling: %v", err)
	}
	q, err := srv.db.AppendForgeSessionMessageRaw(id, "assistant", "question",
		"Sync or async?",
		`{"options":[{"id":"sync","label":"Sync"},{"id":"async","label":"Async (Recommended)"}],"recommendation":"async"}`)
	if err != nil {
		t.Fatalf("seed question: %v", err)
	}

	body := `{"answer_question_id":` + itoa(q.ID) + `,"answer_option_id":"async"}`
	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", body, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("answer: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	waitForTurn(t, srv, turnID)

	msgs, err := srv.db.ListForgeSessionMessages(id)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	var answerFound bool
	for _, m := range msgs {
		if m.Kind == "answer" && m.Role == "user" && strings.Contains(m.Metadata, `"option_id":"async"`) {
			answerFound = true
		}
	}
	if !answerFound {
		t.Fatalf("expected user answer with option_id metadata in persisted history, got %+v", msgs)
	}
}

func TestForgeTurn_MarkReadyTransitionsWithoutClaude(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &stubRunner{}
	srv.SetChatRunner(runner)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"mark_ready":true}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("mark ready: %d body=%s", rec.Code, rec.Body.String())
	}
	var got turnResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Session.Stage != "ready" {
		t.Fatalf("expected stage ready, got %q", got.Session.Stage)
	}
	if len(runner.Calls()) != 0 {
		t.Fatalf("mark_ready must not invoke the AI runner; got %d calls", len(runner.Calls()))
	}
}

func TestForgeTurn_RunnerErrorRecordedOnTurnState(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &stubRunner{err: errors.New("upstream down")}
	srv.SetChatRunner(runner)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	// The handler accepts the turn even when the AI runner will fail. The
	// async goroutine records the error on the TurnState; SSE/polling
	// surfaces it to the client.
	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	st := waitForTurn(t, srv, turnID)
	if st.Status() != TurnStatusError {
		t.Fatalf("expected error status, got %s", st.Status())
	}
	if st.Err() == nil || !strings.Contains(st.Err().Error(), "upstream down") {
		t.Fatalf("expected upstream down error, got %v", st.Err())
	}
}

// Forge-9w62: the drafting agent kept asking the user for the anvil path even
// though the session was created with an anvil association. The handler now
// resolves the path from the live registry and feeds it to the runner via
// TurnRequest.Anvil — verify the wiring end-to-end so a future refactor that
// drops the lookup fails this test loudly.
func TestForgeTurn_ResolvesSessionAnvilForRunner(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &stubRunner{}
	srv.SetChatRunner(runner)
	srv.SetAnvilLister(func() map[string]string {
		return map[string]string{"munin": "/repos/munin"}
	})
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start","anvil":"munin"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	waitForTurn(t, srv, turnID)
	calls := runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(calls))
	}
	if calls[0].Anvil == nil {
		t.Fatalf("runner should have received the resolved anvil, got nil")
	}
	if calls[0].Anvil.Name != "munin" || calls[0].Anvil.Path != "/repos/munin" {
		t.Fatalf("unexpected anvil: %+v", calls[0].Anvil)
	}
}

// Case-insensitive matching mirrors the daemon's anvil routing: a session
// stored as "Munin" still resolves against the registered "munin" key so a
// case-mismatch in the SPA doesn't reintroduce the "where is the codebase?"
// failure mode.
func TestForgeTurn_ResolvesAnvilCaseInsensitively(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &stubRunner{}
	srv.SetChatRunner(runner)
	srv.SetAnvilLister(func() map[string]string {
		return map[string]string{"munin": "/repos/munin"}
	})
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start","anvil":"Munin"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	waitForTurn(t, srv, turnID)
	calls := runner.Calls()
	if len(calls) != 1 || calls[0].Anvil == nil {
		t.Fatalf("expected resolved anvil, got %+v", calls)
	}
	if calls[0].Anvil.Name != "munin" || calls[0].Anvil.Path != "/repos/munin" {
		t.Fatalf("expected canonical anvil munin → /repos/munin, got %+v", calls[0].Anvil)
	}
}

// When two registry keys differ only by case (e.g. "munin" and "Munin") the
// case-insensitive fallback scan is ambiguous — map iteration order is random,
// so picking the first hit would be nondeterministic. resolveSessionAnvil must
// return nil in that case rather than feeding the runner a randomly-chosen path.
// The session name "MUNIN" has no exact-match key, so the fallback scan runs
// and encounters both "munin" and "Munin".
func TestForgeTurn_AmbiguousCaseAnvilProducesNilTarget(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &stubRunner{}
	srv.SetChatRunner(runner)
	srv.SetAnvilLister(func() map[string]string {
		return map[string]string{
			"munin": "/repos/munin-lower",
			"Munin": "/repos/munin-upper",
		}
	})
	cookie := loginAndGetCookie(t, srv)
	sess, err := srv.db.CreateForgeSession(state.ForgeSession{
		Title: "ambiguous", CreatedBy: "alice", Anvil: "MUNIN",
	})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	id := sess.ID

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	waitForTurn(t, srv, turnID)
	calls := runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(calls))
	}
	if calls[0].Anvil != nil {
		t.Fatalf("ambiguous case-insensitive anvil should resolve to nil, got %+v", calls[0].Anvil)
	}
}

// Unknown / unregistered anvil names should produce a nil Anvil rather than a
// stale half-resolved one — emitting "name: X, path: " would worse than
// nothing because the AI would fabricate the missing path. Verify the
// handler still succeeds (the AI can ask the user as a fallback) but does
// not feed the runner a poisoned anvil.
func TestForgeTurn_UnknownAnvilProducesNilTarget(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &stubRunner{}
	srv.SetChatRunner(runner)
	srv.SetAnvilLister(func() map[string]string {
		return map[string]string{"other": "/repos/other"}
	})
	cookie := loginAndGetCookie(t, srv)
	sess, err := srv.db.CreateForgeSession(state.ForgeSession{
		Title: "unknown", CreatedBy: "alice", Anvil: "gone",
	})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	id := sess.ID

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	waitForTurn(t, srv, turnID)
	calls := runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(calls))
	}
	if calls[0].Anvil != nil {
		t.Fatalf("unknown anvil should resolve to nil, got %+v", calls[0].Anvil)
	}
}

// The 202 response must arrive well before the AI work completes — the whole
// point of the async refactor. With the runner blocked on a gate channel, the
// handler should return within 100ms even though the goroutine is sitting in
// the runner indefinitely.
func TestForgeTurn_Returns202WithinTightDeadline(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	gate := make(chan struct{})
	runner := &stubRunner{gate: gate}
	srv.SetChatRunner(runner)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)
	defer close(gate) // unblock the goroutine so it exits cleanly

	start := time.Now()
	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	elapsed := time.Since(start)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("handler should return within 100ms (target) / 200ms (slack), took %s", elapsed)
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	if _, ok := srv.TurnStore().Get(turnID); !ok {
		t.Fatalf("expected turn %s to be in store", turnID)
	}
}

// The 15m backstop is enforced via context.WithTimeout in the handler. With
// the timeout dropped to a few milliseconds and the runner gated, the turn
// should reach the TurnStatusError terminal state with ctx.DeadlineExceeded.
func TestForgeTurn_HardTimeoutBackstopFires(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	srv.SetTurnTimeout(50 * time.Millisecond)
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	runner := &stubRunner{gate: gate}
	srv.SetChatRunner(runner)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d body=%s", rec.Code, rec.Body.String())
	}
	turnID := decodeAccepted(t, rec.Body.Bytes())
	st := waitForTurn(t, srv, turnID)
	if st.Status() != TurnStatusError {
		t.Fatalf("expected error status after timeout, got %s", st.Status())
	}
	if !errors.Is(st.Err(), context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", st.Err())
	}
}
