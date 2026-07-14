package web

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// maxSSEPerSession caps how many Server-Sent Events streams a single web
// session may hold open at once. Each live stream drives its own DB/file
// poller, so a session opening dozens of browser tabs (each with an activity,
// findings, worker-log and turn stream) would otherwise multiply that load
// without bound. Twenty is generous for legitimate multi-tab use while still
// bounding the worst case.
const maxSSEPerSession = 20

// sseCapped wraps an SSE handler with the per-session concurrent-connection
// cap. When the session already holds maxSSEPerSession streams the request is
// rejected with a friendly 429 before any streaming headers are written;
// otherwise the slot is held for the lifetime of next and released when it
// returns (client disconnect, shutdown, or normal completion).
func (s *Server) sseCapped(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		release, ok := s.acquireSSESlot(r)
		if !ok {
			writeError(w, http.StatusTooManyRequests,
				"too many open connections for this session; close some tabs")
			return
		}
		defer release()
		next(w, r)
	}
}

// acquireSSESlot reserves one SSE slot for the request's session. It returns a
// release func and true when a slot was granted, or (nil, false) when the
// session is already at maxSSEPerSession. Requests without a session (which
// should not happen under requireAuth) are not capped so an auth regression
// never silently drops legitimate streams.
func (s *Server) acquireSSESlot(r *http.Request) (func(), bool) {
	sess := SessionFromContext(r.Context())
	if sess == nil {
		return func() {}, true
	}
	key := sess.TokenHash

	s.sseMu.Lock()
	if s.sseConns[key] >= maxSSEPerSession {
		count := s.sseConns[key]
		s.sseMu.Unlock()
		s.logger.Warn("SSE connection cap reached",
			"user", sess.Username, "count", count, "cap", maxSSEPerSession)
		return nil, false
	}
	s.sseConns[key]++
	s.sseMu.Unlock()

	var released bool
	return func() {
		s.sseMu.Lock()
		defer s.sseMu.Unlock()
		if released {
			return
		}
		released = true
		s.sseConns[key]--
		if s.sseConns[key] <= 0 {
			// Drop the entry entirely so the map does not accumulate a key per
			// session that has ever streamed.
			delete(s.sseConns, key)
		}
	}, true
}

// ParseTrustedProxies parses a comma-separated list of IP addresses and CIDR
// ranges into networks suitable for Config.TrustedProxies. Bare IPs are
// widened to a single-host network (/32 for IPv4, /128 for IPv6). Blank
// entries are skipped; an empty or all-blank input yields a nil slice with no
// error. An unparseable entry returns an error so a typo in configuration is
// surfaced rather than silently trusting nothing.
func ParseTrustedProxies(raw string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			_, n, err := net.ParseCIDR(part)
			if err != nil {
				return nil, fmt.Errorf("web: invalid trusted proxy CIDR %q: %w", part, err)
			}
			nets = append(nets, n)
			continue
		}
		ip := net.ParseIP(part)
		if ip == nil {
			return nil, fmt.Errorf("web: invalid trusted proxy IP %q", part)
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return nets, nil
}
