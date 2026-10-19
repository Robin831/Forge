package warden

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Robin831/Forge/internal/config"
)

// config mirrors the warden's default cap because it cannot import this package;
// this is the check that the two copies agree.
func TestTheTwoDefaultCapsAgree(t *testing.T) {
	assert.Equal(t, DefaultMaxRulesPerReview, config.DefaultWardenMaxRulesPerReview)
}
