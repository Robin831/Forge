package pipeline

import (
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

func TestParseSelfReview_UnknownVerdict(t *testing.T) {
	output := "```json\n{\"self_review\": {\"verdict\": \"approved\", \"concerns\": []}}\n```"
	sr := parseSelfReview(output)
	assert.Nil(t, sr, "unknown verdict 'approved' should return nil (only 'approve'/'request_changes' are valid)")
}

func TestParseSelfReview_VerdictCaseNormalized(t *testing.T) {
	output := "```json\n{\"self_review\": {\"verdict\": \"  Approve  \", \"concerns\": []}}\n```"
	sr := parseSelfReview(output)
	require.NotNil(t, sr, "verdict with whitespace/casing should be normalized and accepted")
	assert.Equal(t, "approve", sr.Verdict, "verdict should be normalized to lowercase")
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

func TestShouldRunRealWarden_ApprovedWithConcerns(t *testing.T) {
	// Even an "approve" verdict should trigger real Warden when concerns are listed.
	sr := &SelfReview{Verdict: "approve", Concerns: []string{"potential nil dereference"}}
	bead := poller.Bead{Priority: 3}
	assert.True(t, shouldRunRealWarden(sr, bead, 0.0), "non-empty concerns should always trigger real Warden")
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

func TestShouldRunRealWarden_SamplingDeterministic(t *testing.T) {
	sr := &SelfReview{Verdict: "approve"}
	bead := poller.Bead{Priority: 3}

	// Replace the random source with a deterministic sequence so the test
	// is fully reproducible.
	callCount := 0
	values := []float64{0.05, 0.15, 0.50, 0.95, 0.09, 0.11}
	origRand := randFloat64
	randFloat64 = func() float64 {
		v := values[callCount%len(values)]
		callCount++
		return v
	}
	defer func() { randFloat64 = origRand }()

	sampleRate := 0.1

	// 0.05 < 0.1 → true
	assert.True(t, shouldRunRealWarden(sr, bead, sampleRate), "0.05 should trigger sampling")
	// 0.15 >= 0.1 → false
	assert.False(t, shouldRunRealWarden(sr, bead, sampleRate), "0.15 should not trigger sampling")
	// 0.50 >= 0.1 → false
	assert.False(t, shouldRunRealWarden(sr, bead, sampleRate), "0.50 should not trigger sampling")
	// 0.95 >= 0.1 → false
	assert.False(t, shouldRunRealWarden(sr, bead, sampleRate), "0.95 should not trigger sampling")
	// 0.09 < 0.1 → true
	assert.True(t, shouldRunRealWarden(sr, bead, sampleRate), "0.09 should trigger sampling")
	// 0.11 >= 0.1 → false
	assert.False(t, shouldRunRealWarden(sr, bead, sampleRate), "0.11 should not trigger sampling")
}
