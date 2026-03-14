package github

import (
	"testing"

	"github.com/Robin831/Forge/internal/vcs"
	"github.com/stretchr/testify/assert"
)

func TestProviderImplementsInterface(t *testing.T) {
	var _ vcs.Provider = (*Provider)(nil)
}

func TestProviderName(t *testing.T) {
	p := New()
	assert.Equal(t, "GitHub", p.Name())
}

func TestParsePaginatedComments(t *testing.T) {
	// Single page
	data := []byte(`[{"body":"fix this","path":"a.go","user":{"login":"bot"}}]`)
	comments, err := parsePaginatedComments(data)
	assert.NoError(t, err)
	assert.Len(t, comments, 1)
	assert.Equal(t, "fix this", comments[0].Body)

	// Multi-page (gh api --paginate concatenates arrays)
	data = []byte(`[{"body":"a","path":"x.go","user":{"login":"u1"}}][{"body":"b","path":"y.go","user":{"login":"u2"}}]`)
	comments, err = parsePaginatedComments(data)
	assert.NoError(t, err)
	assert.Len(t, comments, 2)

	// Empty
	comments, err = parsePaginatedComments([]byte{})
	assert.NoError(t, err)
	assert.Len(t, comments, 0)
}

func TestCopilotBotLogins(t *testing.T) {
	assert.True(t, copilotBotLogins["copilot[bot]"])
	assert.True(t, copilotBotLogins["github-actions[bot]"])
	assert.True(t, copilotBotLogins["copilot"])
	assert.False(t, copilotBotLogins["alice"])
}
