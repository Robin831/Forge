package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Robin831/Forge/internal/poller"
)

// The dispatch seam the widened predicate exists for. ForceIndependent is
// json:"-", so a bead restored from the queue cache — or re-fetched from bd —
// arrives carrying only its label; dispatching on the flag alone would send it
// back through its parent's epic on the next poll, which is the divergence the
// per-child opt-out is supposed to be immune to.
func TestSkipsEpicRouting(t *testing.T) {
	tests := []struct {
		name       string
		bead       poller.Bead
		resuming   bool
		wantSkip   bool
		wantReason string
	}{
		{
			name:     "an ordinary bead consults the epic gates",
			bead:     poller.Bead{ID: "b-1"},
			wantSkip: false,
		},
		{
			name:       "labelled but unflagged — the queue-cache case",
			bead:       poller.Bead{ID: "b-1", Labels: []string{"independent"}},
			wantSkip:   true,
			wantReason: skipReasonIndependent,
		},
		{
			name:       "flagged but unlabelled — the manual run-independently case",
			bead:       poller.Bead{ID: "b-1", ForceIndependent: true},
			wantSkip:   true,
			wantReason: skipReasonIndependent,
		},
		{
			name:       "the label is normalised here like everywhere else",
			bead:       poller.Bead{ID: "b-1", Labels: []string{" Independent "}},
			wantSkip:   true,
			wantReason: skipReasonIndependent,
		},
		{
			name:       "a restart-resume never reaches the epic gates",
			bead:       poller.Bead{ID: "b-1"},
			resuming:   true,
			wantSkip:   true,
			wantReason: skipReasonResume,
		},
		{
			name:       "independent wins the reason when a resumed bead is also opted out",
			bead:       poller.Bead{ID: "b-1", Labels: []string{"independent"}},
			resuming:   true,
			wantSkip:   true,
			wantReason: skipReasonIndependent,
		},
		{
			name:     "a parent that opted in is routed, not skipped",
			bead:     poller.Bead{ID: "p-1", Labels: []string{"crucible"}, Blocks: []string{"c-1"}},
			wantSkip: false,
		},
		{
			name: "a parent carrying both labels is skipped: the opt-out wins there too",
			bead: poller.Bead{ID: "p-1", Labels: []string{"crucible", "independent"},
				Blocks: []string{"c-1"}},
			wantSkip:   true,
			wantReason: skipReasonIndependent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, skip := skipsEpicRouting(tt.bead, tt.resuming)
			assert.Equal(t, tt.wantSkip, skip)
			assert.Equal(t, tt.wantReason, reason)
		})
	}
}
