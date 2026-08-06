package kiln

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// hostPort splits a "host:port" address into its parts.
func hostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("splitting %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing port of %q: %v", addr, err)
	}
	return host, port
}

func TestHealthCheckPathBecomesHealthy(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Not ready for the first two probes — the usual "server is up but
		// still migrating" shape.
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	host, port := hostPort(t, u.Host)

	check := HealthCheck{Host: host, Port: port, Path: "/healthz", Timeout: 5 * time.Second, Interval: 10 * time.Millisecond}
	if err := check.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if calls.Load() < 3 {
		t.Errorf("expected the check to retry, got %d probes", calls.Load())
	}
}

func TestHealthCheckAcceptsRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A login redirect still proves the server is up.
		w.Header().Set("Location", "http://example.invalid/login")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	host, port := hostPort(t, u.Host)

	check := HealthCheck{
		Host: host, Port: port, Path: "/", Timeout: 2 * time.Second, Interval: 10 * time.Millisecond,
		// Do not chase the redirect off-box; the status code is the signal.
		Client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
	if err := check.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestHealthCheckPortOpen(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	host, port := hostPort(t, ln.Addr().String())

	// No health path: an open port is the readiness signal, and the raw
	// listener never speaks HTTP.
	check := HealthCheck{Host: host, Port: port, Timeout: 2 * time.Second, Interval: 10 * time.Millisecond}
	if err := check.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestHealthCheckTimesOut(t *testing.T) {
	// A port nobody listens on: reserve one, then release it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port := hostPort(t, ln.Addr().String())
	ln.Close()

	check := HealthCheck{Host: "127.0.0.1", Port: port, Timeout: 150 * time.Millisecond, Interval: 10 * time.Millisecond}
	start := time.Now()
	err = check.Wait(context.Background())
	if err == nil {
		t.Fatal("Wait on a dead port returned nil")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Wait took %s, expected it to stop at the ready timeout", elapsed)
	}
}

func TestHealthCheckStopsWhenProcessExits(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port := hostPort(t, ln.Addr().String())
	ln.Close()

	exited := make(chan struct{})
	close(exited)

	check := HealthCheck{
		Host: "127.0.0.1", Port: port,
		// A long ready timeout that must NOT be waited out: the process is
		// already gone, so there is nothing left to become healthy.
		Timeout: time.Minute, Interval: 10 * time.Millisecond,
		Exited: exited,
	}
	start := time.Now()
	err = check.Wait(context.Background())
	if !errors.Is(err, ErrServiceExited) {
		t.Fatalf("Wait error = %v, want ErrServiceExited", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Wait took %s; it should give up as soon as the process exits", elapsed)
	}
}

func TestHealthCheckHonoursContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	check := HealthCheck{Host: "127.0.0.1", Port: 1, Timeout: time.Minute, Interval: 10 * time.Millisecond}
	if err := check.Wait(ctx); err == nil {
		t.Fatal("Wait with a cancelled context returned nil")
	}
}

func TestHealthCheckProbeHostMapsWildcard(t *testing.T) {
	for _, host := range []string{"", "0.0.0.0", "::", "[::]"} {
		if got := (HealthCheck{Host: host}).probeHost(); got != "127.0.0.1" {
			t.Errorf("probeHost(%q) = %q, want 127.0.0.1", host, got)
		}
	}
	if got := (HealthCheck{Host: "192.168.1.5"}).probeHost(); got != "192.168.1.5" {
		t.Errorf("probeHost kept host = %q", got)
	}
}
