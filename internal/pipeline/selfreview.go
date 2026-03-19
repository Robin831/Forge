package pipeline

import (
	"encoding/json"
	"math/rand"
	"regexp"

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

	// Require a non-empty verdict.
	if env.SelfReview.Verdict == "" {
		return nil
	}

	return &env.SelfReview
}

// shouldRunRealWarden decides whether a real Warden review should be spawned
// when running in combined Smith+Warden mode. A real Warden is always required
// for high-priority beads, when the self-review failed to parse, when Smith
// flagged concerns, or via random sampling for quality validation.
func shouldRunRealWarden(selfReview *SelfReview, bead poller.Bead, sampleRate float64) bool {
	// Always review critical and high-priority beads.
	if bead.Priority <= 1 {
		return true
	}
	// Parse failure or self-review flagged concerns — real review needed.
	if selfReview == nil || selfReview.Verdict == "request_changes" {
		return true
	}
	// Random sampling for ongoing quality validation.
	return rand.Float64() < sampleRate
}
