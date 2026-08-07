package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
)

const proxyBase = "preview.test"

// resolveRecorder is the in-process IPC handler the proxy resolves through. It
// records every preview_resolve so a test can assert both what was asked for
// and — just as importantly — that nothing was asked at all on the paths that
// must never enter the proxy.
type resolveRecorder struct {
	mu    sync.Mutex
	calls []ipc.PreviewResolvePayload
	reply func(ipc.PreviewResolvePayload) ipc.PreviewResolveResponse
}

func (r *resolveRecorder) handler(cmd ipc.Command) ipc.Response {
	if cmd.Type != "preview_resolve" {
		return ipc.Response{Type: "ok", Payload: []byte(`{}`)}
	}
	var p ipc.PreviewResolvePayload
	_ = json.Unmarshal(cmd.Payload, &p)
	r.mu.Lock()
	r.calls = append(r.calls, p)
	r.mu.Unlock()
	raw, _ := json.Marshal(r.reply(p))
	return ipc.Response{Type: "ok", Payload: raw}
}

func (r *resolveRecorder) recorded() []ipc.PreviewResolvePayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ipc.PreviewResolvePayload(nil), r.calls...)
}

// resolveTo answers every lookup with the address of the given upstream server.
func resolveTo(t *testing.T, upstreamURL string) *resolveRecorder {
	t.Helper()
	host, port := hostPort(t, upstreamURL)
	return &resolveRecorder{
		reply: func(p ipc.PreviewResolvePayload) ipc.PreviewResolveResponse {
			return ipc.PreviewResolveResponse{
				Found:   true,
				BeadID:  "Forge-abc1",
				Service: p.Service,
				Host:    host,
				Port:    port,
				Status:  "running",
			}
		},
	}
}

// resolveMiss answers every lookup with the given refusal reason.
func resolveMiss(reason string) *resolveRecorder {
	return &resolveRecorder{
		reply: func(ipc.PreviewResolvePayload) ipc.PreviewResolveResponse {
			return ipc.PreviewResolveResponse{Reason: reason}
		},
	}
}

func hostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse upstream url %q: %v", raw, err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split upstream host %q: %v", u.Host, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("upstream port %q: %v", portStr, err)
	}
	return host, port
}

// newProxyServer wires a Server whose preview proxy is on for proxyBase and
// resolves through rec. Passing an empty base leaves host-based routing off.
//
// Auth gating is switched off (preview_proxy_auth: none) so these tests
// exercise routing and forwarding on their own; the gate has its own file.
func newProxyServer(t *testing.T, base string, rec *resolveRecorder) *Server {
	t.Helper()
	srv := newGatedProxyServer(t, base, rec)
	srv.SetPreviewProxyAuth(func() string { return config.PreviewProxyAuthNone })
	return srv
}

// newGatedProxyServer is newProxyServer with the default auth posture left
// alone, i.e. every proxied request has to prove a Hearth session.
func newGatedProxyServer(t *testing.T, base string, rec *resolveRecorder) *Server {
	t.Helper()
	srv := newServerWithDefaults(t, rec.handler)
	if base != "" {
		srv.SetPreviewProxyBase(func() string { return base })
	}
	return srv
}

// startProxy runs the full router (request logger + preview proxy + routes) on
// a real listener, which the streaming and upgrade tests need — an
// httptest.ResponseRecorder neither streams nor hijacks.
func startProxy(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)
	return ts
}

// getVia issues a GET against the proxy server with an explicit Host header,
// which is the only thing that decides whether a request is proxied.
func getVia(t *testing.T, ts *httptest.Server, host, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", ts.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = host
	// DisableCompression on the inbound leg too, so the upstream's
	// Accept-Encoding tells us what the *proxy* added rather than what the test
	// client did.
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableCompression: true},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s (host %s): %v", path, host, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// echoUpstream reports the request exactly as it arrived, so the test can prove
// the forward changed nothing but the destination.
func echoUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"method":      r.Method,
			"host":        r.Host,
			"request_uri": r.RequestURI,
			"accept_enc":  r.Header.Get("Accept-Encoding"),
		})
	}))
	t.Cleanup(ts.Close)
	return ts
}

func decodeEcho(t *testing.T, resp *http.Response) map[string]string {
	t.Helper()
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	return out
}

// --- routing -------------------------------------------------------------

