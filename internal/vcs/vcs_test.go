package vcs

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidPlatform(t *testing.T) {
	assert.True(t, ValidPlatform(""))
	assert.True(t, ValidPlatform("github"))
	assert.True(t, ValidPlatform("gitlab"))
	assert.True(t, ValidPlatform("gitea"))
	assert.True(t, ValidPlatform("bitbucket"))
	assert.True(t, ValidPlatform("azuredevops"))

	assert.False(t, ValidPlatform("mercurial"))
	assert.False(t, ValidPlatform("svn"))
}

func TestReviewCommentFields(t *testing.T) {
	c := ReviewComment{
		Author:   "alice",
		Body:     "Please fix this",
		Path:     "main.go",
		Line:     42,
		State:    "CHANGES_REQUESTED",
		ThreadID: "T_abc123",
	}
	assert.Equal(t, "alice", c.Author)
	assert.Equal(t, 42, c.Line)
	assert.Equal(t, "T_abc123", c.ThreadID)
}

func TestPRCommentFields(t *testing.T) {
	c := PRComment{
		Body:     "Consider error handling",
		User:     "copilot[bot]",
		Path:     "handler.go",
		PRNumber: 99,
	}
	assert.Equal(t, "copilot[bot]", c.User)
	assert.Equal(t, 99, c.PRNumber)
}
