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
