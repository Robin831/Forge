package wicket

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- isRateLimitStderr ------------------------------------------------------

func TestIsRateLimitStderr(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		want   bool
	}{
		{name: "rate limit exceeded", stderr: "HTTP 403: API rate limit exceeded for user", want: true},
		{name: "secondary rate limit", stderr: "You have exceeded a secondary rate limit", want: true},
		{name: "api rate limit lowercase", stderr: "api rate limit hit", want: true},
		{name: "x-ratelimit-remaining zero", stderr: "X-RateLimit-Remaining: 0", want: true},
		{name: "case insensitive", stderr: "API Rate Limit Exceeded", want: true},
		{name: "other 403", stderr: "HTTP 403: Resource not accessible by integration", want: false},
		{name: "network error", stderr: "connection refused", want: false},
		{name: "empty", stderr: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isRateLimitStderr(tc.stderr))
		})
	}
}

// ---- RateLimitError ---------------------------------------------------------

func TestRateLimitError_Error_NoResetTime(t *testing.T) {
	err := &RateLimitError{Message: "limit exceeded", Remaining: -1}
	assert.Contains(t, err.Error(), "rate limit exceeded")
	assert.Contains(t, err.Error(), "limit exceeded")
}

func TestRateLimitError_Error_WithResetTime(t *testing.T) {
	reset := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	err := &RateLimitError{Message: "limit exceeded", Remaining: 0, ResetAt: reset}
	assert.Contains(t, err.Error(), "resets at")
}

// ---- parseResetTimeFromStderr -----------------------------------------------

