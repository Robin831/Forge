package web

import (
	"testing"
	"time"
)

func TestComputeThrottleDelay_Schedule(t *testing.T) {
	base := time.Second
	max := 60 * time.Second
	threshold := 5
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{4, 0}, // still below threshold — free attempts
		{5, 1 * time.Second},
		{6, 2 * time.Second},
		{7, 4 * time.Second},
		{8, 8 * time.Second},
		{9, 16 * time.Second},
		{10, 32 * time.Second},
		{11, 60 * time.Second},   // 64s would exceed cap
		{20, 60 * time.Second},   // saturated
		{1000, 60 * time.Second}, // overflow-safe
	}
	for _, c := range cases {
		if got := computeThrottleDelay(c.failures, threshold, base, max); got != c.want {
			t.Errorf("computeThrottleDelay(%d) = %v, want %v", c.failures, got, c.want)
		}
	}
}

func TestLoginThrottle_PerUsernameProgression(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	th := newLoginThrottle()
	th.now = func() time.Time { return now }

	// The first `threshold` failures incur no delay.
	for i := 0; i < th.threshold; i++ {
		if d := th.delay("alice", "1.1.1.1"); d != 0 {
			t.Fatalf("attempt %d: expected 0 delay, got %v", i, d)
		}
		th.recordFailure("alice", "1.1.1.1")
	}
	// The next attempt is delayed by base, then doubles.
	if d := th.delay("alice", "1.1.1.1"); d != th.base {
		t.Fatalf("post-threshold delay = %v, want %v", d, th.base)
	}
	th.recordFailure("alice", "1.1.1.1")
	if d := th.delay("alice", "1.1.1.1"); d != 2*th.base {
		t.Fatalf("next delay = %v, want %v", d, 2*th.base)
	}
}

func TestLoginThrottle_SuccessResetsUsername(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	th := newLoginThrottle()
	th.now = func() time.Time { return now }

	for i := 0; i < th.threshold+2; i++ {
		th.recordFailure("bob", "2.2.2.2")
	}
	if d := th.delay("bob", "9.9.9.9"); d == 0 {
		t.Fatalf("expected a non-zero delay before success reset")
	}
	th.recordSuccess("bob")
	// After a successful login the username counter is cleared. Query from a
	// fresh IP so the per-IP bucket (deliberately not reset) does not mask it.
	if d := th.delay("bob", "9.9.9.9"); d != 0 {
		t.Fatalf("expected 0 delay after success reset, got %v", d)
	}
}

func TestLoginThrottle_PerIPBucket(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	th := newLoginThrottle()
	th.now = func() time.Time { return now }

	// Different usernames from the same IP still accumulate on the IP bucket.
	for i := 0; i < th.threshold+1; i++ {
		th.recordFailure("user-a", "5.5.5.5")
		now = now.Add(time.Second)
	}
	// A brand-new username from that IP is still throttled by the IP bucket.
	if d := th.delay("fresh-user", "5.5.5.5"); d == 0 {
		t.Fatalf("expected per-IP throttle to apply to a new username")
	}
}

func TestLoginThrottle_ResetWindow(t *testing.T) {
	now := time.Unix(4_000_000, 0)
	th := newLoginThrottle()
	th.now = func() time.Time { return now }

	for i := 0; i < th.threshold+3; i++ {
		th.recordFailure("carol", "7.7.7.7")
	}
	if d := th.delay("carol", "7.7.7.7"); d == 0 {
		t.Fatalf("expected a delay before the reset window elapses")
	}
	// Advance past the reset window: counters decay to zero.
	now = now.Add(th.resetWindow + time.Second)
	if d := th.delay("carol", "7.7.7.7"); d != 0 {
		t.Fatalf("expected 0 delay after reset window, got %v", d)
	}
}

func TestLoginThrottle_Purge(t *testing.T) {
	now := time.Unix(5_000_000, 0)
	th := newLoginThrottle()
	th.now = func() time.Time { return now }

	th.recordFailure("dave", "8.8.8.8")
	now = now.Add(th.resetWindow + time.Minute)
	th.purge()

	th.mu.Lock()
	defer th.mu.Unlock()
	if len(th.users) != 0 || len(th.ips) != 0 {
		t.Fatalf("expected purge to clear stale entries, got users=%d ips=%d", len(th.users), len(th.ips))
	}
}
