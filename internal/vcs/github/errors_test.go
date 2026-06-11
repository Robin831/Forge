package github

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

// timeoutError is a net.Error whose Timeout() reports true, used to exercise the
// typed network-error branch of the classifier.
type timeoutError struct{}

func (timeoutError) Error() string   { return "operation timed out" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// nonTimeoutNetError is a net.Error whose Timeout() reports false.
type nonTimeoutNetError struct{}

func (nonTimeoutNetError) Error() string   { return "some net problem" }
func (nonTimeoutNetError) Timeout() bool   { return false }
func (nonTimeoutNetError) Temporary() bool { return false }

func TestIsTransient(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		transient bool
	}{
		// --- Transient (criterion 1: retried) ---
		{"http 401", errors.New("gh pr create failed: HTTP 401: Bad credentials"), true},
		{"403 rate limit", errors.New("HTTP 403: API rate limit exceeded"), true},
		{"403 secondary rate limit", errors.New("HTTP 403: You have exceeded a secondary rate limit"), true},
		{"403 abuse", errors.New("HTTP 403: You have triggered an abuse detection mechanism"), true},
		{"500", errors.New("HTTP 500: Internal Server Error"), true},
		{"502", errors.New("gh api graphql: HTTP 502: Bad Gateway"), true},
		{"503", errors.New("status: 503 service unavailable"), true},
		{"dial error", errors.New("dial tcp 140.82.121.6:443: connect: connection refused"), true},
		{"i/o timeout string", errors.New("read tcp: i/o timeout"), true},
		{"connection reset", errors.New("read: connection reset by peer"), true},
		{"eof string", errors.New("unexpected EOF while reading"), true},
		{"io.EOF wrapped", fmt.Errorf("gh api graphql: %w", io.EOF), true},
		{"net.Error timeout", timeoutError{}, true},
		{"net.Error wrapped timeout", fmt.Errorf("request failed: %w", timeoutError{}), true},
		{"context deadline exceeded", errors.New("context deadline exceeded"), true},
		{"graphql requires authentication", errors.New("GraphQL: Requires authentication (repository.pullRequest)"), true},

		// --- Permanent (criterion 2: surfaces immediately) ---
		{"422 no commits between", errors.New("HTTP 422: No commits between main and feature"), false},
		{"422 pr already exists", errors.New("HTTP 422: A pull request already exists for branch"), false},
		{"already exists for branch", errors.New("gh pr create: pull request already exists for branch foo"), false},
		{"branch protection", errors.New("HTTP 422: Changes must be made through a pull request (protected branch)"), false},
		{"required status check", errors.New("HTTP 422: Required status check \"build\" is expected"), false},
		{"404", errors.New("HTTP 404: Not Found"), false},
		{"403 plain forbidden (no rate signal)", errors.New("HTTP 403: Resource not accessible by integration"), false},
		{"unknown error", errors.New("something unexpected happened"), false},
		{"non-timeout net error", nonTimeoutNetError{}, false},
		{"nil", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.transient, IsTransient(tt.err),
				"IsTransient mismatch for %v", tt.err)
		})
	}
}

func TestClassify(t *testing.T) {
	t.Run("transient error is wrapped", func(t *testing.T) {
		orig := errors.New("HTTP 403: secondary rate limit hit")
		got := Classify(orig)

		var te *TransientError
		assert.True(t, errors.As(got, &te), "expected *TransientError")
		assert.True(t, IsTransient(got))
		assert.ErrorIs(t, got, orig, "wrapped error must remain reachable via Unwrap")
		assert.Equal(t, orig.Error(), got.Error(), "Error() delegates to wrapped error")
	})

	t.Run("permanent error is returned unchanged", func(t *testing.T) {
		orig := errors.New("HTTP 422: No commits between main and feature")
		got := Classify(orig)

		var te *TransientError
		assert.False(t, errors.As(got, &te), "permanent error must not be wrapped")
		assert.Same(t, orig, got)
		assert.False(t, IsTransient(got))
	})

	t.Run("nil stays nil", func(t *testing.T) {
		assert.Nil(t, Classify(nil))
	})

	t.Run("already-classified transient is not double-wrapped", func(t *testing.T) {
		orig := errors.New("HTTP 500")
		once := Classify(orig)
		twice := Classify(once)
		assert.Same(t, once, twice, "Classify must be idempotent for transient errors")
	})
}

func TestShouldRetry(t *testing.T) {
	transient := errors.New("HTTP 503 service unavailable")
	permanent := errors.New("HTTP 404 not found")

	t.Run("transient retried while under the bound", func(t *testing.T) {
		for attempt := 0; attempt < MaxTransientAttempts; attempt++ {
			assert.True(t, ShouldRetry(transient, attempt),
				"attempt %d should be retried", attempt)
		}
	})

	t.Run("over-classification can't retry forever", func(t *testing.T) {
		// At or beyond the bound, even a transient error must stop retrying.
		assert.False(t, ShouldRetry(transient, MaxTransientAttempts))
		assert.False(t, ShouldRetry(transient, MaxTransientAttempts+10))
	})

	t.Run("negative attempt is not retried", func(t *testing.T) {
		assert.False(t, ShouldRetry(transient, -1))
	})

	t.Run("permanent never retried", func(t *testing.T) {
		assert.False(t, ShouldRetry(permanent, 0))
	})
}

func TestTransientErrorUnwrap(t *testing.T) {
	sentinel := errors.New("sentinel")
	te := &TransientError{err: fmt.Errorf("wrapped: %w", sentinel)}
	assert.ErrorIs(t, te, sentinel)
	assert.Equal(t, "wrapped: sentinel", te.Error())
}

// Ensure the concrete net.Error fixtures actually satisfy the interface so the
// classifier's errors.As branch is genuinely exercised.
var (
	_ net.Error = timeoutError{}
	_ net.Error = nonTimeoutNetError{}
)
