package poller

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The filtering behind OpenChildren decides between opposite outcomes for an
// opted-in parent: an empty answer dispatches it to main and closes it, a
// non-empty one holds it. Both filters — the dependency type and the status
// vocabulary — are assumptions about bd's output, so they are pinned here.
func TestParseOpenChildren(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   []string
	}{
		{
			name:   "array-wrapped, the shape bd show --json actually returns",
			output: `[{"id":"parent-1","dependents":[{"id":"child-1","dependency_type":"blocks","status":"open"}]}]`,
			want:   []string{"child-1"},
		},
		{
			name:   "bare object",
			output: `{"dependents":[{"id":"child-1","dependency_type":"blocks","status":"open"}]}`,
			want:   []string{"child-1"},
		},
		{
			name: "both child edge types count",
			output: `{"dependents":[
				{"id":"child-1","dependency_type":"blocks","status":"open"},
				{"id":"child-2","dependency_type":"parent-child","status":"in_progress"}]}`,
			want: []string{"child-1", "child-2"},
		},
		{
			name: "a plain dependency edge is not a child",
			output: `{"dependents":[
				{"id":"downstream-1","dependency_type":"discovered-from","status":"open"},
				{"id":"downstream-2","dependency_type":"related","status":"open"}]}`,
			want: nil,
		},
		{
			name: "closed children do not count, whatever the casing or padding",
			output: `{"dependents":[
				{"id":"child-1","dependency_type":"blocks","status":"closed"},
				{"id":"child-2","dependency_type":"blocks","status":"CLOSED"},
				{"id":"child-3","dependency_type":"parent-child","status":" closed "}]}`,
			want: nil,
		},
		{
			name: "an epic mid-flight reports only what is left",
			output: `{"dependents":[
				{"id":"child-1","dependency_type":"blocks","status":"closed"},
				{"id":"child-2","dependency_type":"blocks","status":"blocked"}]}`,
			want: []string{"child-2"},
		},
		{
			name:   "no dependents at all",
			output: `{"id":"parent-1","dependents":[]}`,
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOpenChildren([]byte(tt.output))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Malformed output must not read as "this epic has no children left": that
// answer merges the parent to main while its children are still routed to a
// branch nobody would then create.
func TestParseOpenChildren_MalformedIsAnError(t *testing.T) {
	for _, output := range []string{"", "not json", `{"dependents":`, `{"dependents":"child-1"}`} {
		_, err := parseOpenChildren([]byte(output))
		assert.Error(t, err, "output %q must be an error, not an empty slice", output)
	}
}

// The same contract one level up: a bd that cannot be reached is an error, and
// the caller defers rather than deciding.
func TestOpenChildren_BdFailureIsAnError(t *testing.T) {
	withFakeBd(t, "exit 3")

	_, err := OpenChildren(context.Background(), "parent-1", t.TempDir())

	assert.Error(t, err)
}

// The happy path end to end, through a real subprocess: the wrapping array bd
// emits is unwrapped and the open child survives both filters.
func TestOpenChildren_ReadsBdOutput(t *testing.T) {
	withFakeBd(t, `echo '[{"id":"parent-1","dependents":[`+
		`{"id":"child-1","dependency_type":"blocks","status":"open"},`+
		`{"id":"child-2","dependency_type":"blocks","status":"closed"}]}]'`)

	open, err := OpenChildren(context.Background(), "parent-1", t.TempDir())

	require.NoError(t, err)
	assert.Equal(t, []string{"child-1"}, open)
}

// withFakeBd puts a `bd` on PATH whose body is the given shell snippet, the
// same seam internal/daemon's tests use to exercise bd-shaped code without a
// beads database.
func withFakeBd(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd is a shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "bd")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
