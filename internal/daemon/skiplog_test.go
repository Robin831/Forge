package daemon

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/poller"
	"github.com/Robin831/Forge/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DevInfra-jh9: a tagged ready bead counted as ready on every poll and never dispatched, without a line saying
// why. Each skip a tagged bead can meet now says so at INFO, once per reason.
const (
	taggedBead   = "Forge-tagged"
	untaggedBead = "Forge-untagged"
	skipTag      = "forgeSkybert"
)

func newSkipEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestDaemon(t, withWorkerLimit(2))
	e.reconfigure(func(c *config.Config) {
		c.Settings.CruciblePollInterval = time.Minute
		c.Anvils = map[string]config.AnvilConfig{
			testAnvil: {Path: e.dir, MaxSmiths: 1, AutoDispatch: "tagged", AutoDispatchTag: skipTag},
		}
	})
	stubBdOnPath(t, e.dir,
		`[{"id": "`+taggedBead+`", "title": "tagged", "status": "ready", "priority": 1, "labels": ["`+skipTag+`"]},`+
			` {"id": "`+untaggedBead+`", "title": "untagged", "status": "ready", "priority": 1}]`)
	t.Cleanup(func() { drainDispatch(t, e) })
	return e
}

// skipLines returns the "ready bead not dispatched" lines for one bead.
func skipLines(e *testEnv, beadID string) []logRecord {
	var out []logRecord
	for _, rec := range e.logs() {
		if rec.Level == slog.LevelInfo && rec.Message == "ready bead not dispatched" && rec.Attrs["bead"] == beadID {
			out = append(out, rec)
		}
	}
	return out
}

func TestSkipLog_AnvilCapNamesTheWorkerHoldingIt(t *testing.T) {
	e := newSkipEnv(t)
	holder := e.seedWorkerRow(workerRow{BeadID: "Forge-other", Status: state.WorkerRunning, Phase: "smith",
		StartedAt: time.Now(), Generation: e.generation(), Heartbeat: time.Now()})

	e.d.pollAndDispatch(e.ctx, false)
	require.False(t, dispatched(t, e, taggedBead), "the anvil's one slot is held")
	lines := skipLines(e, taggedBead)
	require.Len(t, lines, 1, "the skip is said")
	assert.Equal(t, "the anvil's max_smiths is reached", lines[0].Attrs["reason"])
	assert.Contains(t, lines[0].Attrs["held_by"], holder.ID+" (Forge-other, running)", "and who holds the slot")

	e.d.pollAndDispatch(e.ctx, false)
	assert.Len(t, skipLines(e, taggedBead), 1, "the same reason on the next poll is not said again")
	assert.Empty(t, skipLines(e, untaggedBead), "a bead the anvil would not auto-dispatch is not logged")
}

func TestSkipLog_ClarificationAndNeedsHumanAreSaid(t *testing.T) {
	e := newSkipEnv(t)
	require.NoError(t, e.db.SetClarificationNeeded(taggedBead, testAnvil, true, "unclear"))
	e.d.pollAndDispatch(e.ctx, false)
	require.False(t, dispatched(t, e, taggedBead))
	lines := skipLines(e, taggedBead)
	require.Len(t, lines, 1)
	assert.Equal(t, "clarification_needed is set in the forge's state", lines[0].Attrs["reason"])
	assert.Equal(t, "unclear", lines[0].Attrs["last_error"], "and what set the flag")

	// The reason changes: said again, with the new one.
	require.NoError(t, e.db.SetClarificationNeeded(taggedBead, testAnvil, false, ""))
	require.NoError(t, e.db.UpsertRetry(&state.RetryRecord{BeadID: taggedBead, Anvil: testAnvil, NeedsHuman: true}))
	e.d.pollAndDispatch(e.ctx, false)
	lines = skipLines(e, taggedBead)
	require.Len(t, lines, 2)
	assert.Equal(t, "needs_human is set in the forge's state", lines[1].Attrs["reason"])
	assert.NotEmpty(t, lines[1].Attrs["updated"], "it says when the flag's record was last updated")
	assert.NotEmpty(t, lines[1].Attrs["age"])
}

func TestSkipLog_ADispatchedBeadIsForgotten(t *testing.T) {
	e := newSkipEnv(t)
	require.NoError(t, e.db.SetClarificationNeeded(taggedBead, testAnvil, true, "unclear"))
	e.d.pollAndDispatch(e.ctx, false)
	require.Len(t, skipLines(e, taggedBead), 1)

	require.NoError(t, e.db.SetClarificationNeeded(taggedBead, testAnvil, false, ""))
	e.d.pollAndDispatch(e.ctx, false)
	require.True(t, dispatched(t, e, taggedBead), "once nothing stands in the way it is dispatched")
	_, remembered := e.d.skipNoted.Load(taggedBead + "\x00" + testAnvil)
	assert.False(t, remembered, "and its last skip reason is forgotten, so a later skip is said again")
}

