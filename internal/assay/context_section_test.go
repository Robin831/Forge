package assay

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContextSectionShowsThePRBodyFenced(t *testing.T) {
	long := strings.Repeat("x", 5000)
	got := contextSection(ReviewRequest{Title: "fix: y", Description: "## Changes\n- ```break out```\n" + long})
	assert.Contains(t, got, "**PR body**:\n```text\n## Changes")
	assert.Contains(t, got, long, "a body of a few thousand characters is kept whole")
	assert.NotContains(t, got, "```break out```", "a fence inside the body cannot close the block")
	assert.Contains(t, contextSection(ReviewRequest{Description: strings.Repeat("y", 7000)}), "...[truncated]...")
}
