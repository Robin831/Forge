package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

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
}

func (s *stubRunner) Turn(_ context.Context, req forgechat.TurnRequest) (*forgechat.TurnResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if s.err != nil {
		return nil, s.err
	}
	if s.response == nil {
		return &forgechat.TurnResponse{
			Messages: []forgechat.EmittedMessage{{Kind: "text", Content: "default reply"}},
		}, nil
	}
	return s.response, nil
}

func (s *stubRunner) Calls() []forgechat.TurnRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]forgechat.TurnRequest, len(s.calls))
	copy(out, s.calls)
	return out
}

// turnResp is the JSON shape returned by /api/forge/sessions/{id}/turn.
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
	if rec.Code != http.StatusOK {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	var got turnResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Session.Stage != "drafting" {
		t.Fatalf("expected stage drafting, got %q", got.Session.Stage)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("expected 2 messages (user + assistant), got %d", len(got.Messages))
	}
	if got.Messages[0].Role != "user" || got.Messages[0].Content != "second user msg" {
		t.Fatalf("first message should be the user echo, got %+v", got.Messages[0])
	}
	if got.Messages[1].Role != "assistant" || got.Messages[1].Content != "claude says hi" {
		t.Fatalf("second message should be the assistant reply, got %+v", got.Messages[1])
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
	if rec.Code != http.StatusOK {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	var got turnResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Session.Plan != "# Plan\n- a\n- b" {
		t.Fatalf("session.plan should hold the emitted plan, got %q", got.Session.Plan)
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

	// Seed a plan so we can transition into grilling.
	stage := "drafting"
	plan := "# Plan\n- a"
	_, err := srv.db.UpdateForgeSessionStageAndPlan(id, &stage, &plan)
	if err != nil {
		t.Fatalf("seed plan: %v", err)
	}

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"start_grilling":true}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	var got turnResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Session.Stage != "grilling" {
		t.Fatalf("expected stage grilling, got %q", got.Session.Stage)
	}
	foundQuestion := false
	for _, m := range got.Messages {
		if m.Kind == "question" && m.Content == "Sync or async?" {
			foundQuestion = true
		}
	}
	if !foundQuestion {
		t.Fatalf("expected question message in response, got %+v", got.Messages)
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

	// Move to grilling and inject a question to answer.
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
	if rec.Code != http.StatusOK {
		t.Fatalf("answer: %d body=%s", rec.Code, rec.Body.String())
	}
	var got turnResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Find the persisted answer message.
	var answerFound bool
	for _, m := range got.Messages {
		if m.Kind == "answer" && m.Role == "user" && strings.Contains(m.Metadata, `"option_id":"async"`) {
			answerFound = true
		}
	}
	if !answerFound {
		t.Fatalf("expected user answer with option_id metadata, got %+v", got.Messages)
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

func TestForgeTurn_RunnerErrorReportsBadGateway(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	runner := &stubRunner{err: errors.New("upstream down")}
	srv.SetChatRunner(runner)
	cookie := loginAndGetCookie(t, srv)
	id := createForgeSessionHelper(t, srv, cookie, `{"initial_message":"start"}`)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when runner errors, got %d body=%s", rec.Code, rec.Body.String())
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
	if rec.Code != http.StatusOK {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
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
	if rec.Code != http.StatusOK {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
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
	// Insert directly via the DB so we can stage an ambiguous case-insensitive
	// anvil reference — the HTTP create handler rejects this at write time, but
	// resolveSessionAnvil still needs to handle the case for sessions that were
	// created before one of the colliding anvils was registered.
	sess, err := srv.db.CreateForgeSession(state.ForgeSession{
		Title: "ambiguous", CreatedBy: "alice", Anvil: "MUNIN",
	})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	id := sess.ID

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
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
	// Insert directly via the DB so we can stage an unknown anvil reference —
	// the HTTP create handler rejects this at write time, but resolveSessionAnvil
	// still needs to handle the case for sessions whose anvil was unregistered
	// after creation.
	sess, err := srv.db.CreateForgeSession(state.ForgeSession{
		Title: "unknown", CreatedBy: "alice", Anvil: "gone",
	})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	id := sess.ID

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions/"+itoa(id)+"/turn", `{"content":"hi"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("turn: %d body=%s", rec.Code, rec.Body.String())
	}
	calls := runner.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(calls))
	}
	if calls[0].Anvil != nil {
		t.Fatalf("unknown anvil should resolve to nil, got %+v", calls[0].Anvil)
	}
}

