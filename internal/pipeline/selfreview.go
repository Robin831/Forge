package pipeline

import (
	"encoding/json"
	"math/rand"
	"regexp"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/poller"
)

// SelfReview captures the self-review verdict emitted by Smith when running in
// combined Smith+Warden mode. Smith is instructed to append a JSON block at the
// end of its output containing a verdict and optional list of concerns.
type SelfReview struct {
	Verdict  string   `json:"verdict"`  // "approve" or "request_changes"
	Concerns []string `json:"concerns"` // free-text concern descriptions
}

// selfReviewEnvelope wraps the JSON structure Smith is expected to produce.
type selfReviewEnvelope struct {
	SelfReview SelfReview `json:"self_review"`
}

// selfReviewJSONRe matches a fenced JSON block containing a "self_review" key.
var selfReviewJSONRe = regexp.MustCompile("(?s)```json\\s*\\n(\\{[^`]*\"self_review\"[^`]*\\})\\s*\\n```")

// parseSelfReview extracts a SelfReview from Smith's output. It looks for a
// fenced ```json block containing a "self_review" key. Returns nil if not found
// or if the JSON is malformed — the caller should treat nil as a signal to
// fall back to a real Warden review.
func parseSelfReview(smithOutput string) *SelfReview {
	matches := selfReviewJSONRe.FindStringSubmatch(smithOutput)
	if len(matches) < 2 {
		return nil
	}

	var env selfReviewEnvelope
	if err := json.Unmarshal([]byte(matches[1]), &env); err != nil {
		return nil
	}

	// Normalize verdict: trim whitespace and lowercase for consistent matching.
	normalized := strings.ToLower(strings.TrimSpace(env.SelfReview.Verdict))
	if normalized != "approve" && normalized != "request_changes" {
		// Unknown or empty verdict — fail safe and return nil so the caller
		// falls back to a real Warden review.
		return nil
	}
	env.SelfReview.Verdict = normalized

	return &env.SelfReview
}

// rng is a time-seeded random number generator for Warden sampling. Using an
// explicit seed makes seeding visible and avoids relying on package-global
// state (even though Go 1.20+ auto-seeds the global source).
var rng = rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec // not cryptographic

// randFloat64 is the random number generator used for Warden sampling.
// It defaults to a time-seeded RNG but can be overridden in tests for
// deterministic behavior.
var randFloat64 = rng.Float64

// shouldRunRealWarden decides whether a real Warden review should be spawned
// when running in combined Smith+Warden mode. A real Warden is always required
// for high-priority beads, when the self-review failed to parse, when Smith
// flagged concerns, or via random sampling for quality validation.
func shouldRunRealWarden(selfReview *SelfReview, bead poller.Bead, sampleRate float64) bool {
	// Always review critical and high-priority beads.
	if bead.Priority <= 1 {
		return true
	}
	// Parse failure, request_changes verdict, or any listed concerns — real review needed.
	// Concerns are treated as a signal even when the overall verdict is "approve",
	// because Smith may self-approve while still identifying issues.
	if selfReview == nil || selfReview.Verdict == "request_changes" || len(selfReview.Concerns) > 0 {
		return true
	}
	// Random sampling for ongoing quality validation.
	return randFloat64() < sampleRate
}
