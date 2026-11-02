package vardebeads

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A throttled or failing Varde is asked again; a refusal is not, and is an *ErrRefused.
func TestClientRetriesThrottlingButNotRefusals(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if strings.Contains(string(body), "beads_close") {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Refused (403): no"}],"isError":true}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[],"structuredContent":{"ok":true}}}`))
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL, Key: "k", HTTP: srv.Client(), Backoff: []time.Duration{time.Millisecond}}
	if _, err := c.Call(context.Background(), "beads_note", map[string]any{}); err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	_, err := c.Call(context.Background(), "beads_close", map[string]any{})
	if _, ok := err.(*ErrRefused); !ok || calls != 3 {
		t.Errorf("err=%v calls=%d", err, calls)
	}
}

func TestClientRefusesPlainHTTP(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvKeyFile, key)
	for url, ok := range map[string]bool{"https://varde.example": true, "http://localhost:8080": true, "http://127.0.0.1": true,
		"http://varde.example": false, "http://localhost.evil.example": false, "http://127.0.0.1.evil.example": false, "varde.example": false} {
		t.Setenv(EnvURL, url)
		if _, err := NewClientFromEnv(); (err == nil) != ok {
			t.Errorf("%s: err = %v", url, err)
		}
	}
}

// A 5xx is not retried for a create: a gateway's 502 can come after Varde wrote it.
func TestClientDoesNotRetryACreateAfterA5xx(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusBadGateway) }))
	defer srv.Close()
	c := &Client{URL: srv.URL, Key: "k", HTTP: srv.Client(), Backoff: []time.Duration{time.Millisecond}}
	if _, err := c.Call(context.Background(), "beads_create", map[string]any{}); err == nil || calls != 1 {
		t.Errorf("err=%v calls=%d", err, calls)
	}
}