func TestParseResetTimeFromStderr(t *testing.T) {
	ts := "2026-06-01T00:00:00Z"
	parsed, _ := time.Parse(time.RFC3339, ts)

	tests := []struct {
		name   string
		stderr string
		want   time.Time
	}{
		{name: "timestamp in message", stderr: "rate limit exceeded, retry after " + ts, want: parsed},
		{name: "timestamp with trailing comma", stderr: "rate limit exceeded " + ts + ", please wait", want: parsed},
		{name: "no timestamp", stderr: "rate limit exceeded", want: time.Time{}},
		{name: "empty", stderr: "", want: time.Time{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseResetTimeFromStderr(tc.stderr)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ---- isRateLimitErr ---------------------------------------------------------

func TestIsRateLimitErr_True(t *testing.T) {
	rlErr := &RateLimitError{Message: "limit exceeded"}
	assert.True(t, isRateLimitErr(rlErr, nil))

	// Wrapped via fmt.Errorf %w chain.
	wrapped := fmt.Errorf("outer: %w", rlErr)
	assert.True(t, isRateLimitErr(wrapped, nil))
}

func TestIsRateLimitErr_False(t *testing.T) {
	assert.False(t, isRateLimitErr(errors.New("plain error"), nil))
}

func TestIsRateLimitErr_PopulatesTarget(t *testing.T) {
	rlErr := &RateLimitError{Message: "hit", Remaining: 5}
	var out *RateLimitError
	assert.True(t, isRateLimitErr(rlErr, &out))
	require.NotNil(t, out)
	assert.Equal(t, 5, out.Remaining)
}

// ---- rateLimiter ------------------------------------------------------------

func TestRateLimiter_InitialState(t *testing.T) {
	rl := newRateLimiter()
	assert.False(t, rl.IsLimited())
	assert.False(t, rl.IsLowQuota())
	assert.Equal(t, -1, rl.Remaining())
	assert.True(t, rl.BackoffUntil().IsZero())
}

func TestRateLimiter_UpdateRemaining_LowQuota(t *testing.T) {
	rl := newRateLimiter()
	rl.UpdateRemaining(50, time.Time{})
	assert.True(t, rl.IsLowQuota())
	assert.Equal(t, 50, rl.Remaining())
}

func TestRateLimiter_UpdateRemaining_AboveThreshold(t *testing.T) {
	rl := newRateLimiter()
	rl.UpdateRemaining(500, time.Time{})
	assert.False(t, rl.IsLowQuota())
}

func TestRateLimiter_UpdateRemaining_AtThreshold(t *testing.T) {
	rl := newRateLimiter()
	// Exactly at threshold — 100 is not low (< 100 is low).
	rl.UpdateRemaining(100, time.Time{})
	assert.False(t, rl.IsLowQuota())

	rl.UpdateRemaining(99, time.Time{})
	assert.True(t, rl.IsLowQuota())
}

func TestRateLimiter_RecordRateLimitHit_SetsLimited(t *testing.T) {
	rl := newRateLimiter()
	delay := rl.RecordRateLimitHit()
	assert.Equal(t, rateLimitMinBackoff, delay)
	assert.True(t, rl.IsLimited())
	assert.Equal(t, 0, rl.Remaining())
}

func TestRateLimiter_RecordRateLimitHit_ExponentialBackoff(t *testing.T) {
	rl := newRateLimiter()

	delay1 := rl.RecordRateLimitHit()
	assert.Equal(t, 1*time.Minute, delay1, "first hit should use base delay")

	delay2 := rl.RecordRateLimitHit()
	assert.Equal(t, 2*time.Minute, delay2, "second hit should double")

	delay3 := rl.RecordRateLimitHit()
	assert.Equal(t, 4*time.Minute, delay3, "third hit should double again")
}

func TestRateLimiter_RecordRateLimitHit_CapsAtMax(t *testing.T) {
	rl := newRateLimiter()
	var lastDelay time.Duration
	for range 10 {
		lastDelay = rl.RecordRateLimitHit()
	}
	assert.LessOrEqual(t, lastDelay, rateLimitMaxBackoff)
}

func TestRateLimiter_RecordRateLimitHit_HonoursResetTime(t *testing.T) {
	rl := newRateLimiter()
	// Set a reset time 30 minutes in the future (longer than the first backoff).
	future := time.Now().Add(30 * time.Minute)
	rl.UpdateRemaining(0, future)

	delay := rl.RecordRateLimitHit()
	assert.GreaterOrEqual(t, delay, 30*time.Minute)
}

func TestRateLimiter_RecordSuccess_ClearsBackoff(t *testing.T) {
	rl := newRateLimiter()
	rl.RecordRateLimitHit()
	require.True(t, rl.IsLimited())

	rl.RecordSuccess()
	assert.False(t, rl.IsLimited())
	assert.Equal(t, 0, rl.consecutiveFails)
}

func TestRateLimiter_RecordSuccess_PreservesRemaining(t *testing.T) {
	rl := newRateLimiter()
	rl.UpdateRemaining(50, time.Time{})
	require.True(t, rl.IsLowQuota())

	// RecordSuccess must not wipe the known quota so IsLowQuota stays accurate.
	rl.RecordSuccess()
	assert.True(t, rl.IsLowQuota(), "known low-quota state must survive RecordSuccess")
	assert.Equal(t, 50, rl.Remaining())
}

func TestRateLimiter_BackoffUntil_NonZeroAfterHit(t *testing.T) {
	rl := newRateLimiter()
	before := time.Now()
	rl.RecordRateLimitHit()
	assert.True(t, rl.BackoffUntil().After(before))
}

// ---- Monitor integration: rate limit detection in scanRepo ------------------

func TestScanRepo_RateLimitError_TriggersBackoff(t *testing.T) {
	m, mock, _ := newTestMonitor(t)
	settings := defaultSettings()

	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		return nil, &RateLimitError{Message: "API rate limit exceeded", Remaining: -1}
	}

	require.False(t, m.rl.IsLimited(), "should not be limited before scan")

	rateLimited := m.scanRepo(context.Background(), "anvil", "org/repo", config.AnvilConfig{}, settings)
	assert.True(t, rateLimited, "scanRepo should report rate limited")
	assert.True(t, m.rl.IsLimited(), "rate limiter should be in backoff after rate limit error")
}

func TestScanRepo_NonRateLimitError_NoBackoff(t *testing.T) {
	m, mock, _ := newTestMonitor(t)
	settings := defaultSettings()

	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		return nil, errors.New("network timeout")
	}

	rateLimited := m.scanRepo(context.Background(), "anvil", "org/repo", config.AnvilConfig{}, settings)
	assert.False(t, rateLimited, "non-rate-limit error should not report rate limited")
	assert.False(t, m.rl.IsLimited(), "rate limiter should not be in backoff for non-rate-limit error")
}

func TestScanRepo_Success_DoesNotSetLimited(t *testing.T) {
	m, mock, _ := newTestMonitor(t)
	settings := defaultSettings()

	mock.OnListIssues = func(_ context.Context, _ string, _ []string) ([]Issue, error) {
		return []Issue{}, nil
	}

	rateLimited := m.scanRepo(context.Background(), "anvil", "org/repo", config.AnvilConfig{}, settings)
	assert.False(t, rateLimited)
	assert.False(t, m.rl.IsLimited())
}

// ---- effectiveInterval ------------------------------------------------------

func TestEffectiveInterval_Normal(t *testing.T) {
	m, _, _ := newTestMonitor(t)
	base := 15 * time.Minute
	assert.Equal(t, base, m.effectiveInterval(base))
}

func TestEffectiveInterval_LowQuota_Doubles(t *testing.T) {
	m, _, _ := newTestMonitor(t)
	m.rl.UpdateRemaining(50, time.Time{})
	base := 15 * time.Minute
	assert.Equal(t, 30*time.Minute, m.effectiveInterval(base))
}

func TestEffectiveInterval_ActiveBackoff_ReturnsWaitTime(t *testing.T) {
	m, _, _ := newTestMonitor(t)
	m.rl.RecordRateLimitHit()
	base := 15 * time.Minute
	interval := m.effectiveInterval(base)
	// Interval should be close to rateLimitMinBackoff (1 minute), not the base 15m.
	assert.LessOrEqual(t, interval, rateLimitMinBackoff+5*time.Second)
	assert.Greater(t, interval, time.Duration(0))
}
