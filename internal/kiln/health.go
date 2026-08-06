package kiln

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

const (
	// DefaultHealthInterval is how often a starting service is probed.
	DefaultHealthInterval = 250 * time.Millisecond
	// probeTimeout bounds a single probe so one hung connection cannot eat the
	// whole ready timeout.
	probeTimeout = 5 * time.Second
)

// ErrServiceExited reports that the process died before it became healthy —
// the common case for a bad command line or a missing dependency. Failing on it
// immediately turns a "wait the full 120s, then say timeout" into an instant,
// accurate answer.
var ErrServiceExited = errors.New("service exited before becoming healthy")

// HealthCheck probes one preview service until it is ready.
//
// With a Path it does an HTTP GET and accepts any 2xx or 3xx response: a
// redirect to a login page still proves the server is up, which is all
// readiness means here. Without a Path it settles for the port being open,
// which is the only signal available for non-HTTP services.
type HealthCheck struct {
	// Host is the address to probe (the preview bind host).
	Host string
	// Port is the service's allocated port.
	Port int
	// Path is the HTTP health path (e.g. "/healthz"); empty means the
	// port-open check.
	Path string
	// Timeout is the manifest's ready_timeout for this service.
	Timeout time.Duration
	// Interval between probes; zero means DefaultHealthInterval.
	Interval time.Duration
	// Exited, when non-nil, is closed by the supervisor once the service's
	// process has died, so the check can stop waiting for a corpse.
	Exited <-chan struct{}
	// Client is the HTTP client used for path checks; zero uses a default with
	// redirect following left on.
	Client *http.Client
}

// Wait blocks until the service is healthy, the ready timeout expires, the
// process exits, or ctx is cancelled.
func (h HealthCheck) Wait(ctx context.Context) error {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = DefaultReadyTimeout
	}
	interval := h.Interval
	if interval <= 0 {
		interval = DefaultHealthInterval
	}

	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastErr error
	for {
		err := h.probe(ctx)
		if err == nil {
			return nil
		}
		lastErr = err

		select {
		case <-h.Exited:
			return fmt.Errorf("%w (last probe: %v)", ErrServiceExited, lastErr)
		case <-ctx.Done():
			// Distinguish "the service never came up" from "we stopped
			// asking", so a cancelled start does not read as a broken app.
			if err := parent.Err(); err != nil {
				return fmt.Errorf("health check abandoned: %w", err)
			}
			return fmt.Errorf("not healthy within %s: %w", timeout, lastErr)
		case <-ticker.C:
		}
	}
}

// probe runs a single readiness check.
func (h HealthCheck) probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	addr := net.JoinHostPort(h.probeHost(), strconv.Itoa(h.Port))
	if h.Path == "" {
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return conn.Close()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+h.Path, nil)
	if err != nil {
		return err
	}
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s returned %s", h.Path, resp.Status)
	}
	return nil
}

// probeHost turns a bind address into something dialable: a service bound to
// 0.0.0.0 (or ::) is reachable on loopback, and connecting to the wildcard
// address itself is not portable.
func (h HealthCheck) probeHost() string {
	switch h.Host {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	default:
		return h.Host
	}
}
