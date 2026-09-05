package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/selfdeploy"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/require"
)

// TestSelfDeployAttentionSink_RaisesAndResolves covers the daemon-side adapter
// end to end: a rollback reported by the deployer becomes a needs-attention row
// with both builds, and a later successful deploy clears it.
func TestSelfDeployAttentionSink_RaisesAndResolves(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	sink := selfDeployAttentionSink{db: db, anvil: "forge", unit: "forge"}
	failedAt := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	require.NoError(t, sink.EmitNeedsAttention(selfdeploy.DeployEvent{
		Reason:       selfdeploy.ReasonRestartFailed,
		AttemptedSHA: "cafebabe0123456789abcdef",
		RestoredSHA:  "deadbeef9876543210fedcba",
		RolledBack:   true,
		Detail:       "restart failed: signal: killed",
		BinaryPath:   "/home/robin/bin/forge",
		Timestamp:    failedAt,
	}))

	items, err := db.NeedsAttentionBeads(5, 5, 3, state.StalenessParams{})
	require.NoError(t, err)
	var found *state.NeedsAttentionBead
	for i := range items {
		if items[i].Kind == state.AttentionKindDeploy {
			found = &items[i]
		}
	}
	require.NotNil(t, found, "the rollback must surface in needs-attention")
	require.Equal(t, "forge", found.Anvil)
	require.Contains(t, found.Title, "rolled back")
	require.Contains(t, found.Reason, "attempted cafebabe0123")
	require.Contains(t, found.Reason, "restored deadbeef9876")
	require.Contains(t, found.Reason, "2026-08-06T12:00:00Z")

	// An unqualified clear is what a successful deploy issues.
	require.NoError(t, sink.ClearNeedsAttention())
	items, err = db.NeedsAttentionBeads(5, 5, 3, state.StalenessParams{})
	require.NoError(t, err)
	for _, it := range items {
		require.NotEqual(t, state.AttentionKindDeploy, it.Kind, "the entry must clear on a later successful deploy")
	}
}

// TestSelfDeployAttentionSink_ClearsOnlyNamedReasons keeps a deferral from
// resolving the record of a rollback, which describes a different problem.
func TestSelfDeployAttentionSink_ClearsOnlyNamedReasons(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	sink := selfDeployAttentionSink{db: db, anvil: "forge", unit: "forge"}
	require.NoError(t, sink.EmitNeedsAttention(selfdeploy.DeployEvent{
		Reason: selfdeploy.ReasonRestartFailed, RolledBack: true, Detail: "restart failed",
	}))
	require.NoError(t, sink.EmitNeedsAttention(selfdeploy.DeployEvent{
		Reason: selfdeploy.ReasonDrainTimeout, Detail: "workers busy",
	}))

	require.NoError(t, sink.ClearNeedsAttention(selfdeploy.ReasonDrainTimeout))

	rows, err := db.DeployFailures()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, state.DeployReasonRestartFailed, rows[0].Reason)
}
