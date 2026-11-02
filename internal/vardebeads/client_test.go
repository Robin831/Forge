package vardebeads

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
	t.Setenv(EnvURL, "http://varde.example")
	t.Setenv(EnvKeyFile, "/nowhere")
	if _, err := NewClientFromEnv(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("err = %v", err)
	}
}
