package depcheck

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseGoListOutput(t *testing.T) {
	input := `github.com/Robin831/Forge
github.com/foo/bar v1.2.3 [v1.2.5]
github.com/baz/qux v0.3.0 [v0.4.1]
github.com/big/lib v2.0.0 [v3.0.0]
github.com/up/to/date v1.0.0
github.com/another/patch v0.1.1 [v0.1.2]
`

	updates := parseGoListOutput(input)
	require.Len(t, updates, 4)

	// Sorted: major first, then minor, then patch (alphabetically within kind)
	assert.Equal(t, "github.com/big/lib", updates[0].Path)
	assert.Equal(t, "major", updates[0].Kind)
	assert.Equal(t, "v2.0.0", updates[0].Current)
	assert.Equal(t, "v3.0.0", updates[0].Latest)

	assert.Equal(t, "github.com/baz/qux", updates[1].Path)
	assert.Equal(t, "minor", updates[1].Kind)

	assert.Equal(t, "github.com/another/patch", updates[2].Path)
	assert.Equal(t, "patch", updates[2].Kind)

	assert.Equal(t, "github.com/foo/bar", updates[3].Path)
	assert.Equal(t, "patch", updates[3].Kind)
}

func TestParseGoListOutput_Empty(t *testing.T) {
	// Only main module, no updates
	input := `github.com/Robin831/Forge
github.com/foo/bar v1.0.0
`
	updates := parseGoListOutput(input)
	assert.Empty(t, updates)
}

func TestParseGoListOutput_PseudoVersions(t *testing.T) {
	input := `github.com/Robin831/Forge
github.com/foo/bar v0.0.0-20230101-abc1234 [v0.1.0]
`
	updates := parseGoListOutput(input)
	require.Len(t, updates, 1)
	assert.Equal(t, "minor", updates[0].Kind)
	assert.Equal(t, "v0.0.0-20230101-abc1234", updates[0].Current)
	assert.Equal(t, "v0.1.0", updates[0].Latest)
}

func TestClassifyUpdate(t *testing.T) {
	tests := []struct {
		current  string
		latest   string
		expected string
	}{
		{"v1.2.3", "v1.2.5", "patch"},
		{"v1.2.3", "v1.3.0", "minor"},
		{"v1.2.3", "v2.0.0", "major"},
		{"v0.1.0", "v0.2.0", "minor"},
		{"v0.0.1", "v0.0.2", "patch"},
		{"v1.0.0", "v1.0.0-rc1", "patch"}, // same version with pre-release
	}

	for _, tt := range tests {
		t.Run(tt.current+"->"+tt.latest, func(t *testing.T) {
			result := classifyUpdate(tt.current, tt.latest)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseSemver(t *testing.T) {
	tests := []struct {
		input                string
		major, minor, patch  string
	}{
		{"v1.2.3", "1", "2", "3"},
		{"v0.0.0", "0", "0", "0"},
		{"v2.1.0-pre.1", "2", "1", "0"},
		{"v0.0.0-20230101120000-abc1234def56", "0", "0", "0"},
		{"1.2.3", "1", "2", "3"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			maj, min, pat := parseSemver(tt.input)
			assert.Equal(t, tt.major, maj)
			assert.Equal(t, tt.minor, min)
			assert.Equal(t, tt.patch, pat)
		})
	}
}
