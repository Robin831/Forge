package daemon

import (
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/stretchr/testify/assert"
)

// A hot reload that changes only depcheck_exclude must still reach the scanner
// (review on Fhi.Metadata-oohxy): updateAnvilPaths returns early unless it sees a change.
func TestDepcheckExcludeChangedAloneCountsAsAChange(t *testing.T) {
	base := config.AnvilConfig{Path: "/p"}
	with := config.AnvilConfig{Path: "/p", DepcheckExclude: []string{"tools/**"}}
	assert.True(t, depcheckExcludeChanged(base, with))
	assert.True(t, depcheckExcludeChanged(with, config.AnvilConfig{Path: "/p", DepcheckExclude: []string{"docs/**"}}))
	assert.False(t, depcheckExcludeChanged(with, config.AnvilConfig{Path: "/p", DepcheckExclude: []string{"tools/**"}}))
	assert.False(t, depcheckExcludeChanged(base, config.AnvilConfig{Path: "/p", DepcheckExclude: []string{}}), "nil and empty are the same: nothing extra")

	got := depcheckExcludes(map[string]config.AnvilConfig{"explorer": with, "munin": base})
	assert.Equal(t, map[string][]string{"explorer": {"tools/**"}}, got)
}
