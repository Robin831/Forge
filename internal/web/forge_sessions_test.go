package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// forgeRequest issues an authenticated request with the X-Forge-Action
// header set so it satisfies the CSRF middleware.
func forgeRequest(t *testing.T, srv *Server, method, path, body, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	var req *http.Request
	if reader != nil {
		req = httptest.NewRequest(method, path, reader)
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("X-Forge-Action", "1")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

func TestForgeSessions_RequiresAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	rec := forgeRequest(t, srv, http.MethodGet, "/api/forge/sessions", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestForgeSessions_CreateAndList(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	body := `{"title":"Refactor poller","initial_message":"Let's break up the poll loop."}`
	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions", body, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Session struct {
			ID           int64  `json:"id"`
			Title        string `json:"title"`
			Status       string `json:"status"`
			MessageCount int    `json:"message_count"`
		} `json:"session"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if created.Session.ID == 0 {
		t.Fatal("expected non-zero session id")
	}
	if created.Session.Title != "Refactor poller" {
		t.Errorf("expected title preserved, got %q", created.Session.Title)
	}
	if created.Session.Status != "draft" {
		t.Errorf("expected default status draft, got %q", created.Session.Status)
	}
	if created.Session.MessageCount != 1 {
		t.Errorf("expected message_count=1, got %d", created.Session.MessageCount)
	}
	if created.Message.Role != "user" || created.Message.Content == "" {
		t.Errorf("unexpected first message: %+v", created.Message)
	}

	// List should include the just-created session.
	rec = forgeRequest(t, srv, http.MethodGet, "/api/forge/sessions", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var list struct {
		Sessions []struct {
			ID           int64  `json:"id"`
			Title        string `json:"title"`
			MessageCount int    `json:"message_count"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("parse list: %v", err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != created.Session.ID {
		t.Fatalf("expected 1 session in list, got %+v", list)
	}
	if list.Sessions[0].MessageCount != 1 {
		t.Errorf("expected message_count=1 in list, got %d", list.Sessions[0].MessageCount)
	}
}

func TestForgeSessions_AutoTitleFromInitialMessage(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	body := `{"initial_message":"Refactor the entire poller please."}`
	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions", body, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Session struct {
			Title string `json:"title"`
		} `json:"session"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(strings.ToLower(created.Session.Title), "refactor") {
		t.Errorf("expected auto-title to derive from message, got %q", created.Session.Title)
	}
}

func TestForgeSessions_GetReturnsMessages(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions",
		`{"title":"chat","initial_message":"hello"}`, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d", rec.Code)
	}
	var created struct {
		Session struct{ ID int64 } `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// Append a second message.
	rec = forgeRequest(t, srv, http.MethodPost,
		"/api/forge/sessions/"+itoa(created.Session.ID)+"/messages",
		`{"content":"second"}`, cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("append: %d body=%s", rec.Code, rec.Body.String())
	}

	rec = forgeRequest(t, srv, http.MethodGet,
		"/api/forge/sessions/"+itoa(created.Session.ID), "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	var got struct {
		Session struct {
			MessageCount int `json:"message_count"`
		} `json:"session"`
		Messages []struct {
			Content string `json:"content"`
			Role    string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got.Messages))
	}
	if got.Messages[0].Content != "hello" || got.Messages[1].Content != "second" {
		t.Errorf("messages out of order: %+v", got.Messages)
	}
	if got.Session.MessageCount != 2 {
		t.Errorf("expected message_count=2, got %d", got.Session.MessageCount)
	}
}

func TestForgeSessions_AppendRejectsAssistantRole(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions",
		`{"title":"chat"}`, cookie)
	var created struct {
		Session struct{ ID int64 } `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = forgeRequest(t, srv, http.MethodPost,
		"/api/forge/sessions/"+itoa(created.Session.ID)+"/messages",
		`{"role":"assistant","content":"hi"}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for assistant role, got %d", rec.Code)
	}
}

func TestForgeSessions_AppendRejectsEmptyContent(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions", `{"title":"x"}`, cookie)
	var created struct {
		Session struct{ ID int64 } `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = forgeRequest(t, srv, http.MethodPost,
		"/api/forge/sessions/"+itoa(created.Session.ID)+"/messages",
		`{"content":"   "}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestForgeSessions_RenameAndArchive(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions", `{"title":"orig"}`, cookie)
	var created struct {
		Session struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
		} `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = forgeRequest(t, srv, http.MethodPatch,
		"/api/forge/sessions/"+itoa(created.Session.ID),
		`{"title":"renamed"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: %d body=%s", rec.Code, rec.Body.String())
	}
	var renamed struct {
		Session struct {
			Title  string `json:"title"`
			Status string `json:"status"`
		} `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &renamed)
	if renamed.Session.Title != "renamed" {
		t.Errorf("expected title=renamed, got %q", renamed.Session.Title)
	}

	rec = forgeRequest(t, srv, http.MethodPatch,
		"/api/forge/sessions/"+itoa(created.Session.ID),
		`{"status":"archived"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive: %d body=%s", rec.Code, rec.Body.String())
	}
	var archived struct {
		Session struct{ Status string } `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &archived)
	if archived.Session.Status != "archived" {
		t.Errorf("expected status=archived, got %q", archived.Session.Status)
	}

	// Invalid status should be rejected.
	rec = forgeRequest(t, srv, http.MethodPatch,
		"/api/forge/sessions/"+itoa(created.Session.ID),
		`{"status":"bogus"}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bogus status, got %d", rec.Code)
	}
}

func TestForgeSessions_Delete(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	rec := forgeRequest(t, srv, http.MethodPost, "/api/forge/sessions",
		`{"title":"doomed","initial_message":"hi"}`, cookie)
	var created struct {
		Session struct{ ID int64 } `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = forgeRequest(t, srv, http.MethodDelete,
		"/api/forge/sessions/"+itoa(created.Session.ID), "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d body=%s", rec.Code, rec.Body.String())
	}

	rec = forgeRequest(t, srv, http.MethodGet,
		"/api/forge/sessions/"+itoa(created.Session.ID), "", cookie)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", rec.Code)
	}
}

func TestForgeSessions_CSRFEnforced(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest(http.MethodPost, "/api/forge/sessions",
		bytes.NewReader([]byte(`{"title":"x"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 without X-Forge-Action header, got %d", rec.Code)
	}
}

// itoa converts a positive int64 to its decimal string. We use a tiny
// helper rather than strconv.Itoa to avoid importing strconv just for the
// int64→string conversion in test URL building.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
