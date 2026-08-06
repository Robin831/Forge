package anvilhealth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRunner replies to queries by substring match. Any query with no configured
// reply returns errNoReply so a test can assert that a query was not expected.
type fakeRunner struct {
	replies map[string]string
	errs    map[string]error
	calls   []string
}

var errNoReply = errors.New("no reply configured")

func (f *fakeRunner) run(_ context.Context, _ string, query string) ([]byte, error) {
	f.calls = append(f.calls, query)
	for frag, err := range f.errs {
		if strings.Contains(query, frag) {
			return nil, err
		}
	}
	for frag, out := range f.replies {
		if strings.Contains(query, frag) {
			return []byte(out), nil
		}
	}
	return nil, errNoReply
}

func (f *fakeRunner) checker() *Checker {
	return &Checker{Run: f.run}
}

// divergenceReplies is the reply set for an anvil with a configured
// upstream that is 1 ahead / 10 behind — the shape of the 2026-08-05 incident.
func divergenceReplies() map[string]string {
	return map[string]string{
		"active_branch() AS branch":                         `[{"branch":"beads-sync"}]`,
		"FROM dolt_branches":                                `[{"remote":"origin","branch":"beads-sync"}]`,
		"dolt_log('remotes/origin/beads-sync..beads-sync')": `[{"n":1}]`,
		"dolt_log('beads-sync..remotes/origin/beads-sync')": `[{"n":10}]`,
	}
}

func TestCheck_HealthyAnvilRunsOnlyOneQuery(t *testing.T) {
	f := &fakeRunner{replies: map[string]string{"dolt_conflicts": `[]`}}
	rep, err := f.checker().Check(context.Background(), "/anvil")
	require.NoError(t, err)
	assert.False(t, rep.Wedged())
	assert.Empty(t, rep.Tables)
	assert.Zero(t, rep.TotalConflicts)
	// A healthy anvil must cost exactly the detection query — no divergence work.
	require.Len(t, f.calls, 1)
	assert.Contains(t, f.calls[0], "dolt_conflicts")
}

func TestCheck_EmptyOutputIsHealthy(t *testing.T) {
	for name, out := range map[string]string{
		"empty string": "",
		"whitespace":   "   \n",
		"json null":    "null",
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeRunner{replies: map[string]string{"dolt_conflicts": out}}
			rep, err := f.checker().Check(context.Background(), "/anvil")
			require.NoError(t, err)
			assert.False(t, rep.Wedged())
		})
	}
}

func TestCheck_SingleConflictedTable(t *testing.T) {
	replies := divergenceReplies()
	replies["dolt_conflicts"] = `[{"conflict_table":"issues","conflict_count":3}]`
	f := &fakeRunner{replies: replies}

	rep, err := f.checker().Check(context.Background(), "/anvil")
	require.NoError(t, err)
	require.True(t, rep.Wedged())
	assert.Equal(t, []string{"issues"}, rep.TableNames())
	assert.EqualValues(t, 3, rep.TotalConflicts)
	assert.Equal(t, "issues (3)", rep.TablesSummary())
	assert.True(t, rep.DivergenceKnown)
	assert.Equal(t, 1, rep.Ahead)
	assert.Equal(t, 10, rep.Behind)
	assert.Equal(t, "beads-sync ahead 1 / behind 10", rep.DivergenceSummary())

	// The operator-facing detail must name tables, count and divergence.
	detail := rep.Detail()
	assert.Contains(t, detail, "issues (3)")
	assert.Contains(t, detail, "Total conflicts: 3")
	assert.Contains(t, detail, "ahead 1 / behind 10")
}

func TestCheck_MultipleConflictedTablesSortedAndSummed(t *testing.T) {
	replies := divergenceReplies()
	replies["dolt_conflicts"] = `[{"conflict_table":"labels","conflict_count":1},{"conflict_table":"issues","conflict_count":4}]`
	f := &fakeRunner{replies: replies}

	rep, err := f.checker().Check(context.Background(), "/anvil")
	require.NoError(t, err)
	assert.Equal(t, []string{"issues", "labels"}, rep.TableNames(), "tables must be reported in a stable order")
	assert.EqualValues(t, 5, rep.TotalConflicts)
	assert.Equal(t, "issues (4), labels (1)", rep.TablesSummary())
}

func TestCheck_LegacyColumnNamesAndStringCounts(t *testing.T) {
	// Defensive: accept the unaliased column names and counts rendered as strings.
	replies := divergenceReplies()
	replies["dolt_conflicts"] = `[{"table":"issues","num_conflicts":"7"}]`
	f := &fakeRunner{replies: replies}

	rep, err := f.checker().Check(context.Background(), "/anvil")
	require.NoError(t, err)
	require.True(t, rep.Wedged())
	assert.Equal(t, []string{"issues"}, rep.TableNames())
	assert.EqualValues(t, 7, rep.TotalConflicts)
}

