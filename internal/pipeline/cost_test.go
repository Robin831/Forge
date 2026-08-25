package pipeline

import (
	"testing"

	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/smith"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// costColumns is one row of the four token columns the three cost tables share.
type costColumns struct {
	input      int
	output     int
	cacheRead  int
	cacheWrite int
}

func readDailyCost(t *testing.T, db *state.DB, date string) costColumns {
	t.Helper()
	var c costColumns
	err := db.Conn().QueryRow(
		`SELECT input_tokens, output_tokens, cache_read, cache_write FROM daily_costs WHERE date = ?`,
		date).Scan(&c.input, &c.output, &c.cacheRead, &c.cacheWrite)
	require.NoError(t, err)
	return c
}

func readProviderDailyCost(t *testing.T, db *state.DB, date, prov string) costColumns {
	t.Helper()
	var c costColumns
	err := db.Conn().QueryRow(
		`SELECT input_tokens, output_tokens, cache_read, cache_write FROM provider_daily_costs WHERE date = ? AND provider = ?`,
		date, prov).Scan(&c.input, &c.output, &c.cacheRead, &c.cacheWrite)
	require.NoError(t, err)
	return c
}

func readBeadCost(t *testing.T, db *state.DB, beadID, anvil string) costColumns {
	t.Helper()
	var c costColumns
	err := db.Conn().QueryRow(
		`SELECT input_tokens, output_tokens, cache_read, cache_write FROM bead_costs WHERE bead_id = ? AND anvil = ?`,
		beadID, anvil).Scan(&c.input, &c.output, &c.cacheRead, &c.cacheWrite)
	require.NoError(t, err)
	return c
}

// TestRecordSpawnCost_PersistsCacheTokens pins the mapping the three tables
// share: cache_read holds what the session read back out of the prompt cache
// and cache_write what it paid to put there. The four counts are deliberately
// distinct so a swapped pair — the regression this test exists for — cannot
// pass, and so is the one that reads the columns back rather than trusting the
// arguments.
func TestRecordSpawnCost_PersistsCacheTokens(t *testing.T) {
	db := newTestDB(t)
	today := cost.Today()

	recordSpawnCost(db, "forge-Forge-wvb6-1", "Forge-wvb6", "forge", "claude", &smith.Result{
		TokensIn:            100,
		TokensOut:           50,
		CacheReadTokens:     41500,
		CacheCreationTokens: 900,
		CostUSD:             0.25,
	})

	want := costColumns{input: 100, output: 50, cacheRead: 41500, cacheWrite: 900}
	assert.Equal(t, want, readDailyCost(t, db, today), "daily_costs")
	assert.Equal(t, want, readProviderDailyCost(t, db, today, "claude"), "provider_daily_costs")
	assert.Equal(t, want, readBeadCost(t, db, "Forge-wvb6", "forge"), "bead_costs")
}

// TestRecordSpawnCost_AccumulatesCacheTokens covers the second spawn of the
// same bead: every one of these tables is an upsert that adds to what is
// already there, so the cache columns have to accumulate exactly like the
// input/output ones rather than overwrite.
func TestRecordSpawnCost_AccumulatesCacheTokens(t *testing.T) {
	db := newTestDB(t)
	today := cost.Today()

	r := &smith.Result{TokensIn: 10, TokensOut: 5, CacheReadTokens: 700, CacheCreationTokens: 30}
	recordSpawnCost(db, "forge-Forge-wvb6-1", "Forge-wvb6", "forge", "claude", r)
	recordSpawnCost(db, "forge-Forge-wvb6-1", "Forge-wvb6", "forge", "claude", r)

	want := costColumns{input: 20, output: 10, cacheRead: 1400, cacheWrite: 60}
	assert.Equal(t, want, readDailyCost(t, db, today), "daily_costs")
	assert.Equal(t, want, readProviderDailyCost(t, db, today, "claude"), "provider_daily_costs")
	assert.Equal(t, want, readBeadCost(t, db, "Forge-wvb6", "forge"), "bead_costs")
}

// TestRecordSpawnCost_CacheOnlySessionRecorded covers a session served almost
// entirely from cache: input/output/cost can all round to nothing while tens of
// thousands of tokens were read from the prefix. The old gate looked only at
// input/output/cost, so such a session recorded nothing at all.
func TestRecordSpawnCost_CacheOnlySessionRecorded(t *testing.T) {
	db := newTestDB(t)

	recordSpawnCost(db, "forge-Forge-wvb6-1", "Forge-wvb6", "forge", "claude", &smith.Result{CacheReadTokens: 32000})

	got := readBeadCost(t, db, "Forge-wvb6", "forge")
	assert.Equal(t, costColumns{cacheRead: 32000}, got)
}

// TestRecordSpawnCost_SkipsRateLimited keeps a refused request off the books:
// it is not a completion, and it never was recorded before either.
func TestRecordSpawnCost_SkipsRateLimited(t *testing.T) {
	db := newTestDB(t)

	recordSpawnCost(db, "forge-Forge-wvb6-1", "Forge-wvb6", "forge", "claude", &smith.Result{
		TokensIn:        100,
		CacheReadTokens: 5000,
		RateLimited:     true,
	})

	var rows int
	require.NoError(t, db.Conn().QueryRow(`SELECT COUNT(*) FROM bead_costs`).Scan(&rows))
	assert.Zero(t, rows)
}
