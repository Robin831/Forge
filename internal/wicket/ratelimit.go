package wicket

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// rateLimitLowQuotaThreshold is the remaining-request count below which
	// the poll interval is temporarily doubled to reduce API pressure.
	rateLimitLowQuotaThreshold = 100

	// rateLimitMinBackoff is the initial backoff delay on a rate-limit hit.
	rateLimitMinBackoff = 1 * time.Minute

	// rateLimitMaxBackoff caps exponential growth to avoid very long waits.
	rateLimitMaxBackoff = 60 * time.Minute

)

// RateLimitError is returned by GitHub client calls when the GitHub API rate
// limit has been exceeded (HTTP 403 or a secondary rate-limit signal).
type RateLimitError struct {
	// Message is the raw error text from the gh CLI stderr.
	Message string
	// Remaining is the X-RateLimit-Remaining value if available, -1 if unknown.
	Remaining int
	// ResetAt is when the rate-limit window resets, zero if unknown.
	ResetAt time.Time
}

func (e *RateLimitError) Error() string {
	if e.ResetAt.IsZero() {
		return fmt.Sprintf("GitHub API rate limit exceeded: %s", e.Message)
	}
	return fmt.Sprintf("GitHub API rate limit exceeded: %s (resets at %s)",
		e.Message, e.ResetAt.Format(time.RFC3339))
}

// rateLimiter tracks GitHub API quota usage and manages exponential backoff
// when the rate limit is hit. All methods are safe for concurrent use.
type rateLimiter struct {
	mu               sync.Mutex
	remaining        int       // last known remaining requests (-1 = unknown)
	resetAt          time.Time // when the current rate-limit window resets
	backoffUntil     time.Time // do not make requests before this time
	consecutiveFails int       // consecutive rate-limit hits used for backoff
}

// newRateLimiter returns a rateLimiter with no known quota state.
func newRateLimiter() *rateLimiter {
	return &rateLimiter{remaining: -1}
}

// IsLimited reports whether the rate limiter is currently in a backoff period.
func (rl *rateLimiter) IsLimited() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return !rl.backoffUntil.IsZero() && time.Now().Before(rl.backoffUntil)
}

// IsLowQuota reports whether the remaining quota is known and below
// rateLimitLowQuotaThreshold.
func (rl *rateLimiter) IsLowQuota() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.remaining >= 0 && rl.remaining < rateLimitLowQuotaThreshold
}

// Remaining returns the last known remaining API quota (-1 if unknown).
func (rl *rateLimiter) Remaining() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.remaining
}

// BackoffUntil returns the time until which requests should be paused (zero
// value means no active backoff).
func (rl *rateLimiter) BackoffUntil() time.Time {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.backoffUntil
}

// UpdateRemaining records an updated remaining-quota value observed from a
// GitHub API response header along with an optional reset time.
func (rl *rateLimiter) UpdateRemaining(remaining int, resetAt time.Time) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.remaining = remaining
	if !resetAt.IsZero() {
		rl.resetAt = resetAt
	}
}

// RecordRateLimitHit records a rate-limit error and computes the next
// exponential backoff delay. Returns the chosen backoff duration.
//
// When the GitHub reset time is known and greater than the computed delay, the
// reset time plus a 5-second buffer is used instead so we avoid retrying while
// the quota window is still exhausted.
//
// Integer duration doubling is used instead of math.Pow to avoid float64→int
// overflow that can occur when consecutiveFails is large.
func (rl *rateLimiter) RecordRateLimitHit() time.Duration {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.consecutiveFails++
	rl.remaining = 0

	// Double the delay once per previous failure, capping at the maximum.
	// We cap consecutiveFails at the point where the delay reaches the max so
	// that the counter does not grow unboundedly in long-running processes.
	// Integer arithmetic is used to avoid float64→int64 overflow that can
	// occur with math.Pow when consecutiveFails is large.
	delay := rateLimitMinBackoff
	for i := 1; i < rl.consecutiveFails; i++ {
		delay *= 2 // rateLimitBackoffMultiplier is 2
		if delay >= rateLimitMaxBackoff {
			delay = rateLimitMaxBackoff
			rl.consecutiveFails = i + 1
			break
		}
	}

	// Honour the API reset time when it would make us wait longer.
	if !rl.resetAt.IsZero() {
		if resetWait := time.Until(rl.resetAt) + 5*time.Second; resetWait > delay {
			delay = resetWait
		}
	}

	rl.backoffUntil = time.Now().Add(delay)
	return delay
}

// RecordSuccess clears the backoff state after a successful API call.
// Known quota values (remaining/resetAt) are preserved so that IsLowQuota()
// continues to reflect the last-known quota state; callers should update quota
// via UpdateRemaining when fresh header values are available.
func (rl *rateLimiter) RecordSuccess() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.consecutiveFails = 0
	rl.backoffUntil = time.Time{}
}

// isRateLimitErr reports whether err is a *RateLimitError. When target is
// non-nil it is populated with the concrete *RateLimitError value via
// errors.As, allowing the caller to inspect fields such as Remaining.
func isRateLimitErr(err error, target **RateLimitError) bool {
	if target != nil {
		return errors.As(err, target)
	}
	var rl *RateLimitError
	return errors.As(err, &rl)
}