func TestSkipLog_AFullGlobalLimitIsAlreadySaid(t *testing.T) {
	e := newSkipEnv(t)
	e.reconfigure(func(c *config.Config) {
		c.Settings.MaxTotalSmiths = 1
		c.Anvils = map[string]config.AnvilConfig{
			testAnvil:   {Path: e.dir, MaxSmiths: 1, AutoDispatch: "tagged", AutoDispatchTag: skipTag},
			"elsewhere": {Path: e.dir, MaxSmiths: 1, AutoDispatch: "all"},
		}
	})
	e.seedWorkerRow(workerRow{BeadID: "Forge-elsewhere", Anvil: "elsewhere", Status: state.WorkerRunning, Phase: "smith",
		StartedAt: time.Now(), Generation: e.generation(), Heartbeat: time.Now()})
	e.d.pollAndDispatch(e.ctx, false)
	require.False(t, dispatched(t, e, taggedBead), "the one global slot is held by another anvil")
	// A global limit that is full before the loop already has its own INFO line, written before any bead is looked
	// at; the per-bead line covers the cap filling up inside one cycle.
	assert.True(t, e.hasLog(slog.LevelInfo, "global smith limit reached, skipping dispatch"))
}

func TestFlagAttrs_LongErrorIsCutOnARuneAndFlattened(t *testing.T) {
	e := newSkipEnv(t)
	long := "første linje\n" + strings.Repeat("ø", 300)
	require.NoError(t, e.db.SetClarificationNeeded(taggedBead, testAnvil, true, long))
	attrs := e.d.flagAttrs(poller.Bead{ID: taggedBead, Anvil: testAnvil})
	var lastError string
	for i := 0; i+1 < len(attrs); i += 2 {
		if attrs[i] == "last_error" {
			lastError = attrs[i+1].(string)
		}
	}
	assert.True(t, utf8.ValidString(lastError), "never cut inside a character")
	assert.NotContains(t, lastError, "\n", "one line")
	assert.Equal(t, 201, utf8.RuneCountInString(lastError), "200 characters and the ellipsis")
}

func TestSkipLog_ASlotTakenEarlierInTheCycleIsNamed(t *testing.T) {
	e := newSkipEnv(t)
	second := "Forge-tagged-2"
	stubBdOnPath(t, e.dir,
		`[{"id": "`+taggedBead+`", "title": "first", "status": "ready", "priority": 1, "labels": ["`+skipTag+`"]},`+
			` {"id": "`+second+`", "title": "second", "status": "ready", "priority": 2, "labels": ["`+skipTag+`"]}]`)
	e.d.pollAndDispatch(e.ctx, false)
	require.True(t, dispatched(t, e, taggedBead), "the first takes the anvil's one slot")
	require.False(t, dispatched(t, e, second))
	lines := skipLines(e, second)
	require.Len(t, lines, 1)
	assert.Equal(t, "the anvil's max_smiths is reached", lines[0].Attrs["reason"])
	assert.Contains(t, fmt.Sprint(lines[0].Attrs["held_by"]), taggedBead+", dispatched this cycle", "named, not an empty list")
}

func TestSkipLog_InFlightWedgedAndOpenPRAreSaid(t *testing.T) {
	cases := []struct {
		name, reason string
		setup        func(e *testEnv)
	}{
		{"in flight", "already in flight in this daemon", func(e *testEnv) { e.d.activeBeads.Store(taggedBead, true) }},
		{"wedged", "the anvil's beads database is wedged", func(e *testEnv) {
			_, _, err := e.db.MarkAnvilWedged(state.AnvilHealth{Anvil: testAnvil, Wedged: true, Detail: "mid-merge"})
			require.NoError(t, err)
		}},
		{"open PR", "the forge's state has an open PR for it", func(e *testEnv) {
			require.NoError(t, e.db.InsertPR(&state.PR{Number: 7, Anvil: testAnvil, BeadID: taggedBead, Branch: "forge/" + taggedBead,
				BaseBranch: "main", Status: state.PROpen, CreatedAt: time.Now()}))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newSkipEnv(t)
			c.setup(e)
			e.d.pollAndDispatch(e.ctx, false)
			e.d.pollAndDispatch(e.ctx, false)
			require.False(t, dispatched(t, e, taggedBead))
			lines := skipLines(e, taggedBead)
			require.Len(t, lines, 1, "said once over two polls")
			assert.Equal(t, c.reason, lines[0].Attrs["reason"])
		})
	}
}
