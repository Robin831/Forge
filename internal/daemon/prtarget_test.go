package daemon

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prTargetDB opens a throwaway state DB holding two PRs that share a number
// across anvils, so a number-scoped lookup that ignored the anvil would fail.
func prTargetDB(t *testing.T) (*state.DB, *state.PR, *state.PR) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	require.NoError(t, db.InsertPR(&state.PR{
		Number: 42, Anvil: "alpha", BeadID: "BD-A", Branch: "forge/BD-A",
		BaseBranch: "main", Status: state.PROpen, CreatedAt: time.Now(),
	}))
	require.NoError(t, db.InsertPR(&state.PR{
		Number: 42, Anvil: "beta", BeadID: "BD-B", Branch: "forge/BD-B",
		BaseBranch: "main", Status: state.PROpen, CreatedAt: time.Now(),
	}))

	alpha, err := db.GetPRByNumber("alpha", 42)
	require.NoError(t, err)
	beta, err := db.GetPRByNumber("beta", 42)
	require.NoError(t, err)
	return db, alpha, beta
}

func TestResolvePRTarget(t *testing.T) {
	db, alpha, beta := prTargetDB(t)

	t.Run("by row id", func(t *testing.T) {
		pr, err := resolvePRTarget(db, beta.ID, 0, "beta")
		require.NoError(t, err)
		assert.Equal(t, beta.ID, pr.ID)
		assert.Equal(t, "BD-B", pr.BeadID)
	})

	t.Run("by row id without an anvil", func(t *testing.T) {
		pr, err := resolvePRTarget(db, alpha.ID, 0, "")
		require.NoError(t, err)
		assert.Equal(t, alpha.ID, pr.ID)
	})

	t.Run("by number scoped to its anvil", func(t *testing.T) {
		pr, err := resolvePRTarget(db, 0, 42, "beta")
		require.NoError(t, err)
		assert.Equal(t, beta.ID, pr.ID, "the number is per-anvil, not global")
	})

	t.Run("both forms is ambiguous", func(t *testing.T) {
		_, err := resolvePRTarget(db, alpha.ID, 42, "alpha")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not both")
	})

	t.Run("neither form", func(t *testing.T) {
		_, err := resolvePRTarget(db, 0, 0, "alpha")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "PR target is required")
	})

	t.Run("number without an anvil", func(t *testing.T) {
		_, err := resolvePRTarget(db, 0, 42, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires an anvil")
	})

	t.Run("unknown row id", func(t *testing.T) {
		_, err := resolvePRTarget(db, 99999, 0, "alpha")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("unknown number on a known anvil", func(t *testing.T) {
		_, err := resolvePRTarget(db, 0, 777, "alpha")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("row id from another anvil", func(t *testing.T) {
		_, err := resolvePRTarget(db, alpha.ID, 0, "beta")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "belongs to anvil")
	})
}

func TestResolvePRTargetPreferID(t *testing.T) {
	db, alpha, beta := prTargetDB(t)

	t.Run("both supplied is not ambiguous here", func(t *testing.T) {
		pr, err := resolvePRTargetPreferID(db, beta.ID, 42, "beta")
		require.NoError(t, err)
		assert.Equal(t, beta.ID, pr.ID)
	})

	t.Run("falls back to the number when no id is known", func(t *testing.T) {
		pr, err := resolvePRTargetPreferID(db, 0, 42, "alpha")
		require.NoError(t, err)
		assert.Equal(t, alpha.ID, pr.ID)
	})

	t.Run("neither form still errors", func(t *testing.T) {
		_, err := resolvePRTargetPreferID(db, 0, 0, "alpha")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "PR target is required")
	})
}

// TestAssayRerunPayloadCompat pins the wire shape: a legacy client sending only
// {"anvil","pr"} must still deserialize into the row-id form, with no PR number
// implied.
func TestAssayRerunPayloadCompat(t *testing.T) {
	var p ipc.AssayRerunPayload
	require.NoError(t, json.Unmarshal([]byte(`{"anvil":"heimdall","pr":12}`), &p))
	assert.Equal(t, "heimdall", p.Anvil)
	assert.Equal(t, 12, p.PR)
	assert.Equal(t, 0, p.PRNumber)

	var q ipc.AssayRerunPayload
	require.NoError(t, json.Unmarshal([]byte(`{"anvil":"heimdall","pr_number":431}`), &q))
	assert.Equal(t, 431, q.PRNumber)
	assert.Equal(t, 0, q.PR)

	round, err := json.Marshal(ipc.AssayRerunPayload{Anvil: "heimdall", PR: 12})
	require.NoError(t, err)
	assert.JSONEq(t, `{"anvil":"heimdall","pr":12}`, string(round))
}
