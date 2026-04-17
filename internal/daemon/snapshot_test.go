package daemon

import (
	"testing"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// taggedAnvilCfg builds a config with one anvil that uses tagged auto-dispatch
// against the auto_dispatch_tag. This mirrors the production deployment that
// surfaced the Forge-1soq flicker.
func taggedAnvilCfg(name, tag string) *config.Config {
	return &config.Config{
		Anvils: map[string]config.AnvilConfig{
			name: {
				AutoDispatch:    "tagged",
				AutoDispatchTag: tag,
			},
		},
	}
}

func bead(id, anvil string, labels ...string) poller.Bead {
	return poller.Bead{ID: id, Anvil: anvil, Title: id, Labels: labels}
}

func successResult(name string, beads []poller.Bead) poller.AnvilResult {
	return poller.AnvilResult{Name: name, Beads: beads}
}

// collectIDs returns just the IDs of the merged snapshot, sorted as the merge
// already sorts (priority then id). Useful for set-style comparisons.
func collectIDs(beads []poller.Bead) []string {
	out := make([]string, 0, len(beads))
	for _, b := range beads {
		out = append(out, b.ID)
	}
	return out
}

// TestSnapshot_FastPollRetainsUnlabeledFromSlowPoll is the regression test for
// Forge-1soq: a fast (label-filtered) poll must not evict beads that a prior
// slow poll surfaced unlabeled.
func TestSnapshot_FastPollRetainsUnlabeledFromSlowPoll(t *testing.T) {
	cfg := taggedAnvilCfg("anvil-a", "forgeReady")
	d := &Daemon{}

	// Slow poll returns one labeled and one unlabeled bead.
	slowBeads := []poller.Bead{
		bead("LBL-1", "anvil-a", "forgeReady"),
		bead("UNL-1", "anvil-a"),
	}
	d.updateBeadSnapshot(cfg, slowBeads, []poller.AnvilResult{successResult("anvil-a", slowBeads)}, false)

	got := d.mergedBeadSnapshotForAnvils([]string{"anvil-a"})
	require.ElementsMatch(t, []string{"LBL-1", "UNL-1"}, collectIDs(got),
		"slow poll should populate both labeled and unlabeled maps")

	// Fast poll returns only the labeled bead.
	fastBeads := []poller.Bead{bead("LBL-1", "anvil-a", "forgeReady")}
	d.updateBeadSnapshot(cfg, fastBeads, []poller.AnvilResult{successResult("anvil-a", fastBeads)}, true)

	got = d.mergedBeadSnapshotForAnvils([]string{"anvil-a"})
	require.ElementsMatch(t, []string{"LBL-1", "UNL-1"}, collectIDs(got),
		"fast poll must NOT evict the unlabeled bead surfaced by the prior slow poll")
}

// TestSnapshot_SlowPollEvictsMissingUnlabeled verifies that an unlabeled bead
// disappears from the snapshot only after a confirming slow poll where it is
// no longer present in the unfiltered set.
func TestSnapshot_SlowPollEvictsMissingUnlabeled(t *testing.T) {
	cfg := taggedAnvilCfg("anvil-a", "forgeReady")
	d := &Daemon{}

	// First slow poll: labeled + unlabeled bead present.
	first := []poller.Bead{
		bead("LBL-1", "anvil-a", "forgeReady"),
		bead("UNL-1", "anvil-a"),
	}
	d.updateBeadSnapshot(cfg, first, []poller.AnvilResult{successResult("anvil-a", first)}, false)

	// Fast poll: only labeled — unlabeled must remain visible.
	fast := []poller.Bead{bead("LBL-1", "anvil-a", "forgeReady")}
	d.updateBeadSnapshot(cfg, fast, []poller.AnvilResult{successResult("anvil-a", fast)}, true)
	require.Contains(t, collectIDs(d.mergedBeadSnapshotForAnvils([]string{"anvil-a"})), "UNL-1",
		"unlabeled bead must survive a fast poll")

	// Second slow poll: UNL-1 has transitioned out (closed / claimed / blocked)
	// and is no longer in the unfiltered result. It must be evicted now.
	second := []poller.Bead{bead("LBL-1", "anvil-a", "forgeReady")}
	d.updateBeadSnapshot(cfg, second, []poller.AnvilResult{successResult("anvil-a", second)}, false)

	got := collectIDs(d.mergedBeadSnapshotForAnvils([]string{"anvil-a"}))
	require.ElementsMatch(t, []string{"LBL-1"}, got,
		"slow poll must evict unlabeled beads that no longer appear in the unfiltered result")
}

// TestSnapshot_LabeledTakesPrecedenceOnCollision verifies that when a bead
// appears in both maps (e.g. it gained the label between slow and fast polls),
// the merged view emits it once with the labeled-map data.
func TestSnapshot_LabeledTakesPrecedenceOnCollision(t *testing.T) {
	cfg := taggedAnvilCfg("anvil-a", "forgeReady")
	d := &Daemon{}

	// Slow poll: bead is unlabeled. Title intentionally differs so we can
	// detect which map the merged entry came from.
	slow := []poller.Bead{
		{ID: "BD-1", Anvil: "anvil-a", Title: "old-unlabeled"},
	}
	d.updateBeadSnapshot(cfg, slow, []poller.AnvilResult{successResult("anvil-a", slow)}, false)

	// Fast poll: same bead now carries the tag with a fresher title.
	fast := []poller.Bead{
		{ID: "BD-1", Anvil: "anvil-a", Title: "fresh-labeled", Labels: []string{"forgeReady"}},
	}
	d.updateBeadSnapshot(cfg, fast, []poller.AnvilResult{successResult("anvil-a", fast)}, true)

	got := d.mergedBeadSnapshotForAnvils([]string{"anvil-a"})
	require.Len(t, got, 1, "bead must not appear twice")
	assert.Equal(t, "fresh-labeled", got[0].Title, "labeled map should win on collision")
}

// TestSnapshot_FailedAnvilPollLeavesSnapshotUntouched verifies that a transient
// bd error does not wipe the cached snapshot for that anvil.
func TestSnapshot_FailedAnvilPollLeavesSnapshotUntouched(t *testing.T) {
	cfg := taggedAnvilCfg("anvil-a", "forgeReady")
	d := &Daemon{}

	// Seed a snapshot.
	seed := []poller.Bead{
		bead("LBL-1", "anvil-a", "forgeReady"),
		bead("UNL-1", "anvil-a"),
	}
	d.updateBeadSnapshot(cfg, seed, []poller.AnvilResult{successResult("anvil-a", seed)}, false)

	// Simulate a failing poll (no beads, error result).
	failed := poller.AnvilResult{Name: "anvil-a", Err: assertErrTransient{}}
	d.updateBeadSnapshot(cfg, nil, []poller.AnvilResult{failed}, true)

	got := d.mergedBeadSnapshotForAnvils([]string{"anvil-a"})
	assert.ElementsMatch(t, []string{"LBL-1", "UNL-1"}, collectIDs(got),
		"failed poll must not wipe snapshot for the affected anvil")
}

// TestSnapshot_NoTagAnvilUsesLabeledMapOnly verifies that anvils without an
// AutoDispatchTag are always treated as fully refreshed (since their polls
// are never label-filtered), so the unlabeled map stays empty for them.
func TestSnapshot_NoTagAnvilUsesLabeledMapOnly(t *testing.T) {
	cfg := &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"anvil-untagged": {
				AutoDispatch: "all",
				// No AutoDispatchTag.
			},
		},
	}
	d := &Daemon{}

	beads := []poller.Bead{
		bead("BD-1", "anvil-untagged"),
		bead("BD-2", "anvil-untagged", "forgeReady"),
	}
	// Even when fastPoll=true, the per-anvil filtered flag is false because
	// the poller does not apply --label without a tag. Both beads should land
	// in the labeled map and the snapshot should stay consistent.
	d.updateBeadSnapshot(cfg, beads, []poller.AnvilResult{successResult("anvil-untagged", beads)}, true)

	got := d.mergedBeadSnapshotForAnvils([]string{"anvil-untagged"})
	require.ElementsMatch(t, []string{"BD-1", "BD-2"}, collectIDs(got))

	d.snapshotMu.RLock()
	defer d.snapshotMu.RUnlock()
	assert.Empty(t, d.unlabeledSnapshot["anvil-untagged"],
		"unlabeled map must stay empty for anvils without an auto_dispatch_tag")
	assert.Len(t, d.labeledSnapshot["anvil-untagged"], 2,
		"both beads should be tracked in the labeled map")
}

