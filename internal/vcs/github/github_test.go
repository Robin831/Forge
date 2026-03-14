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
