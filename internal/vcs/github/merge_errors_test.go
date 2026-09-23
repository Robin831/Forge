package github

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The two ways `gh pr merge` failed on Explorer #401, verbatim.
func TestIsTransient_MergeFailures(t *testing.T) {
	graphQL500 := errors.New("gh pr merge failed: exit status 1\nstderr: GraphQL: Something went wrong while executing your query on 2026-09-22T23:51:39Z. Please include `3F10:3DE84C:11B410:119655:6AB3148A` when reporting this issue.\n")
	assert.True(t, IsTransient(graphQL500), "GraphQL's internal-error message is a 5xx and worth a retry")

	policy := errors.New("gh pr merge failed: exit status 1\nstderr: X Pull request FHIDev/Fhi.Munin.Explorer#401 is not mergeable: the base branch policy prohibits the merge.\nTo have the pull request merged after all the requirements have been met, add the `--auto` flag.\n")
	assert.False(t, IsTransient(policy), "a branch policy refusal is permanent")
}
