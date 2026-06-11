package github

import (
	"errors"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// MaxTransientAttempts bounds how many times a transient error may be retried.
// It is the load-bearing safety net so that over-classification (an error
// mistakenly treated as transient) can never retry forever. Consumers such as
// the CreatePR-retry and Bellows sub-tasks pair IsTransient with this bound via
// ShouldRetry.
const MaxTransientAttempts = 4

// TransientError wraps an error that classification has judged to be transient
// — i.e. likely to succeed on retry (rate limits, transient auth, 5xx, network
// blips). Callers can detect it with errors.As or, more conveniently, with
// IsTransient. The original error remains reachable via Unwrap.
type TransientError struct {
	err error
}

// Error implements the error interface, delegating to the wrapped error.
func (e *TransientError) Error() string {
	if e == nil || e.err == nil {
		return "transient error"
	}
	return e.err.Error()
}

// Unwrap returns the wrapped error so errors.Is/errors.As keep working through
// the transient marker.
func (e *TransientError) Unwrap() error { return e.err }

// Classify inspects err and, when it is judged transient, returns it wrapped in
// a *TransientError so callers can fail-fast on permanent errors and retry on
// transient ones. Permanent (and nil) errors are returned unchanged.
//
// This is the single shared classification point for the vcs/github package:
// both the CreatePR-retry loop and the Bellows PR monitor route gh/GitHub
// errors through it instead of re-implementing ad-hoc string matching.
func Classify(err error) error {
	if err == nil {
		return nil
	}
	if isTransient(err) {
		// Avoid double-wrapping if it's already classified transient.
		var t *TransientError
		if errors.As(err, &t) {
			return err
		}
		return &TransientError{err: err}
	}
	return err
}

// IsTransient reports whether err is a transient GitHub/gh failure that is
// worth retrying. It is the load-bearing predicate consumed by the
// CreatePR-retry and Bellows sub-tasks.
//
// Transient classes:
//   - HTTP 401 (transient auth — token momentarily not accepted)
//   - HTTP 403 carrying rate-limit / secondary-rate-limit / abuse signals
//   - any HTTP 5xx
//   - network failures: dial/timeout/EOF/connection-reset/i-o-timeout, plus
//     net.Error timeouts and io.EOF
//   - the literal GraphQL "Requires authentication" error
//
// Everything else — including unrecognised errors — is treated as PERMANENT so
// that misclassification can never cause an unbounded retry loop. Permanent
// classes explicitly include HTTP 422 validation errors ("No commits between",
// "a pull request already exists"), branch-protection refusals, and HTTP 404.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	// An already-classified transient error stays transient.
	var t *TransientError
	if errors.As(err, &t) {
		return true
	}
	return isTransient(err)
}

// ShouldRetry returns true only when err is transient AND the caller has not
// yet exhausted MaxTransientAttempts. attempt is the zero-based count of
// attempts already made (i.e. pass 0 before the first retry). This is the
// bounded primitive that guarantees over-classification can't retry forever;
// the actual backoff/sleep loop lives in the consuming sub-tasks.
func ShouldRetry(err error, attempt int) bool {
	return IsTransient(err) && attempt < MaxTransientAttempts
}

// isTransient holds the raw classification logic, free of the TransientError
// short-circuits in IsTransient/Classify.
func isTransient(err error) bool {
	if err == nil {
		return false
	}

	// Typed network errors first — these are unambiguous.
	if errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	msg := strings.ToLower(err.Error())

	// Permanent classes win over transient string heuristics: a 422/404 or a
	// branch-protection refusal must fail fast even if the surrounding text
	// happens to mention something that looks transient.
	if isPermanentMessage(msg) {
		return false
	}

	// HTTP status code driven classification.
	if code, ok := statusCode(msg); ok {
		switch {
		case code == 401:
			return true
		case code == 403:
			return messageContains(msg, "rate limit", "secondary rate limit", "abuse")
		case code >= 500 && code <= 599:
			return true
		case code == 404 || code == 422:
			return false
		}
	}

	// GraphQL literal that surfaces without a numeric status code.
	if strings.Contains(msg, "requires authentication") {
		return true
	}

	// Network failures expressed as plain strings (wrapped exec/gh output).
	if messageContains(msg,
		"dial ",
		"i/o timeout",
		"timeout",
		"connection reset",
		"connection refused",
		"no such host",
		"network is unreachable",
		"eof",
		"tls handshake",
		"unexpected eof",
	) {
		return true
	}

	// Unknown/unrecognised: treat as permanent so retries are always bounded.
	return false
}

// isPermanentMessage matches errors that must fail fast regardless of any HTTP
// status code: 422 validation outcomes, branch-protection refusals, and the
// "already exists" PR conflict.
func isPermanentMessage(msg string) bool {
	return messageContains(msg,
		"no commits between",
		"a pull request already exists",
		"pull request already exists",
		"already exists for branch",
		"protected branch",
		"branch protection",
		"required status check",
		"changes must be made through a pull request",
		"review is required by reviewers with write access",
		"not authorized to push",
	)
}

// statusCodeRe matches a GitHub/gh HTTP status code in error text, e.g.
// "HTTP 403", "(HTTP 422)", "status: 500", or "status code 502".
var statusCodeRe = regexp.MustCompile(`(?:http|status(?:\s+code)?)[\s:]+(\d{3})`)

// statusCode extracts an HTTP status code from a (lower-cased) error message.
// The second return value is false when no status code can be found.
func statusCode(msg string) (int, bool) {
	m := statusCodeRe.FindStringSubmatch(msg)
	if len(m) < 2 {
		return 0, false
	}
	code, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return code, true
}

// messageContains reports whether msg contains any of the given substrings.
// msg is expected to already be lower-cased; needles must be lower-case.
func messageContains(msg string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}
