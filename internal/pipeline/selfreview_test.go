package pipeline

import (
	"math"
	"testing"

	"github.com/Robin831/Forge/internal/poller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSelfReview_ValidJSON(t *testing.T) {
	output := `Some implementation output here...

` + "```json" + `
{"self_review": {"verdict": "approve", "concerns": []}}
` + "```" + `
`
	sr := parseSelfReview(output)
	require.NotNil(t, sr)
	assert.Equal(t, "approve", sr.Verdict)
	assert.Empty(t, sr.Concerns)
}

func TestParseSelfReview_RequestChangesWithConcerns(t *testing.T) {
	output := `Done implementing.

` + "```json" + `
{"self_review": {"verdict": "request_changes", "concerns": ["Missing error handling in foo()", "No test for edge case"]}}
` + "```" + `
`
	sr := parseSelfReview(output)
	require.NotNil(t, sr)
	assert.Equal(t, "request_changes", sr.Verdict)
	assert.Len(t, sr.Concerns, 2)
	assert.Contains(t, sr.Concerns[0], "error handling")
}

func TestParseSelfReview_MissingJSON(t *testing.T) {
	output := "Just some text with no JSON block at all."
	sr := parseSelfReview(output)
	assert.Nil(t, sr)
}

func TestParseSelfReview_MalformedJSON(t *testing.T) {
	output := "```json\n{\"self_review\": {\"verdict\": INVALID}}\n```"
	sr := parseSelfReview(output)
	assert.Nil(t, sr)
}

func TestParseSelfReview_EmptyVerdict(t *testing.T) {
	output := "```json\n{\"self_review\": {\"verdict\": \"\", \"concerns\": []}}\n```"
	sr := parseSelfReview(output)
	assert.Nil(t, sr, "empty verdict should return nil")
}

func TestParseSelfReview_NoSelfReviewKey(t *testing.T) {
	output := "```json\n{\"verdict\": \"approve\"}\n```"
	sr := parseSelfReview(output)
	assert.Nil(t, sr, "JSON without self_review key should return nil")
}

func TestShouldRunRealWarden_P0AlwaysTrue(t *testing.T) {
	sr := &SelfReview{Verdict: "approve"}
	bead := poller.Bead{Priority: 0}
	assert.True(t, shouldRunRealWarden(sr, bead, 0.0))
}

func TestShouldRunRealWarden_P1AlwaysTrue(t *testing.T) {
	sr := &SelfReview{Verdict: "approve"}
	bead := poller.Bead{Priority: 1}
	assert.True(t, shouldRunRealWarden(sr, bead, 0.0))
}

func TestShouldRunRealWarden_NilSelfReview(t *testing.T) {
	bead := poller.Bead{Priority: 3}
	assert.True(t, shouldRunRealWarden(nil, bead, 0.0))
}

func TestShouldRunRealWarden_RequestChanges(t *testing.T) {
	sr := &SelfReview{Verdict: "request_changes", Concerns: []string{"bug"}}
	bead := poller.Bead{Priority: 3}
	assert.True(t, shouldRunRealWarden(sr, bead, 0.0))
}

func TestShouldRunRealWarden_ApprovedP3NoSampling(t *testing.T) {
	sr := &SelfReview{Verdict: "approve"}
	bead := poller.Bead{Priority: 3}
	// With sample rate 0.0, should never trigger sampling.
	assert.False(t, shouldRunRealWarden(sr, bead, 0.0))
}

func TestShouldRunRealWarden_ApprovedP4NoSampling(t *testing.T) {
	sr := &SelfReview{Verdict: "approve"}
	bead := poller.Bead{Priority: 4}
	assert.False(t, shouldRunRealWarden(sr, bead, 0.0))
}

func TestShouldRunRealWarden_SamplingRate100Percent(t *testing.T) {
	sr := &SelfReview{Verdict: "approve"}
	bead := poller.Bead{Priority: 3}
	// With sample rate 1.0, should always trigger.
	assert.True(t, shouldRunRealWarden(sr, bead, 1.0))
}

func TestShouldRunRealWarden_SamplingStatistical(t *testing.T) {
	sr := &SelfReview{Verdict: "approve"}
	bead := poller.Bead{Priority: 3}
	sampleRate := 0.5

	triggered := 0
	trials := 10000
	for i := 0; i < trials; i++ {
		if shouldRunRealWarden(sr, bead, sampleRate) {
			triggered++
		}
	}

	// Expect ~50% with some tolerance.
	ratio := float64(triggered) / float64(trials)
	assert.True(t, math.Abs(ratio-sampleRate) < 0.05,
		"expected ~%.0f%% sampling, got %.1f%%", sampleRate*100, ratio*100)
}
