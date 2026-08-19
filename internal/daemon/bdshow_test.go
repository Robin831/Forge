package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
