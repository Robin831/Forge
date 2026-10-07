package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/bellows"
	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newReconcileDaemon(t *testing.T, forgeID string, mock *mockVCSProvider) *Daemon {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	d := &Daemon{
		db:           db,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		vcsProviders: map[string]vcs.Provider{"shared": mock},
	}
	d.cfg.Store(&config.Config{
		Anvils:   map[string]config.AnvilConfig{"shared": {Path: dir}},
		Settings: config.SettingsConfig{ForgeID: forgeID},
	})
	return d
}

func polledBy(t *testing.T, d *Daemon) []int {
	t.Helper()
	prs, err := d.db.OpenPRs()
	require.NoError(t, err)
	var out []int
	for i := range prs {
		if bellows.OwnsPR(&prs[i]) {
			out = append(out, prs[i].Number)
		}
	}
	sort.Ints(out)
	return out
}

// TestTwoForges_EachPRPolledByExactlyOne: two forges reconcile the same repo.
// Branch names cannot tell them apart (both use forge/<bead>); the
// per-instance marker can, and bellows polls exactly what reconcile marked as
// owned — so every forge PR is polled by one daemon and humans' by none.
func TestTwoForges_EachPRPolledByExactlyOne(t *testing.T) {
	prevID := vcs.ForgeID()
	defer vcs.SetForgeID(prevID)

	listing := []vcs.OpenPR{
		{Number: 1, Branch: "forge/Forge-a1", Body: "Bead: Forge-a1 | Branch: forge/Forge-a1\n" + vcs.MarkerForID("forge")},
		{Number: 2, Branch: "forge/Forge-a2", Body: "Bead: Forge-a2 | Branch: forge/Forge-a2\n" + vcs.MarkerForID("forge")},
		{Number: 3, Branch: "forge/Forge-b1", Body: "Bead: Forge-b1 | Branch: forge/Forge-b1\n" + vcs.MarkerForID("forge-sophie")},
		{Number: 4, Branch: "forge/Forge-b2", Body: "Bead: Forge-b2 | Branch: forge/Forge-b2\n" + vcs.MarkerForID("forge-sophie")},
		{Number: 5, Branch: "alice/fix", Body: "Fixes it.\n\n**Bead**: Forge-a1"},
		{Number: 6, Branch: "legacy", Body: "Bead: Forge-old\n<!-- forge-managed: true -->"},
	}
	mock := &mockVCSProvider{openPRs: listing}
	robin := newReconcileDaemon(t, "forge", mock)
	sophie := newReconcileDaemon(t, "forge-sophie", mock)

	vcs.SetForgeID("forge")
	robin.reconcileOpenPRs(context.Background())
	vcs.SetForgeID("forge-sophie")
	sophie.reconcileOpenPRs(context.Background())

	assert.Equal(t, []int{1, 2}, polledBy(t, robin))
	assert.Equal(t, []int{3, 4}, polledBy(t, sophie))
}

// Unowned rows are no longer polled by bellows, so reconcile settles them once
// they leave the open listing: one light call, written only for a terminal
// answer. Owned rows are left to bellows; a full listing proves nothing.
func TestReconcile_SettlesClosedUnownedPRs(t *testing.T) {
	prevID := vcs.ForgeID()
	defer vcs.SetForgeID(prevID)
	vcs.SetForgeID("forge")

	mock := &mockVCSProvider{lightStatus: &vcs.PRStatus{State: "MERGED"}}
	d := newReconcileDaemon(t, "forge", mock)
	insert := func(n int, bead string) *state.PR {
		pr := &state.PR{Number: n, Anvil: "shared", BeadID: bead, Branch: "b", Status: state.PROpen, CreatedAt: time.Now()}
		require.NoError(t, d.db.InsertPR(pr))
		return pr
	}
	human := insert(7, "ext-7")
	own := insert(8, "Forge-own")
	stillOpen := insert(9, "ext-9")
	mock.openPRs = []vcs.OpenPR{{Number: 9, Branch: "b", Body: "x"}}

	d.reconcileOpenPRs(context.Background())

	got, err := d.db.GetPRByID(human.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PRMerged, got.Status, "an unowned PR gone from the listing is settled")
	got, err = d.db.GetPRByID(own.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PROpen, got.Status, "an owned PR is bellows' to settle")
	got, err = d.db.GetPRByID(stillOpen.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PROpen, got.Status)

	// A listing at gh's --limit may be truncated: nothing is settled from it.
	reopened := insert(10, "ext-10")
	mock.openPRs = nil
	for n := 1000; n < 1000+openPRListLimit; n++ {
		mock.openPRs = append(mock.openPRs, vcs.OpenPR{Number: n, Branch: "b", Body: "x"})
	}
	d.reconcileOpenPRs(context.Background())
	got, err = d.db.GetPRByID(reopened.ID)
	require.NoError(t, err)
	assert.Equal(t, state.PROpen, got.Status)
}
