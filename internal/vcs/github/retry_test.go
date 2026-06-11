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
