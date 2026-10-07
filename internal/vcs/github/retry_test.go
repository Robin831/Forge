package github

import (
	"context"
	"errors"
	"testing"
	"time"
)

// noDelay is a zero-delay backoff so tests never sleep.
var noDelay = RetryBackoff{}

func TestRetryTransient_TransientThenSuccess(t *testing.T) {
	calls := 0
	err := RetryTransient(context.Background(), noDelay, nil, func() error {
		calls++
		if calls == 1 {
			return errors.New("HTTP 401: Bad credentials")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success after one transient failure, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls (1 fail + 1 success), got %d", calls)
	}
}

func TestRetryTransient_PermanentNoRetry(t *testing.T) {
	calls := 0
	perm := errors.New("HTTP 422: Validation Failed (No commits between main and feature)")
	err := RetryTransient(context.Background(), noDelay, nil, func() error {
		calls++
		return perm
	})
	if !errors.Is(err, perm) {
		t.Fatalf("expected the permanent error to surface unchanged, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("permanent error must not be retried; expected 1 call, got %d", calls)
	}
}

func TestRetryTransient_ExhaustsThenSurfaces(t *testing.T) {
	calls := 0
	transient := errors.New("HTTP 503: Service Unavailable")
	err := RetryTransient(context.Background(), noDelay, nil, func() error {
		calls++
		return transient
	})
	if !errors.Is(err, transient) {
		t.Fatalf("expected the final transient error to surface, got %v", err)
	}
	// 1 initial attempt + MaxTransientAttempts retries.
	if want := 1 + MaxTransientAttempts; calls != want {
		t.Fatalf("expected %d calls (initial + %d retries), got %d", want, MaxTransientAttempts, calls)
	}
}

func TestRetryTransient_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	// A non-zero delay so the cancellation path is exercised in the wait.
	err := RetryTransient(ctx, RetryBackoff{BaseDelay: time.Hour, Multiplier: 2}, nil, func() error {
		calls++
		return errors.New("HTTP 500: Internal Server Error")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call before cancellation, got %d", calls)
	}
}

func TestRetryTransient_ContextCancelledZeroDelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := RetryTransient(ctx, noDelay, nil, func() error {
		calls++
		if calls == 1 {
			cancel()
		}
		return errors.New("HTTP 500: Internal Server Error")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled with zero-delay backoff, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call before cancellation with zero-delay, got %d", calls)
	}
}

// recordingSleep is an injectable Sleep that records each wait instead of
// sleeping.
func recordingSleep(waits *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}
}

// A secondary-limit refusal is retried once, after the server's Retry-After —
// not replayed four times on the 1s/2s/4s schedule.
func TestRetryTransient_RateLimitHonoursRetryAfterOnce(t *testing.T) {
	var waits []time.Duration
	b := DefaultRetryBackoff()
	b.Sleep = recordingSleep(&waits)
	calls := 0
	refusal := &RateLimitError{Err: errors.New("HTTP 403: secondary rate limit"), Secondary: true, RetryAfter: 20 * time.Second}
	err := RetryTransient(context.Background(), b, nil, func() error {
		calls++
		return refusal
	})
	if !errors.Is(err, refusal) {
		t.Fatalf("want the refusal back, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("want 1 call + %d rate-limit retry, got %d calls", MaxRateLimitRetries, calls)
	}
	if len(waits) != 1 || waits[0] != 20*time.Second {
		t.Fatalf("want one wait of the Retry-After (20s), got %v", waits)
	}
}

func TestRetryTransient_RateLimitSucceedsAfterWait(t *testing.T) {
	var waits []time.Duration
	b := DefaultRetryBackoff()
	b.Sleep = recordingSleep(&waits)
	calls := 0
	err := RetryTransient(context.Background(), b, nil, func() error {
		calls++
		if calls == 1 {
			return errors.New("gh: HTTP 403: You have exceeded a secondary rate limit")
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("want success on the retry, got err=%v calls=%d", err, calls)
	}
	if len(waits) != 1 || waits[0] != DefaultRateLimitWait {
		t.Fatalf("no header: want the %s default wait, got %v", DefaultRateLimitWait, waits)
	}
}

// A hint above the cap — or any hint when the cap is zero — is not waited out
// in-line: the refusal goes straight back for the caller to back off.
func TestRetryTransient_RateLimitOverCapNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  time.Duration
		hint time.Duration
	}{
		{"hint above cap", time.Minute, 10 * time.Minute},
		{"zero cap", 0, time.Second},
		{"zero-value backoff", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var waits []time.Duration
			b := RetryBackoff{MaxRateLimitWait: tc.cap, Sleep: recordingSleep(&waits)}
			calls := 0
			err := RetryTransient(context.Background(), b, nil, func() error {
				calls++
				return &RateLimitError{Err: errors.New("HTTP 429"), RetryAfter: tc.hint}
			})
			if _, ok := AsRateLimit(err); !ok || calls != 1 || len(waits) != 0 {
				t.Fatalf("want 1 call, no wait, the refusal back; got calls=%d waits=%v err=%v", calls, waits, err)
			}
		})
	}
}

// Other transient errors keep the bounded exponential schedule.
func TestRetryTransient_NonRateLimitScheduleUnchanged(t *testing.T) {
	var waits []time.Duration
	b := DefaultRetryBackoff()
	b.Sleep = recordingSleep(&waits)
	calls := 0
	_ = RetryTransient(context.Background(), b, nil, func() error {
		calls++
		return errors.New("HTTP 503: Service Unavailable")
	})
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if calls != 1+MaxTransientAttempts || !equalDurations(waits, want) {
		t.Fatalf("want %d calls with waits %v, got %d with %v", 1+MaxTransientAttempts, want, calls, waits)
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAsRateLimit_FromGhText(t *testing.T) {
	for _, tc := range []struct {
		msg           string
		want          bool
		wantSecondary bool
	}{
		{"gh: You have exceeded a secondary rate limit. (HTTP 403)", true, true},
		{"HTTP 403: API rate limit exceeded for user ID 1", true, false},
		{"HTTP 429: Too Many Requests", true, true},
		{"GraphQL: API rate limit exceeded for user ID 1.", true, false},
		{"HTTP 403: Resource not accessible by integration", false, false},
		{"HTTP 502: Bad Gateway", false, false},
	} {
		rl, ok := AsRateLimit(errors.New(tc.msg))
		if ok != tc.want || (ok && rl.Secondary != tc.wantSecondary) {
			t.Errorf("%q: got ok=%v rl=%+v, want ok=%v secondary=%v", tc.msg, ok, rl, tc.want, tc.wantSecondary)
		}
		if ok && rl.RetryAfter != 0 {
			t.Errorf("%q: gh text carries no header, RetryAfter must be 0", tc.msg)
		}
	}
	if !IsTransient(errors.New("HTTP 429: Too Many Requests")) {
		t.Errorf("429 must be transient")
	}
}