// A preview host forwards to the preview's port with the request untouched:
// same method, same Host header, same percent-encoded path and raw query. A dev
// server generating absolute URLs depends on every one of those.
func TestPreviewProxy_ForwardsRequestByteForByte(t *testing.T) {
	upstream := echoUpstream(t)
	rec := resolveTo(t, upstream.URL)
	ts := startProxy(t, newProxyServer(t, proxyBase, rec))

	host := "forge-abc1." + proxyBase
	resp := getVia(t, ts, host, "/assets/a%2Fb.js?v=1&q=a+b")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, readBody(t, resp))
	}
	echo := decodeEcho(t, resp)
	if echo["host"] != host {
		t.Errorf("Host header rewritten: got %q, want %q", echo["host"], host)
	}
	if want := "/assets/a%2Fb.js?v=1&q=a+b"; echo["request_uri"] != want {
		t.Errorf("request URI = %q, want %q", echo["request_uri"], want)
	}
	// The proxy adds no Accept-Encoding of its own, so it never has to decode a
	// body on the way back — it is a byte pipe, not a content negotiator.
	if echo["accept_enc"] != "" {
		t.Errorf("proxy added Accept-Encoding = %q, want none", echo["accept_enc"])
	}

	calls := rec.recorded()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one preview_resolve, got %d (%+v)", len(calls), calls)
	}
	if calls[0].Label != "forge-abc1" || calls[0].Service != "" {
		t.Errorf("resolved %+v, want label forge-abc1 with the entry service", calls[0])
	}
}

// The `<label>--<service>` form selects a named service; the label reaching the
// daemon must not carry the suffix.
func TestPreviewProxy_NamedServiceHostResolvesThatService(t *testing.T) {
	upstream := echoUpstream(t)
	rec := resolveTo(t, upstream.URL)
	ts := startProxy(t, newProxyServer(t, proxyBase, rec))

	resp := getVia(t, ts, "forge-abc1--api."+proxyBase, "/健康?ok=1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, readBody(t, resp))
	}
	calls := rec.recorded()
	if len(calls) != 1 {
		t.Fatalf("expected one preview_resolve, got %d", len(calls))
	}
	if calls[0].Label != "forge-abc1" || calls[0].Service != "api" {
		t.Errorf("resolved %+v, want label forge-abc1 service api", calls[0])
	}
}

// --- fall-through --------------------------------------------------------

// The apex is the dashboard's own name, not a preview: it must reach the normal
// router and never be looked up as a preview.
func TestPreviewProxy_ApexHostFallsThroughToRouter(t *testing.T) {
	rec := resolveMiss(ipc.PreviewResolveNoPreview)
	ts := startProxy(t, newProxyServer(t, proxyBase, rec))

	for _, host := range []string{proxyBase, "hearth.example.test", "a.forge-abc1." + proxyBase} {
		resp := getVia(t, ts, host, "/healthz")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("host %s: expected the router's 200, got %d", host, resp.StatusCode)
		}
	}
	if calls := rec.recorded(); len(calls) != 0 {
		t.Errorf("non-preview hosts entered the proxy path: %+v", calls)
	}
}

// With preview_proxy_base unset the middleware is inert, even for a Host that
// would otherwise look like a preview name.
func TestPreviewProxy_UnsetBasePassesEverythingThrough(t *testing.T) {
	rec := resolveMiss(ipc.PreviewResolveNoPreview)
	ts := startProxy(t, newProxyServer(t, "", rec))

	resp := getVia(t, ts, "forge-abc1."+proxyBase, "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the router's 200, got %d body=%s", resp.StatusCode, readBody(t, resp))
	}
	if calls := rec.recorded(); len(calls) != 0 {
		t.Errorf("proxy resolved with routing switched off: %+v", calls)
	}
}

// --- misses --------------------------------------------------------------

// Each refusal reason gets its own 404 body: whoever opened the URL needs to
// know whether the preview never existed or has been torn down.
func TestPreviewProxy_MissesAnswer404NamingTheState(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		host   string
		want   string
	}{
		{"unknown", ipc.PreviewResolveNoPreview, "forge-abc1." + proxyBase, "no live preview for forge-abc1"},
		{"disabled", ipc.PreviewResolveDisabled, "forge-abc1." + proxyBase, "no live preview for forge-abc1"},
		{"stopped", ipc.PreviewResolveStopped, "forge-abc1." + proxyBase, "preview forge-abc1 is stopped"},
		{"no service", ipc.PreviewResolveNoService, "forge-abc1--api." + proxyBase, `preview forge-abc1 has no service "api"`},
		{"no port", ipc.PreviewResolveNoPort, "forge-abc1." + proxyBase, "preview forge-abc1 has no port allocated yet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := resolveMiss(tc.reason)
			ts := startProxy(t, newProxyServer(t, proxyBase, rec))

			resp := getVia(t, ts, tc.host, "/")
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("expected 404, got %d", resp.StatusCode)
			}
			if got := strings.TrimSpace(readBody(t, resp)); got != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("Content-Type = %q, want text/plain", ct)
			}
			// One request, one answer — no retry loop behind the miss.
			if calls := rec.recorded(); len(calls) != 1 {
				t.Errorf("expected exactly one lookup, got %d", len(calls))
			}
		})
	}
}

