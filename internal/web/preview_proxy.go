package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"time"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/kiln"
)

// Host-based preview routing (settings.preview_proxy_base).
//
// A preview's services bind ports allocated at start time out of
// preview_port_range, which rotate and which nothing outside the box can route
// to. When preview_proxy_base is set, Hearth fronts them by hostname instead:
// `<label>.<base>` reaches a bead's entry service and `<label>--<service>.<base>`
// one of its named services, both forwarded to the loopback port the service
// actually listens on.
//
// The match is on Host and nothing else. A dev server assumes it owns the URL
// root — vite's HMR websocket and absolute asset paths break the moment a path
// prefix is rewritten underneath it — so the forward leaves the request alone:
// same path, same query, same Host header, no decompression, no redirect
// following, no cookie jar. The single exception is Hearth's own auth cookies,
// which are removed on the way out (stripForgeAuthCookies) — they are the
// credential this middleware just checked, and the upstream is unreviewed
// branch code. Apex traffic (the dashboard's own host) never matches
// ParsePreviewHost and so never enters this path at all.

// previewTargetKey carries the resolved upstream "host:port" from the
// middleware to the shared Director. The proxy is process-wide (one connection
// pool for every preview), so the per-request target cannot live on it.
type previewTargetKey struct{}

// previewProxyTransport is the transport every proxied preview request uses.
//
// The timeouts are deliberately generous rather than absent: previews serve
// SSE, HMR websockets and long-poll, all of which hold a response open for
// minutes with no bytes flowing, so a conventional response-header or idle
// timeout would sever exactly the traffic the feature exists to carry. There is
// no overall write deadline for the same reason.
//
// ForceAttemptHTTP2 is off and compression is disabled on purpose: the upgrade
// path ReverseProxy uses for websockets needs the HTTP/1.1 transport's
// hop-by-hop handling, and re-encoding a body Forge does not read only burns
// CPU on both hops.
var previewProxyTransport = &http.Transport{
	// No proxy: previews are on loopback, and an ambient HTTP_PROXY pointing a
	// preview request at a corporate proxy would be nothing but a failure.
	Proxy: nil,
	DialContext: (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     false,
	DisableCompression:    true,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   32,
	IdleConnTimeout:       5 * time.Minute,
	ResponseHeaderTimeout: 5 * time.Minute,
	ExpectContinueTimeout: time.Second,
}

// previewReverseProxy is the single forwarder shared by every preview host, so
// the connection pool above is reused across previews and requests.
//
// FlushInterval of -1 flushes after every write, which is what makes SSE and
// HMR arrive as they are produced instead of being buffered until the upstream
// handler returns.
var previewReverseProxy = &httputil.ReverseProxy{
	Director:      previewDirector,
	Transport:     previewProxyTransport,
	FlushInterval: -1,
	ErrorHandler:  previewProxyErrorHandler,
}

// previewDirector rewrites the outbound request to point at the resolved
// upstream — and nothing else.
//
// It touches only URL.Scheme and URL.Host. Path, RawPath and RawQuery are left
// exactly as they arrived, and Host is left alone too: ReverseProxy sends
// Request.Host as the Host header, so the preview sees the hostname the browser
// asked for and any absolute URL it generates keeps working.
func previewDirector(req *http.Request) {
	target, _ := req.Context().Value(previewTargetKey{}).(string)
	req.URL.Scheme = "http"
	req.URL.Host = target
}

// previewProxyErrorHandler answers a failed forward with a 502 instead of the
// default handler's stderr log. A cancelled context is the client hanging up
// mid-stream — routine for SSE and websockets — and writing a header after the
// response has already begun would only produce a "superfluous WriteHeader"
// warning, so it is swallowed.
func previewProxyErrorHandler(w http.ResponseWriter, _ *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	writePreviewProxyError(w, http.StatusBadGateway, fmt.Sprintf("preview upstream unreachable: %v", err))
}

// SetPreviewProxyBase installs the callback that supplies the live
// settings.preview_proxy_base. The daemon passes a closure reading its current
// config so a hot-reload of the setting takes effect on the next request; nil
// (the default, and in most tests) means host-based routing is off and every
// request goes straight to the normal router.
func (s *Server) SetPreviewProxyBase(fn func() string) {
	s.previewProxyBase = fn
}

// SetPreviewProxyAuth installs the callback that supplies the live
// settings.preview_proxy_auth. Same shape and same reason as
// SetPreviewProxyBase — the daemon passes a closure reading its current config
// so flipping the setting takes effect on the next request. nil (the default,
// and in most tests) leaves the gated mode in place.
func (s *Server) SetPreviewProxyAuth(fn func() string) {
	s.previewProxyAuth = fn
}

// proxyBase returns the configured preview proxy base, or "" when host-based
// routing is switched off.
func (s *Server) proxyBase() string {
	if s.previewProxyBase == nil {
		return ""
	}
	return kiln.NormalizeHostname(s.previewProxyBase())
}

// PreviewProxyMiddleware forwards requests addressed to a preview hostname to
// the preview's own port, and passes everything else through to next.
//
// It is installed ahead of routing (see routes) so a preview host never reaches
// the dashboard's route table, and it is a strict pass-through in three cases:
// host-based routing is off, the Host is not a preview name, or the Host is the
// apex itself. Only a Host that ParsePreviewHost accepts is ever proxied, which
// is what keeps the dashboard's own traffic — including /login and /api — out
// of this path entirely.
//
// A host that names no live preview is answered in one request with a 404 whose
// body says which state it is in; there is no retry and no fallback to the SPA,
// because rendering the dashboard at a preview URL would be a confusing lie.
//
// The middleware runs ahead of requireAuth, so a proxied preview is gated by
// its own check rather than the router's: see authorizePreviewRequest in
// preview_auth.go. Unless settings.preview_proxy_auth is "none", a request that
// cannot show a Hearth session or a preview grant is refused here — a wildcard
// DNS record is a much wider audience than the loopback ports this replaces,
// and there is no pass-through for "authenticated nobody". Hearth's own cookies
// are then stripped from whatever is forwarded: the upstream is unreviewed
// branch code and has no business seeing them.
func (s *Server) PreviewProxyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := s.proxyBase()
		if base == "" {
			next.ServeHTTP(w, r)
			return
		}
		label, service, ok := kiln.ParsePreviewHost(r.Host, base)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		// Authorise before resolving: an unauthenticated caller must not be
		// able to probe which previews exist, and must not bump anyone's idle
		// clock either.
		if s.previewAuthGated() && !s.authorizePreviewRequest(w, r, label, base) {
			return
		}
		target, ok := s.resolvePreviewTarget(w, label, service)
		if !ok {
			return
		}
		ctx := context.WithValue(r.Context(), previewTargetKey{}, target)
		out := r.Clone(ctx)
		s.stripForgeAuthCookies(out)
		previewReverseProxy.ServeHTTP(w, out)
	})
}

