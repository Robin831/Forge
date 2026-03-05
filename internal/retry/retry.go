// Package retry implements exponential backoff retry logic for failed beads.
//
// When a Smith worker fails, retry determines whether the bead should be
// retried. After max retries are exhausted, the bead is marked as needs_human.
//
// Backoff schedule (default):
//
//	Attempt 1: 5 minutes
//	Attempt 2: 15 minutes
//	Attempt 3: 45 minutes (final, then needs_human)
package retry

import (
	"fmt"
	"math"
	"time"
)

const (
	// DefaultMaxRetries is the default number of retry attempts.
	DefaultMaxRetries = 2

	// DefaultBaseDelay is the base delay for the first retry.
	DefaultBaseDelay = 5 * time.Minute

	// DefaultMultiplier is the backoff multiplier between retries.
	DefaultMultiplier = 3.0
)

// Policy defines the retry behavior for failed beads.
type Policy struct {
	MaxRetries int           // Maximum number of retries (0 = no retries)
	BaseDelay  time.Duration // Delay before first retry
	Multiplier float64       // Multiplier for each subsequent retry
}

// DefaultPolicy returns the default retry policy.
func DefaultPolicy() Policy {
	return Policy{
		MaxRetries: DefaultMaxRetries,
		BaseDelay:  DefaultBaseDelay,
		Multiplier: DefaultMultiplier,
	}
}

// Decision represents the outcome of evaluating a retry.
type Decision struct {
	ShouldRetry bool          // Whether to retry
	Delay       time.Duration // How long to wait before retrying
	Attempt     int           // Which attempt this would be (1-based)
	NeedsHuman  bool          // If true, bead has exhausted retries
	Reason      string        // Human-readable explanation
}

// Evaluate decides whether to retry based on the current retry count.
func (p Policy) Evaluate(currentRetries int) Decision {
	if currentRetries >= p.MaxRetries {
		return Decision{
			ShouldRetry: false,
			NeedsHuman:  true,
			Attempt:     currentRetries + 1,
			Reason:      fmt.Sprintf("exhausted %d retries, needs human intervention", p.MaxRetries),
		}
	}

	delay := p.delayForAttempt(currentRetries)
	return Decision{
		ShouldRetry: true,
		Delay:       delay,
		Attempt:     currentRetries + 1,
		Reason:      fmt.Sprintf("retry %d/%d after %s", currentRetries+1, p.MaxRetries, delay),
	}
}

// delayForAttempt calculates the backoff delay for a given attempt number.
func (p Policy) delayForAttempt(attempt int) time.Duration {
	multiplier := math.Pow(p.Multiplier, float64(attempt))
	return time.Duration(float64(p.BaseDelay) * multiplier)
}

// RetryEntry tracks retry state for a specific bead.
type RetryEntry struct {
	BeadID       string    // The bead being retried
	Anvil        string    // Anvil the bead belongs to
	RetryCount   int       // Number of retries so far
	NextRetryAt  time.Time // When to retry next (zero = now or expired)
	NeedsHuman   bool      // If true, retries exhausted
	LastError    string    // The error from the last attempt
}

// IsReady returns true if the retry entry is due for its next attempt.
func (e RetryEntry) IsReady() bool {
	if e.NeedsHuman {
		return false
	}
	return time.Now().After(e.NextRetryAt)
}