// TestSnapshot_MergeAcrossAnvils verifies that the mergedBeadSnapshot helper
// returns beads across multiple anvils, deterministically ordered.
func TestSnapshot_MergeAcrossAnvils(t *testing.T) {
	cfg := &config.Config{
		Anvils: map[string]config.AnvilConfig{
			"anvil-a": {AutoDispatch: "tagged", AutoDispatchTag: "forgeReady"},
			"anvil-b": {AutoDispatch: "tagged", AutoDispatchTag: "forgeReady"},
		},
	}
	d := &Daemon{}
	d.cfg.Store(cfg)

	all := []poller.Bead{
		{ID: "A-1", Anvil: "anvil-a", Priority: 1, Labels: []string{"forgeReady"}},
		{ID: "A-2", Anvil: "anvil-a", Priority: 3},
		{ID: "B-1", Anvil: "anvil-b", Priority: 2, Labels: []string{"forgeReady"}},
	}
	results := []poller.AnvilResult{
		successResult("anvil-a", []poller.Bead{all[0], all[1]}),
		successResult("anvil-b", []poller.Bead{all[2]}),
	}
	d.updateBeadSnapshot(cfg, all, results, false)

	got := d.mergedBeadSnapshot()
	// Sorted by priority, then ID.
	require.Equal(t, []string{"A-1", "B-1", "A-2"}, collectIDs(got))
}

// assertErrTransient is a sentinel error used in the failed-poll test.
type assertErrTransient struct{}

func (assertErrTransient) Error() string { return "transient" }
