package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/lifecycle"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prTargetDB opens a throwaway state DB holding two PRs that share a number
// across anvils, so a number-scoped lookup that ignored the anvil would fail.
// alpha's PR is a crucible child based on a feature branch rather than main —
// the case where a wrongly-resolved row costs the branch, not just precision.
func prTargetDB(t *testing.T) (*state.DB, *state.PR, *state.PR) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	require.NoError(t, db.InsertPR(&state.PR{
		Number: 42, Anvil: "alpha", BeadID: "BD-A", Branch: "forge/BD-A",
		BaseBranch: "feature/BD-PARENT", Status: state.PROpen, CreatedAt: time.Now(),
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

	// The id wins outright: a supplied id that names no row must error rather
	// than quietly resolve through the accompanying number, which can name a
	// different PR entirely. This is the whole point of the preference.
	t.Run("a missing id does not fall back to the number", func(t *testing.T) {
		pr, err := resolvePRTargetPreferID(db, 99999, 42, "alpha")
		require.Error(t, err)
		assert.Nil(t, pr)
		assert.Contains(t, err.Error(), "not found")
	})

	// Same property for an id owned by another anvil: the ownership refusal
	// stands instead of degrading into the number lookup on the named anvil.
	t.Run("a cross-anvil id does not fall back to the number", func(t *testing.T) {
		pr, err := resolvePRTargetPreferID(db, alpha.ID, 42, "beta")
		require.Error(t, err)
		assert.Nil(t, pr)
		assert.Contains(t, err.Error(), "belongs to anvil")
	})
}

// rebaseTestDaemon wires a daemon over the two-PR fixture DB with the lifecycle
// dispatch replaced, so a pr_action rebase can be observed as the request it
// builds instead of spawning a worker.
func rebaseTestDaemon(t *testing.T, db *state.DB) (*Daemon, chan lifecycle.ActionRequest) {
	t.Helper()
	dispatched := make(chan lifecycle.ActionRequest, 4)
	d := &Daemon{
		db:         db,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		runCtx:     context.Background(),
		reqTracker: *ipc.NewRequestTracker("test-"),
		lifecycleDispatch: func(_ context.Context, req lifecycle.ActionRequest) {
			dispatched <- req
		},
	}
	d.cfg.Store(&config.Config{
		Anvils: map[string]config.AnvilConfig{
			"alpha": {Path: t.TempDir()},
			"beta":  {Path: t.TempDir()},
		},
	})
	return d, dispatched
}

func rebaseAction(pa ipc.PRActionPayload) ipc.Command {
	pa.Action = "rebase"
	payload, _ := json.Marshal(pa)
	return ipc.Command{Type: "pr_action", Payload: payload}
}

// waitDispatch returns the dispatched lifecycle request, failing rather than
// hanging if the handler never sends one.
func waitDispatch(t *testing.T, dispatched <-chan lifecycle.ActionRequest) lifecycle.ActionRequest {
	t.Helper()
	select {
	case req := <-dispatched:
		return req
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the lifecycle action to be dispatched")
		return lifecycle.ActionRequest{}
	}
}

// assertNoDispatch fails if a lifecycle request lands on the channel. The
// dispatch is a goroutine, so a refusal is only proven by waiting a moment for
// one that never comes — an immediate len() check would pass even against the
// old warn-and-continue.
func assertNoDispatch(t *testing.T, dispatched <-chan lifecycle.ActionRequest, why string) {
	t.Helper()
	select {
	case req := <-dispatched:
		t.Fatalf("%s: dispatched %v on %q (base %q)", why, req.Action, req.Branch, req.BaseBranch)
	case <-time.After(100 * time.Millisecond):
	}
}

func errMessage(t *testing.T, resp ipc.Response) string {
	t.Helper()
	var msg map[string]string
	require.NoError(t, json.Unmarshal(resp.Payload, &msg))
	return msg["message"]
}

// TestHandleIPC_PRActionRebaseBaseBranch pins the rebase handler's PR
// resolution — the half that decides what the branch is rewritten onto.
// rebase.Rebase reads an empty BaseBranch as "main" and force-pushes, so a
// resolution that silently produced no base would rebase a crucible child PR
// (based on feature/<parent-id>) onto main and destroy its head.
func TestHandleIPC_PRActionRebaseBaseBranch(t *testing.T) {
	db, alpha, beta := prTargetDB(t)

	t.Run("falls back to the PR number when the caller has no row id", func(t *testing.T) {
		d, dispatched := rebaseTestDaemon(t, db)
		resp := d.handleIPC(rebaseAction(ipc.PRActionPayload{
			PRNumber: 42, Anvil: "alpha", BeadID: "BD-A", Branch: "forge/BD-A",
		}))
		require.Equal(t, "ok", resp.Type, errMessage(t, resp))

		req := waitDispatch(t, dispatched)
		assert.Equal(t, lifecycle.ActionRebase, req.Action)
		assert.Equal(t, "forge/BD-A", req.Branch)
		assert.Equal(t, "feature/BD-PARENT", req.BaseBranch,
			"an ext PR known by number alone must still rebase onto its real base")
	})

	t.Run("the row id wins when both are sent", func(t *testing.T) {
		d, dispatched := rebaseTestDaemon(t, db)
		resp := d.handleIPC(rebaseAction(ipc.PRActionPayload{
			PRID: beta.ID, PRNumber: 42, Anvil: "beta", BeadID: "BD-B", Branch: "forge/BD-B",
		}))
		require.Equal(t, "ok", resp.Type, errMessage(t, resp))

		req := waitDispatch(t, dispatched)
		assert.Equal(t, "main", req.BaseBranch)
	})

	t.Run("an unresolvable PR is refused, not dispatched", func(t *testing.T) {
		d, dispatched := rebaseTestDaemon(t, db)
		resp := d.handleIPC(rebaseAction(ipc.PRActionPayload{
			PRNumber: 777, Anvil: "alpha", BeadID: "BD-A", Branch: "forge/BD-A",
		}))
		require.Equal(t, "error", resp.Type)
		assert.Contains(t, errMessage(t, resp), "not found")
		assertNoDispatch(t, dispatched, "a rebase with no known base must not run")
	})

	t.Run("a cross-anvil row id is refused, not dispatched", func(t *testing.T) {
		d, dispatched := rebaseTestDaemon(t, db)
		resp := d.handleIPC(rebaseAction(ipc.PRActionPayload{
			PRID: alpha.ID, PRNumber: 42, Anvil: "beta", BeadID: "BD-B", Branch: "forge/BD-B",
		}))
		require.Equal(t, "error", resp.Type)
		assert.Contains(t, errMessage(t, resp), "belongs to anvil")
		assertNoDispatch(t, dispatched, "the ownership refusal is not advisory here")
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
