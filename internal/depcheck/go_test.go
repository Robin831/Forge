package depcheck

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseGoJSONOutput(t *testing.T) {
	modules := []goModule{
		{Path: "github.com/Robin831/Forge", Main: true},
		{Path: "github.com/foo/bar", Version: "v1.2.3", Update: &goUpdate{Path: "github.com/foo/bar", Version: "v1.2.5"}},
		{Path: "github.com/baz/qux", Version: "v0.3.0", Update: &goUpdate{Path: "github.com/baz/qux", Version: "v0.4.1"}},
		{Path: "github.com/big/lib", Version: "v2.0.0", Update: &goUpdate{Path: "github.com/big/lib", Version: "v3.0.0"}},
		{Path: "github.com/up/to/date", Version: "v1.0.0"}, // no update
		{Path: "github.com/indirect/dep", Version: "v1.0.0", Indirect: true, Update: &goUpdate{Path: "github.com/indirect/dep", Version: "v1.1.0"}},
		{Path: "github.com/another/patch", Version: "v0.1.1", Update: &goUpdate{Path: "github.com/another/patch", Version: "v0.1.2"}},
	}

	var data []byte
	for _, m := range modules {
		b, _ := json.Marshal(m)
		data = append(data, b...)
		data = append(data, '\n')
	}

	updates := parseGoJSONOutput(data)
	require.Len(t, updates, 4) // main, no-update, and indirect are all skipped

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

func TestParseGoJSONOutput_AllIndirect(t *testing.T) {
	modules := []goModule{
		{Path: "github.com/main/mod", Main: true},
		{Path: "github.com/a", Version: "v1.0.0", Indirect: true, Update: &goUpdate{Version: "v1.1.0"}},
		{Path: "github.com/b", Version: "v2.0.0", Indirect: true, Update: &goUpdate{Version: "v2.0.1"}},
	}

	var data []byte
	for _, m := range modules {
		b, _ := json.Marshal(m)
		data = append(data, b...)
		data = append(data, '\n')
	}

	updates := parseGoJSONOutput(data)
	assert.Empty(t, updates)
}

func TestParseGoJSONOutput_Empty(t *testing.T) {
	updates := parseGoJSONOutput([]byte{})
	assert.Empty(t, updates)
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
		{"v1.0.0", "v1.0.0-rc1", "patch"},
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
		input               string
		major, minor, patch string
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
