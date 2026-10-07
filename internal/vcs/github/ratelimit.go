package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultRateLimitWait is the wait applied when a rate-limit refusal carries
// neither Retry-After nor x-ratelimit-reset. GitHub's guidance for secondary
// limits without a header is to wait at least one minute.
const DefaultRateLimitWait = time.Minute

// MaxRateLimitRetries bounds in-line retries of a rate-limited call. Replaying
// a refused call chain is what escalates a secondary limit, so it is one.
const MaxRateLimitRetries = 1

// RateLimitError is a GitHub refusal on rate-limit grounds, primary or
// secondary. RetryAfter is the server's hint (Retry-After, or
// x-ratelimit-reset minus now); zero means the response carried neither.
type RateLimitError struct {
	Err        error
	Secondary  bool
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e == nil || e.Err == nil {
		return "github rate limit"
	}
	return e.Err.Error()
}

func (e *RateLimitError) Unwrap() error { return e.Err }

// Wait is how long to hold off before the next call: the server's hint, or
// DefaultRateLimitWait when it gave none.
func (e *RateLimitError) Wait() time.Duration {
	if e.RetryAfter > 0 {
		return e.RetryAfter
	}
	return DefaultRateLimitWait
}

// AsRateLimit reports whether err is a rate-limit refusal. A typed
// *RateLimitError (from a call that could read the response headers) is
// returned as is; otherwise one is synthesised from gh's error text, with no
// RetryAfter because gh does not print headers unless asked to.
func AsRateLimit(err error) (*RateLimitError, bool) {
	if err == nil {
		return nil, false
	}
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return rl, true
	}
	msg := strings.ToLower(err.Error())
	secondary := messageContains(msg, "secondary rate limit", "abuse")
	code, hasCode := statusCode(msg)
	switch {
	case hasCode && code == 429:
		return &RateLimitError{Err: err, Secondary: !messageContains(msg, "api rate limit exceeded")}, true
	case hasCode && code == 403 && messageContains(msg, "rate limit", "abuse"):
		return &RateLimitError{Err: err, Secondary: secondary}, true
	case !hasCode && messageContains(msg, "api rate limit exceeded", "secondary rate limit"):
		return &RateLimitError{Err: err, Secondary: secondary}, true
	}
	return nil, false
}

// splitIncludedResponse splits the stdout of `gh api --include` into the HTTP
// status code, the response headers and the body. ok is false when out does
// not start with a status line (gh failed before any response arrived).
func splitIncludedResponse(out []byte) (code int, hdr http.Header, body []byte, ok bool) {
	if !bytes.HasPrefix(out, []byte("HTTP/")) {
		return 0, nil, out, false
	}
	nl := bytes.IndexByte(out, '\n')
	if nl < 0 {
		return 0, nil, out, false
	}
	fields := strings.Fields(string(out[:nl]))
	if len(fields) < 2 {
		return 0, nil, out, false
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, nil, out, false
	}
	rest := out[nl+1:]
	end, sepLen := bytes.Index(rest, []byte("\r\n\r\n")), 4
	if end < 0 {
		end, sepLen = bytes.Index(rest, []byte("\n\n")), 2
	}
	var head []byte
	if end < 0 {
		head, body = rest, nil
	} else {
		head, body = rest[:end], rest[end+sepLen:]
	}
	hdr = http.Header{}
	for _, line := range strings.Split(string(head), "\n") {
		k, v, found := strings.Cut(strings.TrimRight(line, "\r"), ":")
		if found {
			hdr.Add(strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	return code, hdr, body, true
}

// rateLimitFromResponse returns a *RateLimitError wrapping cause when the
// response is a rate-limit refusal, or nil. now anchors x-ratelimit-reset.
func rateLimitFromResponse(code int, hdr http.Header, body []byte, now time.Time, cause error) *RateLimitError {
	lower := strings.ToLower(string(body))
	remaining := hdr.Get("X-Ratelimit-Remaining")
	mentions := strings.Contains(lower, "rate limit") || strings.Contains(lower, "abuse")
	gqlLimited := code == http.StatusOK && graphQLRateLimited(body)
	limited := code == http.StatusTooManyRequests ||
		(code == http.StatusForbidden && (remaining == "0" || mentions)) ||
		gqlLimited
	if !limited {
		return nil
	}
	return &RateLimitError{
		Err:        cause,
		Secondary:  remaining != "0" && !gqlLimited,
		RetryAfter: retryAfterFromHeaders(hdr, now),
	}
}

// retryAfterFromHeaders reads Retry-After (seconds or an HTTP date), falling
// back to x-ratelimit-reset (epoch seconds). Zero means neither was usable.
func retryAfterFromHeaders(hdr http.Header, now time.Time) time.Duration {
	if v := strings.TrimSpace(hdr.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return atLeastOneSecond(time.Duration(secs) * time.Second)
		}
		if t, err := http.ParseTime(v); err == nil {
			return atLeastOneSecond(t.Sub(now))
		}
	}
	if v := strings.TrimSpace(hdr.Get("X-Ratelimit-Reset")); v != "" {
		if epoch, err := strconv.ParseInt(v, 10, 64); err == nil {
			return atLeastOneSecond(time.Unix(epoch, 0).Sub(now))
		}
	}
	return 0
}

// atLeastOneSecond keeps a hint that has already elapsed from reading as
// "no hint" (zero), which would substitute the one-minute default.
func atLeastOneSecond(d time.Duration) time.Duration {
	if d < time.Second {
		return time.Second
	}
	return d
}

func graphQLRateLimited(body []byte) bool {
	var resp struct {
		Errors []struct {
			Type string `json:"type"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return false
	}
	for _, e := range resp.Errors {
		if e.Type == "RATE_LIMITED" {
			return true
		}
	}
	return false
}
