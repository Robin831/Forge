package github

import (
	"context"
	"time"
)

// RetryBackoff configures the bounded exponential backoff used by
// RetryTransient. The retry count itself is bounded by the classifier's
// MaxTransientAttempts (via ShouldRetry), so this struct only controls the
// delay schedule. A zero BaseDelay disables sleeping entirely, which is
// convenient in tests.
type RetryBackoff struct {
	// BaseDelay is the delay before the first retry. Zero means no delay.
	BaseDelay time.Duration
	// Multiplier grows the delay between successive retries. Values <= 1 keep
	// the delay constant at BaseDelay.
	Multiplier float64
}

// DefaultRetryBackoff returns the production backoff schedule: a one-second
// initial delay doubling on each retry. Paired with MaxTransientAttempts (4)
// this yields delays of roughly 1s, 2s, 4s, 8s before the bead/PR falls through
// to its stranded/needs_human path.
func DefaultRetryBackoff() RetryBackoff {
	return RetryBackoff{
		BaseDelay:  1 * time.Second,
		Multiplier: 2.0,
	}
}

// RetryLogFunc is an optional callback invoked before each retry sleep so
// consumers can log the attempt in their own logging style. attempt is the
// 1-based number of the retry about to be performed; delay is how long the
// helper will wait before it; err is the transient error that triggered it.
type RetryLogFunc func(attempt int, delay time.Duration, err error)

// RetryTransient calls fn, retrying only while the returned error is classified
// transient (IsTransient) and the classifier's bound (MaxTransientAttempts) has
// not been reached. Permanent errors return immediately so the caller can fall
// through to its stranded/needs_human path without delay; the final transient
// error after retries are exhausted is likewise returned unchanged so the
// caller still reaches that fallthrough. ctx cancellation is honoured between
// attempts and short-circuits the wait.
//
// This is the single retry primitive shared by the end-of-pipeline CreatePR
// (internal/daemon) and the Bellows gh-calling paths so the transient/permanent
// decision lives in exactly one place (the vcs/github classifier).
func RetryTransient(ctx context.Context, b RetryBackoff, logFn RetryLogFunc, fn func() error) error {
	var err error
	delay := b.BaseDelay
	// retries is the zero-based count of retries already performed, matching the
	// contract of ShouldRetry (pass 0 before the first retry).
	for retries := 0; ; retries++ {
		err = fn()
		if err == nil {
			return nil
		}
		// ShouldRetry returns false for permanent errors AND once the transient
		// retry budget is exhausted; both cases surface the error to the caller.
		if !ShouldRetry(err, retries) {
			return err
		}
		if logFn != nil {
			logFn(retries+1, delay, err)
		}
		if delay > 0 {
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}
		if b.Multiplier > 1 {
			delay = time.Duration(float64(delay) * b.Multiplier)
		}
	}
}