func TestCheck_QueryErrorIsUnknownNotHealthy(t *testing.T) {
	f := &fakeRunner{errs: map[string]error{"dolt_conflicts": errors.New("dial tcp 127.0.0.1:3306: connection refused")}}
	rep, err := f.checker().Check(context.Background(), "/anvil")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dolt_conflicts")
	assert.False(t, rep.Wedged(), "an errored probe must not be reported as wedged either")
}

func TestCheck_MalformedResultIsError(t *testing.T) {
	f := &fakeRunner{replies: map[string]string{"dolt_conflicts": `not json at all`}}
	_, err := f.checker().Check(context.Background(), "/anvil")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing dolt_conflicts result")
}

func TestCheck_EmptyAnvilPathIsError(t *testing.T) {
	f := &fakeRunner{}
	_, err := f.checker().Check(context.Background(), "")
	require.Error(t, err)
	assert.Empty(t, f.calls, "no query may be issued without an anvil path")
}

func TestCheck_DivergenceFailureStillReportsConflicts(t *testing.T) {
	// The conflict report is the point; divergence is a nice-to-have. A failure
	// to compute it must never suppress the wedge.
	f := &fakeRunner{
		replies: map[string]string{
			"dolt_conflicts":            `[{"conflict_table":"issues","conflict_count":2}]`,
			"active_branch() AS branch": `[{"branch":"beads-sync"}]`,
		},
		errs: map[string]error{"dolt_branches": errors.New("boom")},
	}
	rep, err := f.checker().Check(context.Background(), "/anvil")
	require.NoError(t, err)
	require.True(t, rep.Wedged())
	assert.False(t, rep.DivergenceKnown)
	assert.Equal(t, "divergence unknown", rep.DivergenceSummary())
	assert.Contains(t, rep.Detail(), "divergence unknown")
}

func TestCheck_NoUpstreamFallsBackToSoleRemote(t *testing.T) {
	f := &fakeRunner{replies: map[string]string{
		"dolt_conflicts":            `[{"conflict_table":"issues","conflict_count":1}]`,
		"active_branch() AS branch": `[{"branch":"main"}]`,
		// No upstream recorded for the branch.
		"FROM dolt_branches":                    `[{"remote":"","branch":""}]`,
		"FROM dolt_remotes":                     `[{"name":"origin"}]`,
		"dolt_log('remotes/origin/main..main')": `[{"n":2}]`,
		"dolt_log('main..remotes/origin/main')": `[{"n":0}]`,
	}}
	rep, err := f.checker().Check(context.Background(), "/anvil")
	require.NoError(t, err)
	assert.Equal(t, "remotes/origin/main", rep.Upstream)
	assert.True(t, rep.DivergenceKnown)
	assert.Equal(t, 2, rep.Ahead)
	assert.Equal(t, 0, rep.Behind)
}

func TestCheck_NoRemoteLeavesDivergenceUnknown(t *testing.T) {
	f := &fakeRunner{replies: map[string]string{
		"dolt_conflicts":            `[{"conflict_table":"issues","conflict_count":1}]`,
		"active_branch() AS branch": `[{"branch":"main"}]`,
		"FROM dolt_branches":        `[{"remote":"","branch":""}]`,
		"FROM dolt_remotes":         `[]`,
	}}
	rep, err := f.checker().Check(context.Background(), "/anvil")
	require.NoError(t, err)
	require.True(t, rep.Wedged())
	assert.False(t, rep.DivergenceKnown)
	assert.Equal(t, "main", rep.Branch)
}

func TestCheck_UnsafeBranchNameIsNotInterpolated(t *testing.T) {
	f := &fakeRunner{replies: map[string]string{
		"dolt_conflicts":            `[{"conflict_table":"issues","conflict_count":1}]`,
		"active_branch() AS branch": `[{"branch":"weird'; DROP TABLE issues; --"}]`,
	}}
	rep, err := f.checker().Check(context.Background(), "/anvil")
	require.NoError(t, err)
	require.True(t, rep.Wedged())
	assert.False(t, rep.DivergenceKnown)
	for _, q := range f.calls {
		assert.NotContains(t, q, "DROP TABLE", "a branch name must never reach a dolt_log range unvalidated")
	}
}

func TestConflictsQueryQuotesReservedWord(t *testing.T) {
	// `table` is a reserved word; an unquoted identifier makes the probe fail on
	// every poll and silently report "unknown" forever.
	assert.Contains(t, conflictsQuery, "`table`")
	assert.Contains(t, conflictsQuery, "dolt_conflicts")
}
