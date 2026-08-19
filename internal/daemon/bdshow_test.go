package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Robin831/Forge/internal/executil"
)

// bdShowProbe is a minimal target type: enough fields to see which element of
// a payload was decoded, nothing bd-specific.
type bdShowProbe struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// TestDecodeBdShow pins the helper's two deliberate semantics for all four
// call sites at once: the array form is a real fallback (the reason the helper
// exists), and an empty array is an error rather than a zero T — a payload
// that names no bead must never read as "status: not closed" / "no
// dependents". It also pins that noise around either form still decodes, so
// the callers that used to hand-extract `{...}` out of noisy `[{...}]`
// payloads keep working through the shared path.
func TestDecodeBdShow(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bdShowProbe
		wantErr bool
	}{
		{
			name:    "bare object",
			payload: `{"id":"Forge-a1","status":"open"}`,
			want:    bdShowProbe{ID: "Forge-a1", Status: "open"},
		},
		{
			name:    "single-element array",
			payload: `[{"id":"Forge-a2","status":"closed"}]`,
			want:    bdShowProbe{ID: "Forge-a2", Status: "closed"},
		},
		{
			name:    "multi-element array decodes the first element",
			payload: `[{"id":"Forge-a3","status":"open"},{"id":"Forge-zz","status":"closed"}]`,
			want:    bdShowProbe{ID: "Forge-a3", Status: "open"},
		},
		{
			name:    "array with surrounding diagnostic noise",
			payload: "beads: some warning line\n [{\"id\":\"Forge-a4\",\"status\":\"open\"}]\ntrailing noise",
			want:    bdShowProbe{ID: "Forge-a4", Status: "open"},
		},
		{
			name:    "object with surrounding diagnostic noise",
			payload: "beads: some warning line\n {\"id\":\"Forge-a5\",\"status\":\"open\"}\ntrailing noise",
			want:    bdShowProbe{ID: "Forge-a5", Status: "open"},
		},
		{
			name:    "empty array is an error, not a zero value",
			payload: `[]`,
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			payload: "not json at all",
			wantErr: true,
		},
		{
			name:    "empty payload",
			payload: "",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeBdShow[bdShowProbe]([]byte(tc.payload))
			if tc.wantErr {
				require.Error(t, err)
				assert.Zero(t, got, "a failed decode must hand back a zero T, never a partial one")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// withFakeBd puts a `bd` shell script on PATH so defaultBeadShower runs end to
// end. Tests elsewhere replace d.beadShower with a stub, which deletes exactly
// the wiring under test here — the dependents flag and the classification — so
// this is the only place either is executed.
func withFakeBd(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd is a shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "bd")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writing fake bd: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// maybeCloseDecomposedParent reads the dependents array to decide whether a
// parent still has children, so a shower that loses the flag reports every
// parent as childless — the branch that closes a bead its children are still
// blocked on. The fake withholds the array unless asked, the way bd 1.1.2 does,
// so losing the flag fails here instead of closing beads in production.
func TestDefaultBeadShower_AsksForTheDependentsArray(t *testing.T) {
	withFakeBd(t, `flag=""
for a in "$@"; do
  case "$a" in --include-dependents) flag=1 ;; esac
done
if [ -n "$flag" ]; then
  echo '[{"id":"parent-1","status":"open","dependent_count":1,"dependents":[{"id":"child-1","status":"open"}]}]'
else
  echo '[{"id":"parent-1","status":"open","dependent_count":1}]'
fi`)

	out, stderr, err := defaultBeadShower(t.TempDir(), "parent-1")
	if err != nil {
		t.Fatalf("defaultBeadShower: %v (stderr %q)", err, stderr)
	}
	if !strings.Contains(string(out), `"dependents"`) {
		t.Errorf("output %s has no dependents array — the flag was not sent", out)
	}
}

// An older bd is a named refusal, not an empty dependents array: the two mean
// opposite things to every caller of this shower.
func TestDefaultBeadShower_OlderBdIsANamedError(t *testing.T) {
	withFakeBd(t, `echo "Error: unknown flag: --include-dependents" >&2
exit 1`)

	_, stderr, err := defaultBeadShower(t.TempDir(), "parent-1")
	if err == nil {
		t.Fatal("defaultBeadShower succeeded against a bd without the flag")
	}
	if !errors.Is(err, executil.ErrIncludeDependentsUnsupported) {
		t.Errorf("error %v does not wrap ErrIncludeDependentsUnsupported", err)
	}
	if !strings.Contains(stderr, "unknown flag") {
		t.Errorf("stderr %q does not carry bd's diagnostic", stderr)
	}
}