// An upstream that cannot be reached is a 502 from the proxy, not a panic or a
// hang: the resolve succeeded, so the preview is registered but not answering.
func TestPreviewProxy_UnreachableUpstreamAnswers502(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	rec := resolveTo(t, deadURL)
	ts := startProxy(t, newProxyServer(t, proxyBase, rec))

	resp := getVia(t, ts, "forge-abc1."+proxyBase, "/")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", resp.StatusCode, readBody(t, resp))
	}
}

// --- streaming -----------------------------------------------------------

// FlushInterval = -1 means an SSE chunk reaches the browser when it is written,
// not when the upstream handler returns. The upstream here blocks until the
// test has read its first chunk, so a buffering proxy deadlocks instead of
// passing.
func TestPreviewProxy_StreamsChunksBeforeUpstreamReturns(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	rec := resolveTo(t, upstream.URL)
	ts := startProxy(t, newProxyServer(t, proxyBase, rec))

	req, err := http.NewRequest("GET", ts.URL+"/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "forge-abc1." + proxyBase
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	type read struct {
		line string
		err  error
	}
	first := make(chan read, 1)
	br := bufio.NewReader(resp.Body)
	go func() {
		line, err := br.ReadString('\n')
		first <- read{line, err}
	}()

	select {
	case got := <-first:
		if got.err != nil {
			t.Fatalf("reading first chunk: %v", got.err)
		}
		if strings.TrimSpace(got.line) != "data: first" {
			t.Fatalf("first chunk = %q, want %q", got.line, "data: first")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first SSE chunk never arrived — the proxy buffered the response")
	}

	// Only now let the upstream finish; the chunk above proves it streamed.
	close(release)
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("reading remainder: %v", err)
	}
	if !strings.Contains(string(rest), "data: second") {
		t.Errorf("remainder = %q, want it to contain the second chunk", rest)
	}
}

// --- websockets ----------------------------------------------------------

// A websocket upgrade survives the hop: ReverseProxy switches protocols and
// copies bytes both ways, which is what vite's HMR channel needs. The frames
// are plain lines rather than real websocket frames — what is under test is the
// 101 and the bidirectional copy, not the framing.
func TestPreviewProxy_WebsocketUpgradeRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "expected a websocket upgrade", http.StatusBadRequest)
			return
		}
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			http.Error(w, "hijack: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		if err := buf.Flush(); err != nil {
			return
		}
		for {
			line, err := buf.ReadString('\n')
			if err != nil {
				return
			}
			if _, err := buf.WriteString("echo:" + line); err != nil {
				return
			}
			if err := buf.Flush(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)

	rec := resolveTo(t, upstream.URL)
	ts := startProxy(t, newProxyServer(t, proxyBase, rec))

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(ts.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	host := "forge-abc1--hmr." + proxyBase
	_, err = fmt.Fprintf(conn,
		"GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", host)
	if err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101, got %d", resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		t.Errorf("Upgrade header not preserved: %q", resp.Header.Get("Upgrade"))
	}

	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read echoed frame: %v", err)
	}
	if strings.TrimSpace(line) != "echo:ping" {
		t.Errorf("echoed frame = %q, want %q", strings.TrimSpace(line), "echo:ping")
	}
}

// --- idle clock ----------------------------------------------------------

// Every proxied request resolves through the daemon exactly once, which is the
// call that bumps the preview's idle clock (see Daemon.handlePreviewResolve).
// Requests that fall through must not touch it, or browsing the dashboard would
// keep every preview alive forever.
func TestPreviewProxy_TouchesOncePerProxiedRequestOnly(t *testing.T) {
	upstream := echoUpstream(t)
	rec := resolveTo(t, upstream.URL)
	ts := startProxy(t, newProxyServer(t, proxyBase, rec))

	for i := 0; i < 3; i++ {
		resp := getVia(t, ts, "forge-abc1."+proxyBase, "/asset-"+strconv.Itoa(i))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, resp.StatusCode)
		}
		_ = readBody(t, resp)
	}
	// Fall-through traffic on the dashboard's own host.
	getVia(t, ts, proxyBase, "/healthz")
	getVia(t, ts, "hearth.example.test", "/healthz")

	calls := rec.recorded()
	if len(calls) != 3 {
		t.Fatalf("expected one lookup per proxied request (3), got %d: %+v", len(calls), calls)
	}
	for i, c := range calls {
		if c.Label != "forge-abc1" {
			t.Errorf("lookup %d resolved label %q, want forge-abc1", i, c.Label)
		}
	}
}
