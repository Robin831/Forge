package web

import (
	"sync"
	"time"
)

// loginThrottle is an in-process rate limiter for the login endpoint. It
// tracks consecutive failed attempts per username and per client IP and
// returns a progressively longer delay once a threshold is crossed, slowing
// online password guessing without ever locking an account out.
//
// The delay schedule is exponential: no delay for the first `threshold`
// failures, then base, 2*base, 4*base, … capped at `maxDelay`. Counters decay to
// zero after `resetWindow` of inactivity and are cleared for a username on a
// successful login, so a legitimate user who mistypes a few times is not
// penalised for long.
//
// All methods are safe for concurrent use. The zero value is not usable —
// construct with newLoginThrottle.
type loginThrottle struct {
	mu    sync.Mutex
	users map[string]*throttleState
	ips   map[string]*throttleState

	threshold   int           // failures tolerated before delays begin
	base        time.Duration // first delay step
	maxDelay    time.Duration // delay cap
	resetWindow time.Duration // idle period after which a counter resets

	now func() time.Time
}

// throttleState is the failure bookkeeping for a single username or IP.
type throttleState struct {
	failures int
	last     time.Time
}

// Default throttle tuning. Login is rare and interactive, so a low threshold
// with a fast-growing delay is safe: five free attempts, then 1s, 2s, 4s …
// up to a minute.
const (
	defaultThrottleThreshold   = 5
	defaultThrottleBase        = time.Second
	defaultThrottleMax         = 60 * time.Second
	defaultThrottleResetWindow = 15 * time.Minute
)

// newLoginThrottle returns a throttle with the default tuning.
func newLoginThrottle() *loginThrottle {
	return &loginThrottle{
		users:       make(map[string]*throttleState),
		ips:         make(map[string]*throttleState),
		threshold:   defaultThrottleThreshold,
		base:        defaultThrottleBase,
		maxDelay:    defaultThrottleMax,
		resetWindow: defaultThrottleResetWindow,
		now:         time.Now,
	}
}

// delay returns how long the caller should wait before processing a login
// attempt for the given username/IP, based on the failures accumulated so
// far. It does not mutate failure counts (beyond decaying stale entries) so
// it is safe to call once per attempt. The larger of the per-username and
// per-IP delays wins.
func (t *loginThrottle) delay(username, ip string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	d := t.delayForLocked(t.users, username, now)
	if ipDelay := t.delayForLocked(t.ips, ip, now); ipDelay > d {
		d = ipDelay
	}
	return d
}

// delayForLocked computes the current delay for one bucket entry, resetting
// it first if it has been idle past the reset window. Caller holds t.mu.
func (t *loginThrottle) delayForLocked(bucket map[string]*throttleState, key string, now time.Time) time.Duration {
	if key == "" {
		return 0
	}
	st := bucket[key]
	if st == nil {
		return 0
	}
	if now.Sub(st.last) >= t.resetWindow {
		delete(bucket, key)
		return 0
	}
	return computeThrottleDelay(st.failures, t.threshold, t.base, t.maxDelay)
}

// recordFailure increments the failure counters for the username and IP.
func (t *loginThrottle) recordFailure(username, ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.bumpLocked(t.users, username, now)
	t.bumpLocked(t.ips, ip, now)
}

// bumpLocked increments a single bucket entry, resetting it first when it has
// gone idle past the reset window. Caller holds t.mu.
func (t *loginThrottle) bumpLocked(bucket map[string]*throttleState, key string, now time.Time) {
	if key == "" {
		return
	}
	st := bucket[key]
	if st == nil || now.Sub(st.last) >= t.resetWindow {
		st = &throttleState{}
		bucket[key] = st
	}
	st.failures++
	st.last = now
}

// recordSuccess clears the username's failure counter after a successful
// login. The per-IP counter is left to decay on its own so a shared proxy IP
// cannot be reset to zero by whoever happens to hold valid credentials.
func (t *loginThrottle) recordSuccess(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.users, username)
}

// purge drops bucket entries that have been idle past the reset window,
// keeping the maps from growing without bound. Called periodically by the
// server's purge loop.
func (t *loginThrottle) purge() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for _, bucket := range []map[string]*throttleState{t.users, t.ips} {
		for key, st := range bucket {
			if now.Sub(st.last) >= t.resetWindow {
				delete(bucket, key)
			}
		}
	}
}

// computeThrottleDelay returns the delay for a given failure count using an
// exponential schedule: 0 while below threshold, then base, 2*base, 4*base …
// capped at maxDelay. Overflow-safe.
func computeThrottleDelay(failures, threshold int, base, maxDelay time.Duration) time.Duration {
	if failures < threshold || base <= 0 {
		return 0
	}
	steps := failures - threshold
	// Guard the shift against overflow before it can wrap negative; anything
	// past ~30 doublings is already far beyond maxDelay.
	if steps >= 31 {
		return maxDelay
	}
	d := base << uint(steps)
	if d <= 0 || d > maxDelay {
		return maxDelay
	}
	return d
}
