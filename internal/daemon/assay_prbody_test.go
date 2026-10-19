package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Robin831/Forge/internal/assay"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
)

// bodyVCS is a provider that can read a PR body; the embedded Provider is nil,
// so any other method would panic, which keeps the test to the one call.
type bodyVCS struct {
	vcs.Provider
	body string
	err  error
	asks []int
}

func (b *bodyVCS) PRBody(_ context.Context, _ string, prNumber int) (string, error) {
	b.asks = append(b.asks, prNumber)
	return b.body, b.err
}

type noBodyVCS struct{ vcs.Provider }

func TestAssayPRBodyComesFromTheProviderAndFailsOpen(t *testing.T) {
	env := newTestDaemon(t)
	d := env.d
	reader := &bodyVCS{body: "Closes-Bead: Fhi.Metadata-abc12"}
	d.vcsProviders = map[string]vcs.Provider{"munin": reader, "plain": &noBodyVCS{}, "broken": &bodyVCS{err: errors.New("gh down")}}

	assert.Equal(t, "Closes-Bead: Fhi.Metadata-abc12", d.assayPRBody(context.Background(), "munin", "/wt", 6731))
	assert.Equal(t, []int{6731}, reader.asks)
	assert.Equal(t, "", d.assayPRBody(context.Background(), "plain", "/wt", 1), "a provider that cannot read bodies gives none")
	assert.Equal(t, "", d.assayPRBody(context.Background(), "broken", "/wt", 1), "a failed read reviews without the body")
	assert.Equal(t, "", d.assayPRBody(context.Background(), "unknown", "/wt", 1), "no registered provider, no fallback to gh")
}

// TestRunAssayReviewPassesThePRBody pins the call site: the body the provider
// reads is the Description the engine is given.
func TestRunAssayReviewPassesThePRBody(t *testing.T) {
	d, _ := newAssayRunDaemon(t)
	d.vcsProviders = map[string]vcs.Provider{"forge": &bodyVCS{body: "## Changes\n- x"}}
	var got assay.ReviewRequest
	d.assayReview = func(_ context.Context, req assay.ReviewRequest, _ *state.DB, _ assay.Config) (*assay.ReviewResult, error) {
		got = req
		return &assay.ReviewResult{Status: assay.RunStatusComplete, CompletedPasses: 1, TotalPasses: 1}, nil
	}
	_, err := runTestAssayReview(t, d)
	assert.NoError(t, err)
	assert.Equal(t, "## Changes\n- x", got.Description)
}