// resolvePreviewTarget asks the daemon which loopback address serves this
// label, writing the 404 itself when nothing does. The daemon owns the Kiln
// registry, and resolving there is also what bumps the preview's idle clock —
// proxied traffic counts as activity, so a preview being actively browsed is
// never reaped out from under its reviewer.
func (s *Server) resolvePreviewTarget(w http.ResponseWriter, label, service string) (string, bool) {
	body, err := json.Marshal(ipc.PreviewResolvePayload{Label: label, Service: service})
	if err != nil {
		writePreviewProxyError(w, http.StatusInternalServerError, "failed to encode preview lookup")
		return "", false
	}
	resp := s.handler(ipc.Command{Type: "preview_resolve", Payload: body})
	if resp.Type != "ok" {
		writePreviewProxyError(w, http.StatusBadGateway, "preview lookup failed")
		return "", false
	}
	var out ipc.PreviewResolveResponse
	if len(resp.Payload) > 0 {
		if err := json.Unmarshal(resp.Payload, &out); err != nil {
			writePreviewProxyError(w, http.StatusBadGateway, "invalid preview lookup response")
			return "", false
		}
	}
	if !out.Found {
		writePreviewProxyError(w, http.StatusNotFound, previewMissMessage(label, service, out))
		return "", false
	}
	return net.JoinHostPort(out.Host, strconv.Itoa(out.Port)), true
}

// previewMissMessage renders the 404 body for a preview host that resolved to
// nothing. Each reason gets its own sentence: someone who opened a bookmark
// needs to know whether the preview was never there, has been torn down, or is
// still coming up, and "404" alone says none of that.
func previewMissMessage(label, service string, out ipc.PreviewResolveResponse) string {
	switch out.Reason {
	case ipc.PreviewResolveStopped:
		return fmt.Sprintf("preview %s is stopped", label)
	case ipc.PreviewResolveNoService:
		return fmt.Sprintf("preview %s has no service %q", label, service)
	case ipc.PreviewResolveNoPort:
		return fmt.Sprintf("preview %s has no port allocated yet", label)
	default:
		// no_preview, previews_disabled, or an unset reason: from the caller's
		// side these are one situation — nothing is serving this name.
		return fmt.Sprintf("no live preview for %s", label)
	}
}

// writePreviewProxyError answers with a plain-text body. Preview hosts serve
// somebody else's application, so an error from the proxy must not look like a
// JSON API response from it.
func writePreviewProxyError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg + "\n"))
}
