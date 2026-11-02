package vardebeads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"time"
)

// Client calls Varde's MCP endpoint with the Forge's key.
type Client struct {
	URL     string // Varde's base URL, without /mcp
	Key     string
	HTTP    *http.Client
	Backoff []time.Duration
}

// ErrRefused is a tool refusal: Varde answered, and said no. Its text is Varde's own.
type ErrRefused struct{ Text string }

func (e *ErrRefused) Error() string { return "varde refused: " + e.Text }

// NewClientFromEnv reads the URL and the key file named in the environment. The key never leaves this process.
func NewClientFromEnv() (*Client, error) {
	url, file := strings.TrimRight(os.Getenv(EnvURL), "/"), os.Getenv(EnvKeyFile)
	if url == "" || file == "" {
		return nil, fmt.Errorf("%s and %s must both be set", EnvURL, EnvKeyFile)
	}
	// The key is a bearer token: never over plain http, except to this machine.
	parsed, err := neturl.Parse(url)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname()))) {
		return nil, fmt.Errorf("%s must be an https URL", EnvURL)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("reading the Varde key: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return nil, fmt.Errorf("the Varde key file %s is empty", file)
	}
	return &Client{URL: url, Key: key, HTTP: &http.Client{Timeout: 60 * time.Second}}, nil
}

type toolResult struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Structured json.RawMessage `json:"structuredContent"`
	IsError    bool            `json:"isError"`
}

func isLoopback(host string) bool { return host == "localhost" || host == "127.0.0.1" || host == "::1" }

// Retried after a 5xx: tools whose second call changes nothing. A gateway's 502 can come after Varde wrote, so a create
// or a note is not sent twice; a 429 is refused before any write, so every tool is retried after one.
var idempotent = map[string]bool{"beads_label": true, "beads_dep": true, "beads_release": true, "beads_claim": true, "beads_close": true, "beads_show": true}

// Backoff is how long to wait before each retry of a 429 or 5xx; nil means two retries, 2 s then 6 s.
func (c *Client) backoff() []time.Duration {
	if c.Backoff != nil {
		return c.Backoff
	}
	return []time.Duration{2 * time.Second, 6 * time.Second}
}

func (c *Client) post(ctx context.Context, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.Key)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	return raw, res.StatusCode, err
}

// Call runs one tool and returns its structured answer. A refusal is *ErrRefused; anything else is a transport error.
func (c *Client) Call(ctx context.Context, tool string, args map[string]any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		return nil, err
	}
	var raw []byte
	for attempt := 0; ; attempt++ {
		var status int
		raw, status, err = c.post(ctx, body)
		if err != nil {
			return nil, fmt.Errorf("varde %s: %w", tool, err)
		}
		if status == http.StatusOK {
			break
		}
		if attempt < len(c.backoff()) && (status == http.StatusTooManyRequests || (status >= 500 && idempotent[tool])) {
			select {
			case <-time.After(c.backoff()[attempt]):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return nil, fmt.Errorf("varde %s: HTTP %d: %s", tool, status, strings.TrimSpace(string(raw[:min(len(raw), 300)])))
	}
	var answer struct {
		Result *toolResult `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("varde %s: not a JSON-RPC answer: %w", tool, err)
	}
	if answer.Error != nil {
		return nil, fmt.Errorf("varde %s: %s", tool, answer.Error.Message)
	}
	if answer.Result == nil {
		return nil, fmt.Errorf("varde %s: an answer without a result", tool)
	}
	if answer.Result.IsError {
		var text []string
		for _, part := range answer.Result.Content {
			text = append(text, part.Text)
		}
		return nil, &ErrRefused{Text: strings.Join(text, " ")}
	}
	return answer.Result.Structured, nil
}
