package pipeline

import (
	"testing"

	"github.com/Robin831/Forge/internal/cost"
	"github.com/Robin831/Forge/internal/poller"
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

// costParams is a pipeline whose only wired dependency is the DB — enough for
// recordStageCost, which touches nothing else.
func costParams(db *state.DB) Params {
	return Params{DB: db, Bead: poller.Bead{ID: "Forge-wvb6"}, AnvilName: "forge"}
}

// recordSmith is the smith call site in miniature: a finished session projected
// through Result.Usage and handed to the one per-stage recorder.
func recordSmith(db *state.DB, r *smith.Result) {
	p := costParams(db)
	p.recordStageCost("forge-Forge-wvb6-1", "smith", "claude", r.Usage())
}

// TestRecordStageCost_PersistsCacheTokens pins the mapping the three tables
// share: cache_read holds what the session read back out of the prompt cache
// and cache_write what it paid to put there. The four counts are deliberately
// distinct so a swapped pair — the regression this test exists for — cannot
// pass, and so is the one that reads the columns back rather than trusting the
// arguments.
func TestRecordStageCost_PersistsCacheTokens(t *testing.T) {
	db := newTestDB(t)
	today := cost.Today()

	recordSmith(db, &smith.Result{
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

// TestRecordStageCost_AccumulatesCacheTokens covers the second spawn of the
// same bead: every one of these tables is an upsert that adds to what is
// already there, so the cache columns have to accumulate exactly like the
// input/output ones rather than overwrite.
func TestRecordStageCost_AccumulatesCacheTokens(t *testing.T) {
	db := newTestDB(t)
	today := cost.Today()

	r := &smith.Result{TokensIn: 10, TokensOut: 5, CacheReadTokens: 700, CacheCreationTokens: 30}
	recordSmith(db, r)
	recordSmith(db, r)

	want := costColumns{input: 20, output: 10, cacheRead: 1400, cacheWrite: 60}
	assert.Equal(t, want, readDailyCost(t, db, today), "daily_costs")
	assert.Equal(t, want, readProviderDailyCost(t, db, today, "claude"), "provider_daily_costs")
	assert.Equal(t, want, readBeadCost(t, db, "Forge-wvb6", "forge"), "bead_costs")
}

// TestRecordStageCost_CacheOnlySessionRecorded covers a session served almost
// entirely from cache: input/output/cost can all round to nothing while tens of
// thousands of tokens were read from the prefix. The old gate looked only at
// input/output/cost, so such a session recorded nothing at all.
func TestRecordStageCost_CacheOnlySessionRecorded(t *testing.T) {
	db := newTestDB(t)

	recordSmith(db, &smith.Result{CacheReadTokens: 32000})

	got := readBeadCost(t, db, "Forge-wvb6", "forge")
	assert.Equal(t, costColumns{cacheRead: 32000}, got)
}

// TestRecordStageCost_SkipsRateLimited keeps a refused request off the books:
// it is not a completion, and it never was recorded before either.
func TestRecordStageCost_SkipsRateLimited(t *testing.T) {
	db := newTestDB(t)

	recordSmith(db, &smith.Result{
		TokensIn:        100,
		CacheReadTokens: 5000,
		RateLimited:     true,
	})

	var rows int
	require.NoError(t, db.Conn().QueryRow(`SELECT COUNT(*) FROM bead_costs`).Scan(&rows))
	assert.Zero(t, rows)
}

// TestRecordStageCost_MultiStageRunSumsIntoOneBeadRow is the whole point of the
// helper: a bead's run spends money in more than one stage — a schematic
// pre-analysis, one or more Smith turns, a Warden review — and every one of
// them lands in the SAME bead_costs row, summed. Before the helper each stage
// recorded (or, for most of them, did not record) its own way, so a bead's row
// reported its Smith turns and nothing else.
//
// The cache columns are asserted alongside the token ones because they are the
// half that used to be a literal 0 at the call site: a stage that records its
// tokens while passing zeros for the cache is exactly the regression this test
// is here to fail on, and the per-stage numbers are distinct so no swap or
// dropped stage can add up to the same totals.
func TestRecordStageCost_MultiStageRunSumsIntoOneBeadRow(t *testing.T) {
	db := newTestDB(t)
	today := cost.Today()
	p := costParams(db)

	stages := []struct {
		name  string
		usage cost.Usage
	}{
		{"schematic", cost.Usage{InputTokens: 300, OutputTokens: 40, CacheReadTokens: 1200, CacheWriteTokens: 9000, EstimatedCostUSD: 0.11}},
		{"smith", cost.Usage{InputTokens: 5000, OutputTokens: 900, CacheReadTokens: 41500, CacheWriteTokens: 2300, EstimatedCostUSD: 1.25}},
		{"smith", cost.Usage{InputTokens: 2500, OutputTokens: 400, CacheReadTokens: 38000, CacheWriteTokens: 150, EstimatedCostUSD: 0.60}},
		{"warden", cost.Usage{InputTokens: 800, OutputTokens: 120, CacheReadTokens: 7000, CacheWriteTokens: 640, EstimatedCostUSD: 0.19}},
	}
	var want cost.Usage
	for _, s := range stages {
		p.recordStageCost("forge-Forge-wvb6-1", s.name, "claude", s.usage)
		want.Add(s.usage)
	}

	wantCols := costColumns{
		input:      want.InputTokens,
		output:     want.OutputTokens,
		cacheRead:  want.CacheReadTokens,
		cacheWrite: want.CacheWriteTokens,
	}
	assert.Equal(t, wantCols, readBeadCost(t, db, "Forge-wvb6", "forge"), "bead_costs")
	assert.Equal(t, wantCols, readDailyCost(t, db, today), "daily_costs")
	assert.Equal(t, wantCols, readProviderDailyCost(t, db, today, "claude"), "provider_daily_costs")
	assert.NotZero(t, wantCols.cacheRead)
	assert.NotZero(t, wantCols.cacheWrite)

	// One row, not one per stage: four recordings of the same bead upsert.
	var rows int
	require.NoError(t, db.Conn().QueryRow(
		`SELECT COUNT(*) FROM bead_costs WHERE bead_id = ? AND anvil = ?`, "Forge-wvb6", "forge").Scan(&rows))
	assert.Equal(t, 1, rows)

	var usd float64
	require.NoError(t, db.Conn().QueryRow(
		`SELECT estimated_cost FROM bead_costs WHERE bead_id = ? AND anvil = ?`, "Forge-wvb6", "forge").Scan(&usd))
	assert.InDelta(t, want.EstimatedCostUSD, usd, 1e-9)
}
